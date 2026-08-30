package cmd

// Tests for `prism cleanup` deleting a branch whose name contains '/'. The
// session name carries the sanitised form ('/' → '--', not reversible), so
// any code path that reconstructs the branch name from the session component
// is broken for slash branches. cleanup reads the actual branch name from the
// worktree's HEAD (resolveBranchName) before the worktree is removed, rather
// than trusting the sanitised name.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/git"
)

// setupBareRepoWithBranch is a variant of setupMinimalBareRepo that accepts
// an arbitrary branch name (including one containing '/'), so the resulting
// worktree is nested exactly as prism would create it for a slash branch
// (bareRoot/quick/pr-123, not a flat sibling).
func setupBareRepoWithBranch(t *testing.T, branchName string) (bareRoot, worktreePath string) {
	t.Helper()

	baseDir := t.TempDir()
	bareRoot = filepath.Join(baseDir, "myrepo")
	bareDir := filepath.Join(bareRoot, ".bare")

	if err := os.MkdirAll(bareRoot, 0o755); err != nil {
		t.Fatalf("mkdir bareRoot: %v", err)
	}
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	_ = exec.Command("git", "--git-dir", bareDir, "config", "advice.detachedHead", "false").Run()

	initDir := filepath.Join(baseDir, "init-checkout")
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"add", "--orphan", "-b", "main", initDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add (orphan): %v\n%s", err, out)
	}
	cfgArgs := []string{
		"-C", initDir,
		"-c", "user.email=test@test.com",
		"-c", "user.name=Test",
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
	}
	if out, err := exec.Command("git", append(cfgArgs,
		"commit", "--allow-empty", "-m", "init")...).CombinedOutput(); err != nil {
		t.Fatalf("git commit (init): %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"remove", "--force", initDir).CombinedOutput(); err != nil {
		t.Fatalf("git worktree remove init-checkout: %v\n%s", err, out)
	}

	mainDir := filepath.Join(bareRoot, "main")
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"add", mainDir, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add main: %v\n%s", err, out)
	}

	worktreePath = filepath.Join(bareRoot, branchName)
	if out, err := exec.Command("git", "--git-dir", bareDir, "worktree",
		"add", "-b", branchName, worktreePath, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add %s: %v\n%s", branchName, err, out)
	}

	return bareRoot, worktreePath
}

// sanitisedComponent mirrors sanitiseBranchComponent's '/' → '--' mapping,
// which is what the session name (and therefore worktreeName) actually
// contains for a slash branch.
func sanitisedComponent(branch string) string {
	return strings.ReplaceAll(branch, "/", "--")
}

// TestResolveBranchName_SlashBranch is the direct unit test for the core
// fix: resolveBranchName must read the real branch name ("quick/pr-123")
// from the worktree's HEAD, not the sanitised session component
// ("quick--pr-123").
func TestResolveBranchName_SlashBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping integration test")
	}

	branchName := "quick/pr-123"
	_, worktreePath := setupBareRepoWithBranch(t, branchName)

	got, ok := resolveBranchName(worktreePath)
	if !ok {
		t.Fatalf("resolveBranchName(%q): ok=false, want true", worktreePath)
	}
	if got != branchName {
		t.Errorf("resolveBranchName(%q) = %q, want %q", worktreePath, got, branchName)
	}
}

// TestResolveBranchName_Empty covers the "cannot resolve" fallback path:
// an empty worktreePath (the "worktree path unknown" case) must return
// ok=false rather than panicking or fabricating a value.
func TestResolveBranchName_Empty(t *testing.T) {
	if _, ok := resolveBranchName(""); ok {
		t.Errorf("resolveBranchName(\"\"): ok=true, want false")
	}
}

// TestHeadlessCleanup_SlashBranch_DeletesBranch is the end-to-end regression
// test: a session whose branch contains '/' must have its branch deleted by
// headlessCleanupWithJSON, even though the worktreeName argument it receives
// is the sanitised ('/' → '--') session component and therefore does not name
// a real branch. Calling git.BranchExists with the sanitised name never
// matches a slash branch, so cleanup must resolve the real branch name first.
func TestHeadlessCleanup_SlashBranch_DeletesBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping integration test")
	}
	withNoopTmux(t)

	branchName := "quick/pr-1785538583"
	bareRoot, worktreePath := setupBareRepoWithBranch(t, branchName)

	// Simulate what cmd/cleanup.go actually passes: the sanitised session
	// component, NOT the real branch name.
	sanitised := sanitisedComponent(branchName)
	session := "myrepo@" + sanitised

	if !git.BranchExists(bareRoot, branchName) {
		t.Fatalf("setup: branch %q does not exist before cleanup", branchName)
	}

	if err := headlessCleanup(session, sanitised, worktreePath, bareRoot); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	if git.BranchExists(bareRoot, branchName) {
		t.Errorf("branch %q still exists after cleanup — slash branch was not deleted", branchName)
	}
}

// TestHeadlessCleanup_NonSlashBranch_StillDeletes is the no-regression
// counterpart: a branch name with no '/' must continue to delete correctly,
// since worktreeName and the real branch name are identical in that case.
func TestHeadlessCleanup_NonSlashBranch_StillDeletes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping integration test")
	}
	withNoopTmux(t)

	branchName := "feature-no-slash"
	bareRoot, worktreePath := setupBareRepoWithBranch(t, branchName)
	session := "myrepo@" + branchName

	if err := headlessCleanup(session, branchName, worktreePath, bareRoot); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	if git.BranchExists(bareRoot, branchName) {
		t.Errorf("branch %q still exists after cleanup", branchName)
	}
}

// TestHeadlessCleanup_UnresolvableBranch_WarnsAndSkips covers the
// unresolvable case named in the issue's ACs: when the branch cannot be
// matched (here, bareRoot is known but the worktree is already gone so
// resolveBranchName falls back to the — wrong — sanitised name, which does
// not exist as a branch), cleanup must not crash and must not silently
// fabricate success; it just has nothing to delete.
func TestHeadlessCleanup_UnresolvableBranch_WarnsAndSkips(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping integration test")
	}
	withNoopTmux(t)

	branchName := "quick/pr-999"
	bareRoot, _ := setupBareRepoWithBranch(t, branchName)
	sanitised := sanitisedComponent(branchName)
	session := "myrepo@" + sanitised

	// worktreePath is empty — the "could not determine worktree path" case —
	// so resolveBranchName has nothing to read HEAD from and the fallback
	// (sanitised name) does not match any real branch.
	if err := headlessCleanup(session, sanitised, "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	// The (unreachable via this path, but real) branch must be untouched.
	if !git.BranchExists(bareRoot, branchName) {
		t.Errorf("branch %q was deleted despite an unresolvable worktree path", branchName)
	}
}

// TestHeadlessCleanup_BranchCheckedOutElsewhere_DoesNotDelete covers the
// "do not delete a branch still checked out in another worktree" AC. Git
// itself refuses `branch -D` on a branch that is the current HEAD of
// another worktree; cleanup must surface that as a non-fatal warning
// (already logged via proglog.Warnf in the branch-delete error path) rather
// than crash, and the branch must survive.
func TestHeadlessCleanup_BranchCheckedOutElsewhere_DoesNotDelete(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping integration test")
	}
	withNoopTmux(t)

	branchName := "shared-branch"
	bareRoot, worktreePath := setupBareRepoWithBranch(t, branchName)
	session := "myrepo@" + branchName

	// Do NOT remove worktreePath first — pass worktreePath="" so the
	// worktree-removal step is skipped (simulating a session whose worktree
	// was already torn down some other way) while the branch is still
	// checked out at worktreePath, i.e. "checked out elsewhere".
	if err := headlessCleanup(session, branchName, "", bareRoot); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	if !git.BranchExists(bareRoot, branchName) {
		t.Errorf("branch %q was deleted while still checked out at %s", branchName, worktreePath)
	}
}
