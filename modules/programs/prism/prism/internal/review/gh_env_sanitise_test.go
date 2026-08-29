package review_test

// gh_env_sanitise_test.go — tests for sanitisedGHEnv, the runGH-side defence
// against shell-literal `$(cat …)` values propagating through
// the process tree and being sent to gh as GITHUB_TOKEN.

import (
	"slices"
	"testing"

	"github.com/prismatic-koi/prism/internal/review"
)

// TestSanitisedGHEnv_DropsShellLiteralGitHubToken asserts that an inherited
// GITHUB_TOKEN whose value is a `$(cat …)` shell literal is dropped from the
// env passed to gh. Without this, gh would send the literal string to
// GitHub as a bearer token and 401 every request.
func TestSanitisedGHEnv_DropsShellLiteralGitHubToken(t *testing.T) {
	in := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/ben",
		"GITHUB_TOKEN=$(cat /run/secrets/github_token_prismatic_koi_worker)",
		"TERM=tmux-256color",
	}
	out := review.SanitisedGHEnvForTest(in)

	for _, kv := range out {
		if kv == in[2] {
			t.Errorf("shell-literal GITHUB_TOKEN must be dropped; got %q in output", kv)
		}
	}
	// Every other entry must pass through unchanged.
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/ben", "TERM=tmux-256color"} {
		if !slices.Contains(out, want) {
			t.Errorf("expected %q in output, got: %v", want, out)
		}
	}
}

// TestSanitisedGHEnv_DropsShellLiteralPrismVars asserts the same guard for
// the PRISM_GITHUB_TOKEN_* family, so a downstream tool that decides to read
// those vars (unlikely but not impossible) doesn't get the broken values
// either.
func TestSanitisedGHEnv_DropsShellLiteralPrismVars(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER=$(cat /run/secrets/x)",
		"PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR=ghp_valid",
	}
	out := review.SanitisedGHEnvForTest(in)

	for _, kv := range out {
		if kv == in[1] {
			t.Errorf("shell-literal PRISM_GITHUB_TOKEN_* must be dropped; got %q", kv)
		}
	}
	// A valid entry in the same family must pass through.
	if !slices.Contains(out, "PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR=ghp_valid") {
		t.Errorf("valid PRISM_GITHUB_TOKEN_* entry must pass through, got: %v", out)
	}
}

// TestSanitisedGHEnv_KeepsValidGitHubToken is the negative control: a
// well-formed token value must pass through untouched. Without this we'd
// have no proof the guard doesn't drop everything.
func TestSanitisedGHEnv_KeepsValidGitHubToken(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"GITHUB_TOKEN=ghp_1234567890abcdef",
	}
	out := review.SanitisedGHEnvForTest(in)
	if !slices.Contains(out, "GITHUB_TOKEN=ghp_1234567890abcdef") {
		t.Errorf("valid GITHUB_TOKEN must pass through unchanged, got: %v", out)
	}
}

// TestSanitisedGHEnv_MalformedEntriesPassThrough covers the edge case where
// an env entry has no '=' at all (should be impossible for real
// os.Environ() output but defensively handled). Such entries must not
// panic and must pass through unchanged.
func TestSanitisedGHEnv_MalformedEntriesPassThrough(t *testing.T) {
	in := []string{"no-equals-here", "GITHUB_TOKEN=ghp_ok"}
	out := review.SanitisedGHEnvForTest(in)
	if !slices.Contains(out, "no-equals-here") {
		t.Errorf("malformed entry must pass through, got: %v", out)
	}
	if !slices.Contains(out, "GITHUB_TOKEN=ghp_ok") {
		t.Errorf("valid GITHUB_TOKEN must pass through, got: %v", out)
	}
}
