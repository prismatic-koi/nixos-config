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
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-branch",
		AgentRole:   "review-code",
		Prompt:      "review this PR",
		Layout:      LayoutAgentOnly,
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
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-branch",
		AgentRole:   "review-goal",
		Layout:      LayoutAgentOnly,
		GroupID:     groupID,
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
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-branch",
		AgentRole:   "review-qa",
		Layout:      LayoutAgentOnly,
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
		Layout: LayoutAgentOnly,
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
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-branch",
		AgentRole:   "review-security",
		Layout:      LayoutAgentOnly,
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
		Worktree:  "/tmp",
		AgentRole: "worker",
		Layout:    LayoutAgentOnly,
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
		SessionName: "myrepo@branch",
		AgentRole:   "worker",
		Layout:      LayoutAgentOnly,
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
		SessionName: "myrepo@branch",
		Worktree:    "/tmp",
		AgentRole:   "worker",
		Layout:      LayoutAgentOnly,
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
// We test "bwrap" and "podman" modes to verify the write happens for both.
func TestSpawnSession_AgentOnly_WritesIsolationMode(t *testing.T) {
	for _, mode := range []string{"bwrap", "podman"} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			d, _ := openSpawnTestDB(t)
			_ = spyTmuxBin(t)
			t.Setenv("PRISM_TEST_SUBPROCESS", "1")
			t.Setenv("XDG_STATE_HOME", t.TempDir())

			sessionName := "myrepo@branch~review-1-review-code-" + mode
			opts := SpawnOpts{
				SessionName:   sessionName,
				Repo:          "myrepo",
				Worktree:      "/worktrees/myrepo-branch",
				AgentRole:     "review-code",
				Layout:        LayoutAgentOnly,
				IsolationMode: mode,
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

// TestSpawnSession_AgentOnly_PromptEnvVar_WithPrompt_Host verifies the legacy
// path for the host-mode resolution: when opts.IsolationMode is empty and the
// machine default resolves to "host", spawnAgentOnlyLayout sets the inline
// PRISM_INITIAL_PROMPT env var on the agent pane (the pre-#1092 shape).
//
// This is the regression test for #1042 carried forward: review agents in
// host mode must receive their initial prompt via the PRISM_INITIAL_PROMPT
// env var. The new file-based path (PRISM_INITIAL_PROMPT_FILE) is gated on
// bwrap/sandbox-exec modes only — see
// TestSpawnSession_AgentOnly_PromptFile_WithPrompt_Bwrap below.
func TestSpawnSession_AgentOnly_PromptEnvVar_WithPrompt_Host(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	argsFile := spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch~review-1-review-code"
	const prompt = "review this PR"
	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      "/worktrees/myrepo-branch",
		AgentRole:     "review-code",
		Prompt:        prompt,
		Layout:        LayoutAgentOnly,
		IsolationMode: "host",
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	args := readSpyArgs(argsFile)
	if !containsSeq(args, []string{"-e", "PRISM_INITIAL_PROMPT=" + prompt}) {
		t.Errorf("tmux args %v do not contain [-e PRISM_INITIAL_PROMPT=%s] — host-mode review agents should still receive the inline env var (#1042)", args, prompt)
	}
	// The file-based env var must NOT appear for host mode — that path
	// would route the prompt through `prism agent-run`, which only fires
	// for bwrap/sandbox-exec.
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], "PRISM_INITIAL_PROMPT_FILE=") {
			t.Errorf("tmux args %v contain PRISM_INITIAL_PROMPT_FILE for host mode — should only fire for bwrap/sandbox-exec", args)
		}
	}
}

// TestSpawnSession_AgentOnly_PromptEnvVar_NoPrompt verifies that when
// opts.Prompt is empty, spawnAgentOnlyLayout does NOT set
// PRISM_INITIAL_PROMPT in the tmux pane environment. An empty-string -e entry
// would override an inherited value, which is not the desired behaviour.
func TestSpawnSession_AgentOnly_PromptEnvVar_NoPrompt(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	argsFile := spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch~review-1-review-code-noprompt"
	opts := SpawnOpts{
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-branch",
		AgentRole:   "review-code",
		// Prompt intentionally left empty.
		Layout: LayoutAgentOnly,
	}

	if err := SpawnSession(d, opts); err != nil {
		t.Fatalf("SpawnSession: %v", err)
	}

	args := readSpyArgs(argsFile)
	for i, a := range args {
		if a == "-e" {
			next := ""
			if i+1 < len(args) {
				next = args[i+1]
			}
			if strings.HasPrefix(next, "PRISM_INITIAL_PROMPT=") {
				t.Errorf("tmux args %v contain -e PRISM_INITIAL_PROMPT=… when Prompt was empty; an empty entry would override an inherited value", args)
				break
			}
		}
	}
}

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
		got := spawnAgentPaneEnvVars(SpawnOpts{Prompt: "hello"})
		if got == nil {
			t.Fatal("got nil, want non-nil map")
		}
		if v, ok := got["PRISM_INITIAL_PROMPT"]; !ok || v != "hello" {
			t.Errorf("PRISM_INITIAL_PROMPT = %q, want %q", v, "hello")
		}
	})
	t.Run("empty prompt", func(t *testing.T) {
		got := spawnAgentPaneEnvVars(SpawnOpts{Prompt: ""})
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
		SessionName: "myrepo@branch-bad-layout",
		Worktree:    "/tmp",
		AgentRole:   "worker",
		Layout:      LayoutScratchpad, // not supported by SpawnSession
	})
	if err == nil {
		t.Fatal("expected error for unsupported layout, got nil")
	}
	if !strings.Contains(err.Error(), "layout") {
		t.Errorf("error %q does not mention layout", err.Error())
	}
}
