package session

// host_resume_test.go — issue #1838.
//
// Unit tests for the host-mode pi-resume branch in buildDirectAgentCmd.
// The corresponding bwrap/sandbox-exec resume path lives in PIInvocation
// and is covered by internal/container/pi_invocation_resume_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHostModeResumeSession writes a synthetic pi session JSONL under the
// host-mode sessions root (<HOME>/.pi/agent/sessions/<encoded-cwd>/) that
// container.ResolvePIResumeSession will find. Returns the on-disk path.
func writeHostModeResumeSession(t *testing.T, home, worktree, sessionID string) string {
	t.Helper()
	dir := filepath.Join(home, ".pi", "agent", "sessions", piEncodeCWDForTest(worktree))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir host-mode sessions dir: %v", err)
	}
	path := filepath.Join(dir, "2026-01-02T03-04-05-000Z_"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session\"}\n"), 0o600); err != nil {
		t.Fatalf("write host-mode session file: %v", err)
	}
	return path
}

// piEncodeCWDForTest mirrors internal/container.encodePiCWD. Inlined to keep
// this test free of an internal-container dependency for the encoding.
func piEncodeCWDForTest(cwd string) string {
	stripped := strings.TrimLeft(cwd, "/\\")
	replaced := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(stripped)
	return "--" + replaced + "--"
}

// TestBuildDirectAgentCmd_HostMode_AppendsSessionWhenFileExists verifies that
// buildDirectAgentCmd appends `--session '<id>'` when the on-disk pi session
// JSONL is found under the host-mode sessions root.
func TestBuildDirectAgentCmd_HostMode_AppendsSessionWhenFileExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const harnessSessionID = "019e00ed-1234-7890-abcd-ef0123456789"
	worktree := filepath.Join(home, "code", "myrepo", "feature")
	writeHostModeResumeSession(t, home, worktree, harnessSessionID)

	opts := Opts{
		IsolationMode:    "host",
		HarnessName:      "pi",
		Agent:            "worker",
		SessionName:      "myrepo@feature",
		Worktree:         worktree,
		HarnessSessionID: harnessSessionID,
		Prompt:           "hello",
	}
	cmd := buildDirectAgentCmd(opts)

	want := "--session '" + harnessSessionID + "'"
	if !strings.Contains(cmd, want) {
		t.Errorf("buildDirectAgentCmd missing %q\ngot: %s", want, cmd)
	}
	// --session must appear before --prompt so the flag pair stays adjacent to
	// the binary and the prompt remains the trailing argument.
	sessionIdx := strings.Index(cmd, "--session")
	promptIdx := strings.Index(cmd, "--prompt")
	if sessionIdx == -1 || promptIdx == -1 || sessionIdx > promptIdx {
		t.Errorf("--session must appear before --prompt; got: %s", cmd)
	}
}

// TestBuildDirectAgentCmd_HostMode_OmitsSessionWhenFileMissing verifies the
// negative case: HarnessSessionID set, but no file on disk → no --session
// emitted. Proves the positive test isn't a no-op.
func TestBuildDirectAgentCmd_HostMode_OmitsSessionWhenFileMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	opts := Opts{
		IsolationMode:    "host",
		HarnessName:      "pi",
		Agent:            "worker",
		SessionName:      "myrepo@feature",
		Worktree:         "/some/worktree",
		HarnessSessionID: "019e00ed-aaaa-bbbb-cccc-deadbeef0000",
		Prompt:           "hello",
	}
	cmd := buildDirectAgentCmd(opts)

	if strings.Contains(cmd, "--session") {
		t.Errorf("buildDirectAgentCmd unexpectedly contains --session when no JSONL exists; got: %s", cmd)
	}
}

// TestBuildDirectAgentCmd_HostMode_EmptyHarnessSessionIDIsSilent verifies
// AC5 at the host-mode layer: empty HarnessSessionID → no --session, no
// resolver call (no state needed).
func TestBuildDirectAgentCmd_HostMode_EmptyHarnessSessionIDIsSilent(t *testing.T) {
	opts := Opts{
		IsolationMode: "host",
		HarnessName:   "pi",
		Agent:         "worker",
		SessionName:   "myrepo@feature",
		Worktree:      "/some/worktree",
		// HarnessSessionID intentionally empty.
		Prompt: "hello",
	}
	cmd := buildDirectAgentCmd(opts)
	if strings.Contains(cmd, "--session") {
		t.Errorf("buildDirectAgentCmd must not emit --session when HarnessSessionID is empty; got: %s", cmd)
	}
}

// TestBuildDirectAgentCmd_BwrapMode_DoesNotInvokeResolver verifies the
// review-context round-2 blocker fix: BuildAgentCmd calls buildDirectAgentCmd
// for every isolation mode, but the bwrap/sandbox-exec AgentPaneCmd discards
// the result and substitutes `prism agent-run`. If buildDirectAgentCmd ran
// ResolvePIResumeSession unconditionally, the resolver's host-fallback path
// would miss (bwrap sessions live under prism's per-session run dir, not
// under ~/.pi/agent/sessions) and piLogResumeWarning would spuriously write
// a misleading "pi session <id> not found" line to the per-session
// agent-run.log on every bwrap restore. The actual resume succeeds via
// agent-run's DB-read + PIInvocation path — the log line would be operator-
// confusing noise.
//
// Assertion: a bwrap-mode call with HarnessSessionID set must not produce
// any agent-run.log file (the dir wouldn't even be created by the resolver
// when the gate is correctly in place).
func TestBuildDirectAgentCmd_BwrapMode_DoesNotInvokeResolver(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("HOME", t.TempDir())

	const sessionName = "myrepo@feature"
	opts := Opts{
		IsolationMode:    "bwrap",
		HarnessName:      "pi",
		Agent:            "worker",
		SessionName:      sessionName,
		Worktree:         "/some/worktree",
		HarnessSessionID: "019e00ed-1111-2222-3333-444444444444",
		Prompt:           "hello",
	}
	_ = buildDirectAgentCmd(opts)

	// The host-mode resolver would write to
	// <XDG_STATE_HOME>/prism/run/<dirHash>/agent-run.log on a miss. With the
	// host-mode gate in place, the file (and even its parent run dir) must
	// not exist.
	logPath := filepath.Join(stateHome, "prism", "run")
	if _, err := os.Stat(logPath); err == nil {
		t.Errorf("resolver ran on bwrap mode and created %s — the host-mode gate is missing", logPath)
	}
}

// TestBuildDirectAgentCmd_SandboxExecMode_DoesNotInvokeResolver is the
// sandbox-exec analogue of the bwrap-mode test above. Both modes use
// `prism agent-run --session <name>` as their pane command (see
// dispatch.go AgentPaneCmd for both isolators), so buildDirectAgentCmd's
// resolver-invocation must be gated for both.
func TestBuildDirectAgentCmd_SandboxExecMode_DoesNotInvokeResolver(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("HOME", t.TempDir())

	opts := Opts{
		IsolationMode:    "sandbox-exec",
		HarnessName:      "pi",
		Agent:            "worker",
		SessionName:      "myrepo@feature",
		Worktree:         "/some/worktree",
		HarnessSessionID: "019e00ed-1111-2222-3333-555555555555",
		Prompt:           "hello",
	}
	_ = buildDirectAgentCmd(opts)

	logPath := filepath.Join(stateHome, "prism", "run")
	if _, err := os.Stat(logPath); err == nil {
		t.Errorf("resolver ran on sandbox-exec mode and created %s — the host-mode gate is missing", logPath)
	}
}

// TestBuildDirectAgentCmd_HostMode_NonPiHarnessSkipsResume verifies AC7:
// non-pi harnesses never go through the resume path even if HarnessSessionID
// somehow gets set (defence-in-depth — restoreProjectSession only sets it
// from a pi-emitted DB column, but the harness check is the load-bearing
// guard here).
func TestBuildDirectAgentCmd_HostMode_NonPiHarnessSkipsResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	const harnessSessionID = "019e00ed-1234-7890-abcd-ef0123456789"
	worktree := filepath.Join(home, "code", "myrepo", "feature")
	writeHostModeResumeSession(t, home, worktree, harnessSessionID)

	opts := Opts{
		IsolationMode:    "host",
		HarnessName:      "fake", // not pi
		Agent:            "worker",
		SessionName:      "myrepo@feature",
		Worktree:         worktree,
		HarnessSessionID: harnessSessionID,
		Prompt:           "hello",
	}
	cmd := buildDirectAgentCmd(opts)
	if strings.Contains(cmd, "--session") {
		t.Errorf("buildDirectAgentCmd must skip --session for non-pi harness; got: %s", cmd)
	}
}
