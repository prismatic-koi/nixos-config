package exporter

import (
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/metrics"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
)

// lifecycle.go — the six lifecycle and outcome counters.
//
// All six are produced by ONE tailer (TailerLifecycleEvents), running over
// the same agent_events table as the events tailer but keeping its own
// cursor. Every counter comes from the tail cursor, never from an aggregate
// over a pruned table — see LifecycleEventsTailSQL in sql.go for the query
// and the prune-safety argument for its join.
//
// The dispatch below is closed-set by construction: (*lifecycleCounters).apply
// only ever calls Inc on one of the six CounterVecs constructed in New, and
// every label value handed to Inc is either a value already sanctioned as a
// safe label (repo, agent_role, isolation_mode, end_state, profile) or a
// value drawn from a two-element closed set (verdict). prism_spawns_total
// DOES carry a profile label — see the comment on LifecycleEventsTailSQL in
// sql.go for the boundary-test narrowing that makes this safe.

// Metric names for the six counters.
const (
	MetricSpawnsTotal           = "prism_spawns_total"
	MetricSessionsEndedTotal    = "prism_sessions_ended_total"
	MetricReviewVerdictsTotal   = "prism_review_verdicts_total"
	MetricEscalationsTotal      = "prism_escalations_total"
	MetricDoomLoopsTotal        = "prism_doom_loops_total"
	MetricPermissionDeniedTotal = "prism_permission_denied_total"
)

// TailerLifecycleEvents is the state-file key the lifecycle tailer's cursor
// is stored under. It is independent of TailerAgentEvents even though
// both tail the same table — changing this name makes a running daemon lose
// its place on these six counters only.
const TailerLifecycleEvents = "agent_events_lifecycle"

// eventTypeEscalated, eventTypeDoomLoop, and eventTypePermissionDenied name
// the agent_events.type values that directly drive a lifecycle counter with no
// join required. They are unexported because nothing outside this file
// needs them; the durable constants a reader may want to cross-check against
// are session.EventSpawnIntent, db.SessionReapEventType (both imported
// elsewhere), and the review.EventReviewVerdict* pair below.
const (
	eventTypeEscalated        = "session.escalated"
	eventTypeDoomLoop         = "doom_loop_detected"
	eventTypePermissionDenied = "permission_denied"
)

// verdictLabel folds a review.EventReviewVerdict* type into the verdict
// label value. ok is false for any other type.
func verdictLabel(eventType string) (verdict string, ok bool) {
	switch eventType {
	case review.EventReviewVerdictPass:
		return "pass", true
	case review.EventReviewVerdictFail:
		return "fail", true
	default:
		return "", false
	}
}

// lifecycleCounters holds the six CounterVecs New registers, plus the
// dispatch function the tailer applies to each record.
type lifecycleCounters struct {
	spawnsTotal           *metrics.CounterVec
	sessionsEndedTotal    *metrics.CounterVec
	reviewVerdictsTotal   *metrics.CounterVec
	escalationsTotal      *metrics.CounterVec
	doomLoopsTotal        *metrics.CounterVec
	permissionDeniedTotal *metrics.CounterVec
}

// newLifecycleCounters constructs and registers the six CounterVecs.
func newLifecycleCounters(reg *metrics.Registry) *lifecycleCounters {
	lc := &lifecycleCounters{
		// profile is sourced from spawn_inputs.profile_name via the
		// LifecycleEventsTailSQL join; NULL folds to "default"
		// at scan time in sql.go, never the empty string.
		spawnsTotal: metrics.NewCounterVec(
			MetricSpawnsTotal,
			"Total prism spawn attempts observed by the exporter, by repo, agent role, isolation mode, and profile.",
			[]string{"repo", "agent_role", "isolation_mode", "profile"},
		),
		sessionsEndedTotal: metrics.NewCounterVec(
			MetricSessionsEndedTotal,
			"Total prism sessions the exporter has observed end, by repo, agent role, and end state.",
			[]string{"repo", "agent_role", "end_state"},
		),
		reviewVerdictsTotal: metrics.NewCounterVec(
			MetricReviewVerdictsTotal,
			"Total prism review rounds that reached a pass/fail verdict, by verdict.",
			[]string{"verdict"},
		),
		escalationsTotal: metrics.NewCounterVec(
			MetricEscalationsTotal,
			"Total prism escalate invocations observed by the exporter, by repo.",
			[]string{"repo"},
		),
		doomLoopsTotal: metrics.NewCounterVec(
			MetricDoomLoopsTotal,
			"Total prism doom-loop detections observed by the exporter, by repo.",
			[]string{"repo"},
		),
		permissionDeniedTotal: metrics.NewCounterVec(
			MetricPermissionDeniedTotal,
			"Total prism permission denials observed by the exporter, by repo.",
			[]string{"repo"},
		),
	}
	reg.MustRegister(lc.spawnsTotal)
	reg.MustRegister(lc.sessionsEndedTotal)
	reg.MustRegister(lc.reviewVerdictsTotal)
	reg.MustRegister(lc.escalationsTotal)
	reg.MustRegister(lc.doomLoopsTotal)
	reg.MustRegister(lc.permissionDeniedTotal)
	return lc
}

// apply is the tailcursor apply function for the lifecycle tailer. It is a
// closed dispatch over agent_events.type: every type this switch does not
// name is a no-op, which covers the vast majority of rows (msg_assistant,
// tool_call, and so on) that carry none of the six counters.
//
// The five repo-labelled counters (spawnsTotal, sessionsEndedTotal,
// escalationsTotal, doomLoopsTotal, permissionDeniedTotal) fold empty or
// whitespace-only ev.Repo to the unknownRepoLabel placeholder via repoLabel(),
// to prevent unbounded label cardinality and blank template-variable entries
// to prevent unbounded label cardinality and blank template-variable
// entries. Counters are tail-cursor accumulated and persisted across
// restarts, so a label-value correction ends the old series and starts a new
// one at zero: corrected values start a new series going forward;
// pre-correction rows keep their original label value (no backfill).
func (lc *lifecycleCounters) apply(ev lifecycleEvent) error {
	switch ev.Type {
	case session.EventSpawnIntent:
		return lc.spawnsTotal.Inc(repoLabel(ev.Repo), ev.AgentRole, ev.IsolationMode, ev.ProfileName)
	case db.SessionReapEventType:
		return lc.sessionsEndedTotal.Inc(repoLabel(ev.Repo), ev.AgentRole, ev.EndState)
	case eventTypeEscalated:
		return lc.escalationsTotal.Inc(repoLabel(ev.Repo))
	case eventTypeDoomLoop:
		return lc.doomLoopsTotal.Inc(repoLabel(ev.Repo))
	case eventTypePermissionDenied:
		return lc.permissionDeniedTotal.Inc(repoLabel(ev.Repo))
	default:
		if verdict, ok := verdictLabel(ev.Type); ok {
			return lc.reviewVerdictsTotal.Inc(verdict)
		}
		return nil
	}
}
