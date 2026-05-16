package dashboard

// export_test.go — test-only accessors that expose internal Model state to
// the dashboard_test package. Compiled only when `go test` runs, so they
// do not enlarge the public API surface.

import "time"

// SessionCount returns the number of rows currently tracked by the model.
func SessionCount(m Model) int {
	return len(m.sessions)
}

// HasSession returns true if a row exists for the given name.
func HasSession(m Model, name string) bool {
	_, ok := m.sessions[name]
	return ok
}

// SessionState returns the in-memory state field for the named row, or
// "" if no such row exists. Used to assert that session_state frames
// updated the row.
func SessionState(m Model, name string) string {
	r, ok := m.sessions[name]
	if !ok {
		return ""
	}
	return r.snap.State
}

// SessionLastEvent returns (label, at) for the named row's most recent
// observed event. Zero values when no event has been seen.
func SessionLastEvent(m Model, name string) (string, time.Time) {
	r, ok := m.sessions[name]
	if !ok {
		return "", time.Time{}
	}
	return r.lastEventTxt, r.lastEventAt
}

// Order returns the rendered row order. Useful for asserting that a new
// session_spawned row appears in the canonical position.
func Order(m Model) []string {
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}
