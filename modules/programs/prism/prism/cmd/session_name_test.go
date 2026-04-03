package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/session"
)

// initGitRepo creates a minimal git repo in dir with one commit on branchName.
// Returns the short hash of the commit.
func initGitRepo(t *testing.T, dir, branchName string) string {
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
	// Create a commit so HEAD resolves.
	readmeFile := filepath.Join(dir, "README")
	if err := os.WriteFile(readmeFile, []byte("test\n"), 0o644); err != nil {
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
	// Return short hash.
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse --short HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// detachHEAD puts a repo into detached HEAD state.
func detachHead(t *testing.T, dir string) {
	t.Helper()
	c := exec.Command("git", "checkout", "--detach")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git checkout --detach: %v\n%s", err, out)
	}
}

// ── worktreeBranchComponent (via NameFor with a project root) ─────────────────

func TestWorktreeBranchComponent_SimpleBranch(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, "main")
	projectRoot := filepath.Join(filepath.Dir(dir), "myrepo")
	got := session.NameFor(dir, projectRoot)
	if got != "myrepo@main" {
		t.Errorf("NameFor = %q, want %q", got, "myrepo@main")
	}
}

func TestWorktreeBranchComponent_SlashInBranch(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, "feat/add-foo")
	projectRoot := filepath.Join(filepath.Dir(dir), "myrepo")
	got := session.NameFor(dir, projectRoot)
	if got != "myrepo@feat--add-foo" {
		t.Errorf("NameFor = %q, want %q", got, "myrepo@feat--add-foo")
	}
}

func TestWorktreeBranchComponent_MultiSlashBranch(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, "users/alice/fix-login")
	projectRoot := filepath.Join(filepath.Dir(dir), "myrepo")
	got := session.NameFor(dir, projectRoot)
	if got != "myrepo@users--alice--fix-login" {
		t.Errorf("NameFor = %q, want %q", got, "myrepo@users--alice--fix-login")
	}
}

func TestWorktreeBranchComponent_DetachedHead(t *testing.T) {
	dir := t.TempDir()
	shortHash := initGitRepo(t, dir, "main")
	detachHead(t, dir)
	projectRoot := filepath.Join(filepath.Dir(dir), "myrepo")
	got := session.NameFor(dir, projectRoot)
	if got != "myrepo@"+shortHash {
		t.Errorf("NameFor (detached HEAD) = %q, want myrepo@%q", got, shortHash)
	}
}

func TestWorktreeBranchComponent_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	projectRoot := filepath.Join(filepath.Dir(dir), "myrepo")
	got := session.NameFor(dir, projectRoot)
	want := "myrepo@" + filepath.Base(dir)
	if got != want {
		t.Errorf("NameFor (non-git) = %q, want %q", got, want)
	}
}

// ── sessionNameFor ───────────────────────────────────────────────────────────

func TestSessionNameFor_WithProjectRoot_SlashBranch(t *testing.T) {
	dir := t.TempDir()
	projectRoot := filepath.Join(dir, "nixos-config")
	worktree := filepath.Join(projectRoot, "feat/add-foo") // nested path, but worktree dir itself
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initGitRepo(t, worktree, "feat/add-foo")
	got := session.NameFor(worktree, projectRoot)
	if got != "nixos-config@feat--add-foo" {
		t.Errorf("NameFor = %q, want %q", got, "nixos-config@feat--add-foo")
	}
}

func TestSessionNameFor_WithoutProjectRoot(t *testing.T) {
	// No project root — falls straight through to filepath.Base path.
	got := session.NameFor("/home/user/obsidian", "")
	if got != "obsidian" {
		t.Errorf("NameFor = %q, want %q", got, "obsidian")
	}
}

func TestSessionNameFor_DotInProjectRoot(t *testing.T) {
	dir := t.TempDir()
	projectRoot := filepath.Join(dir, "my.repo")
	worktree := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	initGitRepo(t, worktree, "main")
	got := session.NameFor(worktree, projectRoot)
	// Dots in project name are replaced with underscores.
	if got != "my_repo@main" {
		t.Errorf("NameFor = %q, want %q", got, "my_repo@main")
	}
}
