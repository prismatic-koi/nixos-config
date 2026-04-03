package config_test

import (
	"encoding/json"
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
	if cfg.ProjectLocations != "~/code" {
		t.Errorf("ProjectLocations: got %q, want %q", cfg.ProjectLocations, "~/code")
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	data := config.Config{
		ColorPrimary:     "#ff0000",
		KittyBin:         "/nix/store/abc/bin/kitty",
		ProjectLocations: "~/projects:~/work",
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
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
	if cfg.ProjectLocations != "~/projects:~/work" {
		t.Errorf("ProjectLocations: got %q, want %q", cfg.ProjectLocations, "~/projects:~/work")
	}
	// Unset fields should fall back to defaults.
	if cfg.ColorSecondary != "#a89984" {
		t.Errorf("ColorSecondary: got %q, want %q (should be default)", cfg.ColorSecondary, "#a89984")
	}
}

func TestSplitColon(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a:b:c", []string{"a", "b", "c"}},
		{"a::b", []string{"a", "b"}},
	}
	for _, c := range cases {
		got := config.SplitColon(c.in)
		if len(got) != len(c.want) {
			t.Errorf("SplitColon(%q): got %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("SplitColon(%q)[%d]: got %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
