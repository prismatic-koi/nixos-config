package iris

// credential_broker_mount_test.go — D-7 mount-layout negative tests.
//
// These tests assert that the bash sandbox mount/profile layouts produced by
// the platform builders do NOT include the pi-process credential paths or
// any LLM API key environment variables.
//
// The exhaustive list of forbidden paths (per #1638 AC):
//
//   - ~/.claude
//   - ~/.mcp-auth
//   - ~/.pi/agent/*
//   - ~/.cache/bun
//   - ~/.config/pi/*
//
// Tests are split per platform so they can use the platform-specific
// builder directly. Cross-platform tests on the generic broker live in
// credential_broker_test.go.

import (
	"strings"
	"testing"
)

// forbiddenPiPaths lists the substrings that must NEVER appear in the bash
// sandbox mount layout or SBPL profile.  Exported via this helper so the
// Linux and Darwin tests stay in lock-step.
func forbiddenPiPaths() []string {
	return []string{
		"/.claude",
		"/.mcp-auth",
		"/.pi/agent",
		"/.cache/bun",
		"/.config/pi",
	}
}

// forbiddenEnvKeys lists the LLM API key env var names that must never
// appear as bwrap --setenv arguments or in the SBPL profile.
func forbiddenEnvKeys() []string {
	return ForbiddenLLMKeyNames()
}

// TestForbiddenPiPaths_Stable verifies the forbidden-path list matches the
// AC verbatim. Guards against future drift where someone removes an entry
// thinking it is redundant.
func TestForbiddenPiPaths_Stable(t *testing.T) {
	want := []string{
		"/.claude",
		"/.mcp-auth",
		"/.pi/agent",
		"/.cache/bun",
		"/.config/pi",
	}
	got := forbiddenPiPaths()
	if len(got) != len(want) {
		t.Fatalf("forbidden paths length mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("forbiddenPiPaths()[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

// TestBashEnv_NoForbiddenKeys is the cross-platform belt-and-braces check
// that ResolveBash().Env contains no forbidden LLM key for any platform.
func TestBashEnv_NoForbiddenKeys(t *testing.T) {
	for _, k := range forbiddenEnvKeys() {
		t.Setenv(k, "should-be-stripped")
	}
	// Also set every PRISM_GITHUB_TOKEN_* so we cover the raw-leak case.
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", "raw")
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR", "raw")

	env := bashEnv("worker", "")
	joined := strings.Join(env, "\n")

	for _, k := range forbiddenEnvKeys() {
		if strings.Contains(joined, k+"=") {
			t.Errorf("bashEnv() contains forbidden key %q; must be stripped", k)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "PRISM_GITHUB_TOKEN_") {
			t.Errorf("bashEnv() contains raw PRISM_GITHUB_TOKEN_* var: %q", kv)
		}
	}
}
