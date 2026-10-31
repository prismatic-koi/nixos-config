package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// TestHoldsProjectRoots covers each dir that holds project roots (#3381):
// a project location, a dir above it, and a dir above a project_specific
// root. A project root, a dir in it, and a dir outside each entry hold none.
func TestHoldsProjectRoots(t *testing.T) {
	base := t.TempDir()
	code := filepath.Join(base, "home", "code")
	obsidian := filepath.Join(base, "home", "documents", "obsidian")
	notes := filepath.Join(base, "home", "notes")
	for _, d := range []string{
		filepath.Join(code, "repo", ".bare"),
		filepath.Join(code, "repo", "main", "sub"),
		filepath.Join(code, "scratch"),
		obsidian,
		notes,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	linkToCode := filepath.Join(base, "link-to-code")
	if err := os.Symlink(code, linkToCode); err != nil {
		t.Fatal(err)
	}
	linkToHome := filepath.Join(notes, "link-to-home")
	if err := os.Symlink(filepath.Join(base, "home"), linkToHome); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "missing", "code")
	locations := []string{code, missing}
	specific := []string{obsidian}

	for _, tc := range []struct {
		name         string
		dir          string
		wantEntry    string
		wantLocation bool
	}{
		{"the project location", code, code, true},
		{"the project location with a trailing slash", code + "/", code, true},
		{"a dir above the project location", filepath.Join(base, "home"), code, true},
		{"the filesystem root", "/", code, true},
		{"a symlink to the project location", linkToCode, code, true},
		{"a symlink to a dir above the project location", linkToHome, code, true},
		{"a dir above a project location that does not exist", filepath.Join(base, "missing"), missing, true},
		{"a dir above a project_specific root", filepath.Join(base, "home", "documents"), obsidian, false},
		{"a project root with a .bare", filepath.Join(code, "repo"), "", false},
		{"a project root that is not a repo", filepath.Join(code, "scratch"), "", false},
		{"a worktree", filepath.Join(code, "repo", "main"), "", false},
		{"a dir in a worktree", filepath.Join(code, "repo", "main", "sub"), "", false},
		{"a project_specific root", obsidian, "", false},
		{"a dir outside each entry", notes, "", false},
		{"no dir", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry, location, ok := config.HoldsProjectRoots(locations, specific, tc.dir)
			if ok != (tc.wantEntry != "") || entry != tc.wantEntry || location != tc.wantLocation {
				t.Errorf("HoldsProjectRoots(%q) = (%q, %v, %v), want (%q, %v, %v)",
					tc.dir, entry, location, ok, tc.wantEntry, tc.wantLocation, tc.wantEntry != "")
			}
		})
	}
}

// TestHoldsProjectRoots_ExpandsHome makes sure that a "~/" entry, as in the
// default config.json, matches the home dir.
func TestHoldsProjectRoots_ExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "code"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry, location, ok := config.Config{ProjectLocations: []string{"~/code"}}.HoldsProjectRoots(home)
	if !ok || !location || entry != filepath.Join(home, "code") {
		t.Errorf("HoldsProjectRoots(%q) = (%q, %v, %v), want (%q, true, true)", home, entry, location, ok, filepath.Join(home, "code"))
	}
}
