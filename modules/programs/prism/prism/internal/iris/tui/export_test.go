package tui

import "time"

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
	OverlayNone              = int(overlayNone)
	OverlayPicker            = int(overlayPicker)
	OverlaySpawnWorktree     = int(overlaySpawnWorktree)
	OverlaySpawnRole         = int(overlaySpawnRole)
	OverlayDashboard         = int(overlayDashboard)
	OverlayHelp              = int(overlayHelp)
	OverlayCoordinatorEvents = int(overlayCoordinatorEvents)
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

// ModelSessionLastEventAt returns the sidebar's tracked last-event arrival
// time for the session at the given index, or the zero time when the
// index is out of range. Returns a time.Time pointer rather than the
// value so tests can distinguish "no session at that index" (nil) from
// "session present but no event yet" (non-nil zero-valued time).
func ModelSessionLastEventAt(m Model, i int) *time.Time {
	if i < 0 || i >= len(m.sessions) {
		return nil
	}
	t := m.sessions[i].lastEventAt
	return &t
}

// ModelSessionLastAssistantPreview returns the cached one-line preview
// of the most recent msg_assistant text for the session at the given
// index, or "" when no preview has been captured (or i is out of range).
func ModelSessionLastAssistantPreview(m Model, i int) string {
	if i < 0 || i >= len(m.sessions) {
		return ""
	}
	return m.sessions[i].lastAssistantPreview
}

// ModelCoordinatorEventCount returns the number of coordinator events
// the model has accumulated (escalations + merge-queue notifications,
// issue #1772). Used by tests to assert the accumulator wiring without
// reaching inside the private slice.
func ModelCoordinatorEventCount(m Model) int {
	return len(m.coordinatorEvents)
}

// ModelCoordinatorEventSummaryAt returns the human-readable summary
// string of the coordinator event at the given index, or "" when out
// of range. Tests use this to assert that an escalation or merge-queue
// notification flowed into the buffer with the expected payload.
func ModelCoordinatorEventSummaryAt(m Model, i int) string {
	if i < 0 || i >= len(m.coordinatorEvents) {
		return ""
	}
	return m.coordinatorEvents[i].summary
}

// ModelErrorMsg returns the current transient errorMsg (e.g. the
// "not applicable" message a C-o keypress sets on a non-coordinator
// session). Empty when no error is set.
func ModelErrorMsg(m Model) string { return m.errorMsg }

// IsMergeQueueNotificationText re-exports the package-private merge-
// queue text matcher so coordinator_test.go can drive its full
// keyword/prefix table without duplicating the helper.
func IsMergeQueueNotificationText(s string) bool {
	return isMergeQueueNotificationText(s)
}

// Focus rotation states re-exported for tests. The integer values
// mirror the unexported focusArea iota in model.go.
const (
	FocusPrompt   = int(focusPrompt)
	FocusSessions = int(focusSessions)
	FocusEvents   = int(focusEvents)
)

// ModelFocus returns the current focus area as an int matching one of
// the Focus* constants. Used by #1769 tests to assert that the events
// pane is focused before exercising the tab→expand path.
func ModelFocus(m Model) int { return int(m.focus) }

// ModelToolCardCount returns the number of tool-call cards currently
// in the model's event buffer (issue #1769). Zero when no tool_call
// events have been seen for the subscribed session.
func ModelToolCardCount(m Model) int { return len(m.toolCards) }

// ModelToolCardExpanded returns true when the tool card with the
// given MessageID is currently in the expanded state. Returns false
// when the id is unknown or the card is collapsed.
func ModelToolCardExpanded(m Model, msgID string) bool {
	return m.expandedToolCards[msgID]
}

// ModelEventLineCount returns the number of NarrativeLines currently
// in the model's event buffer. Used to assert that expand/collapse
// changes the rendered line count.
func ModelEventLineCount(m Model) int { return len(m.eventLines) }

// SetModelFocus forces the model's focus to the given area. Tests use
// this to land focus on the events pane without driving the full
// tab-rotation sequence — the rotation is covered by the existing
// #1737 tests; #1769 tests focus on the expand/collapse semantics.
func SetModelFocus(m Model, focus int) Model {
	m.focus = focusArea(focus)
	return m
}

// ---------------------------------------------------------------------------
// Issue #1770 child 5 — viewport / scroll / lazy-load accessors.
// ---------------------------------------------------------------------------

// ModelViewportFollowing returns true when the conversation pane
// viewport is in auto-tail mode (sticking to the bottom of the
// buffer). False when the operator has scrolled up to read history.
func ModelViewportFollowing(m Model) bool {
	return m.viewport.Following()
}

// ModelViewportOffset returns the index of the topmost visible line
// in the conversation pane's line buffer. Zero means "at the top".
func ModelViewportOffset(m Model) int {
	return m.viewport.Offset()
}

// ModelViewportAtBottom returns true when the conversation pane is
// currently showing the tail of the buffer.
func ModelViewportAtBottom(m Model) bool {
	return m.viewport.AtBottom(len(m.eventLines))
}

// ModelViewportAtTop returns true when the conversation pane is
// currently showing the head of the buffer.
func ModelViewportAtTop(m Model) bool {
	return m.viewport.AtTop()
}

// ModelPendingNewCount returns the number of new events that have
// arrived while the viewport was scrolled up. Drives the "↓ N new"
// status-line indicator.
func ModelPendingNewCount(m Model) int {
	return m.pendingNewCount
}

// ModelHistoryExhausted returns true once the TUI knows the head of
// the subscribed session's history has been reached.
func ModelHistoryExhausted(m Model) bool {
	return m.historyExhausted
}

// ModelHistoryOldestRowID returns the smallest agent_events.rowid the
// TUI has observed for the subscribed session. Used as the
// `before_row_id` of the next session_history request.
func ModelHistoryOldestRowID(m Model) int64 {
	return m.historyOldestRowID
}

// ModelHistoryRequestInFlight returns true while a session_history
// request is outstanding. Prevents duplicate concurrent requests.
func ModelHistoryRequestInFlight(m Model) bool {
	return m.historyRequestInFlight
}

// SetModelHistoryPageSize overrides the session_history page size on
// the model. Tests use a small page size so the lazy-load behaviour
// can be exercised without seeding hundreds of fixture events.
func SetModelHistoryPageSize(m Model, n int) Model {
	m.historyPageSize = n
	return m
}

// SetModelViewportHeight forces the viewport's content height. Tests
// use this to drive PgUp/PgDn page-size arithmetic without going
// through a full render. The model's rightPaneHeight() depends on
// the WindowSizeMsg-supplied width/height; this helper bypasses the
// frame size for unit-test predictability.
func SetModelViewportHeight(m Model, h int) Model {
	m.viewport.height = h
	m.viewport.Update(len(m.eventLines), h)
	return m
}

// ModelEventPaneEmptyPlaceholder returns the literal string the
// conversation pane uses for the empty-state row (no events yet).
// Test-only so the matching assertion does not have to track the
// exact placeholder text across PRs.
func ModelEventPaneEmptyPlaceholder() string {
	return "no events yet"
}
