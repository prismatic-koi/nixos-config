// Package cmd white-box tests for dashModel, popupModel, and persistentModel.
//
// These tests access unexported fields and helpers directly to verify that the
// CallerClient bug fix (capturing callerClient at init time and using
// dashSwitchTarget to select the right client) is exercised by the actual model
// code, not just by the underlying tmux primitives.
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

func (s *cmdTestServer) setGlobal(option, value string) {
	_ = s.run("set-option", "-g", option, value)
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

// ─── dashSwitchTarget unit tests ──────────────────────────────────────────────

// TestDashSwitchTarget exercises all branches of the dashSwitchTarget helper,
// which encapsulates the client-selection logic for persistent mode.
//
// This is the primary regression test for the CallerClient bug: the old code
// called tmux.CallerClient() live inside the handler, which returned the global
// stamp that could be overwritten by a concurrent dashboard open. The fix
// extracts the logic into dashSwitchTarget, which operates on immutable values
// captured at model-init time (from the --caller-client flag).
//
// In the new split architecture:
//   - Persistent mode: uses dashSwitchTarget(callerClient, client) — callerClient
//     wins if non-empty, falls back to client.
//   - Popup mode: always uses m.client directly (the popup runs inside the
//     caller's own tmux client, so no callerClient indirection is needed).
func TestDashSwitchTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		callerClient string
		client       string
		want         string
	}{
		{
			name:         "persistent mode uses callerClient when set",
			callerClient: "caller-client",
			client:       "process-client",
			want:         "caller-client",
		},
		{
			name:         "persistent mode falls back to client when callerClient empty",
			callerClient: "",
			client:       "process-client",
			want:         "process-client",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := dashSwitchTarget(tc.callerClient, tc.client)
			if got != tc.want {
				t.Errorf("dashSwitchTarget(%q, %q) = %q, want %q",
					tc.callerClient, tc.client, got, tc.want)
			}
		})
	}
}

// TestPopupSwitchTarget verifies that popup mode always uses m.client
// (the popup runs inside the caller's own tmux client — no indirection needed).
func TestPopupSwitchTarget(t *testing.T) {
	t.Parallel()

	m := popupModel{
		client: "popup-client",
	}
	if got := m.switchTarget(); got != "popup-client" {
		t.Errorf("popupModel.switchTarget() = %q, want %q", got, "popup-client")
	}
}

// TestDashModelEnterCursorActivation verifies that in persistent-session mode,
// pressing Enter when the cursor is inactive activates the cursor without
// switching sessions — matching the j/k behaviour.
func TestDashModelEnterCursorActivation(t *testing.T) {
	t.Parallel()

	m := dashModel{
		popup:        false,
		cursorActive: false,
		dashShared: dashShared{
			sessions: []agentSession{{Name: "some-session"}},
		},
	}

	updatedModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	dm := updatedModel.(dashModel)

	if !dm.cursorActive {
		t.Error("cursorActive should be true after first Enter in persistent mode")
	}
	if cmd == nil {
		t.Error("cmd should be non-nil (cursorTimeoutCmd)")
	}
	// The sessions list is unchanged and no switch was initiated — if a switch
	// had fired, the handler would have returned tea.Sequence(sideEffect, tea.Quit)
	// which has a different shape than cursorTimeoutCmd(). We can detect this by
	// checking that the returned message is a cursorTimeoutMsg (fires after a delay).
	// We just verify no immediate quit was issued by ensuring the model is still valid.
	if len(dm.sessions) == 0 {
		t.Error("sessions should be unchanged")
	}
}

// ─── dashModel init tests ─────────────────────────────────────────────────────

// TestDashModelCallerClientCapturedAtInit verifies that newDashModel captures
// the CallerClient stamp at init time and stores it as an immutable field, so
// that a subsequent change to the global stamp does not affect the model.
func TestDashModelCallerClientCapturedAtInit(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s) // redirects TmuxBin; must not be called from parallel tests
	s.newSession("some-session")

	// Stamp the global with "client-A" before constructing the model.
	s.setGlobal("@prism_caller_client", "client-A")
	s.setGlobal("@prism_caller", "some-session")

	// Construct the model — it should capture "client-A".
	model := newDashModel("process-client", false)

	if model.callerClient != "client-A" {
		t.Fatalf("callerClient = %q, want %q (should be captured at init)", model.callerClient, "client-A")
	}

	// Now overwrite the global stamp with "client-B" (simulates another client
	// opening the dashboard after our model was constructed).
	s.setGlobal("@prism_caller_client", "client-B")

	// The model's callerClient must NOT change — it was captured at init.
	if model.callerClient != "client-A" {
		t.Fatalf("callerClient changed to %q after global stamp updated — bug: CallerClient() was called lazily", model.callerClient)
	}
}

// ─── filter mode tests ────────────────────────────────────────────────────────

// TestDashFilterActivation verifies that pressing '/' activates filter mode and
// sets cursorActive.
func TestDashFilterActivation(t *testing.T) {
	t.Parallel()

	m := dashModel{
		dashShared: dashShared{
			sessions:  []agentSession{{Name: "alpha"}, {Name: "beta"}},
			displayed: []agentSession{{Name: "alpha"}, {Name: "beta"}},
		},
	}
	updatedModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	dm := updatedModel.(dashModel)

	if !dm.filterActive {
		t.Error("filterActive should be true after pressing '/'")
	}
	if !dm.cursorActive {
		t.Error("cursorActive should be true after activating filter")
	}
	if dm.filterText != "" {
		t.Errorf("filterText should be empty on activation, got %q", dm.filterText)
	}
}

// TestDashFilterNarrowsList verifies that typing characters in filter mode
// narrows m.displayed to only fuzzy-matching sessions.
func TestDashFilterNarrowsList(t *testing.T) {
	t.Parallel()

	sessions := []agentSession{
		{Name: "alpha"},
		{Name: "beta"},
		{Name: "aleph"},
	}
	m := dashModel{
		dashShared: dashShared{
			sessions:     sessions,
			displayed:    sessions,
			filterActive: true,
		},
	}

	// Type "al" — should match "alpha" and "aleph", not "beta".
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	dm := m2.(dashModel)
	m3, _ := dm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	dm = m3.(dashModel)

	if len(dm.displayed) != 2 {
		t.Errorf("displayed len = %d, want 2 (alpha + aleph)", len(dm.displayed))
	}
	for _, s := range dm.displayed {
		if s.Name == "beta" {
			t.Error("'beta' should not appear in filtered list for pattern 'al'")
		}
	}
	if dm.filterText != "al" {
		t.Errorf("filterText = %q, want %q", dm.filterText, "al")
	}
}

// TestDashFilterBackspace verifies that backspace removes the last character
// from filterText and re-expands the list.
func TestDashFilterBackspace(t *testing.T) {
	t.Parallel()

	sessions := []agentSession{{Name: "alpha"}, {Name: "beta"}}
	m := dashModel{
		dashShared: dashShared{
			sessions:     sessions,
			displayed:    sessions,
			filterActive: true,
			filterText:   "al",
		},
	}
	// Pre-narrow so displayed only has "alpha".
	m = dashRefilter(m)
	if len(m.displayed) != 1 {
		t.Fatalf("setup: displayed len = %d, want 1", len(m.displayed))
	}

	// Backspace should remove 'l' and re-expand.
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	dm := m2.(dashModel)

	if dm.filterText != "a" {
		t.Errorf("filterText = %q, want %q after backspace", dm.filterText, "a")
	}
	// "a" matches both "alpha" and "beta" (b-e-t-a has 'a').
	if len(dm.displayed) == 0 {
		t.Error("displayed should be non-empty after backspace")
	}
}

// TestDashFilterEscapeCancels verifies that pressing Esc in filter mode
// cancels the filter and restores the full session list.
func TestDashFilterEscapeCancels(t *testing.T) {
	t.Parallel()

	sessions := []agentSession{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}}
	m := dashModel{
		dashShared: dashShared{
			sessions:     sessions,
			displayed:    sessions[:1], // narrowed
			filterActive: true,
			filterText:   "al",
		},
	}

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	dm := m2.(dashModel)

	if dm.filterActive {
		t.Error("filterActive should be false after Esc")
	}
	if dm.filterText != "" {
		t.Errorf("filterText should be cleared, got %q", dm.filterText)
	}
	if len(dm.displayed) != len(sessions) {
		t.Errorf("displayed len = %d after cancel, want %d (full list)", len(dm.displayed), len(sessions))
	}
}

// TestDashFilterEnterSwitches verifies that pressing Enter in filter mode with
// an active selection returns a non-nil switch command and exits filter mode.
func TestDashFilterEnterSwitches(t *testing.T) {
	t.Parallel()

	sessions := []agentSession{{Name: "alpha"}, {Name: "beta"}}
	m := dashModel{
		popup: true, // popup so we don't need callerClient
		dashShared: dashShared{
			sessions:     sessions,
			displayed:    sessions,
			filterActive: true,
			filterText:   "al",
			cursor:       0,
		},
	}
	m = dashRefilter(m)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Error("Enter in filter mode should return a non-nil switch command")
	}
}

// TestDashFilterCursorStaysActiveOnTimeout verifies that a cursorTimeoutMsg
// does not deactivate the cursor highlight while filter mode is active.
// Without the fix, the selection bar would silently vanish 3 seconds after
// pressing '/' even while the user is still typing.
func TestDashFilterCursorStaysActiveOnTimeout(t *testing.T) {
	t.Parallel()

	m := dashModel{
		popup:        false, // persistent mode: timeout normally deactivates cursor
		cursorActive: true,
		dashShared: dashShared{
			sessions:     []agentSession{{Name: "alpha"}, {Name: "beta"}},
			displayed:    []agentSession{{Name: "alpha"}, {Name: "beta"}},
			filterActive: true,
		},
	}

	// Fire the cursor timeout while filter mode is active.
	m2, _ := m.Update(cursorTimeoutMsg{})
	dm := m2.(dashModel)

	if !dm.cursorActive {
		t.Error("cursorActive must remain true when cursorTimeoutMsg fires during filter mode")
	}
}

// TestDashFilterBlurKeepsCursorActive verifies that a BlurMsg does not
// deactivate the cursor while filter mode is open. Without this guard the
// selection bar disappears on focus loss but the filter prompt remains, leaving
// the user unable to see which session Enter will select.
func TestDashFilterBlurKeepsCursorActive(t *testing.T) {
	t.Parallel()

	m := dashModel{
		popup:        false, // persistent mode: BlurMsg normally deactivates cursor
		cursorActive: true,
		dashShared: dashShared{
			sessions:     []agentSession{{Name: "alpha"}, {Name: "beta"}},
			displayed:    []agentSession{{Name: "alpha"}, {Name: "beta"}},
			filterActive: true,
		},
	}

	m2, _ := m.Update(tea.BlurMsg{})
	dm := m2.(dashModel)

	if !dm.cursorActive {
		t.Error("cursorActive must remain true when BlurMsg fires during filter mode")
	}
}

// TestDashFilterCtrlCQuitsProgram verifies that ctrl+c in filter mode quits
// the TUI rather than merely dismissing the filter. Previously ctrl+c and esc
// were handled by the same case, so ctrl+c was consumed without quitting.
func TestDashFilterCtrlCQuitsProgram(t *testing.T) {
	t.Parallel()

	m := dashModel{
		cursorActive: true,
		dashShared: dashShared{
			sessions:     []agentSession{{Name: "alpha"}},
			displayed:    []agentSession{{Name: "alpha"}},
			filterActive: true,
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

	m := dashModel{
		cursorActive: true,
		dashShared: dashShared{
			sessions:     []agentSession{{Name: "alpha"}, {Name: "beta"}},
			displayed:    []agentSession{{Name: "alpha"}}, // narrowed
			filterActive: true,
			filterText:   "al",
		},
	}

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	dm := m2.(dashModel)

	if cmd != nil {
		t.Error("Esc in filter mode should return nil cmd (no quit)")
	}
	if dm.filterActive {
		t.Error("filterActive should be false after Esc")
	}
	if len(dm.displayed) != 2 {
		t.Errorf("displayed len = %d, want 2 (full list restored)", len(dm.displayed))
	}
}

// TestDashFilterSnapSkippedWhenFilterActive verifies that the cursor-snap-to-
// currentSession logic in sessionsMsg is skipped when filter mode is already
// active. When filterActive is true, needsSnap is not set, so no snap into the
// sorted m.displayed list runs — avoiding a collision between an in-progress
// filter and a snap that would overwrite the cursor position.
func TestDashFilterSnapSkippedWhenFilterActive(t *testing.T) {
	t.Parallel()

	sessions := []agentSession{{Name: "alpha"}, {Name: "beta"}}

	// Filter is already active and narrowed to "beta" only.
	m := dashModel{
		currentSession: "alpha", // would snap cursor to index 0 in sessions
		dashShared: dashShared{
			filterActive:      true,
			filterText:        "bet",
			cursorInitialised: false,
			cursor:            0,
			sessions:          sessions,
			displayed:         []agentSession{{Name: "beta"}}, // pre-filtered
		},
	}

	// Deliver a sessionsMsg (the first tick).
	m2, _ := m.Update(sessionsMsg{sessions: sessions})
	dm := m2.(dashModel)

	if !dm.cursorInitialised {
		t.Error("cursorInitialised should be true after first sessionsMsg")
	}
	// The snap should have been skipped; cursor must still point into the
	// filtered list (at most index 0 since displayed has one entry).
	if dm.cursor != 0 {
		t.Errorf("cursor = %d, want 0 — snap into m.displayed must not run during filter mode", dm.cursor)
	}
	// "beta" must still be the first displayed entry.
	if len(dm.displayed) == 0 || dm.displayed[0].Name != "beta" {
		t.Errorf("displayed[0] = %v, want {Name:beta}", dm.displayed)
	}
}

// TestDashFilterCursorNavigation verifies that j/k move the cursor within the
// filtered list while filter mode is active.
func TestDashFilterCursorNavigation(t *testing.T) {
	t.Parallel()

	sessions := []agentSession{{Name: "alpha"}, {Name: "aleph"}}
	m := dashModel{
		dashShared: dashShared{
			sessions:     sessions,
			displayed:    sessions,
			filterActive: true,
			cursor:       0,
		},
	}

	// Press 'j' to move down.
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	dm := m2.(dashModel)
	if dm.cursor != 1 {
		t.Errorf("cursor = %d after j, want 1", dm.cursor)
	}

	// Press 'k' to move back up.
	m3, _ := dm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	dm = m3.(dashModel)
	if dm.cursor != 0 {
		t.Errorf("cursor = %d after k, want 0", dm.cursor)
	}
}

// TestDashRefilterClampsOOBCursor verifies that dashRefilter clamps the cursor
// when the filter reduces the list below the current cursor position.
func TestDashRefilterClampsOOBCursor(t *testing.T) {
	t.Parallel()

	sessions := []agentSession{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}}
	m := dashModel{
		dashShared: dashShared{
			sessions:     sessions,
			displayed:    sessions,
			filterActive: true,
			cursor:       2, // pointing at "gamma"
			filterText:   "al",
		},
	}
	m = dashRefilter(m) // "al" only matches "alpha"

	if len(m.displayed) != 1 {
		t.Fatalf("expected 1 match for 'al', got %d", len(m.displayed))
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (clamped to end of filtered list)", m.cursor)
	}
}

// TestDashFilterViewShowsPrompt checks that the View output contains the
// filter prompt when filterActive is true.
func TestDashFilterViewShowsPrompt(t *testing.T) {
	t.Parallel()

	m := dashModel{
		cursorActive: true,
		dashShared: dashShared{
			sessions:     []agentSession{{Name: "alpha"}},
			displayed:    []agentSession{{Name: "alpha"}},
			filterActive: true,
			filterText:   "alp",
			width:        80,
			height:       40,
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

	m := dashModel{
		dashShared: dashShared{
			sessions:  []agentSession{{Name: "alpha"}},
			displayed: []agentSession{{Name: "alpha"}},
			width:     80,
			height:    40,
		},
	}
	view := m.View()
	if !strings.Contains(view, "/ filter") {
		t.Errorf("View() should mention '/ filter' in the help line, got:\n%s", view)
	}
}

// TestPersistentModelEnterSwitchesClient verifies that Enter in persistent mode
// switches m.client (the viewing client) to the selected session and stays
// alive — the TUI does NOT quit (no tea.QuitMsg returned).
//
// The persistent dashboard no longer has a callerClient field; it always
// operates on m.client, the client that is currently attached to the session.
func TestPersistentModelEnterSwitchesClient(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s) // redirects TmuxBin; must not be called from parallel tests
	s.newSession("nixos-config@main")
	s.newSession("nixos-config@feature")

	client := s.attachClientToSession(t, "nixos-config@main")

	// Build a persistentModel whose m.client is the real attached client.
	model := newPersistentModel(client, "nixos-config@main")
	model.sessions = []agentSession{{Name: "nixos-config@feature"}}
	model.displayed = model.sessions
	model.cursor = 0
	model.cursorActive = true // cursor active so Enter fires immediately

	// Verify Update(Enter) returns a non-nil cmd.
	updatedModel, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Update(Enter) returned nil cmd — handler did not fire")
	}
	pm := updatedModel.(persistentModel)
	// Cursor should be deactivated after Enter (returning to passive watch mode).
	if pm.cursorActive {
		t.Error("cursorActive should be false after Enter in persistent mode (returns to passive watch)")
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
// calls SwitchClientLast (switch-client -l) on m.client to return it to its
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

	// Build a persistentModel with m.client set to the real attached client.
	model := newPersistentModel(client, "nixos-config@feature")
	model.cursorActive = true // cursor must be active for q to fire the switch

	// Send 'q' — should return a non-nil cmd and not quit.
	updatedModel, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("Update('q') returned nil cmd — handler did not fire")
	}
	pm := updatedModel.(persistentModel)
	if pm.cursorActive {
		t.Error("cursorActive should be false after q (passive watch mode)")
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

// TestSortDisplayed verifies the sort order produced by sortDisplayed:
//   - repos sort alphabetically
//   - within a repo, @main and plain sessions (no @) sort before worktree branches
//   - worktree branches sort alphabetically after @main
func TestSortDisplayed(t *testing.T) {
	t.Parallel()

	input := []agentSession{
		{Name: "repoB@branch-z"},
		{Name: "repoA@branch-b"},
		{Name: "repoA@main"},
		{Name: "repoB@main"},
		{Name: "repoA@branch-a"},
		{Name: "plain-session"}, // no "@" — sorts first within its "repo"
	}

	sortDisplayed(input)

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

	m := dashModel{
		dashShared: dashShared{
			sessions:  []agentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			displayed: []agentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			gitStats: map[string]gitStatResult{
				"/some/path": {Ok: false}, // stat failed
			},
			width:  80,
			height: 40,
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

	m := dashModel{
		dashShared: dashShared{
			sessions:  []agentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			displayed: []agentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			gitStats: map[string]gitStatResult{
				"/some/path": {Ok: true}, // stat succeeded, zero DiffStat = clean
			},
			width:  80,
			height: 40,
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

	m := dashModel{
		dashShared: dashShared{
			sessions:  []agentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			displayed: []agentSession{{Name: "repo@main", AgentPath: "/some/path"}},
			gitStats: map[string]gitStatResult{
				"/some/path": {
					Ok:   true,
					Stat: git.DiffStat{Files: 3, Insertions: 42, Deletions: 7},
				},
			},
			width:  120,
			height: 40,
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

	sessions := []agentSession{
		{Name: "repo@main", AgentPath: "/good/path"},
		{Name: "repo@bad", AgentPath: "/bad/path"},
	}
	m := dashModel{
		dashShared: dashShared{
			sessions:  sessions,
			displayed: sessions,
			gitStats: map[string]gitStatResult{
				"/good/path": {Ok: true},  // clean
				"/bad/path":  {Ok: false}, // stat failed
			},
			width:  120,
			height: 40,
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

// TestDashViewStatEmptyAgentPath verifies that a session with an empty
// AgentPath renders "—" (no stat available), not "?" (which would imply a
// git error). Sessions with no worktree path are not stat-failed; they simply
// have no path to stat.
func TestDashViewStatEmptyAgentPath(t *testing.T) {
	t.Parallel()

	m := dashModel{
		dashShared: dashShared{
			sessions:  []agentSession{{Name: "scratchpad-like", AgentPath: ""}},
			displayed: []agentSession{{Name: "scratchpad-like", AgentPath: ""}},
			gitStats:  map[string]gitStatResult{}, // no entry for empty path
			width:     80,
			height:    40,
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
