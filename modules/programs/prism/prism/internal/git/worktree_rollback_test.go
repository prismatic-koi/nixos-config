package git

// worktree_rollback_test.go — tests for the CreateWorktree fork-info fields
// and RollbackCreatedWorktree.
//
// Coverage:
//   - CreateWorktree classification: fresh fork vs pre-existing local branch
//     vs pre-existing remote branch (BranchForked / ForkPoint fields)
//   - RollbackCreatedWorktree removes the worktree in all cases
//   - branch deletion fires ONLY for a freshly forked branch with no commits
//     beyond its fork point
//   - rollback is idempotent (second call and already-deleted-dir cases)

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// setupBareLayoutRepo builds a prism bare-layout repo (a .bare/ dir plus a
// main worktree) with a real origin remote, via the same CloneWorktree path
// production uses. Any remoteBranches are created in the remote (forked from
// main) before the clone, so they appear as refs/remotes/origin/<name> in
// the layout repo.
func setupBareLayoutRepo(t *testing.T, remoteBranches ...string) string {
	t.Helper()
	srcDir := t.TempDir()
	initRepo(t, srcDir, "main")
	for _, b := range remoteBranches {
		runGitIn(t, srcDir, "branch", b)
	}
	remoteDir := t.TempDir()
	if out, err := exec.Command("git", "clone", "--bare", srcDir, remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("bare clone for remote: %v\n%s", err, out)
	}
	targetDir := t.TempDir()
	if err := CloneWorktree(remoteDir, targetDir, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	return targetDir
}

// branchExists reports whether refs/heads/<branch> exists in the bare-layout
// repo at projectPath.
func branchExists(t *testing.T, projectPath, branch string) bool {
	t.Helper()
	err := exec.Command("git", "--git-dir", filepath.Join(projectPath, ".bare"),
		"rev-parse", "--verify", "refs/heads/"+branch).Run()
	return err == nil
}

// commitIn makes an empty commit in the given worktree with signing disabled
// (host gitconfig may enable gpgsign globally).
func commitIn(t *testing.T, worktree string) {
	t.Helper()
	out, err := exec.Command("git",
		"-C", worktree,
		"-c", "user.email=test@test.com",
		"-c", "user.name=Test",
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
		"commit", "--allow-empty", "-m", "wip").CombinedOutput()
	if err != nil {
		t.Fatalf("git commit in %s: %v\n%s", worktree, err, out)
	}
}

// ── CreateWorktree classification ────────────────────────────────────────────

func TestCreateWorktree_FreshBranch_ForkInfo(t *testing.T) {
	projectPath := setupBareLayoutRepo(t)

	created, err := CreateWorktree(projectPath, "feat-x")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if created.Path != filepath.Join(projectPath, "feat-x") {
		t.Errorf("Path = %q, want %q", created.Path, filepath.Join(projectPath, "feat-x"))
	}
	if created.Branch != "feat-x" {
		t.Errorf("Branch = %q, want %q", created.Branch, "feat-x")
	}
	if !created.BranchForked {
		t.Error("BranchForked = false, want true for a freshly forked branch")
	}
	mainTip, err := runGit("--git-dir", filepath.Join(projectPath, ".bare"),
		"rev-parse", "--verify", "refs/heads/main")
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	if created.ForkPoint != mainTip {
		t.Errorf("ForkPoint = %q, want the main tip %q", created.ForkPoint, mainTip)
	}
}

func TestCreateWorktree_ExistingLocalBranch_NotForked(t *testing.T) {
	projectPath := setupBareLayoutRepo(t)
	bare := filepath.Join(projectPath, ".bare")
	if out, err := exec.Command("git", "--git-dir", bare, "branch", "local-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git branch local-b: %v\n%s", err, out)
	}

	created, err := CreateWorktree(projectPath, "local-b")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if created.BranchForked {
		t.Error("BranchForked = true, want false for a pre-existing local branch")
	}
	if created.ForkPoint != "" {
		t.Errorf("ForkPoint = %q, want empty for a pre-existing local branch", created.ForkPoint)
	}
}

func TestCreateWorktree_RemoteBranch_NotForked(t *testing.T) {
	projectPath := setupBareLayoutRepo(t, "pr-branch")

	created, err := CreateWorktree(projectPath, "pr-branch")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if created.BranchForked {
		t.Error("BranchForked = true, want false for a remote-tracking checkout")
	}
}

// ── RollbackCreatedWorktree semantics ────────────────────────────────────────

func TestRollbackCreatedWorktree_FreshBranchNoCommits_DeletesBoth(t *testing.T) {
	projectPath := setupBareLayoutRepo(t)
	created, err := CreateWorktree(projectPath, "feat-x")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	if err := RollbackCreatedWorktree(projectPath, created); err != nil {
		t.Fatalf("RollbackCreatedWorktree: %v", err)
	}

	if _, statErr := os.Stat(created.Path); !os.IsNotExist(statErr) {
		t.Errorf("worktree dir %q still exists after rollback (stat err: %v)", created.Path, statErr)
	}
	if isWorktreeRegistered(projectPath, created.Path) {
		t.Errorf("worktree %q still registered after rollback", created.Path)
	}
	if branchExists(t, projectPath, "feat-x") {
		t.Error("freshly forked branch feat-x still exists after rollback")
	}
}

func TestRollbackCreatedWorktree_BranchWithCommits_KeepsBranch(t *testing.T) {
	projectPath := setupBareLayoutRepo(t)
	created, err := CreateWorktree(projectPath, "feat-x")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	commitIn(t, created.Path)

	if err := RollbackCreatedWorktree(projectPath, created); err != nil {
		t.Fatalf("RollbackCreatedWorktree: %v", err)
	}

	if isWorktreeRegistered(projectPath, created.Path) {
		t.Errorf("worktree %q still registered after rollback", created.Path)
	}
	if !branchExists(t, projectPath, "feat-x") {
		t.Error("branch feat-x with commits beyond its fork point was deleted by rollback")
	}
	// The commit itself must survive: the branch tip must differ from the
	// fork point.
	tip, err := runGit("--git-dir", filepath.Join(projectPath, ".bare"),
		"rev-parse", "--verify", "refs/heads/feat-x")
	if err != nil {
		t.Fatalf("rev-parse feat-x: %v", err)
	}
	if tip == created.ForkPoint {
		t.Error("branch tip equals fork point — the wip commit was lost")
	}
}

func TestRollbackCreatedWorktree_PreexistingLocalBranch_KeepsBranch(t *testing.T) {
	projectPath := setupBareLayoutRepo(t)
	bare := filepath.Join(projectPath, ".bare")
	if out, err := exec.Command("git", "--git-dir", bare, "branch", "local-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git branch local-b: %v\n%s", err, out)
	}
	created, err := CreateWorktree(projectPath, "local-b")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	if err := RollbackCreatedWorktree(projectPath, created); err != nil {
		t.Fatalf("RollbackCreatedWorktree: %v", err)
	}

	if isWorktreeRegistered(projectPath, created.Path) {
		t.Errorf("worktree %q still registered after rollback", created.Path)
	}
	if !branchExists(t, projectPath, "local-b") {
		t.Error("pre-existing local branch local-b was deleted by rollback")
	}
}

// TestRollbackCreatedWorktree_RemoteTrackingBranch_KeepsBranch covers the
// `prism pr` AC: a pre-existing remote branch checked out into a worktree is
// not deleted on rollback — only the worktree created for it is removed.
func TestRollbackCreatedWorktree_RemoteTrackingBranch_KeepsBranch(t *testing.T) {
	projectPath := setupBareLayoutRepo(t, "pr-branch")
	created, err := CreateWorktree(projectPath, "pr-branch")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	if err := RollbackCreatedWorktree(projectPath, created); err != nil {
		t.Fatalf("RollbackCreatedWorktree: %v", err)
	}

	if isWorktreeRegistered(projectPath, created.Path) {
		t.Errorf("worktree %q still registered after rollback", created.Path)
	}
	if !branchExists(t, projectPath, "pr-branch") {
		t.Error("local tracking branch pr-branch was deleted by rollback")
	}
	// The remote ref must be untouched.
	if err := exec.Command("git", "--git-dir", filepath.Join(projectPath, ".bare"),
		"rev-parse", "--verify", "refs/remotes/origin/pr-branch").Run(); err != nil {
		t.Errorf("refs/remotes/origin/pr-branch missing after rollback: %v", err)
	}
}

func TestRollbackCreatedWorktree_Idempotent(t *testing.T) {
	projectPath := setupBareLayoutRepo(t)
	created, err := CreateWorktree(projectPath, "feat-x")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	if err := RollbackCreatedWorktree(projectPath, created); err != nil {
		t.Fatalf("first RollbackCreatedWorktree: %v", err)
	}
	// Second rollback must be a clean no-op — the already-removed worktree
	// and already-deleted branch are success states, not errors.
	if err := RollbackCreatedWorktree(projectPath, created); err != nil {
		t.Fatalf("second RollbackCreatedWorktree: %v", err)
	}
}

// TestRollbackCreatedWorktree_DirAlreadyGone covers the edge-case AC: when an
// unwind step finds the worktree directory already deleted out from under
// git, the rollback still succeeds (registration is pruned, the branch is
// still cleaned up) and returns nil so the original spawn error is what the
// caller reports.
func TestRollbackCreatedWorktree_DirAlreadyGone(t *testing.T) {
	projectPath := setupBareLayoutRepo(t)
	created, err := CreateWorktree(projectPath, "feat-x")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if err := os.RemoveAll(created.Path); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}

	if err := RollbackCreatedWorktree(projectPath, created); err != nil {
		t.Fatalf("RollbackCreatedWorktree with missing dir: %v", err)
	}

	if isWorktreeRegistered(projectPath, created.Path) {
		t.Errorf("worktree %q still registered after rollback with missing dir", created.Path)
	}
	if branchExists(t, projectPath, "feat-x") {
		t.Error("freshly forked branch feat-x still exists after rollback with missing dir")
	}
}
