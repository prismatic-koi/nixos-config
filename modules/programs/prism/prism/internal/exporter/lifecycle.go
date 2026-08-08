package exporter

import (
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/metrics"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
)

// lifecycle.go — the six lifecycle and outcome counters of issue #2703.
//
// All six are produced by ONE tailer (TailerLifecycleEvents), running over
// the same agent_events table as the #2700 events tailer but keeping its own
// cursor. Every counter comes from the tail cursor, never from an aggregate
// over a pruned table (#2699 section 3) — see LifecycleEventsTailSQL in
// sql.go for the query and the prune-safety argument for its join.
//
// The dispatch below is closed-set by construction: (*lifecycleCounters).apply
// only ever calls Inc on one of the six CounterVecs constructed in New, and
// every label value handed to Inc is either a value already sanctioned as a
// safe label by #2699 section 6 (repo, agent_role, isolation_mode, end_state,
// profile) or a value drawn from a two-element closed set (verdict).
// prism_spawns_total DOES carry a profile label (issue #2720) — see the
// comment on LifecycleEventsTailSQL in sql.go for the boundary-test
// narrowing that made this safe.

// Metric names for the six #2703 counters.
const (
	MetricSpawnsTotal           = "prism_spawns_total"
	MetricSessionsEndedTotal    = "prism_sessions_ended_total"
	MetricReviewVerdictsTotal   = "prism_review_verdicts_total"
	MetricEscalationsTotal      = "prism_escalations_total"
	MetricDoomLoopsTotal        = "prism_doom_loops_total"
	MetricPermissionDeniedTotal = "prism_permission_denied_total"
)

// TailerLifecycleEvents is the state-file key the #2703 tailer's cursor is
// stored under. It is independent of TailerAgentEvents (#2700) even though
// both tail the same table — changing this name makes a running daemon lose
// its place on these six counters only.
const TailerLifecycleEvents = "agent_events_lifecycle"

// eventTypeEscalated, eventTypeDoomLoop, and eventTypePermissionDenied name
// the agent_events.type values that directly drive a #2703 counter with no
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

// newLifecycleCounters constructs and registers the six #2703 CounterVecs.
func newLifecycleCounters(reg *metrics.Registry) *lifecycleCounters {
	lc := &lifecycleCounters{
		// profile is sourced from spawn_inputs.profile_name via the
		// LifecycleEventsTailSQL join (issue #2720); NULL folds to "default"
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
func (lc *lifecycleCounters) apply(ev lifecycleEvent) error {
	switch ev.Type {
	case session.EventSpawnIntent:
		return lc.spawnsTotal.Inc(ev.Repo, ev.AgentRole, ev.IsolationMode, ev.ProfileName)
	case db.SessionReapEventType:
		return lc.sessionsEndedTotal.Inc(ev.Repo, ev.AgentRole, ev.EndState)
	case eventTypeEscalated:
		return lc.escalationsTotal.Inc(ev.Repo)
	case eventTypeDoomLoop:
		return lc.doomLoopsTotal.Inc(ev.Repo)
	case eventTypePermissionDenied:
		return lc.permissionDeniedTotal.Inc(ev.Repo)
	default:
		if verdict, ok := verdictLabel(ev.Type); ok {
			return lc.reviewVerdictsTotal.Inc(verdict)
		}
		return nil
	}
}
