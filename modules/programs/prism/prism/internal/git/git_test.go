package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a minimal git repo at dir on branchName with one commit.
func initRepo(t *testing.T, dir, branchName string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init", "-b", branchName},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args[1:], err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "init"},
	} {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args[1:], err, out)
		}
	}
}

func TestSymbolicRef_SimpleBranch(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir, "main")
	ref, err := SymbolicRef(dir)
	if err != nil {
		t.Fatalf("SymbolicRef: %v", err)
	}
	if ref != "refs/heads/main" {
		t.Errorf("SymbolicRef = %q, want %q", ref, "refs/heads/main")
	}
}

func TestSymbolicRef_SlashBranch(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir, "feat/add-foo")
	ref, err := SymbolicRef(dir)
	if err != nil {
		t.Fatalf("SymbolicRef: %v", err)
	}
	if ref != "refs/heads/feat/add-foo" {
		t.Errorf("SymbolicRef = %q, want %q", ref, "refs/heads/feat/add-foo")
	}
}

func TestSymbolicRef_DetachedHead(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir, "main")
	// Detach HEAD.
	c := exec.Command("git", "checkout", "--detach")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git checkout --detach: %v\n%s", err, out)
	}
	_, err := SymbolicRef(dir)
	if err == nil {
		t.Error("SymbolicRef on detached HEAD should return an error")
	}
}

func TestSymbolicRef_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	_, err := SymbolicRef(dir)
	if err == nil {
		t.Error("SymbolicRef in non-git dir should return an error")
	}
}

func TestShortHash_ReturnsHash(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir, "main")
	hash, err := ShortHash(dir)
	if err != nil {
		t.Fatalf("ShortHash: %v", err)
	}
	if hash == "" {
		t.Error("ShortHash returned empty string")
	}
	// Short hashes are typically 7 hex chars (can vary by repo size, but always hex).
	for _, r := range hash {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("ShortHash %q contains non-hex character %q", hash, string(r))
		}
	}
}

func TestShortHash_DetachedHead(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir, "main")
	// Get hash before detaching.
	attached, err := ShortHash(dir)
	if err != nil {
		t.Fatalf("ShortHash (attached): %v", err)
	}
	// Detach.
	c := exec.Command("git", "checkout", "--detach")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git checkout --detach: %v\n%s", err, out)
	}
	detached, err := ShortHash(dir)
	if err != nil {
		t.Fatalf("ShortHash (detached): %v", err)
	}
	if detached != attached {
		t.Errorf("ShortHash mismatch: attached=%q detached=%q", attached, detached)
	}
}

func TestShortHash_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	_, err := ShortHash(dir)
	if err == nil {
		t.Error("ShortHash in non-git dir should return an error")
	}
}

// initRepoWithRemote creates a regular git repo at dir with one commit on
// branchName and a bare repo at remoteDir configured as "origin". The caller
// is responsible for creating both directories via t.TempDir().
func initRepoWithRemote(t *testing.T, dir, remoteDir, branchName string) {
	t.Helper()

	// Create the working repo with a commit.
	initRepo(t, dir, branchName)

	// Create a bare clone to act as origin.
	if out, err := exec.Command("git", "clone", "--bare", dir, remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("bare clone for remote: %v\n%s", err, out)
	}

	// Point the working repo's origin at the bare clone.
	run := func(args ...string) {
		t.Helper()
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "remote", "add", "origin", remoteDir)
	run("git", "fetch", "origin")
	run("git", "branch", "--set-upstream-to", "origin/"+branchName, branchName)
}

// runGitIn runs a git command in dir and returns trimmed stdout, or fatals.
func runGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestConvertToBare(t *testing.T) {
	dir := t.TempDir()
	remoteDir := t.TempDir()
	initRepoWithRemote(t, dir, remoteDir, "main")

	var progress []string
	worktreePath, err := ConvertToBare(dir, func(msg string) { progress = append(progress, msg) })
	if err != nil {
		t.Fatalf("ConvertToBare: %v", err)
	}
	if worktreePath == "" {
		t.Fatal("ConvertToBare returned empty worktree path")
	}

	// worktree path must exist as a directory.
	if info, err := os.Stat(worktreePath); err != nil || !info.IsDir() {
		t.Fatalf("worktree path %s does not exist or is not a directory: %v", worktreePath, err)
	}

	barePath := filepath.Join(dir, ".bare")

	// git worktree list --porcelain must list exactly one worktree entry whose
	// worktree field is the path returned by ConvertToBare.
	wtList := runGitIn(t, dir, "--git-dir", barePath, "worktree", "list", "--porcelain")
	var worktreePaths []string
	// Identify bare entries by the "bare" attribute line, matching the
	// production Worktrees() logic. This is immune to symlink resolution
	// differences (e.g. /var → /private/var on macOS).
	var current string
	isBare := false
	for _, line := range strings.Split(wtList, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			if current != "" && !isBare {
				worktreePaths = append(worktreePaths, current)
			}
			current = strings.TrimPrefix(line, "worktree ")
			isBare = false
		} else if strings.TrimSpace(line) == "bare" {
			isBare = true
		}
	}
	if current != "" && !isBare {
		worktreePaths = append(worktreePaths, current)
	}
	if len(worktreePaths) != 1 {
		t.Fatalf("expected exactly 1 non-bare worktree, got %d: %v\nworktree list output:\n%s",
			len(worktreePaths), worktreePaths, wtList)
	}
	// Use os.SameFile for the path equality check so that symlink differences
	// (e.g. /var vs /private/var on macOS) do not cause a false mismatch.
	info0, err0 := os.Stat(worktreePaths[0])
	infoWt, errWt := os.Stat(worktreePath)
	if err0 != nil || errWt != nil || !os.SameFile(info0, infoWt) {
		t.Errorf("worktree list path = %q, want %q (same file)", worktreePaths[0], worktreePath)
	}

	// git status in worktree must exit 0 and report nothing to commit.
	statusOut := runGitIn(t, worktreePath, "status")
	if !strings.Contains(statusOut, "nothing to commit") {
		t.Errorf("git status output does not contain 'nothing to commit':\n%s", statusOut)
	}

	// git log in worktree must return a non-empty line with "init".
	logOut := runGitIn(t, worktreePath, "log", "--oneline", "-1")
	if logOut == "" {
		t.Error("git log --oneline -1 returned empty output")
	}
	if !strings.Contains(logOut, "init") {
		t.Errorf("git log --oneline -1 = %q, want it to contain 'init'", logOut)
	}

	// worktree/.git must exist and begin with "gitdir:".
	worktreeGitFile := filepath.Join(worktreePath, ".git")
	gitFileContent, err := os.ReadFile(worktreeGitFile)
	if err != nil {
		t.Fatalf("read worktree .git: %v", err)
	}
	if !strings.HasPrefix(string(gitFileContent), "gitdir:") {
		t.Errorf("worktree .git content = %q, want prefix 'gitdir:'", string(gitFileContent))
	}

	// .bare/worktrees/<branch>/ must contain all four registration files.
	worktreesDir := filepath.Join(barePath, "worktrees", "main")
	for _, name := range []string{"gitdir", "commondir", "HEAD", "index"} {
		p := filepath.Join(worktreesDir, name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("registration file %s missing: %v", p, err)
		}
	}
}

func TestConvertToBare_SlashBranch(t *testing.T) {
	dir := t.TempDir()
	remoteDir := t.TempDir()
	branchName := "feat/foo"
	initRepoWithRemote(t, dir, remoteDir, branchName)

	worktreePath, err := ConvertToBare(dir, func(string) {})
	if err != nil {
		t.Fatalf("ConvertToBare: %v", err)
	}

	// The returned path must end with the full branch name (slash preserved).
	expectedPath := filepath.Join(dir, branchName)
	if worktreePath != expectedPath {
		t.Errorf("worktreePath = %q, want %q", worktreePath, expectedPath)
	}

	// Git uses the last path component as the worktrees entry name (matching
	// the behaviour of `git worktree add` with slash branch names).
	// i.e. "feat/foo" → worktrees/foo
	barePath := filepath.Join(dir, ".bare")
	worktreeEntryName := filepath.Base(branchName) // "foo"
	worktreesDir := filepath.Join(barePath, "worktrees", worktreeEntryName)
	for _, name := range []string{"gitdir", "commondir", "HEAD", "index"} {
		p := filepath.Join(worktreesDir, name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("registration file %s missing: %v", p, err)
		}
	}

	// git status must show nothing to commit.
	statusOut := runGitIn(t, worktreePath, "status")
	if !strings.Contains(statusOut, "nothing to commit") {
		t.Errorf("git status does not contain 'nothing to commit':\n%s", statusOut)
	}
}

func TestConvertToBare_PreexistingFiles(t *testing.T) {
	dir := t.TempDir()
	remoteDir := t.TempDir()
	initRepoWithRemote(t, dir, remoteDir, "main")

	// Add extra files and a subdirectory.
	extraFile := filepath.Join(dir, "extra.txt")
	if err := os.WriteFile(extraFile, []byte("extra\n"), 0o644); err != nil {
		t.Fatalf("write extra.txt: %v", err)
	}
	subDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatalf("write nested.txt: %v", err)
	}

	worktreePath, err := ConvertToBare(dir, func(string) {})
	if err != nil {
		t.Fatalf("ConvertToBare: %v", err)
	}

	// All extra files must be inside the worktree directory.
	if _, err := os.Stat(filepath.Join(worktreePath, "extra.txt")); err != nil {
		t.Errorf("extra.txt not found inside worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktreePath, "subdir", "nested.txt")); err != nil {
		t.Errorf("subdir/nested.txt not found inside worktree: %v", err)
	}

	// Extra files must not exist at the root any more.
	if _, err := os.Stat(filepath.Join(dir, "extra.txt")); err == nil {
		t.Error("extra.txt still present at repo root after conversion")
	}
	if _, err := os.Stat(filepath.Join(dir, "subdir")); err == nil {
		t.Error("subdir still present at repo root after conversion")
	}
}

func TestConvertToBare_NoRemote(t *testing.T) {
	dir := t.TempDir()
	// initRepo creates a repo with no remote.
	initRepo(t, dir, "main")

	// Capture the original .git directory content to verify rollback.
	origGitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(origGitDir); err != nil {
		t.Fatalf(".git not present before conversion: %v", err)
	}

	_, err := ConvertToBare(dir, func(string) {})
	if err == nil {
		t.Fatal("ConvertToBare on repo with no remote should return an error")
	}

	// .bare/ must not exist (no partial state left behind).
	if _, err := os.Stat(filepath.Join(dir, ".bare")); err == nil {
		t.Error(".bare directory exists after failed conversion — partial state not cleaned up")
	}

	// .git must be restored as a directory (rollback).
	gitInfo, err := os.Stat(origGitDir)
	if err != nil {
		t.Fatalf(".git missing after rollback: %v", err)
	}
	if !gitInfo.IsDir() {
		t.Errorf(".git is not a directory after rollback (got %v)", gitInfo.Mode())
	}
}

// TestStat_EmptyDir verifies that Stat returns an error for an empty string path.
func TestStat_EmptyDir(t *testing.T) {
	t.Parallel()
	_, err := Stat("")
	if err == nil {
		t.Error("Stat(\"\") should return an error")
	}
}

// TestStat_NonExistentPath verifies that Stat returns an error for a path that
// does not exist on disk.
func TestStat_NonExistentPath(t *testing.T) {
	t.Parallel()
	_, err := Stat("/nonexistent/path/that/should/not/exist")
	if err == nil {
		t.Error("Stat on non-existent path should return an error")
	}
}

// TestStat_NonGitDir verifies that Stat returns an error for a directory that
// exists but is not a git repository.
func TestStat_NonGitDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Stat(dir)
	if err == nil {
		t.Error("Stat on non-git directory should return an error")
	}
}

// TestStat_CleanRepo verifies that Stat returns a zero DiffStat (no error) for
// a clean git repository with no uncommitted changes.
func TestStat_CleanRepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepo(t, dir, "main")
	stat, err := Stat(dir)
	if err != nil {
		t.Fatalf("Stat on clean repo returned error: %v", err)
	}
	if stat.Files != 0 || stat.Insertions != 0 || stat.Deletions != 0 {
		t.Errorf("Stat on clean repo = %+v, want zero DiffStat", stat)
	}
}

// TestStat_DirtyRepo verifies that Stat returns a non-zero DiffStat for a
// repository with uncommitted changes.
func TestStat_DirtyRepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepo(t, dir, "main")

	// Modify the existing README file — unstaged change.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	stat, err := Stat(dir)
	if err != nil {
		t.Fatalf("Stat on dirty repo returned error: %v", err)
	}
	if stat.Files == 0 {
		t.Error("Stat on dirty repo returned zero files, want > 0")
	}
}

// TestStat_StagedChanges verifies that Stat captures staged (cached) changes.
func TestStat_StagedChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepo(t, dir, "main")

	// Add a new file and stage it.
	newFile := filepath.Join(dir, "newfile.txt")
	if err := os.WriteFile(newFile, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("write newfile.txt: %v", err)
	}
	c := exec.Command("git", "add", "newfile.txt")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	stat, err := Stat(dir)
	if err != nil {
		t.Fatalf("Stat with staged changes returned error: %v", err)
	}
	if stat.Files == 0 {
		t.Error("Stat with staged changes returned zero files, want > 0")
	}
	if stat.Insertions == 0 {
		t.Error("Stat with staged changes returned zero insertions, want > 0")
	}
}

// TestStat_DeduplicatesStagedAndUnstaged verifies that a file that appears in
// both staged and unstaged diff is counted only once.
func TestStat_DeduplicatesStagedAndUnstaged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepo(t, dir, "main")

	// Modify README and stage part of it (stage then modify again so it
	// appears in both diff HEAD and diff --cached).
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("staged change\n"), 0o644); err != nil {
		t.Fatalf("write README (staged): %v", err)
	}
	c := exec.Command("git", "add", "README")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add README: %v\n%s", err, out)
	}
	// Modify again so it also appears in unstaged diff.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("staged change\nunstaged change\n"), 0o644); err != nil {
		t.Fatalf("write README (unstaged): %v", err)
	}

	stat, err := Stat(dir)
	if err != nil {
		t.Fatalf("Stat returned error: %v", err)
	}
	// README appears in both diffs but must be counted as exactly 1 file.
	if stat.Files != 1 {
		t.Errorf("Stat.Files = %d, want 1 (README should be deduplicated)", stat.Files)
	}
}

// initEmptyBareRepo creates an empty bare git repo at dir with HEAD pointing
// at defaultBranch. No commits are made.
func initEmptyBareRepo(t *testing.T, dir, defaultBranch string) {
	t.Helper()
	if out, err := exec.Command("git", "init", "--bare", "-b", defaultBranch, dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
}

// TestCloneWorktree_EmptyRepo verifies that CloneWorktree succeeds on a remote
// repository that has no commits, creating an orphan worktree.
func TestCloneWorktree_EmptyRepo(t *testing.T) {
	remoteDir := t.TempDir()
	targetDir := t.TempDir()

	initEmptyBareRepo(t, remoteDir, "main")

	var progressMsgs []string
	err := CloneWorktree(remoteDir, targetDir, func(msg string) {
		progressMsgs = append(progressMsgs, msg)
	})
	if err != nil {
		t.Fatalf("CloneWorktree on empty repo: %v", err)
	}

	// .bare/ must exist as a directory.
	bareDir := filepath.Join(targetDir, ".bare")
	if info, err := os.Stat(bareDir); err != nil || !info.IsDir() {
		t.Fatalf(".bare/ does not exist or is not a directory: %v", err)
	}

	// Orphan worktree dir must exist as a directory.
	worktreeDir := filepath.Join(targetDir, "main")
	if info, err := os.Stat(worktreeDir); err != nil || !info.IsDir() {
		t.Fatalf("worktree dir %s does not exist or is not a directory: %v", worktreeDir, err)
	}

	// At least one progress message must mention "empty" or "orphan".
	foundOrphanMsg := false
	for _, msg := range progressMsgs {
		if strings.Contains(msg, "empty") || strings.Contains(msg, "orphan") {
			foundOrphanMsg = true
			break
		}
	}
	if !foundOrphanMsg {
		t.Errorf("no progress message about empty/orphan repo; got: %v", progressMsgs)
	}

	// At least one progress message must include bootstrap instructions
	// (push + branch name).
	foundBootstrapMsg := false
	for _, msg := range progressMsgs {
		if strings.Contains(msg, "push") && strings.Contains(msg, "main") {
			foundBootstrapMsg = true
			break
		}
	}
	if !foundBootstrapMsg {
		t.Errorf("no bootstrap instructions in progress messages; got: %v", progressMsgs)
	}

	// User must be able to make a commit in the orphan worktree.
	runGitIn(t, worktreeDir, "config", "user.email", "test@test.com")
	runGitIn(t, worktreeDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(worktreeDir, "README"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGitIn(t, worktreeDir, "add", ".")
	runGitIn(t, worktreeDir, "commit", "-m", "initial commit")

	logOut := runGitIn(t, worktreeDir, "log", "--oneline", "-1")
	if !strings.Contains(logOut, "initial commit") {
		t.Errorf("git log in orphan worktree = %q, want 'initial commit'", logOut)
	}
}

// TestCloneWorktree_NonEmptyRepo verifies that CloneWorktree succeeds on a
// remote repository that already has commits, preserving upstream tracking and
// using the standard (non-orphan) worktree layout.
func TestCloneWorktree_NonEmptyRepo(t *testing.T) {
	remoteDir := t.TempDir()
	targetDir := t.TempDir()

	// Create a non-empty bare remote: clone a repo with one commit.
	srcDir := t.TempDir()
	initRepo(t, srcDir, "main")
	if out, err := exec.Command("git", "clone", "--bare", srcDir, remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("bare clone for remote: %v\n%s", err, out)
	}

	var progressMsgs []string
	err := CloneWorktree(remoteDir, targetDir, func(msg string) {
		progressMsgs = append(progressMsgs, msg)
	})
	if err != nil {
		t.Fatalf("CloneWorktree on non-empty repo: %v", err)
	}

	// .bare/ must exist as a directory.
	bareDir := filepath.Join(targetDir, ".bare")
	if info, err := os.Stat(bareDir); err != nil || !info.IsDir() {
		t.Fatalf(".bare/ does not exist or is not a directory: %v", err)
	}

	// Worktree dir must exist as a directory.
	worktreeDir := filepath.Join(targetDir, "main")
	if info, err := os.Stat(worktreeDir); err != nil || !info.IsDir() {
		t.Fatalf("worktree dir %s does not exist or is not a directory: %v", worktreeDir, err)
	}

	// git status must report nothing to commit (clean worktree).
	statusOut := runGitIn(t, worktreeDir, "status")
	if !strings.Contains(statusOut, "nothing to commit") {
		t.Errorf("git status does not contain 'nothing to commit':\n%s", statusOut)
	}

	// Upstream tracking must be configured: git status should mention origin/main.
	if !strings.Contains(statusOut, "origin/main") {
		t.Errorf("git status does not mention 'origin/main' (upstream not tracked?):\n%s", statusOut)
	}

	// No empty-repo or orphan messages must appear for a non-empty repo.
	for _, msg := range progressMsgs {
		if strings.Contains(msg, "empty") || strings.Contains(msg, "orphan") {
			t.Errorf("unexpected empty/orphan message for non-empty repo: %q", msg)
		}
	}
}
