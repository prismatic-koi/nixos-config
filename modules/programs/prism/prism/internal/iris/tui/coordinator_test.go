package tui_test

// coordinator_test.go — tests for the coordinator-only affordances added
// in issue #1772 (child 7 of the iris-tui-design tracker #1765).
//
// Coverage map:
//
//	IsCoordinatorSessionName          → TestIsCoordinatorSessionName
//	isMergeQueueNotificationText      → TestIsMergeQueueNotificationText
//	session.escalated rendering       → TestCoordinator_EscalationProminent /
//	                                    TestCoordinator_EscalationNotProminentOnWorker
//	msg_user merge-queue re-label     → TestCoordinator_MergeQueueProminent
//	C-o on coordinator opens overlay  → TestCoordinator_CtrlO_OpensOverlay
//	C-o on worker is soft no-op       → TestCoordinator_CtrlO_NoopOnWorker
//	overlay close on Esc              → TestCoordinator_OverlayCloseOnEsc
//	empty-state placeholder           → TestCoordinator_OverlayEmptyState
//	accumulator bounding              → TestCoordinator_AccumulatorBuffered

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/tui"
)

// ---------------------------------------------------------------------------
// Pure-function unit tests
// ---------------------------------------------------------------------------

// TestIsCoordinatorSessionName covers the heuristic table laid out in
// the AC: a coordinator (`<repo>@main`), a worker (`<repo>@<feature>`),
// a review child (`<repo>@<feature>~review-N-<agent>`), and an
// investigator (`<repo>@<feature>~investigate-...`). The function must
// also handle malformed names without panicking.
func TestIsCoordinatorSessionName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// AC-mandated cases.
		{"nixos-config@main", true},
		{"nixos-config@feature-branch", false},
		{"nixos-config@feature~review-1-review-goal", false},
		{"nixos-config@feature~investigate-foo", false},

		// master is also a recognised main branch.
		{"some-repo@master", true},

		// "main" as substring of a longer branch must NOT match.
		{"nixos-config@maintenance", false},
		{"nixos-config@main-rework", false},

		// Review child off main (uncommon but possible): still not a
		// coordinator because the tilde infix rules it out.
		{"nixos-config@main~review-1-review-code", false},

		// Defensive shapes.
		{"", false},
		{"nixos-config", false}, // no @-separator
		{"@main", false},        // empty repo segment rejected — a real session always has a non-empty repo prefix
		{"nixos-config@", false},
		{"nixos-config@trunk", false}, // not in the recognised set
	}
	for _, tc := range cases {
		got := tui.IsCoordinatorSessionName(tc.name)
		if got != tc.want {
			t.Errorf("IsCoordinatorSessionName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIsMergeQueueNotificationText asserts the substring-match table
// for merge-queue notification text. Every phrasing produced by
// internal/mergequeue/watcher.go must be detected; free-form user
// prompts that happen to mention PR numbers must NOT be detected.
func TestIsMergeQueueNotificationText(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		// Real watcher emissions (verbatim from succeedAndNotify /
		// failAndNotify in internal/mergequeue/watcher.go).
		{"PR #1772 merged. Run `git pull` in @main and `prism cleanup` the worker session.", true},
		{"PR #1772 merged. Archive: /var/lib/.../archive. Run `git pull` ...", true},
		{"PR #1772 has merge conflicts — worker rebase needed", true},
		{"PR #1772 CI failed — needs worker fix", true},
		{"PR #1772 was closed without merging — removed from queue", true},
		{"PR #1772 is blocked — human reviewer approval required before merge", true},
		{"PR #1772 merge failed: something else went wrong", true},

		// Operator prompts that talk about PRs but are NOT watcher emissions.
		{"please review PR #1772", false},
		{"PR #1772 looks good", false},
		{"discuss PR #1772 with the team", false},
		{"", false},
		{"PR # is bad", false},

		// Defensive shapes.
		{"PR #1772", false}, // prefix only, no keyword
	}
	for _, tc := range cases {
		got := tui.IsMergeQueueNotificationText(tc.text)
		if got != tc.want {
			t.Errorf("IsMergeQueueNotificationText(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Rendering / wiring tests
// ---------------------------------------------------------------------------

// modelWithCoordinatorSession returns a connected model focused on a
// session named like a coordinator (`<repo>@main`). Used by the
// prominence tests so we don't repeat the snapshot-fixture boilerplate.
func modelWithCoordinatorSession(t *testing.T, name string) tui.Model {
	t.Helper()
	m := newConnectedModel()
	snap := iris.DaemonSessionsSnapshotFrame{
		Type: iris.DaemonFrameSessionsSnapshot,
		Sessions: []iris.SessionSnapshot{
			{Name: name, InstanceID: "iid-" + name, State: "active", Role: "coordinator", Worktree: "/repo/" + name},
		},
	}
	m2, _ := m.Update(tui.DaemonFrame{RawType: iris.DaemonFrameSessionsSnapshot, Snapshot: &snap})
	return m2.(tui.Model)
}

// TestCoordinator_EscalationProminent asserts that a session.escalated
// event delivered to a coordinator-focused session renders prominently
// — the row must contain the escalation glyph (⚠) and the target
// coordinator's name. We can't introspect ANSI styling from a
// black-box test, but we can verify the event is in the buffer and
// the row text follows the documented format.
//
// This test exercises the coordinator-stream wire path: the daemon's
// writeSessionEscalatedEvent (post-#1772 fix) publishes a second
// frame under the target coordinator's SessionName so a
// coordinator-focused TUI receives it without subscribing to every
// worker. The event SessionName is the coordinator; the payload's
// `source` field names the escalating worker.
func TestCoordinator_EscalationProminent(t *testing.T) {
	const coord = "nixos-config@main"
	const worker = "nixos-config@feature-x"
	m := modelWithCoordinatorSession(t, coord)

	// Pre-condition: coordinator detection wired correctly.
	if got, want := tui.IsCoordinatorSessionName(coord), true; got != want {
		t.Fatalf("setup: IsCoordinatorSessionName(%q) = %v, want %v", coord, got, want)
	}

	// Deliver a session.escalated frame routed via the coordinator's
	// subscription (e.SessionName == coord).
	m = deliverEvent(t, m, coord, "session.escalated", 1, map[string]any{
		"source":      worker,
		"target":      coord,
		"prompt":      "I am stuck on review cycle 3, need direction",
		"delivery_id": "del-1",
	})

	view := m.View()
	if !strings.Contains(view, "⚠") {
		t.Errorf("escalation glyph '⚠' not in coordinator view; excerpt:\n%s", excerpt(view, 800))
	}
	if !strings.Contains(view, "escalated") {
		t.Errorf("escalation label not in view; excerpt:\n%s", excerpt(view, 800))
	}
	if !strings.Contains(view, "stuck on review cycle 3") {
		t.Errorf("escalation prompt preview not in view; excerpt:\n%s", excerpt(view, 800))
	}

	// And the buffer must have the event ready for the overlay,
	// summarised with the WORKER's name (from the payload's `source`),
	// not the coordinator's name (the frame envelope).
	if got, want := tui.ModelCoordinatorEventCount(m), 1; got != want {
		t.Errorf("ModelCoordinatorEventCount = %d, want %d", got, want)
	}
	summary := tui.ModelCoordinatorEventSummaryAt(m, 0)
	if !strings.Contains(summary, worker) {
		t.Errorf("buffered summary should name the escalating worker (%q); got %q", worker, summary)
	}
}

// TestCoordinator_EscalationProminent_WorkerWirePath is the regression
// test review-context flagged as missing: it asserts the production
// wire path for escalations works when the frame is tagged with the
// WORKER's SessionName (which is how writeSessionEscalatedEvent's
// primary Publish behaves) and the TUI is focused on that worker.
//
// The coordinator-side fan-out (TestCoordinator_EscalationProminent
// above) handles the more common case; this test ensures the
// payload-sourced worker-name lookup is independent of which stream
// the frame arrived on. Without this coverage a regression in either
// publish call could silently break the feature on the focus the
// test does not exercise.
func TestCoordinator_EscalationProminent_WorkerWirePath(t *testing.T) {
	const worker = "nixos-config@feature-y"
	const coord = "nixos-config@main"

	// Focus the TUI on the WORKER session this time (not a
	// coordinator). The session.escalated event is published by the
	// daemon on the worker's stream (primary publish in
	// writeSessionEscalatedEvent), so a worker-focused TUI also
	// receives the frame — typically when a coordinator-tier human
	// is watching the worker session directly.
	m := newConnectedModel()
	snap := iris.DaemonSessionsSnapshotFrame{
		Type: iris.DaemonFrameSessionsSnapshot,
		Sessions: []iris.SessionSnapshot{
			{Name: worker, InstanceID: "iid", State: "active", Role: "worker", Worktree: "/repo"},
		},
	}
	m2, _ := m.Update(tui.DaemonFrame{RawType: iris.DaemonFrameSessionsSnapshot, Snapshot: &snap})
	m = m2.(tui.Model)

	// Deliver session.escalated tagged with the WORKER's SessionName —
	// matching the daemon's writeSessionEscalatedEvent primary publish.
	m = deliverEvent(t, m, worker, "session.escalated", 1, map[string]any{
		"source": worker,
		"target": coord,
		"prompt": "stuck on something",
	})

	// The row must render (no silent drop).
	view := m.View()
	if !strings.Contains(view, "escalated") {
		t.Errorf("escalation row not rendered on worker-tagged stream; excerpt:\n%s",
			excerpt(view, 800))
	}

	// The buffer must accumulate. The summary must name the worker
	// (from the payload's source field) regardless of which stream
	// carried the frame.
	if got, want := tui.ModelCoordinatorEventCount(m), 1; got != want {
		t.Fatalf("ModelCoordinatorEventCount = %d, want %d", got, want)
	}
	summary := tui.ModelCoordinatorEventSummaryAt(m, 0)
	if !strings.Contains(summary, worker) {
		t.Errorf("summary should name worker %q; got %q", worker, summary)
	}
	if !strings.Contains(summary, coord) {
		t.Errorf("summary should also name target coord %q; got %q", coord, summary)
	}
}

// TestCoordinator_EscalationNotProminentOnWorker asserts that the new
// visual treatments are NOT applied when the focused session is a
// worker. The renderer must produce a row (so we don't drop the event),
// but the styling falls back to the non-coordinator path. Black-box
// indication: the merge-queue glyph styles are coordinator-gated, and
// the buffer accumulation is session-independent, so we assert the
// row renders AND the buffer is populated AND C-o yields a no-op on
// this worker session (covered by TestCoordinator_CtrlO_NoopOnWorker).
//
// This test focuses on the "row still renders" half of the AC: a
// worker viewing their own session.escalated event still sees it,
// just without the coordinator-flavoured prominence.
func TestCoordinator_EscalationNotProminentOnWorker(t *testing.T) {
	const worker = "nixos-config@feature-y"
	m := newConnectedModel()
	snap := iris.DaemonSessionsSnapshotFrame{
		Type: iris.DaemonFrameSessionsSnapshot,
		Sessions: []iris.SessionSnapshot{
			{Name: worker, InstanceID: "iid", State: "active", Role: "worker", Worktree: "/repo"},
		},
	}
	m2, _ := m.Update(tui.DaemonFrame{RawType: iris.DaemonFrameSessionsSnapshot, Snapshot: &snap})
	m = m2.(tui.Model)

	if tui.IsCoordinatorSessionName(worker) {
		t.Fatalf("setup: %q should not be a coordinator", worker)
	}

	m = deliverEvent(t, m, worker, "session.escalated", 1, map[string]any{
		"source": worker, "target": "nixos-config@main", "prompt": "help",
	})

	view := m.View()
	// Row must still render — silent drop would hide the event from a
	// worker debugging their own escalation flow.
	if !strings.Contains(view, "escalated") {
		t.Errorf("escalation row not rendered on worker view; excerpt:\n%s", excerpt(view, 800))
	}
	// Buffer still accumulates — the buffer is cross-session by
	// design, indexed by event type not focus.
	if got, want := tui.ModelCoordinatorEventCount(m), 1; got != want {
		t.Errorf("ModelCoordinatorEventCount = %d, want %d", got, want)
	}
}

// TestCoordinator_MergeQueueProminent asserts that a msg_user event
// whose text matches the merge-queue notification format is detected,
// re-labelled, and accumulated in the coordinator-events buffer. The
// rendered row must include the verbatim notification text.
func TestCoordinator_MergeQueueProminent(t *testing.T) {
	const coord = "nixos-config@main"
	m := modelWithCoordinatorSession(t, coord)

	const notif = "PR #1772 merged. Run `git pull` in @main and `prism cleanup` the worker session."
	m = deliverEvent(t, m, coord, "msg_user", 1, map[string]any{
		"messageId": "mu-mq-1",
		"text":      notif,
	})

	view := m.View()
	if !strings.Contains(view, "PR #1772 merged") {
		t.Errorf("merge-queue notification text not in view; excerpt:\n%s", excerpt(view, 800))
	}
	if got, want := tui.ModelCoordinatorEventCount(m), 1; got != want {
		t.Errorf("ModelCoordinatorEventCount = %d, want %d", got, want)
	}
	summary := tui.ModelCoordinatorEventSummaryAt(m, 0)
	if !strings.Contains(summary, "PR #1772 merged") {
		t.Errorf("buffered summary missing notification text; summary=%q", summary)
	}
}

// TestCoordinator_MergeQueuePlainPromptNotPromoted asserts that a
// msg_user event whose text is a free-form operator prompt that
// happens to mention a PR number does NOT get promoted to a
// merge-queue notification. False-positive promotion would visually
// shout at unrelated operator chat.
func TestCoordinator_MergeQueuePlainPromptNotPromoted(t *testing.T) {
	const coord = "nixos-config@main"
	m := modelWithCoordinatorSession(t, coord)

	m = deliverEvent(t, m, coord, "msg_user", 1, map[string]any{
		"messageId": "mu-plain",
		"text":      "please review PR #1772 when you have time",
	})

	if got, want := tui.ModelCoordinatorEventCount(m), 0; got != want {
		t.Errorf("plain msg_user accumulated as merge-queue event; "+
			"ModelCoordinatorEventCount = %d, want %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// Overlay open/close behaviour
// ---------------------------------------------------------------------------

// TestCoordinator_CtrlO_OpensOverlay asserts that on a coordinator
// session, pressing C-o opens overlayCoordinatorEvents.
func TestCoordinator_CtrlO_OpensOverlay(t *testing.T) {
	m := modelWithCoordinatorSession(t, "nixos-config@main")

	if got, want := tui.ModelOverlay(m), tui.OverlayNone; got != want {
		t.Fatalf("baseline overlay = %d, want OverlayNone (%d)", got, want)
	}

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = m2.(tui.Model)

	if got, want := tui.ModelOverlay(m), tui.OverlayCoordinatorEvents; got != want {
		t.Fatalf("after C-o on coordinator: overlay = %d, want OverlayCoordinatorEvents (%d)", got, want)
	}

	view := m.View()
	if !strings.Contains(view, "coordinator events") {
		t.Errorf("overlay title not in view; excerpt:\n%s", excerpt(view, 600))
	}
}

// TestCoordinator_CtrlO_NoopOnWorker asserts that on a worker session,
// pressing C-o does NOT open the overlay and instead surfaces a
// "not applicable" hint via the prompt's errorMsg row. The TUI must
// continue to function normally afterwards.
func TestCoordinator_CtrlO_NoopOnWorker(t *testing.T) {
	const worker = "nixos-config@feature-z"
	m := newConnectedModel()
	snap := iris.DaemonSessionsSnapshotFrame{
		Type: iris.DaemonFrameSessionsSnapshot,
		Sessions: []iris.SessionSnapshot{
			{Name: worker, InstanceID: "iid", State: "active", Role: "worker", Worktree: "/repo"},
		},
	}
	m2, _ := m.Update(tui.DaemonFrame{RawType: iris.DaemonFrameSessionsSnapshot, Snapshot: &snap})
	m = m2.(tui.Model)

	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = m2.(tui.Model)

	if got, want := tui.ModelOverlay(m), tui.OverlayNone; got != want {
		t.Fatalf("after C-o on worker: overlay = %d, want OverlayNone (%d)", got, want)
	}
	if msg := tui.ModelErrorMsg(m); !strings.Contains(msg, "coordinator") {
		t.Errorf("expected errorMsg to mention coordinators; got %q", msg)
	}
	// The view must reflect the error so the operator sees feedback.
	view := m.View()
	if !strings.Contains(view, "coordinator") {
		t.Errorf("errorMsg about coordinators not surfaced in view; excerpt:\n%s",
			excerpt(view, 600))
	}
}

// TestCoordinator_OverlayCloseOnEsc asserts the overlay closes on Esc,
// matching the existing overlay pattern.
func TestCoordinator_OverlayCloseOnEsc(t *testing.T) {
	m := modelWithCoordinatorSession(t, "nixos-config@main")

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = m2.(tui.Model)
	if got := tui.ModelOverlay(m); got != tui.OverlayCoordinatorEvents {
		t.Fatalf("setup: overlay = %d, want OverlayCoordinatorEvents", got)
	}

	m2, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = m2.(tui.Model)
	if got, want := tui.ModelOverlay(m), tui.OverlayNone; got != want {
		t.Errorf("after Esc: overlay = %d, want OverlayNone", got)
	}
}

// TestCoordinator_OverlayEmptyState asserts the overlay renders an
// empty-state placeholder when the buffer is empty, rather than a
// blank pane.
func TestCoordinator_OverlayEmptyState(t *testing.T) {
	m := modelWithCoordinatorSession(t, "nixos-config@main")

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = m2.(tui.Model)

	view := m.View()
	if !strings.Contains(view, "no coordinator events yet") {
		t.Errorf("empty-state placeholder not in overlay view; excerpt:\n%s",
			excerpt(view, 600))
	}
}

// TestCoordinator_OverlayListsAccumulatedEvents asserts that
// once events have accumulated, the overlay lists them with their
// summary text, newest-first.
func TestCoordinator_OverlayListsAccumulatedEvents(t *testing.T) {
	const coord = "nixos-config@main"
	m := modelWithCoordinatorSession(t, coord)

	// First an escalation, then a merge-queue notification. The
	// overlay should list the merge-queue row above the escalation
	// row (newest first).
	m = deliverEvent(t, m, coord, "session.escalated", 1, map[string]any{
		"source": "nixos-config@feature", "target": coord, "prompt": "stuck",
	})
	m = deliverEvent(t, m, coord, "msg_user", 2, map[string]any{
		"messageId": "mu-1",
		"text":      "PR #1772 merged. Archive: /tmp.",
	})

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	m = m2.(tui.Model)

	view := m.View()
	for _, want := range []string{"PR #1772 merged", "stuck"} {
		if !strings.Contains(view, want) {
			t.Errorf("overlay missing expected content %q; excerpt:\n%s",
				want, excerpt(view, 800))
		}
	}
	// Newest-first ordering: merge-queue row must appear before the
	// escalation row in the rendered text.
	pqIdx := strings.Index(view, "PR #1772 merged")
	escIdx := strings.Index(view, "stuck")
	if pqIdx < 0 || escIdx < 0 {
		t.Fatalf("rows missing; pqIdx=%d escIdx=%d", pqIdx, escIdx)
	}
	if pqIdx > escIdx {
		t.Errorf("merge-queue row should render above escalation row (newest first); "+
			"got merge-queue at %d, escalation at %d", pqIdx, escIdx)
	}
}

// TestCoordinator_AccumulatorBuffered asserts that the coordinator
// event accumulator drops the oldest entries when capacity is
// exceeded, so a long-running TUI session does not grow the buffer
// unboundedly.
func TestCoordinator_AccumulatorBuffered(t *testing.T) {
	const coord = "nixos-config@main"
	m := modelWithCoordinatorSession(t, coord)

	// Deliver 250 escalation events — comfortably over the 200 cap.
	for i := 1; i <= 250; i++ {
		text := fmt.Sprintf("escalation %d", i)
		pb, _ := json.Marshal(map[string]any{
			"source": "nixos-config@feature", "target": coord, "prompt": text,
		})
		m2, _ := m.Update(tui.DaemonFrame{
			RawType: iris.DaemonFrameSessionEvent,
			Event: &iris.DaemonSessionEventFrame{
				Type:        iris.DaemonFrameSessionEvent,
				SessionName: coord,
				RowID:       int64(i),
				EventType:   "session.escalated",
				Payload:     string(pb),
			},
		})
		m = m2.(tui.Model)
	}

	if got := tui.ModelCoordinatorEventCount(m); got > 200 {
		t.Errorf("buffer not bounded; ModelCoordinatorEventCount = %d, want <= 200", got)
	}
	if got := tui.ModelCoordinatorEventCount(m); got != 200 {
		t.Errorf("buffer should be exactly 200 after 250 deliveries; got %d", got)
	}

	// The oldest entry should have been dropped — the front of the
	// buffer should now reference escalation #51 (250-200+1).
	front := tui.ModelCoordinatorEventSummaryAt(m, 0)
	if !strings.Contains(front, "escalation 51") {
		t.Errorf("oldest retained entry should mention 'escalation 51'; got %q", front)
	}
}

// ---------------------------------------------------------------------------
// Help-overlay regression: C-o is documented
// ---------------------------------------------------------------------------

// TestHelpOverlayMentionsCtrlO asserts the help overlay includes a
// row for the new C-o binding so operators can discover it.
func TestHelpOverlayMentionsCtrlO(t *testing.T) {
	m := newConnectedModel()
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = m2.(tui.Model)
	view := m.View()
	if !strings.Contains(view, "C-o") {
		t.Errorf("help overlay does not list C-o; excerpt:\n%s", excerpt(view, 800))
	}
}
