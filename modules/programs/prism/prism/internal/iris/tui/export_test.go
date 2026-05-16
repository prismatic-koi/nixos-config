package tui

// export_test.go — test-only accessors that expose internal Model state to
// the external _test package. These are compiled only when `go test` runs,
// so they do not enlarge the public API surface.

// ModelSessionCount returns the number of sessions currently in the model's
// session list. Used by tui_test.go to assert dedupe behaviour.
func ModelSessionCount(m Model) int {
	return len(m.sessions)
}

// ModelSessionAt returns (name, state, role) for the session at the given
// index. Used by tui_test.go to assert per-row fields without rendering.
// If i is out of range, returns empty strings.
func ModelSessionAt(m Model, i int) (name, state, role string) {
	if i < 0 || i >= len(m.sessions) {
		return "", "", ""
	}
	s := m.sessions[i].snap
	return s.Name, s.State, s.Role
}

// ModelSubscribedTo returns the session name the model is currently
// subscribed to (the gate that prompt-send is conditioned on). Used by
// tui_test.go to assert the auto-subscribe behaviour on session_spawned
// when the list transitions from empty to non-empty.
func ModelSubscribedTo(m Model) string {
	return m.subscribedTo
}

// ModelCursor returns the cursor index. Used by tui_test.go to assert
// cursor invariants after navigation / auto-subscribe.
func ModelCursor(m Model) int {
	return m.cursor
}

// Overlay kind constants re-exported for tests. See overlay.go for the
// canonical names — these mirror them 1:1.
const (
	OverlayNone          = int(overlayNone)
	OverlayPicker        = int(overlayPicker)
	OverlaySpawnWorktree = int(overlaySpawnWorktree)
	OverlaySpawnRole     = int(overlaySpawnRole)
	OverlayDashboard     = int(overlayDashboard)
	OverlayHelp          = int(overlayHelp)
)

// ModelOverlay returns the current overlay kind as an int, matching one of
// the Overlay* constants above. Used by tests to assert overlay state
// transitions without exposing the internal overlayKind type.
func ModelOverlay(m Model) int { return int(m.overlay) }

// ModelPickerRowCount returns the number of rows currently in the picker
// overlay (the spawn row + one row per known session). Used by tests to
// assert the picker was populated from the session list.
func ModelPickerRowCount(m Model) int { return len(m.picker.rows) }

// ModelPickerFilter returns the picker's current filter string.
func ModelPickerFilter(m Model) string { return m.picker.filter }

// ModelSpawnWorktree returns the worktree-input buffer contents during the
// spawn flow. Empty when no spawn overlay is active.
func ModelSpawnWorktree(m Model) string { return string(m.spawn.worktree) }

// ModelSpawnRole returns the role-input buffer contents during the spawn flow.
func ModelSpawnRole(m Model) string { return string(m.spawn.role) }
