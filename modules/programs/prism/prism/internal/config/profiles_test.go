package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// sampleProfilesFile returns a minimal ProfilesFile for testing.
func sampleProfilesFile() *config.ProfilesFile {
	return &config.ProfilesFile{
		Default: "anthropic",
		RoleMapping: map[string][]string{
			"primary":     {"coordinator", "plan"},
			"secondary":   {"worker", "review", "ac"},
			"lightweight": {"explore", "title", "summary", "compaction"},
		},
		Profiles: map[string]config.ProfileEntry{
			"anthropic": {
				"primary":     {Model: "anthropic/claude-opus-4-6"},
				"secondary":   {Model: "anthropic/claude-sonnet-4-6"},
				"lightweight": {Model: "anthropic/claude-haiku-4-5"},
			},
			"gemini-hybrid": {
				"primary":     {Model: "anthropic/claude-opus-4-6"},
				"secondary":   {Model: "google/gemini-3.1-pro-preview-customtools", Variant: "medium"},
				"lightweight": {Model: "anthropic/claude-haiku-4-5"},
			},
		},
	}
}

func TestBuildConfigContent_NoFlags(t *testing.T) {
	result, err := config.BuildConfigContent(nil, "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string when no flags set, got %q", result)
	}
}

func TestBuildConfigContent_ProfileOnly(t *testing.T) {
	pf := sampleProfilesFile()
	result, err := config.BuildConfigContent(pf, "anthropic", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result for profile=anthropic")
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Top-level model should be primary role's model.
	if cfg["model"] != "anthropic/claude-opus-4-6" {
		t.Errorf("top-level model: got %v, want anthropic/claude-opus-4-6", cfg["model"])
	}

	// Agents section.
	agents, ok := cfg["agent"].(map[string]any)
	if !ok {
		t.Fatalf("agent section missing or wrong type")
	}

	// coordinator is primary role.
	coordinator, ok := agents["coordinator"].(map[string]any)
	if !ok {
		t.Fatalf("coordinator agent missing")
	}
	if coordinator["model"] != "anthropic/claude-opus-4-6" {
		t.Errorf("coordinator model: got %v, want anthropic/claude-opus-4-6", coordinator["model"])
	}
	// No variant for anthropic profile.
	if _, hasVariant := coordinator["variant"]; hasVariant {
		t.Error("coordinator should have no variant field for anthropic profile")
	}

	// worker is secondary role.
	worker, ok := agents["worker"].(map[string]any)
	if !ok {
		t.Fatalf("worker agent missing")
	}
	if worker["model"] != "anthropic/claude-sonnet-4-6" {
		t.Errorf("worker model: got %v, want anthropic/claude-sonnet-4-6", worker["model"])
	}
	if _, hasVariant := worker["variant"]; hasVariant {
		t.Error("worker should have no variant field for anthropic profile")
	}
}

func TestBuildConfigContent_GeminiHybridProfile(t *testing.T) {
	pf := sampleProfilesFile()
	result, err := config.BuildConfigContent(pf, "gemini-hybrid", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Primary role: Opus, no variant.
	agents := cfg["agent"].(map[string]any)
	coordinator := agents["coordinator"].(map[string]any)
	if coordinator["model"] != "anthropic/claude-opus-4-6" {
		t.Errorf("coordinator model: got %v, want anthropic/claude-opus-4-6", coordinator["model"])
	}
	if _, hasVariant := coordinator["variant"]; hasVariant {
		t.Error("coordinator should have no variant for gemini-hybrid")
	}

	// Secondary role: Gemini 3.1 Pro with medium variant.
	worker := agents["worker"].(map[string]any)
	if worker["model"] != "google/gemini-3.1-pro-preview-customtools" {
		t.Errorf("worker model: got %v, want google/gemini-3.1-pro-preview-customtools", worker["model"])
	}
	if worker["variant"] != "medium" {
		t.Errorf("worker variant: got %v, want medium", worker["variant"])
	}

	// review and ac should also have the secondary role model+variant.
	review := agents["review"].(map[string]any)
	if review["variant"] != "medium" {
		t.Errorf("review variant: got %v, want medium", review["variant"])
	}

	// Lightweight role: Haiku, no variant.
	explore := agents["explore"].(map[string]any)
	if explore["model"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("explore model: got %v, want anthropic/claude-haiku-4-5", explore["model"])
	}
	if _, hasVariant := explore["variant"]; hasVariant {
		t.Error("explore should have no variant for gemini-hybrid")
	}
}

func TestBuildConfigContent_ModelOnly(t *testing.T) {
	result, err := config.BuildConfigContent(nil, "", "anthropic/claude-haiku-4-5", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Top-level model should be set.
	if cfg["model"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("top-level model: got %v", cfg["model"])
	}

	// All agents should have the same model.
	agents := cfg["agent"].(map[string]any)
	for name, a := range agents {
		entry := a.(map[string]any)
		if entry["model"] != "anthropic/claude-haiku-4-5" {
			t.Errorf("agent %s model: got %v, want anthropic/claude-haiku-4-5", name, entry["model"])
		}
	}
}

func TestBuildConfigContent_VariantOnly(t *testing.T) {
	result, err := config.BuildConfigContent(nil, "", "", "high")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// No top-level model (none set).
	if _, ok := cfg["model"]; ok {
		t.Error("unexpected top-level model when only variant is set")
	}

	// All agents should have variant=high.
	agents := cfg["agent"].(map[string]any)
	for name, a := range agents {
		entry := a.(map[string]any)
		if entry["variant"] != "high" {
			t.Errorf("agent %s variant: got %v, want high", name, entry["variant"])
		}
	}
}

func TestBuildConfigContent_ProfilePlusModelOverride(t *testing.T) {
	pf := sampleProfilesFile()
	// --profile anthropic --model custom/model → primary agents get custom/model,
	// secondary and lightweight keep their profile values.
	result, err := config.BuildConfigContent(pf, "anthropic", "custom/model", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Top-level model: the override.
	if cfg["model"] != "custom/model" {
		t.Errorf("top-level model: got %v, want custom/model", cfg["model"])
	}

	agents := cfg["agent"].(map[string]any)

	// Primary agents get override.
	coordinator := agents["coordinator"].(map[string]any)
	if coordinator["model"] != "custom/model" {
		t.Errorf("coordinator model: got %v, want custom/model", coordinator["model"])
	}

	// Secondary agents retain profile value.
	worker := agents["worker"].(map[string]any)
	if worker["model"] != "anthropic/claude-sonnet-4-6" {
		t.Errorf("worker model: got %v, want anthropic/claude-sonnet-4-6", worker["model"])
	}
}

func TestBuildConfigContent_ProfilePlusVariantOverride(t *testing.T) {
	pf := sampleProfilesFile()
	// --profile gemini-hybrid --variant low → all agents get variant=low (overrides medium).
	result, err := config.BuildConfigContent(pf, "gemini-hybrid", "", "low")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	agents := cfg["agent"].(map[string]any)
	for name, a := range agents {
		entry := a.(map[string]any)
		if entry["variant"] != "low" {
			t.Errorf("agent %s variant: got %v, want low", name, entry["variant"])
		}
	}
}

func TestBuildConfigContent_UnknownProfile(t *testing.T) {
	pf := sampleProfilesFile()
	_, err := config.BuildConfigContent(pf, "nonexistent", "", "")
	if err == nil {
		t.Fatal("expected error for unknown profile, got nil")
	}
}

func TestLoadProfiles_MissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/nonexistent/path")
	_, err := config.LoadProfiles()
	if err == nil {
		t.Fatal("expected error for missing profiles.json, got nil")
	}
}

func TestLoadProfiles_MalformedFile(t *testing.T) {
	dir := t.TempDir()
	prismDir := filepath.Join(dir, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prismDir, "profiles.json"), []byte("not json {{"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
	_, err := config.LoadProfiles()
	if err == nil {
		t.Fatal("expected error for malformed profiles.json, got nil")
	}
}

func TestLoadProfiles_ValidFile(t *testing.T) {
	dir := t.TempDir()
	prismDir := filepath.Join(dir, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatal(err)
	}

	pf := sampleProfilesFile()
	data, err := json.Marshal(pf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prismDir, "profiles.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	loaded, err := config.LoadProfiles()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded.Default != "anthropic" {
		t.Errorf("default: got %q, want anthropic", loaded.Default)
	}
	if len(loaded.Profiles) != 2 {
		t.Errorf("profiles count: got %d, want 2", len(loaded.Profiles))
	}
	if _, ok := loaded.Profiles["gemini-hybrid"]; !ok {
		t.Error("missing gemini-hybrid profile")
	}
}
