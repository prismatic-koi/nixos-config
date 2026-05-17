// supervisor_args_test.go — argv-construction regression coverage for the
// iris supervisor's pi invocation.
//
// Background: issue #1777 — the supervisor was not passing `--provider`,
// `--model`, or `--thinking` to pi, so pi fell back to a built-in default
// that picked github-copilot/gpt-5.4. The user had no auth for that
// provider, so every turn died with `400 bad request: Authorization header
// is badly formatted`. The fix plumbs three new fields through `Config` →
// `SupervisorConfig` → `(*Supervisor).buildPiArgs` so the corresponding
// flags are appended to pi's command line when set, and omitted when empty
// (defensive — pi falls back to its own defaults).
//
// These tests assert the contract on `buildPiArgs` directly so future
// regressions are caught by `go test ./internal/iris/...` without needing
// to spawn a real pi child. They reuse `newShadowTestSupervisor` from
// `supervisor_agent_dir_shadow_test.go` (issue #1778) — that helper is the
// established pattern for argv-only inspection tests in this package.
//
// AC-10: no sidecar is constructed, no host paths are touched, all I/O is
// confined to `t.TempDir()` and `os.MkdirTemp` cleaned up by the helper.
package iris

import (
	"os"
	"path/filepath"
	"testing"
)

// hasFlagWithValue returns true when args contains `flag` followed
// immediately by `value` as separate elements (the pattern produced by
// `args = append(args, flag, value)`).
func hasFlagWithValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// hasFlag returns true when args contains `flag` as any element.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestBuildPiArgs_ProviderModelThinking_Set asserts that when all three of
// PIProvider / PIModel / PIThinking are non-empty, each corresponding flag
// is appended with the configured value (issue #1777, AC-1 and AC-6).
func TestBuildPiArgs_ProviderModelThinking_Set(t *testing.T) {
	sup := newShadowTestSupervisor(t)
	sup.cfg.PIProvider = "anthropic"
	sup.cfg.PIModel = "claude-sonnet-4-20250514"
	sup.cfg.PIThinking = "medium"

	args := sup.buildPiArgs("/tmp/fake-sessions")

	// --mode rpc must always be present and lead.
	if len(args) < 2 || args[0] != "--mode" || args[1] != "rpc" {
		t.Fatalf("expected args to start with `--mode rpc`, got %v", args)
	}

	for _, tc := range []struct {
		flag, value string
	}{
		{"--extension", sup.cfg.ExtensionPath},
		{"--session-dir", "/tmp/fake-sessions"},
		{"--provider", "anthropic"},
		{"--model", "claude-sonnet-4-20250514"},
		{"--thinking", "medium"},
	} {
		if !hasFlagWithValue(args, tc.flag, tc.value) {
			t.Errorf("expected args to contain %q %q, got %v", tc.flag, tc.value, args)
		}
	}
}

// TestBuildPiArgs_ProviderModelThinking_AllEmpty_OmitsFlags asserts that
// when all three optional LLM-routing fields are empty, none of the
// corresponding flags are emitted (issue #1777, AC-1 defensive path).
//
// This pins the contract so a future regression that emits `--provider ""`
// or otherwise leaks an empty-string flag will fail this test.
func TestBuildPiArgs_ProviderModelThinking_AllEmpty_OmitsFlags(t *testing.T) {
	sup := newShadowTestSupervisor(t)
	// Explicitly clear in case a future default ends up flowing through
	// newShadowTestSupervisor.
	sup.cfg.PIProvider = ""
	sup.cfg.PIModel = ""
	sup.cfg.PIThinking = ""

	args := sup.buildPiArgs("/tmp/fake-sessions")

	for _, flag := range []string{"--provider", "--model", "--thinking"} {
		if hasFlag(args, flag) {
			t.Errorf("expected no %q flag with empty config, got %v", flag, args)
		}
	}
}

// TestBuildPiArgs_ProviderOnly asserts that an isolated PIProvider produces
// `--provider <value>` and no `--model` / `--thinking` flag (AC-6).
func TestBuildPiArgs_ProviderOnly(t *testing.T) {
	sup := newShadowTestSupervisor(t)
	sup.cfg.PIProvider = "anthropic"
	sup.cfg.PIModel = ""
	sup.cfg.PIThinking = ""

	args := sup.buildPiArgs("/tmp/fake-sessions")

	if !hasFlagWithValue(args, "--provider", "anthropic") {
		t.Errorf("expected `--provider anthropic` in %v", args)
	}
	if hasFlag(args, "--model") {
		t.Errorf("expected no `--model` flag, got %v", args)
	}
	if hasFlag(args, "--thinking") {
		t.Errorf("expected no `--thinking` flag, got %v", args)
	}
}

// TestBuildPiArgs_ModelOnly asserts the symmetric case for PIModel (AC-6).
func TestBuildPiArgs_ModelOnly(t *testing.T) {
	sup := newShadowTestSupervisor(t)
	sup.cfg.PIProvider = ""
	sup.cfg.PIModel = "claude-sonnet-4-20250514"
	sup.cfg.PIThinking = ""

	args := sup.buildPiArgs("/tmp/fake-sessions")

	if !hasFlagWithValue(args, "--model", "claude-sonnet-4-20250514") {
		t.Errorf("expected `--model claude-sonnet-4-20250514` in %v", args)
	}
	if hasFlag(args, "--provider") {
		t.Errorf("expected no `--provider` flag, got %v", args)
	}
	if hasFlag(args, "--thinking") {
		t.Errorf("expected no `--thinking` flag, got %v", args)
	}
}

// TestBuildPiArgs_ThinkingOnly asserts the symmetric case for PIThinking
// (AC-6).
func TestBuildPiArgs_ThinkingOnly(t *testing.T) {
	sup := newShadowTestSupervisor(t)
	sup.cfg.PIProvider = ""
	sup.cfg.PIModel = ""
	sup.cfg.PIThinking = "high"

	args := sup.buildPiArgs("/tmp/fake-sessions")

	if !hasFlagWithValue(args, "--thinking", "high") {
		t.Errorf("expected `--thinking high` in %v", args)
	}
	if hasFlag(args, "--provider") {
		t.Errorf("expected no `--provider` flag, got %v", args)
	}
	if hasFlag(args, "--model") {
		t.Errorf("expected no `--model` flag, got %v", args)
	}
}

// TestBuildPiArgs_LLMFlagsCoexistWithSessionContinue pins that the new
// `--provider`/`--model`/`--thinking` flags compose correctly with the
// existing D-9 `--session <path>` resume wiring (issue #1777 × D-9).
func TestBuildPiArgs_LLMFlagsCoexistWithSessionContinue(t *testing.T) {
	sup := newShadowTestSupervisor(t)
	sup.cfg.PIProvider = "anthropic"
	sup.cfg.PIModel = "claude-sonnet-4-20250514"
	sup.cfg.SessionContinuePath = "/tmp/iris/sess.jsonl"

	args := sup.buildPiArgs("/tmp/fake-sessions")

	if !hasFlagWithValue(args, "--session", "/tmp/iris/sess.jsonl") {
		t.Errorf("expected `--session /tmp/iris/sess.jsonl` in %v", args)
	}
	if !hasFlagWithValue(args, "--session-dir", "/tmp/fake-sessions") {
		t.Errorf("expected `--session-dir /tmp/fake-sessions` in %v", args)
	}
	if !hasFlagWithValue(args, "--provider", "anthropic") {
		t.Errorf("expected `--provider anthropic` in %v", args)
	}
	if !hasFlagWithValue(args, "--model", "claude-sonnet-4-20250514") {
		t.Errorf("expected `--model claude-sonnet-4-20250514` in %v", args)
	}
}

// TestConfigDefaults_ProviderModelThinking pins the compiled-in defaults
// for the three new Config fields (AC-3). If the user's iris config file
// is absent or omits these fields, iris must default to
// anthropic / claude-sonnet-4-20250514 / medium — matching what the
// (now-deprecated) per-session settings.json injection used to write, plus
// a sensible thinking level.
func TestConfigDefaults_ProviderModelThinking(t *testing.T) {
	cfg := defaults()

	if cfg.PIProvider != "anthropic" {
		t.Errorf("default PIProvider = %q, want %q", cfg.PIProvider, "anthropic")
	}
	if cfg.PIModel != "claude-sonnet-4-20250514" {
		t.Errorf("default PIModel = %q, want %q", cfg.PIModel, "claude-sonnet-4-20250514")
	}
	if cfg.PIThinking != "medium" {
		t.Errorf("default PIThinking = %q, want %q", cfg.PIThinking, "medium")
	}
}

// TestLoadConfig_ParsedOverridesDefaults asserts that explicit values in
// the JSON file win over the compiled-in defaults for the three new
// fields, matching the existing merge pattern used for PIBinaryPath /
// PIExtensionPath (AC-3).
//
// LoadConfig is called with a path under t.TempDir() — no $HOME or
// $XDG_STATE_HOME leakage, so this test is safe under the nix sandbox
// (homeless-shelter gate, AGENTS.md).
func TestLoadConfig_ParsedOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
	  "pi_provider": "openai",
	  "pi_model":    "gpt-5",
	  "pi_thinking": "high"
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.PIProvider != "openai" {
		t.Errorf("PIProvider = %q, want %q", cfg.PIProvider, "openai")
	}
	if cfg.PIModel != "gpt-5" {
		t.Errorf("PIModel = %q, want %q", cfg.PIModel, "gpt-5")
	}
	if cfg.PIThinking != "high" {
		t.Errorf("PIThinking = %q, want %q", cfg.PIThinking, "high")
	}
}

// TestLoadConfig_PartialOverride_PreservesUntouchedDefaults asserts the
// merge semantics: a JSON file that only sets one of the three fields
// keeps the compiled-in defaults for the other two (AC-3). This pins the
// existing "explicit non-zero values override defaults" merge convention.
func TestLoadConfig_PartialOverride_PreservesUntouchedDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"pi_provider": "openai"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.PIProvider != "openai" {
		t.Errorf("PIProvider = %q, want %q", cfg.PIProvider, "openai")
	}
	// Other two retain defaults.
	if cfg.PIModel != "claude-sonnet-4-20250514" {
		t.Errorf("PIModel = %q, want default %q", cfg.PIModel, "claude-sonnet-4-20250514")
	}
	if cfg.PIThinking != "medium" {
		t.Errorf("PIThinking = %q, want default %q", cfg.PIThinking, "medium")
	}
}
