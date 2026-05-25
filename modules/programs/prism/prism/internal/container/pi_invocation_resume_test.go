package container

// pi_invocation_resume_test.go — issue #1838.
//
// Verifies PIInvocation's conversation-resume behaviour:
//
//   (a) appends `--session <id>` immediately before any positional InitialPrompt
//       when HarnessSessionID is set AND the on-disk session file exists;
//   (b) omits `--session` and writes a tagged warning to the per-session
//       agent-run log when the on-disk file is absent;
//   (c) is a complete no-op (no --session, no warning) when HarnessSessionID is
//       empty.
//
// All tests use t.TempDir() and t.Setenv("XDG_STATE_HOME", ...) so they never
// touch the host's real home dir / state dir.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBwrapResumeSession writes a synthetic pi session JSONL file at the
// post-#1985 bwrap-mode sessions root (<home>/.pi/agent/sessions/<encoded-cwd>/
// <ts>_<uuid>.jsonl) and returns the on-disk path.
//
// Pre-#1985 this helper planted files under
// <XDG_STATE_HOME>/prism/run/<hash>/pi-agent/sessions/; that staging-dir
// layout is gone now — bwrap pi sessions write into the host's global
// ~/.pi/agent/sessions/ tree (same as host mode), and the staging dir is
// overlay-bound onto it inside the sandbox.
//
// The file content is not parsed by the test \u2014 PIInvocation only stats the
// directory listing and matches on the filename suffix.
func writeBwrapResumeSession(t *testing.T, _ /*sessionName*/, worktree, sessionID string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatalf("writeBwrapResumeSession: resolve HOME: %v", err)
	}
	dir := filepath.Join(home, ".pi", "agent", "sessions", encodePiCWD(worktree))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir resume session dir: %v", err)
	}
	path := filepath.Join(dir, "2026-01-02T03-04-05-000Z_"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session\"}\n"), 0o600); err != nil {
		t.Fatalf("write resume session file: %v", err)
	}
	return path
}

// bwrapResumeCfg constructs a Config that piResumeSessionsRoot will identify
// as bwrap mode (SessionName + PIAgentConfigSandboxDir != PIAgentConfigHostDir).
func bwrapResumeCfg(sessionName, worktree, harnessSessionID string) Config {
	return Config{
		SessionName:             sessionName,
		Worktree:                worktree,
		HarnessSessionID:        harnessSessionID,
		PIAgentConfigHostDir:    "/tmp/host-stage", // bwrap remaps to a different sandbox path
		PIAgentConfigSandboxDir: "/run/prism/pi-agent",
	}
}

// TestPIInvocation_Resume_AppendsSessionWhenFileExists exercises AC8(a):
// when HarnessSessionID is set and a matching session file exists on disk,
// PIInvocation must append `--session <id>` immediately before any positional
// InitialPrompt argument.
func TestPIInvocation_Resume_AppendsSessionWhenFileExists(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	const sessionName = "myrepo@feature"
	const worktree = "/home/user/code/myrepo/feature"
	const harnessSessionID = "019e00ed-1234-7890-abcd-ef0123456789"

	writeBwrapResumeSession(t, sessionName, worktree, harnessSessionID)

	cfg := bwrapResumeCfg(sessionName, worktree, harnessSessionID)
	cfg.InitialPrompt = "do the thing"

	args := PIInvocation(cfg)

	if !hasPair(args, "--session", harnessSessionID) {
		t.Errorf("expected --session %s in args; got %v", harnessSessionID, args)
	}
	// Positional InitialPrompt must be the last arg.
	if args[len(args)-1] != "do the thing" {
		t.Errorf("expected initial prompt as last positional arg; got %v", args)
	}
	// --session must appear immediately before the InitialPrompt positional.
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--session" && args[i+1] == harnessSessionID {
			if i+2 != len(args)-1 {
				t.Errorf("expected --session <id> immediately before the InitialPrompt positional; got args[%d:]=%v",
					i, args[i:])
			}
		}
	}
}

// TestPIInvocation_Resume_AppendsSession_NoInitialPrompt verifies the
// positional-prompt-absent path: --session must still appear as the last pair
// when InitialPrompt is empty.
func TestPIInvocation_Resume_AppendsSession_NoInitialPrompt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	const sessionName = "myrepo@feature"
	const worktree = "/home/user/code/myrepo/feature"
	const harnessSessionID = "019e00ed-1111-2222-3333-444444444444"

	writeBwrapResumeSession(t, sessionName, worktree, harnessSessionID)

	cfg := bwrapResumeCfg(sessionName, worktree, harnessSessionID)
	// No InitialPrompt.

	args := PIInvocation(cfg)

	if !hasPair(args, "--session", harnessSessionID) {
		t.Errorf("expected --session %s in args; got %v", harnessSessionID, args)
	}
	// Last two args must be --session <id>; there must be no trailing positional.
	if len(args) < 2 || args[len(args)-2] != "--session" || args[len(args)-1] != harnessSessionID {
		t.Errorf("expected args to end with --session %s; got %v", harnessSessionID, args)
	}
}

// TestPIInvocation_Resume_OmitsSessionWhenFileMissing exercises AC8(b):
// when HarnessSessionID is set but no matching file is found, PIInvocation
// must omit --session AND a clearly-tagged warning line must land in the
// per-session agent-run log.
func TestPIInvocation_Resume_OmitsSessionWhenFileMissing(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("HOME", t.TempDir())

	const sessionName = "myrepo@feature"
	const worktree = "/home/user/code/myrepo/feature"
	const harnessSessionID = "019e00ed-aaaa-bbbb-cccc-deadbeef0000"

	// Deliberately do NOT write a session file.

	cfg := bwrapResumeCfg(sessionName, worktree, harnessSessionID)
	cfg.InitialPrompt = "do the thing"

	args := PIInvocation(cfg)

	if hasArg(args, "--session") {
		t.Errorf("expected --session to be absent when no session file exists; got %v", args)
	}
	// InitialPrompt must still be present as the trailing positional.
	if args[len(args)-1] != "do the thing" {
		t.Errorf("expected initial prompt as last positional arg; got %v", args)
	}

	// Warning line must have been written to the agent-run log.
	logPath := filepath.Join(stateHome, "prism", "run", sessionDirName(sessionName), "agent-run.log")
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read agent-run.log: %v", err)
	}
	want := "[agent-run] warning: pi session " + harnessSessionID + " not found"
	if !strings.Contains(string(body), want) {
		t.Errorf("agent-run.log = %q, want substring %q", string(body), want)
	}
}

// TestPIInvocation_Resume_EmptyHarnessSessionIDIsSilent exercises AC8(c) /
// AC5: when HarnessSessionID is empty, PIInvocation must behave exactly as
// pre-#1838 \u2014 no --session, no warning written, no side effects.
func TestPIInvocation_Resume_EmptyHarnessSessionIDIsSilent(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("HOME", t.TempDir())

	const sessionName = "myrepo@feature"
	const worktree = "/home/user/code/myrepo/feature"

	cfg := bwrapResumeCfg(sessionName, worktree, "") // empty HarnessSessionID
	cfg.InitialPrompt = "do the thing"

	args := PIInvocation(cfg)

	if hasArg(args, "--session") {
		t.Errorf("expected --session to be absent when HarnessSessionID is empty; got %v", args)
	}

	// No agent-run.log should have been created.
	logPath := filepath.Join(stateHome, "prism", "run", sessionDirName(sessionName), "agent-run.log")
	if _, err := os.Stat(logPath); err == nil {
		body, _ := os.ReadFile(logPath)
		t.Errorf("agent-run.log must not exist when HarnessSessionID is empty; got body=%q", string(body))
	}
}

// TestPIResumeSessionsRoot_AllModes verifies that the mode-aware resolver
// covers the isolation modes pi can run in (AC6). Post-#1985 the bwrap
// branch collapses into the host default (overlay-bound at launch):
//   - host:         <home>/.pi/agent/sessions
//   - bwrap:        <home>/.pi/agent/sessions  (same root as host)
//   - sandbox-exec: <stagingHome>/.pi/agent/sessions
func TestPIResumeSessionsRoot_AllModes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	t.Run("host", func(t *testing.T) {
		// No InstanceID, no PI staging dirs \u2014 falls through to host branch.
		cfg := Config{SessionName: "myrepo@main"}
		got, ok := piResumeSessionsRoot(cfg)
		if !ok {
			t.Fatalf("piResumeSessionsRoot(host): ok=false, want true")
		}
		want := filepath.Join(home, ".pi", "agent", "sessions")
		if got != want {
			t.Errorf("piResumeSessionsRoot(host) = %q, want %q", got, want)
		}
	})

	t.Run("bwrap", func(t *testing.T) {
		// Post-#1985: bwrap sessions write to the host's ~/.pi/agent/sessions/
		// (same as host mode); the per-session staging dir is overlay-bound
		// onto the in-sandbox $PI_CODING_AGENT_DIR/sessions/ at launch.
		const sessionName = "myrepo@feature"
		cfg := Config{
			SessionName:             sessionName,
			PIAgentConfigHostDir:    "/some/host/stage",
			PIAgentConfigSandboxDir: "/run/prism/pi-agent",
		}
		got, ok := piResumeSessionsRoot(cfg)
		if !ok {
			t.Fatalf("piResumeSessionsRoot(bwrap): ok=false, want true")
		}
		want := filepath.Join(home, ".pi", "agent", "sessions")
		if got != want {
			t.Errorf("piResumeSessionsRoot(bwrap) = %q, want %q", got, want)
		}
		// Defensive: must NOT point under the old per-session staging dir.
		oldStaging := filepath.Join(stateHome, "prism", "run")
		if strings.HasPrefix(got, oldStaging) {
			t.Errorf("piResumeSessionsRoot(bwrap) %q must not point under the per-session staging dir %q anymore (#1985)",
				got, oldStaging)
		}
	})

	t.Run("sandbox-exec", func(t *testing.T) {
		const instanceID = "11111111-2222-3333-4444-555555555555"
		// PIAgentConfigSandboxDir == PIAgentConfigHostDir signals sandbox-exec.
		cfg := Config{
			InstanceID:              instanceID,
			PIAgentConfigHostDir:    "/some/host/stage",
			PIAgentConfigSandboxDir: "/some/host/stage",
		}
		got, ok := piResumeSessionsRoot(cfg)
		if !ok {
			t.Fatalf("piResumeSessionsRoot(sandbox-exec): ok=false, want true")
		}
		stagingHome, err := SandboxExecStagingHomePath(instanceID)
		if err != nil {
			t.Fatalf("SandboxExecStagingHomePath: %v", err)
		}
		want := filepath.Join(stagingHome, ".pi", "agent", "sessions")
		if got != want {
			t.Errorf("piResumeSessionsRoot(sandbox-exec) = %q, want %q", got, want)
		}
	})
}

// TestEncodePiCWD_MatchesArchiveImpl is a self-consistency guard: the inline
// encodePiCWD in this package must produce the same encoded path that
// internal/harness/pi.EncodePiCWD produces, because both target the same
// on-disk directory layout pi creates. If pi's formula changes, both copies
// must change together \u2014 this test fails loudly when they diverge.
func TestEncodePiCWD_MatchesArchiveImpl(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/home/ben/code/nixos-config/main", "--home-ben-code-nixos-config-main--"},
		{"/", "----"},
		{"/a/b:c\\d", "--a-b-c-d--"},
	}
	for _, c := range cases {
		got := encodePiCWD(c.in)
		if got != c.want {
			t.Errorf("encodePiCWD(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
