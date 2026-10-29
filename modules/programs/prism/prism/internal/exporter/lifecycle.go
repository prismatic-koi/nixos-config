package exporter

import (
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/metrics"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/spawnbudget"
)

// lifecycle.go — the seven lifecycle and outcome counters.
//
// All seven are produced by ONE tailer (TailerLifecycleEvents), running over
// the same agent_events table as the events tailer but keeping its own
// cursor. Every counter comes from the tail cursor, never from an aggregate
// over a pruned table — see LifecycleEventsTailSQL in sql.go for the query
// and the prune-safety argument for its join.
//
// The dispatch below is closed-set by construction: (*lifecycleCounters).apply
// only ever calls Inc on one of the seven CounterVecs constructed in New, and
// every label value handed to Inc is either a value already sanctioned as a
// safe label (repo, agent_role, isolation_mode, end_state, profile) or a
// value drawn from a small closed set (verdict: three values at the round
// level, four at the per-agent level). prism_spawns_total
// DOES carry a profile label — see the comment on LifecycleEventsTailSQL in
// sql.go for the boundary-test narrowing that makes this safe.

// Metric names for the seven counters.
const (
	MetricSpawnsTotal              = "prism_spawns_total"
	MetricSessionsEndedTotal       = "prism_sessions_ended_total"
	MetricReviewVerdictsTotal      = "prism_review_verdicts_total"
	MetricReviewAgentVerdictsTotal = "prism_review_agent_verdicts_total"
	MetricEscalationsTotal         = "prism_escalations_total"
	MetricDoomLoopsTotal           = "prism_doom_loops_total"
	MetricPermissionDeniedTotal    = "prism_permission_denied_total"
	MetricSpawnOverridesTotal      = "prism_spawn_overrides_total"
)

// TailerLifecycleEvents is the state-file key the lifecycle tailer's cursor
// is stored under. It is independent of TailerAgentEvents even though
// both tail the same table — changing this name makes a running daemon lose
// its place on these seven counters only.
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
	case review.EventReviewVerdictPassWithDisagreement:
		return "pass_with_disagreement", true
	default:
		return "", false
	}
}

// agentVerdictLabel folds a review.EventReviewAgentVerdict* type into the
// verdict label value of the per-agent counter. ok is false for any other
// type.
//
// It has three values where verdictLabel has two: the per-agent event keeps a
// parseable FAIL verdict ("fail") apart from a round that recorded no PASS and
// no FAIL for the agent ("error"). The round-level pair cannot make that
// distinction, because a round is a pass only when every agent passed.
//
// "error" holds an agent that failed to start, stalled, exited uncleanly,
// produced no output, or was absent from the round. It does NOT hold
// PASS_WITH_DISAGREEMENT: since #2970 the marker is a terminating pass and
// records its own "pass_with_disagreement" value, so the per-agent counter
// agrees with the round counter about a round that carries it. The writer's
// constant comment (review.EventReviewAgentVerdictError) carries the full
// bucket definition and why it is drawn there.
func agentVerdictLabel(eventType string) (verdict string, ok bool) {
	switch eventType {
	case review.EventReviewAgentVerdictPass:
		return "pass", true
	case review.EventReviewAgentVerdictFail:
		return "fail", true
	case review.EventReviewAgentVerdictError:
		return "error", true
	case review.EventReviewAgentVerdictPassWithDisagreement:
		return "pass_with_disagreement", true
	default:
		return "", false
	}
}

// lifecycleCounters holds the seven CounterVecs New registers, plus the
// dispatch function the tailer applies to each record.
type lifecycleCounters struct {
	spawnsTotal              *metrics.CounterVec
	sessionsEndedTotal       *metrics.CounterVec
	reviewVerdictsTotal      *metrics.CounterVec
	reviewAgentVerdictsTotal *metrics.CounterVec
	escalationsTotal         *metrics.CounterVec
	doomLoopsTotal           *metrics.CounterVec
	permissionDeniedTotal    *metrics.CounterVec
	spawnOverridesTotal      *metrics.CounterVec
}

// newLifecycleCounters constructs and registers the seven CounterVecs.
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
			"Total prism review rounds that reached a verdict, by verdict, repo, agent role, and profile. "+
				"verdict is one of pass, fail, or pass_with_disagreement (a round that passed but carried review-goal's "+
				"PASS_WITH_DISAGREEMENT marker, escalated to the coordinator).",
			[]string{"verdict", "repo", "agent_role", "profile"},
		),
		// One increment per review AGENT per round, beside the round-level
		// counter above. agent_role is the review dimension
		// (review-goal, review-code, review-security, review-qa,
		// review-context), resolved from the event's instance_id through the
		// same sessions join every other counter here uses. verdict takes
		// four values: pass, fail, error, and pass_with_disagreement. The HELP
		// text states what "error" covers, because an operator reading a rising
		// error rate for one role must not read it as "that agent keeps
		// crashing" when it can mean the agent produced no verdict for a mix of
		// reasons. review-goal's PASS_WITH_DISAGREEMENT is NOT in "error": since
		// #2970 it is a terminating pass and records pass_with_disagreement.
		reviewAgentVerdictsTotal: metrics.NewCounterVec(
			MetricReviewAgentVerdictsTotal,
			"Total prism review agent verdicts, by verdict, review agent role, and repo. One per agent per round. "+
				"verdict=\"error\" means the round recorded neither a PASS nor a FAIL for that agent: it failed to start, "+
				"stalled, exited uncleanly, produced no output, or was absent from the round. "+
				"verdict=\"pass_with_disagreement\" is review-goal's terminating PASS_WITH_DISAGREEMENT marker, escalated to the coordinator.",
			[]string{"verdict", "agent_role", "repo"},
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
		spawnOverridesTotal: metrics.NewCounterVec(
			MetricSpawnOverridesTotal,
			"Total prism spawn --over-budget overrides observed by the exporter, by repo.",
			[]string{"repo"},
		),
	}
	reg.MustRegister(lc.spawnsTotal)
	reg.MustRegister(lc.sessionsEndedTotal)
	reg.MustRegister(lc.reviewVerdictsTotal)
	reg.MustRegister(lc.reviewAgentVerdictsTotal)
	reg.MustRegister(lc.escalationsTotal)
	reg.MustRegister(lc.doomLoopsTotal)
	reg.MustRegister(lc.permissionDeniedTotal)
	reg.MustRegister(lc.spawnOverridesTotal)
	return lc
}

// apply is the tailcursor apply function for the lifecycle tailer. It is a
// closed dispatch over agent_events.type: every type this switch does not
// name is a no-op, which covers the vast majority of rows (msg_assistant,
// tool_call, and so on) that carry none of the seven counters.
//
// EVERY counter here carries a repo label, and every one of the seven folds
// empty or whitespace-only ev.Repo to the unknownRepoLabel placeholder via
// repoLabel(), to prevent unbounded label cardinality and blank
// template-variable entries. Counters are tail-cursor accumulated and
// persisted across restarts, so a label-value correction ends the old series
// and starts a new one at zero: corrected values start a new series going
// forward; pre-correction rows keep their original label value (no backfill).
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
	case spawnbudget.EventSpawnOverride:
		return lc.spawnOverridesTotal.Inc(repoLabel(ev.Repo))
	default:
		if verdict, ok := verdictLabel(ev.Type); ok {
			return lc.reviewVerdictsTotal.Inc(verdict, repoLabel(ev.Repo), ev.AgentRole, ev.ProfileName)
		}
		if verdict, ok := agentVerdictLabel(ev.Type); ok {
			// ev.AgentRole is the REVIEW agent's role here, not the worker's:
			// the per-agent event carries the review agent's instance_id, and
			// the sessions join resolves the role from it.
			return lc.reviewAgentVerdictsTotal.Inc(verdict, ev.AgentRole, repoLabel(ev.Repo))
		}
		return nil
	}
}
