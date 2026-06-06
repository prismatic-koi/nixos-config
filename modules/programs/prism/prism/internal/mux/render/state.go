package render

// State enumerates the prism session states the sidebar must visually
// distinguish per §3.1 of docs/multiplexer-proposal.md. The set mirrors
// prism's actual agent-state vocabulary so the renderer can be wired to
// the sidecar's agent_status table without translation in #2155.
//
// StateIdle is the zero value so a StateProvider that doesn't know about a
// session (or a Model constructed without a provider) reports "idle"
// rather than an undefined glyph.
type State int

const (
	// StateIdle — no in-flight turn. Zero value.
	StateIdle State = iota
	// StateActive — worker mid-turn.
	StateActive
	// StateWaiting — paused for user input (prism's `waiting` state).
	StateWaiting
	// StateReviewing — review group in progress.
	StateReviewing
	// StateEscalated — escalated to coordinator / error.
	StateEscalated
	// StateFinished — terminal, clean exit.
	StateFinished
)

// String returns the lowercase prism-state token used in the §3.1
// right-aligned state label.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateActive:
		return "active"
	case StateWaiting:
		return "waiting"
	case StateReviewing:
		return "reviewing"
	case StateEscalated:
		return "escalated"
	case StateFinished:
		return "finished"
	}
	return "unknown"
}

// StateProvider reports the prism-state for a session identified by its
// pane.Session.ID. Implementations are expected to be cheap — the renderer
// calls into them once per visible row per frame.
//
// The Model treats a nil provider as "every session is idle". An
// implementation that does not know about a session SHOULD return
// StateIdle (the zero value) so the sidebar still renders cleanly.
type StateProvider interface {
	State(sessionID string) State
}

// StateMap is the trivial StateProvider — a static map from session ID to
// state. Useful for tests and for the initial wiring step before #2155
// lands a sidecar-backed implementation.
type StateMap map[string]State

// State implements StateProvider. Unknown sessions report StateIdle.
func (m StateMap) State(id string) State { return m[id] }
