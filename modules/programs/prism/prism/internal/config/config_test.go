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
