// Package doclint verifies that backticked identifier-shaped tokens in
// prism's markdown docs resolve against the current source tree.
//
// See ../../docs/doclint.md for the operational spec: what token classes
// the lint recognises, what resolution rules it applies, and how to
// annotate an intentionally-unresolvable token.
//
// The lint is enforced by TestDocsResolve in doclint_test.go, which runs
// as part of `go test ./...` from modules/programs/prism/prism/ and
// therefore inherits the existing pr-gate enforcement (both the
// `go-tests` CI job and the homeless-shelter `nix-build-prism-checked`
// job execute this test).
//
// Dual-context constraint. This test runs in two environments:
//
//  1. A full repo checkout (CI's `go-tests` job, local dev) — both
//     `modules/programs/prism/prism/docs/*.md` and the repo-root
//     `AGENTS.md` are present. The lint scans both.
//  2. The nix sandbox (`runChecks = true`) — only the prism subtree is
//     copied in. The repo-root `AGENTS.md` does not exist and $HOME is
//     unwritable. The lint skips absent doc files gracefully and never
//     touches the network or $HOME.
//
// Scan roots are located relative to this package's source file via
// runtime.Caller, never via $HOME or the current working directory.
package doclint

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Finding records a single unresolved backticked token.
type Finding struct {
	File  string // absolute path
	Line  int    // 1-based
	Token string // the backticked text, stripped of trailing punctuation
	Rule  string // resolution rule that was attempted
	Note  string // human-readable diagnostic
}

// String formats a Finding for test output.
func (f Finding) String() string {
	rel := f.File
	if abs, err := filepath.Abs(f.File); err == nil {
		rel = abs
	}
	return fmt.Sprintf("%s:%d: unresolved `%s` (rule=%s): %s", rel, f.Line, f.Token, f.Rule, f.Note)
}

// Scan performs the doclint scan against the given roots.
//
// prismSourceRoot must point at modules/programs/prism/prism/ (the directory
// that contains cmd/, internal/, docs/, main.go, go.mod).
//
// repoRoot points at the repo root (the directory that contains the top-level
// AGENTS.md). Pass "" if the repo root is not available (e.g. the nix sandbox
// build where only the prism subtree is present) — the top-level AGENTS.md
// scan is skipped in that case.
func Scan(prismSourceRoot, repoRoot string) ([]Finding, error) {
	idx, err := buildSourceIndexWithRepoRoot(prismSourceRoot, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("build source index: %w", err)
	}

	docs, err := discoverDocs(prismSourceRoot, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("discover docs: %w", err)
	}

	var findings []Finding
	for _, doc := range docs {
		fs, err := scanDoc(doc, idx)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", doc, err)
		}
		findings = append(findings, fs...)
	}

	// Deterministic ordering by file, line, token.
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Token < findings[j].Token
	})
	return findings, nil
}

// LocateRoots returns absolute paths to (prismSourceRoot, repoRoot) using
// runtime.Caller to locate this package's source file at test time.
// repoRoot is returned as "" when the repo root cannot be verified (e.g.
// inside the nix sandbox where only the prism subtree is copied in).
//
// This deliberately does not consult $HOME, PWD, or any env var — the
// nix-sandbox homeless-shelter build must succeed here.
func LocateRoots() (prismSourceRoot, repoRoot string, err error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", "", fmt.Errorf("runtime.Caller: could not determine source file location")
	}
	// thisFile = <prismSourceRoot>/internal/doclint/doclint.go
	// So prismSourceRoot = filepath.Dir x 3.
	prismSourceRoot = filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	if _, err := os.Stat(filepath.Join(prismSourceRoot, "go.mod")); err != nil {
		return "", "", fmt.Errorf("prism source root %q missing go.mod: %w", prismSourceRoot, err)
	}
	// Repo root sits four levels up from the prism source root:
	// modules/programs/prism/prism/ -> modules/programs/prism/ -> modules/programs/ -> modules/ -> <repo>
	candidate := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(prismSourceRoot))))
	agentsMd := filepath.Join(candidate, "AGENTS.md")
	// Any stat error — NotExist, permission-denied, or unusable path —
	// means the repo-root AGENTS.md is not reachable and the caller
	// should skip that scan target. That is the expected shape inside
	// the nix sandbox (only the prism subtree is copied in) and we do
	// not want to fail the test just because we could not stat a file
	// four directories above the package source.
	if _, err := os.Stat(agentsMd); err == nil {
		repoRoot = candidate
	}
	return prismSourceRoot, repoRoot, nil
}

// discoverDocs returns the list of markdown files to scan. Files that do not
// exist are silently omitted (see the dual-context constraint in the package
// doc comment).
func discoverDocs(prismSourceRoot, repoRoot string) ([]string, error) {
	var out []string

	// modules/programs/prism/prism/docs/*.md
	docDir := filepath.Join(prismSourceRoot, "docs")
	entries, err := os.ReadDir(docDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, filepath.Join(docDir, e.Name()))
	}

	// Repo-root AGENTS.md (only if repoRoot is known and readable).
	if repoRoot != "" {
		p := filepath.Join(repoRoot, "AGENTS.md")
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}

	sort.Strings(out)
	return out, nil
}
