package cmd

// worktree_rollback_test.go — tests for the caller-level worktree rollback
// wiring (#2363): the shared rollbackCreatedWorktree helper and the runSpawn
// failure-path integration.
//
// Coverage:
//   - helper no-ops on nil created (reuse paths never arm the rollback)
//   - helper refuses to roll back while a tmux session exists for the
//     worktree (failure downstream of session creation, e.g. attach)
//   - helper removes worktree + freshly forked branch when no session exists
//   - runSpawn end-to-end: a spawn that fails after CreateWorktree leaves no
//     worktree and no branch behind

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// withAllFailTmux redirects tmux.TmuxBin to a stub that exits 1 for every
// invocation: has-session reports "no such session" and new-session fails,
// so SpawnSession fails fast at the layout step without a real tmux server.
//
// Only call this from non-parallel tests — TmuxBin is a package-level global.
func withAllFailTmux(t *testing.T) {
	t.Helper()
	wrapperPath := t.TempDir() + "/tmux"
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(wrapperPath, []byte(script), 0755); err != nil {
		t.Fatalf("write failing tmux: %v", err)
	}
	orig := tmux.TmuxBin
	tmux.TmuxBin = wrapperPath
	t.Cleanup(func() { tmux.TmuxBin = orig })
}

// cmdBranchExists reports whether refs/heads/<branch> exists in the
// bare-layout repo at bareRoot.
func cmdBranchExists(t *testing.T, bareRoot, branch string) bool {
	t.Helper()
	err := exec.Command("git", "--git-dir", filepath.Join(bareRoot, ".bare"),
		"rev-parse", "--verify", "refs/heads/"+branch).Run()
	return err == nil
}

// cmdWorktreeListed reports whether path appears in `git worktree list` for
// the bare-layout repo at bareRoot.
func cmdWorktreeListed(t *testing.T, bareRoot, path string) bool {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", filepath.Join(bareRoot, ".bare"),
		"worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	return strings.Contains(string(out), "worktree "+path)
}

// ── rollbackCreatedWorktree helper ───────────────────────────────────────────

func TestRollbackCreatedWorktreeHelper_NilCreated(t *testing.T) {
	// Reuse paths pass nil — must be a silent no-op, not a panic.
	rollbackCreatedWorktree(t.TempDir(), nil, "test")
}

func TestRollbackCreatedWorktreeHelper_SkipsWhenSessionLive(t *testing.T) {
	// Noop tmux: has-session exits 0, so the helper sees a live session for
	// the worktree and must leave both the worktree and the branch alone.
	withNoopTmux(t)
	baseDir := t.TempDir()
	bareRoot := initBareRepoWithMainWorktree(t, baseDir, "myrepo")

	created, err := git.CreateWorktree(bareRoot, "feat-live")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	rollbackCreatedWorktree(bareRoot, &created, "test")

	if !cmdWorktreeListed(t, bareRoot, created.Path) {
		t.Error("worktree was removed despite a live tmux session for it")
	}
	if !cmdBranchExists(t, bareRoot, "feat-live") {
		t.Error("branch was deleted despite a live tmux session for its worktree")
	}
}

func TestRollbackCreatedWorktreeHelper_RollsBackWhenNoSession(t *testing.T) {
	withAllFailTmux(t)
	baseDir := t.TempDir()
	bareRoot := initBareRepoWithMainWorktree(t, baseDir, "myrepo")

	created, err := git.CreateWorktree(bareRoot, "feat-dead")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	rollbackCreatedWorktree(bareRoot, &created, "test")

	if cmdWorktreeListed(t, bareRoot, created.Path) {
		t.Error("worktree still listed after rollback")
	}
	if _, statErr := os.Stat(created.Path); !os.IsNotExist(statErr) {
		t.Errorf("worktree dir still exists after rollback (stat err: %v)", statErr)
	}
	if cmdBranchExists(t, bareRoot, "feat-dead") {
		t.Error("freshly forked branch still exists after rollback")
	}
}

// ── runSpawn wiring ──────────────────────────────────────────────────────────

// TestRunSpawn_FailureAfterWorktreeCreation_RollsBack covers the core #2363
// AC: SpawnSession fails well after CreateWorktree (in environments without
// a configured pi_extension_dir it fails at the host-mode PIExtensionDir
// check; on machines with a full config it fails at the tmux layout step —
// the stub fails every tmux command). Either way the failure is in the
// armed window between CreateWorktree and SpawnSession success — the same
// error-return path a readiness timeout takes — and the deferred
// caller-level rollback must remove the freshly created worktree and its
// freshly forked branch. The rollback failure-isolation AC is also
// exercised: the error surfaced to the caller is the spawn error, never a
// rollback error (the unwind helper only logs).
func TestRunSpawn_FailureAfterWorktreeCreation_RollsBack(t *testing.T) {
	dbFile := setupIsolatedSpawn(t)
	_ = openTestDBAt(t, dbFile)
	baseDir := t.TempDir()
	bareRoot := initBareRepoWithMainWorktree(t, baseDir, "myrepo")
	t.Setenv("PRISM_BARE_ROOT", bareRoot)
	withAllFailTmux(t)

	cmd := buildSpawnCmdForMainReuseTest(t)
	_ = cmd.Flags().Set("branch", "feat-unwind")
	_ = cmd.Flags().Set("isolation", "host")
	_ = cmd.Flags().Set("prompt", "do the mahi")

	err := runSpawn(cmd, nil)
	if err == nil {
		t.Fatal("runSpawn: expected a spawn failure under failing tmux, got nil")
	}
	// The surfaced error must be the spawn failure, not a rollback error.
	if strings.Contains(err.Error(), "rollback created worktree") {
		t.Errorf("surfaced error carries the unwind error — the rollback masked the original failure: %v", err)
	}

	worktreePath := filepath.Join(bareRoot, "feat-unwind")
	if cmdWorktreeListed(t, bareRoot, worktreePath) {
		t.Error("worktree still in `git worktree list` after failed spawn")
	}
	if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
		t.Errorf("worktree dir still exists after failed spawn (stat err: %v)", statErr)
	}
	if cmdBranchExists(t, bareRoot, "feat-unwind") {
		t.Error("freshly forked branch still exists after failed spawn")
	}
}
