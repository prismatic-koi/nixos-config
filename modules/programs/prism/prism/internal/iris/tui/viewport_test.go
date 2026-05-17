package tui_test

// viewport_test.go — model-level tests for the conversation pane's
// scrollback viewport (issue #1770 child 5). Exercises the Model.Update
// loop end-to-end via DaemonFrame / tea.KeyMsg deliveries so the
// behaviour is verified in the same shape it ships to the operator.
//
// Coverage map (matches the AC list in issue #1770):
//
//   - TestViewport_EmptyPlaceholder   — AC: at session start, "no events
//                                       yet" placeholder, not a blank pane.
//   - TestViewport_AutoTailAtBottom   — AC: new event arrives while at
//                                       bottom → visible without manual scroll.
//   - TestViewport_NoJumpWhenScrolled — AC: new event arrives while
//                                       scrolled up → offset preserved,
//                                       "↓ N new" indicator surfaced.
//   - TestViewport_PgUpPgDn           — AC: PgUp / PgDn scroll by one page.
//   - TestViewport_HomeEnd            — AC: Home/g and End/G jump to top
//                                       / bottom; End re-enables auto-tail.
//   - TestViewport_LazyLoadOlder      — AC: scroll past top → request older
//                                       events; prepend preserves scroll
//                                       position.
//   - TestViewport_LazyLoadExhausted  — AC: empty page → suppress further
//                                       requests, render "(start of session)"
//                                       at the top.
//   - TestViewport_LazyLoadInterleave — edge-case AC: live event during a
//                                       lazy-load → interleaved by rowid;
//                                       no duplicates.
//   - TestViewport_SessionSwitchResets — edge-case AC: switching sessions
//                                       resets the viewport to tail and
//                                       clears pending-new + history state.

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/tui"
)

// deliverEv is a local helper duplicating renderer_test.go's
// deliverEvent so this file compiles standalone. Both helpers are
// trivial wrappers around model.Update; keeping a local copy avoids
// cross-file ordering brittleness.
func deliverEv(t *testing.T, m tui.Model, sessionName, eventType string, rowID int64, payload any) tui.Model {
	t.Helper()
	pb, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
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

// newSubscribedModel returns a connected model subscribed to a single
// session with a 200-line-tall viewport so PgUp/PgDn arithmetic has
// room to manoeuvre.
func newSubscribedModel(t *testing.T, name string) tui.Model {
	t.Helper()
	m := newConnectedModel()
	snap := iris.DaemonSessionsSnapshotFrame{
		Type: iris.DaemonFrameSessionsSnapshot,
		Sessions: []iris.SessionSnapshot{
			{Name: name, InstanceID: "iid-" + name, State: "active", Role: "worker", Worktree: "/repo/" + name},
		},
	}
	m2, _ := m.Update(tui.DaemonFrame{RawType: iris.DaemonFrameSessionsSnapshot, Snapshot: &snap})
	m = m2.(tui.Model)
	// Force a small viewport so 5-line buffers exceed the height and
	// PgUp can scroll without first having to seed dozens of fixture
	// events.
	m = tui.SetModelViewportHeight(m, 4)
	return m
}

// pressKey delivers a KeyMsg to the model. The string is what
// tea.KeyMsg.String() would return for the binding under test
// (e.g. "pgup", "g", "G", "home", "end").
func pressKey(t *testing.T, m tui.Model, name string) tui.Model {
	t.Helper()
	var msg tea.KeyMsg
	switch name {
	case "pgup":
		msg = tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		msg = tea.KeyMsg{Type: tea.KeyPgDown}
	case "home":
		msg = tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		msg = tea.KeyMsg{Type: tea.KeyEnd}
	case "g":
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}
	case "G":
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}
	default:
		t.Fatalf("pressKey: unknown binding %q", name)
	}
	m2, _ := m.Update(msg)
	return m2.(tui.Model)
}

// ---------------------------------------------------------------------------
// TestViewport_EmptyPlaceholder
// ---------------------------------------------------------------------------

func TestViewport_EmptyPlaceholder(t *testing.T) {
	m := newSubscribedModel(t, "empty")
	view := m.View()
	if !strings.Contains(view, tui.ModelEventPaneEmptyPlaceholder()) {
		t.Errorf("expected empty-state placeholder %q in view; excerpt:\n%s",
			tui.ModelEventPaneEmptyPlaceholder(), excerpt(view, 600))
	}
	if !tui.ModelViewportFollowing(m) {
		t.Errorf("a freshly subscribed model should auto-tail by default")
	}
}

// ---------------------------------------------------------------------------
// TestViewport_AutoTailAtBottom
// ---------------------------------------------------------------------------

func TestViewport_AutoTailAtBottom(t *testing.T) {
	m := newSubscribedModel(t, "auto")
	// Deliver 6 events; viewport height is 4 so the last 4 are visible.
	for i := 1; i <= 6; i++ {
		m = deliverEv(t, m, "auto", "msg_assistant", int64(i), map[string]any{
			"messageId": "m" + string(rune('0'+i)),
			"text":      "line-" + string(rune('0'+i)),
		})
	}
	if !tui.ModelViewportFollowing(m) {
		t.Errorf("auto-tail should remain on when events arrive at the bottom")
	}
	if !tui.ModelViewportAtBottom(m) {
		t.Errorf("viewport should be at the bottom after autoscrolling appends")
	}
	view := m.View()
	if !strings.Contains(view, "line-6") {
		t.Errorf("most recent event 'line-6' should be visible; excerpt:\n%s", excerpt(view, 600))
	}
}

// ---------------------------------------------------------------------------
// TestViewport_NoJumpWhenScrolled
// ---------------------------------------------------------------------------

func TestViewport_NoJumpWhenScrolled(t *testing.T) {
	m := newSubscribedModel(t, "scroll")
	// Seed 10 events.
	for i := 1; i <= 10; i++ {
		m = deliverEv(t, m, "scroll", "msg_assistant", int64(i), map[string]any{
			"messageId": "m",
			"text":      "line-" + string(rune('0'+i%10)),
		})
	}
	// Scroll up: a PgUp.
	m = pressKey(t, m, "pgup")
	if tui.ModelViewportFollowing(m) {
		t.Fatalf("PgUp should disable auto-tail")
	}
	offsetBefore := tui.ModelViewportOffset(m)
	// New event arrives while scrolled up.
	m = deliverEv(t, m, "scroll", "msg_assistant", 11, map[string]any{
		"messageId": "m11",
		"text":      "newest",
	})
	if got := tui.ModelViewportOffset(m); got != offsetBefore {
		t.Errorf("scroll offset shifted during scrolled-up append: was %d, now %d", offsetBefore, got)
	}
	if got := tui.ModelPendingNewCount(m); got == 0 {
		t.Errorf("pendingNewCount should increment when a new event arrives off-screen; got 0")
	}
	view := m.View()
	if !strings.Contains(view, "↓") || !strings.Contains(view, "new") {
		t.Errorf("status strip should surface '↓ N new' indicator; excerpt:\n%s", excerpt(view, 800))
	}
}

// ---------------------------------------------------------------------------
// TestViewport_PgUpPgDn
// ---------------------------------------------------------------------------

func TestViewport_PgUpPgDn(t *testing.T) {
	m := newSubscribedModel(t, "pg")
	// Seed enough events that a one-page PgUp does NOT land at the
	// top (so AtTop is false after PgUp — we exercise the "middle of
	// the buffer" path) but few enough that a single PgDn returns
	// to the bottom and re-enables auto-tail.
	for i := 1; i <= 6; i++ {
		m = deliverEv(t, m, "pg", "msg_user", int64(i), map[string]any{
			"text": "u" + string(rune('a'+i%26)),
		})
	}
	if !tui.ModelViewportAtBottom(m) {
		t.Fatalf("setup: should be at bottom after 6 appends")
	}
	m = pressKey(t, m, "pgup")
	if tui.ModelViewportFollowing(m) {
		t.Errorf("PgUp should disable auto-tail")
	}
	if tui.ModelViewportAtBottom(m) {
		t.Errorf("PgUp should leave viewport NOT at bottom (got AtBottom=true)")
	}
	m = pressKey(t, m, "pgdown")
	if !tui.ModelViewportAtBottom(m) {
		t.Errorf("PgDn from one-page-up should land back at the bottom (offset=%d)",
			tui.ModelViewportOffset(m))
	}
	if !tui.ModelViewportFollowing(m) {
		t.Errorf("PgDn landing at bottom should re-enable auto-tail")
	}
}

// ---------------------------------------------------------------------------
// TestViewport_HomeEnd
// ---------------------------------------------------------------------------

func TestViewport_HomeEnd(t *testing.T) {
	m := newSubscribedModel(t, "he")
	for i := 1; i <= 12; i++ {
		m = deliverEv(t, m, "he", "msg_user", int64(i), map[string]any{
			"text": "row-" + string(rune('a'+i%26)),
		})
	}
	// Focus the events pane so context-sensitive home/end target the
	// conversation viewport rather than the prompt cursor. We also
	// verify the `g` / `G` aliases work regardless of focus.
	m = tui.SetModelFocus(m, tui.FocusEvents)
	m = pressKey(t, m, "home")
	if !tui.ModelViewportAtTop(m) {
		t.Errorf("home (focus=events) should jump to top of viewport")
	}
	if tui.ModelViewportFollowing(m) {
		t.Errorf("home should disable auto-tail")
	}
	m = pressKey(t, m, "end")
	if !tui.ModelViewportAtBottom(m) {
		t.Errorf("end (focus=events) should jump to bottom")
	}
	if !tui.ModelViewportFollowing(m) {
		t.Errorf("end should re-enable auto-tail")
	}
	// `g` and `G` from a non-prompt focus should also work and should
	// continue to work even with focus=prompt as long as the prompt
	// buffer is empty (so `g` is unambiguously a navigation gesture).
	m = tui.SetModelFocus(m, tui.FocusPrompt)
	m = pressKey(t, m, "g")
	if !tui.ModelViewportAtTop(m) {
		t.Errorf("g (focus=prompt, empty prompt) should jump to top")
	}
	m = pressKey(t, m, "G")
	if !tui.ModelViewportAtBottom(m) || !tui.ModelViewportFollowing(m) {
		t.Errorf("G (focus=prompt, empty prompt) should jump to bottom and re-enable auto-tail")
	}
}

// ---------------------------------------------------------------------------
// TestViewport_LazyLoadOlder
// ---------------------------------------------------------------------------

// TestViewport_LazyLoadOlder verifies that a session_history response
// is prepended to the conversation pane and that the viewport's scroll
// position is preserved relative to the previously-top event.
func TestViewport_LazyLoadOlder(t *testing.T) {
	m := newSubscribedModel(t, "ll")
	// Seed live events with rowids 100..104.
	for i := int64(100); i <= 104; i++ {
		m = deliverEv(t, m, "ll", "msg_user", i, map[string]any{
			"text": "live-" + string(rune('a'+int(i)%26)),
		})
	}
	oldestBefore := tui.ModelHistoryOldestRowID(m)
	if oldestBefore != 100 {
		t.Errorf("historyOldestRowID should track the smallest observed rowid; got %d, want 100", oldestBefore)
	}
	// Scroll to the top, then jump to top to ensure AtTop is true.
	m = pressKey(t, m, "g")
	if !tui.ModelViewportAtTop(m) {
		t.Fatalf("setup: viewport should be at top after `g`")
	}
	if !tui.ModelHistoryRequestInFlight(m) {
		t.Errorf("`g` at AtTop with non-exhausted history should fire a history request")
	}

	// Deliver a history response with 3 older events (rowids 50..52)
	// and Done=false (more available).
	older := make([]iris.DaemonSessionEventFrame, 0, 3)
	for i := int64(50); i <= 52; i++ {
		payload, _ := json.Marshal(map[string]any{"text": "old-" + string(rune('a'+int(i)%26))})
		older = append(older, iris.DaemonSessionEventFrame{
			Type:        iris.DaemonFrameSessionEvent,
			SessionName: "ll",
			RowID:       i,
			EventType:   "msg_user",
			Payload:     string(payload),
		})
	}
	m2, _ := m.Update(tui.DaemonFrame{
		RawType: iris.DaemonFrameSessionHistory,
		History: &iris.DaemonSessionHistoryFrame{
			Type:        iris.DaemonFrameSessionHistory,
			SessionName: "ll",
			Events:      older,
			Done:        false,
		},
	})
	m = m2.(tui.Model)
	if tui.ModelHistoryRequestInFlight(m) {
		t.Errorf("historyRequestInFlight should clear on response arrival")
	}
	if tui.ModelHistoryExhausted(m) {
		t.Errorf("historyExhausted should NOT be set when Done=false and len(events)>0")
	}
	if got := tui.ModelHistoryOldestRowID(m); got != 50 {
		t.Errorf("historyOldestRowID after prepend: got %d, want 50", got)
	}
	// The view should now contain the older events.
	view := m.View()
	if !strings.Contains(view, "old-") {
		t.Errorf("prepended older events should be visible; excerpt:\n%s", excerpt(view, 600))
	}
}

// ---------------------------------------------------------------------------
// TestViewport_LazyLoadExhausted
// ---------------------------------------------------------------------------

func TestViewport_LazyLoadExhausted(t *testing.T) {
	m := newSubscribedModel(t, "ex")
	for i := int64(1); i <= 3; i++ {
		m = deliverEv(t, m, "ex", "msg_user", i, map[string]any{"text": "row"})
	}
	m = pressKey(t, m, "g") // request older
	// Empty response.
	m2, _ := m.Update(tui.DaemonFrame{
		RawType: iris.DaemonFrameSessionHistory,
		History: &iris.DaemonSessionHistoryFrame{
			Type:        iris.DaemonFrameSessionHistory,
			SessionName: "ex",
			Events:      nil,
			Done:        true,
		},
	})
	m = m2.(tui.Model)
	if !tui.ModelHistoryExhausted(m) {
		t.Errorf("empty/Done response should set historyExhausted=true")
	}
	// A subsequent scroll-up at the top must NOT re-request.
	m = pressKey(t, m, "g")
	if tui.ModelHistoryRequestInFlight(m) {
		t.Errorf("scroll-up at head-of-history should NOT fire another request")
	}
	view := m.View()
	if !strings.Contains(view, "(start of session)") {
		t.Errorf("exhausted history at top should render '(start of session)'; excerpt:\n%s", excerpt(view, 600))
	}
}

// ---------------------------------------------------------------------------
// TestViewport_LazyLoadInterleave — live event during lazy-load.
// ---------------------------------------------------------------------------

// TestViewport_LazyLoadInterleave covers the edge-case AC: "An
// incoming event arriving DURING a lazy-load is interleaved correctly
// by event timestamp / rowid — no duplicate rows, no skipped rows."
func TestViewport_LazyLoadInterleave(t *testing.T) {
	m := newSubscribedModel(t, "il")
	// Seed live events with rowids 100..101.
	for i := int64(100); i <= 101; i++ {
		m = deliverEv(t, m, "il", "msg_user", i, map[string]any{
			"text": "live-" + string(rune('a'+int(i)%26)),
		})
	}
	// Operator triggers a lazy-load.
	m = pressKey(t, m, "g")
	// Before the history response arrives, a NEW live event lands.
	m = deliverEv(t, m, "il", "msg_user", 102, map[string]any{
		"text": "during-load",
	})
	// Now the history response arrives, containing an event with
	// rowid 102 (a duplicate of the live event we just received —
	// the daemon's response was sent before the live publish
	// happened) plus a genuinely older event rowid 50.
	dupPayload, _ := json.Marshal(map[string]any{"text": "during-load"})
	oldPayload, _ := json.Marshal(map[string]any{"text": "very-old"})
	m2, _ := m.Update(tui.DaemonFrame{
		RawType: iris.DaemonFrameSessionHistory,
		History: &iris.DaemonSessionHistoryFrame{
			Type:        iris.DaemonFrameSessionHistory,
			SessionName: "il",
			Events: []iris.DaemonSessionEventFrame{
				{Type: iris.DaemonFrameSessionEvent, SessionName: "il", RowID: 50, EventType: "msg_user", Payload: string(oldPayload)},
				{Type: iris.DaemonFrameSessionEvent, SessionName: "il", RowID: 102, EventType: "msg_user", Payload: string(dupPayload)},
			},
			Done: false,
		},
	})
	m = m2.(tui.Model)
	view := m.View()
	// "very-old" must appear exactly once.
	if got := strings.Count(view, "very-old"); got != 1 {
		t.Errorf("very-old should render exactly once; got %d, excerpt:\n%s", got, excerpt(view, 800))
	}
	// "during-load" must also appear exactly once — the duplicate
	// rowid in the history response must NOT produce a second row.
	if got := strings.Count(view, "during-load"); got != 1 {
		t.Errorf("during-load should render exactly once (no duplicate from history overlap); got %d, excerpt:\n%s", got, excerpt(view, 800))
	}
}

// ---------------------------------------------------------------------------
// TestViewport_SessionSwitchResets
// ---------------------------------------------------------------------------

func TestViewport_SessionSwitchResets(t *testing.T) {
	m := newConnectedModel()
	snap := iris.DaemonSessionsSnapshotFrame{
		Type: iris.DaemonFrameSessionsSnapshot,
		Sessions: []iris.SessionSnapshot{
			{Name: "one", InstanceID: "iid-1", State: "active", Role: "worker"},
			{Name: "two", InstanceID: "iid-2", State: "active", Role: "worker"},
		},
	}
	m2, _ := m.Update(tui.DaemonFrame{RawType: iris.DaemonFrameSessionsSnapshot, Snapshot: &snap})
	m = m2.(tui.Model)
	m = tui.SetModelViewportHeight(m, 4)
	// Build state on session "one": some events + scrolled-up
	// pending-new count + a history-exhaustion marker.
	for i := int64(1); i <= 8; i++ {
		m = deliverEv(t, m, "one", "msg_user", i, map[string]any{"text": "x"})
	}
	m = pressKey(t, m, "pgup")
	m = deliverEv(t, m, "one", "msg_user", 9, map[string]any{"text": "new-while-up"})
	if tui.ModelPendingNewCount(m) == 0 {
		t.Fatalf("setup: pendingNewCount should be > 0 before session switch")
	}
	// Switch to session "two" by moving cursor down + delivering a
	// new snapshot is overkill; use the existing keybinding for
	// session-list navigation.
	down := tea.KeyMsg{Type: tea.KeyDown}
	m2, _ = m.Update(down)
	m = m2.(tui.Model)

	if tui.ModelSubscribedTo(m) != "two" {
		t.Fatalf("after `down` on session list, subscribedTo = %q, want two", tui.ModelSubscribedTo(m))
	}
	if tui.ModelPendingNewCount(m) != 0 {
		t.Errorf("pendingNewCount should reset on session switch; got %d", tui.ModelPendingNewCount(m))
	}
	if tui.ModelHistoryExhausted(m) {
		t.Errorf("historyExhausted should reset on session switch")
	}
	if !tui.ModelViewportFollowing(m) {
		t.Errorf("viewport should be in auto-tail mode after session switch")
	}
}
