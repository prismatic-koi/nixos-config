package exporter

import (
	"database/sql"
	"log"
	"sync"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/metrics"
)

// gauges.go — the four #2702 state gauges: point-in-time fleet state,
// recomputed with plain SQL on every scrape.
//
// Unlike the counters in lifecycle.go and cost.go, none of these use the
// tail cursor (#2699 section 4): a gauge carries no monotonicity contract,
// so the 90-day prune cannot invalidate it the way it would a full-table
// counter aggregate (#2699 section 3). A collector below simply issues a
// plain SELECT at Collect() time and folds the rows into label counts in
// Go — there is no COUNT()/SUM() in the SQL itself (see sql.go for why that
// also keeps these queries clear of the aggregate ban that governs the
// counter-producing statements).
//
//	prism_sessions_active{repo,agent_role,state}   gauge  agent_status
//	prism_merge_queue_depth{repo}                  gauge  pending_merges (status='watching')
//	prism_merges_by_status{repo,status}            gauge  pending_merges (all statuses)
//	prism_bus_messages_pending{repo}               gauge  bus_messages (delivered_at IS NULL)
//
// All four read a small, indexed table (agent_status.ended_at,
// pending_merges' PK, and the pending-bus partial index at db.go:181), so
// they are cheap enough to recompute on every scrape.

// Metric names for the four #2702 gauges.
const (
	MetricSessionsActive     = "prism_sessions_active"
	MetricMergeQueueDepth    = "prism_merge_queue_depth"
	MetricMergesByStatus     = "prism_merges_by_status"
	MetricBusMessagesPending = "prism_bus_messages_pending"
)

// unknownStateLabel is the label value prism_sessions_active{state} uses for
// an agent_status.state value outside the pinned set (see stateLabel below).
const unknownStateLabel = "other"

// stateLabel folds an agent_status.state value into the closed label set
// pinned to internal/agent/agent.go's AgentState constants — the
// authoritative state enum for a prism session (#2702's "pin the state
// label set" requirement).
//
// Folding matters because agent_status.state is not actually
// constrained to this set at write time: internal/agent/agent.go's own doc
// comment on ValidTransitions records that "the TypeScript plugin writes
// state directly to SQLite and is not constrained by this table --
// validation here is additive and advisory only." A stray value must not
// turn into a new, unbounded label on a fleet-wide series, so anything
// outside the pinned set folds to unknownStateLabel rather than being
// exposed verbatim.
func stateLabel(state string) (label string, known bool) {
	switch agent.AgentState(state) {
	case agent.StateActive, agent.StateWaiting, agent.StateFinished,
		agent.StateCompacting, agent.StateError, agent.StateIdle,
		agent.StateInterrupted, agent.StateDeleted, agent.StateReviewing,
		agent.StateEscalated:
		return state, true
	default:
		return unknownStateLabel, false
	}
}

// sessionsActiveCollector implements prism_sessions_active by reading
// agent_status directly (SessionsActiveSQL) and counting rows in Go, grouped
// by (repo, agent_role, state).
type sessionsActiveCollector struct {
	conn   *sql.DB
	logger *log.Logger

	// warnUnknownState logs the "unknown state" advisory exactly once for
	// the lifetime of the collector, never per scrape (#2702 edge-case AC).
	warnUnknownState sync.Once
}

func (c *sessionsActiveCollector) Name() string       { return MetricSessionsActive }
func (c *sessionsActiveCollector) Kind() metrics.Kind { return metrics.KindGauge }
func (c *sessionsActiveCollector) Help() string {
	return "Prism sessions currently active (agent_status.ended_at IS NULL), by repo, agent role, and state."
}

type sessionsActiveKey struct {
	repo, role, state string
}

func (c *sessionsActiveCollector) Collect() []metrics.Sample {
	rows, err := c.conn.Query(SessionsActiveSQL)
	if err != nil {
		c.logger.Printf("gauge %s: query failed: %v", MetricSessionsActive, err)
		return nil
	}
	defer rows.Close()

	counts := make(map[sessionsActiveKey]float64)
	sawUnknown := false
	for rows.Next() {
		var repo string
		var role, state sql.NullString
		if err := rows.Scan(&repo, &role, &state); err != nil {
			c.logger.Printf("gauge %s: scan failed: %v", MetricSessionsActive, err)
			return nil
		}
		label, known := stateLabel(state.String)
		if !known {
			sawUnknown = true
		}
		counts[sessionsActiveKey{repo: repo, role: role.String, state: label}]++
	}
	if err := rows.Err(); err != nil {
		c.logger.Printf("gauge %s: iterate rows failed: %v", MetricSessionsActive, err)
		return nil
	}
	if sawUnknown {
		c.warnUnknownState.Do(func() {
			c.logger.Printf(
				"gauge %s: agent_status.state held a value outside the pinned set "+
					"(internal/agent/agent.go's AgentState constants); folded to %q",
				MetricSessionsActive, unknownStateLabel)
		})
	}

	labelNames := []string{"repo", "agent_role", "state"}
	samples := make([]metrics.Sample, 0, len(counts))
	for k, v := range counts {
		samples = append(samples, metrics.Sample{
			LabelNames:  append([]string{}, labelNames...),
			LabelValues: []string{k.repo, k.role, k.state},
			Value:       v,
		})
	}
	return samples
}

// mergeQueueDepthCollector implements prism_merge_queue_depth by reading
// pending_merges rows with status='watching' (MergeQueueDepthSQL) and
// counting them in Go, grouped by repo.
type mergeQueueDepthCollector struct {
	conn   *sql.DB
	logger *log.Logger
}

func (c *mergeQueueDepthCollector) Name() string       { return MetricMergeQueueDepth }
func (c *mergeQueueDepthCollector) Kind() metrics.Kind { return metrics.KindGauge }
func (c *mergeQueueDepthCollector) Help() string {
	return "Prism PRs currently enqueued for merge (pending_merges.status = 'watching'), by repo."
}

func (c *mergeQueueDepthCollector) Collect() []metrics.Sample {
	rows, err := c.conn.Query(MergeQueueDepthSQL)
	if err != nil {
		c.logger.Printf("gauge %s: query failed: %v", MetricMergeQueueDepth, err)
		return nil
	}
	defer rows.Close()

	counts := make(map[string]float64)
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			c.logger.Printf("gauge %s: scan failed: %v", MetricMergeQueueDepth, err)
			return nil
		}
		counts[repo]++
	}
	if err := rows.Err(); err != nil {
		c.logger.Printf("gauge %s: iterate rows failed: %v", MetricMergeQueueDepth, err)
		return nil
	}

	samples := make([]metrics.Sample, 0, len(counts))
	for repo, v := range counts {
		samples = append(samples, metrics.Sample{
			LabelNames:  []string{"repo"},
			LabelValues: []string{repo},
			Value:       v,
		})
	}
	return samples
}

// mergesByStatusCollector implements prism_merges_by_status by reading every
// pending_merges row (MergesByStatusSQL) and counting them in Go, grouped by
// (repo, status).
type mergesByStatusCollector struct {
	conn   *sql.DB
	logger *log.Logger
}

func (c *mergesByStatusCollector) Name() string       { return MetricMergesByStatus }
func (c *mergesByStatusCollector) Kind() metrics.Kind { return metrics.KindGauge }
func (c *mergesByStatusCollector) Help() string {
	return "Prism pending_merges rows, by repo and status (watching, merged, failed, cancelled, abandoned)."
}

type mergesByStatusKey struct {
	repo, status string
}

func (c *mergesByStatusCollector) Collect() []metrics.Sample {
	rows, err := c.conn.Query(MergesByStatusSQL)
	if err != nil {
		c.logger.Printf("gauge %s: query failed: %v", MetricMergesByStatus, err)
		return nil
	}
	defer rows.Close()

	counts := make(map[mergesByStatusKey]float64)
	for rows.Next() {
		var repo, status string
		if err := rows.Scan(&repo, &status); err != nil {
			c.logger.Printf("gauge %s: scan failed: %v", MetricMergesByStatus, err)
			return nil
		}
		counts[mergesByStatusKey{repo: repo, status: status}]++
	}
	if err := rows.Err(); err != nil {
		c.logger.Printf("gauge %s: iterate rows failed: %v", MetricMergesByStatus, err)
		return nil
	}

	samples := make([]metrics.Sample, 0, len(counts))
	for k, v := range counts {
		samples = append(samples, metrics.Sample{
			LabelNames:  []string{"repo", "status"},
			LabelValues: []string{k.repo, k.status},
			Value:       v,
		})
	}
	return samples
}

// busMessagesPendingCollector implements prism_bus_messages_pending by
// reading undelivered bus_messages rows (BusMessagesPendingSQL) and counting
// them in Go, grouped by repo.
//
// It reads bus_messages.repo only — never bus_messages.text, the free-form
// inter-session message body that #2699 section 5 bans (see the boundary-
// test narrowing in sql_boundary_test.go: the table-level ban is narrowed to
// that one column, exactly as #2720 narrowed spawn_inputs).
type busMessagesPendingCollector struct {
	conn   *sql.DB
	logger *log.Logger
}

func (c *busMessagesPendingCollector) Name() string       { return MetricBusMessagesPending }
func (c *busMessagesPendingCollector) Kind() metrics.Kind { return metrics.KindGauge }
func (c *busMessagesPendingCollector) Help() string {
	return "Prism inter-session bus messages awaiting delivery (bus_messages.delivered_at IS NULL), by repo."
}

func (c *busMessagesPendingCollector) Collect() []metrics.Sample {
	rows, err := c.conn.Query(BusMessagesPendingSQL)
	if err != nil {
		c.logger.Printf("gauge %s: query failed: %v", MetricBusMessagesPending, err)
		return nil
	}
	defer rows.Close()

	counts := make(map[string]float64)
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			c.logger.Printf("gauge %s: scan failed: %v", MetricBusMessagesPending, err)
			return nil
		}
		counts[repo]++
	}
	if err := rows.Err(); err != nil {
		c.logger.Printf("gauge %s: iterate rows failed: %v", MetricBusMessagesPending, err)
		return nil
	}

	samples := make([]metrics.Sample, 0, len(counts))
	for repo, v := range counts {
		samples = append(samples, metrics.Sample{
			LabelNames:  []string{"repo"},
			LabelValues: []string{repo},
			Value:       v,
		})
	}
	return samples
}

// registerStateGauges constructs and registers the four #2702 gauges.
func registerStateGauges(reg *metrics.Registry, conn *sql.DB, logger *log.Logger) {
	reg.MustRegister(&sessionsActiveCollector{conn: conn, logger: logger})
	reg.MustRegister(&mergeQueueDepthCollector{conn: conn, logger: logger})
	reg.MustRegister(&mergesByStatusCollector{conn: conn, logger: logger})
	reg.MustRegister(&busMessagesPendingCollector{conn: conn, logger: logger})
}
