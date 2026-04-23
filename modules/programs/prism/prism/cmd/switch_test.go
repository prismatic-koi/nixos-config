// Package cmd integration tests for prism switch.
//
// TestSwitchPath verifies the end-to-end path:
//
//	prism switch --path <dir>
//
// Design: we invoke the prism binary as a new-window command rather than via
// send-keys. send-keys types characters into the PTY which echoes them back;
// if the command is long the PTY output buffer fills and the tmux server
// blocks until script drains the PTY — causing a deadlock with
// CombinedOutput's pipe.  new-window runs the command directly (no PTY echo)
// and returns immediately, avoiding the deadlock.
//
// Because the command runs as a new-window inside the test server, tmux sets
// TMUX in the pane environment to the test socket automatically. The prism
// binary calls display-message -p #{client_name} which returns the correct
// client name (the script-attached client viewing the session).
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/session"
)

// buildPrismBinary compiles the prism binary from the module root into a temp
// directory and returns the path to the resulting executable.
func buildPrismBinary(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found in PATH — skipping integration test")
	}
	// Module root is one directory above this file's package (cmd/ → prism/).
	// In Go tests, the working directory is the package directory (cmd/), so
	// the module root is one level up.
	moduleRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	outDir := t.TempDir()
	outBin := filepath.Join(outDir, "prism")
	cmd := exec.Command("go", "build", "-o", outBin, ".")
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return outBin
}

// runInNewWindow launches cmd as the command for a new temporary window in
// targetSession and returns immediately (does not wait for the command to
// finish).  The window will close on its own when cmd exits.  Using new-window
// avoids the PTY echo buffer issue that arises with send-keys when the command
// string is longer than the terminal width.
func runInNewWindow(t *testing.T, s *cmdTestServer, targetSession, workdir, cmd string) {
	t.Helper()
	if err := s.run("new-window", "-t", targetSession+":", "-c", workdir, cmd); err != nil {
		t.Fatalf("new-window in %q: %v", targetSession, err)
	}
}

// TestSwitchPath_SwitchesClientToNewSession exercises the --path fast path in
// switchCmd. It:
//
//  1. Builds the prism binary.
//  2. Creates an isolated test tmux server.
//  3. Attaches a real client to a "home" session.
//  4. Runs `prism switch --path <targetDir>` as a new-window command.
//  5. Polls until the client's session matches the target session name.
//
// Because the new-window runs inside the test server, tmux sets TMUX in the
// command's environment to the test socket — the binary contacts the right
// server and CurrentClient() returns the script-attached client correctly.
func TestSwitchPath_SwitchesClientToNewSession(t *testing.T) {
	// This test mutates package-level state (TmuxBin) via withCmdServer, so it
	// must NOT run in parallel.
	skipIfSandboxPTY(t)

	// Redirect XDG_STATE_HOME to a per-test temp dir so the prism binary
	// writes its DB and sidecar state to an isolated location instead of the
	// production ~/.local/state/prism/ path.  Must be set before newCmdTestServer
	// starts the tmux server (which inherits the environment).
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Ignore the host's global gitconfig so the spawned prism binary is not
	// affected by host-level signing config (gpgsign=true without a key
	// available would break any git commit it runs).
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	// Clear PRISM_HOST_API so the spawned binary runs the host-side switch
	// path directly rather than proxying to a sidecar that doesn't know
	// about the test's isolated tmux server. This env var is inherited when
	// the test itself runs inside a prism-managed session.
	t.Setenv("PRISM_HOST_API", "")

	prismBin := buildPrismBinary(t)

	s := newCmdTestServer(t)
	withCmdServer(t, s)

	// Create a "home" session for the client to start on.
	s.newSession("home")

	// Create a target directory. The session name will be derived from the
	// base name of the directory (filepath.Base), so use a predictable name.
	targetDir := filepath.Join(t.TempDir(), "myproject")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir targetDir: %v", err)
	}

	// Attach a real client to the "home" session.
	clientName := s.attachClientToSession(t, "home")

	// Run `prism switch --path <targetDir>` inside a new window on "home".
	// The new window's environment has TMUX set to the test socket, so the
	// binary can call display-message -p #{client_name} correctly.
	// Using new-window avoids the PTY echo buffer deadlock that send-keys
	// causes with long command strings (see package-level comment).
	switchArgs := fmt.Sprintf("%s switch --path %s", prismBin, targetDir)
	runInNewWindow(t, s, "home", "/tmp", switchArgs)

	// Derive the expected session name via the same helper used by the binary.
	expectedSession := session.NameFor(targetDir, "")

	// Register sidecar cleanup before polling — the binary may have already
	// launched a sidecar by the time the session appears in tmux.
	t.Cleanup(func() { session.KillSidecar(expectedSession) })

	// Poll until the client moves to the expected session.
	deadline := time.Now().Add(10 * time.Second)
	var gotSession string
	for time.Now().Before(deadline) {
		sess, err := s.clientSession(clientName)
		if err == nil {
			gotSession = sess
			if sess == expectedSession {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	if gotSession != expectedSession {
		t.Errorf("client %q session = %q, want %q (timed out waiting for switch)",
			clientName, gotSession, expectedSession)
	}
}

// TestSwitchPath_OnlyMovesTargetClient verifies that when two clients are
// attached to different sessions, running `prism switch --path <dir>` in one
// session's new-window moves only the client attached to that session — not the
// other.
func TestSwitchPath_OnlyMovesTargetClient(t *testing.T) {
	// Mutates TmuxBin via withCmdServer — must not be parallel.
	skipIfSandboxPTY(t)

	// Redirect XDG_STATE_HOME to a per-test temp dir so the prism binary
	// writes its DB and sidecar state to an isolated location instead of the
	// production ~/.local/state/prism/ path.  Must be set before newCmdTestServer
	// starts the tmux server (which inherits the environment).
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Ignore the host's global gitconfig and unset PRISM_HOST_API for the
	// same reasons as the first test in this file — see
	// TestSwitchPath_SwitchesClientToNewSession.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("PRISM_HOST_API", "")

	prismBin := buildPrismBinary(t)

	s := newCmdTestServer(t)
	withCmdServer(t, s)

	s.newSession("sessionA")
	s.newSession("sessionB")

	targetDir := filepath.Join(t.TempDir(), "targetproject")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir targetDir: %v", err)
	}

	// Attach two real clients to different sessions.
	clientA := s.attachClientToSession(t, "sessionA")
	clientB := s.attachClientToSession(t, "sessionB")

	// Run prism switch in a new window on sessionA.  The binary's
	// display-message call returns clientA (the only client on sessionA).
	switchArgs := fmt.Sprintf("%s switch --path %s", prismBin, targetDir)
	runInNewWindow(t, s, "sessionA", "/tmp", switchArgs)

	expectedSession := session.NameFor(targetDir, "")

	// Register sidecar cleanup before polling — the binary may have already
	// launched a sidecar by the time the session appears in tmux.
	t.Cleanup(func() { session.KillSidecar(expectedSession) })

	// Wait for clientA to land on the target session.
	deadline := time.Now().Add(10 * time.Second)
	var gotA string
	for time.Now().Before(deadline) {
		sess, err := s.clientSession(clientA)
		if err == nil {
			gotA = sess
			if sess == expectedSession {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Confirm clientB stayed on "sessionB" over a short stability window.
	// We poll and track the last successful read; if no successful read is
	// obtained (e.g. tmux is briefly unresponsive) we skip the assertion to
	// avoid a spurious failure — a transient error is not evidence of a wrong
	// switch.
	var gotB string
	bDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(bDeadline) {
		if sess, err := s.clientSession(clientB); err == nil {
			gotB = sess
		}
		time.Sleep(50 * time.Millisecond)
	}

	if gotA != expectedSession {
		t.Errorf("clientA session = %q, want %q — switch did not move the right client", gotA, expectedSession)
	}
	if gotB != "" && gotB != "sessionB" {
		t.Errorf("clientB session = %q, want %q — unrelated client was incorrectly moved", gotB, "sessionB")
	}
}
