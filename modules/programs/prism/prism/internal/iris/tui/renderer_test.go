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

// TestRenderer_ToolCallOneLineCard asserts tool_call renders as a
// single visual row containing the tool name and its primary argument
// (the design doc's "one-line card"). The row must be distinguishable
// from surrounding assistant text — we check for the leading "→"
// marker that prefixes every tool_call card.
func TestRenderer_ToolCallOneLineCard(t *testing.T) {
	m := modelWithOneSession(t, "tc-test")
	m = deliverEvent(t, m, "tc-test", "tool_call", 1, map[string]any{
		"tool":      "bash",
		"args":      `{"command":"go test ./..."}`,
		"messageId": "msg-tc-001",
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
	// One-row check: the rendered "→ bash: …" payload should appear on
	// exactly one line in the events pane. We split on '\n' and count
	// lines mentioning "→ bash" — anything other than 1 means the card
	// has spilled across rows.
	cardLines := 0
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "→") && strings.Contains(l, "bash") {
			cardLines++
		}
	}
	if cardLines != 1 {
		t.Errorf("expected exactly one '→ bash' card row; got %d. view excerpt:\n%s",
			cardLines, excerpt(view, 800))
	}
}

// TestRenderer_ToolResultIndentedSummary asserts that a tool_result
// renders as an indented summary beneath its matching tool_call. The
// model layer pairs tool_result into the tool_call line (existing
// behaviour preserved post-refactor) so we look for the result
// summary text co-located with the tool name.
func TestRenderer_ToolResultIndentedSummary(t *testing.T) {
	m := modelWithOneSession(t, "tr-test")
	m = deliverEvent(t, m, "tr-test", "tool_call", 1, map[string]any{
		"tool":      "bash",
		"args":      `{"command":"echo hi"}`,
		"messageId": "msg-tr-001",
	})
	m = deliverEvent(t, m, "tr-test", "tool_result", 2, map[string]any{
		"tool":      "bash",
		"result":    "hi",
		"messageId": "msg-tr-001",
	})

	view := m.View()
	// The result summary must be present somewhere in the pane …
	if !strings.Contains(view, "hi") {
		t.Errorf("tool_result summary not in view; excerpt:\n%s", excerpt(view, 600))
	}
	// … and co-located with its parent tool_call row, not on a
	// separate top-level line — we check by finding the line carrying
	// "bash" and verifying the result text is on the same line.
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "→") && strings.Contains(l, "bash") {
			if !strings.Contains(l, "hi") {
				t.Errorf("tool_result summary not paired into tool_call line; row:\n%q", l)
			}
			return
		}
	}
	t.Errorf("no tool_call row found at all; view excerpt:\n%s", excerpt(view, 600))
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
	if !strings.Contains(view, "waiting for events") {
		t.Errorf("expected 'waiting for events…' placeholder (session_status should be suppressed); "+
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
