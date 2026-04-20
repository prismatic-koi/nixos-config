package opencode

import (
	"os"
	"path/filepath"
	"testing"
)

// ── ConfigEnvVar ──────────────────────────────────────────────────────────────

func TestConfigEnvVar(t *testing.T) {
	a := New("", nil, "", "")
	got := a.ConfigEnvVar()
	if got != "OPENCODE_CONFIG_CONTENT" {
		t.Errorf("ConfigEnvVar() = %q, want %q", got, "OPENCODE_CONFIG_CONTENT")
	}
}

// ── RuntimeEnv ────────────────────────────────────────────────────────────────

func TestRuntimeEnv_ContainsBashTimeout(t *testing.T) {
	a := New("", nil, "", "")
	env := a.RuntimeEnv()
	if env == nil {
		t.Fatal("RuntimeEnv() returned nil")
	}
	val, ok := env["OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS"]
	if !ok {
		t.Error("RuntimeEnv() missing OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS")
	}
	if val != "900000" {
		t.Errorf("OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS = %q, want %q", val, "900000")
	}
}

func TestRuntimeEnv_ReturnsNewMapEachCall(t *testing.T) {
	a := New("", nil, "", "")
	env1 := a.RuntimeEnv()
	env2 := a.RuntimeEnv()
	// Mutating the returned map should not affect subsequent calls.
	env1["NEW_KEY"] = "value"
	if _, ok := env2["NEW_KEY"]; ok {
		t.Error("RuntimeEnv() returned the same map instance — mutations leak across calls")
	}
}

// ── ValidateAgentRole ─────────────────────────────────────────────────────────

func TestValidateAgentRole_ValidRole(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	agentsDir := filepath.Join(dir, "opencode", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "worker.md"), []byte("# worker"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := New("", nil, "", "")
	if err := a.ValidateAgentRole("worker"); err != nil {
		t.Errorf("ValidateAgentRole(worker): unexpected error: %v", err)
	}
}

func TestValidateAgentRole_InvalidRole(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Create the agents dir but don't add any files.
	agentsDir := filepath.Join(dir, "opencode", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	a := New("", nil, "", "")
	err := a.ValidateAgentRole("nonexistent-agent")
	if err == nil {
		t.Fatal("ValidateAgentRole(nonexistent-agent): expected error, got nil")
	}
	// Error should mention "opencode" (harness name) and the role.
	if got := err.Error(); !contains(got, "opencode") {
		t.Errorf("error %q does not mention harness name 'opencode'", got)
	}
	if got := err.Error(); !contains(got, "nonexistent-agent") {
		t.Errorf("error %q does not mention the role 'nonexistent-agent'", got)
	}
}

func TestValidateAgentRole_NoAgentsDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Don't create the agents dir at all.

	a := New("", nil, "", "")
	err := a.ValidateAgentRole("worker")
	if err == nil {
		t.Fatal("ValidateAgentRole(worker): expected error when agents dir is missing, got nil")
	}
}

// ── EffectiveModel ────────────────────────────────────────────────────────────

func TestEffectiveModel_WithConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	configDir := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := `{"agent":{"worker":{"model":"anthropic/claude-sonnet-4-6"},"coordinator":{"model":"anthropic/claude-opus-4-6"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := New("", nil, "", "")

	if got := a.EffectiveModel("worker"); got != "anthropic/claude-sonnet-4-6" {
		t.Errorf("EffectiveModel(worker) = %q, want %q", got, "anthropic/claude-sonnet-4-6")
	}
	if got := a.EffectiveModel("coordinator"); got != "anthropic/claude-opus-4-6" {
		t.Errorf("EffectiveModel(coordinator) = %q, want %q", got, "anthropic/claude-opus-4-6")
	}
}

func TestEffectiveModel_UnknownRole(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	configDir := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	config := `{"agent":{"worker":{"model":"anthropic/claude-sonnet-4-6"}}}`
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := New("", nil, "", "")
	if got := a.EffectiveModel("unknown-role"); got != "" {
		t.Errorf("EffectiveModel(unknown-role) = %q, want empty string", got)
	}
}

func TestEffectiveModel_NoConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Don't create the config file.

	a := New("", nil, "", "")
	if got := a.EffectiveModel("worker"); got != "" {
		t.Errorf("EffectiveModel(worker) = %q, want empty string when config is missing", got)
	}
}

func TestEffectiveModel_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	configDir := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := New("", nil, "", "")
	if got := a.EffectiveModel("worker"); got != "" {
		t.Errorf("EffectiveModel(worker) = %q, want empty string for invalid JSON", got)
	}
}

// contains is a helper to check substring presence.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
