// Package agent defines the typed AgentState and its canonical values.
// All state comparisons and assignments should use these constants rather
// than raw string literals so that typos produce compile errors and a grep
// for any constant surfaces every usage site.
//
// db.Status.State and tmux.Session.AgentState remain typed as string.
// Convert at call sites with string(agent.StateXxx) and agent.AgentState(s).
package agent

// AgentState is the lifecycle state of a prism agent session.
type AgentState string

const (
	StateActive      AgentState = "active"
	StateWaiting     AgentState = "waiting"
	StateFinished    AgentState = "finished"
	StateCompacting  AgentState = "compacting"
	StateError       AgentState = "error"
	StateIdle        AgentState = "idle"
	StateInterrupted AgentState = "interrupted"
	StateDeleted     AgentState = "deleted"
	// StateReviewing is a non-terminal state entered by a worker session
	// immediately after calling `prism review`. The worker stays in this state
	// until the review-complete prompt is received and resolved:
	//   - PASS verdict → transitions to finished (coordinator notified).
	//   - FAIL verdict → transitions back to active (worker fixes issues).
	// The coordinator must not receive a "has finished" notification while the
	// worker is in the reviewing state.
	StateReviewing AgentState = "reviewing"
	// StateEscalated is a non-terminal state entered by a worker session via
	// `prism escalate`. The worker has handed a question to its coordinator
	// and stops its current turn while awaiting guidance. Any incoming
	// turn_start (from `prism prompt`, a human typing into tmux, or any
	// other source) transitions the session back to active. While in this
	// state the sidecar must NOT emit the "has finished" notification — the
	// `session.escalated` bus event already informed the coordinator.
	StateEscalated AgentState = "escalated"
)

// IsTerminal reports whether s is a terminal agent state. Terminal states
// are the lifecycle endpoints for which post-hoc aggregates (token totals,
// cost, durations) are meaningful: the agent has stopped producing new
// events, so a snapshot taken at this point will not be invalidated by
// later activity.
//
// The set is the same one the cleanup pipeline and `prism stats compare`
// treat as "data is final": finished, error, interrupted, and deleted. It
// excludes the in-progress states (active, idle, waiting, compacting,
// reviewing, escalated) — those sessions may still emit events.
//
// Called by `prism stats compare` to decide whether to compute an outcome
// on the fly when no `spawn_outcome` row exists yet (issue #2102).
func IsTerminal(s AgentState) bool {
	switch s {
	case StateFinished, StateError, StateInterrupted, StateDeleted:
		return true
	}
	return false
}
