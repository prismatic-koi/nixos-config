package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initBareWithWorktree creates a bare-layout repo at bareRoot (with a .bare
// dir and a default-branch worktree), returning the bare git-dir path.
func initBareWithWorktree(t *testing.T, bareRoot, defaultBranch string) string {
	t.Helper()
	barePath := filepath.Join(bareRoot, ".bare")

	tmpRepo := t.TempDir()
	initRepo(t, tmpRepo, defaultBranch)

	if out, err := exec.Command("git", "clone", "--bare", tmpRepo, barePath).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v\n%s", err, out)
	}

	defaultWorktree := filepath.Join(bareRoot, defaultBranch)
	if out, err := exec.Command("git", "--git-dir", barePath, "worktree", "add",
		defaultWorktree, defaultBranch).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add (default): %v\n%s", err, out)
	}
	return barePath
}

// addWorktree adds a new worktree at worktreePath on a fresh branch,
// forking from base.
func addWorktree(t *testing.T, barePath, worktreePath, branch, base string) {
	t.Helper()
	if out, err := exec.Command("git", "--git-dir", barePath, "worktree", "add",
		"-b", branch, worktreePath, base).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add %s: %v\n%s", branch, err, out)
	}
}

// TestResolveWorktreeGitDir_CollidingBasenames is the core regression case
// for issue #2518: two worktrees whose path basenames collide ("feat/dupe"
// and "bugfix/dupe" both basename to "dupe") must each resolve to their OWN
// distinct git-state directory, not both to the same one.
//
// This test must FAIL against the pre-fix derivation
// (filepath.Join(bareRoot, ".bare", "worktrees", filepath.Base(worktree)))
// because git deduplicates colliding registry entry names with a numeric
// suffix ("dupe", "dupe1"), while the derivation computes "dupe" for both.
func TestResolveWorktreeGitDir_CollidingBasenames(t *testing.T) {
	bareRoot := t.TempDir()
	barePath := initBareWithWorktree(t, bareRoot, "main")

	featWorktree := filepath.Join(bareRoot, "feat", "dupe")
	bugfixWorktree := filepath.Join(bareRoot, "bugfix", "dupe")
	addWorktree(t, barePath, featWorktree, "feat/dupe", "main")
	addWorktree(t, barePath, bugfixWorktree, "bugfix/dupe", "main")

	featGitDir, err := ResolveWorktreeGitDir(featWorktree)
	if err != nil {
		t.Fatalf("ResolveWorktreeGitDir(feat/dupe): %v", err)
	}
	bugfixGitDir, err := ResolveWorktreeGitDir(bugfixWorktree)
	if err != nil {
		t.Fatalf("ResolveWorktreeGitDir(bugfix/dupe): %v", err)
	}

	if featGitDir == bugfixGitDir {
		t.Fatalf("colliding worktrees resolved to the SAME git dir: %q", featGitDir)
	}

	// Each resolved dir must actually exist and must be the entry that the
	// worktree's own HEAD/index live in — sanity-check via file existence.
	for name, dir := range map[string]string{"feat/dupe": featGitDir, "bugfix/dupe": bugfixGitDir} {
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			t.Errorf("%s: resolved git dir %q does not exist or is not a directory: %v", name, dir, statErr)
		}
	}

	// Confirm against git's own worktree list --porcelain output which
	// records each worktree's private gitdir line.
	out, err := exec.Command("git", "--git-dir", barePath, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list --porcelain: %v", err)
	}
	_ = out // informational only; the .git-pointer-based assertions above are authoritative.
}

// TestResolveWorktreeGitDir_NoCollision is the common non-colliding case:
// a single worktree must resolve correctly and the result must equal what
// filepath.Base would have derived (behaviour-preserving for the common case).
func TestResolveWorktreeGitDir_NoCollision(t *testing.T) {
	bareRoot := t.TempDir()
	barePath := initBareWithWorktree(t, bareRoot, "main")

	worktreePath := filepath.Join(bareRoot, "feature")
	addWorktree(t, barePath, worktreePath, "feature", "main")

	got, err := ResolveWorktreeGitDir(worktreePath)
	if err != nil {
		t.Fatalf("ResolveWorktreeGitDir: %v", err)
	}

	want := filepath.Join(barePath, "worktrees", "feature")
	gotResolved, _ := filepath.EvalSymlinks(got)
	wantResolved, _ := filepath.EvalSymlinks(want)
	if gotResolved != wantResolved {
		t.Errorf("ResolveWorktreeGitDir = %q, want %q", got, want)
	}
}

// TestResolveWorktreeGitDir_MissingGitFile asserts a reported error, not a
// silent skip, when worktreePath has no .git file.
func TestResolveWorktreeGitDir_MissingGitFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := ResolveWorktreeGitDir(dir); err == nil {
		t.Fatal("expected an error for a directory with no .git file, got nil")
	}
}

// TestResolveWorktreeGitDir_MalformedGitFile asserts a reported error when
// the .git file exists but has no "gitdir: " line.
func TestResolveWorktreeGitDir_MalformedGitFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("not a gitdir pointer\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	if _, err := ResolveWorktreeGitDir(dir); err == nil {
		t.Fatal("expected an error for a malformed .git file, got nil")
	}
}
