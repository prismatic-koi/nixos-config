package review_test

// lifetime_prose_guard_test.go — guards against prose that promises the
// removed session-lifetime contract.
//
// Review-agent sessions are released 15 minutes after their round is
// delivered. They are NOT held until `prism cleanup` of the parent. Prose
// that still promises the old contract is not a style problem. It is a false
// statement about behaviour, sitting in the package that implements the
// current one, where the next reader will believe it.
//
// This guard is mechanical rather than a comment listing the prose sites,
// because the claims appear in Go source as well as documentation, and a
// documentation-scoped grep misses them.
//
// The scan is deliberately narrow. It looks for the specific phrasing that
// asserts the removed lifetime, not for the words "persist" or "cleanup",
// which have many legitimate uses in this package.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// removedLifetimeClaims are substrings that assert the removed contract.
// Matching is case-insensitive and whitespace-normalised, so a claim split
// across two comment lines is still caught.
var removedLifetimeClaims = []string{
	"persist until prism cleanup",
	"persist until cleanup",
	"remain alive until prism cleanup",
	"remain alive until cleanup",
	"until prism cleanup is invoked on the parent",
	"until prism cleanup on the parent",
}

// TestNoProseClaimsSessionsPersistUntilCleanup scans every non-test Go file in
// this package for a statement that review sessions live until parent cleanup.
//
// Scope note: the guard covers `internal/review`, the package that owns the
// release. `prism review --help` is covered separately and mechanically by
// TestReviewHelp_* in package cmd, because that text is a runtime value.
// Markdown sites are covered by review, not by this test.
func TestNoProseClaimsSessionsPersistUntilCleanup(t *testing.T) {
	dir := packageSourceDir(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package source dir %q: %v", dir, err)
	}

	found := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %q: %v", path, readErr)
		}
		found++

		for i, line := range strings.Split(string(data), "\n") {
			// Join each line with the one after it, so a claim wrapped across
			// two comment lines is visible to the scan.
			window := normaliseProse(line)
			lines := strings.Split(string(data), "\n")
			if i+1 < len(lines) {
				window = normaliseProse(line + " " + lines[i+1])
			}
			for _, claim := range removedLifetimeClaims {
				if strings.Contains(window, claim) {
					t.Errorf("%s:%d asserts the lifetime contract #2649 removed (%q).\n"+
						"Review agents are released 15 minutes after their round is delivered; they are NOT held until `prism cleanup` of the parent.\n"+
						"Line: %s",
						name, i+1, claim, strings.TrimSpace(line))
				}
			}
		}
	}

	if found == 0 {
		t.Fatalf("scanned no Go files in %q — the guard is not looking at anything", dir)
	}
}

// normaliseProse lowercases and collapses runs of whitespace and comment
// markers, so wrapping and indentation cannot hide a claim from the scan.
func normaliseProse(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "//", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	// Collapse whitespace runs, and drop the em dash prism comments use as a
	// mid-sentence break so it cannot split a matched phrase.
	s = strings.ReplaceAll(s, "\u2014", " ")
	return strings.Join(strings.Fields(s), " ")
}

// packageSourceDir locates this package's source directory via runtime.Caller,
// never via $HOME or the working directory. That is what makes the guard work
// inside the nix build sandbox, where $HOME is /homeless-shelter and the
// working directory is not the repo root. Mirrors the approach in
// internal/doclint and the cmd-package guard tests.
func packageSourceDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: could not determine source file location")
	}
	return filepath.Dir(thisFile)
}
