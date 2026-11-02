package session

// stale_zombie_test.go — regression test for the stale-zombie socket hazard.
//
// The hazard: when ensureAndSwitch encounters a tmux session whose last_seen
// is ≥ 60s (stale or zombie), killing only the tmux session leaves the sidecar
// process alive. The sidecar holds open its host-API Unix socket; when the new
// sidecar starts, checkNoLiveSidecar dials the socket, finds it live, and
// refuses to start — returning duplicateStartError.
//
// So ensureAndSwitch stops the old sidecar (EnsureSidecarStopped) before
// session.Create, so that by the time the new sidecar attempts its
// duplicate-start probe, the old socket is gone.
//
// These tests verify the key invariant for EnsureSidecarStopped:
//
//   After the stop reports the sidecar gone, a dial to the socket that the
//   killed process was listening on either fails with ECONNREFUSED (tombstone)
//   or returns an error — never succeeds. A successful dial is what triggers
//   the duplicateStartError in the real sidecar; its absence means the new
//   sidecar's duplicate-start guard will pass.
//
// We simulate the old sidecar by:
//  1. Spawning a subprocess that binds a Unix socket and sleeps (PRISM_TEST_STUB_WITH_SOCKET=<path>).
//  2. Writing its PID file.
//  3. Calling the stop.
//  4. Asserting that the socket is no longer accepting connections.

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestMain in sidecar_test.go already handles the PRISM_TEST_SUBPROCESS and
// PRISM_TEST_STUB_LONG stubs. This file's stub (PRISM_TEST_STUB_WITH_SOCKET)
// is also intercepted there. The TestMain entry point is shared across all
// _test.go files in the package.

// init registers the PRISM_TEST_STUB_WITH_SOCKET handler at package init so
// it runs before TestMain calls m.Run(). We cannot add another TestMain to this
// package (only one is allowed per package), so we inject via an init function
// that reads the env var early and, if set, binds the socket and sleeps.
//
// Note: init runs before TestMain's m.Run() call, so calling os.Exit here is
// safe — it prevents the test runner from executing any tests.
func init() {
	sockPath := os.Getenv("PRISM_TEST_STUB_WITH_SOCKET")
	if sockPath == "" {
		return
	}
	// We are a stub sidecar. Bind the socket and sleep until SIGTERM.
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		// If we can't bind, exit so the parent doesn't wait forever.
		os.Exit(1)
	}
	// Accept in background so the socket is genuinely responsive (not just a
	// tombstone) — this makes checkNoLiveSidecar return socketLive as it would
	// for a real sidecar.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	// Sleep until killed.
	time.Sleep(60 * time.Second)
	ln.Close()
	os.Exit(0)
}

// TestStaleZombie_EnsureSidecarStopped_SocketFreedBeforeReturn is the
// stale-zombie regression test. It runs the check on EnsureSidecarStopped,
// the stop of the stale-zombie restart path of ensureAndSwitch. The test is
// Linux-only, so its name must not start with TestEnsureSidecarStopped: the
// macOS job of pr-gate.yml selects that prefix and fails on a SKIP.
func TestStaleZombie_EnsureSidecarStopped_SocketFreedBeforeReturn(t *testing.T) {
	checkStaleZombieSocketFreed(t, "EnsureSidecarStopped", func(sessionName string) bool {
		return EnsureSidecarStopped(sessionName, 5*time.Second)
	})
}

// checkStaleZombieSocketFreed starts the socket stub as the sidecar of a
// session, calls stop for that session, and checks that the socket no
// longer accepts a dial. stop reports whether the sidecar is gone.
func checkStaleZombieSocketFreed(t *testing.T, stopName string, stop func(sessionName string) bool) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("test uses /proc/<pid>/status and PRISM_TEST_STUB_WITH_SOCKET — Linux only")
	}

	// Use a short-prefix MkdirTemp (not t.TempDir) for XDG_STATE_HOME so the
	// derived socket path stays under Linux's 108-byte sun_path limit.
	// t.TempDir() embeds the full test function name which on GitHub Actions
	// produces a path like /tmp/TestStaleZombie_EnsureSidecarStopped_…<N>/
	// that, combined with the per-session hash subdir and "hostapi.sock",
	// exceeds 108 bytes and causes bind(2) to fail with EINVAL.
	tmp, err := os.MkdirTemp("", "pst-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("XDG_STATE_HOME", tmp)

	const sessionName = "prism-test@stale-zombie-regression"

	// Derive the socket path exactly as the sidecar would.
	sockDir := filepath.Join(tmp, "prism", "run", SessionDirName(sessionName))
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	sockPath := filepath.Join(sockDir, "hostapi.sock")

	// Launch the stub with argv "sidecar --session <name>" so that
	// KillSidecar's cmdline guard accepts it.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(self, "sidecar", "--session", sessionName)
	cmd.Env = append(os.Environ(),
		"PRISM_TEST_STUB_WITH_SOCKET="+sockPath,
		// Disable the normal test stubs so this process becomes our socket stub.
		"PRISM_TEST_SUBPROCESS=",
		"PRISM_TEST_STUB_LONG=",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start socket stub: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Wait for the stub to bind the socket (up to 3 s).
	sockDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(sockDeadline) {
		conn, dialErr := net.DialTimeout("unix", sockPath, 100*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			break // Socket is live.
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Assert the socket is live before we do anything (positive proof the stub works).
	conn, dialErr := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if dialErr != nil {
		t.Fatalf("socket stub is not accepting before kill: %v — stub may not have bound in time", dialErr)
	}
	conn.Close()

	// Write the PID file so the stop can find the process.
	runDir := filepath.Join(tmp, "prism", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	pidPath := filepath.Join(runDir, sessionName+"-sidecar.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	if !stop(sessionName) {
		t.Errorf("%s: the sidecar is not gone", stopName)
	}

	// After the stop returns, the socket must not be accepting.
	// A successful dial here would be the exact condition that triggers
	// duplicateStartError in the new sidecar's checkNoLiveSidecar probe.
	_, dialAfterErr := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if dialAfterErr == nil {
		t.Errorf("socket is still accepting after %s returned — "+
			"a new sidecar's duplicate-start guard would fire; the stale-zombie bug is not fixed", stopName)
	}
	// A nil dialAfterErr is the regression. Any error (ECONNREFUSED, ENOENT,
	// etc.) is acceptable — it means the socket is no longer live.
}
