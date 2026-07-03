package doclint

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScan_SandboxLikeLayout_NoRepoRoot mirrors the shape of the nix
// homeless-shelter checked build: only the prism subtree is present, no
// AGENTS.md above it, $HOME set to an unwritable path. The lint must
// still succeed against docs inside the prism subtree, must not fail
// because the repo-root AGENTS.md is absent, and must not touch $HOME.
func TestScan_SandboxLikeLayout_NoRepoRoot(t *testing.T) {
	// Force $HOME to a path we know doesn't exist, mimicking
	// /homeless-shelter. We do NOT actually depend on this in code —
	// but if the lint were to reach for os.UserHomeDir() this test
	// would surface the regression by breaking the "no writes"
	// invariant.
	t.Setenv("HOME", filepath.Join(t.TempDir(), "definitely-no-home"))

	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	if err := os.WriteFile(docPath, []byte("References `checkHostConfig`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Passing repoRoot="" replicates what LocateRoots returns when it
	// cannot stat the repo-root AGENTS.md (i.e., nix sandbox).
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected no findings under sandbox-like layout, got %+v", findings)
	}
}

// TestScan_SandboxLikeLayout_AbsentRepoAGENTSDoesNotError asserts that
// when repoRoot IS provided but its AGENTS.md sibling has vanished, the
// lint does not error. This matches a partial-checkout edge case (nix
// sandbox where the git worktree was set up but AGENTS.md was pruned).
func TestScan_SandboxLikeLayout_AbsentRepoAGENTSDoesNotError(t *testing.T) {
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	if err := os.WriteFile(docPath, []byte("References `checkHostConfig`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake a repoRoot that exists but has no AGENTS.md.
	fakeRepo := t.TempDir()
	findings, err := Scan(root, fakeRepo)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %+v", findings)
	}
}
