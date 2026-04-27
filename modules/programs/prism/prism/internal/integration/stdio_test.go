package integration_test

// stdio_test.go contains integration tests for the B.3 Stage 2 fake stdio
// harness path. These tests verify that runStartupStdio correctly:
//
//  1. Launches the harness binary under bwrap, reads its JSONL frames, writes
//     them to agent_events, and transitions the session to state=finished.
//  2. Handles the error path where the harness exits before writing any frames.
//
// The "harness binary" used here is the test binary itself, invoked with
// PRISM_FAKE_STDIO_HARNESS set to control its behaviour (see testmain_test.go).
//
// Both tests require bwrap to be available on the host — they are skipped
// automatically when bwrap is not found in PATH. This mirrors the requireTmux
// pattern used by the tmux integration tests in integration_test.go.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/sidecar"
)

// requireBwrap skips the test if bwrap is not available in PATH.
func requireBwrap(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not found in PATH — skipping bwrap stdio integration test")
	}
	return bin
}

// testBinaryPath returns the path to the current test binary.
// This is the binary the tests use as the fake stdio harness by setting
// PRISM_FAKE_STDIO_HARNESS before the sidecar launches it under bwrap.
func testBinaryPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// Resolve symlinks so bwrap can bind-mount the real directory.
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// EvalSymlinks can fail on some platforms; fall back to the raw path.
		return exe
	}
	return real
}

// newStdioSidecar constructs a sidecar configured for the "fake-stdio" harness
// with the given binary path. The sidecar is not started; callers must invoke
// sc.Run(ctx).
func newStdioSidecar(t *testing.T, d *db.DB, bwrapBin, harnessBinPath string) *sidecar.Sidecar {
	t.Helper()

	const (
		sessionName = "test-repo@stdio-test"
		repo        = "test-repo"
		worktree    = "/tmp/test-stdio-worktree"
	)

	h, err := harness.New("fake-stdio", "", nil, "", "")
	if err != nil {
		t.Fatalf("harness.New(fake-stdio): %v", err)
	}

	cfg := sidecar.Config{
		SessionName:       sessionName,
		Repo:              repo,
		Worktree:          worktree,
		DB:                d,
		Clock:             sidecar.RealClock(),
		Harness:           h,
		HarnessName:       "fake-stdio",
		HarnessBinaryPath: harnessBinPath,
		BwrapPath:         bwrapBin,
	}
	return sidecar.New(cfg)
}

// TestRunStartupStdio_HappyPath verifies that runStartupStdio, invoked via
// sc.Run, correctly:
//
//   - Launches the fake harness under bwrap.
//   - Returns without error when the harness writes 3 valid JSONL frames and
//     exits 0.
//   - Writes a state_change event for each state_change frame.
//   - Writes a msg_assistant event for the assistant message frame.
//   - Transitions the session to state=finished in the DB.
//
// NOTE: Does not call t.Parallel() — uses t.Setenv to set the fake harness
// mode, which requires a non-parallel test.
func TestRunStartupStdio_HappyPath(t *testing.T) {
	bwrapBin := requireBwrap(t)

	// Point PRISM_FAKE_STDIO_HARNESS=normal so the test binary writes the
	// standard 3-frame sequence when invoked as the harness.
	t.Setenv("PRISM_FAKE_STDIO_HARNESS", "normal")

	d := openTestDB(t)
	sc := newStdioSidecar(t, d, bwrapBin, testBinaryPath(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := sc.Run(ctx); err != nil {
		t.Fatalf("sc.Run: unexpected error: %v", err)
	}

	// Assert state=finished in the DB.
	status, err := d.CurrentStatus("test-repo@stdio-test")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	if status.State != string(agent.StateFinished) {
		t.Errorf("state: got %q, want %q", status.State, agent.StateFinished)
	}

	// Assert that agent_events contains at least one state_change event and
	// one msg_assistant event.
	events, err := d.AllSessionEvents("test-repo@stdio-test")
	if err != nil {
		t.Fatalf("AllSessionEvents: %v", err)
	}

	var stateChangeCount, msgAssistantCount int
	for _, e := range events {
		switch e.Type {
		case "state_change":
			stateChangeCount++
		case "msg_assistant":
			msgAssistantCount++
		}
	}

	if stateChangeCount < 2 {
		t.Errorf("state_change event count: got %d, want >= 2 (active + finished)", stateChangeCount)
	}
	if msgAssistantCount < 1 {
		t.Errorf("msg_assistant event count: got %d, want >= 1", msgAssistantCount)
	}
}

// TestRunStartupStdio_SilentExit verifies the error path: when the harness
// binary exits 0 without writing any frames, runStartupStdio writes a
// startup-error event and returns an error.
//
// NOTE: Does not call t.Parallel() — uses t.Setenv to set the fake harness
// mode, which requires a non-parallel test.
func TestRunStartupStdio_SilentExit(t *testing.T) {
	bwrapBin := requireBwrap(t)

	// Point PRISM_FAKE_STDIO_HARNESS=silent so the test binary exits
	// immediately without writing any JSONL frames.
	t.Setenv("PRISM_FAKE_STDIO_HARNESS", "silent")

	d := openTestDB(t)
	sc := newStdioSidecar(t, d, bwrapBin, testBinaryPath(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := sc.Run(ctx)
	if err == nil {
		t.Fatal("sc.Run: expected error (harness exited without writing frames), got nil")
	}
	t.Logf("sc.Run returned expected error: %v", err)

	// Assert a startup-error state_change event was written.
	events, dbErr := d.AllSessionEvents("test-repo@stdio-test")
	if dbErr != nil {
		t.Fatalf("AllSessionEvents: %v", dbErr)
	}

	var errorStateFound bool
	for _, e := range events {
		if e.Type == "state_change" && strings.Contains(e.Payload, `"error"`) {
			errorStateFound = true
			break
		}
	}
	if !errorStateFound {
		t.Error("expected a state_change event with state=error in agent_events, but none found")
		for _, e := range events {
			t.Logf("  event: type=%q payload=%s", e.Type, e.Payload)
		}
	}

	// DB state should be "error".
	status, err2 := d.CurrentStatus("test-repo@stdio-test")
	if err2 != nil {
		t.Fatalf("CurrentStatus: %v", err2)
	}
	if status == nil {
		t.Fatal("CurrentStatus: got nil, want a row")
	}
	if status.State != string(agent.StateError) {
		t.Errorf("state: got %q, want %q", status.State, agent.StateError)
	}
}
