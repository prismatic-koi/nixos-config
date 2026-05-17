package tui_test

// renderer_test.go — tests for the bubbletea conversation-pane renderer
// introduced in issue #1767 (child 2 of the iris-tui-design tracker
// #1765). The tests drive the public Model + DaemonFrame path so they
// also exercise dispatch wiring inside handleDaemonFrame, not just the
// per-type renderer in isolation.
//
// Coverage map (one happy-path test per design-doc renderer-table row):
//
//	msg_assistant   → TestRenderer_MsgAssistant
//	tool_call       → TestRenderer_ToolCallOneLineCard
//	tool_result     → TestRenderer_ToolResultIndentedSummary
//	state_change    → TestRenderer_StateChangeDimLine
//	extension_error → TestRenderer_ExtensionErrorProminentBlock
//	session_status  → TestRenderer_SessionStatusSuppressed (edge-case AC)
//	unknown type    → TestRenderer_UnknownEventDoesNotPanic
//
// Plus tests for the status-line strip:
//
//	TestStatusLine_PopulatedFromMsgAssistant
//	TestStatusLine_DegradesWithoutModelOrCost

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/tui"
)

// deliverEvent helper: pushes a session_event DaemonFrame through the
// model and returns the new Model state. Tests use it to keep the
// per-test boilerplate down to one line per event.
func deliverEvent(t *testing.T, m tui.Model, sessionName, eventType string, rowID int64, payload any) tui.Model {
	t.Helper()
	pb, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	m2, _ := m.Update(tui.DaemonFrame{
		RawType: iris.DaemonFrameSessionEvent,
		Event: &iris.DaemonSessionEventFrame{
			Type:        iris.DaemonFrameSessionEvent,
			SessionName: sessionName,
			RowID:       rowID,
			EventType:   eventType,
			Payload:     string(pb),
		},
	})
	return m2.(tui.Model)
}

// modelWithOneSession returns a connected model subscribed to a single
// session with the given name. Test code uses this so it can immediately
// deliver session_events without rebuilding the snapshot fixture.
func modelWithOneSession(t *testing.T, name string) tui.Model {
	t.Helper()
	m := newConnectedModel()
	snap := iris.DaemonSessionsSnapshotFrame{
		Type: iris.DaemonFrameSessionsSnapshot,
		Sessions: []iris.SessionSnapshot{
			{Name: name, InstanceID: "iid-" + name, State: "active", Role: "worker", Worktree: "/repo/" + name},
		},
	}
	m2, _ := m.Update(tui.DaemonFrame{RawType: iris.DaemonFrameSessionsSnapshot, Snapshot: &snap})
	return m2.(tui.Model)
}

// ---------------------------------------------------------------------------
// Per-event-type happy-path tests
// ---------------------------------------------------------------------------

// TestRenderer_MsgAssistant asserts msg_assistant renders as a rich-text
// block in the conversation pane: a header (with "assistant" label and
// the agent/model context) and a body line carrying the message text.
func TestRenderer_MsgAssistant(t *testing.T) {
	m := modelWithOneSession(t, "msg-test")
	m = deliverEvent(t, m, "msg-test", "msg_assistant", 1, map[string]any{
		"messageId": "msg-001",
		"text":      "Booted iris and synced state.",
		"agent":     "worker",
		"model":     "anthropic/claude-sonnet-4",
	})

	view := m.View()
	if !strings.Contains(view, "assistant") {
		t.Errorf("msg_assistant header label not in view; excerpt:\n%s", excerpt(view, 600))
	}
	if !strings.Contains(view, "Booted iris and synced state.") {
		t.Errorf("msg_assistant body not in view; excerpt:\n%s", excerpt(view, 600))
	}
	// The agent/model label should appear in the header so the operator
	// can see who is speaking. Pre-existing checkin-narrative format
	// uses "agent · model" inside brackets.
	if !strings.Contains(view, "worker") || !strings.Contains(view, "claude-sonnet-4") {
		t.Errorf("msg_assistant agent/model label not in header; excerpt:\n%s", excerpt(view, 600))
	}
}

// TestRenderer_ToolCallOneLineCard asserts a fresh tool_call renders
// as a multi-line in-flight card (issue #1769 replaces child 2's
// one-line placeholder). The card carries the tool name on the
// header line, the truncated args on an args line, and an in-flight
// status marker ("⏳ running…") on the status line. The exact name of
// this test is retained from child 2 so the renderer-table coverage
// map at the top of this file still resolves to a real test — the
// assertions inside have been updated to match the post-#1769 design.
func TestRenderer_ToolCallOneLineCard(t *testing.T) {
	m := modelWithOneSession(t, "tc-test")
	m = deliverEvent(t, m, "tc-test", "tool_call", 1, map[string]any{
		"name": "bash",
		"args": map[string]any{"command": "go test ./..."},
		"id":   "call-tc-001",
	})

	view := m.View()
	if !strings.Contains(view, "bash") {
		t.Errorf("tool_call tool name not in view; excerpt:\n%s", excerpt(view, 600))
	}
	if !strings.Contains(view, "go test ./...") {
		t.Errorf("tool_call command argument not in view; excerpt:\n%s", excerpt(view, 600))
	}
	if !strings.Contains(view, "→") {
		t.Errorf("tool_call card marker '→' not in view; excerpt:\n%s", excerpt(view, 600))
	}
	// In-flight status marker must be present (distinct visual for
	// in-flight vs completed) — AC: "distinct visual styling from a
	// completed card".
	if !strings.Contains(view, "running") {
		t.Errorf("in-flight status marker 'running' not in view; excerpt:\n%s",
			excerpt(view, 600))
	}
	// One-header check: the rendered "→ bash" header should appear on
	// exactly one line. The args line is a separate row carrying the
	// args text but not the "→" marker, so this is still tight.
	headerLines := 0
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "→") && strings.Contains(l, "bash") {
			headerLines++
		}
	}
	if headerLines != 1 {
		t.Errorf("expected exactly one '→ bash' header row; got %d. view excerpt:\n%s",
			headerLines, excerpt(view, 800))
	}
}

// TestRenderer_ToolResultIndentedSummary asserts that a paired
// tool_call + tool_result renders as a single combined multi-line
// card with the result preview replacing the in-flight "⏳ running…"
// status line (issue #1769). The card stays a single block in the
// pane — no orphaned "↳ result: …" row below the header.
func TestRenderer_ToolResultIndentedSummary(t *testing.T) {
	m := modelWithOneSession(t, "tr-test")
	m = deliverEvent(t, m, "tr-test", "tool_call", 1, map[string]any{
		"name": "bash",
		"args": map[string]any{"command": "echo hi"},
		"id":   "call-tr-001",
	})
	m = deliverEvent(t, m, "tr-test", "tool_result", 2, map[string]any{
		"id":      "call-tr-001",
		"success": true,
		"output":  "hi",
	})

	view := m.View()
	// The result text must be present somewhere in the pane.
	if !strings.Contains(view, "hi") {
		t.Errorf("tool_result text not in view; excerpt:\n%s", excerpt(view, 600))
	}
	// The tool header is still present.
	if !strings.Contains(view, "bash") {
		t.Errorf("tool name not in view; excerpt:\n%s", excerpt(view, 600))
	}
	// After pairing, the in-flight "running…" placeholder must be
	// replaced — distinct visual for completed vs in-flight per AC.
	if strings.Contains(view, "running…") {
		t.Errorf("in-flight 'running…' placeholder leaked into a paired card; excerpt:\n%s",
			excerpt(view, 800))
	}
	// The result preview line should carry the "↳" marker indicating
	// it's the completed-card status line.
	if !strings.Contains(view, "↳") {
		t.Errorf("paired card missing '↳' result marker; excerpt:\n%s", excerpt(view, 800))
	}
}

// TestRenderer_StateChangeDimLine asserts that state_change events
// render as a single dim line carrying the state name and the "●"
// marker from the design-doc renderer table.
func TestRenderer_StateChangeDimLine(t *testing.T) {
	m := modelWithOneSession(t, "sc-test")
	m = deliverEvent(t, m, "sc-test", "state_change", 1, map[string]any{
		"state": "waiting",
	})

	view := m.View()
	if !strings.Contains(view, "waiting") {
		t.Errorf("state_change state not in view; excerpt:\n%s", excerpt(view, 600))
	}
	if !strings.Contains(view, "●") {
		t.Errorf("state_change marker '●' not in view; excerpt:\n%s", excerpt(view, 600))
	}
}

// TestRenderer_ExtensionErrorProminentBlock asserts that an
// extension_error event renders as a prominent block — the design-doc
// renderer table calls these out specifically because they are
// fatal-class (issue #1757). The block must show the extension path,
// the failing event name, and the error message, so the operator can
// triage without digging into logs.
func TestRenderer_ExtensionErrorProminentBlock(t *testing.T) {
	m := modelWithOneSession(t, "ee-test")
	m = deliverEvent(t, m, "ee-test", "extension_error", 1, map[string]any{
		"extensionPath": "/some/path/prism.ts",
		"event":         "tool.execute.before",
		"error":         "TypeError: undefined is not a function",
	})

	view := m.View()
	for _, want := range []string{
		"extension error",
		"tool.execute.before",
		"/some/path/prism.ts",
		"TypeError: undefined is not a function",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("extension_error block missing %q; excerpt:\n%s", want, excerpt(view, 800))
		}
	}
	// "Prominent" / "block" requirement: the block must occupy at
	// least two visual lines (header + body) so the error message is
	// readable without scrolling. We can't introspect ANSI styling
	// from a black-box test, but we can verify the body text appears
	// on a different line from the header.
	var headerLine, bodyLine string
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "extension error") {
			headerLine = l
		}
		if strings.Contains(l, "TypeError: undefined is not a function") {
			bodyLine = l
		}
	}
	if headerLine == "" || bodyLine == "" {
		t.Fatalf("expected both header and body lines; got header=%q body=%q",
			headerLine, bodyLine)
	}
	if headerLine == bodyLine {
		t.Errorf("extension_error block should span >=2 rows; got single line:\n%q", headerLine)
	}
}

// TestRenderer_SessionStatusSuppressed asserts that a session_status
// event does NOT contribute any row to the conversation pane. The
// design doc renderer table explicitly suppresses these — they belong
// to the sidebar's per-session state column. We verify by delivering a
// session_status frame and confirming the right pane's "waiting for
// events…" placeholder is still displayed (no event lines were added).
func TestRenderer_SessionStatusSuppressed(t *testing.T) {
	m := modelWithOneSession(t, "ss-test")
	m = deliverEvent(t, m, "ss-test", "session_status", 1, map[string]any{
		"session_id": "pi-some-ulid",
		"phase":      "active",
	})

	view := m.View()
	// The pane should render the empty-state placeholder — issue
	// #1770 changed the wording from "waiting for events…" to
	// "no events yet…" to better match the AC's expected language.
	// We accept either form to keep this test resilient to future
	// wording tweaks of the placeholder.
	if !strings.Contains(view, "no events yet") && !strings.Contains(view, "waiting for events") {
		t.Errorf("expected empty-state placeholder (session_status should be suppressed); "+
			"excerpt:\n%s", excerpt(view, 600))
	}
	// The literal payload-id string must not appear anywhere in the
	// rendered output — if it does, the suppression failed.
	if strings.Contains(view, "pi-some-ulid") {
		t.Errorf("session_status payload leaked into rendered view; excerpt:\n%s", excerpt(view, 600))
	}

	// Follow up with a real msg_assistant — that should render, while
	// the earlier session_status remains absent. This confirms the
	// suppression is event-type-specific, not a blanket drop.
	m = deliverEvent(t, m, "ss-test", "msg_assistant", 2, map[string]any{
		"text": "I'm here.",
	})
	view = m.View()
	if !strings.Contains(view, "I'm here.") {
		t.Errorf("subsequent msg_assistant after session_status not rendered; excerpt:\n%s",
			excerpt(view, 600))
	}
}

// TestRenderer_UnknownEventDoesNotPanic covers the edge-case AC: an
// unknown event type must not panic the renderer; we accept either
// "skipped" or "rendered as debug-visible" — this test asserts the
// no-panic invariant and that the model continues to accept further
// events afterwards.
func TestRenderer_UnknownEventDoesNotPanic(t *testing.T) {
	m := modelWithOneSession(t, "unk-test")

	// Recover from any panic so the test failure mode is "FAIL with
	// readable message", not "process aborted".
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("renderer panicked on unknown event type: %v", r)
		}
	}()

	m = deliverEvent(t, m, "unk-test", "future_event_xyz", 1, map[string]any{
		"some": "field",
	})
	// Deliver a known event afterwards to confirm the model is still
	// healthy and accepting frames.
	m = deliverEvent(t, m, "unk-test", "msg_assistant", 2, map[string]any{
		"text": "after unknown",
	})
	view := m.View()
	if !strings.Contains(view, "after unknown") {
		t.Errorf("post-unknown-event msg_assistant did not render; excerpt:\n%s",
			excerpt(view, 600))
	}
}

// ---------------------------------------------------------------------------
// Status-line strip tests
// ---------------------------------------------------------------------------

// TestStatusLine_PopulatedFromMsgAssistant asserts the bottom strip
// shows the focused session's name, state, model and cost once a
// msg_assistant event with model/cost metadata has arrived.
func TestStatusLine_PopulatedFromMsgAssistant(t *testing.T) {
	m := modelWithOneSession(t, "status-pop")

	// Baseline: with no msg_assistant yet, the strip should not
	// contain a cost figure (no model/cost captured).
	v := m.View()
	if strings.Contains(v, "$") {
		// Defensive — if the strip ever starts rendering a placeholder
		// cost we want to fail loudly so the test gets updated.
		t.Errorf("baseline status line should not contain '$' (no msg_assistant yet); "+
			"excerpt:\n%s", excerpt(v, 600))
	}

	// Deliver a msg_assistant with model + cost.
	m = deliverEvent(t, m, "status-pop", "msg_assistant", 1, map[string]any{
		"text":  "ok",
		"model": "anthropic/claude-sonnet-4",
		"cost":  0.05,
	})

	v = m.View()
	if !strings.Contains(v, "status-pop") {
		t.Errorf("status line missing session name; excerpt:\n%s", excerpt(v, 600))
	}
	if !strings.Contains(v, "active") {
		t.Errorf("status line missing session state; excerpt:\n%s", excerpt(v, 600))
	}
	if !strings.Contains(v, "claude-sonnet-4") {
		t.Errorf("status line missing model; excerpt:\n%s", excerpt(v, 600))
	}
	if !strings.Contains(v, "$0.05") {
		t.Errorf("status line missing cost; excerpt:\n%s", excerpt(v, 600))
	}

	// Deliver a second msg_assistant — cost should accumulate.
	m = deliverEvent(t, m, "status-pop", "msg_assistant", 2, map[string]any{
		"text":  "more",
		"model": "anthropic/claude-sonnet-4",
		"cost":  0.10,
	})
	v = m.View()
	if !strings.Contains(v, "$0.15") {
		t.Errorf("status line cost did not accumulate; expected $0.15; excerpt:\n%s",
			excerpt(v, 600))
	}
}

// TestStatusLine_DegradesWithoutModelOrCost asserts the strip
// gracefully degrades when no model/cost is available: it must NOT
// render the literal string "<nil>", "$0.00", or any other placeholder
// the operator would misread as live data. The AC says "When model/cost
// is absent, the strip degrades gracefully (no panic, no empty
// placeholder text rendered as a literal '<nil>')".
func TestStatusLine_DegradesWithoutModelOrCost(t *testing.T) {
	m := modelWithOneSession(t, "degrade")

	// No msg_assistant delivered → no model, no cost.
	v := m.View()

	// Must not crash, must contain the session name (degraded form).
	if !strings.Contains(v, "degrade") {
		t.Errorf("degraded status line missing session name; excerpt:\n%s", excerpt(v, 600))
	}
	if !strings.Contains(v, "active") {
		t.Errorf("degraded status line missing session state; excerpt:\n%s", excerpt(v, 600))
	}
	// Critically: must not contain the failure-mode placeholders.
	for _, bad := range []string{"<nil>", "$0.00", "$NaN", "model: "} {
		if strings.Contains(v, bad) {
			t.Errorf("degraded status line contains forbidden placeholder %q; excerpt:\n%s",
				bad, excerpt(v, 600))
		}
	}

	// Deliver a msg_assistant with neither model nor cost — strip
	// should still not render any cost / model placeholders.
	m = deliverEvent(t, m, "degrade", "msg_assistant", 1, map[string]any{
		"text": "no metadata",
	})
	v = m.View()
	for _, bad := range []string{"<nil>", "$0.00", "$NaN"} {
		if strings.Contains(v, bad) {
			t.Errorf("post-bare-msg_assistant strip contains %q; excerpt:\n%s",
				bad, excerpt(v, 600))
		}
	}
}

// TestStatusLine_NoSubscribedSession asserts that with no subscribed
// session (e.g. empty session list, pre-snapshot) the strip renders a
// "no session selected" placeholder rather than blanking the bottom
// row.
func TestStatusLine_NoSubscribedSession(t *testing.T) {
	client := tui.NewDaemonClient("/dev/null")
	m := tui.NewModel(client)
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = m2.(tui.Model)
	m2, _ = m.Update(tui.ConnectedMsg{})
	m = m2.(tui.Model)

	// Empty snapshot — no subscription.
	snap := iris.DaemonSessionsSnapshotFrame{
		Type:     iris.DaemonFrameSessionsSnapshot,
		Sessions: []iris.SessionSnapshot{},
	}
	m2, _ = m.Update(tui.DaemonFrame{RawType: iris.DaemonFrameSessionsSnapshot, Snapshot: &snap})
	m = m2.(tui.Model)

	v := m.View()
	if !strings.Contains(v, "no session selected") {
		t.Errorf("status line should show 'no session selected' placeholder when nothing subscribed; "+
			"excerpt:\n%s", excerpt(v, 600))
	}
}

// ---------------------------------------------------------------------------
// Tool-call card tests (issue #1769, child 4 of the iris-tui design)
// ---------------------------------------------------------------------------

// TestToolCard_InFlightRendering asserts the multi-line card shape on
// a fresh tool_call: header line with tool name, args line with the
// truncated args, and an in-flight status line carrying the "running"
// marker. These three rows together form one card (#1769 AC: "A
// tool_call event renders as a multi-line card with: tool name on the
// header line, args displayed (truncated to a sensible width), and
// visual styling distinct from a completed card").
func TestToolCard_InFlightRendering(t *testing.T) {
	m := modelWithOneSession(t, "tc-inflight")
	m = deliverEvent(t, m, "tc-inflight", "tool_call", 1, map[string]any{
		"name": "bash",
		"args": map[string]any{"command": "sleep 60"},
		"id":   "call-inflight-001",
	})

	view := m.View()

	// Header: "→ bash" on a line.
	headerFound := false
	argsFound := false
	statusFound := false
	for _, l := range strings.Split(view, "\n") {
		switch {
		case strings.Contains(l, "→") && strings.Contains(l, "bash"):
			headerFound = true
		case strings.Contains(l, "args:") && strings.Contains(l, "sleep 60"):
			argsFound = true
		case strings.Contains(l, "running"):
			statusFound = true
		}
	}
	if !headerFound {
		t.Errorf("tool-card header '→ bash' not found; excerpt:\n%s", excerpt(view, 800))
	}
	if !argsFound {
		t.Errorf("tool-card args line not found; excerpt:\n%s", excerpt(view, 800))
	}
	if !statusFound {
		t.Errorf("tool-card in-flight status not found; excerpt:\n%s", excerpt(view, 800))
	}

	// Card bookkeeping: one card is registered.
	if got := tui.ModelToolCardCount(m); got != 1 {
		t.Errorf("expected 1 tool card; got %d", got)
	}
}

// TestToolCard_CompletedRendering asserts that a tool_call followed by
// its matching tool_result renders as a single combined card with the
// result preview replacing the in-flight placeholder. Distinct visual
// from in-flight: the "running" marker must be gone and the "↳"
// result marker must be present.
func TestToolCard_CompletedRendering(t *testing.T) {
	m := modelWithOneSession(t, "tc-done")
	m = deliverEvent(t, m, "tc-done", "tool_call", 1, map[string]any{
		"name": "bash",
		"args": map[string]any{"command": "echo done"},
		"id":   "call-done-001",
	})
	m = deliverEvent(t, m, "tc-done", "tool_result", 2, map[string]any{
		"id":      "call-done-001",
		"success": true,
		"output":  "done",
	})

	view := m.View()

	if !strings.Contains(view, "→") || !strings.Contains(view, "bash") {
		t.Errorf("tool-card header missing after pair; excerpt:\n%s", excerpt(view, 800))
	}
	if !strings.Contains(view, "echo done") {
		t.Errorf("tool-card args missing after pair; excerpt:\n%s", excerpt(view, 800))
	}
	if !strings.Contains(view, "↳") {
		t.Errorf("completed result marker '↳' missing; excerpt:\n%s", excerpt(view, 800))
	}
	if strings.Contains(view, "running…") {
		t.Errorf("in-flight 'running…' placeholder leaked into paired card; excerpt:\n%s",
			excerpt(view, 800))
	}
	// Still exactly one card.
	if got := tui.ModelToolCardCount(m); got != 1 {
		t.Errorf("expected 1 tool card after pairing; got %d", got)
	}
}

// TestToolCard_TabExpandsAndCollapses asserts the expand/collapse
// keybinding (#1769 AC: "Pressing `tab` while the conversation pane
// has focus expands the currently-selected tool-call card to show
// full args and full result. Pressing `tab` again collapses it
// back"). We set focus to the events pane directly via the test
// helper, then drive a tab key, asserting both the model's
// expandedToolCards bookkeeping and that the line count grows /
// shrinks on toggle.
func TestToolCard_TabExpandsAndCollapses(t *testing.T) {
	m := modelWithOneSession(t, "tc-expand")
	// Use a long args + result so the expanded view has something
	// substantial to add beyond the collapsed summary.
	// Post-#1783: args is a JSON object (RawMessage on the Go
	// side). Use a Go map so json.Marshal produces a properly
	// formed object literal. Multi-line content inside the command
	// string exercises the expanded card's wrap behaviour without
	// breaking the JSON.
	longArgs := map[string]any{"command": "echo line1\necho line2\necho line3"}
	longResult := "line1\nline2\nline3\nline4\nline5"
	m = deliverEvent(t, m, "tc-expand", "tool_call", 1, map[string]any{
		"name": "bash",
		"args": longArgs,
		"id":   "call-expand-001",
	})
	m = deliverEvent(t, m, "tc-expand", "tool_result", 2, map[string]any{
		"id":      "call-expand-001",
		"success": true,
		"output":  longResult,
	})

	collapsedLines := tui.ModelEventLineCount(m)
	if tui.ModelToolCardExpanded(m, "call-expand-001") {
		t.Fatalf("card unexpectedly starts expanded")
	}

	// Land focus on events pane (the rotation path is covered by the
	// #1737 focus tests; this test only cares about the tab→expand
	// edge).
	m = tui.SetModelFocus(m, tui.FocusEvents)
	if tui.ModelFocus(m) != tui.FocusEvents {
		t.Fatalf("focus did not land on events: %d", tui.ModelFocus(m))
	}

	// Tab → expand.
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = m2.(tui.Model)
	if !tui.ModelToolCardExpanded(m, "call-expand-001") {
		t.Errorf("tab did not expand the tool card")
	}
	expandedLines := tui.ModelEventLineCount(m)
	if expandedLines <= collapsedLines {
		t.Errorf("expanded line count (%d) did not grow vs collapsed (%d)",
			expandedLines, collapsedLines)
	}
	// Expanded view should contain at least one of the inner lines
	// from the full result (which is multi-line in the input).
	view := m.View()
	if !strings.Contains(view, "line3") {
		t.Errorf("expanded card missing full-result content 'line3'; excerpt:\n%s",
			excerpt(view, 800))
	}

	// Tab again → collapse.
	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = m2.(tui.Model)
	if tui.ModelToolCardExpanded(m, "call-expand-001") {
		t.Errorf("second tab did not collapse the tool card")
	}
	if got := tui.ModelEventLineCount(m); got != collapsedLines {
		t.Errorf("collapsed line count after toggle = %d; want %d", got, collapsedLines)
	}

	// Focus must NOT have rotated to prompt — tab was consumed by the
	// expand toggle.
	if tui.ModelFocus(m) != tui.FocusEvents {
		t.Errorf("focus rotated away from events after tab-toggle; got %d", tui.ModelFocus(m))
	}
}

// TestToolCard_ExpansionSurvivesNewEvents asserts the AC: "The
// expand/collapse state survives subsequent event arrivals — receiving
// a new event while a card is expanded does not collapse it."
func TestToolCard_ExpansionSurvivesNewEvents(t *testing.T) {
	m := modelWithOneSession(t, "tc-survive")
	m = deliverEvent(t, m, "tc-survive", "tool_call", 1, map[string]any{
		"name": "bash",
		"args": map[string]any{"command": "echo a"},
		"id":   "call-survive-001",
	})

	// Expand the card.
	m = tui.SetModelFocus(m, tui.FocusEvents)
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = m2.(tui.Model)
	if !tui.ModelToolCardExpanded(m, "call-survive-001") {
		t.Fatalf("setup: card did not expand on tab")
	}

	// Deliver an unrelated event.
	m = deliverEvent(t, m, "tc-survive", "msg_assistant", 2, map[string]any{
		"text": "still here",
	})
	if !tui.ModelToolCardExpanded(m, "call-survive-001") {
		t.Errorf("card collapsed when an unrelated event arrived")
	}

	// Deliver the matching tool_result — pairing should retain
	// expansion.
	m = deliverEvent(t, m, "tc-survive", "tool_result", 3, map[string]any{
		"id":      "call-survive-001",
		"success": true,
		"output":  "a",
	})
	if !tui.ModelToolCardExpanded(m, "call-survive-001") {
		t.Errorf("card collapsed when its matching tool_result arrived")
	}
	// View should still show the assistant text after the card.
	view := m.View()
	if !strings.Contains(view, "still here") {
		t.Errorf("post-pair view missing intervening msg_assistant; excerpt:\n%s",
			excerpt(view, 800))
	}
}

// TestToolCard_MalformedArgs asserts the edge-case AC: "A tool_call
// with no args, with empty args, or with malformed args JSON does not
// panic — renders the card with a clear placeholder ('(no args)' or
// equivalent)."
//
// Post-#1783 the wire shape for args is a JSON value (object,
// usually). Each subtest below builds the payload JSON manually so
// we can inject specific edge-case shapes (missing field, null,
// empty object, malformed bytes) and prove the renderer doesn't
// panic and produces a useful placeholder.
func TestToolCard_MalformedArgs(t *testing.T) {
	cases := []struct {
		name           string
		rawPayload     string // full payload JSON
		wantNoArgsHint bool   // expect "(no args)" in view
	}{
		{
			name:           "args field missing",
			rawPayload:     `{"name":"bash","id":"call-m-1"}`,
			wantNoArgsHint: true,
		},
		{
			name:           "args null",
			rawPayload:     `{"name":"bash","id":"call-m-2","args":null}`,
			wantNoArgsHint: true,
		},
		{
			name:           "args empty object",
			rawPayload:     `{"name":"bash","id":"call-m-3","args":{}}`,
			wantNoArgsHint: true,
		},
		{
			name: "args has unrelated keys",
			// `{"foo":"bar"}` is well-formed JSON but doesn't carry
			// the per-tool key (e.g. command for bash). ToolKeyArg
			// falls through to its default "raw args" branch; we
			// accept either a placeholder or a literal echo as a
			// non-blank render.
			rawPayload:     `{"name":"bash","id":"call-m-4","args":{"foo":"bar"}}`,
			wantNoArgsHint: false,
		},
		{
			name: "args is a string literal (legacy)",
			// Some replay paths may surface args as a string. The
			// renderer must not panic; ExtractBashCommand falls
			// back to treating the string as the command verbatim.
			rawPayload:     `{"name":"bash","id":"call-m-5","args":"echo legacy"}`,
			wantNoArgsHint: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("renderer panicked on %s: %v", tc.name, r)
				}
			}()
			m := modelWithOneSession(t, "tc-malformed")
			m2, _ := m.Update(tui.DaemonFrame{
				RawType: iris.DaemonFrameSessionEvent,
				Event: &iris.DaemonSessionEventFrame{
					Type:        iris.DaemonFrameSessionEvent,
					SessionName: "tc-malformed",
					RowID:       1,
					EventType:   "tool_call",
					Payload:     tc.rawPayload,
				},
			})
			m = m2.(tui.Model)

			view := m.View()
			// Card header still renders.
			if !strings.Contains(view, "bash") {
				t.Errorf("[%s] missing tool header; excerpt:\n%s", tc.name, excerpt(view, 600))
			}
			if tc.wantNoArgsHint && !strings.Contains(view, "(no args)") {
				t.Errorf("[%s] missing '(no args)' placeholder; excerpt:\n%s",
					tc.name, excerpt(view, 600))
			}
			// Card is still registered.
			if got := tui.ModelToolCardCount(m); got != 1 {
				t.Errorf("[%s] expected 1 card; got %d", tc.name, got)
			}
		})
	}
}

// TestToolCard_OrphanToolResult asserts the edge-case AC: "A
// tool_result with no matching tool_call (e.g. the parent scrolled
// out of the in-memory window) renders as a standalone indented
// summary, same as child 2's current behaviour. Don't regress that
// path."
func TestToolCard_OrphanToolResult(t *testing.T) {
	m := modelWithOneSession(t, "tc-orphan")
	m = deliverEvent(t, m, "tc-orphan", "tool_result", 1, map[string]any{
		"id":      "call-no-parent",
		"success": true,
		"output":  "hello",
	})

	view := m.View()
	// The legacy single-line "↳ result:" path must still produce a row.
	if !strings.Contains(view, "result:") || !strings.Contains(view, "hello") {
		t.Errorf("orphan tool_result not rendered via legacy path; excerpt:\n%s",
			excerpt(view, 800))
	}
	// No card was registered — orphan results do not create cards.
	if got := tui.ModelToolCardCount(m); got != 0 {
		t.Errorf("orphan tool_result installed a card; got %d", got)
	}
}

// TestToolCard_LateToolResultPairs asserts the edge-case AC: "A
// tool_result arriving for a tool_call that has scrolled out of the
// visible viewport still pairs correctly when the user scrolls back —
// no orphan one-line tool_result rows."
//
// We can't simulate viewport scroll in unit tests directly, but we
// CAN verify the underlying invariant: a tool_result arriving after
// the tool_call's lines have been mutated by intervening events
// still finds the matching card via the MessageID index, and folds
// into the card rather than producing an orphan "↳ result:" row.
func TestToolCard_LateToolResultPairs(t *testing.T) {
	m := modelWithOneSession(t, "tc-late")
	m = deliverEvent(t, m, "tc-late", "tool_call", 1, map[string]any{
		"name": "bash",
		"args": map[string]any{"command": "sleep 1"},
		"id":   "call-late-001",
	})
	// Several intervening events shift the layout but the card
	// remains tracked by id.
	for i := 0; i < 5; i++ {
		m = deliverEvent(t, m, "tc-late", "msg_assistant", int64(i+2), map[string]any{
			"text": "interim message",
		})
	}
	m = deliverEvent(t, m, "tc-late", "tool_result", 100, map[string]any{
		"id":      "call-late-001",
		"success": true,
		"output":  "ok",
	})

	view := m.View()
	// In-flight marker must be replaced by the result preview.
	if strings.Contains(view, "running…") {
		t.Errorf("late tool_result did not replace in-flight marker; excerpt:\n%s",
			excerpt(view, 1200))
	}
	if !strings.Contains(view, "↳") {
		t.Errorf("late tool_result did not produce the completed status marker; excerpt:\n%s",
			excerpt(view, 1200))
	}
	// And NO orphan "↳ result:" line (that's the legacy fallback the
	// pair path is supposed to avoid).
	orphanCount := 0
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "↳ result:") {
			orphanCount++
		}
	}
	if orphanCount > 0 {
		t.Errorf("late tool_result produced %d orphan '↳ result:' rows; want 0", orphanCount)
	}
}

// TestToolCard_TabWithoutCardsRotatesFocus asserts that tab continues
// to rotate focus when no tool card exists. Without this fallback,
// the pre-#1769 tab-rotation behaviour would regress whenever the
// conversation pane is empty / has no tool calls.
func TestToolCard_TabWithoutCardsRotatesFocus(t *testing.T) {
	m := modelWithOneSession(t, "tc-norotate")
	m = tui.SetModelFocus(m, tui.FocusEvents)

	// No tool cards yet → tab must rotate focus to prompt.
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = m2.(tui.Model)
	if got := tui.ModelFocus(m); got != tui.FocusPrompt {
		t.Errorf("tab on empty events pane did not rotate focus to prompt; got %d", got)
	}
}
