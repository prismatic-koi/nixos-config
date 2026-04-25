package agent

import "fmt"

// ValidTransitions defines the complete set of allowed agent state transitions.
// Each key is a "from" state; its value is the set of valid "to" states.
//
// Design notes:
//
//   - idle → active: the normal startup path (tmux-session-start sets idle, then
//     session.created fires active in the plugin).
//   - idle → interrupted: pane-died before the session was ever active (rare but
//     possible if the opencode process crashes before session.created fires).
//   - idle → error: container startup failure (WaitHealthy timeout or CreateSession
//     failure) before the session became active. The sidecar writes error directly
//     to avoid relying on the pane-died tmux hook for state cleanup.
//   - active → reviewing: entered by a worker immediately after calling
//     `prism review`. The worker remains here until the review-complete prompt
//     arrives. The coordinator does NOT receive a "has finished" notification
//     while the worker is in this state.
//   - reviewing → finished: PASS verdict received; coordinator is notified now.
//   - reviewing → active: FAIL verdict received; worker returns to active to
//     fix blocking issues and re-run the review.
//   - reviewing → interrupted: pane-died or SIGTERM while awaiting review results.
//   - finished → interrupted: explicitly allowed for the pane-died hook when the
//     pane exits with a non-zero code — a crash overrides a cleanly-written
//     "finished" (see UpsertStatusInterruptedOverrideFinished).
//   - finished → active: the resumed-session path — opencode reopens a session
//     that was previously closed cleanly (see session.updated in the plugin).
//   - finished → idle: tmux-session-start resets a previously-finished session
//     back to idle when the tmux session is recreated (e.g. prism restore or
//     manual kill+recreate).
//   - interrupted → active: session resumed after an interruption.
//   - interrupted → idle: same as finished → idle but starting from interrupted.
//   - * → deleted: any state can transition to deleted when session.deleted fires.
//
// The TypeScript plugin writes state directly to SQLite and is not constrained
// by this table — validation here is additive and advisory only.
var ValidTransitions = map[AgentState]map[AgentState]bool{
	StateIdle: {
		StateActive:      true,
		StateInterrupted: true,
		StateError:       true, // container startup failure before session.created
		StateDeleted:     true,
	},
	StateActive: {
		StateWaiting:     true,
		StateFinished:    true,
		StateInterrupted: true,
		StateError:       true,
		StateCompacting:  true,
		StateReviewing:   true, // prism review called — awaiting review results
		StateDeleted:     true,
	},
	StateWaiting: {
		StateActive:      true,
		StateFinished:    true,
		StateInterrupted: true,
		StateError:       true,
		StateDeleted:     true,
	},
	StateCompacting: {
		StateActive:      true,
		StateInterrupted: true,
		StateDeleted:     true,
	},
	StateError: {
		StateActive:      true,
		StateInterrupted: true,
		StateFinished:    true,
		StateDeleted:     true,
	},
	// reviewing→finished: PASS verdict received; coordinator notified now.
	// reviewing→active: FAIL verdict received; worker resumes to fix issues.
	// reviewing→interrupted: pane-died or SIGTERM while awaiting results.
	StateReviewing: {
		StateFinished:    true,
		StateActive:      true,
		StateInterrupted: true,
		StateDeleted:     true,
	},
	// finished→interrupted: non-zero pane exit overrides a clean finish.
	// finished→active: resumed-session path after prior close.
	// finished→idle: tmux-session-start resets the session on recreate.
	StateFinished: {
		StateInterrupted: true,
		StateActive:      true,
		StateIdle:        true,
		StateDeleted:     true,
	},
	// interrupted→active: session resumed after interruption.
	// interrupted→idle: tmux-session-start resets the session on recreate.
	StateInterrupted: {
		StateActive:  true,
		StateIdle:    true,
		StateDeleted: true,
	},
	// deleted is a terminal state — no outgoing transitions.
	StateDeleted: {},
}

// Transition validates whether transitioning from → to is permitted by the
// state machine. It returns nil when the transition is valid, or a descriptive
// error when it is not.
//
// Callers that must not crash on an invalid transition should log the error
// and continue rather than returning it to the caller. See the advisory-guard
// pattern used in db.go and cmd/event.go.
func Transition(from, to AgentState) error {
	toStates, ok := ValidTransitions[from]
	if !ok {
		return fmt.Errorf("agent state machine: unknown from-state %q", from)
	}
	if !toStates[to] {
		return fmt.Errorf("agent state machine: invalid transition %q → %q", from, to)
	}
	return nil
}
