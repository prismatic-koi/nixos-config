package dashboard

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// seedPersistent builds a PersistentModel in its default passive-watch state
// (CursorActive=false) with a single live session row displayed. In this
// state the cursor is inactive, which is where the first Enter must switch
// immediately rather than only re-activating the cursor.
func seedPersistent(t *testing.T, sessionName string) PersistentModel {
	t.Helper()
	m := NewPersistentModel("", "")
	m2, _ := m.Update(SessionsMsg{Sessions: []AgentSession{{Name: sessionName}}})
	pm, ok := m2.(PersistentModel)
	if !ok {
		t.Fatalf("Update returned %T, want PersistentModel", m2)
	}
	if pm.CursorActive {
		t.Fatal("precondition: persistent model must start in passive watch mode (CursorActive=false)")
	}
	if len(pm.Displayed) == 0 {
		t.Fatal("precondition: expected a displayed session row")
	}
	return pm
}

// TestPersistentEnter_SwitchesOnFirstKeypress asserts that a single Enter on a
// highlighted live row switches immediately, with no cursor-activation keypress
// first. If Enter re-activated the cursor when CursorActive is false, the
// first Enter would return CursorTimeoutMsg and record no switch, failing this
// test.
func TestPersistentEnter_SwitchesOnFirstKeypress(t *testing.T) {
	origSwitch := switchSessionFunc
	origResolve := resolveDashClientFunc
	t.Cleanup(func() { switchSessionFunc = origSwitch; resolveDashClientFunc = origResolve })

	var gotSession, gotClient string
	switchSessionFunc = func(sessionName, target string) string {
		gotSession = sessionName
		gotClient = target
		return ""
	}
	resolveDashClientFunc = func() (string, error) { return "dash-client", nil }

	pm := seedPersistent(t, "nixos-config@feature")

	m3, cmd := pm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := m3.(PersistentModel); !ok {
		t.Fatalf("Update returned %T, want PersistentModel", m3)
	}
	if cmd == nil {
		t.Fatal("Enter returned nil cmd; expected a switch command on the first keypress")
	}
	msg := cmd()
	if _, isTimeout := msg.(CursorTimeoutMsg); isTimeout {
		t.Fatal("first Enter only activated the cursor (returned CursorTimeoutMsg); want an immediate switch")
	}
	if gotSession != "nixos-config@feature" {
		t.Errorf("switch target session = %q, want %q", gotSession, "nixos-config@feature")
	}
	if gotClient != "dash-client" {
		t.Errorf("switch target client = %q, want %q", gotClient, "dash-client")
	}
}

// TestPersistentEnter_SwitchesDeterministicDashClient asserts that Enter
// switches the client resolved from the dashboard session (the client that
// pressed Enter), NOT the client that display-message would return. Using
// CurrentClientFunc here would target the leaked other-session client, failing
// this test.
func TestPersistentEnter_SwitchesDeterministicDashClient(t *testing.T) {
	origSwitch := switchSessionFunc
	origResolve := resolveDashClientFunc
	origCurrent := CurrentClientFunc
	t.Cleanup(func() {
		switchSessionFunc = origSwitch
		resolveDashClientFunc = origResolve
		CurrentClientFunc = origCurrent
	})

	var gotClient string
	switchSessionFunc = func(_, target string) string { gotClient = target; return "" }
	// resolveDashClientFunc returns the correct viewing client...
	resolveDashClientFunc = func() (string, error) { return "correct-dash-client", nil }
	// ...while CurrentClientFunc (display-message) leaks a client attached to a
	// different session. The Enter path must NOT use this.
	CurrentClientFunc = func() (string, error) { return "wrong-other-session-client", nil }

	pm := seedPersistent(t, "nixos-config@feature")

	_, cmd := pm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter returned nil cmd")
	}
	_ = cmd()
	if gotClient != "correct-dash-client" {
		t.Errorf("switch used client %q, want %q (must resolve via the dashboard session, not display-message)", gotClient, "correct-dash-client")
	}
}

// TestPersistentEnter_NoClientAttached_ShowsStatus asserts that when no client
// is attached to the dashboard session, Enter reports a visible status message
// and does not silently no-op or attempt a switch with an empty client.
func TestPersistentEnter_NoClientAttached_ShowsStatus(t *testing.T) {
	origSwitch := switchSessionFunc
	origResolve := resolveDashClientFunc
	t.Cleanup(func() { switchSessionFunc = origSwitch; resolveDashClientFunc = origResolve })

	switchCalled := false
	switchSessionFunc = func(_, _ string) string { switchCalled = true; return "" }
	resolveDashClientFunc = func() (string, error) { return "", nil } // no client attached

	pm := seedPersistent(t, "nixos-config@feature")

	_, cmd := pm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter returned nil cmd")
	}
	msg := cmd()
	status, ok := msg.(DashStatusMsg)
	if !ok {
		t.Fatalf("with no client attached, Enter produced %T, want a DashStatusMsg", msg)
	}
	if string(status) == "" {
		t.Error("status message is empty; want a visible message")
	}
	if switchCalled {
		t.Error("switchSessionFunc was called with an empty client; want no switch attempt")
	}
}

// TestPersistentEnter_ReviewGroupToggles asserts that Enter on a review-round
// group row toggles expand/collapse instead of switching sessions, even on the
// first keypress.
func TestPersistentEnter_ReviewGroupToggles(t *testing.T) {
	origSwitch := switchSessionFunc
	t.Cleanup(func() { switchSessionFunc = origSwitch })

	switchCalled := false
	switchSessionFunc = func(_, _ string) string { switchCalled = true; return "" }

	pm := NewPersistentModel("", "")
	pm.Displayed = []AgentSession{{Name: "nixos-config@feature~review-1", IsReviewGroup: true}}
	pm.Cursor = 0

	m2, cmd := pm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm2, ok := m2.(PersistentModel)
	if !ok {
		t.Fatalf("Update returned %T, want PersistentModel", m2)
	}
	if switchCalled {
		t.Error("Enter on a review-group row must not switch sessions")
	}
	if cmd == nil {
		t.Fatal("Enter on a review-group row returned nil cmd; expected a cursor-timeout cmd after toggling")
	}
	if !pm2.CursorActive {
		t.Error("Enter on a review-group row should activate the cursor so the toggle is visible")
	}
}
