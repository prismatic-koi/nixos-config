package config_test

// checkin_privilege_test.go — issue #2587.
//
// LoadCheckinPrivilegedRepos backs the tier-3 `/checkin` troubleshooting
// privilege. The security-relevant property is the fail-closed direction:
// every path that cannot produce a list must produce an EMPTY list, never a
// wider one. These tests pin that direction on each path — absent file, empty
// list, absent key, malformed JSON — plus the happy path and the entry
// hygiene that stops a stray "" from matching a session whose repo failed to
// resolve.
//
// Isolation: every test redirects $XDG_CONFIG_HOME at a t.TempDir(), so no
// test reads the developer's real ~/.config/prism/, and none needs a writable
// $HOME (the homeless-shelter sandbox constraint).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// withCheckinPrivilegeFixture points $XDG_CONFIG_HOME at a temp dir. When
// body is non-nil it is written to <cfg>/prism/checkin-privileged-repos.json;
// a nil body leaves the file absent. Returns the config dir.
func withCheckinPrivilegeFixture(t *testing.T, body []byte) string {
	t.Helper()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	prismDir := filepath.Join(configHome, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", prismDir, err)
	}
	if body != nil {
		path := filepath.Join(prismDir, config.CheckinPrivilegedReposFileName)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return configHome
}

// TestCheckinPrivilegedReposPath_UsesXDGConfigHome pins the resolution rule:
// $XDG_CONFIG_HOME/prism/<file>, matching profilesFilePath.
func TestCheckinPrivilegedReposPath_UsesXDGConfigHome(t *testing.T) {
	configHome := withCheckinPrivilegeFixture(t, nil)

	want := filepath.Join(configHome, "prism", config.CheckinPrivilegedReposFileName)
	if got := config.CheckinPrivilegedReposPath(); got != want {
		t.Errorf("CheckinPrivilegedReposPath() = %q, want %q", got, want)
	}
}

// TestLoadCheckinPrivilegedRepos_HappyPath covers the rendered shape the Nix
// module writes.
func TestLoadCheckinPrivilegedRepos_HappyPath(t *testing.T) {
	withCheckinPrivilegeFixture(t, []byte(`{"privileged_repos":["nixos-config","platform-infra"]}`))

	repos, err := config.LoadCheckinPrivilegedRepos()
	if err != nil {
		t.Fatalf("LoadCheckinPrivilegedRepos: %v", err)
	}
	if len(repos) != 2 || repos[0] != "nixos-config" || repos[1] != "platform-infra" {
		t.Errorf("repos = %v, want [nixos-config platform-infra]", repos)
	}
}

// TestLoadCheckinPrivilegedRepos_FailsClosed covers the AC "an empty or
// missing privileged-repo list grants the privilege to nobody". Each case must
// yield an empty list; the malformed case must additionally report an error so
// the caller can log it.
func TestLoadCheckinPrivilegedRepos_FailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte // nil means "no file at all"
		wantErr bool
	}{
		{name: "file absent", body: nil},
		{name: "empty list", body: []byte(`{"privileged_repos":[]}`)},
		{name: "key absent", body: []byte(`{}`)},
		{name: "null list", body: []byte(`{"privileged_repos":null}`)},
		{name: "blank entries only", body: []byte(`{"privileged_repos":["","   "]}`)},
		{name: "malformed json", body: []byte("not json {{"), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withCheckinPrivilegeFixture(t, tc.body)

			repos, err := config.LoadCheckinPrivilegedRepos()
			if tc.wantErr && err == nil {
				t.Errorf("err = nil, want a parse error the caller can log")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("err = %v, want nil", err)
			}
			if len(repos) != 0 {
				t.Errorf("repos = %v, want empty (the privilege must reach nobody)", repos)
			}
		})
	}
}

// TestLoadCheckinPrivilegedRepos_TrimsEntries pins the entry hygiene: a value
// with surrounding whitespace still matches its repo, and a blank entry is
// dropped rather than kept as a "" that could match an unresolved repo.
func TestLoadCheckinPrivilegedRepos_TrimsEntries(t *testing.T) {
	withCheckinPrivilegeFixture(t, []byte(`{"privileged_repos":["  nixos-config  ","","  "]}`))

	repos, err := config.LoadCheckinPrivilegedRepos()
	if err != nil {
		t.Fatalf("LoadCheckinPrivilegedRepos: %v", err)
	}
	if len(repos) != 1 || repos[0] != "nixos-config" {
		t.Errorf("repos = %v, want [nixos-config]", repos)
	}
}

// TestLoadCheckinPrivilegedRepos_ParsesTheRenderedNixOutput is the
// producer/consumer parity check. The literal below is the exact string the
// prism NixOS module renders for the default option value, captured from:
//
//	nix-instantiate --eval --strict --expr '
//	  let f = builtins.getFlake (toString ./.);
//	      cfg = f.darwinConfigurations.m4mac.config;
//	      hm = cfg.home-manager.users.${cfg.nx.username};
//	  in hm.xdg.configFile."prism/checkin-privileged-repos.json".text'
//
// The producer is modules/programs/prism/checkin.nix. If its JSON key is
// renamed on one side only, this test fails instead of the privilege silently
// reaching nobody in production.
func TestLoadCheckinPrivilegedRepos_ParsesTheRenderedNixOutput(t *testing.T) {
	const renderedByNix = `{"privileged_repos":["nixos-config"]}`
	withCheckinPrivilegeFixture(t, []byte(renderedByNix))

	repos, err := config.LoadCheckinPrivilegedRepos()
	if err != nil {
		t.Fatalf("LoadCheckinPrivilegedRepos: %v", err)
	}
	if len(repos) != 1 || repos[0] != "nixos-config" {
		t.Errorf("repos = %v, want [nixos-config] — the Go loader and checkin.nix have diverged", repos)
	}
}

// TestLoadCheckinPrivilegedRepos_ErrorNamesTheFile keeps the failure
// actionable: the message must name the path the operator has to fix.
func TestLoadCheckinPrivilegedRepos_ErrorNamesTheFile(t *testing.T) {
	withCheckinPrivilegeFixture(t, []byte("not json {{"))

	_, err := config.LoadCheckinPrivilegedRepos()
	if err == nil {
		t.Fatal("err = nil, want a parse error")
	}
	if !strings.Contains(err.Error(), config.CheckinPrivilegedReposFileName) {
		t.Errorf("error %q must name %q", err.Error(), config.CheckinPrivilegedReposFileName)
	}
}
