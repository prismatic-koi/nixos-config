package sidecar

// Unit tests for the pi_extension_dir staleness diagnostic (issue #2739).
//
// piExtensionStaleDiagnostic is the pure core of the fix: given the sidecar's
// startup-cached extension dir and a fresh re-read of config.json, it returns
// a loud, named diagnostic only when the two genuinely diverge. Every
// acceptance criterion that does not need the wired /spawn handler is pinned
// here; the handler-level wiring and fail-open behaviour are pinned in
// pi_extension_staleness_spawn_test.go.

import (
	"strings"
	"testing"
)

func TestPIExtensionStaleDiagnostic(t *testing.T) {
	const (
		oldPath = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-prism-pi-extension"
		newPath = "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-prism-pi-extension"
	)

	for _, tc := range []struct {
		name      string
		cached    string
		current   string
		wantWarn  bool
		wantNames []string // substrings the diagnostic must contain when wantWarn
	}{
		{
			// The reported defect: a switch moved the extension store path
			// after the sidecar started. The diagnostic must name BOTH paths
			// so the mismatch is observable without comparing store paths by
			// hand (AC: observable), and point at the fix.
			name:      "mismatch after switch warns and names both paths",
			cached:    oldPath,
			current:   newPath,
			wantWarn:  true,
			wantNames: []string{oldPath, newPath, "prism restart", "#2739"},
		},
		{
			// AC edge-case: a switch that does NOT change pi_extension_dir
			// produces no warning and no behaviour change.
			name:     "equal paths are silent",
			cached:   oldPath,
			current:  oldPath,
			wantWarn: false,
		},
		{
			// AC edge-case: a missing or unreadable config.json makes
			// config.LoadFresh() fall back to defaults (empty
			// pi_extension_dir). An empty current value is "unknown" — fail
			// open, no warning, spawn never blocked.
			name:     "empty current (unreadable config) is silent",
			cached:   oldPath,
			current:  "",
			wantWarn: false,
		},
		{
			// A sidecar that started with no pi_extension_dir set cannot make
			// a meaningful comparison. Fail open.
			name:     "empty cached is silent",
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
			got := piExtensionStaleDiagnostic(tc.cached, tc.current)
			if tc.wantWarn && got == "" {
				t.Fatalf("piExtensionStaleDiagnostic(%q, %q) = \"\", want a diagnostic", tc.cached, tc.current)
			}
			if !tc.wantWarn && got != "" {
				t.Fatalf("piExtensionStaleDiagnostic(%q, %q) = %q, want \"\"", tc.cached, tc.current, got)
			}
			for _, want := range tc.wantNames {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostic %q does not contain %q", got, want)
				}
			}
		})
	}
}
