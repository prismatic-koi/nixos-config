// Package agent defines the typed AgentState and its canonical values.
// All state comparisons and assignments should use these constants rather
// than raw string literals so that typos produce compile errors and a grep
// for any constant surfaces every usage site.
//
// db.Status.State and tmux.Session.AgentState remain typed as string.
// Convert at call sites with string(agent.StateXxx) and agent.AgentState(s).
package agent

// AgentState is the lifecycle state of an opencode agent session.
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
)
