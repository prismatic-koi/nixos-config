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
