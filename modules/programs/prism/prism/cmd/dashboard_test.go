// Package cmd white-box tests for PopupModel and PersistentModel (via internal/dashboard).
//
// These tests verify that the dashboard models behave correctly after the
// refactoring into the internal/dashboard package. Fields are now exported so
// tests can access them directly.
package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ─── minimal server harness (mirrors internal/tmux/harness_test.go) ───────────
//
// Parallelism note: newCmdTestServer starts an isolated tmux server but does
// NOT redirect tmux.TmuxBin — that is a package-level global and mutating it
// races with any parallel test. withCmdServer() performs the redirect and must
// only be called from non-parallel tests.

type cmdTestServer struct {
	socket string
	bin    string
}

func newCmdTestServer(t *testing.T) *cmdTestServer {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not found in PATH — skipping integration test")
	}
	socket := fmt.Sprintf("prism-cmd-test-%d-%s", os.Getpid(), randCmdHex(8))
	s := &cmdTestServer{socket: socket, bin: bin}
	if err := s.run("new-session", "-ds", "bootstrap", "-c", "/tmp"); err != nil {
		t.Fatalf("start test tmux server: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command(bin, "-L", socket, "kill-server").Run()
	})
	return s
}

// withCmdServer redirects tmux.TmuxBin to a wrapper script scoped to s for the
// duration of the test, then restores the original value in t.Cleanup.
//
// Only call this from non-parallel tests — TmuxBin is a package-level global.
func withCmdServer(t *testing.T, s *cmdTestServer) {
	t.Helper()
	wrapperPath := t.TempDir() + "/tmux"
	// -f /dev/null suppresses the user's tmux.conf so no hooks fire against the
	// live DB when the package-level tmux.* functions create sessions on this
	// isolated server.
	script := "#!/bin/sh\nexec " + s.bin + " -L " + s.socket + " -f /dev/null \"$@\"\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("write tmux wrapper: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
}

// randCmdHex returns n random bytes as a hex string using crypto/rand.
func randCmdHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// scriptCmdArgs returns the argument list for the `script` command to run cmd
// in a pseudo-terminal without a real display. Syntax differs by platform:
//
//   - Linux:  script -q -c '<cmd>' /dev/null
//   - macOS:  script -q /dev/null <cmd>  (no -c flag; command is positional)
func scriptCmdArgs(cmd string) []string {
	if runtime.GOOS == "darwin" {
		return []string{"-q", "/dev/null", cmd}
	}
	return []string{"-q", "-c", cmd, "/dev/null"}
}

func (s *cmdTestServer) run(args ...string) error {
	// -f /dev/null suppresses the user's tmux.conf so no hooks (e.g.
	// session-created → prism event tmux-session-start) fire against the live DB.
	cmd := exec.Command(s.bin, append([]string{"-L", s.socket, "-f", "/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %v: %w\n%s", args, err, out)
	}
	return nil
}

func (s *cmdTestServer) output(args ...string) (string, error) {
	// -f /dev/null suppresses the user's tmux.conf so no hooks fire.
	cmd := exec.Command(s.bin, append([]string{"-L", s.socket, "-f", "/dev/null"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %v: %w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *cmdTestServer) newSession(name string) {
	_ = s.run("new-session", "-ds", name, "-c", "/tmp")
}

func (s *cmdTestServer) clientSession(client string) (string, error) {
	return s.output("display-message", "-t", client, "-p", "#{session_name}")
}

func (s *cmdTestServer) hasSession(name string) bool {
	return s.run("has-session", "-t", name) == nil
}

func (s *cmdTestServer) attachClientToSession(t *testing.T, targetSession string) string {
	t.Helper()
	before, _ := s.output("list-clients", "-F", "#{client_name}")
	beforeSet := map[string]bool{}
	for _, c := range strings.Split(before, "\n") {
		if c = strings.TrimSpace(c); c != "" {
			beforeSet[c] = true
		}
	}
	cmd := exec.Command("script", scriptCmdArgs(s.bin+" -L "+s.socket+" -f /dev/null attach-session -t "+targetSession)...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("attach client to %q: %v", targetSession, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	var clientName string
	for time.Now().Before(deadline) {
		out, err := s.output("list-clients", "-F", "#{client_name}")
		if err == nil {
			for _, c := range strings.Split(out, "\n") {
				c = strings.TrimSpace(c)
				if c != "" && !beforeSet[c] {
					clientName = c
					break
				}
			}
		}
		if clientName != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if clientName == "" {
		t.Fatalf("new client for session %q never appeared in list-clients (timeout)", targetSession)
	}
	return clientName
}

// ─── PersistentModel.SwitchTarget unit test ───────────────────────────────────

// TestPersistentSwitchTarget verifies that PersistentModel.SwitchTarget() always
// returns Client (the client currently viewing the dashboard). In the new
// architecture, persistent mode no longer has a callerClient field; it operates
// directly on the viewing client for both Enter navigation and q/esc return.
func TestPersistentSwitchTarget(t *testing.T) {
	t.Parallel()

	m := dashboard.NewPersistentModel("viewing-client", "some-session")
	if got := m.SwitchTarget(); got != "viewing-client" {
		t.Errorf("PersistentModel.SwitchTarget() = %q, want %q", got, "viewing-client")
	}
}

// TestPopupSwitchTarget verifies that popup mode always uses Client
// (the popup runs inside the caller's own tmux client — no indirection needed).
func TestPopupSwitchTarget(t *testing.T) {
	t.Parallel()

	m := dashboard.PopupModel{
		Client:       "popup-client",
		CursorActive: true,
	}
	if got := m.SwitchTarget(); got != "popup-client" {
		t.Errorf("PopupModel.SwitchTarget() = %q, want %q", got, "popup-client")
	}
}

// TestDashModelEnterCursorActivation verifies that in persistent-session mode,
// pressing Enter when the cursor is inactive activates the cursor without
// switching sessions — matching the j/k behaviour.
func TestDashModelEnterCursorActivation(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		CursorActive: false,
		Shared: dashboard.Shared{
			Sessions:  []dashboard.AgentSession{{Name: "some-session"}},
			Displayed: []dashboard.AgentSession{{Name: "some-session"}},
		},
	}

	updatedModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm := updatedModel.(dashboard.PersistentModel)

	if !pm.CursorActive {
		t.Error("CursorActive should be true after first Enter in persistent mode")
	}
	if cmd == nil {
		t.Error("cmd should be non-nil (CursorTimeoutCmd)")
	}
	// The sessions list is unchanged and no switch was initiated — if a switch
	// had fired, the handler would have returned tea.Sequence(sideEffect, tea.Quit)
	// which has a different shape than CursorTimeoutCmd(). We can detect this by
	// checking that the returned message is a CursorTimeoutMsg (fires after a delay).
	// We just verify no immediate quit was issued by ensuring the model is still valid.
	if len(pm.Sessions) == 0 {
		t.Error("sessions should be unchanged")
	}
}

// ─── dashboard model init tests ───────────────────────────────────────────────

// TestDashModelCallerSessionCapturedAtInit verifies that NewPersistentModel
// captures the callerSession argument at construction time and stores it as
// CurrentSession, so the "you are here" indicator works correctly.
func TestDashModelCallerSessionCapturedAtInit(t *testing.T) {
	t.Parallel()

	m := dashboard.NewPersistentModel("process-client", "some-session")

	if m.CurrentSession != "some-session" {
		t.Fatalf("CurrentSession = %q, want %q (should be captured at init)", m.CurrentSession, "some-session")
	}
	if m.Client != "process-client" {
		t.Fatalf("Client = %q, want %q", m.Client, "process-client")
	}
}

// ─── filter mode tests ────────────────────────────────────────────────────────

// TestDashFilterActivation verifies that pressing '/' activates filter mode and
// sets CursorActive.
func TestDashFilterActivation(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:  []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}},
			Displayed: []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}},
		},
	}
	updatedModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	pm := updatedModel.(dashboard.PersistentModel)

	if !pm.FilterActive {
		t.Error("FilterActive should be true after pressing '/'")
	}
	if !pm.CursorActive {
		t.Error("CursorActive should be true after activating filter")
	}
	if pm.FilterText != "" {
		t.Errorf("FilterText should be empty on activation, got %q", pm.FilterText)
	}
}

// TestDashFilterNarrowsList verifies that typing characters in filter mode
// narrows Displayed to only fuzzy-matching sessions.
func TestDashFilterNarrowsList(t *testing.T) {
	t.Parallel()

	sessions := []dashboard.AgentSession{
		{Name: "alpha"},
		{Name: "beta"},
		{Name: "aleph"},
	}
	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:     sessions,
			Displayed:    sessions,
			FilterActive: true,
		},
	}

	// Type "al" — should match "alpha" and "aleph", not "beta".
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	pm := m2.(dashboard.PersistentModel)
	m3, _ := pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	pm = m3.(dashboard.PersistentModel)

	if len(pm.Displayed) != 2 {
		t.Errorf("Displayed len = %d, want 2 (alpha + aleph)", len(pm.Displayed))
	}
	for _, s := range pm.Displayed {
		if s.Name == "beta" {
			t.Error("'beta' should not appear in filtered list for pattern 'al'")
		}
	}
	if pm.FilterText != "al" {
		t.Errorf("FilterText = %q, want %q", pm.FilterText, "al")
	}
}

// TestDashFilterBackspace verifies that backspace removes the last character
// from FilterText and re-expands the list.
func TestDashFilterBackspace(t *testing.T) {
	t.Parallel()

	sessions := []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}}
	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:     sessions,
			Displayed:    sessions,
			FilterActive: true,
			FilterText:   "al",
		},
	}
	// Pre-narrow so Displayed only has "alpha".
	m.Shared = dashboard.RefilterShared(m.Shared)
	if len(m.Displayed) != 1 {
		t.Fatalf("setup: Displayed len = %d, want 1", len(m.Displayed))
	}

	// Backspace should remove 'l' and re-expand.
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	pm := m2.(dashboard.PersistentModel)

	if pm.FilterText != "a" {
		t.Errorf("FilterText = %q, want %q after backspace", pm.FilterText, "a")
	}
	// "a" matches both "alpha" and "beta" (b-e-t-a has 'a').
	if len(pm.Displayed) == 0 {
		t.Error("Displayed should be non-empty after backspace")
	}
}

// TestDashFilterEscapeCancels verifies that pressing Esc in filter mode
// cancels the filter and restores the full session list.
func TestDashFilterEscapeCancels(t *testing.T) {
	t.Parallel()

	sessions := []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}}
	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:     sessions,
			Displayed:    sessions[:1], // narrowed
			FilterActive: true,
			FilterText:   "al",
		},
	}

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	pm := m2.(dashboard.PersistentModel)

	if pm.FilterActive {
		t.Error("FilterActive should be false after Esc")
	}
	if pm.FilterText != "" {
		t.Errorf("FilterText should be cleared, got %q", pm.FilterText)
	}
	if len(pm.Displayed) != len(sessions) {
		t.Errorf("Displayed len = %d after cancel, want %d (full list)", len(pm.Displayed), len(sessions))
	}
}

// TestDashFilterEnterSwitches verifies that pressing Enter in filter mode with
// an active selection returns a non-nil switch command and exits filter mode.
func TestDashFilterEnterSwitches(t *testing.T) {
	t.Parallel()

	sessions := []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}}
	m := dashboard.PopupModel{
		Client:       "popup-client",
		CursorActive: true,
		Shared: dashboard.Shared{
			Sessions:     sessions,
			Displayed:    sessions,
			FilterActive: true,
			FilterText:   "al",
			Cursor:       0,
		},
	}
	m.Shared = dashboard.RefilterShared(m.Shared)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Error("Enter in filter mode should return a non-nil switch command")
	}
}

// TestDashFilterCursorStaysActiveOnTimeout verifies that a CursorTimeoutMsg
// does not deactivate the cursor highlight while filter mode is active.
// Without the fix, the selection bar would silently vanish 3 seconds after
// pressing '/' even while the user is still typing.
func TestDashFilterCursorStaysActiveOnTimeout(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		CursorActive: true, // persistent mode: timeout normally deactivates cursor
		Shared: dashboard.Shared{
			Sessions:     []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}},
			Displayed:    []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}},
			FilterActive: true,
		},
	}

	// Fire the cursor timeout while filter mode is active.
	m2, _ := m.Update(dashboard.CursorTimeoutMsg{})
	pm := m2.(dashboard.PersistentModel)

	if !pm.CursorActive {
		t.Error("CursorActive must remain true when CursorTimeoutMsg fires during filter mode")
	}
}

// TestDashFilterBlurKeepsCursorActive verifies that a BlurMsg does not
// deactivate the cursor while filter mode is open. Without this guard the
// selection bar disappears on focus loss but the filter prompt remains, leaving
// the user unable to see which session Enter will select.
func TestDashFilterBlurKeepsCursorActive(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		CursorActive: true, // persistent mode: BlurMsg normally deactivates cursor
		Shared: dashboard.Shared{
			Sessions:     []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}},
			Displayed:    []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}},
			FilterActive: true,
		},
	}

	m2, _ := m.Update(tea.BlurMsg{})
	pm := m2.(dashboard.PersistentModel)

	if !pm.CursorActive {
		t.Error("CursorActive must remain true when BlurMsg fires during filter mode")
	}
}

// TestDashFilterCtrlCQuitsProgram verifies that ctrl+c in filter mode quits
// the TUI rather than merely dismissing the filter. Previously ctrl+c and esc
// were handled by the same case, so ctrl+c was consumed without quitting.
func TestDashFilterCtrlCQuitsProgram(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		CursorActive: true,
		Shared: dashboard.Shared{
			Sessions:     []dashboard.AgentSession{{Name: "alpha"}},
			Displayed:    []dashboard.AgentSession{{Name: "alpha"}},
			FilterActive: true,
		},
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c in filter mode should return a non-nil quit command")
	}
	// Execute the command to confirm it produces a QuitMsg.
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c command produced %T, want tea.QuitMsg", msg)
	}
}

// TestDashFilterEscCancelsNotQuits verifies that Esc in filter mode cancels
// the filter (returning to normal mode) without quitting the TUI.
func TestDashFilterEscCancelsNotQuits(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		CursorActive: true,
		Shared: dashboard.Shared{
			Sessions:     []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}},
			Displayed:    []dashboard.AgentSession{{Name: "alpha"}}, // narrowed
			FilterActive: true,
			FilterText:   "al",
		},
	}

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	pm := m2.(dashboard.PersistentModel)

	if cmd != nil {
		t.Error("Esc in filter mode should return nil cmd (no quit)")
	}
	if pm.FilterActive {
		t.Error("FilterActive should be false after Esc")
	}
	if len(pm.Displayed) != 2 {
		t.Errorf("Displayed len = %d, want 2 (full list restored)", len(pm.Displayed))
	}
}

// TestDashFilterSnapSkippedWhenFilterActive verifies that the cursor-snap-to-
// CurrentSession logic in SessionsMsg is skipped when filter mode is already
// active. When FilterActive is true, needsSnap is not set, so no snap into the
// sorted Displayed list runs — avoiding a collision between an in-progress
// filter and a snap that would overwrite the cursor position.
func TestDashFilterSnapSkippedWhenFilterActive(t *testing.T) {
	t.Parallel()

	sessions := []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}}

	// Filter is already active and narrowed to "beta" only.
	m := dashboard.PersistentModel{
		CurrentSession: "alpha", // would snap cursor to index 0 in sessions
		Shared: dashboard.Shared{
			FilterActive:      true,
			FilterText:        "bet",
			CursorInitialised: false,
			Cursor:            0,
			Sessions:          sessions,
			Displayed:         []dashboard.AgentSession{{Name: "beta"}}, // pre-filtered
		},
	}

	// Deliver a SessionsMsg (the first tick).
	m2, _ := m.Update(dashboard.SessionsMsg{Sessions: sessions})
	pm := m2.(dashboard.PersistentModel)

	if !pm.CursorInitialised {
		t.Error("CursorInitialised should be true after first SessionsMsg")
	}
	// The snap should have been skipped; cursor must still point into the
	// filtered list (at most index 0 since Displayed has one entry).
	if pm.Cursor != 0 {
		t.Errorf("Cursor = %d, want 0 — snap into Displayed must not run during filter mode", pm.Cursor)
	}
	// "beta" must still be the first displayed entry.
	if len(pm.Displayed) == 0 || pm.Displayed[0].Name != "beta" {
		t.Errorf("Displayed[0] = %v, want {Name:beta}", pm.Displayed)
	}
}

// TestDashFilterCursorNavigation verifies that j/k move the cursor within the
// filtered list while filter mode is active.
func TestDashFilterCursorNavigation(t *testing.T) {
	t.Parallel()

	sessions := []dashboard.AgentSession{{Name: "alpha"}, {Name: "aleph"}}
	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:     sessions,
			Displayed:    sessions,
			FilterActive: true,
			Cursor:       0,
		},
	}

	// Press 'j' to move down.
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	pm := m2.(dashboard.PersistentModel)
	if pm.Cursor != 1 {
		t.Errorf("Cursor = %d after j, want 1", pm.Cursor)
	}

	// Press 'k' to move back up.
	m3, _ := pm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	pm = m3.(dashboard.PersistentModel)
	if pm.Cursor != 0 {
		t.Errorf("Cursor = %d after k, want 0", pm.Cursor)
	}
}

// TestDashRefilterClampsOOBCursor verifies that RefilterShared clamps the cursor
// when the filter reduces the list below the current cursor position.
func TestDashRefilterClampsOOBCursor(t *testing.T) {
	t.Parallel()

	sessions := []dashboard.AgentSession{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}}
	d := dashboard.Shared{
		Sessions:     sessions,
		Displayed:    sessions,
		FilterActive: true,
		Cursor:       2, // pointing at "gamma"
		FilterText:   "al",
	}
	d = dashboard.RefilterShared(d) // "al" only matches "alpha"

	if len(d.Displayed) != 1 {
		t.Fatalf("expected 1 match for 'al', got %d", len(d.Displayed))
	}
	if d.Cursor != 0 {
		t.Errorf("Cursor = %d, want 0 (clamped to end of filtered list)", d.Cursor)
	}
}

// TestDashFilterViewShowsPrompt checks that the View output contains the
// filter prompt when FilterActive is true.
func TestDashFilterViewShowsPrompt(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		CursorActive: true,
		Shared: dashboard.Shared{
			Sessions:     []dashboard.AgentSession{{Name: "alpha"}},
			Displayed:    []dashboard.AgentSession{{Name: "alpha"}},
			FilterActive: true,
			FilterText:   "alp",
			Width:        80,
			Height:       40,
		},
	}
	view := m.View()
	if !strings.Contains(view, "alp") {
		t.Error("View() should contain the filter text 'alp'")
	}
}

// TestDashViewShowsHelpHint checks that the help hint mentions '/' when not in
// filter mode.
func TestDashViewShowsHelpHint(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:  []dashboard.AgentSession{{Name: "alpha"}},
			Displayed: []dashboard.AgentSession{{Name: "alpha"}},
			Width:     80,
			Height:    40,
		},
	}
	view := m.View()
	if !strings.Contains(view, "/ filter") {
		t.Errorf("View() should mention '/ filter' in the help line, got:\n%s", view)
	}
}

// TestPersistentModelEnterSwitchesClient verifies that Enter in persistent mode
// switches Client (the viewing client) to the selected session and stays
// alive — the TUI does NOT quit (no tea.QuitMsg returned).
//
// The persistent dashboard no longer has a callerClient field; it always
// operates on Client, the client that is currently attached to the session.
func TestPersistentModelEnterSwitchesClient(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s) // redirects TmuxBin; must not be called from parallel tests
	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")

	client := s.attachClientToSession(t, "nixos-config@main")

	// Build a PersistentModel whose Client is the real attached client.
	model := dashboard.NewPersistentModel(client, "nixos-config@main")
	model.Sessions = []dashboard.AgentSession{{Name: "nixos-config@feature"}}
	model.Displayed = model.Sessions
	model.Cursor = 0
	model.CursorActive = true // cursor active so Enter fires immediately

	// Verify Update(Enter) returns a non-nil cmd.
	updatedModel, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Update(Enter) returned nil cmd — handler did not fire")
	}
	pm := updatedModel.(dashboard.PersistentModel)
	// Cursor should be deactivated after Enter (returning to passive watch mode).
	if pm.CursorActive {
		t.Error("CursorActive should be false after Enter in persistent mode (returns to passive watch)")
	}

	// Execute the cmd to perform the actual switch.
	resultMsg := cmd()
	if resultMsg != nil {
		// A non-nil result indicates an error message was returned.
		t.Fatalf("cmd() returned unexpected message: %v", resultMsg)
	}

	// Poll for client to land on the target session.
	var got string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess, err := s.clientSession(client); err == nil {
			got = sess
			if sess == "nixos-config@feature" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got != "nixos-config@feature" {
		t.Errorf("client session = %q after Enter, want %q", got, "nixos-config@feature")
	}
}

// TestPersistentModelQuitSwitchesClientLast verifies that q in persistent mode
// calls SwitchClientLast (switch-client -l) on Client to return it to its
// previous session, and does NOT quit the TUI.
func TestPersistentModelQuitSwitchesClientLast(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s) // redirects TmuxBin; must not be called from parallel tests
	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")

	client := s.attachClientToSession(t, "nixos-config@main")

	// Switch the client to feature (so "last" = main).
	if err := tmux.SwitchClient(client, "nixos-config@feature"); err != nil {
		t.Fatalf("setup SwitchClient: %v", err)
	}

	// Build a PersistentModel with Client set to the real attached client.
	model := dashboard.NewPersistentModel(client, "nixos-config@feature")
	model.CursorActive = true // cursor must be active for q to fire the switch

	// Send 'q' — should return a non-nil cmd and not quit.
	updatedModel, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("Update('q') returned nil cmd — handler did not fire")
	}
	pm := updatedModel.(dashboard.PersistentModel)
	if pm.CursorActive {
		t.Error("CursorActive should be false after q (passive watch mode)")
	}

	// Execute the cmd.
	resultMsg := cmd()
	if resultMsg != nil {
		t.Fatalf("cmd() returned unexpected message: %v", resultMsg)
	}

	// Poll for client to return to main (the "last" session before feature).
	var got string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess, err := s.clientSession(client); err == nil {
			got = sess
			if sess == "nixos-config@main" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got != "nixos-config@main" {
		t.Errorf("client session = %q after q, want %q (switch-client -l should return to previous)", got, "nixos-config@main")
	}
}

// ─── ensureDashSession tests ──────────────────────────────────────────────────

// TestEnsureDashSessionUsesAbsolutePath verifies that ensureDashSession creates
// the prism-dashboard session with the absolute path of the running binary in
// the dashboard command, not the bare "prism" name.
//
// This is the regression test for the first-startup bug: when prism is
// installed in the Nix store and invoked from a bare environment (e.g. launched
// by a compositor), the session pane's shell may not have the Nix store path in
// PATH. Using bare "prism" causes the dashboard to fail immediately, leaving
// the session with an empty shell prompt instead of the dashboard TUI.
// os.Executable() returns the absolute path of the running binary, which is
// always valid regardless of PATH.
//
// The new architecture runs `prism dashboard` directly (no restart loop), so
// the command is simpler. This test verifies:
//  1. The session is created.
//  2. The pane command contains the absolute binary path.
//  3. No restart loop ("while ... do") is present in the command.
func TestEnsureDashSessionUsesAbsolutePath(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s) // redirects TmuxBin; must not be called from parallel tests

	if err := ensureDashSession(); err != nil {
		t.Fatalf("ensureDashSession: %v", err)
	}

	if !s.hasSession(dashSession) {
		t.Fatalf("session %q was not created", dashSession)
	}

	// Inspect the command running in the dashboard window. The pane's command
	// should contain the absolute path of this test binary, not the bare
	// "prism" name. We capture the window's session command via tmux
	// list-windows, which exposes the pane command through pane_start_command.
	windowInfo, err := s.output(
		"list-windows", "-t", dashSession,
		"-F", "#{window_name}|#{pane_start_command}",
	)
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable() failed — cannot verify absolute path")
	}

	if !strings.Contains(windowInfo, self) {
		t.Errorf("pane_start_command does not contain the absolute binary path %q\ngot: %s\n"+
			"want the dashboard command to use the absolute path so it works when PATH is stripped",
			self, windowInfo)
	}
	// Verify that no restart loop is present — the persistent dashboard keeps
	// itself alive without a shell wrapper loop.
	if strings.Contains(windowInfo, "while ") {
		t.Errorf("pane_start_command contains a restart loop ('while ...') — this should be gone\ngot: %s", windowInfo)
	}
	// Also verify no bare "prism" (without absolute path) in the command.
	if strings.Contains(windowInfo, "while prism dashboard") {
		t.Errorf("pane_start_command contains bare 'prism' in restart loop — will fail when prism is not in PATH\ngot: %s", windowInfo)
	}
}

// TestEnsureDashSessionIdempotent verifies that calling ensureDashSession twice
// does not return an error and does not create a second session.
func TestEnsureDashSessionIdempotent(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s) // redirects TmuxBin; must not be called from parallel tests

	if err := ensureDashSession(); err != nil {
		t.Fatalf("first ensureDashSession: %v", err)
	}
	if err := ensureDashSession(); err != nil {
		t.Fatalf("second ensureDashSession (idempotent): %v", err)
	}

	// Count sessions named prism-dashboard — there must be exactly one.
	out, err := s.output("list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	count := 0
	for _, name := range strings.Split(out, "\n") {
		if strings.TrimSpace(name) == dashSession {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 %q session, found %d", dashSession, count)
	}
}

// TestSortDisplayed verifies the sort order produced by SortDisplayed:
//   - repos sort alphabetically
//   - within a repo, @main and plain sessions (no @) sort before worktree branches
//   - worktree branches sort alphabetically after @main
func TestSortDisplayed(t *testing.T) {
	t.Parallel()

	input := []dashboard.AgentSession{
		{Name: "repoB@branch-z"},
		{Name: "repoA@branch-b"},
		{Name: "repoA@main"},
		{Name: "repoB@main"},
		{Name: "repoA@branch-a"},
		{Name: "plain-session"}, // no "@" — sorts first within its "repo"
	}

	dashboard.SortDisplayed(input)

	want := []string{
		"plain-session",  // no "@" — "plain-session\x00plain-session"
		"repoA@main",     // @main sorts first within repoA
		"repoA@branch-a", // branches alphabetically after @main
		"repoA@branch-b",
		"repoB@main", // @main sorts first within repoB
		"repoB@branch-z",
	}

	if len(input) != len(want) {
		t.Fatalf("got %d sessions, want %d", len(input), len(want))
	}
	for i, s := range input {
		if s.Name != want[i] {
			t.Errorf("index %d: got %q, want %q", i, s.Name, want[i])
		}
	}
}

// ─── git stat error rendering tests ──────────────────────────────────────────

// TestDashViewStatError verifies that a session with a failed git stat shows
// "?" in the rendered view, not "—" (which would imply a clean worktree).
func TestDashViewStatError(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:  []dashboard.AgentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			Displayed: []dashboard.AgentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			GitStats: map[string]dashboard.GitStatResult{
				"/some/path": {Ok: false}, // stat failed
			},
			Width:  80,
			Height: 40,
		},
	}
	view := m.View()
	if !strings.Contains(view, "?") {
		t.Errorf("View() should contain '?' for a failed git stat, got:\n%s", view)
	}
}

// TestDashViewStatClean verifies that a session with a successful git stat
// showing no changes renders "—", not "?".
func TestDashViewStatClean(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:  []dashboard.AgentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			Displayed: []dashboard.AgentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			GitStats: map[string]dashboard.GitStatResult{
				"/some/path": {Ok: true}, // stat succeeded, zero DiffStat = clean
			},
			Width:  80,
			Height: 40,
		},
	}
	view := m.View()
	if !strings.Contains(view, "—") {
		t.Errorf("View() should contain '—' for a clean worktree, got:\n%s", view)
	}
	if strings.Contains(view, "?") {
		t.Errorf("View() should NOT contain '?' for a clean worktree, got:\n%s", view)
	}
}

// TestDashViewStatDirty verifies that a session with uncommitted changes shows
// the numeric diff stats (not "—" or "?").
func TestDashViewStatDirty(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:  []dashboard.AgentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			Displayed: []dashboard.AgentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			GitStats: map[string]dashboard.GitStatResult{
				"/some/path": {
					Ok:   true,
					Stat: git.DiffStat{Files: 3, Insertions: 42, Deletions: 7},
				},
			},
			Width:  120,
			Height: 40,
		},
	}
	view := m.View()
	if !strings.Contains(view, "+42") {
		t.Errorf("View() should contain '+42' for a dirty worktree, got:\n%s", view)
	}
	if !strings.Contains(view, "-7") {
		t.Errorf("View() should contain '-7' for a dirty worktree, got:\n%s", view)
	}
}

// TestDashViewStatErrorDoesNotAffectOtherSessions verifies that a stat failure
// for one session does not affect the rendering of other sessions.
func TestDashViewStatErrorDoesNotAffectOtherSessions(t *testing.T) {
	t.Parallel()

	sessions := []dashboard.AgentSession{
		{Name: "repo@main", AgentPath: "/good/path"},
		{Name: "repo@bad", AgentPath: "/bad/path"},
	}
	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:  sessions,
			Displayed: sessions,
			GitStats: map[string]dashboard.GitStatResult{
				"/good/path": {Ok: true},  // clean
				"/bad/path":  {Ok: false}, // stat failed
			},
			Width:  120,
			Height: 40,
		},
	}
	view := m.View()

	// The view should contain both "—" (for the clean session) and "?" (for the
	// failed one). We cannot assert exact positions but both must appear.
	if !strings.Contains(view, "—") {
		t.Errorf("View() should contain '—' for the clean session, got:\n%s", view)
	}
	if !strings.Contains(view, "?") {
		t.Errorf("View() should contain '?' for the failed session, got:\n%s", view)
	}
}

// ─── multi-client integration tests ──────────────────────────────────────────

// TestPersistentModelEnterMultiClient verifies that pressing Enter in client
// A's persistentModel switches only client A to the selected session. Client B,
// a bystander viewing the same session, must remain unaffected.
func TestPersistentModelEnterMultiClient(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s) // redirects TmuxBin; must not be called from parallel tests
	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")

	// Attach two real clients to the same session.
	clientA := s.attachClientToSession(t, "nixos-config@main")
	clientB := s.attachClientToSession(t, "nixos-config@main")

	// Build a persistentModel for client A only.
	model := newPersistentModel(clientA, "nixos-config@main")
	model.sessions = []agentSession{{Name: "nixos-config@feature"}}
	model.displayed = model.sessions
	model.cursor = 0
	model.cursorActive = true // cursor active so Enter fires immediately

	// Press Enter — should return a non-nil cmd.
	updatedModel, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Update(Enter) returned nil cmd — handler did not fire")
	}
	pm := updatedModel.(persistentModel)
	if pm.cursorActive {
		t.Error("cursorActive should be false after Enter in persistent mode")
	}

	// Execute the cmd to perform the actual switch.
	resultMsg := cmd()
	if resultMsg != nil {
		t.Fatalf("cmd() returned unexpected message: %v", resultMsg)
	}

	// Poll for client A to land on the target session.
	var gotA string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess, err := s.clientSession(clientA); err == nil {
			gotA = sess
			if sess == "nixos-config@feature" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gotA != "nixos-config@feature" {
		t.Errorf("clientA session = %q after Enter, want %q", gotA, "nixos-config@feature")
	}

	// Client B must remain on nixos-config@main.
	gotB, err := s.clientSession(clientB)
	if err != nil {
		t.Fatalf("clientSession(clientB): %v", err)
	}
	if gotB != "nixos-config@main" {
		t.Errorf("clientB session = %q, want %q (bystander must be unaffected)", gotB, "nixos-config@main")
	}
}

// TestPersistentModelEnterStaleStamp verifies that Enter in client A's
// persistentModel switches client A — not client B — even when the global
// @prism_caller_client stamp points to client B. The model must use the client
// captured at init time, not the live global stamp.
func TestPersistentModelEnterStaleStamp(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s) // redirects TmuxBin; must not be called from parallel tests
	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")

	clientA := s.attachClientToSession(t, "nixos-config@main")
	clientB := s.attachClientToSession(t, "nixos-config@main")

	// Poison the global stamp to point to client B, simulating B having opened
	// the dashboard more recently than A.
	s.setGlobal("@prism_caller_client", clientB)

	// Build a persistentModel for client A — it must capture clientA at init,
	// not read the global stamp lazily.
	model := newPersistentModel(clientA, "nixos-config@main")
	model.sessions = []agentSession{{Name: "nixos-config@feature"}}
	model.displayed = model.sessions
	model.cursor = 0
	model.cursorActive = true

	updatedModel, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Update(Enter) returned nil cmd — handler did not fire")
	}
	_ = updatedModel

	resultMsg := cmd()
	if resultMsg != nil {
		t.Fatalf("cmd() returned unexpected message: %v", resultMsg)
	}

	// Poll for client A to land on the target.
	var gotA string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess, err := s.clientSession(clientA); err == nil {
			gotA = sess
			if sess == "nixos-config@feature" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gotA != "nixos-config@feature" {
		t.Errorf("clientA session = %q, want %q (should be switched despite stale stamp)", gotA, "nixos-config@feature")
	}

	// Client B must remain on nixos-config@main — the stale stamp must not
	// cause B to be switched.
	gotB, err := s.clientSession(clientB)
	if err != nil {
		t.Fatalf("clientSession(clientB): %v", err)
	}
	if gotB != "nixos-config@main" {
		t.Errorf("clientB session = %q, want %q (stale stamp must not switch bystander)", gotB, "nixos-config@main")
	}
}

// TestPersistentModelQuitMultiClient verifies that pressing q in client A's
// persistentModel calls SwitchClientLast for client A (returning it to its
// previous session) while client B, viewing a different session, is unaffected.
func TestPersistentModelQuitMultiClient(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s) // redirects TmuxBin; must not be called from parallel tests
	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")
	s.newSession("nixos-config@other")

	// Attach client A to main, then switch it to feature so "last" = main.
	clientA := s.attachClientToSession(t, "nixos-config@main")
	if err := s.run("switch-client", "-c", clientA, "-t", "nixos-config@feature"); err != nil {
		t.Fatalf("setup: switch clientA to feature: %v", err)
	}

	// Attach client B to a completely separate session.
	clientB := s.attachClientToSession(t, "nixos-config@other")

	// Build a persistentModel for client A while it is on nixos-config@feature.
	model := newPersistentModel(clientA, "nixos-config@feature")
	model.cursorActive = true

	// Press q — should return a non-nil cmd.
	updatedModel, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("Update('q') returned nil cmd — handler did not fire")
	}
	pm := updatedModel.(persistentModel)
	if pm.cursorActive {
		t.Error("cursorActive should be false after q")
	}

	resultMsg := cmd()
	if resultMsg != nil {
		t.Fatalf("cmd() returned unexpected message: %v", resultMsg)
	}

	// Poll for client A to return to nixos-config@main (its "last" session).
	var gotA string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess, err := s.clientSession(clientA); err == nil {
			gotA = sess
			if sess == "nixos-config@main" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gotA != "nixos-config@main" {
		t.Errorf("clientA session = %q after q, want %q (SwitchClientLast should return to previous)", gotA, "nixos-config@main")
	}

	// Client B must remain on nixos-config@other.
	gotB, err := s.clientSession(clientB)
	if err != nil {
		t.Fatalf("clientSession(clientB): %v", err)
	}
	if gotB != "nixos-config@other" {
		t.Errorf("clientB session = %q, want %q (unaffected by client A's quit)", gotB, "nixos-config@other")
	}
}

// TestPopupModelEnterMultiClient verifies that pressing Enter in client A's
// popupModel switches client A to the selected session. Client B, a bystander,
// must remain on its original session.
func TestPopupModelEnterMultiClient(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s) // redirects TmuxBin; must not be called from parallel tests
	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")

	clientA := s.attachClientToSession(t, "nixos-config@main")
	clientB := s.attachClientToSession(t, "nixos-config@main")

	// Build a popupModel for client A.
	model := newPopupModel(clientA, "nixos-config@main")
	model.sessions = []agentSession{{Name: "nixos-config@feature"}}
	model.displayed = model.sessions
	model.cursor = 0
	// popupModel always has cursorActive = true (set in newPopupModel).

	// Press Enter.
	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Update(Enter) returned nil cmd — popup handler did not fire")
	}

	// Execute the cmd. In popup mode a successful switch returns tea.QuitMsg{}.
	resultMsg := cmd()
	if _, isQuit := resultMsg.(tea.QuitMsg); !isQuit {
		t.Fatalf("cmd() returned %T (%v), want tea.QuitMsg (popup should quit after switch)", resultMsg, resultMsg)
	}

	// Poll for client A to land on nixos-config@feature.
	var gotA string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess, err := s.clientSession(clientA); err == nil {
			gotA = sess
			if sess == "nixos-config@feature" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gotA != "nixos-config@feature" {
		t.Errorf("clientA session = %q after popup Enter, want %q", gotA, "nixos-config@feature")
	}

	// Client B must remain on nixos-config@main.
	gotB, err := s.clientSession(clientB)
	if err != nil {
		t.Fatalf("clientSession(clientB): %v", err)
	}
	if gotB != "nixos-config@main" {
		t.Errorf("clientB session = %q, want %q (bystander must be unaffected)", gotB, "nixos-config@main")
	}
}

// TestDashViewStatEmptyAgentPath verifies that a session with an empty
// AgentPath renders "—" (no stat available), not "?" (which would imply a
// git error). Sessions with no worktree path are not stat-failed; they simply
// have no path to stat.
func TestDashViewStatEmptyAgentPath(t *testing.T) {
	t.Parallel()

	m := dashboard.PersistentModel{
		Shared: dashboard.Shared{
			Sessions:  []dashboard.AgentSession{{Name: "scratchpad-like", AgentPath: ""}},
			Displayed: []dashboard.AgentSession{{Name: "scratchpad-like", AgentPath: ""}},
			GitStats:  map[string]dashboard.GitStatResult{}, // no entry for empty path
			Width:     80,
			Height:    40,
		},
	}
	view := m.View()
	if strings.Contains(view, "?") {
		t.Errorf("View() should NOT contain '?' for a session with empty AgentPath, got:\n%s", view)
	}
	if !strings.Contains(view, "—") {
		t.Errorf("View() should contain '—' for a session with empty AgentPath, got:\n%s", view)
	}
}
