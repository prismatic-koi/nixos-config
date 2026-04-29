package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// sampleProfilesFile returns a minimal ProfilesFile for testing under the
// role-keyed profile schema (#1206). Each profile is expanded into per-role
// slots so coordinator/plan share the primary-tier slot, worker/review/ac
// share the secondary-tier slot, and explore/title/summary/compaction share
// the lightweight-tier slot — mirroring the migration shape produced by
// profileFromTiers in profiles.nix.
func sampleProfilesFile() *config.ProfilesFile {
	roleMapping := map[string][]string{
		"primary":     {"coordinator", "plan"},
		"secondary":   {"worker", "review", "ac"},
		"lightweight": {"explore", "title", "summary", "compaction"},
	}
	expand := func(primary, secondary, lightweight config.RoleSlot) config.ProfileEntry {
		entry := config.ProfileEntry{}
		for _, name := range roleMapping["primary"] {
			entry[name] = primary
		}
		for _, name := range roleMapping["secondary"] {
			entry[name] = secondary
		}
		for _, name := range roleMapping["lightweight"] {
			entry[name] = lightweight
		}
		return entry
	}
	return &config.ProfilesFile{
		Default:     "anthropic",
		RoleMapping: roleMapping,
		Profiles: map[string]config.ProfileEntry{
			"anthropic": expand(
				config.RoleSlot{Provider: "anthropic", Model: "anthropic/claude-opus-4-6"},
				config.RoleSlot{Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				config.RoleSlot{Provider: "anthropic", Model: "anthropic/claude-haiku-4-5"},
			),
			"gemini-hybrid": expand(
				config.RoleSlot{Provider: "anthropic", Model: "anthropic/claude-opus-4-6"},
				config.RoleSlot{Provider: "google", Model: "google/gemini-3.1-pro-preview-customtools", Thinking: "medium"},
				config.RoleSlot{Provider: "anthropic", Model: "anthropic/claude-haiku-4-5"},
			),
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

// ── ApplyModelOverrides tests ─────────────────────────────────────────────────

// workerBlob is a realistic ContainerWorkerConfig blob as produced by Nix.
// It includes a top-level model, agent entries with model fields, a $schema,
// and a system prompt field to verify those are preserved unchanged.
const workerBlob = `{
  "$schema": "https://opencode.ai/opencode.json",
  "model": "anthropic/claude-sonnet-4-6",
  "agent": {
    "worker": {
      "model": "anthropic/claude-sonnet-4-6",
      "system": "You are a worker agent."
    },
    "title": {
      "model": "anthropic/claude-haiku-4-5"
    }
  }
}`

// TestApplyModelOverrides_NoOverrides verifies that an empty modelOverride and
// variantOverride returns the blob unchanged (no re-marshal side-effects).
func TestApplyModelOverrides_NoOverrides(t *testing.T) {
	result, err := config.ApplyModelOverrides(workerBlob, "", "", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != workerBlob {
		t.Errorf("expected blob unchanged, got %q", result)
	}
}

// TestApplyModelOverrides_EmptyBlob verifies that an empty blob is returned
// as-is even when overrides are set.
func TestApplyModelOverrides_EmptyBlob(t *testing.T) {
	result, err := config.ApplyModelOverrides("", "", "anthropic/claude-opus-4-7", "high", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for empty blob, got %q", result)
	}
}

// TestApplyModelOverrides_ModelOnly verifies that --model sets the top-level
// model and all agent models, preserving other fields.
func TestApplyModelOverrides_ModelOnly(t *testing.T) {
	result, err := config.ApplyModelOverrides(workerBlob, "", "anthropic/claude-opus-4-7", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Top-level model must be overridden.
	if cfg["model"] != "anthropic/claude-opus-4-7" {
		t.Errorf("top-level model: got %v, want anthropic/claude-opus-4-7", cfg["model"])
	}

	// $schema must be preserved.
	if cfg["$schema"] == nil {
		t.Error("$schema field was dropped")
	}

	agents := cfg["agent"].(map[string]any)

	// worker model must be overridden, system prompt preserved.
	worker := agents["worker"].(map[string]any)
	if worker["model"] != "anthropic/claude-opus-4-7" {
		t.Errorf("worker model: got %v, want anthropic/claude-opus-4-7", worker["model"])
	}
	if worker["system"] == nil {
		t.Error("worker system prompt was dropped")
	}

	// title model must also be overridden.
	title := agents["title"].(map[string]any)
	if title["model"] != "anthropic/claude-opus-4-7" {
		t.Errorf("title model: got %v, want anthropic/claude-opus-4-7", title["model"])
	}
}

// TestApplyModelOverrides_VariantOnly verifies that --variant sets the variant
// on all agents while leaving models unchanged.
func TestApplyModelOverrides_VariantOnly(t *testing.T) {
	result, err := config.ApplyModelOverrides(workerBlob, "", "", "high", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Top-level model must be unchanged.
	if cfg["model"] != "anthropic/claude-sonnet-4-6" {
		t.Errorf("top-level model changed: got %v", cfg["model"])
	}

	agents := cfg["agent"].(map[string]any)

	// worker: model unchanged, variant added.
	worker := agents["worker"].(map[string]any)
	if worker["model"] != "anthropic/claude-sonnet-4-6" {
		t.Errorf("worker model changed: got %v", worker["model"])
	}
	if worker["variant"] != "high" {
		t.Errorf("worker variant: got %v, want high", worker["variant"])
	}

	// title: model unchanged, variant added.
	title := agents["title"].(map[string]any)
	if title["model"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("title model changed: got %v", title["model"])
	}
	if title["variant"] != "high" {
		t.Errorf("title variant: got %v, want high", title["variant"])
	}
}

// TestApplyModelOverrides_ModelAndVariant verifies that both --model and
// --variant are applied together.
func TestApplyModelOverrides_ModelAndVariant(t *testing.T) {
	result, err := config.ApplyModelOverrides(workerBlob, "", "anthropic/claude-opus-4-7", "high", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if cfg["model"] != "anthropic/claude-opus-4-7" {
		t.Errorf("top-level model: got %v, want anthropic/claude-opus-4-7", cfg["model"])
	}

	agents := cfg["agent"].(map[string]any)
	worker := agents["worker"].(map[string]any)
	if worker["model"] != "anthropic/claude-opus-4-7" {
		t.Errorf("worker model: got %v, want anthropic/claude-opus-4-7", worker["model"])
	}
	if worker["variant"] != "high" {
		t.Errorf("worker variant: got %v, want high", worker["variant"])
	}
}

// TestApplyModelOverrides_WithProfile verifies that when a profile name is
// supplied alongside a model override, only primary-role agents have their
// model overridden (matching BuildConfigContent semantics).
func TestApplyModelOverrides_WithProfile(t *testing.T) {
	pf := sampleProfilesFile()
	// "anthropic" profile: primary=["coordinator","plan"], secondary=["worker","review","ac"],
	// lightweight=["explore","title","summary","compaction"].
	// The blob has worker (secondary) and title (lightweight) plus we add coordinator.
	blob := `{
		"model": "anthropic/claude-sonnet-4-6",
		"agent": {
			"coordinator": {"model": "anthropic/claude-opus-4-6"},
			"worker":      {"model": "anthropic/claude-sonnet-4-6"},
			"title":       {"model": "anthropic/claude-haiku-4-5"}
		}
	}`

	result, err := config.ApplyModelOverrides(blob, "anthropic", "custom/model", "", pf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Top-level model must be overridden.
	if cfg["model"] != "custom/model" {
		t.Errorf("top-level model: got %v, want custom/model", cfg["model"])
	}

	agents := cfg["agent"].(map[string]any)

	// coordinator is primary → model overridden.
	coordinator := agents["coordinator"].(map[string]any)
	if coordinator["model"] != "custom/model" {
		t.Errorf("coordinator model: got %v, want custom/model", coordinator["model"])
	}

	// worker is secondary → model NOT overridden.
	worker := agents["worker"].(map[string]any)
	if worker["model"] != "anthropic/claude-sonnet-4-6" {
		t.Errorf("worker model: got %v, want anthropic/claude-sonnet-4-6 (unchanged)", worker["model"])
	}

	// title is lightweight → model NOT overridden.
	title := agents["title"].(map[string]any)
	if title["model"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("title model: got %v, want anthropic/claude-haiku-4-5 (unchanged)", title["model"])
	}
}

// TestApplyModelOverrides_PreservesNonModelFields verifies that non-model
// fields in the blob (system prompt, tool permissions, $schema, etc.) are
// preserved unchanged after applying overrides.
func TestApplyModelOverrides_PreservesNonModelFields(t *testing.T) {
	blob := `{
		"$schema": "https://opencode.ai/opencode.json",
		"model": "anthropic/claude-sonnet-4-6",
		"agent": {
			"worker": {
				"model": "anthropic/claude-sonnet-4-6",
				"system": "You are a worker agent.",
				"tools": {"bash": {"deny": ["rm -rf"]}}
			}
		}
	}`

	result, err := config.ApplyModelOverrides(blob, "", "anthropic/claude-opus-4-7", "high", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if cfg["$schema"] == nil {
		t.Error("$schema was dropped")
	}

	agents := cfg["agent"].(map[string]any)
	worker := agents["worker"].(map[string]any)

	if worker["system"] == nil {
		t.Error("system prompt was dropped")
	}
	if worker["tools"] == nil {
		t.Error("tools field was dropped")
	}
	if worker["model"] != "anthropic/claude-opus-4-7" {
		t.Errorf("worker model: got %v, want anthropic/claude-opus-4-7", worker["model"])
	}
	if worker["variant"] != "high" {
		t.Errorf("worker variant: got %v, want high", worker["variant"])
	}
}

// TestApplyModelOverrides_InvalidBlob verifies that an invalid JSON blob
// returns an error.
func TestApplyModelOverrides_InvalidBlob(t *testing.T) {
	_, err := config.ApplyModelOverrides("not json {{", "", "some/model", "", nil)
	if err == nil {
		t.Fatal("expected error for invalid JSON blob, got nil")
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

// ── Role-keyed schema (#1206) ─────────────────────────────────────────────────

// TestSlotForRole_HitsAndMisses verifies that the role-keyed lookup returns
// the slot for known roles and reports a clean miss for unknown roles. This
// is the canonical accessor introduced by #1206 — every other helper that
// resolves "what does role X get under profile Y" goes through it.
func TestSlotForRole_HitsAndMisses(t *testing.T) {
	pf := sampleProfilesFile()

	// coordinator is in the primary tier of "anthropic".
	slot, ok := config.SlotForRole(pf, "anthropic", "coordinator")
	if !ok {
		t.Fatal("expected coordinator slot for anthropic")
	}
	if slot.Model != "anthropic/claude-opus-4-6" {
		t.Errorf("coordinator model: got %q, want anthropic/claude-opus-4-6", slot.Model)
	}
	if slot.Provider != "anthropic" {
		t.Errorf("coordinator provider: got %q, want anthropic", slot.Provider)
	}

	// explore is in the lightweight tier of "anthropic".
	slot, ok = config.SlotForRole(pf, "anthropic", "explore")
	if !ok {
		t.Fatal("expected explore slot for anthropic")
	}
	if slot.Model != "anthropic/claude-haiku-4-5" {
		t.Errorf("explore model: got %q, want anthropic/claude-haiku-4-5", slot.Model)
	}

	// "build" is intentionally not in any tier — it inherits the top-level
	// model rather than carrying a per-role override.
	if _, ok := config.SlotForRole(pf, "anthropic", "build"); ok {
		t.Error("build should not have a slot in the anthropic profile")
	}

	// Unknown profile: clean miss.
	if _, ok := config.SlotForRole(pf, "nonexistent", "coordinator"); ok {
		t.Error("expected miss for unknown profile, got hit")
	}

	// Nil pf: clean miss, no panic.
	if _, ok := config.SlotForRole(nil, "anthropic", "coordinator"); ok {
		t.Error("expected miss for nil pf, got hit")
	}
}

// TestRequireSlot_PassesWhenPresent verifies that RequireSlot returns nil
// when the active profile defines the requested slot.
func TestRequireSlot_PassesWhenPresent(t *testing.T) {
	pf := sampleProfilesFile()
	if err := config.RequireSlot(pf, "anthropic", "coordinator"); err != nil {
		t.Errorf("RequireSlot(anthropic, coordinator): unexpected error: %v", err)
	}
	if err := config.RequireSlot(pf, "gemini-hybrid", "worker"); err != nil {
		t.Errorf("RequireSlot(gemini-hybrid, worker): unexpected error: %v", err)
	}
}

// TestRequireSlot_FailsWhenSlotMissing verifies the spawn-time edge case
// from #1206: a profile missing the slot the session needs must fail with a
// clear error message that lists the slots the profile currently defines so
// the operator can see the gap immediately.
func TestRequireSlot_FailsWhenSlotMissing(t *testing.T) {
	pf := &config.ProfilesFile{
		Default: "minimal",
		RoleMapping: map[string][]string{
			"primary":   {"coordinator"},
			"secondary": {"worker"},
		},
		Profiles: map[string]config.ProfileEntry{
			"minimal": {
				// only worker is defined — no coordinator slot.
				"worker": {Model: "anthropic/claude-sonnet-4-6"},
			},
		},
	}
	err := config.RequireSlot(pf, "minimal", "coordinator")
	if err == nil {
		t.Fatal("expected error for missing coordinator slot, got nil")
	}
	msg := err.Error()
	// Error must name the missing role …
	if !contains(msg, "coordinator") {
		t.Errorf("error %q does not mention the missing role 'coordinator'", msg)
	}
	// … the profile being checked …
	if !contains(msg, "minimal") {
		t.Errorf("error %q does not mention the profile 'minimal'", msg)
	}
	// … and list the slots the profile currently defines.
	if !contains(msg, "worker") {
		t.Errorf("error %q does not list defined slots (expected to see 'worker')", msg)
	}
}

// TestRequireSlot_FailsWhenProfileUnknown verifies that an unknown profile
// produces a clear error listing available profile names.
func TestRequireSlot_FailsWhenProfileUnknown(t *testing.T) {
	pf := sampleProfilesFile()
	err := config.RequireSlot(pf, "nonexistent", "coordinator")
	if err == nil {
		t.Fatal("expected error for unknown profile, got nil")
	}
	if !contains(err.Error(), "anthropic") {
		t.Errorf("error %q does not list available profiles", err.Error())
	}
}

// TestRequireSlot_NilProfilesFile verifies that a nil profiles file produces
// a descriptive error rather than panicking.
func TestRequireSlot_NilProfilesFile(t *testing.T) {
	err := config.RequireSlot(nil, "anthropic", "coordinator")
	if err == nil {
		t.Fatal("expected error for nil pf, got nil")
	}
}

// TestLegacyOpencodeConfigFor_AnthropicProfile verifies the compatibility
// shim renders the role-keyed "anthropic" profile into the legacy opencode
// config blob shape: a top-level "model" plus per-agent "model" entries
// matching the role-keyed slot for each role in role_mapping.
func TestLegacyOpencodeConfigFor_AnthropicProfile(t *testing.T) {
	pf := sampleProfilesFile()
	out, err := config.LegacyOpencodeConfigFor(pf, "anthropic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty output")
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if cfg["model"] != "anthropic/claude-opus-4-6" {
		t.Errorf("top-level model: got %v, want anthropic/claude-opus-4-6", cfg["model"])
	}

	agents, ok := cfg["agent"].(map[string]any)
	if !ok {
		t.Fatal("missing 'agent' map in output")
	}
	// Primary tier — coordinator/plan get the primary slot model.
	for _, name := range []string{"coordinator", "plan"} {
		entry, ok := agents[name].(map[string]any)
		if !ok {
			t.Errorf("missing agent %q", name)
			continue
		}
		if entry["model"] != "anthropic/claude-opus-4-6" {
			t.Errorf("%s model: got %v, want anthropic/claude-opus-4-6", name, entry["model"])
		}
	}
	// Secondary tier — worker/review/ac get the secondary slot model.
	for _, name := range []string{"worker", "review", "ac"} {
		entry, ok := agents[name].(map[string]any)
		if !ok {
			t.Errorf("missing agent %q", name)
			continue
		}
		if entry["model"] != "anthropic/claude-sonnet-4-6" {
			t.Errorf("%s model: got %v, want anthropic/claude-sonnet-4-6", name, entry["model"])
		}
	}
	// Lightweight tier — explore/title/summary/compaction get the lightweight slot.
	for _, name := range []string{"explore", "title", "summary", "compaction"} {
		entry, ok := agents[name].(map[string]any)
		if !ok {
			t.Errorf("missing agent %q", name)
			continue
		}
		if entry["model"] != "anthropic/claude-haiku-4-5" {
			t.Errorf("%s model: got %v, want anthropic/claude-haiku-4-5", name, entry["model"])
		}
	}
}

// TestLegacyOpencodeConfigFor_GeminiHybridPropagatesThinking verifies that a
// non-empty `thinking` value is rendered as opencode's `variant` field, while
// roles without a thinking value omit `variant` entirely (preserving
// opencode's default variant resolution).
func TestLegacyOpencodeConfigFor_GeminiHybridPropagatesThinking(t *testing.T) {
	pf := sampleProfilesFile()
	out, err := config.LegacyOpencodeConfigFor(pf, "gemini-hybrid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	agents := cfg["agent"].(map[string]any)
	worker := agents["worker"].(map[string]any)
	if worker["variant"] != "medium" {
		t.Errorf("worker variant: got %v, want medium", worker["variant"])
	}
	coordinator := agents["coordinator"].(map[string]any)
	if _, hasVariant := coordinator["variant"]; hasVariant {
		t.Error("coordinator should have no variant when thinking is empty")
	}
}

// TestLegacyOpencodeConfigFor_BitIdenticalToBuildConfigContent is the gate
// from #1206: applying the role-keyed profile via the compat shim must
// produce the same opencode config blob as BuildConfigContent for the same
// profile with no overrides. Identical means "the same JSON object" —
// compared after re-decode so key ordering does not affect the result.
func TestLegacyOpencodeConfigFor_BitIdenticalToBuildConfigContent(t *testing.T) {
	pf := sampleProfilesFile()
	for _, profile := range []string{"anthropic", "gemini-hybrid"} {
		shim, err := config.LegacyOpencodeConfigFor(pf, profile)
		if err != nil {
			t.Fatalf("LegacyOpencodeConfigFor(%s): %v", profile, err)
		}
		built, err := config.BuildConfigContent(pf, profile, "", "")
		if err != nil {
			t.Fatalf("BuildConfigContent(%s): %v", profile, err)
		}

		var shimMap, builtMap map[string]any
		if err := json.Unmarshal([]byte(shim), &shimMap); err != nil {
			t.Fatalf("shim output for %s is not valid JSON: %v", profile, err)
		}
		if err := json.Unmarshal([]byte(built), &builtMap); err != nil {
			t.Fatalf("built output for %s is not valid JSON: %v", profile, err)
		}
		if !jsonDeepEqual(shimMap, builtMap) {
			t.Errorf("profile %s: shim and BuildConfigContent diverged\n  shim:  %s\n  built: %s",
				profile, shim, built)
		}
	}
}

// TestLegacyOpencodeConfigFor_UnknownProfile verifies that the shim returns
// an error for unknown profiles (rather than silently producing an empty
// blob), so callers can surface the misconfiguration.
func TestLegacyOpencodeConfigFor_UnknownProfile(t *testing.T) {
	pf := sampleProfilesFile()
	_, err := config.LegacyOpencodeConfigFor(pf, "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown profile, got nil")
	}
}

// TestLegacyOpencodeConfigFor_NilProfilesFile verifies that nil pf returns
// an empty string with no error (matches the behaviour of other helpers
// that gracefully degrade when the profiles file is unavailable).
func TestLegacyOpencodeConfigFor_NilProfilesFile(t *testing.T) {
	out, err := config.LegacyOpencodeConfigFor(nil, "anthropic")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output, got %q", out)
	}
}

// jsonDeepEqual compares two decoded JSON values for structural equality.
// It is a minimal implementation tuned for the shapes BuildConfigContent
// emits: nested map[string]any with string/number leaves.
func jsonDeepEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !jsonDeepEqual(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonDeepEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// contains is a substring helper duplicated locally so the test file does
// not need to import strings just for one call site.
func contains(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
