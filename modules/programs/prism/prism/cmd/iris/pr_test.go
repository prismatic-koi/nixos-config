package main

// pr_test.go — unit tests for the small helpers in pr.go that don't need a
// live socket / git repo. The end-to-end wire path is exercised by
// pr_integration_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolvePRBareRoot_RepoFlagAbsolute asserts that an absolute path is
// accepted when it points at a prism bare repo.
func TestResolvePRBareRoot_RepoFlagAbsolute(t *testing.T) {
	tmp := t.TempDir()
	bareRoot := filepath.Join(tmp, "myrepo")
	if err := os.MkdirAll(filepath.Join(bareRoot, ".bare"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	got, err := resolvePRBareRoot(bareRoot, "", "")
	if err != nil {
		t.Fatalf("resolvePRBareRoot: %v", err)
	}
	if got != bareRoot {
		t.Errorf("got %q, want %q", got, bareRoot)
	}
}

// TestResolvePRBareRoot_RepoFlagShorthand asserts that a shorthand name is
// resolved under ~/code (with $HOME pointed at a tempdir).
func TestResolvePRBareRoot_RepoFlagShorthand(t *testing.T) {
	tmp := t.TempDir()
	bareRoot := filepath.Join(tmp, "code", "myrepo")
	if err := os.MkdirAll(filepath.Join(bareRoot, ".bare"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	got, err := resolvePRBareRoot("myrepo", tmp, "")
	if err != nil {
		t.Fatalf("resolvePRBareRoot: %v", err)
	}
	if got != bareRoot {
		t.Errorf("got %q, want %q", got, bareRoot)
	}
}

// TestResolvePRBareRoot_RepoFlagTildeExpansion asserts that "~/code/X" expands
// using the supplied home dir override (test-friendly hook).
func TestResolvePRBareRoot_RepoFlagTildeExpansion(t *testing.T) {
	tmp := t.TempDir()
	bareRoot := filepath.Join(tmp, "code", "myrepo")
	if err := os.MkdirAll(filepath.Join(bareRoot, ".bare"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	got, err := resolvePRBareRoot("~/code/myrepo", tmp, "")
	if err != nil {
		t.Fatalf("resolvePRBareRoot: %v", err)
	}
	if got != bareRoot {
		t.Errorf("got %q, want %q", got, bareRoot)
	}
}

// TestResolvePRBareRoot_AbsoluteNonBareRepoRejected asserts that an absolute
// path without a .bare/ entry is rejected with a clear error.
func TestResolvePRBareRoot_AbsoluteNonBareRepoRejected(t *testing.T) {
	tmp := t.TempDir() // No .bare/ inside.
	_, err := resolvePRBareRoot(tmp, "", "")
	if err == nil {
		t.Fatalf("want error for non-bare repo, got nil")
	}
	if !strings.Contains(err.Error(), "not a prism bare repo") {
		t.Errorf("error missing 'not a prism bare repo' wording: %q", err.Error())
	}
}

// TestResolvePRBareRoot_ShorthandNotFound asserts that a shorthand that does
// not exist under ~/code returns a helpful error message.
func TestResolvePRBareRoot_ShorthandNotFound(t *testing.T) {
	tmp := t.TempDir()
	_, err := resolvePRBareRoot("does-not-exist", tmp, "")
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found under ~/code") {
		t.Errorf("error missing '~/code' hint: %q", err.Error())
	}
}

// TestResolvePRBareRoot_CWDInsideWorktree asserts that the bare-root walk
// finds the parent directory when cwd is a worktree inside a bare layout.
func TestResolvePRBareRoot_CWDInsideWorktree(t *testing.T) {
	tmp := t.TempDir()
	bareRoot := filepath.Join(tmp, "myrepo")
	worktree := filepath.Join(bareRoot, "feature")
	if err := os.MkdirAll(filepath.Join(bareRoot, ".bare"), 0o755); err != nil {
		t.Fatalf("MkdirAll .bare: %v", err)
	}
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}

	got, err := resolvePRBareRoot("", "", worktree)
	if err != nil {
		t.Fatalf("resolvePRBareRoot: %v", err)
	}
	if got != bareRoot {
		t.Errorf("got %q, want %q", got, bareRoot)
	}
}

// TestResolvePRBareRoot_CWDNotInRepo asserts that a cwd outside any prism
// bare repo produces a clear "pass --repo" hint.
func TestResolvePRBareRoot_CWDNotInRepo(t *testing.T) {
	tmp := t.TempDir() // no .bare/ anywhere in this tree
	_, err := resolvePRBareRoot("", "", tmp)
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("error missing '--repo' hint: %q", err.Error())
	}
}

// TestSamePath_Identical asserts that two distinct strings resolving to the
// same absolute path compare equal.
func TestSamePath_Identical(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "x")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Same path, different forms.
	b := filepath.Join(tmp, "y", "..", "x")
	same, err := samePath(a, b)
	if err != nil {
		t.Fatalf("samePath: %v", err)
	}
	if !same {
		t.Errorf("expected samePath(%q, %q) = true", a, b)
	}
}

// TestSamePath_Different asserts that two distinct paths compare unequal.
func TestSamePath_Different(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "x")
	b := filepath.Join(tmp, "y")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatalf("MkdirAll a: %v", err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatalf("MkdirAll b: %v", err)
	}
	same, err := samePath(a, b)
	if err != nil {
		t.Fatalf("samePath: %v", err)
	}
	if same {
		t.Errorf("expected samePath(%q, %q) = false", a, b)
	}
}
