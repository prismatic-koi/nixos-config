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
	"syscall"
	"testing"
	"time"
)

// TestMain intercepts execution when this test binary is re-invoked as a fake
// sidecar subprocess (via PRISM_TEST_SUBPROCESS=1). In that case we just exit
// successfully so that StartSidecar's cmd.Start() succeeds.
//
// When PRISM_TEST_STUB_LONG=1 the binary acts as a long-running stub that
// sleeps for 60 seconds, interruptible by SIGTERM. This is used by tests that
// exercise the KillSidecarAndWait wait path.
func TestMain(m *testing.M) {
	if os.Getenv("PRISM_TEST_SUBPROCESS") == "1" {
		// We are the child process acting as the sidecar stub.
		// Sleep briefly so the parent can read the PID file, then exit.
		time.Sleep(50 * time.Millisecond)
		os.Exit(0)
	}
	if os.Getenv("PRISM_TEST_STUB_LONG") == "1" {
		// Long-running stub: sleep 60s, interruptible by SIGTERM.
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
		"agent-launcher --attach prism-repo-main")
	if !strings.Contains(cmd, "/home/user/.local/state/prism/run/my-session-sidecar.ready") {
		t.Errorf("readiness command does not reference ready path: %q", cmd)
	}
}

func TestBuildReadinessWaitCmd_ContainsAttachCmd(t *testing.T) {
	attach := "agent-launcher --attach prism-nixos-config-feature"
	cmd := buildReadinessWaitCmd("/tmp/test.ready", attach)
	if !strings.Contains(cmd, attach) {
		t.Errorf("readiness command does not contain attach command %q: %q", attach, cmd)
	}
}

func TestBuildReadinessWaitCmd_TimeoutMessage(t *testing.T) {
	cmd := buildReadinessWaitCmd("/tmp/test.ready", "agent-launcher --attach prism-repo-main")
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
	cmd := buildReadinessWaitCmd(path, "agent-launcher --attach prism-repo-main")
	// The path should be single-quoted.
	if !strings.Contains(cmd, "'"+path+"'") {
		t.Errorf("path with spaces not properly quoted in command: %q", cmd)
	}
}

func TestBuildReadinessWaitCmd_ExitsOneOnTimeout(t *testing.T) {
	cmd := buildReadinessWaitCmd("/tmp/test.ready", "agent-launcher --attach prism-repo-main")
	if !strings.Contains(cmd, "exit 1") {
		t.Errorf("readiness command should exit 1 on timeout: %q", cmd)
	}
}

// TestBuildReadinessWaitCmd_NoSidHandoff verifies that the readiness wait
// script does not contain any .sid file read or -s flag injection — the
// wrapped command connects to the agent PTY directly (RFC #691, Phase 1a).
func TestBuildReadinessWaitCmd_NoSidHandoff(t *testing.T) {
	cmd := buildReadinessWaitCmd("/tmp/test.ready", "agent-launcher --attach prism-repo-main")
	if strings.Contains(cmd, ".sid") {
		t.Errorf("readiness command should not reference .sid file: %q", cmd)
	}
	if strings.Contains(cmd, " -s ") {
		t.Errorf("readiness command should not inject -s flag: %q", cmd)
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

// ── AgentRunLogPath tests ─────────────────────────────────────────────────────

func TestAgentRunLogPath_DefaultXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	const sess = "myrepo@feature"
	got, err := AgentRunLogPath(sess)
	if err != nil {
		t.Fatalf("AgentRunLogPath: %v", err)
	}

	// Per-session subdirectory format (#1050): run/<12-hex-of-sha256(session)>/agent-run.log
	want := filepath.Join(home, ".local", "state", "prism", "run", SessionDirName(sess), "agent-run.log")
	if got != want {
		t.Errorf("AgentRunLogPath = %q, want %q", got, want)
	}
}

func TestAgentRunLogPath_CustomXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sess = "myrepo@main"
	got, err := AgentRunLogPath(sess)
	if err != nil {
		t.Fatalf("AgentRunLogPath: %v", err)
	}

	// Per-session subdirectory format (#1050): run/<12-hex-of-sha256(session)>/agent-run.log
	want := filepath.Join(tmp, "prism", "run", SessionDirName(sess), "agent-run.log")
	if got != want {
		t.Errorf("AgentRunLogPath = %q, want %q", got, want)
	}
}

// TestAgentRunLogPath_SameDirectoryAsHostAPIPath verifies that the agent-run log
// and the host-API socket live in the same per-session directory. This is
// important for cleanup: both are removed when the per-session dir is cleaned up.
func TestAgentRunLogPath_SameDirectoryAsHostAPIPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const session = "myrepo@feature"

	agentRunPath, err := AgentRunLogPath(session)
	if err != nil {
		t.Fatalf("AgentRunLogPath: %v", err)
	}
	hostAPIPath, err := SidecarHostAPIPath(session)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}

	if filepath.Dir(agentRunPath) != filepath.Dir(hostAPIPath) {
		t.Errorf("agent-run log dir %q != host-API dir %q — they must share the per-session directory",
			filepath.Dir(agentRunPath), filepath.Dir(hostAPIPath))
	}
}

// ── SidecarHostAPIPath tests ──────────────────────────────────────────────────

func TestSidecarHostAPIPath_DefaultXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	const sess = "myrepo@feature"
	got, err := SidecarHostAPIPath(sess)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}

	// Per-session subdirectory format (security fix #960, hashed for #1050):
	// run/<12-hex-of-sha256(session)>/hostapi.sock
	want := filepath.Join(home, ".local", "state", "prism", "run", SessionDirName(sess), "hostapi.sock")
	if got != want {
		t.Errorf("SidecarHostAPIPath = %q, want %q", got, want)
	}
}

func TestSidecarHostAPIPath_CustomXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sess = "myrepo@main"
	got, err := SidecarHostAPIPath(sess)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}

	// Per-session subdirectory format (security fix #960, hashed for #1050):
	// run/<12-hex-of-sha256(session)>/hostapi.sock
	want := filepath.Join(tmp, "prism", "run", SessionDirName(sess), "hostapi.sock")
	if got != want {
		t.Errorf("SidecarHostAPIPath = %q, want %q", got, want)
	}
}

// ── Path-length invariant tests (#1050) ──────────────────────────────────────
//
// The host-API socket path is bound by the kernel's sun_path limit:
//   - Linux:  108 bytes (sizeof(((struct sockaddr_un *)0)->sun_path))
//   - Darwin: 104 bytes
// We assert ≤ 104 bytes on every platform so the same code path works on both.
//
// These tests use a deliberately-pessimistic synthetic $HOME that matches the
// real-world layout under which #1050 was first observed
// (/home/<user>/.local/state/prism/run/...). Test-time temp dirs are too short
// to surface the bug, so we substitute the realistic prefix manually.

const sunPathBudget = 104

// realisticHostAPIPath builds the host-API socket path that a production
// system with the given $HOME would produce, without depending on test-time
// $XDG_STATE_HOME (which a test harness sets to a short path under /tmp).
func realisticHostAPIPath(home, sessionName string) string {
	return filepath.Join(home, ".local", "state", "prism", "run", SessionDirName(sessionName), "hostapi.sock")
}

// TestSidecarHostAPIPath_LengthInvariant_WorstCaseSession exercises the
// worst-case session name from the issue (#1050 AC-1) and asserts the
// resulting path fits the cross-platform budget.
func TestSidecarHostAPIPath_LengthInvariant_WorstCaseSession(t *testing.T) {
	const home = "/home/prismatic-koi"
	worstCase := "nixos-config@" + strings.Repeat("x", 80) + "~review-99-review-context"

	got := realisticHostAPIPath(home, worstCase)
	if len(got) > sunPathBudget {
		t.Errorf("worst-case socket path is %d bytes (limit %d): %q",
			len(got), sunPathBudget, got)
	}
}

// TestSidecarHostAPIPath_LengthInvariant_PlausibleShapes asserts the
// path-length invariant for the three plausible session shapes called out by
// AC-2: a short coordinator name, a long branch name, and a long branch +
// review suffix.
func TestSidecarHostAPIPath_LengthInvariant_PlausibleShapes(t *testing.T) {
	const home = "/home/prismatic-koi"
	cases := []struct {
		name    string
		session string
	}{
		{
			name:    "short coordinator",
			session: "nixos-config@main",
		},
		{
			name:    "long branch name",
			session: "nixos-config@fix-something-with-a-fairly-long-branch-name",
		},
		{
			name:    "long branch + review suffix",
			session: "nixos-config@fix-something-with-a-fairly-long-branch-name~review-1-review-security",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := realisticHostAPIPath(home, tc.session)
			if len(got) > sunPathBudget {
				t.Errorf("session %q produced %d-byte path (limit %d): %q",
					tc.session, len(got), sunPathBudget, got)
			}
		})
	}
}

// TestSessionDirName_Deterministic asserts that SessionDirName is a pure
// function of its input — bwrap.BuildArgs, container.Manager.buildRunArgs,
// the sidecar bind site and cleanup all derive the directory by calling this
// function (or SidecarHostAPIPath which calls it), so they must agree
// regardless of when they are called.
func TestSessionDirName_Deterministic(t *testing.T) {
	const session = "nixos-config@fix-mergequeue-merged-field~review-1-review-context"
	first := SessionDirName(session)
	second := SessionDirName(session)
	if first != second {
		t.Errorf("SessionDirName not deterministic: %q vs %q", first, second)
	}
	if got, want := len(first), sessionDirHashLen; got != want {
		t.Errorf("SessionDirName length = %d, want %d", got, want)
	}
}

// TestSessionDirName_StableFormula pins the exact formula (12-hex-char SHA-256
// prefix) so that any silent drift in the algorithm is caught immediately.
// internal/container has a parallel test (TestHostAPIPath_RoundTrip_*) that
// re-derives the dir name with the same formula — if either side changes
// without the other, both this test and the container-side round-trip test
// will fail loudly.
func TestSessionDirName_StableFormula(t *testing.T) {
	// First 12 hex chars of SHA-256("nixos-config@main") = "896d0e575a71".
	// Verify with: printf 'nixos-config@main' | sha256sum | cut -c1-12
	if got, want := SessionDirName("nixos-config@main"), "896d0e575a71"; got != want {
		t.Errorf("SessionDirName(\"nixos-config@main\") = %q, want %q "+
			"(formula drift: must remain first 12 hex chars of SHA-256)", got, want)
	}
}

// ── SidecarHarnessPipePath tests ─────────────────────────────────────────────

func TestSidecarHarnessPipePath_DefaultXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	const sess = "myrepo@feature"
	got, err := SidecarHarnessPipePath(sess)
	if err != nil {
		t.Fatalf("SidecarHarnessPipePath: %v", err)
	}

	// Socket path: $HOME/.local/state/prism/run/<12-hex>/pipe.sock
	want := filepath.Join(home, ".local", "state", "prism", "run", SessionDirName(sess), "pipe.sock")
	if got != want {
		t.Errorf("SidecarHarnessPipePath = %q, want %q", got, want)
	}
}

func TestSidecarHarnessPipePath_CustomXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sess = "myrepo@main"
	got, err := SidecarHarnessPipePath(sess)
	if err != nil {
		t.Fatalf("SidecarHarnessPipePath: %v", err)
	}

	want := filepath.Join(tmp, "prism", "run", SessionDirName(sess), "pipe.sock")
	if got != want {
		t.Errorf("SidecarHarnessPipePath = %q, want %q", got, want)
	}
}

// realisticHarnessPipePath builds the pipe socket path a production system
// with the given $HOME would produce, without depending on test-time XDG_STATE_HOME.
func realisticHarnessPipePath(home, sessionName string) string {
	return filepath.Join(home, ".local", "state", "prism", "run", SessionDirName(sessionName), "pipe.sock")
}

// TestSidecarHarnessPipePath_LengthInvariant_WorstCaseSession asserts that the
// harness pipe socket path fits within the cross-platform sun_path budget (104 bytes).
func TestSidecarHarnessPipePath_LengthInvariant_WorstCaseSession(t *testing.T) {
	const home = "/home/prismatic-koi"
	worstCase := "nixos-config@" + strings.Repeat("x", 80) + "~review-99-review-context"

	got := realisticHarnessPipePath(home, worstCase)
	if len(got) > sunPathBudget {
		t.Errorf("worst-case pipe socket path is %d bytes (limit %d): %q",
			len(got), sunPathBudget, got)
	}
}

// TestSidecarHarnessPipePath_LengthInvariant_PlausibleShapes asserts path-length
// invariant for common session name shapes.
func TestSidecarHarnessPipePath_LengthInvariant_PlausibleShapes(t *testing.T) {
	const home = "/home/prismatic-koi"
	cases := []struct {
		name    string
		session string
	}{
		{
			name:    "short coordinator",
			session: "nixos-config@main",
		},
		{
			name:    "long branch name",
			session: "nixos-config@fix-something-with-a-fairly-long-branch-name",
		},
		{
			name:    "long branch + review suffix",
			session: "nixos-config@fix-something-with-a-fairly-long-branch-name~review-1-review-security",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := realisticHarnessPipePath(home, tc.session)
			if len(got) > sunPathBudget {
				t.Errorf("session %q produced %d-byte pipe path (limit %d): %q",
					tc.session, len(got), sunPathBudget, got)
			}
		})
	}
}

// TestSidecarHarnessPipePath_CoLocatesWithHostAPI asserts that the harness pipe
// socket lives in the same directory as the host-API socket, so the existing
// bind-mount for that directory covers both sockets.
func TestSidecarHarnessPipePath_CoLocatesWithHostAPI(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sess = "nixos-config@main"
	hostAPIPath, err := SidecarHostAPIPath(sess)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	pipePath, err := SidecarHarnessPipePath(sess)
	if err != nil {
		t.Fatalf("SidecarHarnessPipePath: %v", err)
	}

	if filepath.Dir(hostAPIPath) != filepath.Dir(pipePath) {
		t.Errorf("host-API dir %q != pipe dir %q — bind-mount assumption violated",
			filepath.Dir(hostAPIPath), filepath.Dir(pipePath))
	}
}

// ── KillSidecarAndWait tests ────────────────────────────────────────────────────────────────────

// startLongRunningStub re-invokes the current test binary with
// PRISM_TEST_STUB_LONG=1 and a cmdline containing "sidecar --session <name>"
// so that /proc/<pid>/cmdline satisfies the KillSidecar guard. The stub sleeps
// for 60 seconds, interruptible by SIGTERM (see TestMain above).
//
// Returns the PID of the stub and a cleanup function that kills the process if
// it is still running (belt-and-suspenders fallback).
func startLongRunningStub(t *testing.T) (pid int, cleanup func()) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("KillSidecarAndWait wait path uses /proc/<pid>/status — Linux only")
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	// Argv must contain both "sidecar" and "--session" to satisfy KillSidecar's
	// cmdline guard.
	cmd := exec.Command(self, "sidecar", "--session", "stub-session-long")
	cmd.Env = append(os.Environ(), "PRISM_TEST_STUB_LONG=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start long-running stub: %v", err)
	}

	return cmd.Process.Pid, func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// TestKillSidecarAndWait_WaitsForExit verifies that KillSidecarAndWait sends
// SIGTERM to a running process, removes the PID file, and returns nil only
// after the process has exited.
func TestKillSidecarAndWait_WaitsForExit(t *testing.T) {
	pid, cleanupProc := startLongRunningStub(t)
	t.Cleanup(cleanupProc)

	// Let the stub start.
	time.Sleep(30 * time.Millisecond)

	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sessionName = "prism-test@kill-and-wait"

	// Write the PID file.
	runDir := filepath.Join(tmp, "prism", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pidPath := filepath.Join(runDir, sessionName+"-sidecar.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	err := KillSidecarAndWait(sessionName, 5*time.Second)

	// KillSidecarAndWait must return nil (process exited within budget).
	if err != nil {
		t.Errorf("KillSidecarAndWait: unexpected error: %v", err)
	}

	// PID file must be removed.
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("PID file still exists after KillSidecarAndWait")
	}

	// The process must no longer exist.
	if sidecarProcessExists(pid) {
		t.Errorf("process pid %d still running after KillSidecarAndWait returned nil", pid)
	}
}

// TestKillSidecarAndWait_TimeoutError verifies that KillSidecarAndWait returns
// a non-nil error when the process does not exit within the timeout budget.
// We simulate this by using a very short timeout (5ms) against a process that
// takes longer than that to die after SIGTERM.
//
// This test is inherently racy — if the system is fast enough the process can
// exit within 5ms. We accept this with a skip rather than adding a
// hard-to-control delay. In practice, a 5ms timeout fires before any user-
// space process handles SIGTERM and calls exit().
func TestKillSidecarAndWait_TimeoutError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("test uses /proc/<pid>/status for liveness check — Linux only")
	}

	pid, cleanupProc := startLongRunningStub(t)
	t.Cleanup(cleanupProc)

	// Give the stub a moment to start.
	time.Sleep(30 * time.Millisecond)

	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sessionName = "prism-test@kill-and-wait-timeout"

	runDir := filepath.Join(tmp, "prism", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pidPath := filepath.Join(runDir, sessionName+"-sidecar.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	// 5ms timeout — short enough that the process will not have handled
	// SIGTERM and exited yet on any realistic system.
	err := KillSidecarAndWait(sessionName, 5*time.Millisecond)

	if err == nil {
		// The process exited within 5ms; this is not an error but the test
		// cannot exercise the timeout path. Skip rather than failing, since
		// this is a racy scenario by design.
		t.Skip("process exited within 5ms timeout — cannot test timeout path on this run")
	}

	if !strings.Contains(err.Error(), "did not exit within") {
		t.Errorf("expected timeout error containing \"did not exit within\", got: %v", err)
	}
}

// TestKillSidecarAndWait_NoPIDFile verifies that KillSidecarAndWait returns nil
// immediately when no PID file exists (the stale-zombie branch still proceeds).
func TestKillSidecarAndWait_NoPIDFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	err := KillSidecarAndWait("prism-test@no-pid-wait", 2*time.Second)
	if err != nil {
		t.Errorf("KillSidecarAndWait with no PID file: unexpected error: %v", err)
	}
}

// TestKillSidecarAndWait_CorruptPIDFile verifies that KillSidecarAndWait returns nil
// and removes the corrupt file (same graceful handling as KillSidecar).
func TestKillSidecarAndWait_CorruptPIDFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sessionName = "prism-test@corrupt-pid-wait"
	runDir := filepath.Join(tmp, "prism", "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pidPath := filepath.Join(runDir, sessionName+"-sidecar.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatalf("write corrupt PID file: %v", err)
	}

	err := KillSidecarAndWait(sessionName, 2*time.Second)
	if err != nil {
		t.Errorf("KillSidecarAndWait with corrupt PID file: unexpected error: %v", err)
	}
	// KillSidecar removes the corrupt file.
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("corrupt PID file still exists after KillSidecarAndWait")
	}
}
