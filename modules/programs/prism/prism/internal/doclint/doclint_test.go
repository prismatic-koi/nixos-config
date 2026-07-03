package doclint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsResolve runs the doc-lint against the current source tree.
//
// This is the load-bearing test: every prism-touching PR runs it as part of
// `go test ./...` under both the `go-tests` CI job (full repo checkout) and
// the `nix-build-prism-checked` CI job (nix sandbox — prism subtree only,
// $HOME unwritable, no repo-root AGENTS.md).
//
// If this test fails, either fix the stale identifier in the doc or, if the
// token is intentionally unresolvable (a hypothetical field in a
// walkthrough, a to-be-added constant, etc.), add a doclint-ignore
// annotation to the doc — see ../../docs/doclint.md for the annotation
// syntax.
func TestDocsResolve(t *testing.T) {
	prismRoot, repoRoot, err := LocateRoots()
	if err != nil {
		t.Fatalf("LocateRoots: %v", err)
	}
	findings, err := Scan(prismRoot, repoRoot)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("doclint: unresolved backticked identifiers in docs (fix the doc, rename the identifier, or add a doclint-ignore annotation — see docs/doclint.md):\n")
	for _, f := range findings {
		b.WriteString("  ")
		b.WriteString(f.String())
		b.WriteByte('\n')
	}
	t.Fatalf("%s", b.String())
}

// TestLocateRoots_PrismRootHasGoMod is a sanity check that our runtime.Caller
// arithmetic lands on the prism source root.
func TestLocateRoots_PrismRootHasGoMod(t *testing.T) {
	prismRoot, _, err := LocateRoots()
	if err != nil {
		t.Fatalf("LocateRoots: %v", err)
	}
	if _, err := os.Stat(filepath.Join(prismRoot, "go.mod")); err != nil {
		t.Fatalf("prism root %q should contain go.mod: %v", prismRoot, err)
	}
	if _, err := os.Stat(filepath.Join(prismRoot, "docs")); err != nil {
		t.Fatalf("prism root %q should contain docs/: %v", prismRoot, err)
	}
}
