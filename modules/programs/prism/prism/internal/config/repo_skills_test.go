package config_test

// repo_skills_test.go — LoadRepoSkills is the only source of the --skill
// paths that prism gives pi (issue #3463). Every path that cannot produce a
// map must return an error, so the caller logs a warning and passes no
// --skill flag.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// withRepoSkillsFile points $XDG_CONFIG_HOME at a temp dir. A nil body
// leaves repo-skills.json absent.
func withRepoSkillsFile(t *testing.T, body []byte) string {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	prismDir := filepath.Join(configHome, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if body != nil {
		if err := os.WriteFile(filepath.Join(prismDir, config.RepoSkillsFileName), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return configHome
}

func TestRepoSkillsPath_UsesXDGConfigHome(t *testing.T) {
	configHome := withRepoSkillsFile(t, nil)
	want := filepath.Join(configHome, "prism", "repo-skills.json")
	if got := config.RepoSkillsPath(); got != want {
		t.Errorf("RepoSkillsPath() = %q, want %q", got, want)
	}
}

func TestLoadRepoSkills(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		want    map[string][]string
		wantErr bool
	}{
		{"absent file is an error", nil, nil, true},
		{"malformed JSON is an error", []byte(`{"nixos-config":`), nil, true},
		{"a value that is not a list is an error", []byte(`{"nixos-config":"/nix/store/a-x"}`), nil, true},
		{"a top-level list is an error", []byte(`["/nix/store/a-x"]`), nil, true},
		{"rendered shape", []byte(`{"nixos-config":["/nix/store/a-x","/nix/store/b-y"]}`), map[string][]string{"nixos-config": {"/nix/store/a-x", "/nix/store/b-y"}}, false},
		{"empty map", []byte(`{}`), map[string][]string{}, false},
		{"blank entries dropped and entries trimmed", []byte(`{" nixos-config ":["", "  ", " /nix/store/a-x "], "  ":["/nix/store/b-y"], "home-ops":[]}`), map[string][]string{"nixos-config": {"/nix/store/a-x"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withRepoSkillsFile(t, tc.body)
			got, err := config.LoadRepoSkills()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("LoadRepoSkills() = %q, want %q", got, tc.want)
			}
		})
	}
}
