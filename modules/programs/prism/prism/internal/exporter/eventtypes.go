package exporter

import "sort"

// The closed label set behind prism_agent_events_total{type} (#2699 section 6).
//
// # Why an allowlist and not the raw column
//
// agent_events.type looks like a closed set and is not one. Three facts
// compose into an unbounded-cardinality hole:
//
//  1. internal/sidecar/sidecar.go persists an unrecognised wire frame
//     verbatim — its `default:` branch calls writeEvent(frame.Type, ...) for
//     any type it does not have a case for. That is deliberate
//     forward-compatibility, and it is correct for the database.
//  2. internal/db WriteEvent does not validate the value.
//  3. internal/container/bwrap.go bind-mounts the per-session socket
//     directory read-write into the sandbox and exports PRISM_HARNESS_PIPE,
//     so a worker agent can write frames of its own choosing.
//
// A prompt-injected agent could therefore mint an unbounded number of label
// values. Each becomes a permanent series in a FLEET-WIDE Prometheus, shipped
// there by Alloy's remote_write, and — because this exporter persists and
// reloads its counter values — one injection would survive every restart.
//
// So the exporter folds the column through the allowlist below and puts
// everything else in a single OtherEventType bucket. The series count of the
// metric is then bounded by construction at MaxAgentEventsSeries, whatever
// the database holds. The fold is applied by the CounterVec itself
// (metrics.WithLabelValueNormaliser), which covers the restore path as well
// as the tail path, so a state file cannot reintroduce a value either.
//
// # Adding a type
//
// Add the string below. TestKnownEventTypes_CoversEveryStaticallyWrittenType
// walks the prism source and fails when a writer emits a type that is not
// here. It covers three shapes:
//
//  1. a string literal at a writer site — writeEvent("x", ...) or
//     db.Event{Type: "x"};
//  2. an identifier at a writer site that resolves to a package-level string
//     constant, qualified (db.SessionReapEventType) or not;
//  3. any package-level string constant named by the event-type convention
//     (Event<X> or <X>EventType), wherever it is declared. This one matters
//     because a constant can reach db.Event.Type through a helper's
//     PARAMETER — session.EventSpawnIntent does exactly that, through
//     writeSpawnEvent — and no writer-site scan can follow that.
//
// What the scan cannot see, in full:
//
//   - a type computed at run time;
//   - a type arriving from the wire through the verbatim `default:` path;
//   - a literal RETURNED by a mapping function and forwarded to a writer
//     through a local, as internal/harness/pi NormaliseFrame does;
//   - a literal on a `case` label whose body writes frame.Type, as
//     internal/sidecar/sidecar.go does for tool_call, tool_result,
//     provider_error, and auto_retry_end.
//
// The last two are static but sit away from the writer site. Every value
// they produce today is already in the list below, so there is no gap now;
// they are named here so this comment does not claim more coverage than the
// scan delivers. OtherEventType is the backstop for all four, and a rising
// `type="other"` is the signal to come back here.

// OtherEventType is the bucket every value outside knownEventTypes folds
// into. It is deliberately a value that no real event type uses.
const OtherEventType = "other"

// knownEventTypes is the set of agent_events.type values exposed verbatim.
//
// Derived from the writers in the prism tree: the literal writeEvent calls in
// internal/sidecar, the db.Event composite literals in cmd/ and internal/,
// db.SessionReapEventType, and the wire frame types that
// internal/sidecar/sidecar.go persists from a named case rather than from its
// `default:` branch.
var knownEventTypes = map[string]struct{}{
	"audit":              {},
	"auto_retry_end":     {},
	"auto_retry_start":   {},
	"compaction":         {},
	"doom_loop_detected": {},
	"error":              {},
	"escalation":         {},
	"msg_assistant":      {},
	"msg_user":           {},
	"permission_ask":     {},
	"permission_denied":  {},
	"provider_error":     {},
	"session.escalated":  {},
	// review.verdict_pass / review.verdict_fail are written by
	// internal/review/monitor.go persistReviewOutcome (#2703): one durable
	// event per verdict-producing review round, feeding
	// prism_review_verdicts_total.
	"review.verdict_pass": {},
	"review.verdict_fail": {},
	// session.spawn_intent is written at the SpawnSession chokepoint, so it
	// occurs on EVERY spawn through every front door. Leaving it out put the
	// highest-frequency in-tree event type into the "other" bucket, which
	// would have made a rising "other" meaningless from day one.
	"session.spawn_failed": {},
	"session.spawn_intent": {},
	"session_reaped":       {},
	"session_status":       {},
	"stall_error":          {},
	"startup_error":        {},
	"state_change":         {},
	"thinking":             {},
	"tmux_session_end":     {},
	"tmux_session_start":   {},
	"tool_call":            {},
	"tool_error":           {},
	"tool_result":          {},
	"turn_end":             {},
	"turn_start":           {},
}

// MaxAgentEventsSeries is the hard upper bound on the number of series
// prism_agent_events_total can ever have: one per known type, plus the
// OtherEventType bucket.
//
// This is the number a reviewer should check the exposition against. It does
// not depend on what is in the database.
var MaxAgentEventsSeries = len(knownEventTypes) + 1

// KnownEventTypes returns the allowlist as a sorted slice. For tests and for
// operators who want to see the closed set without reading the source.
func KnownEventTypes() []string {
	out := make([]string, 0, len(knownEventTypes))
	for t := range knownEventTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// EventTypeLabel folds one agent_events.type value into the closed label set.
func EventTypeLabel(eventType string) string {
	if _, ok := knownEventTypes[eventType]; ok {
		return eventType
	}
	return OtherEventType
}

// eventTypeNormaliser is the metrics.WithLabelValueNormaliser function for
// prism_agent_events_total. It folds the single `type` label value and leaves
// the arity untouched.
func eventTypeNormaliser(labelValues []string) []string {
	if len(labelValues) != 1 {
		// Arity is wrong; return it unchanged and let CounterVec reject it
		// with a message that names the metric.
		return labelValues
	}
	return []string{EventTypeLabel(labelValues[0])}
}
