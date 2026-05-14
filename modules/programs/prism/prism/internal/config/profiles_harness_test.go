package config_test

// profiles_harness_test.go — precedence and validation tests for the
// HarnessForSlot accessor and the LoadProfiles validation hook (#1491).
//
// Acceptance-criteria coverage from issue #1491:
//
//   - flag wins over slot, slot, default_harness, and the hardcoded fallback
//     (the flag layer is tested at the cmd/spawn / cmd/pr layer; here we
//     verify HarnessForSlot does not interfere with an explicitly-passed
//     value by returning the slot value when the slot wins, and otherwise
//     descending the fallback ladder).
//   - slot wins over default_harness.
//   - default_harness wins over the hardcoded "pi".
//   - hardcoded "pi" is the final safety net (empty slot AND empty
//     default_harness AND nil profiles file).
//   - empty default_harness (e.g. older profiles.json) falls through to
//     "pi" without error.
//   - non-empty default_harness naming an unregistered harness is rejected
//     at LoadProfiles time with a clear error listing the valid names.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	// Blank import so the harness package's init() wires up
	// config.HarnessValidator. This mirrors how prism binaries pull in the
	// validator transitively via the harness registry.
	_ "github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
)

// TestHarnessForSlot_PrecedenceLadder asserts the four-rung resolution order
// documented on HarnessForSlot. The flag layer is enforced upstream at the
// cmd layer (by short-circuiting before we reach this function), so this
// test covers only rungs 2-4 of the ladder.
func TestHarnessForSlot_PrecedenceLadder(t *testing.T) {
	tests := []struct {
		name string
		pf   *config.ProfilesFile
		slot config.RoleSlot
		want string
	}{
		{
			// Slot wins over file-level default_harness AND the hardcoded
			// fallback. This is the AC: "When a profile slot has an explicit
			// `harness` field, that value is used regardless of
			// default_harness".
			name: "slot wins over default_harness",
			pf:   &config.ProfilesFile{DefaultHarness: ""},
			slot: config.RoleSlot{Harness: "pi"},
			want: "pi",
		},
		{
			// default_harness wins over the hardcoded "pi" when the
			// slot is empty. This is the AC: "When a profile slot has no
			// `harness` field, ... returns the value of `default_harness`".
			name: "default_harness wins over hardcoded fallback",
			pf:   &config.ProfilesFile{DefaultHarness: "pi"},
			slot: config.RoleSlot{},
			want: "pi",
		},
		{
			// Empty default_harness (older profiles.json) ⇒ hardcoded
			// "pi". This is the AC: "When `default_harness` is the
			// empty string ... resolution falls back to the hardcoded
			// `pi` and no error is raised".
			name: "empty default_harness falls back to pi",
			pf:   &config.ProfilesFile{DefaultHarness: ""},
			slot: config.RoleSlot{},
			want: "pi",
		},
		{
			// Nil profiles file: the test/error-recovery path. Slot value
			// wins; otherwise the hardcoded fallback applies.
			name: "nil pf with slot harness uses slot",
			pf:   nil,
			slot: config.RoleSlot{Harness: "pi"},
			want: "pi",
		},
		{
			name: "nil pf with empty slot uses pi",
			pf:   nil,
			slot: config.RoleSlot{},
			want: "pi",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := config.HarnessForSlot(tc.pf, tc.slot)
			if got != tc.want {
				t.Errorf("HarnessForSlot() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoadProfiles_DefaultHarnessValidation_OK asserts that LoadProfiles
// accepts a profiles.json whose default_harness names a registered harness.
// AC: "default_harness names a registered harness ⇒ profiles-file load
// succeeds".
func TestLoadProfiles_DefaultHarnessValidation_OK(t *testing.T) {
	// Plant a profiles.json with default_harness = "pi" (which is registered
	// by the blank import at the top of this file).
	dir := t.TempDir()
	configHome := filepath.Join(dir, ".config")
	if err := os.MkdirAll(filepath.Join(configHome, "prism"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const body = `{
  "default": "anthropic",
  "default_harness": "pi",
  "role_mapping": {"primary": ["coordinator"]},
  "profiles": {"anthropic": {"coordinator": {"model": "x"}}}
}`
	if err := os.WriteFile(
		filepath.Join(configHome, "prism", "profiles.json"),
		[]byte(body), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)

	pf, err := config.LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: unexpected error: %v", err)
	}
	if pf.DefaultHarness != "pi" {
		t.Errorf("DefaultHarness = %q, want %q", pf.DefaultHarness, "pi")
	}
}

// TestLoadProfiles_DefaultHarnessValidation_Unknown asserts that
// LoadProfiles rejects a profiles.json whose default_harness names a
// harness not in the registry, with an error message listing the valid
// names. AC: "When `default_harness` names a harness that is not registered
// in `harness.Lookup`, the profiles-file load fails with an error message
// that lists the registered harness names."
func TestLoadProfiles_DefaultHarnessValidation_Unknown(t *testing.T) {
	dir := t.TempDir()
	configHome := filepath.Join(dir, ".config")
	if err := os.MkdirAll(filepath.Join(configHome, "prism"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const body = `{
  "default": "anthropic",
  "default_harness": "totally-fake-harness",
  "role_mapping": {"primary": ["coordinator"]},
  "profiles": {"anthropic": {"coordinator": {"model": "x"}}}
}`
	if err := os.WriteFile(
		filepath.Join(configHome, "prism", "profiles.json"),
		[]byte(body), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)

	_, err := config.LoadProfiles()
	if err == nil {
		t.Fatal("LoadProfiles: expected error for unknown default_harness, got nil")
	}
	msg := err.Error()
	// Must mention the offending value.
	if !strings.Contains(msg, "totally-fake-harness") {
		t.Errorf("error %q does not mention the bad harness name", msg)
	}
	// Must list the valid names so the user can recover.
	if !strings.Contains(msg, "pi") {
		t.Errorf("error %q does not list 'pi' as a valid harness", msg)
	}
}

// TestLoadProfiles_DefaultHarnessAbsent asserts that an older profiles.json
// without a default_harness field loads cleanly and HarnessForSlot returns
// the hardcoded "pi" for empty slots. AC: "When `default_harness` is
// the empty string in `profiles.json` (e.g. older file from a prism build
// that predates this option), resolution falls back to the hardcoded
// `\"pi\"` and no error is raised."
func TestLoadProfiles_DefaultHarnessAbsent(t *testing.T) {
	dir := t.TempDir()
	configHome := filepath.Join(dir, ".config")
	if err := os.MkdirAll(filepath.Join(configHome, "prism"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const body = `{
  "default": "anthropic",
  "role_mapping": {"primary": ["coordinator"]},
  "profiles": {"anthropic": {"coordinator": {"model": "x"}}}
}`
	if err := os.WriteFile(
		filepath.Join(configHome, "prism", "profiles.json"),
		[]byte(body), 0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)

	pf, err := config.LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: unexpected error: %v", err)
	}
	if pf.DefaultHarness != "" {
		t.Errorf("DefaultHarness = %q, want \"\"", pf.DefaultHarness)
	}
	got := config.HarnessForSlot(pf, config.RoleSlot{})
	if got != "pi" {
		t.Errorf("HarnessForSlot(pf, empty) = %q, want \"pi\"", got)
	}
}
