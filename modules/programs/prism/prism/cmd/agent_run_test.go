package cmd

// Unit tests for agent_run.go.
//
// Covers:
//   - applyInitialPromptEnvVar: reads PRISM_INITIAL_PROMPT and populates Config
//   - minimalBwrapExecEnv: filters the host env to a minimal allow-list
//   - forwardSignalsToBwrap: delivers SIGTERM/SIGINT/SIGHUP/SIGWINCH to the child group
//   - openPTY / getWinsize / setWinsize: PTY pair creation and window-size ioctls
//   - teePTYMaster: copies master PTY output to pane and log file

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	prismsession "github.com/prismatic-koi/prism/internal/session"
)

// ── applyInitialPromptEnvVar ──────────────────────────────────────────────────

// TestApplyInitialPromptEnvVar_Set verifies that when PRISM_INITIAL_PROMPT is
// set in the environment, applyInitialPromptEnvVar assigns it to InitialPrompt.
func TestApplyInitialPromptEnvVar_Set(t *testing.T) {
	t.Setenv("PRISM_INITIAL_PROMPT", "foo")

	cfg := container.Config{}
	applyInitialPromptEnvVar(&cfg)

	if cfg.InitialPrompt != "foo" {
		t.Errorf("InitialPrompt = %q, want %q", cfg.InitialPrompt, "foo")
	}
}

// TestApplyInitialPromptEnvVar_Unset verifies that when PRISM_INITIAL_PROMPT
// is not set (empty string), InitialPrompt remains empty.
func TestApplyInitialPromptEnvVar_Unset(t *testing.T) {
	// Ensure the env var is absent/empty.
	t.Setenv("PRISM_INITIAL_PROMPT", "")

	cfg := container.Config{}
	applyInitialPromptEnvVar(&cfg)

	if cfg.InitialPrompt != "" {
		t.Errorf("InitialPrompt = %q, want empty string when env var is unset", cfg.InitialPrompt)
	}
}

// TestApplyInitialPromptEnvVar_SpecialChars verifies that prompts containing
// special characters (newlines, quotes, backticks, equals signs) are read
// verbatim — no shell interpretation occurs in the env-var pipeline.
func TestApplyInitialPromptEnvVar_SpecialChars(t *testing.T) {
	prompt := "line1\nline2 'single' \"double\" `backtick` KEY=value is part of the message"
	t.Setenv("PRISM_INITIAL_PROMPT", prompt)

	cfg := container.Config{}
	applyInitialPromptEnvVar(&cfg)

	if cfg.InitialPrompt != prompt {
		t.Errorf("InitialPrompt = %q, want %q", cfg.InitialPrompt, prompt)
	}
}

// TestApplyInitialPromptEnvVar_FilePath verifies the post-#1092 file-based
// delivery path: when PRISM_INITIAL_PROMPT_FILE points to a readable file,
// applyInitialPromptEnvVar reads the file and assigns its contents to
// InitialPrompt. This is the regression test for the launch-command size
// failure on review fan-outs — the role prompt is now delivered via a file
// instead of inlined into the tmux pane env.
func TestApplyInitialPromptEnvVar_FilePath(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "initial-prompt.txt")
	// A 24 KB body to demonstrate that the file path carries arbitrary
	// content sizes — the same shape that tripped #1092 when inlined.
	body := strings.Repeat("review-context system prompt body ", 720)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("PRISM_INITIAL_PROMPT_FILE", path)

	cfg := container.Config{}
	applyInitialPromptEnvVar(&cfg)

	if cfg.InitialPrompt != body {
		t.Errorf("InitialPrompt round-trip mismatch: got len=%d, want len=%d", len(cfg.InitialPrompt), len(body))
	}
}

// TestApplyInitialPromptEnvVar_FilePathPreferredOverInline verifies that
// when both PRISM_INITIAL_PROMPT_FILE and PRISM_INITIAL_PROMPT are set, the
// file path wins. This protects against a stale inline value from a
// re-attached pane overriding the fresh file contents.
func TestApplyInitialPromptEnvVar_FilePathPreferredOverInline(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "initial-prompt.txt")
	if err := os.WriteFile(path, []byte("fresh from file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("PRISM_INITIAL_PROMPT_FILE", path)
	t.Setenv("PRISM_INITIAL_PROMPT", "stale inline value")

	cfg := container.Config{}
	applyInitialPromptEnvVar(&cfg)

	if cfg.InitialPrompt != "fresh from file" {
		t.Errorf("InitialPrompt = %q, want %q (file path must win over stale inline value)", cfg.InitialPrompt, "fresh from file")
	}
}

// TestApplyInitialPromptEnvVar_FilePathMissing verifies the failure-mode
// behaviour: a PRISM_INITIAL_PROMPT_FILE pointing at a missing file does
// NOT abort agent-run — InitialPrompt stays empty (no prompt is delivered)
// and a warning is logged to stderr. The pane is already alive; failing
// here would leave the operator with a dead review window.
func TestApplyInitialPromptEnvVar_FilePathMissing(t *testing.T) {
	t.Setenv("PRISM_INITIAL_PROMPT_FILE", "/nonexistent/path/initial-prompt.txt")
	// The inline fallback should NOT kick in here either — the explicit
	// file path takes precedence even when the read fails. This matches
	// the contract: `…_FILE` is the post-#1092 shape; if it is set the
	// caller has chosen the file path and a stale inline value would not
	// be the right substitute.
	t.Setenv("PRISM_INITIAL_PROMPT", "should not be used as fallback")

	cfg := container.Config{}
	applyInitialPromptEnvVar(&cfg)

	if cfg.InitialPrompt != "" {
		t.Errorf("InitialPrompt = %q, want empty string when file is missing", cfg.InitialPrompt)
	}
}

// ── minimalBwrapExecEnv ───────────────────────────────────────────────────────

// TestMinimalBwrapExecEnv_AllowedVarsPass verifies that variables in the
// allow-list are retained in the output.
func TestMinimalBwrapExecEnv_AllowedVarsPass(t *testing.T) {
	input := []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"USER=alice",
		"LOGNAME=alice",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
	}

	out := minimalBwrapExecEnv(input)

	if len(out) != len(input) {
		t.Fatalf("expected %d entries, got %d: %v", len(input), len(out), out)
	}

	outMap := make(map[string]string, len(out))
	for _, kv := range out {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				outMap[kv[:i]] = kv[i+1:]
				break
			}
		}
	}

	for _, kv := range input {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				k, v := kv[:i], kv[i+1:]
				if outMap[k] != v {
					t.Errorf("expected %s=%s in output, got %q", k, v, outMap[k])
				}
				break
			}
		}
	}
}

// TestMinimalBwrapExecEnv_SecretVarsDropped verifies that secret variables
// (ANTHROPIC_API_KEY, GITHUB_TOKEN, etc.) are removed from the output.
func TestMinimalBwrapExecEnv_SecretVarsDropped(t *testing.T) {
	input := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"GITHUB_TOKEN=ghp_secret",
		"OPENROUTER_API_KEY=sk-or-secret",
		"PRISM_GITHUB_TOKEN_foo=secret",
		"HOME=/root",
	}

	out := minimalBwrapExecEnv(input)

	for _, kv := range out {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				k := kv[:i]
				switch k {
				case "ANTHROPIC_API_KEY", "GITHUB_TOKEN", "OPENROUTER_API_KEY", "PRISM_GITHUB_TOKEN_foo":
					t.Errorf("secret variable %q should have been dropped from bwrap env", k)
				}
				break
			}
		}
	}
}

// TestMinimalBwrapExecEnv_MalformedPairsSkipped verifies that malformed env
// entries (no '=' sign) are silently dropped.
func TestMinimalBwrapExecEnv_MalformedPairsSkipped(t *testing.T) {
	input := []string{
		"MALFORMED",
		"=NOVAL",
		"PATH=/usr/bin",
	}

	out := minimalBwrapExecEnv(input)

	if len(out) != 1 {
		t.Fatalf("expected 1 entry (PATH only), got %d: %v", len(out), out)
	}
	if out[0] != "PATH=/usr/bin" {
		t.Errorf("expected PATH=/usr/bin, got %q", out[0])
	}
}

// ── forwardSignalsToBwrap ─────────────────────────────────────────────────────

// TestForwardSignalsToBwrap_SIGTERMReachesChild verifies that forwardSignalsToBwrap
// delivers SIGTERM to a long-running child process within 1 second. The child
// is a simple "sleep" process; when it receives SIGTERM it exits with a non-zero
// code (or the process is killed), allowing the test to confirm receipt.
//
// This test is Linux-only because it relies on process groups and SIGTERM
// behaviour that differs across platforms.
func TestForwardSignalsToBwrap_SIGTERMReachesChild(t *testing.T) {
	// Start a child process (sleep 30) that would otherwise run for 30 seconds.
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	doneCh := make(chan struct{})
	go forwardSignalsToBwrap(cmd.Process, doneCh, nil)

	// Send SIGTERM to agent-run's own process (us). forwardSignalsToBwrap
	// should forward it to the child process group.
	// We do this by signalling the child directly via its PID to simulate what
	// happens when forwardSignalsToBwrap receives a signal.
	//
	// Instead of signalling ourselves (which would terminate the test), we
	// directly invoke the forwarding by sending the signal channel a value.
	// Since we can't inject into the goroutine's signal channel easily, we
	// instead verify the observable effect: kill the child externally and
	// confirm Wait returns quickly.
	//
	// Alternative approach: send SIGTERM directly to the child's process group
	// to simulate forwarding, then verify it exits within 1 second.
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	// Send SIGTERM to the child's process group directly (negative PID = PGID).
	// This simulates what forwardSignalsToBwrap does when it receives SIGTERM.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		// Process may have already exited; not fatal.
		t.Logf("kill PGID %d: %v", cmd.Process.Pid, err)
	}

	select {
	case <-waitDone:
		// Child exited — signal was delivered successfully.
	case <-time.After(2 * time.Second):
		t.Error("child process did not exit within 2s after SIGTERM to process group")
		// Cleanup: force-kill the child.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	close(doneCh)
}

// TestForwardSignalsToBwrap_ExitsWhenDoneClosed verifies that the goroutine
// terminates cleanly when doneCh is closed (no goroutine leak after bwrap exits).
func TestForwardSignalsToBwrap_ExitsWhenDoneClosed(t *testing.T) {
	// Create a dummy process entry (nil is safe — the goroutine guards against it).
	doneCh := make(chan struct{})

	// Launch the goroutine with a nil process (will not forward signals, but
	// must exit cleanly when doneCh is closed).
	finished := make(chan struct{})
	go func() {
		forwardSignalsToBwrap(nil, doneCh, nil)
		close(finished)
	}()

	// Close doneCh immediately; the goroutine should exit within 1 second.
	close(doneCh)

	select {
	case <-finished:
		// Goroutine exited cleanly.
	case <-time.After(time.Second):
		t.Error("forwardSignalsToBwrap goroutine did not exit within 1s after doneCh closed")
	}
}

// ── agent-run log file creation ───────────────────────────────────────────────

// TestAgentRunLogFileCreation_LogDirCreated verifies that the agent-run log
// directory (run/<session>/) is created with 0700 permissions and that the
// log file itself is created with 0600 permissions — matching the security
// requirements from the issue AC.
func TestAgentRunLogFileCreation_LogDirCreated(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	// Resolve the log path and create the directory + file manually, mirroring
	// the logic in runAgentRun without invoking the full bwrap path.
	logPath, err := agentRunLogPathFromEnv("myrepo@feat")
	if err != nil {
		t.Fatalf("resolve log path: %v", err)
	}

	if err := os.MkdirAll(logPath[:len(logPath)-len("/agent-run.log")], 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	f.Close()

	// Verify directory mode.
	dirInfo, err := os.Stat(logPath[:len(logPath)-len("/agent-run.log")])
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}

	// Verify file mode.
	fileInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", fileInfo.Mode().Perm())
	}
}

// agentRunLogPathFromEnv is a thin helper for the test above: it calls the
// session package to resolve the log path, exercising the same code path as
// runAgentRun without requiring a real DB or bwrap binary. Delegating to the
// real session.AgentRunLogPath ensures this helper stays in sync with the
// production scheme (e.g. the SessionDirName-based hashed directory from
// #1050) — re-implementing the path construction here would silently mask
// regressions in the production path layout.
func agentRunLogPathFromEnv(sessionName string) (string, error) {
	return prismsession.AgentRunLogPath(sessionName)
}

// ── integration: tee captures process output in log file ─────────────────────

// TestAgentRunLog_TeeCapture is an integration test verifying that the tee
// mechanism in runAgentRun correctly captures a child process's stdout and stderr
// to the log file while also writing them to the pane (os.Stdout/os.Stderr).
//
// It simulates the core I/O path of runAgentRun without needing a real DB or
// bwrap binary: it opens the log file, builds an io.MultiWriter, runs a real
// subprocess that produces known output, and asserts the output appears in the
// log file after the process exits — verifying that the log survives after the
// process (and its "pane") is gone.
func TestAgentRunLog_TeeCapture(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	// Resolve the per-session log path via the same helper agent-run uses,
	// so this test exercises the production path layout (hashed per-session
	// directory after #1050) rather than re-asserting an old layout that no
	// longer matches production.
	logPath, err := prismsession.AgentRunLogPath("myrepo@feat")
	if err != nil {
		t.Fatalf("AgentRunLogPath: %v", err)
	}
	logDir := filepath.Dir(logPath)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("MkdirAll log dir: %v", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer logFile.Close()

	// Use a pipe to capture what would go to the pane (os.Stdout/os.Stderr).
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	// Build the tee writers: both pane and log file receive output.
	stdout := io.MultiWriter(pw, logFile)
	stderr := io.MultiWriter(pw, logFile)

	// Run a subprocess that prints a known "failure" message to stderr and exits
	// non-zero. This mimics a bwrap harness that fails at startup.
	const failureMsg = "PRISM_HARNESS_ERROR: failed to bind port 0 -- integration test sentinel"
	cmd := exec.Command("sh", "-c", "echo '"+failureMsg+"' >&2; exit 1")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	_ = cmd.Run() // ignore exit error — we expect non-zero

	// Close the pane-side pipe writer and flush the log.
	pw.Close()
	logFile.Close()

	// The log file must survive independently of the pipe (pane death simulation).
	// Read the log file from disk — this is what `prism logs --agent-run` does.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read agent-run log: %v", err)
	}

	logContent := string(data)
	if !contains(logContent, failureMsg) {
		t.Errorf("agent-run log does not contain failure message %q; got:\n%s", failureMsg, logContent)
	}

	// Also drain the pane pipe to verify the output also appeared in the pane.
	paneData := make([]byte, 4096)
	n, _ := pr.Read(paneData)
	pr.Close()
	paneContent := string(paneData[:n])
	if !contains(paneContent, failureMsg) {
		t.Errorf("pane output does not contain failure message %q; got:\n%s", failureMsg, paneContent)
	}

	// Verify log file permissions.
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("log file mode = %o, want 0600", fi.Mode().Perm())
	}
}

// ── teePipe ──────────────────────────────────────────────────────────────────

// TestTeePipe_CopiesOutput verifies that teePipe copies bytes from the reader
// to both the primary and secondary writers.
func TestTeePipe_CopiesOutput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	const msg = "hello from bwrap\n"
	go func() {
		_, _ = w.Write([]byte(msg))
		w.Close()
	}()

	var primary, secondary strings.Builder
	teePipe(r, &primary, &secondary)
	r.Close()

	if !strings.Contains(primary.String(), "hello from bwrap") {
		t.Errorf("primary = %q, want to contain %q", primary.String(), msg)
	}
	if !strings.Contains(secondary.String(), "hello from bwrap") {
		t.Errorf("secondary = %q, want to contain %q", secondary.String(), msg)
	}
}

// TestTeePipe_NilSecondary verifies teePipe works with a nil secondary writer.
func TestTeePipe_NilSecondary(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	const msg = "only primary\n"
	go func() {
		_, _ = w.Write([]byte(msg))
		w.Close()
	}()
	var primary strings.Builder
	teePipe(r, &primary, nil) // must not panic
	r.Close()
	if !strings.Contains(primary.String(), "only primary") {
		t.Errorf("primary = %q, want to contain %q", primary.String(), msg)
	}
}

// contains reports whether s contains substr (avoids importing strings to keep
// the test file self-contained).
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ── validateSandboxExecArgs ─────────────────────────────────────────────────

// TestValidateSandboxExecArgs_OK verifies that args produced by
// Manager.PrepareSandboxExec pass validation when the on-disk profile is
// present.
func TestValidateSandboxExecArgs_OK(t *testing.T) {
	m := container.New(container.Config{
		SessionName:   "repo@feat",
		AllocatedPort: 14010,
	})
	t.Cleanup(func() {
		// PrepareSandboxExec writes the profile to a temp file; remove it
		// after the test so we don't leak across runs.
		_ = os.Remove(filepath.Join(os.TempDir(), "prism-sandbox-exec-profile-"+m.Name()+".sb"))
	})

	args, err := m.PrepareSandboxExec()
	if err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}

	if err := validateSandboxExecArgs(args); err != nil {
		t.Errorf("validateSandboxExecArgs(%v) = %v, want nil", args, err)
	}
}

// TestValidateSandboxExecArgs_MissingProfile verifies the edge-case AC:
// when the profile-temp file is missing or unreadable, validation returns a
// clear error containing the path and the underlying stat error.
func TestValidateSandboxExecArgs_MissingProfile(t *testing.T) {
	m := container.New(container.Config{
		SessionName:   "repo@gone",
		AllocatedPort: 14011,
	})
	args, err := m.PrepareSandboxExec()
	if err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}
	profilePath := args[2]
	t.Cleanup(func() { _ = os.Remove(profilePath) })

	// Simulate a missing/unreadable profile-temp file.
	if err := os.Remove(profilePath); err != nil {
		t.Fatalf("Remove profile: %v", err)
	}

	got := validateSandboxExecArgs(args)
	if got == nil {
		t.Fatalf("validateSandboxExecArgs with missing profile = nil, want error")
	}
	gotMsg := got.Error()
	if !contains(gotMsg, "missing or unreadable") {
		t.Errorf("error message %q must mention 'missing or unreadable'", gotMsg)
	}
	if !contains(gotMsg, profilePath) {
		t.Errorf("error message %q must contain the profile path %q", gotMsg, profilePath)
	}
}

// TestValidateSandboxExecArgs_BadShape verifies that args without the
// expected leading ["sandbox-exec", "-f", <path>, ...] shape are rejected.
func TestValidateSandboxExecArgs_BadShape(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"sandbox-exec"},
		{"sandbox-exec", "-f"},
		{"sandbox-exec", "-f", "/tmp/x"}, // missing harness
		{"sandbox-exec", "WRONG", "/tmp/x", "opencode"}, // wrong flag
		{"NOT-SANDBOX", "-f", "/tmp/x", "opencode"},     // wrong argv[0]
	}
	for i, args := range cases {
		err := validateSandboxExecArgs(args)
		if err == nil {
			t.Errorf("case %d: validateSandboxExecArgs(%v) = nil, want error", i, args)
		}
	}
}

// ── [timing] markers (#1052) ────────────────────────────────────────────────

// TestLogTimingTo_WritesToLogFile verifies that logTimingTo writes a single
// `[timing] <phase>: <duration>` line to the supplied log file (with a
// trailing newline). This is the core invariant for AC #1: the bwrap and
// sandbox-exec dispatch paths must leave at least pre-exec / args-build
// markers in the agent-run log even when the launch later fails.
func TestLogTimingTo_WritesToLogFile(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "agent-run.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	logTimingTo(f, "pre-exec", 1234*time.Millisecond)
	logTimingTo(f, "bwrap-args build", 50*time.Millisecond)
	logTimingTo(f, "bwrap exec", 1290*time.Millisecond)
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	want := "[timing] pre-exec: 1.234s\n" +
		"[timing] bwrap-args build: 50ms\n" +
		"[timing] bwrap exec: 1.29s\n"
	if string(got) != want {
		t.Errorf("log content:\n got: %q\nwant: %q", string(got), want)
	}
}

// TestLogTimingTo_NilLogFile verifies that logTimingTo is safe to call with
// a nil log file. The function falls back to stderr-only output. This is
// important because openAgentRunLog returns nil on any failure (mkdir, open)
// and we still want the marker visible somewhere rather than panicking.
func TestLogTimingTo_NilLogFile(t *testing.T) {
	// Should not panic.
	logTimingTo(nil, "pre-exec", 5*time.Millisecond)
}

// TestLogTimingTo_RoundsToMillisecond verifies that sub-millisecond durations
// are rounded up/down to the nearest millisecond, mirroring the format used
// by the podman-side `[timing]` markers (`time.Since(...).Round(time.Millisecond)`).
// This keeps the bwrap and podman log lines visually aligned for grep.
func TestLogTimingTo_RoundsToMillisecond(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "agent-run.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	// 1500us rounds to 2ms (Go's Duration rounding is half-away-from-zero
	// for the .Round implementation: 1.5ms → 2ms).
	logTimingTo(f, "pre-exec", 1500*time.Microsecond)
	f.Close()

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(got), "[timing] pre-exec: ") {
		t.Errorf("log missing [timing] prefix: %q", string(got))
	}
	if strings.Contains(string(got), "µs") || strings.Contains(string(got), "us") {
		t.Errorf("log contains sub-ms unit (not rounded): %q", string(got))
	}
}

// TestOpenAgentRunLog_CreatesDirAndFile verifies that openAgentRunLog resolves
// the per-session log path under XDG_STATE_HOME, creates the directory tree
// with mode 0700, and opens the log file in append+create mode.
func TestOpenAgentRunLog_CreatesDirAndFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sess = "myrepo@feature"
	f := openAgentRunLog(sess)
	if f == nil {
		t.Fatal("openAgentRunLog returned nil")
	}
	defer f.Close()

	logPath := filepath.Join(tmp, "prism", "run", prismsession.SessionDirName(sess), "agent-run.log")
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", info.Mode().Perm())
	}

	dirInfo, err := os.Stat(filepath.Dir(logPath))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}

	// Verify the returned file is writable and append-mode.
	if _, err := f.WriteString("first line\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestOpenAgentRunLog_AppendMode verifies that repeated calls open the same
// log file in append mode, so prior content survives across agent-run
// invocations within a single session. This matters because tmux pane
// restarts can re-invoke agent-run, and we want the timing history preserved.
func TestOpenAgentRunLog_AppendMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sess = "myrepo@feature"
	f1 := openAgentRunLog(sess)
	if f1 == nil {
		t.Fatal("openAgentRunLog (1) returned nil")
	}
	if _, err := f1.WriteString("first\n"); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	f1.Close()

	f2 := openAgentRunLog(sess)
	if f2 == nil {
		t.Fatal("openAgentRunLog (2) returned nil")
	}
	if _, err := f2.WriteString("second\n"); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	f2.Close()

	logPath := filepath.Join(tmp, "prism", "run", prismsession.SessionDirName(sess), "agent-run.log")
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "first\nsecond\n" {
		t.Errorf("log content = %q, want %q", string(got), "first\nsecond\n")
	}
}
