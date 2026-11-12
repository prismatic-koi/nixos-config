package unifi_test

// unifi_test.go — a session gets the UniFi API key only for a repo in
// unifi.json, found by the repo identity of the project-trust decision, and
// only when both secret files hold a usable value (issue #3472).
//
// SECURITY: every value in this file is synthetic.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/unifi"
	"github.com/prismatic-koi/prism/internal/unifi/unifitest"
)

var fourRepos = []string{"home-ops", "nixos-config", "hass-config", "obsidian"}

func collect() (func(string), *[]string) {
	var got []string
	return func(msg string) { got = append(got, msg) }, &got
}

type fixture struct {
	l                 projecttrusttest.Layout
	cfg               config.Config
	keyPath, hostPath string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	l := projecttrusttest.New(t)
	keyPath, hostPath := unifitest.Secrets(t, unifitest.Key, unifitest.Host)
	unifitest.WriteFile(t, l.ConfigDir, fourRepos, keyPath, hostPath)
	return fixture{
		l:        l,
		cfg:      config.Config{ProjectLocations: []string{l.Code}, ProjectSpecific: []string{l.Obsidian}},
		keyPath:  keyPath,
		hostPath: hostPath,
	}
}

func TestForSession_ByRepoIdentityAndRole(t *testing.T) {
	f := newFixture(t)
	homeOps := projecttrusttest.Worktree(t, f.l.Code, "home-ops", "feat")
	nixos := projecttrusttest.Worktree(t, f.l.Code, "nixos-config", "main")
	hass := projecttrusttest.Worktree(t, f.l.Code, "hass-config", "fix")
	other := projecttrusttest.Worktree(t, f.l.Code, "other-repo", "main")
	forged := filepath.Join(f.l.Writable, "home-ops")
	subdir := filepath.Join(homeOps, "kubernetes")
	for _, d := range []string{forged, subdir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	want := unifi.Delivery{APIKeyPath: f.keyPath, Host: unifitest.Host}

	cases := []struct {
		name, session, role, dir string
		want                     unifi.Delivery
	}{
		{"home-ops worker", "home-ops@feat", "worker", homeOps, want},
		{"nixos-config coordinator", "nixos-config@main", "coordinator", nixos, want},
		{"nixos-config worker", "nixos-config@main", "worker", nixos, want},
		{"hass-config worker", "hass-config@fix", "worker", hass, want},
		{"obsidian root session", "obsidian", "", f.l.Obsidian, want},
		{"obsidian worker", "obsidian", "worker", f.l.Obsidian, want},
		{"investigator", "home-ops@feat~investigate-x", "investigate", homeOps, want},
		{"obsidian investigator", "obsidian~investigate-x", "investigate", f.l.Obsidian, want},

		{"review agent", "home-ops@feat~review-1-review-goal", "review-goal", homeOps, unifi.Delivery{}},
		{"fallback reviewer", "home-ops@feat~review-2-review-qa-fallback-1", "review-qa", homeOps, unifi.Delivery{}},
		{"review agent of an investigator", "home-ops@feat~investigate-x~review-1-review-code", "review-code", homeOps, unifi.Delivery{}},
		{"obsidian review agent", "obsidian~review-1-review-security", "review-security", f.l.Obsidian, unifi.Delivery{}},
		{"repo not in the list", "other-repo@main", "worker", other, unifi.Delivery{}},
		{"name of a listed repo in a worktree of another repo", "home-ops@main", "worker", other, unifi.Delivery{}},
		{"name of a listed repo in a dir that is not a project root", "home-ops", "worker", forged, unifi.Delivery{}},
		{"subdirectory of a worktree", "home-ops@feat", "worker", subdir, unifi.Delivery{}},
		{"empty dir", "home-ops@feat", "worker", "", unifi.Delivery{}},
		{"relative dir", "home-ops@feat", "worker", "home-ops/feat", unifi.Delivery{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, warnings := collect()
			got := unifi.ForSession(tc.session, tc.role, tc.dir, f.cfg, warn)
			if got != tc.want {
				t.Errorf("ForSession(%q, %q, %q) = %+v, want %+v", tc.session, tc.role, tc.dir, got, tc.want)
			}
			if len(*warnings) != 0 {
				t.Errorf("warnings = %q, want none", *warnings)
			}
		})
	}
}

// The repo list in unifi.json alone decides which repos get the key.
func TestForSession_RepoListComesFromTheFile(t *testing.T) {
	f := newFixture(t)
	other := projecttrusttest.Worktree(t, f.l.Code, "other-repo", "main")
	homeOps := projecttrusttest.Worktree(t, f.l.Code, "home-ops", "main")
	unifitest.WriteFile(t, f.l.ConfigDir, []string{"other-repo"}, f.keyPath, f.hostPath)

	warn, _ := collect()
	if got := unifi.ForSession("other-repo@main", "worker", other, f.cfg, warn); !got.Enabled() {
		t.Errorf("other-repo, added to the list, got %+v", got)
	}
	if got := unifi.ForSession("home-ops@main", "worker", homeOps, f.cfg, warn); got.Enabled() {
		t.Errorf("home-ops, removed from the list, got %+v", got)
	}
	unifitest.WriteFile(t, f.l.ConfigDir, nil, f.keyPath, f.hostPath)
	if got := unifi.ForSession("other-repo@main", "worker", other, f.cfg, warn); got.Enabled() {
		t.Errorf("an empty list gave %+v", got)
	}
}

func TestForSession_NoFileGivesNothingAndNoWarning(t *testing.T) {
	l := projecttrusttest.New(t)
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	warn, warnings := collect()
	got := unifi.ForSession("home-ops@main", "worker", wt, config.Config{ProjectLocations: []string{l.Code}}, warn)
	if got.Enabled() || len(*warnings) != 0 {
		t.Errorf("ForSession = %+v, warnings %q; want nothing", got, *warnings)
	}
}

func TestForSession_UnusableFileWarns(t *testing.T) {
	for name, body := range map[string]string{
		"malformed JSON":    `{"repos": [`,
		"wrong shape":       `{"repos": "home-ops"}`,
		"relative key path": `{"repos": ["home-ops"], "api_key_path": "secrets/key", "host_path": "/x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			l := projecttrusttest.New(t)
			wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
			unifitest.WriteRaw(t, l.ConfigDir, []byte(body))
			warn, warnings := collect()
			got := unifi.ForSession("home-ops@main", "worker", wt, config.Config{ProjectLocations: []string{l.Code}}, warn)
			if got.Enabled() {
				t.Errorf("ForSession = %+v, want nothing", got)
			}
			if len(*warnings) != 1 || !strings.Contains((*warnings)[0], config.UniFiFileName) {
				t.Errorf("warnings = %q, want one that names %s", *warnings, config.UniFiFileName)
			}
		})
	}
}

// A secret that is missing or does not decrypt gives no variable and one
// warning with the reason. No warning holds a secret value.
func TestForSession_SecretProblemsWarn(t *testing.T) {
	const shortKey = "abc1234"
	cases := []struct {
		name, key, host string
		removeKey       bool
		removeHost      bool
		wantInWarning   string
	}{
		{name: "key file missing", key: unifitest.Key, host: unifitest.Host, removeKey: true, wantInWarning: "unifi_api_key"},
		{name: "host file missing", key: unifitest.Key, host: unifitest.Host, removeHost: true, wantInWarning: "unifi_host"},
		{name: "key file empty", key: "", host: unifitest.Host, wantInWarning: "fewer than 8 characters"},
		{name: "key file short", key: shortKey, host: unifitest.Host, wantInWarning: "fewer than 8 characters"},
		{name: "host file empty", key: unifitest.Key, host: "", wantInWarning: "unifi_host"},
		{name: "host with a space", key: unifitest.Key, host: "10.0.0.1 evil", wantInWarning: "space or a control character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := projecttrusttest.New(t)
			wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
			keyPath, hostPath := unifitest.Secrets(t, tc.key, tc.host)
			if tc.removeKey {
				os.Remove(keyPath)
			}
			if tc.removeHost {
				os.Remove(hostPath)
			}
			unifitest.WriteFile(t, l.ConfigDir, fourRepos, keyPath, hostPath)
			warn, warnings := collect()
			got := unifi.ForSession("home-ops@main", "worker", wt, config.Config{ProjectLocations: []string{l.Code}}, warn)
			if got.Enabled() || got.EnvVars() != nil {
				t.Errorf("ForSession = %+v, want nothing", got)
			}
			if len(*warnings) != 1 {
				t.Fatalf("warnings = %q, want one", *warnings)
			}
			w := (*warnings)[0]
			if !strings.Contains(w, tc.wantInWarning) || !strings.Contains(w, unifi.APIKeyFileEnvName) {
				t.Errorf("warning %q does not give the reason %q and the variable", w, tc.wantInWarning)
			}
			for _, secret := range []string{unifitest.Key, shortKey} {
				if strings.Contains(w, secret) {
					t.Errorf("warning holds a secret value: %q", w)
				}
			}
		})
	}
}

func TestDelivery_EnvVars(t *testing.T) {
	if got := (unifi.Delivery{}).EnvVars(); got != nil {
		t.Errorf("zero Delivery EnvVars = %v, want nil", got)
	}
	got := unifi.Delivery{APIKeyPath: "/run/secrets/unifi_api_key", Host: unifitest.Host}.EnvVars()
	want := map[string]string{"UNIFI_API_KEY_FILE": "/run/secrets/unifi_api_key", "UNIFI_HOST": unifitest.Host}
	if len(got) != len(want) || got["UNIFI_API_KEY_FILE"] != want["UNIFI_API_KEY_FILE"] || got["UNIFI_HOST"] != want["UNIFI_HOST"] {
		t.Errorf("EnvVars = %v, want %v", got, want)
	}
	for k, v := range got {
		if strings.Contains(v, unifitest.Key) {
			t.Errorf("%s holds the key value", k)
		}
	}
}

// Each role of the canonical review set gets no key. If a review role is
// added to internal/review and not here, this test fails.
func TestForSession_EveryReviewRoleGetsNothing(t *testing.T) {
	f := newFixture(t)
	wt := projecttrusttest.Worktree(t, f.l.Code, "home-ops", "main")
	warn, _ := collect()
	for _, a := range review.Agents() {
		if got := unifi.ForSession("home-ops@main~review-1-"+a.Name, a.Name, wt, f.cfg, warn); got.Enabled() {
			t.Errorf("review role %s got %+v", a.Name, got)
		}
	}
}
