package doclint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// relocatePrismSubtree copies the REAL prism source subtree (docs and Go
// source, exactly what LocateRoots resolves to) into a fresh temp location
// whose four-levels-up ancestor holds no AGENTS.md. This is the working
// reproduction of the nix homeless-shelter build called out in issue #2679:
// only then does findRepoRootForDoc return "" for real.
//
// A naive Scan(prismRoot, "") does NOT reproduce the sandbox. scanDoc
// ignores the repoRoot passed to Scan and re-derives the repo root per doc
// via findRepoRootForDoc, which walks up from the doc to the ancestor that
// holds AGENTS.md. On any developer worktree that always finds the real
// repo root four levels above modules/programs/prism/prism, so out-of-subtree
// paths always resolve and a test built on that form passes forever and
// catches nothing. Relocating the tree removes that AGENTS.md ancestor.
func relocatePrismSubtree(t *testing.T) (prismRoot, dest string) {
	t.Helper()
	real, _, err := LocateRoots()
	if err != nil {
		t.Fatalf("LocateRoots: %v", err)
	}
	// Mirror the real depth (modules/programs/prism/prism) so the
	// four-levels-up derivation in findRepoRootForDoc lands on the temp
	// root, which has no AGENTS.md.
	dest = filepath.Join(t.TempDir(), "modules", "programs", "prism", "prism")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(dest, os.DirFS(real)); err != nil {
		t.Fatalf("copy prism subtree: %v", err)
	}
	// Guard: the relocated four-levels-up ancestor must NOT hold an
	// AGENTS.md, otherwise the relocation failed to strip the repo root
	// and the test would silently revert to the vacuous configuration.
	fourUp := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(dest))))
	if _, err := os.Stat(filepath.Join(fourUp, "AGENTS.md")); err == nil {
		t.Fatalf("relocated tree still has an AGENTS.md ancestor at %s; reproduction is vacuous", fourUp)
	}
	return real, dest
}

// TestDocsResolve_NixSandboxConfiguration exercises the doclint scan in the
// exact configuration the nix homeless-shelter checked build produces: the
// prism subtree present, the repo root genuinely absent, so every
// out-of-subtree path reference must be caught. It is the local mirror of
// the nix-build-prism-checked CI job for the doclint scan, so a broken
// out-of-subtree reference fails `go test` instead of only failing CI.
//
// The mid-test guard asserts findRepoRootForDoc returns "" for a relocated
// doc. That assertion is what keeps this test from decaying into the
// vacuous Scan(prismRoot, "") form described in issue #2679.
func TestDocsResolve_NixSandboxConfiguration(t *testing.T) {
	_, dest := relocatePrismSubtree(t)

	// Prove we are in the repo-root-absent configuration, not the
	// developer-worktree one. Without this the test could pass while
	// exercising the wrong path.
	if rr := findRepoRootForDoc(filepath.Join(dest, "docs", "doclint.md"), dest); rr != "" {
		t.Fatalf("expected findRepoRootForDoc to return \"\" in the relocated tree, got %q", rr)
	}

	findings, err := Scan(dest, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		var b strings.Builder
		b.WriteString("doclint: unresolved references under the nix-sandbox configuration ")
		b.WriteString("(repo root absent). An out-of-subtree path reference resolves on a ")
		b.WriteString("developer worktree but not in the nix build. Remove the backticks, or ")
		b.WriteString("add a doclint-ignore annotation — see docs/doclint.md:\n")
		for _, f := range findings {
			b.WriteString("  ")
			b.WriteString(f.String())
			b.WriteByte('\n')
		}
		t.Fatalf("%s", b.String())
	}
}

// TestDocsResolve_NixSandboxConfiguration_DetectsOutOfSubtreePath is the
// non-vacuous guard for TestDocsResolve_NixSandboxConfiguration. It injects
// a backticked out-of-subtree path (modules/programs/prism/tmux.nix) into a
// scanned doc in the RELOCATED copy and asserts the sandbox scan reports it.
// The injection touches only the disposable temp copy, never a tracked doc.
//
// This encodes issue #2679's demonstration requirement permanently: if a
// future refactor made the scan blind to out-of-subtree paths again, this
// test would go green with the injection present and fail here instead.
func TestDocsResolve_NixSandboxConfiguration_DetectsOutOfSubtreePath(t *testing.T) {
	_, dest := relocatePrismSubtree(t)

	const badPath = "modules/programs/prism/tmux.nix"
	inj := filepath.Join(dest, "docs", "zz-out-of-subtree-injection.md")
	if err := os.WriteFile(inj, []byte("References `"+badPath+"`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings, err := Scan(dest, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var found bool
	for _, f := range findings {
		if f.Token == badPath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a finding for injected out-of-subtree path %q under the "+
			"nix-sandbox configuration, got %+v", badPath, findings)
	}
}

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
