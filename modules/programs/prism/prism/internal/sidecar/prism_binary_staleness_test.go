package sidecar

// Unit tests for the prism-binary staleness diagnostic (issue #2742).
//
// prismBinaryStaleDiagnostic is the pure core of the fix: given the
// sidecar's launch-time binary path and a fresh resolution of the
// currently-installed prism binary, it returns a loud, named diagnostic only
// when the two genuinely diverge. Every acceptance criterion that does not
// need the wired /spawn handler is pinned here; the handler-level wiring and
// fail-open behaviour are pinned in prism_binary_staleness_spawn_test.go.

import (
	"strings"
	"testing"
)

func TestPrismBinaryStaleDiagnostic(t *testing.T) {
	const (
		oldPath = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-prism-0.1.0/bin/prism"
		newPath = "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-prism-0.1.0/bin/prism"
	)

	for _, tc := range []struct {
		name      string
		cached    string
		current   string
		wantWarn  bool
		wantNames []string // substrings the diagnostic must contain when wantWarn
	}{
		{
			// The reported defect: a switch moved the prism store path after
			// the sidecar launched. The diagnostic must name both paths (so
			// the mismatch is observable without comparing store paths by
			// hand), the fix, and the issue.
			name:      "mismatch after switch warns and names both paths",
			cached:    oldPath,
			current:   newPath,
			wantWarn:  true,
			wantNames: []string{oldPath, newPath, "prism restart", "#2742"},
		},
		{
			// AC edge-case: a sidecar that has not survived a switch produces
			// no warning.
			name:     "equal paths are silent",
			cached:   oldPath,
			current:  oldPath,
			wantWarn: false,
		},
		{
			// AC edge-case: the currently-installed prism cannot be resolved
			// (no `prism` on PATH, or a broken symlink chain) — unknown, fail
			// open.
			name:     "empty current (unresolvable) is silent",
			cached:   oldPath,
			current:  "",
			wantWarn: false,
		},
		{
			// AC edge-case: the sidecar's own launch-time path could not be
			// resolved — unknown, fail open.
			name:     "empty cached (unresolvable) is silent",
			cached:   "",
			current:  newPath,
			wantWarn: false,
		},
		{
			name:     "both empty is silent",
			cached:   "",
			current:  "",
			wantWarn: false,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := prismBinaryStaleDiagnostic(tc.cached, tc.current)
			if tc.wantWarn && got == "" {
				t.Fatalf("prismBinaryStaleDiagnostic(%q, %q) = \"\", want a diagnostic", tc.cached, tc.current)
			}
			if !tc.wantWarn && got != "" {
				t.Fatalf("prismBinaryStaleDiagnostic(%q, %q) = %q, want \"\"", tc.cached, tc.current, got)
			}
			for _, want := range tc.wantNames {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostic %q does not contain %q", got, want)
				}
			}
		})
	}
}

func TestCurrentInstalledPrismPathUnresolvable(t *testing.T) {
	// AC edge-case: when `prism` is not on PATH, resolution fails rather than
	// silently returning a wrong value, so the caller can fail open.
	t.Setenv("PATH", t.TempDir())
	if _, err := currentInstalledPrismPath(); err == nil {
		t.Fatal("currentInstalledPrismPath() with no prism on PATH: want error, got nil")
	}
}
