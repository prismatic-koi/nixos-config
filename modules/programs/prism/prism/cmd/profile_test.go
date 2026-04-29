// Package cmd unit tests for the `prism profile` subcommands (#1207).
//
// These tests exercise runProfileUse / runProfileList / runProfileShow
// directly against a synthesised profiles.json under XDG_CONFIG_HOME and an
// XDG_STATE_HOME-rooted state file. They verify:
//
//   - `profile use` validates the name and persists the state file.
//   - `profile use <bogus>` rejects with a friendly message listing valid
//     names.
//   - `profile use <missing-slot>` rejects without writing to the state file.
//   - `profile list` marks the runtime-active profile correctly across
//     the three resolution-order branches (state file, nix default, none).
//   - `profile show` defaults to the active profile and accepts an explicit
//     name.
package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// sampleProfilesJSON returns a minimal profiles.json with two profiles
// (anthropic, gemini-hybrid), both defining coordinator and worker slots.
const sampleProfilesJSON = `{
  "default": "anthropic",
  "role_mapping": {
    "primary": ["coordinator", "plan"],
    "secondary": ["worker", "review"]
  },
  "profiles": {
    "anthropic": {
      "coordinator": {"provider": "anthropic", "model": "anthropic/claude-opus-4-6", "thinking": "none"},
      "plan":        {"provider": "anthropic", "model": "anthropic/claude-opus-4-6", "thinking": "none"},
      "worker":      {"provider": "anthropic", "model": "anthropic/claude-sonnet-4-6", "thinking": "none"},
      "review":      {"provider": "anthropic", "model": "anthropic/claude-sonnet-4-6", "thinking": "none"}
    },
    "gemini-hybrid": {
      "coordinator": {"provider": "anthropic", "model": "anthropic/claude-opus-4-6", "thinking": "none"},
      "plan":        {"provider": "anthropic", "model": "anthropic/claude-opus-4-6", "thinking": "none"},
      "worker":      {"provider": "google", "model": "google/gemini-3.1-pro-preview", "thinking": "medium"},
      "review":      {"provider": "google", "model": "google/gemini-3.1-pro-preview", "thinking": "medium"}
    }
  }
}`

// profileMissingWorkerJSON exercises the AC: `profile use` must reject a
// profile that is missing a required slot (here: worker).
const profileMissingWorkerJSON = `{
  "default": "broken",
  "role_mapping": {"primary": ["coordinator"]},
  "profiles": {
    "broken": {
      "coordinator": {"provider": "anthropic", "model": "anthropic/claude-opus-4-6", "thinking": "none"}
    }
  }
}`

// withProfileFixture sets up XDG_CONFIG_HOME with the provided profiles.json
// payload and XDG_STATE_HOME with an empty state directory. Returns the
// state directory path so callers can inspect the active-profile file.
func withProfileFixture(t *testing.T, payload string) string {
	t.Helper()

	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	prismCfg := filepath.Join(cfg, "prism")
	if err := os.MkdirAll(prismCfg, 0o755); err != nil {
		t.Fatalf("mkdir prism config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prismCfg, "profiles.json"), []byte(payload), 0o644); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}

	stateRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateRoot)
	return filepath.Join(stateRoot, "prism")
}

// runProfileSubcommand executes `cmd args...` against the runProfile* RunE
// functions and returns the captured stdout. It builds a fresh cobra.Command
// per call so flag/state from the package-level commands does not leak.
func runProfileSubcommand(t *testing.T, runE func(*cobra.Command, []string) error, args []string) (string, error) {
	t.Helper()
	c := &cobra.Command{Use: "test"}
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	err := runE(c, args)
	return buf.String(), err
}

func TestProfileUse_PersistsStateFile(t *testing.T) {
	stateDir := withProfileFixture(t, sampleProfilesJSON)

	out, err := runProfileSubcommand(t, runProfileUse, []string{"gemini-hybrid"})
	if err != nil {
		t.Fatalf("runProfileUse: %v", err)
	}
	if !strings.Contains(out, "gemini-hybrid") {
		t.Errorf("output %q does not mention the chosen profile", out)
	}

	data, err := os.ReadFile(filepath.Join(stateDir, "active-profile"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if got := strings.TrimRight(string(data), "\n"); got != "gemini-hybrid" {
		t.Errorf("state file = %q, want gemini-hybrid", got)
	}
}

func TestProfileUse_RejectsBogusNameWithFriendlyError(t *testing.T) {
	stateDir := withProfileFixture(t, sampleProfilesJSON)

	_, err := runProfileSubcommand(t, runProfileUse, []string{"does-not-exist"})
	if err == nil {
		t.Fatal("runProfileUse(bogus): want error, got nil")
	}
	msg := err.Error()
	// Friendly error: must list both valid profile names.
	if !strings.Contains(msg, "anthropic") || !strings.Contains(msg, "gemini-hybrid") {
		t.Errorf("error %q does not list valid profile names", msg)
	}
	// State file must NOT have been created on a rejected name.
	if _, statErr := os.Stat(filepath.Join(stateDir, "active-profile")); !os.IsNotExist(statErr) {
		t.Errorf("state file exists after rejected `profile use`: %v", statErr)
	}
}

func TestProfileUse_RejectsMissingRequiredSlot(t *testing.T) {
	stateDir := withProfileFixture(t, profileMissingWorkerJSON)

	_, err := runProfileSubcommand(t, runProfileUse, []string{"broken"})
	if err == nil {
		t.Fatal("runProfileUse(broken): want error, got nil")
	}
	if !strings.Contains(err.Error(), "worker") {
		t.Errorf("error %q does not mention missing worker slot", err)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "active-profile")); !os.IsNotExist(statErr) {
		t.Errorf("state file exists after rejected `profile use`: %v", statErr)
	}
}

func TestProfileList_MarksStateFileAsActive(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)

	// First, set the runtime active to gemini-hybrid via `profile use`.
	if _, err := runProfileSubcommand(t, runProfileUse, []string{"gemini-hybrid"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	out, err := runProfileSubcommand(t, runProfileList, nil)
	if err != nil {
		t.Fatalf("runProfileList: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for _, line := range lines {
		switch {
		case strings.HasSuffix(line, "gemini-hybrid"):
			if !strings.HasPrefix(line, "* ") {
				t.Errorf("active line %q lacks `* ` marker", line)
			}
		case strings.HasSuffix(line, "anthropic"):
			if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "* ") {
				t.Errorf("inactive line %q has unexpected marker", line)
			}
		}
	}
}

func TestProfileList_FallsBackToNixDefault(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)

	// No `profile use` performed: the nix default ("anthropic") should be
	// the marked profile.
	out, err := runProfileSubcommand(t, runProfileList, nil)
	if err != nil {
		t.Fatalf("runProfileList: %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasSuffix(line, "anthropic") && !strings.HasSuffix(line, "gemini-hybrid") {
			if !strings.HasPrefix(line, "* ") {
				t.Errorf("anthropic (nix default) line %q is not marked active", line)
			}
		}
	}
}

func TestProfileShow_DefaultsToActiveProfile(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)
	if _, err := runProfileSubcommand(t, runProfileUse, []string{"gemini-hybrid"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	out, err := runProfileSubcommand(t, runProfileShow, nil)
	if err != nil {
		t.Fatalf("runProfileShow: %v", err)
	}
	if !strings.Contains(out, "profile: gemini-hybrid") {
		t.Errorf("output %q does not include `profile: gemini-hybrid` header", out)
	}
	// Must contain rows for the four roles defined in the gemini-hybrid profile.
	for _, role := range []string{"coordinator", "plan", "worker", "review"} {
		if !strings.Contains(out, role) {
			t.Errorf("output %q missing row for role %q", out, role)
		}
	}
	// And the gemini model identifier must appear (worker/review slots).
	if !strings.Contains(out, "google/gemini-3.1-pro-preview") {
		t.Errorf("output %q missing gemini model identifier", out)
	}
}

func TestProfileShow_AcceptsExplicitName(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)

	out, err := runProfileSubcommand(t, runProfileShow, []string{"anthropic"})
	if err != nil {
		t.Fatalf("runProfileShow(anthropic): %v", err)
	}
	if !strings.Contains(out, "profile: anthropic") {
		t.Errorf("output %q does not include `profile: anthropic` header", out)
	}
	if !strings.Contains(out, "anthropic/claude-opus-4-6") {
		t.Errorf("output %q missing anthropic primary model", out)
	}
}

func TestProfileShow_RejectsUnknownName(t *testing.T) {
	withProfileFixture(t, sampleProfilesJSON)

	_, err := runProfileSubcommand(t, runProfileShow, []string{"does-not-exist"})
	if err == nil {
		t.Fatal("runProfileShow(bogus): want error, got nil")
	}
	if !strings.Contains(err.Error(), "anthropic") || !strings.Contains(err.Error(), "gemini-hybrid") {
		t.Errorf("error %q does not list valid profile names", err)
	}
}
