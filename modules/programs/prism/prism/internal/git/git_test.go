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
