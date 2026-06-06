// Tests for the prismd mux cobra surface. Every test redirects
// $XDG_STATE_HOME to t.TempDir() so the homeless-shelter signal in
// the nix sandbox is preserved.
//
// The suite is structured as a thin layer on top of the lifecycle
// package's own tests — the heavy lifting (start/stop/status semantic
// invariants) is exercised in lifecycle_test.go. Here we confirm the
// cobra wiring routes flags to lifecycle.Config correctly, exit codes
// match the convention the systemd unit will rely on, and the
// output strings are stable.
package internalcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/mux/lifecycle"
)

// runRoot executes the prismd cobra tree with the given args. ctx is
// attached to the root command so foreground tests can cancel.
func runRoot(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRoot()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	if ctx != nil {
		root.SetContext(ctx)
	}
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// exitCode unwraps an exitError and returns its code. Returns 0 for
// nil, -1 for a non-exitError (indicating a programming bug — the
// cobra surface should always return either nil or an exitError).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ec *exitError
	if errors.As(err, &ec) {
		return ec.code
	}
	return -1
}

// stateRoot redirects $XDG_STATE_HOME to t.TempDir() and returns the
// derived paths a test would want to assert on.
func stateRoot(t *testing.T) (xdg, pidPath, sockPath, snapPath string) {
	t.Helper()
	xdg = t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	pidPath = filepath.Join(xdg, "prism", "run", "mux.pid")
	// Use an explicit short socket path so the default
	// $XDG_STATE_HOME/prism/run/<hash>/mux.sock never goes near the
	// sun_path budget regardless of /tmp depth.
	sockDir := t.TempDir()
	sockPath = filepath.Join(sockDir, "s")
	if len(sockPath) >= 100 {
		t.Skipf("temp socket path too long: %s", sockPath)
	}
	snapPath = filepath.Join(xdg, "prism", "mux", "session.json")
	return xdg, pidPath, sockPath, snapPath
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func TestStatus_Stopped(t *testing.T) {
	_, pidPath, _, _ := stateRoot(t)
	out, _, err := runRoot(t, nil, "mux", "status", "--pid-file", pidPath)
	if got := exitCode(err); got != 1 {
		t.Errorf("exit = %d, want 1 (stopped); err=%v", got, err)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("stdout = %q, want to contain 'stopped'", out)
	}
}

func TestStatus_StalePIDFile(t *testing.T) {
	_, pidPath, _, _ := stateRoot(t)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	out, _, err := runRoot(t, nil, "mux", "status", "--pid-file", pidPath)
	if got := exitCode(err); got != 2 {
		t.Errorf("exit = %d, want 2 (stale); err=%v", got, err)
	}
	if !strings.Contains(out, "stale") {
		t.Errorf("stdout = %q, want to contain 'stale'", out)
	}
}

// ---------------------------------------------------------------------------
// start + status + stop — end-to-end via the cobra surface
// ---------------------------------------------------------------------------

// TestStart_ForegroundLifecycle exercises the full cobra surface:
//
//   - start --foreground (in a goroutine, the test is the supervisor)
//   - status from a sibling goroutine — must report running
//   - ctx-cancel (the systemd-style "SIGTERM the process" equivalent
//     from inside the same test binary) — must drive a clean exit
//     and the post-shutdown status must report stopped
//
// The test does NOT fork a child process: lifecycle.Run is invoked
// inside this test binary so we get coverage and race-detector signal
// without paying the cost of os/exec.
//
// Note we deliberately do NOT invoke `mux stop` here: stop sends
// SIGTERM to the daemon PID, which in an in-process test is the
// test runner itself — lifecycle.Stop's grace-then-SIGKILL escalation
// would terminate the test binary. The cobra `stop` surface is
// exercised separately in TestStopCommand_SignalsSleepSentinel
// against a sentinel sleep child that does have a PID distinct from
// the test process.
func TestStart_ForegroundLifecycle(t *testing.T) {
	_, pidPath, sockPath, snapPath := stateRoot(t)

	startCtx, startCancel := context.WithCancel(context.Background())
	defer startCancel()
	startErrCh := make(chan error, 1)
	go func() {
		_, _, err := runRoot(t, startCtx, "mux", "start", "--foreground",
			"--pid-file", pidPath,
			"--socket", sockPath,
			"--snapshot", snapPath,
			"--snapshot-interval", "50ms",
		)
		startErrCh <- err
	}()

	// Poll for the daemon to be ready — PID file present + socket
	// file present. The test budget is generous (2s) to absorb the
	// CI scheduler's noise.
	deadline := time.Now().Add(2 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		_, sockErr := os.Stat(sockPath)
		_, pidErr := os.Stat(pidPath)
		if sockErr == nil && pidErr == nil {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("daemon did not become ready (pid=%q sock=%q)", pidPath, sockPath)
	}

	// status — must report running.
	out, _, err := runRoot(t, nil, "mux", "status",
		"--pid-file", pidPath, "--socket", sockPath)
	if got := exitCode(err); got != 0 {
		t.Errorf("status exit = %d, want 0 (running); err=%v", got, err)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("status stdout = %q, want to contain 'running'", out)
	}
	if !strings.Contains(out, strconv.Itoa(os.Getpid())) {
		t.Errorf("status stdout = %q, want to contain our pid %d", out, os.Getpid())
	}

	// Trigger graceful shutdown by cancelling the start context.
	// lifecycle.Run watches its parent ctx; cancellation drives the
	// same teardown path SIGTERM does, without the
	// terminate-the-test-process side effect.
	startCancel()
	select {
	case err := <-startErrCh:
		if got := exitCode(err); got != 0 {
			t.Errorf("start exit = %d, want 0; err=%v", got, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start did not return within 5s of ctx cancel")
	}

	// status again — must report stopped (the daemon removed its
	// own PID file on shutdown).
	finalOut, _, err := runRoot(t, nil, "mux", "status", "--pid-file", pidPath)
	if got := exitCode(err); got != 1 {
		t.Errorf("final status exit = %d, want 1 (stopped); err=%v", got, err)
	}
	if !strings.Contains(finalOut, "stopped") {
		t.Errorf("final status stdout = %q, want to contain 'stopped'", finalOut)
	}
}

// TestStopCommand_SignalsSleepSentinel exercises the `prismd mux stop`
// cobra path against a sentinel `sleep` child whose PID is recorded in
// a synthetic PID file. The lifecycle package's own tests cover the
// SIGTERM-then-SIGKILL escalation in detail; here we only need to
// confirm the cobra wiring resolves the right PID file path, invokes
// lifecycle.Stop, and surfaces the result with the expected output
// string and exit code.
func TestStopCommand_SignalsSleepSentinel(t *testing.T) {
	_, pidPath, _, _ := stateRoot(t)

	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}
	cmd := exec.Command(sleepBin, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	doneCh := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(doneCh)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-doneCh:
		case <-time.After(2 * time.Second):
		}
	})

	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}

	out, _, err := runRoot(t, nil, "mux", "stop", "--pid-file", pidPath, "--grace", "2s")
	if got := exitCode(err); got != 0 {
		t.Errorf("stop exit = %d, want 0; err=%v", got, err)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("stop stdout = %q, want to contain 'stopped'", out)
	}

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("sleep sentinel did not exit within 3s of stop")
	}

	// PID file should be gone after stop.
	if _, err := os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pid file %s still present after stop", pidPath)
	}
}

// TestStart_ForegroundRefusesWhenRunning exercises the
// ErrAlreadyRunning path: the first foreground daemon is up; a second
// start in the same test must exit with code 2 ("conflict — already
// running") rather than fall through into Run.
func TestStart_ForegroundRefusesWhenRunning(t *testing.T) {
	_, pidPath, sockPath, snapPath := stateRoot(t)

	cfg := lifecycle.Config{
		PIDPath:          pidPath,
		SocketPath:       sockPath,
		SnapshotPath:     snapPath,
		SnapshotInterval: 50 * time.Millisecond,
	}
	// Boot a daemon directly via lifecycle.Run so we control its
	// lifetime independently of the cobra surface.
	daemonCtx, daemonCancel := context.WithCancel(context.Background())
	defer daemonCancel()
	daemonErr := make(chan error, 1)
	go func() {
		daemonErr <- lifecycle.Run(daemonCtx, cfg)
	}()
	// Wait for the daemon to be ready.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Second start --foreground — must refuse with exit code 2.
	_, _, err := runRoot(t, context.Background(), "mux", "start", "--foreground",
		"--pid-file", pidPath, "--socket", sockPath, "--snapshot", snapPath,
	)
	if got := exitCode(err); got != 2 {
		t.Errorf("second start exit = %d, want 2; err=%v", got, err)
	}
	if msg := fmt.Sprint(err); !strings.Contains(msg, "already running") {
		t.Errorf("second start err = %q, want to contain 'already running'", msg)
	}

	// Clean teardown.
	daemonCancel()
	if err := <-daemonErr; err != nil {
		t.Errorf("daemon Run: %v", err)
	}
}

// TestStop_NoPIDFileIsNoop confirms the cobra surface preserves the
// lifecycle package's "stop on absent daemon is success" semantics.
func TestStop_NoPIDFileIsNoop(t *testing.T) {
	_, pidPath, _, _ := stateRoot(t)
	out, _, err := runRoot(t, nil, "mux", "stop", "--pid-file", pidPath)
	if got := exitCode(err); got != 0 {
		t.Errorf("exit = %d, want 0; err=%v", got, err)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("stdout = %q, want to contain 'stopped'", out)
	}
}

// TestBuildDetachArgs_PassesThroughPathOverrides asserts the helper
// that constructs the re-exec argv preserves every path override the
// user supplied to `start`. This is load-bearing: the systemd unit
// will set --pid-file / --socket explicitly, and a regression that
// drops them would silently start the daemon under the default
// paths.
func TestBuildDetachArgs_PassesThroughPathOverrides(t *testing.T) {
	cfg := lifecycle.Config{
		PIDPath:          "/x/pid",
		SocketPath:       "/x/sock",
		SnapshotPath:     "/x/snap",
		SnapshotInterval: 12 * time.Second,
	}
	args := buildDetachArgs(cfg)
	wantFlags := map[string]string{
		"--pid-file":          "/x/pid",
		"--socket":            "/x/sock",
		"--snapshot":          "/x/snap",
		"--snapshot-interval": "12s",
	}
	for flag, value := range wantFlags {
		found := false
		for i, a := range args {
			if a == flag {
				if i+1 >= len(args) {
					t.Errorf("flag %s has no value in args %v", flag, args)
					continue
				}
				if args[i+1] != value {
					t.Errorf("flag %s = %q, want %q", flag, args[i+1], value)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("flag %s missing from args %v", flag, args)
		}
	}
	if args[0] != "mux" || args[1] != "start" || args[2] != "--foreground" {
		t.Errorf("argv prefix = %v, want [mux start --foreground ...]", args[:3])
	}
}
