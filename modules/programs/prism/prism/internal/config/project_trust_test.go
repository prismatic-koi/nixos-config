package config_test

// project_trust_test.go — LoadProjectTrustedRepos backs the --approve
// decision for pi (issue #3216). Every path that cannot produce a list must
// produce an empty list (every pi gets --no-approve), never a wider one.

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// withProjectTrustFile points $XDG_CONFIG_HOME at a temp dir. A nil body
// leaves project-trust.json absent.
func withProjectTrustFile(t *testing.T, body []byte) string {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	prismDir := filepath.Join(configHome, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if body != nil {
		if err := os.WriteFile(filepath.Join(prismDir, config.ProjectTrustFileName), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return configHome
}

func TestProjectTrustPath_UsesXDGConfigHome(t *testing.T) {
	configHome := withProjectTrustFile(t, nil)
	want := filepath.Join(configHome, "prism", "project-trust.json")
	if got := config.ProjectTrustPath(); got != want {
		t.Errorf("ProjectTrustPath() = %q, want %q", got, want)
	}
}

func TestLoadProjectTrustedRepos(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		want    []string
		wantErr bool
	}{
		{"absent file trusts no repo", nil, nil, false},
		{"rendered shape", []byte(`{"trusted_repos":["home-ops","obsidian"]}`), []string{"home-ops", "obsidian"}, false},
		{"empty list", []byte(`{"trusted_repos":[]}`), nil, false},
		{"absent key", []byte(`{}`), nil, false},
		{"blank entries dropped and entries trimmed", []byte(`{"trusted_repos":["", "  ", " home-ops "]}`), []string{"home-ops"}, false},
		{"malformed JSON is an error", []byte(`{"trusted_repos":`), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProjectTrustFile(t, tc.body)
			got, err := config.LoadProjectTrustedRepos()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("repos = %q, want %q", got, tc.want)
			}
		})
	}
}
