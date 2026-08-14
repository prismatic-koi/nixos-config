package exporter

import (
	"database/sql"
	"log"
	"strings"
	"sync"
	"time"

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
//
// Two more gauges, added by #2708, share this file and this shape for the
// same reason -- a point-in-time liveness question is exactly what a
// scrape-time plain-SQL recompute is for:
//
//	prism_sidecars_live{repo}    gauge  agent_status (activity-expected state, last_seen fresh)
//	prism_sidecars_stale{repo}   gauge  agent_status (activity-expected state, last_seen stale)
//
// Both restrict to agent_status.state values where continuous event
// production is actually expected (active, compacting, reviewing) -- see
// sidecarActivityExpected below for why the quiet-by-design states (idle,
// waiting, escalated) are excluded from both gauges rather than counted
// stale.

// Metric names for the four #2702 gauges.
const (
	MetricSessionsActive     = "prism_sessions_active"
	MetricMergeQueueDepth    = "prism_merge_queue_depth"
	MetricMergesByStatus     = "prism_merges_by_status"
	MetricBusMessagesPending = "prism_bus_messages_pending"
)

// Metric names for the #2708 sidecar-liveness gauges.
const (
	MetricSidecarsLive  = "prism_sidecars_live"
	MetricSidecarsStale = "prism_sidecars_stale"
)

// SidecarStaleThreshold is how long agent_status.last_seen can go silent,
// for a session in an activity-expected state (see
// sidecarActivityExpected below) with ended_at IS NULL, before its sidecar
// counts as dead or wedged rather than merely quiet (#2708).
//
// last_seen is a heartbeat: it is populated from MAX(agent_events.created_at)
// (see the v13->v14 migration in internal/db/db.go) and updated by every
// live WriteEvent call. A sidecar that has died or wedged stops emitting
// events entirely, so its last_seen stops advancing while ended_at stays
// NULL -- exactly the failure prism-restore.service cannot see, because it
// reports only whether the spawner ran, not whether any given sidecar is
// still alive.
//
// The value is pinned to DefaultReviewAgentInactivityTimeout /
// reviewAgentActivityWindow (both 15m: internal/sidecar/sidecar.go and
// internal/review/monitor.go), rather than picked independently, for two
// reasons:
//
//  1. prism already has an established definition of "this agent has gone
//     quiet too long": the review-agent inactivity watchdog. Reusing it
//     keeps the fleet's notion of "too quiet" consistent across the
//     exporter and the watchdog rather than introducing a second, competing
//     number.
//  2. Even an activity-expected session has legitimate quiet stretches --
//     a single long tool call (a nix build, a large test run) emits
//     tool_call at the start and tool_result at the end, with nothing in
//     between. A short threshold (an earlier version of this gauge used 5m)
//     trips on exactly that shape and produces a false positive. 15m is the
//     number prism itself already trusts not to flap on ordinary tool
//     latency; see TestSidecarStaleThreshold_MatchesReviewWatchdog for the
//     mechanical anti-drift guard (analogous to
//     TestReviewAgentActivityWindow_MatchesWatchdog in internal/review).
//
// It is still well above DefaultPollInterval (15s, see exporter.go): the
// exporter recomputes this gauge on every scrape, so a threshold close to
// the scrape cadence would flap a healthy-but-quiet session in and out of
// "stale" on ordinary jitter.
const SidecarStaleThreshold = 15 * time.Minute

// unknownStateLabel is the label value prism_sessions_active{state} uses for
// an agent_status.state value outside the pinned set (see stateLabel below).
const unknownStateLabel = "other"

// unknownRepoLabel is the label value all repo-labelled gauges use for an
// agent_status.repo value that is empty or whitespace-only. Empty repo label
// values create unbounded label sets and confuse dashboard templating (see
// stateLabel's doc comment for the rationale; the pattern is identical).
const unknownRepoLabel = "unknown"

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

// repoLabel folds an agent_status.repo value that is empty or whitespace-only
// into unknownRepoLabel. An empty repo label value has two effects on a
// dashboard (#2764):
//   - sum by (repo) collects every repo-less session into one unnamed bucket
//   - a repo template variable gets a blank entry, which an operator cannot
//     read
//
// Folding to a single explicit placeholder prevents both: the series still
// appears (it is a real session), but with a readable, stable label.
func repoLabel(repo string) string {
	if strings.TrimSpace(repo) == "" {
		return unknownRepoLabel
	}
	return repo
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
		counts[sessionsActiveKey{repo: repoLabel(repo), role: role.String, state: label}]++
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
		counts[repoLabel(repo)]++
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
		counts[mergesByStatusKey{repo: repoLabel(repo), status: status}]++
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
		counts[repoLabel(repo)]++
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

// sidecarActivityExpected reports whether a session in state s is expected
// to be continuously producing agent_events -- and so whether a silent
// last_seen in that state is evidence the sidecar has died or wedged,
// rather than evidence of nothing at all (#2708 round-1 review finding).
//
// Real fleet data caught the gap this closes: a session in StateIdle can sit
// quiet for well over an hour with a perfectly healthy sidecar -- nobody is
// talking to it, so it has nothing to emit. Flagging that as "stale" is a
// standing false positive that trains an operator to ignore the gauge.
//
// The candidate set is deliberately narrow -- only the states where
// continuous event production is the expected behaviour:
//
//   - StateActive: the agent is mid-turn. Silence here for longer than
//     SidecarStaleThreshold is genuinely suspicious (modulo the long-tool-call
//     allowance the threshold itself already gives, per its doc comment).
//   - StateCompacting: an automatic, bounded, in-turn operation; the agent is
//     still working and still expected to report back.
//   - StateReviewing: the worker is mid-review, waiting on `prism review`'s
//     own agents -- but it is still the active turn of the parent session,
//     which resumes and reports the moment the review completes.
//
// Excluded, and never counted in either gauge, because silence is the
// expected steady state, not a symptom:
//
//   - StateIdle: nobody is talking to it (the case found in review).
//   - StateWaiting: blocked on human input; silence is the point.
//   - StateEscalated: awaiting coordinator guidance via `prism escalate`;
//     same shape as waiting, just addressed to a different party.
//
// Also excluded: the terminal states (agent.IsTerminal: finished, error,
// interrupted, deleted). SidecarLivenessSQL already filters to
// `ended_at IS NULL`, so a terminal-state row here is a narrow race window
// (state written moments before ended_at) rather than the normal case, and
// it carries no liveness signal either way -- the session is already on its
// way out. An unrecognised state (outside agent.AgentState's pinned set,
// per stateLabel's advisory-only note above) is excluded for the same
// reason a terminal state is: absent positive evidence that continuous
// activity is expected, silence must not be read as a symptom.
func sidecarActivityExpected(state string) bool {
	switch agent.AgentState(state) {
	case agent.StateActive, agent.StateCompacting, agent.StateReviewing:
		return true
	default:
		return false
	}
}

type sidecarLivenessCollector struct {
	conn   *sql.DB
	logger *log.Logger

	// live selects which of the two gauges this collector instance reports:
	// true for prism_sidecars_live, false for prism_sidecars_stale.
	live bool

	// now returns the reference time staleness is measured against. Set in
	// tests to something other than time.Now for a deterministic threshold
	// crossing; nil defaults to time.Now.
	now func() time.Time
}

func (c *sidecarLivenessCollector) Name() string {
	if c.live {
		return MetricSidecarsLive
	}
	return MetricSidecarsStale
}

func (c *sidecarLivenessCollector) Kind() metrics.Kind { return metrics.KindGauge }

func (c *sidecarLivenessCollector) Help() string {
	if c.live {
		return "Prism sessions in an activity-expected state (active, compacting, reviewing) whose sidecar is live (ended_at IS NULL, last_seen within SidecarStaleThreshold), by repo."
	}
	return "Prism sessions in an activity-expected state (active, compacting, reviewing) that have not ended but whose sidecar has gone quiet (last_seen older than SidecarStaleThreshold, or never set) -- a dead or wedged sidecar, by repo. Sessions in a quiet-by-design state (idle, waiting, escalated) are excluded, not counted stale."
}

func (c *sidecarLivenessCollector) nowFunc() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *sidecarLivenessCollector) Collect() []metrics.Sample {
	rows, err := c.conn.Query(SidecarLivenessSQL)
	if err != nil {
		c.logger.Printf("gauge %s: query failed: %v", c.Name(), err)
		return nil
	}
	defer rows.Close()

	cutoff := c.nowFunc().Add(-SidecarStaleThreshold)
	counts := make(map[string]float64)
	for rows.Next() {
		var repo string
		var lastSeen sql.NullInt64
		var state sql.NullString
		if err := rows.Scan(&repo, &lastSeen, &state); err != nil {
			c.logger.Printf("gauge %s: scan failed: %v", c.Name(), err)
			return nil
		}

		// A session in a quiet-by-design state (idle, waiting, escalated) or
		// a terminal/unrecognised state carries no liveness signal either
		// way and is excluded from both gauges (#2708 round-1 review
		// finding; see sidecarActivityExpected).
		if !sidecarActivityExpected(state.String) {
			continue
		}

		// A NULL or zero last_seen has no positive evidence of a live
		// sidecar, so it is never treated as fresh, regardless of cutoff.
		isLive := false
		if lastSeen.Valid && lastSeen.Int64 > 0 {
			seen := time.UnixMilli(lastSeen.Int64)
			isLive = !seen.Before(cutoff)
		}

		if isLive == c.live {
			counts[repoLabel(repo)]++
		}
	}
	if err := rows.Err(); err != nil {
		c.logger.Printf("gauge %s: iterate rows failed: %v", c.Name(), err)
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

// registerStateGauges constructs and registers the four #2702 gauges plus
// the two #2708 sidecar-liveness gauges.
func registerStateGauges(reg *metrics.Registry, conn *sql.DB, logger *log.Logger) {
	reg.MustRegister(&sessionsActiveCollector{conn: conn, logger: logger})
	reg.MustRegister(&mergeQueueDepthCollector{conn: conn, logger: logger})
	reg.MustRegister(&mergesByStatusCollector{conn: conn, logger: logger})
	reg.MustRegister(&busMessagesPendingCollector{conn: conn, logger: logger})
	reg.MustRegister(&sidecarLivenessCollector{conn: conn, logger: logger, live: true})
	reg.MustRegister(&sidecarLivenessCollector{conn: conn, logger: logger, live: false})
}
