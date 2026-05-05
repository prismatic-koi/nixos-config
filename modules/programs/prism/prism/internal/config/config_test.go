package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

func TestDefaults(t *testing.T) {
	t.Setenv("PRISM_CONFIG_FILE", "/nonexistent/path/config.json")

	cfg := config.LoadFresh()

	if cfg.ColorPrimary != "#d4be98" {
		t.Errorf("ColorPrimary: got %q, want %q", cfg.ColorPrimary, "#d4be98")
	}
	if cfg.KittyBin != "kitty" {
		t.Errorf("KittyBin: got %q, want %q", cfg.KittyBin, "kitty")
	}
	if len(cfg.ProjectLocations) != 1 || cfg.ProjectLocations[0] != "~/code" {
		t.Errorf("ProjectLocations: got %v, want [~/code]", cfg.ProjectLocations)
	}
	if len(cfg.WorktreeExclude) != 1 || cfg.WorktreeExclude[0] != "obsidian" {
		t.Errorf("WorktreeExclude: got %v, want [obsidian]", cfg.WorktreeExclude)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	raw := `{
		"color_primary": "#ff0000",
		"kitty_bin": "/nix/store/abc/bin/kitty",
		"project_locations": ["~/projects", "~/work"]
	}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	if cfg.ColorPrimary != "#ff0000" {
		t.Errorf("ColorPrimary: got %q, want %q", cfg.ColorPrimary, "#ff0000")
	}
	if cfg.KittyBin != "/nix/store/abc/bin/kitty" {
		t.Errorf("KittyBin: got %q, want %q", cfg.KittyBin, "/nix/store/abc/bin/kitty")
	}
	if len(cfg.ProjectLocations) != 2 || cfg.ProjectLocations[0] != "~/projects" || cfg.ProjectLocations[1] != "~/work" {
		t.Errorf("ProjectLocations: got %v, want [~/projects ~/work]", cfg.ProjectLocations)
	}
	if cfg.ColorSecondary != "#a89984" {
		t.Errorf("ColorSecondary: got %q, want %q (should be default)", cfg.ColorSecondary, "#a89984")
	}
}

func TestEmptyArrayClearsDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	raw := `{"worktree_exclude": [], "project_specific": []}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	if len(cfg.WorktreeExclude) != 0 {
		t.Errorf("WorktreeExclude: got %v, want [] (explicit empty should clear default)", cfg.WorktreeExclude)
	}
	if len(cfg.ProjectSpecific) != 0 {
		t.Errorf("ProjectSpecific: got %v, want [] (explicit empty should clear default)", cfg.ProjectSpecific)
	}
	if len(cfg.ProjectLocations) != 1 || cfg.ProjectLocations[0] != "~/code" {
		t.Errorf("ProjectLocations: got %v, want [~/code] (absent key should keep default)", cfg.ProjectLocations)
	}
}

func TestSshKeyNameDefaults(t *testing.T) {
	// When no config file is present, SshAccessKeyName and SshSigningKeyName
	// should return the compiled-in defaults from defaults().
	t.Setenv("PRISM_CONFIG_FILE", "/nonexistent/path/config.json")

	cfg := config.LoadFresh()

	if cfg.SshAccessKeyName != "prismatic-koi-ed25519" {
		t.Errorf("SshAccessKeyName: got %q, want %q", cfg.SshAccessKeyName, "prismatic-koi-ed25519")
	}
	if cfg.SshSigningKeyName != "prismatic-koi-ed25519-signingkey" {
		t.Errorf("SshSigningKeyName: got %q, want %q", cfg.SshSigningKeyName, "prismatic-koi-ed25519-signingkey")
	}
}

func TestSshKeyNameOverriddenFromFile(t *testing.T) {
	// When ssh_access_key_name and ssh_signing_key_name are set in the config
	// file, the loaded values should override the compiled-in defaults.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	raw := `{
		"ssh_access_key_name": "my-access-key",
		"ssh_signing_key_name": "my-signing-key"
	}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	if cfg.SshAccessKeyName != "my-access-key" {
		t.Errorf("SshAccessKeyName: got %q, want %q", cfg.SshAccessKeyName, "my-access-key")
	}
	if cfg.SshSigningKeyName != "my-signing-key" {
		t.Errorf("SshSigningKeyName: got %q, want %q", cfg.SshSigningKeyName, "my-signing-key")
	}
}

func TestSshKeyNameAbsentKeepsDefault(t *testing.T) {
	// When ssh_access_key_name and ssh_signing_key_name are absent from the
	// config file, the compiled-in defaults are kept (not empty string).
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// Config file exists but doesn't mention SSH key names.
	raw := `{"color_primary": "#ff0000"}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	if cfg.SshAccessKeyName != "prismatic-koi-ed25519" {
		t.Errorf("SshAccessKeyName: got %q, want default %q when absent from config", cfg.SshAccessKeyName, "prismatic-koi-ed25519")
	}
	if cfg.SshSigningKeyName != "prismatic-koi-ed25519-signingkey" {
		t.Errorf("SshSigningKeyName: got %q, want default %q when absent from config", cfg.SshSigningKeyName, "prismatic-koi-ed25519-signingkey")
	}
}

func TestSshBinOverriddenFromFile(t *testing.T) {
	// When ssh_bin is set in the config file, the loaded value should override
	// the zero default. This is used by prism-tui.nix to bake the absolute
	// Nix-store openssh path into config.json at darwin-rebuild switch time.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	raw := `{"ssh_bin": "/nix/store/abc123-openssh-10.3p1/bin/ssh"}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	if cfg.SshBin != "/nix/store/abc123-openssh-10.3p1/bin/ssh" {
		t.Errorf("SshBin: got %q, want %q", cfg.SshBin, "/nix/store/abc123-openssh-10.3p1/bin/ssh")
	}
}

func TestSshBinAbsentKeepsEmpty(t *testing.T) {
	// When ssh_bin is absent from the config file, SshBin should remain empty
	// (its zero value). The GIT_SSH_COMMAND code in agent_run_sandbox_exec_darwin.go
	// falls back to bare "ssh" when SshBin is empty.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// Config file exists but doesn't mention ssh_bin.
	raw := `{"color_primary": "#ff0000"}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	if cfg.SshBin != "" {
		t.Errorf("SshBin: got %q, want empty string when absent from config", cfg.SshBin)
	}
}

func TestIsolationModeFromNewField(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	raw := `{"default_isolation_mode": "bwrap"}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)
	cfg := config.LoadFresh()

	if cfg.DefaultIsolationMode != config.IsolationBwrap {
		t.Errorf("DefaultIsolationMode: got %q, want %q", cfg.DefaultIsolationMode, config.IsolationBwrap)
	}
}

// TestContainerModeSilentlyIgnored verifies the AC [edge-case]: a config file
// containing "container_mode": true loads without error, produces no warning
// to stderr, and resolves to the new default IsolationHost. The field is
// unknown to parsedConfig (removed in A4.PB), so Go's JSON decoder silently
// drops it, and DefaultIsolationMode falls back to the compiled-in default.
func TestContainerModeSilentlyIgnored(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// container_mode=true only — no default_isolation_mode.
	raw := `{"container_mode": true}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)
	cfg := config.LoadFresh()

	// container_mode is silently dropped by the JSON decoder; DefaultIsolationMode
	// should be the compiled-in default "host".
	if cfg.DefaultIsolationMode != config.IsolationHost {
		t.Errorf("DefaultIsolationMode: got %q, want %q (container_mode ignored; default applies)", cfg.DefaultIsolationMode, config.IsolationHost)
	}
}

func TestIsolationModeDefaultWhenNeitherFieldPresent(t *testing.T) {
	t.Setenv("PRISM_CONFIG_FILE", "/nonexistent/path/config.json")
	cfg := config.LoadFresh()

	// No config file — DefaultIsolationMode should be the compiled-in default "host".
	if cfg.DefaultIsolationMode != config.IsolationHost {
		t.Errorf("DefaultIsolationMode: got %q, want %q (default when no config)", cfg.DefaultIsolationMode, config.IsolationHost)
	}
}

func TestIsolationModeInvalidIgnored(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// An invalid mode string should be ignored; fall back to the compiled-in default.
	raw := `{"default_isolation_mode": "unknown-mode"}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)
	cfg := config.LoadFresh()

	// Invalid mode is ignored; DefaultIsolationMode falls back to compiled-in default "host".
	if cfg.DefaultIsolationMode != config.IsolationHost {
		t.Errorf("DefaultIsolationMode: got %q, want %q (invalid mode falls back to default)", cfg.DefaultIsolationMode, config.IsolationHost)
	}
}

func TestMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	if err := os.WriteFile(cfgPath, []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	if cfg.ColorPrimary != "#d4be98" {
		t.Errorf("ColorPrimary: got %q, want default %q", cfg.ColorPrimary, "#d4be98")
	}
	if len(cfg.ProjectLocations) != 1 || cfg.ProjectLocations[0] != "~/code" {
		t.Errorf("ProjectLocations: got %v, want default [~/code]", cfg.ProjectLocations)
	}
}

// TestProjectIsolationOverridesDefault verifies that defaults() includes the
// Obsidian vault path mapped to "host" so a fresh Darwin install works without
// any config.json entry.
func TestProjectIsolationOverridesDefault(t *testing.T) {
	t.Setenv("PRISM_CONFIG_FILE", "/nonexistent/path/config.json")

	cfg := config.LoadFresh()

	if cfg.ProjectIsolationOverrides == nil {
		t.Fatal("ProjectIsolationOverrides: got nil, want non-nil map from defaults")
	}
	got, ok := cfg.ProjectIsolationOverrides["~/documents/obsidian"]
	if !ok {
		t.Errorf("ProjectIsolationOverrides: missing key %q", "~/documents/obsidian")
	} else if got != "host" {
		t.Errorf("ProjectIsolationOverrides[%q]: got %q, want %q", "~/documents/obsidian", got, "host")
	}
}

// TestProjectIsolationOverridesFromFile verifies that a config.json with
// project_isolation_overrides is loaded and replaces the defaults entirely.
func TestProjectIsolationOverridesFromFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	raw := `{"project_isolation_overrides": {"~/myproject": "bwrap"}}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	// The file replaces the defaults entirely (map replacement, not merge).
	if len(cfg.ProjectIsolationOverrides) != 1 {
		t.Errorf("ProjectIsolationOverrides: got %d entries, want 1", len(cfg.ProjectIsolationOverrides))
	}
	got, ok := cfg.ProjectIsolationOverrides["~/myproject"]
	if !ok {
		t.Errorf("ProjectIsolationOverrides: missing key %q", "~/myproject")
	} else if got != "bwrap" {
		t.Errorf("ProjectIsolationOverrides[%q]: got %q, want %q", "~/myproject", got, "bwrap")
	}
	// Defaults must be gone since the file provided a non-nil map.
	if _, obsidianPresent := cfg.ProjectIsolationOverrides["~/documents/obsidian"]; obsidianPresent {
		t.Errorf("ProjectIsolationOverrides: default key %q should not be present when file provides the map", "~/documents/obsidian")
	}
}

// TestProjectIsolationOverridesAbsentKeepsDefaults verifies that an absent
// project_isolation_overrides key in config.json leaves the compiled-in
// defaults intact.
func TestProjectIsolationOverridesAbsentKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// Config file exists but has no project_isolation_overrides key.
	raw := `{"color_primary": "#ff0000"}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	got, ok := cfg.ProjectIsolationOverrides["~/documents/obsidian"]
	if !ok {
		t.Errorf("ProjectIsolationOverrides: default key %q missing when key absent from config", "~/documents/obsidian")
	} else if got != "host" {
		t.Errorf("ProjectIsolationOverrides[%q]: got %q, want %q", "~/documents/obsidian", got, "host")
	}
}

// TestProjectIsolationOverridesEmptyMapClearsDefaults verifies that an
// explicit empty object {} in config.json replaces defaults with an empty map.
func TestProjectIsolationOverridesEmptyMapClearsDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	raw := `{"project_isolation_overrides": {}}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	if len(cfg.ProjectIsolationOverrides) != 0 {
		t.Errorf("ProjectIsolationOverrides: got %d entries, want 0 (explicit empty should clear defaults)", len(cfg.ProjectIsolationOverrides))
	}
}

// TestIsolationOverrideForPathMatch verifies that IsolationOverrideForPath
// returns the correct mode for a path that matches a configured override.
func TestIsolationOverrideForPathMatch(t *testing.T) {
	t.Setenv("PRISM_CONFIG_FILE", "/nonexistent/path/config.json")

	cfg := config.LoadFresh()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home dir: %v", err)
	}

	obsidianPath := filepath.Join(home, "documents/obsidian")
	got := cfg.IsolationOverrideForPath(obsidianPath)
	if got != config.IsolationHost {
		t.Errorf("IsolationOverrideForPath(%q): got %q, want %q", obsidianPath, got, config.IsolationHost)
	}
}

// TestIsolationOverrideForPathTildeKeyMatch verifies that a "~/" key in
// ProjectIsolationOverrides matches an already-expanded absolute path.
func TestIsolationOverrideForPathTildeKeyMatch(t *testing.T) {
	t.Setenv("PRISM_CONFIG_FILE", "/nonexistent/path/config.json")

	cfg := config.LoadFresh()

	// The default key "~/documents/obsidian" should match the tilde form too.
	got := cfg.IsolationOverrideForPath("~/documents/obsidian")
	if got != config.IsolationHost {
		t.Errorf("IsolationOverrideForPath(\"~/documents/obsidian\"): got %q, want %q", got, config.IsolationHost)
	}
}

// TestIsolationOverrideForPathNoMatch verifies that IsolationOverrideForPath
// returns "" for a path that does not match any override.
func TestIsolationOverrideForPathNoMatch(t *testing.T) {
	t.Setenv("PRISM_CONFIG_FILE", "/nonexistent/path/config.json")

	cfg := config.LoadFresh()

	got := cfg.IsolationOverrideForPath("/some/other/path")
	if got != "" {
		t.Errorf("IsolationOverrideForPath(%q): got %q, want empty string", "/some/other/path", got)
	}
}

// TestIsolationOverrideForPathInvalidMode verifies that an override entry with
// an invalid isolation mode value is silently ignored (returns "").
func TestIsolationOverrideForPathInvalidMode(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	raw := `{"project_isolation_overrides": {"~/myproject": "unknown-mode"}}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	got := cfg.IsolationOverrideForPath("~/myproject")
	if got != "" {
		t.Errorf("IsolationOverrideForPath(%q): got %q, want empty string (invalid mode ignored)", "~/myproject", got)
	}
}

// TestIsolationOverrideForPathEmptyMap verifies that IsolationOverrideForPath
// returns "" when ProjectIsolationOverrides is empty.
func TestIsolationOverrideForPathEmptyMap(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	raw := `{"project_isolation_overrides": {}}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_CONFIG_FILE", cfgPath)

	cfg := config.LoadFresh()

	got := cfg.IsolationOverrideForPath("~/documents/obsidian")
	if got != "" {
		t.Errorf("IsolationOverrideForPath(%q): got %q, want empty string (empty overrides map)", "~/documents/obsidian", got)
	}
}
