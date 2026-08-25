package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// sampleProfilesFile returns a minimal ProfilesFile for testing under the
// flat per-role profile schema (#1612). Each profile is a direct map from
// role name to slot — no tier indirection.
func sampleProfilesFile() *config.ProfilesFile {
	return &config.ProfilesFile{
		Default: "anthropic",
		Profiles: map[string]config.ProfileEntry{
			"anthropic": {
				"coordinator":     {Provider: "anthropic", Model: "anthropic/claude-opus-4-7", Thinking: "medium"},
				"worker":          {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6", Thinking: "low"},
				"ac":              {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"retro":           {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"investigate":     {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"review-goal":     {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"review-code":     {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"review-security": {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"review-qa":       {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"review-context":  {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
			},
			"gemini-hybrid": {
				"coordinator":     {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6"},
				"worker":          {Provider: "google", Model: "google/gemini-3.1-pro-preview-customtools", Thinking: "medium"},
				"ac":              {Provider: "google", Model: "google/gemini-3.1-pro-preview-customtools"},
				"retro":           {Provider: "google", Model: "google/gemini-3.1-pro-preview-customtools"},
				"investigate":     {Provider: "google", Model: "google/gemini-3.1-pro-preview-customtools"},
				"review-goal":     {Provider: "google", Model: "google/gemini-3.1-pro-preview-customtools", Thinking: "medium"},
				"review-code":     {Provider: "google", Model: "google/gemini-3.1-pro-preview-customtools", Thinking: "medium"},
				"review-security": {Provider: "google", Model: "google/gemini-3.1-pro-preview-customtools"},
				"review-qa":       {Provider: "google", Model: "google/gemini-3.1-pro-preview-customtools"},
				"review-context":  {Provider: "google", Model: "google/gemini-3.1-pro-preview-customtools"},
			},
		},
	}
}

// ── BuildConfigContent tests ──────────────────────────────────────────────────

func TestBuildConfigContent_NoFlags(t *testing.T) {
	result, err := config.BuildConfigContent(nil, "", "", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string when no flags set, got %q", result)
	}
}

// TestBuildConfigContent_ProfileOnly_Coordinator verifies that --profile
// resolves the coordinator slot's model when rootRole="coordinator".
func TestBuildConfigContent_ProfileOnly_Coordinator(t *testing.T) {
	pf := sampleProfilesFile()
	result, err := config.BuildConfigContent(pf, "anthropic", "coordinator", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result for profile=anthropic role=coordinator")
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Top-level model should be coordinator's model.
	if cfg["model"] != "anthropic/claude-opus-4-7" {
		t.Errorf("top-level model: got %v, want anthropic/claude-opus-4-7", cfg["model"])
	}

	// Variant: thinking="medium" → variant="medium".
	if cfg["variant"] != "medium" {
		t.Errorf("variant: got %v, want medium", cfg["variant"])
	}

	// No agent sub-map: pi sessions have a single root agent.
	if _, hasAgent := cfg["agent"]; hasAgent {
		t.Error("unexpected 'agent' sub-map — pi sessions use root-level model/variant only")
	}
}

// TestBuildConfigContent_ProfileOnly_Worker verifies that --profile with
// rootRole="worker" resolves the worker slot, not coordinator's.
func TestBuildConfigContent_ProfileOnly_Worker(t *testing.T) {
	pf := sampleProfilesFile()
	result, err := config.BuildConfigContent(pf, "anthropic", "worker", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Worker slot in anthropic profile.
	if cfg["model"] != "anthropic/claude-sonnet-4-6" {
		t.Errorf("model: got %v, want anthropic/claude-sonnet-4-6 (worker slot)", cfg["model"])
	}
	// Variant: thinking="low" → variant="low".
	if cfg["variant"] != "low" {
		t.Errorf("variant: got %v, want low", cfg["variant"])
	}
}

// TestBuildConfigContent_ProfileOnly_ReviewGoal verifies the review-goal slot.
func TestBuildConfigContent_ProfileOnly_ReviewGoal(t *testing.T) {
	pf := sampleProfilesFile()
	result, err := config.BuildConfigContent(pf, "gemini-hybrid", "review-goal", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if cfg["model"] != "google/gemini-3.1-pro-preview-customtools" {
		t.Errorf("model: got %v, want google/gemini-3.1-pro-preview-customtools", cfg["model"])
	}
	if cfg["variant"] != "medium" {
		t.Errorf("variant: got %v, want medium", cfg["variant"])
	}
}

// TestBuildConfigContent_ProfileAndModelOverride verifies that --profile P
// --model X overrides only the root role's model.
func TestBuildConfigContent_ProfileAndModelOverride(t *testing.T) {
	pf := sampleProfilesFile()
	// --profile anthropic --model custom/model --agent coordinator
	result, err := config.BuildConfigContent(pf, "anthropic", "coordinator", "custom/model", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Model must be the override.
	if cfg["model"] != "custom/model" {
		t.Errorf("model: got %v, want custom/model", cfg["model"])
	}
}

// TestBuildConfigContent_ProfileAndVariantOverride verifies that --profile P
// --variant V overrides the root role's variant.
func TestBuildConfigContent_ProfileAndVariantOverride(t *testing.T) {
	pf := sampleProfilesFile()
	result, err := config.BuildConfigContent(pf, "gemini-hybrid", "worker", "", "low", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Variant must be the override (worker had "medium" from profile).
	if cfg["variant"] != "low" {
		t.Errorf("variant: got %v, want low", cfg["variant"])
	}
}

// TestBuildConfigContent_ModelOnly verifies that --model X without --profile
// sets only the root role's model (no "agent" map, single top-level model).
func TestBuildConfigContent_ModelOnly(t *testing.T) {
	result, err := config.BuildConfigContent(nil, "", "coordinator", "anthropic/claude-haiku-4-5", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if cfg["model"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("top-level model: got %v", cfg["model"])
	}
	if _, hasAgent := cfg["agent"]; hasAgent {
		t.Error("unexpected 'agent' sub-map — model-only override must not emit an agent map")
	}
}

// TestBuildConfigContent_VariantOnly verifies that --variant X without
// --profile sets only the root role's variant.
func TestBuildConfigContent_VariantOnly(t *testing.T) {
	result, err := config.BuildConfigContent(nil, "", "coordinator", "", "high", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if _, ok := cfg["model"]; ok {
		t.Error("unexpected top-level model when only variant is set")
	}
	if cfg["variant"] != "high" {
		t.Errorf("variant: got %v, want high", cfg["variant"])
	}
}

// TestBuildConfigContent_ProviderOnly verifies that --provider X without
// --profile sets only the root role's defaultProvider (issue #2852).
func TestBuildConfigContent_ProviderOnly(t *testing.T) {
	result, err := config.BuildConfigContent(nil, "", "coordinator", "", "", "openrouter")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if _, ok := cfg["model"]; ok {
		t.Error("unexpected top-level model when only provider is set")
	}
	if cfg["defaultProvider"] != "openrouter" {
		t.Errorf("defaultProvider: got %v, want openrouter", cfg["defaultProvider"])
	}
}

// TestBuildConfigContent_ProfileAndProviderOverride verifies that --provider
// overrides the slot's provider on the root config content (issue #2852).
// The slot still supplies the model — provider and model are independent axes.
func TestBuildConfigContent_ProfileAndProviderOverride(t *testing.T) {
	pf := sampleProfilesFile()
	result, err := config.BuildConfigContent(pf, "anthropic", "coordinator", "", "", "openrouter")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if cfg["model"] == "" {
		t.Error("expected slot model to be present alongside the provider override")
	}
	if cfg["defaultProvider"] != "openrouter" {
		t.Errorf("defaultProvider: got %v, want openrouter (override must win over slot)", cfg["defaultProvider"])
	}
}

// TestBuildConfigContent_EmptyProviderFallsThrough verifies that an
// empty-string provider override produces no defaultProvider key — no blank
// override is ever emitted (issue #2852 edge case).
func TestBuildConfigContent_EmptyProviderFallsThrough(t *testing.T) {
	pf := sampleProfilesFile()
	result, err := config.BuildConfigContent(pf, "anthropic", "coordinator", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result), &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if _, ok := cfg["defaultProvider"]; ok {
		t.Error("unexpected defaultProvider key with empty provider override")
	}
}

// TestBuildConfigContent_UnknownProfile verifies that an unknown profile returns an error.
func TestBuildConfigContent_UnknownProfile(t *testing.T) {
	pf := sampleProfilesFile()
	_, err := config.BuildConfigContent(pf, "nonexistent", "coordinator", "", "", "")
	if err == nil {
		t.Fatal("expected error for unknown profile, got nil")
	}
}

// TestBuildConfigContent_OffThinkingTranslatesToNoneVariant verifies that
// thinking="off" produces variant="none".
func TestBuildConfigContent_OffThinkingTranslatesToNoneVariant(t *testing.T) {
	pf := &config.ProfilesFile{
		Default: "off-test",
		Profiles: map[string]config.ProfileEntry{
			"off-test": {
				"coordinator": {Provider: "anthropic", Model: "anthropic/claude-opus-4-7", Thinking: "off"},
				"worker":      {Provider: "anthropic", Model: "anthropic/claude-sonnet-4-6", Thinking: "off"},
			},
		},
	}
	out, err := config.BuildConfigContent(pf, "off-test", "coordinator", "", "", "")
	if err != nil {
		t.Fatalf("BuildConfigContent: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	// thinking="off" must produce variant="none", not "off".
	if v, hasVariant := cfg["variant"]; hasVariant && v == "off" {
		t.Errorf("variant = 'off' — should have been translated to 'none'")
	}
	if v, hasVariant := cfg["variant"]; hasVariant && v != "none" {
		t.Errorf("variant = %v, want none", v)
	}
}

// TestBuildConfigContent_NonZeroThinkingPassesThrough verifies that a
// non-zero thinking level (e.g. "medium") is passed through unchanged.
func TestBuildConfigContent_NonZeroThinkingPassesThrough(t *testing.T) {
	pf := sampleProfilesFile()
	out, err := config.BuildConfigContent(pf, "gemini-hybrid", "worker", "", "", "")
	if err != nil {
		t.Fatalf("BuildConfigContent: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if cfg["variant"] != "medium" {
		t.Errorf("variant: got %v, want medium", cfg["variant"])
	}
}

// TestBuildConfigContent_AnthropicCoordinatorNotEqualWorker verifies that
// coordinator and worker resolve different models for the anthropic profile
// (edge-case AC from #1612).
func TestBuildConfigContent_AnthropicCoordinatorNotEqualWorker(t *testing.T) {
	pf := sampleProfilesFile()

	coordResult, err := config.BuildConfigContent(pf, "anthropic", "coordinator", "", "", "")
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	workerResult, err := config.BuildConfigContent(pf, "anthropic", "worker", "", "", "")
	if err != nil {
		t.Fatalf("worker: %v", err)
	}

	var coordCfg, workerCfg map[string]any
	_ = json.Unmarshal([]byte(coordResult), &coordCfg)
	_ = json.Unmarshal([]byte(workerResult), &workerCfg)

	// Coordinator gets opus, worker gets sonnet.
	if coordCfg["model"] == workerCfg["model"] {
		t.Errorf("coordinator and worker resolved the same model %v — expected different slots",
			coordCfg["model"])
	}
	if coordCfg["model"] != "anthropic/claude-opus-4-7" {
		t.Errorf("coordinator model: got %v, want anthropic/claude-opus-4-7", coordCfg["model"])
	}
	if workerCfg["model"] != "anthropic/claude-sonnet-4-6" {
		t.Errorf("worker model: got %v, want anthropic/claude-sonnet-4-6", workerCfg["model"])
	}
}

// TestBuildConfigContent_WorkerModelOverrideDoesNotAffectOtherRoles verifies
// the edge case: --profile anthropic --agent worker --model haiku overrides
// only the worker's model and leaves other roles' configs unaffected.
// (We verify by calling BuildConfigContent with "coordinator" separately.)
func TestBuildConfigContent_WorkerModelOverrideDoesNotAffectOtherRoles(t *testing.T) {
	pf := sampleProfilesFile()
	// Spawn with worker role + model override.
	workerResult, err := config.BuildConfigContent(pf, "anthropic", "worker", "anthropic/claude-haiku-4-5", "", "")
	if err != nil {
		t.Fatalf("worker override: %v", err)
	}
	var workerCfg map[string]any
	_ = json.Unmarshal([]byte(workerResult), &workerCfg)
	if workerCfg["model"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("worker model: got %v, want anthropic/claude-haiku-4-5", workerCfg["model"])
	}

	// Coordinator role (separate call, simulating a different session) must
	// retain its profile model — the worker override must not bleed over.
	coordResult, err := config.BuildConfigContent(pf, "anthropic", "coordinator", "", "", "")
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	var coordCfg map[string]any
	_ = json.Unmarshal([]byte(coordResult), &coordCfg)
	if coordCfg["model"] != "anthropic/claude-opus-4-7" {
		t.Errorf("coordinator model after worker override: got %v, want anthropic/claude-opus-4-7 (unaffected)",
			coordCfg["model"])
	}
}

// ── SlotForRole tests ─────────────────────────────────────────────────────────

func TestSlotForRole_HitsAndMisses(t *testing.T) {
	pf := sampleProfilesFile()

	// coordinator is present in "anthropic".
	slot, ok := config.SlotForRole(pf, "anthropic", "coordinator")
	if !ok {
		t.Fatal("expected coordinator slot for anthropic")
	}
	if slot.Model != "anthropic/claude-opus-4-7" {
		t.Errorf("coordinator model: got %q, want anthropic/claude-opus-4-7", slot.Model)
	}
	if slot.Provider != "anthropic" {
		t.Errorf("coordinator provider: got %q, want anthropic", slot.Provider)
	}

	// review-goal is present.
	slot, ok = config.SlotForRole(pf, "anthropic", "review-goal")
	if !ok {
		t.Fatal("expected review-goal slot for anthropic")
	}
	if slot.Model != "anthropic/claude-sonnet-4-6" {
		t.Errorf("review-goal model: got %q, want anthropic/claude-sonnet-4-6", slot.Model)
	}

	// "build" is not a pi role — clean miss.
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

// ── RequireSlot tests ─────────────────────────────────────────────────────────

func TestRequireSlot_PassesWhenPresent(t *testing.T) {
	pf := sampleProfilesFile()
	if err := config.RequireSlot(pf, "anthropic", "coordinator"); err != nil {
		t.Errorf("RequireSlot(anthropic, coordinator): unexpected error: %v", err)
	}
	if err := config.RequireSlot(pf, "gemini-hybrid", "worker"); err != nil {
		t.Errorf("RequireSlot(gemini-hybrid, worker): unexpected error: %v", err)
	}
}

func TestRequireSlot_FailsWhenSlotMissing(t *testing.T) {
	pf := &config.ProfilesFile{
		Default: "minimal",
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
	if !contains(msg, "coordinator") {
		t.Errorf("error %q does not mention the missing role 'coordinator'", msg)
	}
	if !contains(msg, "minimal") {
		t.Errorf("error %q does not mention the profile 'minimal'", msg)
	}
	if !contains(msg, "worker") {
		t.Errorf("error %q does not list defined slots (expected to see 'worker')", msg)
	}
}

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

func TestRequireSlot_NilProfilesFile(t *testing.T) {
	err := config.RequireSlot(nil, "anthropic", "coordinator")
	if err == nil {
		t.Fatal("expected error for nil pf, got nil")
	}
}

// ── LoadProfiles tests ────────────────────────────────────────────────────────

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

// TestLoadProfiles_StaleKeysIgnored verifies that an old profiles.json with
// stale keys (role_mapping, default_harness, container_*_config) loads
// cleanly — unknown JSON fields are ignored.
func TestLoadProfiles_StaleKeysIgnored(t *testing.T) {
	dir := t.TempDir()
	prismDir := filepath.Join(dir, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const body = `{
  "default": "anthropic",
  "default_harness": "pi",
  "role_mapping": {"primary": ["coordinator"], "secondary": ["worker"]},
  "container_worker_config": "{}",
  "container_coordinator_config": "{}",
  "profiles": {"anthropic": {"coordinator": {"model": "anthropic/claude-opus-4-7"}}}
}`
	if err := os.WriteFile(filepath.Join(prismDir, "profiles.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)

	pf, err := config.LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: unexpected error for stale keys: %v", err)
	}
	if pf.Default != "anthropic" {
		t.Errorf("default: got %q, want anthropic", pf.Default)
	}
}

// contains is a substring helper.
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
