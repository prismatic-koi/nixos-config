package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// sampleProfilesFile returns a minimal ProfilesFile for testing under the
// flat per-role profile schema. Each profile is a direct map from
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

// ── RequireProfile tests ─────────────────────────────────────────────────
//
// RequireProfile carries the profile-existence half of RequireSlot for call
// sites that must validate before the session role is known.

func TestRequireProfile_PassesWhenPresent(t *testing.T) {
	pf := sampleProfilesFile()
	if err := config.RequireProfile(pf, "anthropic"); err != nil {
		t.Errorf("RequireProfile(anthropic): unexpected error: %v", err)
	}
}

// TestRequireProfile_FailsWhenProfileUnknown is the regression shape:
// a state file naming a profile that profiles.json does not define
// (e.g. "ox-alpha") must be rejected, and the message must list what IS
// available so the user can recover.
func TestRequireProfile_FailsWhenProfileUnknown(t *testing.T) {
	pf := sampleProfilesFile()
	err := config.RequireProfile(pf, "ox-alpha")
	if err == nil {
		t.Fatal("expected error for unknown profile, got nil")
	}
	if !contains(err.Error(), "ox-alpha") {
		t.Errorf("error must name the unknown profile; got %q", err)
	}
	if !contains(err.Error(), "anthropic") {
		t.Errorf("error must list available profiles; got %q", err)
	}
}

func TestRequireProfile_NilProfilesFile(t *testing.T) {
	err := config.RequireProfile(nil, "anthropic")
	if err == nil {
		t.Fatal("expected error for nil pf, got nil")
	}
	if !contains(err.Error(), "not loaded") {
		t.Errorf("nil-pf error must say the profiles file was not loaded; got %q", err)
	}
}

// TestRequireSlot_DelegatesUnknownProfileToRequireProfile pins the delegation
// so the two call sites cannot drift into two different error texts.
func TestRequireSlot_DelegatesUnknownProfileToRequireProfile(t *testing.T) {
	pf := sampleProfilesFile()
	slotErr := config.RequireSlot(pf, "ox-alpha", "coordinator")
	profErr := config.RequireProfile(pf, "ox-alpha")
	if slotErr == nil || profErr == nil {
		t.Fatal("both must error for an unknown profile")
	}
	if slotErr.Error() != profErr.Error() {
		t.Errorf("unknown-profile message drifted:\n RequireSlot:    %q\n RequireProfile: %q", slotErr, profErr)
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
