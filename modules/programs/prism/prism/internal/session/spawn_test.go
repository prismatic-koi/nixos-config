package session

// Tests for SpawnSession — the shared primitive that powers both `prism spawn`
// and `prism review`'s per-agent spawn loop. See #849 §3.1 and #859.
//
// SpawnSession's responsibilities, in order:
//   1. Seed agent_status with root_agent_name (via UpsertStatusSeedRootAgentName).
//   2. Write group_id when opts.GroupID is non-empty (hook for Issue E).
//   3. Allocate a port from the DB range.
//   4. Create the tmux session and (for LayoutAgentOnly) start the sidecar.
//
// These tests exercise the behaviour that belongs to SpawnSession specifically:
// the DB side-effects (step 1–3) and the fail-fast path when port allocation
// fails. The tmux/sidecar mechanics in step 4 are covered separately by the
// existing session.Create, sidecar_test.go, and createAgentSession-equivalent
// tests — SpawnSession composes those helpers without adding new logic.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// openSpawnTestDB creates a fresh temp DB and registers cleanup.
func openSpawnTestDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	SetTestDBPath(dbFile)
	t.Cleanup(func() {
		d.Close()
		SetTestDBPath("")
	})
	return d, dbFile
}

// TestSpawnSession_AgentOnly_SeedsRootAgentName verifies that, after a
// successful LayoutAgentOnly spawn, the agent_status row has the expected
// root_agent_name, repo, worktree, and a non-zero allocated port — all written
// by SpawnSession before the sidecar and tmux session were created.
func TestSpawnSession_AgentOnly_SeedsRootAgentName(t *testing.T) {
	d, _ := openSpawnTestDB(t)

	// Redirect tmux to a spy so we don't need a real tmux server.
	_ = spyTmuxBin(t)
	// Make the sidecar stub exit quickly (see TestMain in sidecar_test.go).
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	// Route sidecar state files to a temp dir.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch~review-1-review-code"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "myrepo",
		Worktree:       "/worktrees/myrepo-branch",
		AgentRole:      "review-code",
		Prompt:         "review this PR",
		Layout:         LayoutAgentOnly,
		PIExtensionDir: testPIExtensionDir,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: got nil, want row")
	}
	if st.RootAgentName == nil || *st.RootAgentName != "review-code" {
		got := "<nil>"
		if st.RootAgentName != nil {
			got = *st.RootAgentName
		}
		t.Errorf("root_agent_name = %q, want %q", got, "review-code")
	}
	if st.Repo != "myrepo" {
		t.Errorf("repo = %q, want %q", st.Repo, "myrepo")
	}
	if st.Worktree != "/worktrees/myrepo-branch" {
		t.Errorf("worktree = %q, want %q", st.Worktree, "/worktrees/myrepo-branch")
	}
	if st.HarnessPort == nil || *st.HarnessPort == 0 {
		t.Error("expected non-zero harness_port; got nil or 0")
	}
	if st.GroupID != nil {
		t.Errorf("group_id = %q, want nil (GroupID was not set in opts)", *st.GroupID)
	}
}

// TestSpawnSession_AgentOnly_WritesGroupID verifies that when opts.GroupID is
// non-empty, SpawnSession writes it to agent_status.group_id. This is the hook
// Issue E (#860) will use to wire review rounds into session_groups.
func TestSpawnSession_AgentOnly_WritesGroupID(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// RegisterGroup first so the FK (or future FK) is satisfied. The
	// group_id column references session_groups(group_id) ON DELETE SET NULL.
	groupID, err := d.RegisterGroup("myrepo@branch")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	const sessionName = "myrepo@branch~review-1-review-goal"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "myrepo",
		Worktree:       "/worktrees/myrepo-branch",
		AgentRole:      "review-goal",
		Prompt:         "go",
		Layout:         LayoutAgentOnly,
		GroupID:        groupID,
		PIExtensionDir: testPIExtensionDir,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: got nil, want row")
	}
	if st.GroupID == nil {
		t.Fatal("group_id is nil; want it to be populated from opts.GroupID")
	}
	if *st.GroupID != groupID {
		t.Errorf("group_id = %q, want %q", *st.GroupID, groupID)
	}
}

// TestSpawnSession_AgentOnly_CreatesTmuxSession verifies that SpawnSession
// invokes tmux.NewSessionDetached and tmux.NewWindow for the agent window —
// i.e. SpawnSession produces the tmux-side shape that previously lived in
// review.go's createAgentSession helper.
func TestSpawnSession_AgentOnly_CreatesTmuxSession(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	argsFile := spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch~review-1-review-qa"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "myrepo",
		Worktree:       "/worktrees/myrepo-branch",
		AgentRole:      "review-qa",
		Prompt:         "go",
		Layout:         LayoutAgentOnly,
		PIExtensionDir: testPIExtensionDir,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	args := readSpyArgs(argsFile)
	joined := strings.Join(args, " ")

	// new-session arguments go through tmux.NewSessionDetached.
	if !strings.Contains(joined, "new-session") {
		t.Errorf("expected tmux new-session invocation, got args: %v", args)
	}
	if !strings.Contains(joined, sessionName) {
		t.Errorf("expected session name %q in tmux args, got: %v", sessionName, args)
	}
	// new-window for the agent pane.
	if !strings.Contains(joined, "new-window") {
		t.Errorf("expected tmux new-window invocation for agent pane, got args: %v", args)
	}
}

// TestSpawnSession_NoAgentRole_LeavesRootAgentNameNull verifies that when
// opts.AgentRole is empty, SpawnSession does NOT write a root_agent_name value
// — the UpsertStatusSeedRootAgentName helper preserves NULL in that case. This
// matches the pre-spawn-time seeding semantics (see Issue B / #857).
func TestSpawnSession_NoAgentRole_LeavesRootAgentNameNull(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch-no-role"
	opts := SpawnOpts{
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-branch",
		// AgentRole intentionally left empty.
		Prompt:         "go",
		Layout:         LayoutAgentOnly,
		PIExtensionDir: testPIExtensionDir,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: got nil, want row")
	}
	if st.RootAgentName != nil {
		t.Errorf("root_agent_name = %q, want nil when AgentRole is empty", *st.RootAgentName)
	}
}

// TestSpawnSession_AllocatePortFails_ReturnsError verifies SpawnSession's
// fail-fast path when port allocation fails. We force failure by closing the
// DB before the call so the subsequent AllocatePort query errors out.
//
// The error must be wrapped so callers can inspect it, and no tmux session
// must be created (tmux spy should see zero new-session args).
func TestSpawnSession_AllocatePortFails_ReturnsError(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	argsFile := spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Close the DB so AllocatePort fails with "sql: database is closed".
	// UpsertStatusSeedRootAgentName also fails on a closed DB, which is
	// fine — SpawnSession must surface that as an error too.
	d.Close()

	const sessionName = "myrepo@branch-alloc-fail"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "myrepo",
		Worktree:       "/worktrees/myrepo-branch",
		AgentRole:      "review-security",
		Prompt:         "go",
		Layout:         LayoutAgentOnly,
		PIExtensionDir: testPIExtensionDir,
	}

	err := SpawnSession(d, opts)
	if err == nil {
		t.Fatal("SpawnSession: got nil error, want an error (DB is closed)")
	}
	// The error message must clearly identify this as a SpawnSession
	// failure so callers can tell it apart from downstream errors.
	if !strings.Contains(err.Error(), "spawn session:") {
		t.Errorf("error %q does not begin with 'spawn session:' prefix", err.Error())
	}

	// The tmux spy must not see any new-session calls — a failure before
	// tmux creation must not leak a zombie tmux session.
	args := readSpyArgs(argsFile)
	for _, a := range args {
		if a == "new-session" {
			t.Errorf("tmux new-session was invoked despite early DB failure; args: %v", args)
			break
		}
	}
}

// TestSpawnSession_Validation_RequiresSessionName verifies the argument guard.
func TestSpawnSession_Validation_RequiresSessionName(t *testing.T) {
	d, _ := openSpawnTestDB(t)

	err := SpawnSession(d, SpawnOpts{
		Worktree:       "/tmp",
		AgentRole:      "worker",
		Layout:         LayoutAgentOnly,
		PIExtensionDir: testPIExtensionDir,
	})
	if err == nil {
		t.Fatal("expected error when SessionName is empty, got nil")
	}
	if !strings.Contains(err.Error(), "SessionName") {
		t.Errorf("error %q does not mention SessionName", err.Error())
	}
}

// TestSpawnSession_Validation_RequiresWorktree verifies the argument guard.
func TestSpawnSession_Validation_RequiresWorktree(t *testing.T) {
	d, _ := openSpawnTestDB(t)

	err := SpawnSession(d, SpawnOpts{
		SessionName:    "myrepo@branch",
		AgentRole:      "worker",
		Layout:         LayoutAgentOnly,
		PIExtensionDir: testPIExtensionDir,
	})
	if err == nil {
		t.Fatal("expected error when Worktree is empty, got nil")
	}
	if !strings.Contains(err.Error(), "Worktree") {
		t.Errorf("error %q does not mention Worktree", err.Error())
	}
}

// TestSpawnSession_Validation_RequiresDB verifies the argument guard.
func TestSpawnSession_Validation_RequiresDB(t *testing.T) {
	err := SpawnSession(nil, SpawnOpts{
		SessionName:    "myrepo@branch",
		Worktree:       "/tmp",
		AgentRole:      "worker",
		Layout:         LayoutAgentOnly,
		PIExtensionDir: testPIExtensionDir,
	})
	if err == nil {
		t.Fatal("expected error when db is nil, got nil")
	}
	if !strings.Contains(err.Error(), "db") {
		t.Errorf("error %q does not mention db", err.Error())
	}
}

// TestSpawnSession_AgentOnly_WritesIsolationMode verifies that spawnAgentOnlyLayout
// writes isolation_mode to agent_status BEFORE the agent window is created.
// This is the fix for issue #1034: prism agent-run reads isolation_mode from
// the DB immediately on startup and rejects the session if the mode doesn't
// match "bwrap". If we write isolation_mode only after tmux.NewWindow, agent-run
// races and sees NULL → the agent-run rejects the session.
//
// We test "bwrap" and "sandbox-exec" modes to verify the write happens for
// both.
func TestSpawnSession_AgentOnly_WritesIsolationMode(t *testing.T) {
	for _, mode := range []string{"bwrap", "sandbox-exec"} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			d, _ := openSpawnTestDB(t)
			_ = spyTmuxBin(t)
			t.Setenv("PRISM_TEST_SUBPROCESS", "1")
			t.Setenv("XDG_STATE_HOME", t.TempDir())

			sessionName := "myrepo@branch~review-1-review-code-" + mode
			opts := SpawnOpts{
				SessionName:    sessionName,
				Repo:           "myrepo",
				Worktree:       "/worktrees/myrepo-branch",
				AgentRole:      "review-code",
				Prompt:         "go",
				Layout:         LayoutAgentOnly,
				IsolationMode:  mode,
				PIExtensionDir: testPIExtensionDir,
			}

			if err := SpawnSession(d, opts); err != nil {
				t.Fatalf("SpawnSession(%q): %v", mode, err)
			}

			st, err := d.CurrentStatus(sessionName)
			if err != nil {
				t.Fatalf("CurrentStatus: %v", err)
			}
			if st == nil {
				t.Fatal("CurrentStatus: got nil, want row")
			}
			if st.IsolationMode != mode {
				t.Errorf("isolation_mode = %q, want %q — the DB write must happen before the agent window is created so prism agent-run does not race and see NULL",
					st.IsolationMode, mode)
			}
		})
	}
}

// TestSpawnSession_AgentOnly_PromptFile_WithPrompt_Host verifies the
// LayoutAgentOnly+host cell that was missing from the needsPromptFile gate
// before issue #1195. Darwin coordinators running review fan-outs use
// IsolationMode="host" with Layout=LayoutAgentOnly. Before the fix, this
// combination fell through to the legacy PRISM_INITIAL_PROMPT inline path,
// exceeding HostLaunchCmdSafeBound on non-trivial PRs.
//
// Post-#1195: host mode also uses PRISM_INITIAL_PROMPT_FILE regardless of
// layout. The legacy PRISM_INITIAL_PROMPT env var is no longer set by
// SpawnSession for any mode — it remains as a fallback only for direct
// callers of spawnAgentPaneEnvVars / agentPaneEnvVars that bypass SpawnSession.
func TestSpawnSession_AgentOnly_PromptFile_WithPrompt_Host(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	argsFile := spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sessionName = "myrepo@branch~review-1-review-code"
	const prompt = "review this PR"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "myrepo",
		Worktree:       "/worktrees/myrepo-branch",
		AgentRole:      "review-code",
		Prompt:         prompt,
		Layout:         LayoutAgentOnly,
		IsolationMode:  "host",
		PIExtensionDir: testPIExtensionDir,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	// Post-#1195: PRISM_INITIAL_PROMPT_FILE must appear (file-based delivery).
	filePath, pathErr := InitialPromptPath(sessionName)
	if pathErr != nil {
		t.Fatalf("InitialPromptPath: %v", pathErr)
	}
	args := readSpyArgs(argsFile)
	if !containsSeq(args, []string{"-e", "PRISM_INITIAL_PROMPT_FILE=" + filePath}) {
		t.Errorf("tmux args %v do not contain [-e PRISM_INITIAL_PROMPT_FILE=%s] — host-mode LayoutAgentOnly must now use file-based delivery (#1195)", args, filePath)
	}

	// The legacy inline env var must NOT appear — that was the pre-#1195 broken path.
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], "PRISM_INITIAL_PROMPT=") {
			t.Errorf("tmux args contain inline PRISM_INITIAL_PROMPT for host mode — would re-introduce #1195 launch-cmd size failure: %v", args)
			break
		}
	}

	// The prompt file must exist and contain the prompt verbatim.
	body, readErr := os.ReadFile(filePath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v — prompt file must exist after host-mode SpawnSession (#1195)", filePath, readErr)
	}
	if string(body) != prompt {
		t.Errorf("prompt round-trip mismatch: file=%q, want=%q", string(body), prompt)
	}
}

// TestSpawnSession_AgentOnly_PromptEnvVar_NoPrompt used to verify that
// spawnAgentOnlyLayout did not set an empty PRISM_INITIAL_PROMPT env var when
// opts.Prompt was empty. Since issue #1891 that combination is rejected at
// the SpawnSession entry point (LayoutAgentOnly requires a non-empty Prompt),
// so this test would never reach the env-var setup code it was guarding. The
// new rejection is covered by TestSpawnSession_NoPrompt_LayoutAgentOnly_Rejected
// in lost_prompt_test.go.

// TestSpawnSession_AgentOnly_PromptFile_WithPrompt_Bwrap is the regression
// test for #1092. When opts.IsolationMode is "bwrap" and opts.Prompt is
// non-empty, SpawnSession must write the prompt to a per-session file and
// set PRISM_INITIAL_PROMPT_FILE on the agent pane — not the inline
// PRISM_INITIAL_PROMPT env var that previously carried the prompt body
// onto tmux's argv and tripped the launch-command size guard for review
// fan-outs with long role prompts.
//
// Verifies the three pieces of the contract together:
//   - The file exists at the expected per-session path.
//   - The file contents byte-for-byte equal opts.Prompt.
//   - The tmux new-window argv carries PRISM_INITIAL_PROMPT_FILE=<path> and
//     does NOT carry the prompt body inline.
func TestSpawnSession_AgentOnly_PromptFile_WithPrompt_Bwrap(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	argsFile := spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sessionName = "myrepo@long-branch~review-2-review-context"
	// A 24 KB prompt — well above the 16 KiB safe bound, so any inline
	// path would also trip the guard. The file path keeps the launch
	// command O(1) in prompt size, so spawn must succeed.
	prompt := strings.Repeat("review-context system prompt body ", 720) // ~24 KB

	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      "/worktrees/myrepo-long-branch",
		AgentRole:     "review-context",
		Prompt:        prompt,
		Layout:        LayoutAgentOnly,
		IsolationMode: "bwrap",
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v — bwrap-mode review fan-out must accept large role prompts via the prompt-file path (#1092)", err)
	}

	// File side: must exist and round-trip the prompt bytes intact.
	filePath, pathErr := InitialPromptPath(sessionName)
	if pathErr != nil {
		t.Fatalf("InitialPromptPath: %v", pathErr)
	}
	body, readErr := os.ReadFile(filePath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v — prompt file must exist after a bwrap-mode SpawnSession", filePath, readErr)
	}
	if string(body) != prompt {
		t.Errorf("prompt round-trip mismatch: file len=%d, prompt len=%d", len(body), len(prompt))
	}

	// Permission side: the file must be 0600 — prompts can carry secrets
	// or branch context the operator does not want world-readable.
	st, statErr := os.Stat(filePath)
	if statErr != nil {
		t.Fatalf("Stat(%s): %v", filePath, statErr)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("prompt-file perms = %o, want 0600 (operator-only)", mode)
	}

	// Tmux side: PRISM_INITIAL_PROMPT_FILE must point to the file, and
	// PRISM_INITIAL_PROMPT must NOT carry the body — that would re-introduce
	// the #1092 failure mode.
	args := readSpyArgs(argsFile)
	if !containsSeq(args, []string{"-e", "PRISM_INITIAL_PROMPT_FILE=" + filePath}) {
		t.Errorf("tmux args do not contain [-e PRISM_INITIAL_PROMPT_FILE=%s] — review agents in bwrap mode must receive the prompt-file path (#1092)\nargs: %v", filePath, args)
	}
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], "PRISM_INITIAL_PROMPT=") {
			t.Errorf("tmux args carry inline PRISM_INITIAL_PROMPT for bwrap mode — would re-introduce the #1092 launch-cmd size failure: %v", args)
			break
		}
	}
	// Defence-in-depth: the tmux argv as a whole must not contain the
	// prompt body. With #1092 fixed, the only thing tmux sees is the
	// file path; a regression that put the body back on argv would fail
	// this assertion before the size guard or kernel ARG_MAX trips.
	for _, a := range args {
		if strings.Contains(a, prompt) {
			t.Errorf("tmux argv element contains the prompt body inline (len=%d) — file-based delivery is broken", len(a))
			break
		}
	}
}

// TestSpawnSession_AgentOnly_PromptFile_WithPrompt_SandboxExec mirrors the
// bwrap test for the sandbox-exec mode. The file-based path must fire for
// both bwrap and sandbox-exec because both delegate prompt delivery to
// `prism agent-run` (which now reads PRISM_INITIAL_PROMPT_FILE).
func TestSpawnSession_AgentOnly_PromptFile_WithPrompt_SandboxExec(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	argsFile := spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sessionName = "myrepo@branch~review-3-review-security"
	const prompt = "review-security system prompt body"

	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      "/worktrees/myrepo-branch",
		AgentRole:     "review-security",
		Prompt:        prompt,
		Layout:        LayoutAgentOnly,
		IsolationMode: "sandbox-exec",
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	filePath, pathErr := InitialPromptPath(sessionName)
	if pathErr != nil {
		t.Fatalf("InitialPromptPath: %v", pathErr)
	}
	body, readErr := os.ReadFile(filePath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v", filePath, readErr)
	}
	if string(body) != prompt {
		t.Errorf("prompt round-trip mismatch: got %q, want %q", string(body), prompt)
	}

	args := readSpyArgs(argsFile)
	if !containsSeq(args, []string{"-e", "PRISM_INITIAL_PROMPT_FILE=" + filePath}) {
		t.Errorf("tmux args do not contain [-e PRISM_INITIAL_PROMPT_FILE=%s] for sandbox-exec mode\nargs: %v", filePath, args)
	}
}

// TestSpawnAgentPaneEnvVars verifies the helper directly across the three
// shapes: file path set (post-#1092), inline prompt only (legacy), and no
// prompt at all (no env vars emitted).
func TestSpawnAgentPaneEnvVars(t *testing.T) {
	t.Run("with prompt file", func(t *testing.T) {
		got := spawnAgentPaneEnvVars(SpawnOpts{
			Prompt:         "hello",
			PromptFilePath: "/var/state/prism/run/abc/initial-prompt.txt",
			PIExtensionDir: testPIExtensionDir,
		})
		if got == nil {
			t.Fatal("got nil, want non-nil map")
		}
		if v, ok := got["PRISM_INITIAL_PROMPT_FILE"]; !ok || v != "/var/state/prism/run/abc/initial-prompt.txt" {
			t.Errorf("PRISM_INITIAL_PROMPT_FILE = %q, want %q", v, "/var/state/prism/run/abc/initial-prompt.txt")
		}
		if _, present := got["PRISM_INITIAL_PROMPT"]; present {
			t.Errorf("PRISM_INITIAL_PROMPT must NOT be set when PRISM_INITIAL_PROMPT_FILE is — would inline prompt body and re-introduce #1092: got %v", got)
		}
	})
	t.Run("with prompt only (legacy)", func(t *testing.T) {
		got := spawnAgentPaneEnvVars(SpawnOpts{Prompt: "hello",
			PIExtensionDir: testPIExtensionDir,
		})
		if got == nil {
			t.Fatal("got nil, want non-nil map")
		}
		if v, ok := got["PRISM_INITIAL_PROMPT"]; !ok || v != "hello" {
			t.Errorf("PRISM_INITIAL_PROMPT = %q, want %q", v, "hello")
		}
	})
	t.Run("empty prompt", func(t *testing.T) {
		got := spawnAgentPaneEnvVars(SpawnOpts{Prompt: "",
			PIExtensionDir: testPIExtensionDir,
		})
		if got != nil {
			t.Errorf("got %v, want nil (an empty entry would override an inherited value)", got)
		}
	})
}

// TestSpawnSession_UnsupportedLayout_ReturnsError verifies that Layout values
// outside the supported set (LayoutFull / LayoutAgentOnly) surface a clear
// error rather than silently falling through.
func TestSpawnSession_UnsupportedLayout_ReturnsError(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	err := SpawnSession(d, SpawnOpts{
		SessionName:    "myrepo@branch-bad-layout",
		Worktree:       "/tmp",
		AgentRole:      "worker",
		Layout:         LayoutScratchpad, // not supported by SpawnSession,
		PIExtensionDir: testPIExtensionDir,
	})
	if err == nil {
		t.Fatal("expected error for unsupported layout, got nil")
	}
	if !strings.Contains(err.Error(), "layout") {
		t.Errorf("error %q does not mention layout", err.Error())
	}
}

// TestSpawnSession_NeedsPromptFile_AllModesAndLayouts is a table-driven
// regression test for issue #1195. It verifies that SpawnSession writes the
// initial-prompt file for every (mode, layout) combination that carries a
// non-empty prompt — including the LayoutAgentOnly+host cell that was missing
// from the gate before #1195 and caused Darwin host-mode review fan-outs to
// inline large prompts in the tmux argv.
//
// The test checks the observable outcome (file written to InitialPromptPath)
// rather than the internal needsPromptFile variable, making it robust to
// future refactors that rename or restructure the gate.
func TestSpawnSession_NeedsPromptFile_AllModesAndLayouts(t *testing.T) {
	cases := []struct {
		name          string
		layout        Layout
		isolationMode string
	}{
		// LayoutFull cases
		{"LayoutFull+host", LayoutFull, "host"},
		{"LayoutFull+bwrap", LayoutFull, "bwrap"},
		{"LayoutFull+sandbox-exec", LayoutFull, "sandbox-exec"},

		// LayoutAgentOnly cases (the #1195 regression was LayoutAgentOnly+host)
		{"LayoutAgentOnly+host", LayoutAgentOnly, "host"},
		{"LayoutAgentOnly+bwrap", LayoutAgentOnly, "bwrap"},
		{"LayoutAgentOnly+sandbox-exec", LayoutAgentOnly, "sandbox-exec"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// These tests cannot run in parallel: they rewrite TmuxBin and
			// XDG_STATE_HOME (package-level globals).
			d, _ := openSpawnTestDB(t)
			_ = spyTmuxBin(t)
			t.Setenv("PRISM_TEST_SUBPROCESS", "1")
			tmp := t.TempDir()
			t.Setenv("XDG_STATE_HOME", tmp)

			const prompt = "review this PR — this is the initial prompt"
			sessionName := "myrepo@branch~review-1-review-code-" + strings.ReplaceAll(tc.name, "+", "-")

			opts := SpawnOpts{
				SessionName:    sessionName,
				Repo:           "myrepo",
				Worktree:       "/worktrees/myrepo",
				AgentRole:      "review-code",
				Prompt:         prompt,
				Layout:         tc.layout,
				IsolationMode:  tc.isolationMode,
				PIExtensionDir: testPIExtensionDir,
			}

			if err := SpawnSession(d, opts); err != nil {
				t.Fatalf("SpawnSession(%s): %v", tc.name, err)
			}

			// The prompt file must exist and contain the prompt verbatim.
			filePath, pathErr := InitialPromptPath(sessionName)
			if pathErr != nil {
				t.Fatalf("InitialPromptPath: %v", pathErr)
			}
			body, readErr := os.ReadFile(filePath)
			if readErr != nil {
				t.Fatalf("ReadFile(%s): %v — SpawnSession(%s) must write the prompt file for every mode/layout combination when Prompt is non-empty (#1195)", filePath, readErr, tc.name)
			}
			if string(body) != prompt {
				t.Errorf("SpawnSession(%s): prompt round-trip mismatch: file len=%d, prompt len=%d", tc.name, len(body), len(prompt))
			}
		})
	}
}

// TestSpawnSession_AgentOnly_PromptFile_WriteFails_ReturnsError is the AC5
// edge-case test for issue #1195. When WriteInitialPrompt fails (e.g. because
// the run directory is not writable), SpawnSession must return a clear error
// naming the failed write target and must NOT create any tmux session.
//
// We force the failure by making XDG_STATE_HOME point to a read-only
// directory after sidecarStateDir() would resolve the path, so the
// MkdirAll inside WriteInitialPrompt fails.
func TestSpawnSession_AgentOnly_PromptFile_WriteFails_ReturnsError(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	argsFile := spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")

	// Create a read-only root so any attempt to mkdir or write under it fails.
	roDir := t.TempDir()
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Skipf("could not chmod dir to read-only: %v (skipping on this platform)", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) }) // restore so t.Cleanup can remove it
	t.Setenv("XDG_STATE_HOME", roDir)

	const sessionName = "myrepo@branch~review-1-review-code-write-fail"
	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      "/worktrees/myrepo",
		AgentRole:     "review-code",
		Prompt:        "review this PR",
		Layout:        LayoutAgentOnly,
		IsolationMode: "bwrap",
	}

	err := SpawnSession(d, opts)
	if err == nil {
		t.Fatal("SpawnSession with read-only XDG_STATE_HOME: got nil error, want write-failure error (AC5 edge-case)")
	}

	// Error must mention the write operation so the operator can diagnose it.
	if !strings.Contains(err.Error(), "write initial prompt") &&
		!strings.Contains(err.Error(), "initial-prompt") &&
		!strings.Contains(err.Error(), "spawn session") {
		t.Errorf("error %q does not identify the write-failure cause — expected 'write initial prompt' or similar (#1195 AC5)", err.Error())
	}

	// No tmux session must have been created (fail before tmux state).
	args := readSpyArgs(argsFile)
	for _, a := range args {
		if a == "new-session" {
			t.Errorf("tmux new-session was invoked despite write failure — AC5 requires the spawn to fail before any tmux state is created; args: %v", args)
			break
		}
	}
}

// TestSpawnSession_AgentOnly_PromptFile_CleanedUpOnReadinessTimeout verifies
// AC6 (edge-case): the initial-prompt file is cleaned up when SpawnSession's
// readiness gate trips on timeout. This ensures a second spawn attempt with the
// same session name starts fresh rather than inheriting a stale prompt file.
//
// We rely on the existing readiness-gate-timeout path that fires when
// ReadinessTimeout > 0 and no sidecar writes state_change events (the PRISM_TEST_SUBPROCESS
// stub sidecar exits immediately, so no readiness signal ever arrives).
func TestSpawnSession_AgentOnly_PromptFile_CleanedUpOnReadinessTimeout(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)

	const sessionName = "myrepo@branch~review-1-review-code-cleanup"
	opts := SpawnOpts{
		SessionName:      sessionName,
		Repo:             "myrepo",
		Worktree:         "/worktrees/myrepo",
		AgentRole:        "review-code",
		Prompt:           "review this PR",
		Layout:           LayoutAgentOnly,
		IsolationMode:    "bwrap",
		ReadinessTimeout: 300 * time.Millisecond, // short; stub sidecar never signals readiness
	}

	// The spawn will fail (readiness gate trips after 300ms).
	if err := SpawnSession(d, opts); err == nil {
		t.Fatal("SpawnSession with short ReadinessTimeout: got nil, want *ReadinessTimeoutError")
	}

	// AC6: the initial-prompt file must be removed after the readiness-gate
	// timeout cleanup path runs removeInitialPrompt.
	filePath, pathErr := InitialPromptPath(sessionName)
	if pathErr != nil {
		t.Fatalf("InitialPromptPath: %v", pathErr)
	}
	if _, err := os.Stat(filePath); err == nil {
		t.Errorf("initial-prompt file %s still exists after readiness-gate timeout cleanup — AC6 requires it to be removed so a re-spawn with the same session name starts fresh (#1195)", filePath)
	}
}

// ── empty-prompt rejection (issue #1891 layer 4) ────────────────────────────
//
// LayoutFull and LayoutAgentOnly host an agent pane and require a prompt to
// drive the agent. LayoutBare and LayoutScratchpad are plain shells or
// dashboards and legitimately have no prompt. The layer-4 guard must reject
// the former and accept the latter — see issue #1891 AC5/AC6.

// TestSpawnSession_EmptyPrompt_LayoutFull_Rejected verifies AC6(d): an empty
// Prompt with LayoutFull is rejected before any side-effects (no tmux session,
// no DB row, no port allocation).
func TestSpawnSession_EmptyPrompt_LayoutFull_Rejected(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	argsFile := spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@empty-prompt-full"
	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      "/worktrees/myrepo-empty-full",
		AgentRole:     "worker",
		Layout:        LayoutFull,
		IsolationMode: "host",
		HarnessName:   "pi",
		// Prompt deliberately empty.,
		PIExtensionDir: testPIExtensionDir,
	}

	err := SpawnSession(d, opts)
	if err == nil {
		t.Fatal("SpawnSession: got nil, want error for empty Prompt with LayoutFull")
	}
	if !strings.Contains(err.Error(), "Prompt is required") {
		t.Errorf("error %q does not mention 'Prompt is required'", err.Error())
	}

	// Refusal must happen before any side-effects.
	if st, _ := d.CurrentStatus(sessionName); st != nil {
		t.Errorf("agent_status row created despite empty-prompt rejection: %+v", st)
	}
	args := readSpyArgs(argsFile)
	for _, a := range args {
		if a == "new-session" {
			t.Errorf("tmux new-session was invoked despite empty-prompt rejection; args: %v", args)
			break
		}
	}
}

// TestSpawnSession_EmptyPrompt_NonAgentLayouts_NotRejectedForEmptyPrompt is
// the regression guard for AC6(e) / AC5 of issue #1891: the layer-4 empty
// prompt guard must fire ONLY for LayoutFull and LayoutAgentOnly. LayoutBare
// and LayoutScratchpad are shell/dashboard layouts with no agent pane and
// legitimately have no prompt.
//
// SpawnSession does not currently support LayoutBare/LayoutScratchpad end to
// end — they have their own creation path — so we cannot assert nil-error
// here. What we *can* assert (and what would catch the regression #1891
// warns against) is that the error returned for these layouts is NOT the
// new "Prompt is required" guard. If a future refactor routes these layouts
// through SpawnSession, this test still passes; if the guard accidentally
// widens to reject them, this test fails immediately.
func TestSpawnSession_EmptyPrompt_NonAgentLayouts_NotRejectedForEmptyPrompt(t *testing.T) {
	for _, layout := range []struct {
		name string
		v    Layout
	}{
		{name: "LayoutBare", v: LayoutBare},
		{name: "LayoutScratchpad", v: LayoutScratchpad},
	} {
		layout := layout
		t.Run(layout.name, func(t *testing.T) {
			d, _ := openSpawnTestDB(t)
			_ = spyTmuxBin(t)
			t.Setenv("PRISM_TEST_SUBPROCESS", "1")
			t.Setenv("XDG_STATE_HOME", t.TempDir())

			opts := SpawnOpts{
				SessionName: "myrepo@" + layout.name + "-no-prompt",
				Repo:        "myrepo",
				Worktree:    "/worktrees/myrepo-" + layout.name,
				Layout:      layout.v,
				// Prompt deliberately empty — legitimate for these layouts.,
				PIExtensionDir: testPIExtensionDir,
			}

			err := SpawnSession(d, opts)
			if err != nil && strings.Contains(err.Error(), "Prompt is required") {
				t.Errorf("SpawnSession with empty Prompt + %s returned a 'Prompt is required' error: %v — the layer-4 guard must fire only for LayoutFull/LayoutAgentOnly (issue #1891 AC5)", layout.name, err)
			}
		})
	}
}

// TestSpawnSession_WritesSpawnInputs_AllFields is the end-to-end integration
// test for the centralised spawn_inputs writer (issue #2087). It verifies
// that SpawnSession, given a SpawnOpts populated with every audit-mirror
// field, inserts a single spawn_inputs row keyed by the host-minted
// instance_id and that every column lands with the expected value.
//
// This test exercises the writer path inside SpawnSession itself (rather than
// the helper SpawnInputsFromOpts in isolation), so a regression that
// silently drops the InsertSpawnInputs call — the failure mode that
// motivated #2087 — would be caught here.
func TestSpawnSession_WritesSpawnInputs_AllFields(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "prism-test@worker-spawn-inputs-end-to-end"
	overrides := map[string]string{
		"review-context": "google/gemini-2.5-pro",
	}
	opts := SpawnOpts{
		SessionName:          sessionName,
		Repo:                 "myrepo",
		Worktree:             "/worktrees/myrepo-spawn-inputs",
		AgentRole:            "worker",
		Prompt:               "ship the audit row",
		PromptSource:         "cli-positional",
		PromptTemplateHash:   "tmpl-sha-e2e",
		Layout:               LayoutAgentOnly,
		IsolationMode:        "host",
		HarnessName:          "pi",
		PIExtensionDir:       testPIExtensionDir,
		ProfileName:          "anthropic",
		ModelFlag:            "anthropic/claude-opus-4-7",
		VariantFlag:          "high",
		AgentFlag:            "worker",
		HarnessFlag:          "pi",
		IsolationFlag:        "host",
		HostModeFlag:         false,
		PRNumber:             2087,
		BranchFlag:           "prism-spawn-inputs-writer",
		IgnoreConcurrencyCap: true,
		ModelsByRole:         overrides,
		SkillsManifestHash:   "skills-e2e",
		AgentPromptHash:      "agent-e2e",
		AbtestPairID:         "abtest-e2e",
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil || st.InstanceID == nil || *st.InstanceID == "" {
		t.Fatal("CurrentStatus: missing instance_id after SpawnSession")
	}

	si, err := d.SpawnInputsByInstanceID(*st.InstanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if si == nil {
		t.Fatal("spawn_inputs row missing — SpawnSession must write a row for every spawn (#2087)")
	}

	checkPtr := func(name string, got *string, want string) {
		t.Helper()
		if got == nil {
			t.Errorf("spawn_inputs.%s: got nil, want %q", name, want)
			return
		}
		if *got != want {
			t.Errorf("spawn_inputs.%s: got %q, want %q", name, *got, want)
		}
	}
	checkPtr("profile_name", si.ProfileName, "anthropic")
	checkPtr("model_flag", si.ModelFlag, "anthropic/claude-opus-4-7")
	checkPtr("variant_flag", si.VariantFlag, "high")
	checkPtr("agent_flag", si.AgentFlag, "worker")
	checkPtr("harness_flag", si.HarnessFlag, "pi")
	checkPtr("isolation_flag", si.IsolationFlag, "host")
	checkPtr("branch_flag", si.BranchFlag, "prism-spawn-inputs-writer")
	checkPtr("skills_manifest_hash", si.SkillsManifestHash, "skills-e2e")
	checkPtr("prompt_template_hash", si.PromptTemplateHash, "tmpl-sha-e2e")
	checkPtr("agent_prompt_hash", si.AgentPromptHash, "agent-e2e")
	checkPtr("prompt_text", si.PromptText, "ship the audit row")
	checkPtr("prompt_source", si.PromptSource, "cli-positional")
	checkPtr("abtest_pair_id", si.AbtestPairID, "abtest-e2e")

	if si.PRNumber == nil || *si.PRNumber != 2087 {
		t.Errorf("spawn_inputs.pr_number: got %v, want 2087", si.PRNumber)
	}
	if !si.IgnoreConcurrencyCap {
		t.Error("spawn_inputs.ignore_concurrency_cap: got false, want true")
	}
	if si.HostModeFlag {
		t.Error("spawn_inputs.host_mode_flag: got true, want false")
	}
	if si.CreatedAt == 0 {
		t.Error("spawn_inputs.created_at: got 0, want non-zero")
	}
	if si.ModelVariantOverrides == nil || *si.ModelVariantOverrides == "" {
		t.Error("spawn_inputs.model_variant_overrides: got empty, want JSON of ModelsByRole")
	} else if !strings.Contains(*si.ModelVariantOverrides, "review-context") {
		t.Errorf("spawn_inputs.model_variant_overrides: got %q, expected to contain %q",
			*si.ModelVariantOverrides, "review-context")
	}
}

// TestSpawnSession_WritesSpawnInputs_MinimalRow verifies the floor of the
// AC contract for #2087: a SpawnSession invocation with only the required
// inputs (no audit flags) still produces a spawn_inputs row keyed by
// instance_id with created_at populated, so downstream JOINs see the row
// instead of the pre-fix empty-table state.
func TestSpawnSession_WritesSpawnInputs_MinimalRow(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "prism-test@worker-spawn-inputs-minimal"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "myrepo",
		Worktree:       "/worktrees/myrepo-minimal",
		AgentRole:      "worker",
		Prompt:         "minimal",
		Layout:         LayoutAgentOnly,
		PIExtensionDir: testPIExtensionDir,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}
	st, err := d.CurrentStatus(sessionName)
	if err != nil || st == nil || st.InstanceID == nil {
		t.Fatalf("CurrentStatus: %v / %v", err, st)
	}

	si, err := d.SpawnInputsByInstanceID(*st.InstanceID)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if si == nil {
		t.Fatal("spawn_inputs row missing — minimal spawn must still produce an audit row (#2087)")
	}
	if si.InstanceID != *st.InstanceID {
		t.Errorf("spawn_inputs.instance_id = %q, want %q", si.InstanceID, *st.InstanceID)
	}
	if si.CreatedAt == 0 {
		t.Error("spawn_inputs.created_at: got 0, want non-zero")
	}
	if si.PromptText == nil || *si.PromptText != "minimal" {
		t.Errorf("spawn_inputs.prompt_text: got %v, want %q", si.PromptText, "minimal")
	}
}

// TestSpawnSession_WritesSpawnInputs_AbtestPairIDShared verifies that two
// SpawnSession calls carrying the same AbtestPairID land two rows that
// share the abtest_pair_id column, matching the contract of `prism spawn
// --abtest` (cmd/spawn.go's runAbtestSpawn mints one pairID and passes it
// to both legs).
func TestSpawnSession_WritesSpawnInputs_AbtestPairIDShared(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	pairID := "abtest-pair-shared-uuid"

	legs := []struct{ name, profile string }{
		{"prism-test@worker-abtest-leg-a", "profileA"},
		{"prism-test@worker-abtest-leg-b", "profileB"},
	}
	instanceIDs := make([]string, len(legs))

	for i, leg := range legs {
		opts := SpawnOpts{
			SessionName:    leg.name,
			Repo:           "myrepo",
			Worktree:       "/worktrees/myrepo-" + leg.profile,
			AgentRole:      "worker",
			Prompt:         "abtest leg",
			Layout:         LayoutAgentOnly,
			PIExtensionDir: testPIExtensionDir,
			ProfileName:    leg.profile,
			AbtestPairID:   pairID,
		}
		if err := SpawnSession(d, opts); err != nil {
			t.Fatalf("SpawnSession leg %d (%q): %v", i, leg.name, err)
		}
		st, _ := d.CurrentStatus(leg.name)
		if st == nil || st.InstanceID == nil {
			t.Fatalf("leg %d: missing instance_id after SpawnSession", i)
		}
		instanceIDs[i] = *st.InstanceID
	}

	for i, iid := range instanceIDs {
		si, err := d.SpawnInputsByInstanceID(iid)
		if err != nil {
			t.Fatalf("SpawnInputsByInstanceID leg %d: %v", i, err)
		}
		if si == nil {
			t.Fatalf("leg %d: missing spawn_inputs row", i)
		}
		if si.AbtestPairID == nil || *si.AbtestPairID != pairID {
			t.Errorf("leg %d: abtest_pair_id = %v, want %q", i, si.AbtestPairID, pairID)
		}
		if si.ProfileName == nil || *si.ProfileName != legs[i].profile {
			t.Errorf("leg %d: profile_name = %v, want %q", i, si.ProfileName, legs[i].profile)
		}
	}
}
