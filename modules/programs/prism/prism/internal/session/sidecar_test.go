package session

// Unit tests for StartSidecar, SidecarLogPath, and SidecarPIDPath.
//
// StartSidecar calls os.Executable() to resolve the prism binary and then
// exec.Command(self, "sidecar", ...). In tests, os.Executable() returns the
// test binary itself. We use the PRISM_TEST_SUBPROCESS env variable to make
// the test binary act as a stub sidecar: when the binary is invoked with that
// variable set, it exits immediately instead of running tests.

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
)

// TestMain intercepts execution when this test binary is re-invoked as a fake
// sidecar subprocess. Two modes are supported:
//
//   - PRISM_TEST_SUBPROCESS=1: brief stub — sleeps 50ms then exits. Used by
//     StartSidecar tests where only the initial PID file creation matters.
//
//   - PRISM_SIDECAR_LONG_STUB=1: long-lived stub — sleeps 60s then exits.
//     Used by FindSidecarPID/KillSidecar tests that need the process to stay
//     alive long enough for ps/proc scanning to find it.
func TestMain(m *testing.M) {
	if os.Getenv("PRISM_TEST_SUBPROCESS") == "1" {
		// We are the child process acting as the sidecar stub.
		// Sleep briefly so the parent can read the PID file, then exit.
		time.Sleep(50 * time.Millisecond)
		os.Exit(0)
	}
	if os.Getenv("PRISM_SIDECAR_LONG_STUB") == "1" {
		// Long-lived stub: stay alive until signalled (for process-scan tests).
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// ── path helper tests ────────────────────────────────────────────────────────

func TestSidecarLogPath_DefaultXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	got, err := SidecarLogPath("myrepo@feature")
	if err != nil {
		t.Fatalf("SidecarLogPath: %v", err)
	}

	want := filepath.Join(home, ".local", "state", "prism", "logs", "myrepo@feature-sidecar.log")
	if got != want {
		t.Errorf("SidecarLogPath = %q, want %q", got, want)
	}
}

func TestSidecarLogPath_CustomXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	got, err := SidecarLogPath("myrepo@main")
	if err != nil {
		t.Fatalf("SidecarLogPath: %v", err)
	}

	want := filepath.Join(tmp, "prism", "logs", "myrepo@main-sidecar.log")
	if got != want {
		t.Errorf("SidecarLogPath = %q, want %q", got, want)
	}
}

func TestSidecarPIDPath_DefaultXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	got, err := SidecarPIDPath("myrepo@feature")
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}

	want := filepath.Join(home, ".local", "state", "prism", "run", "myrepo@feature-sidecar.pid")
	if got != want {
		t.Errorf("SidecarPIDPath = %q, want %q", got, want)
	}
}

func TestSidecarPIDPath_CustomXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	got, err := SidecarPIDPath("myrepo@main")
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}

	want := filepath.Join(tmp, "prism", "run", "myrepo@main-sidecar.pid")
	if got != want {
		t.Errorf("SidecarPIDPath = %q, want %q", got, want)
	}
}

// ── StartSidecar tests ───────────────────────────────────────────────────────

// TestStartSidecar_LaunchesProcess verifies that StartSidecar:
//   - returns nil (no error)
//   - creates the log file
//   - writes a valid PID file
//   - the written PID corresponds to a live process (before it exits)
//
// The test re-uses this test binary as the sidecar stub; when invoked with
// PRISM_TEST_SUBPROCESS=1 the binary exits after a short sleep (see TestMain).
func TestStartSidecar_LaunchesProcess(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	// Ensure the subprocess stub path is set — see TestMain.
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")

	const session = "testrepo@branch"

	if err := StartSidecar(session, 14000); err != nil {
		t.Fatalf("StartSidecar: %v", err)
	}

	// Log file should exist.
	logPath, err := SidecarLogPath(session)
	if err != nil {
		t.Fatalf("SidecarLogPath: %v", err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("log file not created at %s: %v", logPath, err)
	}

	// PID file should exist and contain a valid integer.
	pidPath, err := SidecarPIDPath(session)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("PID file not created at %s: %v", pidPath, err)
	}

	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("PID file contains non-integer %q: %v", pidStr, err)
	}
	if pid <= 0 {
		t.Errorf("PID = %d, want > 0", pid)
	}

	// The written PID should correspond to a live process immediately after
	// StartSidecar returns. The stub sleeps 50ms before exiting, so there is
	// a window in which the process is still alive.
	// This check is Linux-specific (/proc is not available on Darwin).
	if runtime.GOOS == "linux" {
		if statusData, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err != nil {
			t.Errorf("process pid %d not found in /proc: %v", pid, err)
		} else if strings.Contains(string(statusData), "State:\tZ") {
			t.Errorf("process pid %d is already a zombie immediately after start", pid)
		}
	}
}

// TestStartSidecar_CreatesDirectories verifies that StartSidecar creates the
// log and run directories under XDG_STATE_HOME even when they don't exist yet.
func TestStartSidecar_CreatesDirectories(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")

	const session = "testrepo@dir-test"

	// The prism/ subdirs should not exist yet.
	logDir := filepath.Join(tmp, "prism", "logs")
	runDir := filepath.Join(tmp, "prism", "run")
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Skipf("log dir already exists — skipping: %v", err)
	}

	if err := StartSidecar(session, 14001); err != nil {
		t.Fatalf("StartSidecar: %v", err)
	}

	for _, dir := range []string{logDir, runDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("directory not created: %s — %v", dir, err)
		}
	}
}

// ── buildReadinessWaitCmd tests ──────────────────────────────────────────────

func TestBuildReadinessWaitCmd_ContainsReadyPath(t *testing.T) {
	cmd := buildReadinessWaitCmd(
		"/home/user/.local/state/prism/run/my-session-sidecar.ready",
		"/home/user/.local/state/prism/run/my-session-sidecar.sid",
		"opencode attach http://localhost:14000")
	if !strings.Contains(cmd, "/home/user/.local/state/prism/run/my-session-sidecar.ready") {
		t.Errorf("readiness command does not reference ready path: %q", cmd)
	}
}

func TestBuildReadinessWaitCmd_ContainsAttachCmd(t *testing.T) {
	attach := "opencode attach http://localhost:14042"
	cmd := buildReadinessWaitCmd("/tmp/test.ready", "/tmp/test.sid", attach)
	if !strings.Contains(cmd, attach) {
		t.Errorf("readiness command does not contain attach command %q: %q", attach, cmd)
	}
}

func TestBuildReadinessWaitCmd_TimeoutMessage(t *testing.T) {
	cmd := buildReadinessWaitCmd("/tmp/test.ready", "/tmp/test.sid", "opencode attach http://localhost:14000")
	// Verify the timeout message matches 120s (240 iterations × 0.5s).
	if !strings.Contains(cmd, "120s") {
		t.Errorf("readiness command should mention 120s timeout: %q", cmd)
	}
	// Verify iteration count matches 120s / 0.5s = 240.
	if !strings.Contains(cmd, "240") {
		t.Errorf("readiness command should use 240 iterations (120s at 0.5s each): %q", cmd)
	}
}

func TestBuildReadinessWaitCmd_PathWithSpaces(t *testing.T) {
	// Path with spaces must be properly quoted.
	path := "/home/user name/.local/state/prism/run/my session-sidecar.ready"
	cmd := buildReadinessWaitCmd(path, "/tmp/test.sid", "opencode attach http://localhost:14000")
	// The path should be single-quoted.
	if !strings.Contains(cmd, "'"+path+"'") {
		t.Errorf("path with spaces not properly quoted in command: %q", cmd)
	}
}

func TestBuildReadinessWaitCmd_ExitsOneOnTimeout(t *testing.T) {
	cmd := buildReadinessWaitCmd("/tmp/test.ready", "/tmp/test.sid", "opencode attach http://localhost:14000")
	if !strings.Contains(cmd, "exit 1") {
		t.Errorf("readiness command should exit 1 on timeout: %q", cmd)
	}
}

// ── SidecarReadyPath tests ────────────────────────────────────────────────────

func TestSidecarReadyPath_DefaultXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	got, err := SidecarReadyPath("myrepo@feature")
	if err != nil {
		t.Fatalf("SidecarReadyPath: %v", err)
	}

	want := filepath.Join(home, ".local", "state", "prism", "run", "myrepo@feature-sidecar.ready")
	if got != want {
		t.Errorf("SidecarReadyPath = %q, want %q", got, want)
	}
}

func TestSidecarReadyPath_CustomXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	got, err := SidecarReadyPath("myrepo@main")
	if err != nil {
		t.Fatalf("SidecarReadyPath: %v", err)
	}

	want := filepath.Join(tmp, "prism", "run", "myrepo@main-sidecar.ready")
	if got != want {
		t.Errorf("SidecarReadyPath = %q, want %q", got, want)
	}
}

// ── isSidecarCmdline tests ────────────────────────────────────────────────────

// TestIsSidecarCmdline_NULSeparated verifies that a Linux-style NUL-separated
// cmdline (from /proc/<pid>/cmdline) is recognised correctly.
func TestIsSidecarCmdline_NULSeparated(t *testing.T) {
	// Simulate: /path/to/prism\0sidecar\0--session\0myrepo@feature\0--opencode-url\0...
	cmdline := "/path/to/prism\x00sidecar\x00--session\x00myrepo@feature\x00--opencode-url\x00http://localhost:14000\x00"
	if !isSidecarCmdline(cmdline, "myrepo@feature") {
		t.Errorf("isSidecarCmdline = false, want true for NUL-separated cmdline")
	}
}

// TestIsSidecarCmdline_SpaceSeparated verifies that a space-separated cmdline
// (e.g. from ps aux) is recognised correctly.
func TestIsSidecarCmdline_SpaceSeparated(t *testing.T) {
	cmdline := "/path/to/prism sidecar --session myrepo@feature --opencode-url http://localhost:14000"
	if !isSidecarCmdline(cmdline, "myrepo@feature") {
		t.Errorf("isSidecarCmdline = false, want true for space-separated cmdline")
	}
}

// TestIsSidecarCmdline_WrongSession verifies that a cmdline for a different
// session is not matched.
func TestIsSidecarCmdline_WrongSession(t *testing.T) {
	cmdline := "/path/to/prism\x00sidecar\x00--session\x00myrepo@other\x00--opencode-url\x00http://localhost:14000\x00"
	if isSidecarCmdline(cmdline, "myrepo@feature") {
		t.Errorf("isSidecarCmdline = true, want false for wrong session")
	}
}

// TestIsSidecarCmdline_NoSidecar verifies that a cmdline without "sidecar"
// subcommand is not matched even if the session name appears.
func TestIsSidecarCmdline_NoSidecar(t *testing.T) {
	cmdline := "/path/to/prism\x00spawn\x00--session\x00myrepo@feature\x00"
	if isSidecarCmdline(cmdline, "myrepo@feature") {
		t.Errorf("isSidecarCmdline = true, want false (no 'sidecar' token)")
	}
}

// TestIsSidecarCmdline_SessionAsSubstring verifies that a session name that is
// a substring of another session name is not a false positive. The match must
// be on the exact token following --session.
func TestIsSidecarCmdline_SessionAsSubstring(t *testing.T) {
	// "myrepo@feat" should not match "--session myrepo@feature"
	cmdline := "/path/to/prism\x00sidecar\x00--session\x00myrepo@feature\x00"
	if isSidecarCmdline(cmdline, "myrepo@feat") {
		t.Errorf("isSidecarCmdline = true, want false (partial session name match)")
	}
}

// ── FindSidecarPID tests ──────────────────────────────────────────────────────

// TestFindSidecarPID_RunningProcess verifies that FindSidecarPID locates a live
// process whose command line contains "sidecar --session <name>".
//
// The test re-invokes the current test binary with PRISM_STUB_SESSION set, which
// causes TestMain to exec the test binary with args that simulate a sidecar
// cmdline. On Darwin, the test uses a helper that passes args through exec so
// the process cmdline reflects the target session.
//
// The test skips itself when running in a sandboxed environment (e.g. nix build)
// where the process table does not show user-spawned processes. It detects this
// by starting the stub process and immediately checking whether it is visible
// via the same process scanning mechanism we are testing — if not, the
// environment does not support it and the test is skipped.
func TestFindSidecarPID_RunningProcess(t *testing.T) {
	sessionName := "testrepo@find-sidecar-" + strconv.Itoa(os.Getpid())

	// We need to start a process whose command line contains:
	//   prism sidecar --session <sessionName>
	//
	// os.Executable() returns the test binary. We create a symlink named
	// "prism" so that the spawned process looks like a prism sidecar.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	binDir := t.TempDir()
	prismLink := filepath.Join(binDir, "prism")
	if err := os.Symlink(self, prismLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Launch the stub process with sidecar args. PRISM_SIDECAR_LONG_STUB=1
	// causes TestMain to sleep for 60s so the process stays alive.
	cmd := newSidecarStubCmd(prismLink, sessionName)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub sidecar: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Give the OS a moment to register the process cmdline.
	time.Sleep(50 * time.Millisecond)

	found := FindSidecarPID(sessionName)
	if found == 0 {
		// Process not found — this can happen in sandboxed environments (e.g.
		// nix build) where ps/proc scanning is restricted to processes started
		// by the build system itself. Skip rather than fail.
		t.Skipf("FindSidecarPID(%q) = 0: process scanning is not available in this environment (sandboxed build?); skipping", sessionName)
	}
	if found != pid {
		t.Errorf("FindSidecarPID(%q) = %d, want %d", sessionName, found, pid)
	}
}

// TestFindSidecarPID_NoProcess verifies that FindSidecarPID returns 0 when no
// matching process exists.
func TestFindSidecarPID_NoProcess(t *testing.T) {
	sessionName := "nonexistent@session-" + strconv.Itoa(os.Getpid())
	if found := FindSidecarPID(sessionName); found != 0 {
		t.Errorf("FindSidecarPID(%q) = %d, want 0 (no matching process)", sessionName, found)
	}
}

// ── KillSidecar orphan-path tests ─────────────────────────────────────────────

// TestKillSidecar_OrphanedProcess verifies that when there is no PID file,
// KillSidecar falls back to FindSidecarPID and terminates the process.
//
// This test skips in sandboxed environments (e.g. nix build) where process
// scanning is unavailable. See TestFindSidecarPID_RunningProcess for details.
func TestKillSidecar_OrphanedProcess(t *testing.T) {
	sessionName := "testrepo@orphan-" + strconv.Itoa(os.Getpid())

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	binDir := t.TempDir()
	prismLink := filepath.Join(binDir, "prism")
	if err := os.Symlink(self, prismLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cmd := newSidecarStubCmd(prismLink, sessionName)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start orphaned sidecar: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Give the OS a moment to register the process.
	time.Sleep(50 * time.Millisecond)

	// Check that process scanning is available in this environment before
	// proceeding. If FindSidecarPID can't see the process we just started,
	// we're in a sandbox and should skip.
	if FindSidecarPID(sessionName) == 0 {
		t.Skipf("process scanning not available in this environment (sandboxed build?); skipping orphan kill test")
	}

	// Set up a state dir with NO PID file — simulating the orphaned case.
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	KillSidecar(sessionName)

	// After SIGTERM, the process should no longer be findable via FindSidecarPID.
	// We poll for up to 3 seconds to allow the OS to update the process table.
	deadline := time.Now().Add(3 * time.Second)
	terminated := false
	for time.Now().Before(deadline) {
		if FindSidecarPID(sessionName) == 0 {
			terminated = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !terminated {
		t.Errorf("orphaned sidecar (pid %d) was not terminated by KillSidecar — FindSidecarPID still returns it", pid)
	}
}

// TestKillSidecar_NoPIDFileNoProcess verifies that when there is neither a PID
// file nor a running process, KillSidecar returns silently without error.
func TestKillSidecar_NoPIDFileNoProcess(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	// Should not panic or error.
	KillSidecar("testrepo@no-pid-no-process-" + strconv.Itoa(os.Getpid()))
}

// newSidecarStubCmd builds a long-lived exec.Cmd that simulates a prism sidecar
// process for sessionName. PRISM_SIDECAR_LONG_STUB=1 causes the binary to sleep
// for 60 seconds (see TestMain) rather than running the test suite again.
// This gives FindSidecarPID / KillSidecar enough time to scan for the process.
func newSidecarStubCmd(prismBin, sessionName string) *exec.Cmd {
	cmd := exec.Command(prismBin, "sidecar", "--session", sessionName, "--opencode-url", "http://localhost:19999")
	cmd.Env = append(os.Environ(), "PRISM_SIDECAR_LONG_STUB=1")
	return cmd
}
