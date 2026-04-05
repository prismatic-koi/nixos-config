package cmd

// Unit tests for session.KillSidecar.
//
// KillSidecar was moved from the cmd package (where it was unexported as
// killSidecar) to the session package so that both cmd and integration tests
// can call it for test teardown.  These tests live in the cmd package because
// they re-use the TestMain stub mechanism already present here.
//
// Paths exercised:
//   - Normal operation: PID file exists, process running, SIGTERM delivered,
//     PID file removed.
//   - Stale PID file: process no longer exists (ESRCH), PID file still removed.
//   - Missing PID file: function returns silently without error.
//   - Corrupt PID file: non-integer content, file removed.
//   - PID recycled to unrelated process: /proc/<pid>/cmdline doesn't contain
//     "prism", kill is skipped, PID file removed.
//
// TestKillSidecar_NormalOperation re-invokes the test binary itself as a stub
// sidecar subprocess. When PRISM_CMD_TEST_STUB=1 the binary sleeps briefly so
// its /proc/<pid>/cmdline contains the test binary path (which includes "prism"
// because the test binary is named after the package). See TestMain below.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// TestMain intercepts re-invocations of the test binary used as a stub sidecar.
// When PRISM_CMD_TEST_STUB=1 the binary sleeps for 60 seconds (simulating a
// long-running sidecar), then exits. The sleep is interruptible by SIGTERM,
// which is exactly what KillSidecar sends.
func TestMain(m *testing.M) {
	if os.Getenv("PRISM_CMD_TEST_STUB") == "1" {
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// writePIDFile writes a PID file under stateDir/prism/run/ and returns the path.
// stateDir must already be set as XDG_STATE_HOME by the caller.
func writePIDFile(t *testing.T, stateDir, sessionName string, pid int) string {
	t.Helper()
	runDir := filepath.Join(stateDir, "prism", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	pidPath := filepath.Join(runDir, sessionName+"-sidecar.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}
	return pidPath
}

// processExists returns true if the given PID corresponds to a running (non-zombie)
// process on Linux. Zombies are considered "not running" since they have already
// terminated and are just waiting to be reaped.
// On non-Linux platforms, this always returns true (skips the assertion).
func processExists(pid int) bool {
	if runtime.GOOS != "linux" {
		return true
	}
	// A zombie process still has a /proc entry but its state is "Z".
	// We consider it "not running" — the process body has exited.
	statusData, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		// No /proc entry → definitely not running.
		return false
	}
	// Check for zombie state.
	if strings.Contains(string(statusData), "State:\tZ") {
		return false
	}
	return true
}

// startStubProcess re-invokes the current test binary with PRISM_CMD_TEST_STUB=1
// so it acts as a long-running sidecar stub whose cmdline contains the word
// "prism" (the test binary is typically named cmd.test or similar, but the
// full path goes through os.Executable which may contain the project name).
//
// To guarantee the cmdline check passes, the stub binary is invoked via a
// symlink named "prism-stub" placed in a temp dir and added to PATH. Since the
// test binary is already a real binary with a path that may or may not contain
// "prism", we instead copy/symlink to a path that explicitly contains "prism".
func startStubProcess(t *testing.T) (pid int, cleanup func()) {
	t.Helper()

	// Resolve the current test binary.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	// Create a symlink named "prism" → current test binary in a temp dir.
	// This ensures /proc/<pid>/cmdline will contain the path "…/prism".
	binDir := t.TempDir()
	prismLink := filepath.Join(binDir, "prism")
	if err := os.Symlink(self, prismLink); err != nil {
		t.Fatalf("symlink prism → test binary: %v", err)
	}

	cmd := exec.Command(prismLink)
	cmd.Env = append(os.Environ(), "PRISM_CMD_TEST_STUB=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub process: %v", err)
	}

	return cmd.Process.Pid, func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// TestKillSidecar_NormalOperation verifies that KillSidecar sends SIGTERM to a
// running process and removes the PID file.
func TestKillSidecar_NormalOperation(t *testing.T) {
	pid, cleanupProc := startStubProcess(t)
	t.Cleanup(cleanupProc)

	// Verify cmdline contains "prism" before we proceed (skip if not).
	time.Sleep(30 * time.Millisecond) // Let the process start.
	cmdlineData, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Skipf("could not read /proc/%d/cmdline: %v (process may have exited already)", pid, err)
	}
	if !strings.Contains(string(cmdlineData), "prism") {
		t.Fatalf("stub process cmdline does not contain 'prism': %q — startStubProcess setup is broken", string(cmdlineData))
	}

	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	const session = "testrepo@kill-normal"
	pidPath := writePIDFile(t, stateDir, session, pid)

	prismSession.KillSidecar(session)

	// PID file should be gone.
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("PID file still exists after KillSidecar: %v", err)
	}

	// Wait for the process to actually terminate (it received SIGTERM).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(pid) {
		t.Errorf("process pid %d still exists after KillSidecar", pid)
	}
}

// TestKillSidecar_StalePID verifies that KillSidecar handles a PID that no
// longer exists (ESRCH) gracefully — it removes the stale PID file and does
// not panic.
func TestKillSidecar_StalePID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	// Scan for a non-existent PID in a high range.
	stalePID := 0
	for candidate := 2000000; candidate < 2001000; candidate++ {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", candidate)); os.IsNotExist(err) {
			stalePID = candidate
			break
		}
	}
	if stalePID == 0 {
		t.Skip("could not find a non-existent PID in range 2000000-2001000")
	}

	const session = "testrepo@kill-stale"
	pidPath := writePIDFile(t, tmp, session, stalePID)

	prismSession.KillSidecar(session)

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("stale PID file still exists after KillSidecar: %v", err)
	}
}

// TestKillSidecar_MissingPIDFile verifies that KillSidecar returns silently
// when no PID file is present.
func TestKillSidecar_MissingPIDFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	// No PID file written — just call KillSidecar and ensure no panic.
	prismSession.KillSidecar("testrepo@no-pid-file")
}

// TestKillSidecar_CorruptPIDFile verifies that KillSidecar removes a corrupt
// (non-integer) PID file without panicking.
func TestKillSidecar_CorruptPIDFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const session = "testrepo@kill-corrupt"
	runDir := filepath.Join(tmp, "prism", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	pidPath := filepath.Join(runDir, session+"-sidecar.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatalf("write corrupt PID file: %v", err)
	}

	prismSession.KillSidecar(session)

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("corrupt PID file still exists after KillSidecar: %v", err)
	}
}

// TestKillSidecar_PIDRecycledToUnrelatedProcess verifies that when
// /proc/<pid>/cmdline does not contain "prism", KillSidecar skips the kill
// but still removes the stale PID file.
func TestKillSidecar_PIDRecycledToUnrelatedProcess(t *testing.T) {
	// Use the absolute path to sleep (not a symlink) so its cmdline won't
	// contain "prism".
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found in PATH")
	}

	cmd := exec.Command(sleepBin, "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep process: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const session = "testrepo@kill-recycled"
	pidPath := writePIDFile(t, tmp, session, pid)

	// Verify the process cmdline does NOT contain "prism".
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Skipf("could not read /proc/%d/cmdline: %v", pid, err)
	}
	if strings.Contains(string(cmdline), "prism") {
		t.Skipf("cmdline unexpectedly contains 'prism': %q", string(cmdline))
	}

	prismSession.KillSidecar(session)

	// PID file should be removed.
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("PID file still exists after KillSidecar (recycled PID): %v", err)
	}

	// The unrelated process should still be alive.
	if !processExists(pid) {
		t.Errorf("unrelated process (pid %d) was killed — KillSidecar should have skipped it", pid)
	}
}
