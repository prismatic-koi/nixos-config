// Tests for `prism launch` (issue #2521).
//
// runLaunch has three branches, chosen by whether the caller is inside tmux
// (TMUX env var) and whether --in-terminal was passed:
//
//   - inTmux:          switch the calling tmux client to the dashboard.
//   - launchInTerminal: not in tmux, attach in the current terminal via
//     syscall.Exec (intercepted here via the syscallExec indirection so the
//     test process is not replaced).
//   - default:         not in tmux, spawn a new kitty window attached to the
//     dashboard (intercepted here via the execStart indirection, redirected
//     through a real pty via `script` against the isolated test tmux server,
//     so the test exercises tmux's actual command-list-abort-on-error
//     semantics rather than only inspecting argv).
//
// Issue #2521: the default branch used to chain scratchpad creation, dashboard
// creation, and the final attach into a single tmux command list run inside
// the new kitty window. If prism-dashboard already existed, the chained
// "new-session -ds prism-dashboard" command failed, which aborted the rest of
// the command list (including the trailing switch-client/attach), so the new
// client landed on scratchpad instead of the dashboard. The fix ensures both
// sessions from the Go side first (mirroring the other two branches) and then
// issues a single, un-chainable "attach-session" command.
package cmd

import (
	"bytes"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/dashboard"
)

// resetLaunchFlags resets the package-level launch flag vars to their zero
// values after a test, since they are cobra flag targets shared across
// invocations of runLaunch within the same test binary.
func resetLaunchFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		launchInTerminal = false
		launchPath = ""
		launchFresh = false
	})
}

// runViaPTYAndWaitForClient starts cmd (a tmux invocation, e.g. the one built
// by runLaunch's default branch) through a real pty via `script`, mirroring
// what happens when kitty spawns a new terminal window running that same
// argv. It waits for a new client to appear on the test server and returns
// its client name. t.Helper.
func runViaPTYAndWaitForClient(t *testing.T, s *cmdTestServer, cmd *exec.Cmd) string {
	t.Helper()

	before, _ := s.output("list-clients", "-F", "#{client_name}")
	beforeSet := map[string]bool{}
	for _, c := range strings.Split(before, "\n") {
		if c = strings.TrimSpace(c); c != "" {
			beforeSet[c] = true
		}
	}

	// cmd.Args[0] is the resolved kitty binary path; the remaining args
	// (after "--title", "Prism") are the tmux command list a real kitty
	// window would exec. Reproduce that by running it through `script`,
	// which supplies the pty tmux's attach/switch-client need.
	tmuxArgs := cmd.Args[3:]
	cmdStr := strings.Join(tmuxArgs, " ")

	var scriptStderr bytes.Buffer
	ptyCmd := exec.Command("script", scriptCmdArgs(cmdStr)...)
	ptyCmd.Stderr = &scriptStderr
	if err := ptyCmd.Start(); err != nil {
		t.Fatalf("run tmux command via pty: %v", err)
	}
	t.Cleanup(func() {
		_ = ptyCmd.Process.Kill()
		_ = ptyCmd.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	var clientName string
	var lastListOut string
	for time.Now().Before(deadline) {
		out, err := s.output("list-clients", "-F", "#{client_name}")
		if err == nil {
			lastListOut = out
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
		scriptAlive := "alive"
		if err := ptyCmd.Process.Signal(syscall.Signal(0)); err != nil {
			scriptAlive = "dead (" + err.Error() + ")"
		}
		t.Fatalf(
			"no new client appeared after running %q via pty (timeout)\n"+
				"  script process state: %s\n"+
				"  script stderr:        %q\n"+
				"  list-clients:         %q\n"+
				"  clients before:       %q",
			cmdStr, scriptAlive, scriptStderr.String(), lastListOut, before,
		)
	}
	return clientName
}

// interceptExecStart overrides execStart to run the command through
// runViaPTYAndWaitForClient and record the resulting client name, instead of
// actually spawning kitty (which is not installed in the test environment).
func interceptExecStart(t *testing.T, s *cmdTestServer) *string {
	t.Helper()
	var clientName string
	orig := execStart
	execStart = func(cmd *exec.Cmd) error {
		clientName = runViaPTYAndWaitForClient(t, s, cmd)
		return nil
	}
	t.Cleanup(func() { execStart = orig })
	return &clientName
}

// TestRunLaunchInTmux_SwitchesClientToDashboard covers the inTmux branch: a
// client already attached to some other session must be switched to the
// dashboard, and the scratchpad fallback session must still exist afterward.
func TestRunLaunchInTmux_SwitchesClientToDashboard(t *testing.T) {
	skipIfSandboxPTY(t)
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	resetLaunchFlags(t)

	s.newSession("caller")
	client := s.attachClientToSession(t, "caller")

	t.Setenv("TMUX", "fake/0,0")

	if err := runLaunch(nil, nil); err != nil {
		t.Fatalf("runLaunch (inTmux branch): %v", err)
	}

	got, err := s.clientSession(client)
	if err != nil {
		t.Fatalf("clientSession: %v", err)
	}
	if got != dashboard.DashSession {
		t.Errorf("client landed on %q, want %q", got, dashboard.DashSession)
	}
	if !s.hasSession("scratchpad") {
		t.Error("scratchpad session was not created/preserved")
	}
	if !s.hasSession(dashboard.DashSession) {
		t.Error("prism-dashboard session was not created")
	}
}

// TestRunLaunchInTmux_PreexistingDashboardStillLandsThere covers the edge
// case where prism-dashboard already exists before the inTmux branch runs.
func TestRunLaunchInTmux_PreexistingDashboardStillLandsThere(t *testing.T) {
	skipIfSandboxPTY(t)
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	resetLaunchFlags(t)

	s.newSession("caller")
	s.newSession(dashboard.DashSession)
	client := s.attachClientToSession(t, "caller")

	t.Setenv("TMUX", "fake/0,0")

	if err := runLaunch(nil, nil); err != nil {
		t.Fatalf("runLaunch (inTmux branch, dashboard pre-existing): %v", err)
	}

	got, err := s.clientSession(client)
	if err != nil {
		t.Fatalf("clientSession: %v", err)
	}
	if got != dashboard.DashSession {
		t.Errorf("client landed on %q, want %q", got, dashboard.DashSession)
	}
}

// TestRunLaunchInTmux_CurrentClientErrorIsSurfaced covers the edge case where
// resolving the current tmux client fails: runLaunch must return a non-nil
// error instead of silently landing on the wrong (or no) session, which is
// what happened when the error from tmux.CurrentClient() was discarded.
func TestRunLaunchInTmux_CurrentClientErrorIsSurfaced(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	resetLaunchFlags(t)

	// No client is attached to the test server, so "display-message -p
	// #{client_name}" (tmux.CurrentClient) fails with "no client".
	t.Setenv("TMUX", "fake/0,0")

	err := runLaunch(nil, nil)
	if err == nil {
		t.Fatal("runLaunch: expected error when the current tmux client cannot be resolved, got nil")
	}
}

// TestRunLaunchInTerminal_ExecTargetsDashboard covers the launchInTerminal
// branch: syscallExecTmux must be invoked with the dashboard session as the
// attach target. The real syscall.Exec is intercepted via the syscallExec
// indirection so the test process is not replaced.
func TestRunLaunchInTerminal_ExecTargetsDashboard(t *testing.T) {
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	resetLaunchFlags(t)

	launchInTerminal = true

	var gotArgv []string
	origExec := syscallExec
	syscallExec = func(_ string, argv []string, _ []string) error {
		gotArgv = argv
		return nil
	}
	t.Cleanup(func() { syscallExec = origExec })

	if err := runLaunch(nil, nil); err != nil {
		t.Fatalf("runLaunch (launchInTerminal branch): %v", err)
	}

	if len(gotArgv) == 0 {
		t.Fatal("syscallExec was never called")
	}
	joined := strings.Join(gotArgv, " ")
	if !strings.Contains(joined, "attach-session") || !strings.HasSuffix(joined, dashboard.DashSession) {
		t.Errorf("exec argv %v does not attach to %q", gotArgv, dashboard.DashSession)
	}
	if !s.hasSession("scratchpad") {
		t.Error("scratchpad session was not created")
	}
	if !s.hasSession(dashboard.DashSession) {
		t.Error("prism-dashboard session was not created")
	}
}

// TestRunLaunchDefault_KittyAttachesToDashboard covers the outside-tmux,
// non-terminal branch: the client spawned by the kitty invocation must end up
// attached to the dashboard session (asserted via a real pty run against the
// isolated test tmux server, not just an argv inspection).
func TestRunLaunchDefault_KittyAttachesToDashboard(t *testing.T) {
	skipIfSandboxPTY(t)
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	resetLaunchFlags(t)

	client := interceptExecStart(t, s)

	if err := runLaunch(nil, nil); err != nil {
		t.Fatalf("runLaunch (default branch): %v", err)
	}

	got, err := s.clientSession(*client)
	if err != nil {
		t.Fatalf("clientSession: %v", err)
	}
	if got != dashboard.DashSession {
		t.Errorf("client landed on %q, want %q", got, dashboard.DashSession)
	}
	if !s.hasSession("scratchpad") {
		t.Error("scratchpad session was not created")
	}
}

// TestRunLaunchDefault_PreexistingDashboardStillSucceeds is the direct
// regression test for issue #2521's reported failure mode: prism-dashboard
// already exists (e.g. from a previous prism launch) when the default branch
// runs again. Before the fix, the chained "new-session -ds prism-dashboard"
// command failed because the session already existed, which aborted the rest
// of the tmux command list — including the trailing attach — so the client
// landed on scratchpad instead.
func TestRunLaunchDefault_PreexistingDashboardStillSucceeds(t *testing.T) {
	skipIfSandboxPTY(t)
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	resetLaunchFlags(t)

	s.newSession(dashboard.DashSession)
	s.newSession("scratchpad")

	client := interceptExecStart(t, s)

	if err := runLaunch(nil, nil); err != nil {
		t.Fatalf("runLaunch (default branch, dashboard pre-existing): %v", err)
	}

	got, err := s.clientSession(*client)
	if err != nil {
		t.Fatalf("clientSession: %v", err)
	}
	if got != dashboard.DashSession {
		t.Errorf("client landed on %q, want %q — this is issue #2521: a chained tmux command list aborts on the first failing command", got, dashboard.DashSession)
	}
	if !s.hasSession("scratchpad") {
		t.Error("scratchpad session no longer exists")
	}
}

// TestRunLaunchPath_DoesNotTouchDashboard verifies that --path continues to
// bypass the dashboard entirely and does not create or reference the
// dashboard session, preserving the existing ALT+o / ALT+n / zsh ^o behaviour.
// Exercised via the inTmux branch of runLaunchWithPath, which is the one that
// explicitly ensures the scratchpad session on the Go side (the other two
// branches build scratchpad creation into a kitty/tmux command list that is
// unrelated to this issue's scope and left unchanged).
func TestRunLaunchPath_DoesNotTouchDashboard(t *testing.T) {
	skipIfSandboxPTY(t)
	s := newCmdTestServer(t)
	withCmdServer(t, s)
	resetLaunchFlags(t)

	launchPath = "/tmp/some-project"

	s.newSession("caller")
	client := s.attachClientToSession(t, "caller")

	t.Setenv("TMUX", "fake/0,0")

	// openSwitcher's final "display-popup" can legitimately fail in a headless
	// test environment lacking a real display/curses-capable pty; what this
	// test cares about is the session/client state produced up to that point,
	// so the error is not fatal here.
	_ = runLaunch(nil, nil)

	if !s.hasSession("scratchpad") {
		t.Fatal("--path invocation must still create the scratchpad session")
	}
	if s.hasSession(dashboard.DashSession) {
		t.Error("--path invocation must not create the dashboard session")
	}
	got, err := s.clientSession(client)
	if err != nil {
		t.Fatalf("clientSession: %v", err)
	}
	if got != "scratchpad" {
		t.Errorf("--path invocation must switch the client to scratchpad, got %q", got)
	}
}
