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

// sampleProfilesFileWithReview returns a ProfilesFile with per-agent review
// config blobs set, for testing ContainerConfigForRole. The fixture blobs use
// the correct opencode.ai schema key "agent" (singular), matching what real
// Nix-generated blobs produce. Each blob declares only its own agent.
func sampleProfilesFileWithReview() *config.ProfilesFile {
	pf := sampleProfilesFile()
	pf.ContainerWorkerConfig = `{"agent":{"worker":{}}}`
	pf.ContainerCoordinatorConfig = `{"agent":{"coordinator":{}}}`
	pf.ContainerReviewGoalConfig = `{"$schema":"https://opencode.ai/opencode.json","agent":{"review-goal":{}}}`
	pf.ContainerReviewCodeConfig = `{"$schema":"https://opencode.ai/opencode.json","agent":{"review-code":{}}}`
	pf.ContainerReviewSecurityConfig = `{"$schema":"https://opencode.ai/opencode.json","agent":{"review-security":{}}}`
	pf.ContainerReviewQaConfig = `{"$schema":"https://opencode.ai/opencode.json","agent":{"review-qa":{}}}`
	pf.ContainerReviewContextConfig = `{"$schema":"https://opencode.ai/opencode.json","agent":{"review-context":{}}}`
	return pf
}

// TestContainerConfigForRole_ReviewGoal verifies that passing role "review-goal"
// returns the ContainerReviewGoalConfig blob (non-empty, valid JSON, has
// "$schema" and "agent" keys).
func TestContainerConfigForRole_ReviewGoal(t *testing.T) {
	pf := sampleProfilesFileWithReview()
	result, err := config.ContainerConfigForRole(pf, "review-goal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result for role=review-goal")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if _, ok := cfg["$schema"]; !ok {
		t.Error("expected top-level '$schema' key in review-goal config blob")
	}
	agents, ok := cfg["agent"].(map[string]any)
	if !ok {
		t.Fatal("expected top-level 'agent' key in review-goal config blob")
	}
	if _, ok := agents["review-goal"]; !ok {
		t.Error("expected 'review-goal' agent in review-goal config blob")
	}
	// Must NOT declare other review agents.
	for _, other := range []string{"review-code", "review-security", "review-qa", "review-context"} {
		if _, ok := agents[other]; ok {
			t.Errorf("review-goal blob must not declare agent %q", other)
		}
	}
}

// TestContainerConfigForRole_ReviewCode verifies the review-code per-agent blob.
func TestContainerConfigForRole_ReviewCode(t *testing.T) {
	pf := sampleProfilesFileWithReview()
	result, err := config.ContainerConfigForRole(pf, "review-code")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result for role=review-code")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	agents, ok := cfg["agent"].(map[string]any)
	if !ok {
		t.Fatal("expected 'agent' key in review-code config blob")
	}
	if _, ok := agents["review-code"]; !ok {
		t.Error("expected 'review-code' agent in review-code config blob")
	}
	for _, other := range []string{"review-goal", "review-security", "review-qa", "review-context"} {
		if _, ok := agents[other]; ok {
			t.Errorf("review-code blob must not declare agent %q", other)
		}
	}
}

// TestContainerConfigForRole_ReviewSecurity verifies the review-security per-agent blob.
func TestContainerConfigForRole_ReviewSecurity(t *testing.T) {
	pf := sampleProfilesFileWithReview()
	result, err := config.ContainerConfigForRole(pf, "review-security")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result for role=review-security")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	agents, ok := cfg["agent"].(map[string]any)
	if !ok {
		t.Fatal("expected 'agent' key in review-security config blob")
	}
	if _, ok := agents["review-security"]; !ok {
		t.Error("expected 'review-security' agent in review-security config blob")
	}
	for _, other := range []string{"review-goal", "review-code", "review-qa", "review-context"} {
		if _, ok := agents[other]; ok {
			t.Errorf("review-security blob must not declare agent %q", other)
		}
	}
}

// TestContainerConfigForRole_ReviewQa verifies the review-qa per-agent blob.
func TestContainerConfigForRole_ReviewQa(t *testing.T) {
	pf := sampleProfilesFileWithReview()
	result, err := config.ContainerConfigForRole(pf, "review-qa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result for role=review-qa")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	agents, ok := cfg["agent"].(map[string]any)
	if !ok {
		t.Fatal("expected 'agent' key in review-qa config blob")
	}
	if _, ok := agents["review-qa"]; !ok {
		t.Error("expected 'review-qa' agent in review-qa config blob")
	}
	for _, other := range []string{"review-goal", "review-code", "review-security", "review-context"} {
		if _, ok := agents[other]; ok {
			t.Errorf("review-qa blob must not declare agent %q", other)
		}
	}
}

// TestContainerConfigForRole_ReviewContext verifies the review-context per-agent blob.
func TestContainerConfigForRole_ReviewContext(t *testing.T) {
	pf := sampleProfilesFileWithReview()
	result, err := config.ContainerConfigForRole(pf, "review-context")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result for role=review-context")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	agents, ok := cfg["agent"].(map[string]any)
	if !ok {
		t.Fatal("expected 'agent' key in review-context config blob")
	}
	if _, ok := agents["review-context"]; !ok {
		t.Error("expected 'review-context' agent in review-context config blob")
	}
	for _, other := range []string{"review-goal", "review-code", "review-security", "review-qa"} {
		if _, ok := agents[other]; ok {
			t.Errorf("review-context blob must not declare agent %q", other)
		}
	}
}

// TestContainerConfigForRole_Worker verifies that "worker" still returns the
// worker blob (regression guard).
func TestContainerConfigForRole_Worker(t *testing.T) {
	pf := sampleProfilesFileWithReview()
	result, err := config.ContainerConfigForRole(pf, "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != pf.ContainerWorkerConfig {
		t.Errorf("worker blob: got %q, want %q", result, pf.ContainerWorkerConfig)
	}
}

// TestContainerConfigForRole_Coordinator verifies that "coordinator" still
// returns the coordinator blob (regression guard).
func TestContainerConfigForRole_Coordinator(t *testing.T) {
	pf := sampleProfilesFileWithReview()
	result, err := config.ContainerConfigForRole(pf, "coordinator")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != pf.ContainerCoordinatorConfig {
		t.Errorf("coordinator blob: got %q, want %q", result, pf.ContainerCoordinatorConfig)
	}
}

// TestContainerConfigForRole_UnknownRole verifies that an unrecognised role
// (e.g. "plan") returns ("", nil) without error. The retired "review" role
// (from PR-A) also returns ("", nil) after PR-B retires it.
func TestContainerConfigForRole_UnknownRole(t *testing.T) {
	pf := sampleProfilesFileWithReview()
	for _, role := range []string{"plan", "explore", "ac", "unknown", "review"} {
		result, err := config.ContainerConfigForRole(pf, role)
		if err != nil {
			t.Errorf("role %q: unexpected error: %v", role, err)
		}
		if result != "" {
			t.Errorf("role %q: expected empty string, got %q", role, result)
		}
	}
}

// TestContainerConfigForRole_NilProfilesFile verifies that a nil *ProfilesFile
// returns ("", nil) for all roles — no panic.
func TestContainerConfigForRole_NilProfilesFile(t *testing.T) {
	roles := []string{
		"worker", "coordinator",
		"review-goal", "review-code", "review-security", "review-qa", "review-context",
		"review", "plan", "unknown",
	}
	for _, role := range roles {
		result, err := config.ContainerConfigForRole(nil, role)
		if err != nil {
			t.Errorf("nil pf, role %q: unexpected error: %v", role, err)
		}
		if result != "" {
			t.Errorf("nil pf, role %q: expected empty string, got %q", role, result)
		}
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
