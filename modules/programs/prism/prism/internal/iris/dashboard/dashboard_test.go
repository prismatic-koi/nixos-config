package dashboard_test

// dashboard_test.go — model-level unit tests for the iris dashboard
// (issue #1703). These exercise the bubbletea Update() reducer directly
// without starting a terminal program: the AC asserts that "the dashboard
// updates live as sessions are spawned, transition states, or are cleaned
// up", which translates to three Update transitions:
//
//   1. session added         (sessions_snapshot adds a row)
//   2. session removed       (sessions_snapshot reconciles a removal)
//   3. state changed         (session_state updates the in-memory row)
//
// We also assert that session_spawned drives a row appearance directly
// (the originating-client fast path) and that session_event populates the
// recent-activity column.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/dashboard"
	"github.com/prismatic-koi/prism/internal/iris/tui"
)

// newTestModel returns a Model that has received a WindowSizeMsg and
// ConnectedMsg so subsequent frames can flow.
func newTestModel(t *testing.T) dashboard.Model {
	t.Helper()
	client := tui.NewDaemonClient("/dev/null") // no real socket — frames driven by tests
	m := dashboard.NewModel(client, dashboard.ModePopup, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = m2.(dashboard.Model)
	m2, _ = m.Update(tui.ConnectedMsg{})
	return m2.(dashboard.Model)
}

func snapFrame(snaps ...iris.SessionSnapshot) tui.DaemonFrame {
	return tui.DaemonFrame{
		RawType: iris.DaemonFrameSessionsSnapshot,
		Snapshot: &iris.DaemonSessionsSnapshotFrame{
			Type:     iris.DaemonFrameSessionsSnapshot,
			Sessions: snaps,
		},
	}
}

// TestSessionAddedViaSnapshot verifies that a sessions_snapshot frame populates
// the dashboard row map.
func TestSessionAddedViaSnapshot(t *testing.T) {
	m := newTestModel(t)
	if got := dashboard.SessionCount(m); got != 0 {
		t.Fatalf("initial session count: got %d, want 0", got)
	}

	snap := iris.SessionSnapshot{
		Name:       "nixos-config@feat-x",
		InstanceID: "iid-1",
		State:      "active",
		Role:       "worker",
		Worktree:   "/repo/feat-x",
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	m2, _ := m.Update(snapFrame(snap))
	m = m2.(dashboard.Model)

	if got := dashboard.SessionCount(m); got != 1 {
		t.Fatalf("after snapshot: session count got %d, want 1", got)
	}
	if !dashboard.HasSession(m, "nixos-config@feat-x") {
		t.Fatalf("session not found in dashboard map")
	}
	if got := dashboard.SessionState(m, "nixos-config@feat-x"); got != "active" {
		t.Fatalf("session state got %q, want %q", got, "active")
	}
}

// TestSessionRemovedViaSnapshot verifies that a sessions_snapshot frame which
// no longer contains a previously-known session removes it from the map. This
// covers the "row disappears within ~1 second" cleanup AC.
func TestSessionRemovedViaSnapshot(t *testing.T) {
	m := newTestModel(t)
	a := iris.SessionSnapshot{Name: "a", InstanceID: "iid-a", State: "active", Role: "worker"}
	b := iris.SessionSnapshot{Name: "b", InstanceID: "iid-b", State: "active", Role: "worker"}
	m2, _ := m.Update(snapFrame(a, b))
	m = m2.(dashboard.Model)
	if got := dashboard.SessionCount(m); got != 2 {
		t.Fatalf("after first snapshot: got %d sessions, want 2", got)
	}

	// Second snapshot drops "a" — simulates cleanup.
	m2, _ = m.Update(snapFrame(b))
	m = m2.(dashboard.Model)
	if got := dashboard.SessionCount(m); got != 1 {
		t.Fatalf("after reconcile: got %d sessions, want 1", got)
	}
	if dashboard.HasSession(m, "a") {
		t.Fatalf("session 'a' should have been removed by reconcile")
	}
	if !dashboard.HasSession(m, "b") {
		t.Fatalf("session 'b' should still be present after reconcile")
	}
}

// TestStateChangePropagates verifies session_state frames mutate the
// in-memory row's state field. This is the live-update path used when the
// daemon publishes state transitions to subscribed clients.
func TestStateChangePropagates(t *testing.T) {
	m := newTestModel(t)
	a := iris.SessionSnapshot{Name: "a", InstanceID: "iid-a", State: "spawning", Role: "worker"}
	m2, _ := m.Update(snapFrame(a))
	m = m2.(dashboard.Model)
	if got := dashboard.SessionState(m, "a"); got != "spawning" {
		t.Fatalf("initial state got %q, want %q", got, "spawning")
	}

	state := tui.DaemonFrame{
		RawType: iris.DaemonFrameSessionState,
		State: &iris.DaemonSessionStateFrame{
			Type:        iris.DaemonFrameSessionState,
			SessionName: "a",
			State:       "active",
		},
	}
	m2, _ = m.Update(state)
	m = m2.(dashboard.Model)

	if got := dashboard.SessionState(m, "a"); got != "active" {
		t.Fatalf("after session_state: state got %q, want %q", got, "active")
	}
}

// TestSessionSpawnedAppendsRow verifies the originating-client fast path:
// a session_spawned frame appends a row before the next sessions_snapshot
// reconcile. This is the same eager-row UX the iris TUI provides via
// session_spawned (#1670).
func TestSessionSpawnedAppendsRow(t *testing.T) {
	m := newTestModel(t)
	spawned := tui.DaemonFrame{
		RawType: iris.DaemonFrameSessionSpawned,
		Spawned: &iris.DaemonSessionSpawnedFrame{
			Type:       iris.DaemonFrameSessionSpawned,
			Name:       "fresh@now",
			InstanceID: "iid-fresh",
			Session: &iris.SessionSnapshot{
				Name:       "fresh@now",
				InstanceID: "iid-fresh",
				State:      "spawning",
				Role:       "worker",
				StartedAt:  time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	m2, _ := m.Update(spawned)
	m = m2.(dashboard.Model)
	if !dashboard.HasSession(m, "fresh@now") {
		t.Fatalf("session_spawned did not append row")
	}
	if got := dashboard.SessionState(m, "fresh@now"); got != "spawning" {
		t.Fatalf("spawned row state got %q, want %q", got, "spawning")
	}
}

// TestSessionEventUpdatesActivityColumn verifies that a session_event frame
// records the event type and timestamp on the matching row, populating the
// recent-activity indicator.
func TestSessionEventUpdatesActivityColumn(t *testing.T) {
	m := newTestModel(t)
	a := iris.SessionSnapshot{Name: "a", InstanceID: "iid-a", State: "active", Role: "worker"}
	m2, _ := m.Update(snapFrame(a))
	m = m2.(dashboard.Model)

	before := time.Now()
	event := tui.DaemonFrame{
		RawType: iris.DaemonFrameSessionEvent,
		Event: &iris.DaemonSessionEventFrame{
			Type:        iris.DaemonFrameSessionEvent,
			SessionName: "a",
			RowID:       42,
			EventType:   "tool_call",
			Payload:     "{}",
		},
	}
	m2, _ = m.Update(event)
	m = m2.(dashboard.Model)

	label, at := dashboard.SessionLastEvent(m, "a")
	if label == "" {
		t.Fatalf("expected a non-empty activity label after session_event")
	}
	if !strings.Contains(label, "tool") {
		t.Fatalf("activity label got %q, want it to mention tool", label)
	}
	if at.Before(before) {
		t.Fatalf("activity timestamp not updated: %v before %v", at, before)
	}
}

// TestEmptySessionsView verifies the dashboard renders the empty-state hint
// (and remains responsive to q) when no sessions are present.
func TestEmptySessionsView(t *testing.T) {
	m := newTestModel(t)
	view := m.View()
	if !strings.Contains(view, "no iris sessions yet") {
		t.Fatalf("empty view does not contain empty-state hint; view excerpt:\n%s", excerpt(view, 600))
	}
	// q should still cause a Quit Cmd.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatalf("q produced no Cmd; expected tea.Quit")
	}
	// Cmd is a closure that returns tea.QuitMsg{} when invoked.
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("q Cmd produced %T, want tea.QuitMsg", msg)
	}
}

// TestDisconnectedView verifies the daemon-down overlay renders the canonical
// hint, satisfying the "daemon not running → clear hint" AC.
func TestDisconnectedView(t *testing.T) {
	client := tui.NewDaemonClient("/dev/null")
	m := dashboard.NewModel(client, dashboard.ModePopup, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = m2.(dashboard.Model)
	// Note: we deliberately did not send ConnectedMsg, so the model is in
	// the disconnected state.
	view := m.View()
	if !strings.Contains(view, "iris daemon not connected") {
		t.Fatalf("disconnected view missing not-connected line; view excerpt:\n%s", excerpt(view, 400))
	}
	if !strings.Contains(view, "systemctl --user start iris") {
		t.Fatalf("disconnected view missing start-the-daemon hint; view excerpt:\n%s", excerpt(view, 400))
	}
}

// TestCallerSessionMarker verifies that the --caller-session row is
// decorated with the "you are here" diamond.
func TestCallerSessionMarker(t *testing.T) {
	client := tui.NewDaemonClient("/dev/null")
	m := dashboard.NewModel(client, dashboard.ModePopup, "myself@here")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	m = m2.(dashboard.Model)
	m2, _ = m.Update(tui.ConnectedMsg{})
	m = m2.(dashboard.Model)
	a := iris.SessionSnapshot{Name: "myself@here", InstanceID: "iid-1", State: "active", Role: "coordinator"}
	b := iris.SessionSnapshot{Name: "other@there", InstanceID: "iid-2", State: "active", Role: "worker"}
	m2, _ = m.Update(snapFrame(a, b))
	m = m2.(dashboard.Model)

	view := m.View()
	if !strings.Contains(view, "◆") {
		t.Fatalf("caller-session view missing the ◆ marker; view excerpt:\n%s", excerpt(view, 600))
	}
}

// TestRenderIncludesSessionAndState verifies the rendered view contains the
// session name and a colourised state for both rows, exercising the full
// view path end-to-end.
func TestRenderIncludesSessionAndState(t *testing.T) {
	m := newTestModel(t)
	a := iris.SessionSnapshot{Name: "alpha@one", InstanceID: "iid-a", State: "active", Role: "worker", Worktree: "/p/alpha", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	b := iris.SessionSnapshot{Name: "beta@two", InstanceID: "iid-b", State: "error", Role: "coordinator", Worktree: "/p/beta", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	m2, _ := m.Update(snapFrame(a, b))
	m = m2.(dashboard.Model)

	view := m.View()
	for _, want := range []string{"alpha@one", "beta@two", "active", "error", "alpha", "beta"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q; excerpt:\n%s", want, excerpt(view, 800))
		}
	}
}

// excerpt returns up to n bytes of s with newlines preserved, useful for
// failure messages that need a peek at the rendered View().
func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
