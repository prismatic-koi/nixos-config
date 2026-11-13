package agentcred_test

// agentcred_test.go — a session gets an agent credential only for a repo in
// the repo list of that credential, found by the repo identity of the
// project-trust decision, and only when each secret file of the credential
// holds a usable value. A problem with one credential does not stop the
// others.
//
// SECURITY: every value in this file is synthetic.

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/agentcred"
	"github.com/prismatic-koi/prism/internal/agentcred/agentcredtest"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
	"github.com/prismatic-koi/prism/internal/review"
)

var fourRepos = []string{"home-ops", "nixos-config", "hass-config", "obsidian"}

func collect() (func(string), *[]string) {
	var got []string
	return func(msg string) { got = append(got, msg) }, &got
}

type fixture struct {
	l                      projecttrusttest.Layout
	cfg                    config.Config
	passwordPath, hostPath string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	l := projecttrusttest.New(t)
	passwordPath, hostPath := agentcredtest.ExampleSecrets(t)
	agentcredtest.WriteFile(t, l.ConfigDir, map[string]config.AgentCredential{
		agentcredtest.Name: agentcredtest.Example(fourRepos, passwordPath, hostPath),
	})
	return fixture{
		l:            l,
		cfg:          config.Config{ProjectLocations: []string{l.Code}, ProjectSpecific: []string{l.Obsidian}},
		passwordPath: passwordPath,
		hostPath:     hostPath,
	}
}

func (f fixture) exampleDelivery() agentcred.Delivery {
	return agentcred.Delivery{Credentials: []agentcred.Credential{{
		Name:  agentcredtest.Name,
		Files: []agentcred.File{{Env: agentcredtest.PasswordFileEnv, Path: f.passwordPath, Marker: agentcredtest.PasswordMarker}},
		Vars:  map[string]string{agentcredtest.HostEnv: agentcredtest.Host, agentcredtest.UsernameEnv: agentcredtest.Username},
	}}}
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
	want := f.exampleDelivery()
	none := agentcred.Delivery{}

	cases := []struct {
		name, session, role, dir string
		want                     agentcred.Delivery
	}{
		{"home-ops worker", "home-ops@feat", "worker", homeOps, want},
		{"nixos-config coordinator", "nixos-config@main", "coordinator", nixos, want},
		{"nixos-config worker", "nixos-config@main", "worker", nixos, want},
		{"hass-config worker", "hass-config@fix", "worker", hass, want},
		{"obsidian root session", "obsidian", "", f.l.Obsidian, want},
		{"obsidian worker", "obsidian", "worker", f.l.Obsidian, want},
		{"investigator", "home-ops@feat~investigate-x", "investigate", homeOps, want},
		{"obsidian investigator", "obsidian~investigate-x", "investigate", f.l.Obsidian, want},

		{"review agent", "home-ops@feat~review-1-review-goal", "review-goal", homeOps, none},
		{"fallback reviewer", "home-ops@feat~review-2-review-qa-fallback-1", "review-qa", homeOps, none},
		{"review agent of an investigator", "home-ops@feat~investigate-x~review-1-review-code", "review-code", homeOps, none},
		{"obsidian review agent", "obsidian~review-1-review-security", "review-security", f.l.Obsidian, none},
		{"repo not in the list", "other-repo@main", "worker", other, none},
		{"name of a listed repo in a worktree of another repo", "home-ops@main", "worker", other, none},
		{"name of a listed repo in a dir that is not a project root", "home-ops", "worker", forged, none},
		{"subdirectory of a worktree", "home-ops@feat", "worker", subdir, none},
		{"empty dir", "home-ops@feat", "worker", "", none},
		{"relative dir", "home-ops@feat", "worker", "home-ops/feat", none},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, warnings := collect()
			got := agentcred.ForSession(tc.session, tc.role, tc.dir, f.cfg, warn)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ForSession(%q, %q, %q) = %+v, want %+v", tc.session, tc.role, tc.dir, got, tc.want)
			}
			if len(*warnings) != 0 {
				t.Errorf("warnings = %q, want none", *warnings)
			}
		})
	}
}

// The repo list of each credential in agent-credentials.json alone decides
// which repos get it.
func TestForSession_RepoListComesFromTheFile(t *testing.T) {
	f := newFixture(t)
	other := projecttrusttest.Worktree(t, f.l.Code, "other-repo", "main")
	homeOps := projecttrusttest.Worktree(t, f.l.Code, "home-ops", "main")
	write := func(repos []string) {
		agentcredtest.WriteFile(t, f.l.ConfigDir, map[string]config.AgentCredential{
			agentcredtest.Name: agentcredtest.Example(repos, f.passwordPath, f.hostPath),
		})
	}
	write([]string{"other-repo"})

	warn, _ := collect()
	if got := agentcred.ForSession("other-repo@main", "worker", other, f.cfg, warn); !got.Enabled() {
		t.Errorf("other-repo, added to the list, got %+v", got)
	}
	if got := agentcred.ForSession("home-ops@main", "worker", homeOps, f.cfg, warn); got.Enabled() {
		t.Errorf("home-ops, removed from the list, got %+v", got)
	}
	write(nil)
	if got := agentcred.ForSession("other-repo@main", "worker", other, f.cfg, warn); got.Enabled() {
		t.Errorf("an empty list gave %+v", got)
	}
}

// Each credential has its own repo list. A session gets the credentials of
// its repo only, in the order of their names.
func TestForSession_EachCredentialHasItsOwnRepos(t *testing.T) {
	f := newFixture(t)
	homeOps := projecttrusttest.Worktree(t, f.l.Code, "home-ops", "main")
	hass := projecttrusttest.Worktree(t, f.l.Code, "hass-config", "main")
	tokenPath := agentcredtest.Secret(t, "second_token", "synthetic-second-token-0123456789")
	second := config.AgentCredential{
		Repos: []string{"hass-config"},
		Files: map[string]config.AgentCredentialFile{"SECOND_TOKEN_FILE": {Path: tokenPath, Marker: "SECOND_TOKEN"}},
	}
	agentcredtest.WriteFile(t, f.l.ConfigDir, map[string]config.AgentCredential{
		agentcredtest.Name: agentcredtest.Example([]string{"home-ops", "hass-config"}, f.passwordPath, f.hostPath),
		"second":           second,
	})

	warn, warnings := collect()
	if got := agentcred.ForSession("home-ops@main", "worker", homeOps, f.cfg, warn); !reflect.DeepEqual(got, f.exampleDelivery()) {
		t.Errorf("home-ops got %+v, want the example credential only", got)
	}
	got := agentcred.ForSession("hass-config@main", "worker", hass, f.cfg, warn)
	var names []string
	for _, c := range got.Credentials {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{agentcredtest.Name, "second"}) {
		t.Errorf("hass-config got the credentials %q, want example and second", names)
	}
	if len(*warnings) != 0 {
		t.Errorf("warnings = %q, want none", *warnings)
	}
}

func TestForSession_NoFileGivesNothingAndNoWarning(t *testing.T) {
	l := projecttrusttest.New(t)
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	warn, warnings := collect()
	got := agentcred.ForSession("home-ops@main", "worker", wt, config.Config{ProjectLocations: []string{l.Code}}, warn)
	if got.Enabled() || len(*warnings) != 0 {
		t.Errorf("ForSession = %+v, warnings %q; want nothing", got, *warnings)
	}
}

func TestForSession_UnusableFileWarns(t *testing.T) {
	for name, body := range map[string]string{
		"malformed JSON": `{"example": {`,
		"wrong shape":    `{"example": {"repos": "home-ops"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			l := projecttrusttest.New(t)
			wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
			agentcredtest.WriteRaw(t, l.ConfigDir, []byte(body))
			warn, warnings := collect()
			got := agentcred.ForSession("home-ops@main", "worker", wt, config.Config{ProjectLocations: []string{l.Code}}, warn)
			if got.Enabled() {
				t.Errorf("ForSession = %+v, want nothing", got)
			}
			if len(*warnings) != 1 || !strings.Contains((*warnings)[0], config.AgentCredentialsFileName) {
				t.Errorf("warnings = %q, want one that names %s", *warnings, config.AgentCredentialsFileName)
			}
		})
	}
}

// A secret file that is missing or holds no usable value, or a declaration
// that is not valid, gives no variable of the credential, and one warning that
// names the credential, the cause, and the variables. No warning holds a
// secret value.
func TestForSession_CredentialProblemsWarn(t *testing.T) {
	const shortPassword = "abc1234"
	cases := []struct {
		name          string
		edit          func(t *testing.T, c *config.AgentCredential)
		wantInWarning string
	}{
		{"password file missing", func(t *testing.T, c *config.AgentCredential) {
			removeFile(t, c.Files[agentcredtest.PasswordFileEnv].Path)
		}, "no such file"},
		{"host file missing", func(t *testing.T, c *config.AgentCredential) {
			removeFile(t, c.Values[agentcredtest.HostEnv].Path)
		}, "no such file"},
		{"password file empty", func(t *testing.T, c *config.AgentCredential) {
			setFile(c, agentcredtest.Secret(t, "p", ""))
		}, "fewer than 8 characters"},
		{"password file short", func(t *testing.T, c *config.AgentCredential) {
			setFile(c, agentcredtest.Secret(t, "p", shortPassword))
		}, "fewer than 8 characters"},
		{"host file empty", func(t *testing.T, c *config.AgentCredential) {
			c.Values[agentcredtest.HostEnv] = config.AgentCredentialValue{Path: agentcredtest.Secret(t, "h", "")}
		}, "empty or holds a space"},
		{"host with a space", func(t *testing.T, c *config.AgentCredential) {
			c.Values[agentcredtest.HostEnv] = config.AgentCredentialValue{Path: agentcredtest.Secret(t, "h", "host.example.invalid evil")}
		}, "empty or holds a space"},
		{"relative password path", func(t *testing.T, c *config.AgentCredential) {
			setFile(c, "secrets/password")
		}, "not absolute"},
		{"relative host path", func(t *testing.T, c *config.AgentCredential) {
			c.Values[agentcredtest.HostEnv] = config.AgentCredentialValue{Path: "secrets/host"}
		}, "not absolute"},
		{"empty username", func(t *testing.T, c *config.AgentCredential) {
			c.Literals[agentcredtest.UsernameEnv] = ""
		}, "literal value is empty"},
		{"username with a space", func(t *testing.T, c *config.AgentCredential) {
			c.Literals[agentcredtest.UsernameEnv] = "agent readonly"
		}, "literal value is empty"},
		{"username with a newline", func(t *testing.T, c *config.AgentCredential) {
			c.Literals[agentcredtest.UsernameEnv] = "agent\nreadonly"
		}, "literal value is empty"},
		{"marker that is not a name", func(t *testing.T, c *config.AgentCredential) {
			f := c.Files[agentcredtest.PasswordFileEnv]
			f.Marker = "example password"
			c.Files[agentcredtest.PasswordFileEnv] = f
		}, "the marker"},
		{"variable name that is not a name", func(t *testing.T, c *config.AgentCredential) {
			c.Literals["example-user"] = "u"
		}, "does not match"},
		{"variable name of prism", func(t *testing.T, c *config.AgentCredential) {
			c.Literals[payload.CredentialFilesEnvName] = "X=Y"
		}, "starts with PRISM_"},
		{"one name for two kinds", func(t *testing.T, c *config.AgentCredential) {
			c.Literals[agentcredtest.HostEnv] = "x"
		}, "declared more than once"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := projecttrusttest.New(t)
			wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
			passwordPath, hostPath := agentcredtest.ExampleSecrets(t)
			c := agentcredtest.Example(fourRepos, passwordPath, hostPath)
			tc.edit(t, &c)
			agentcredtest.WriteFile(t, l.ConfigDir, map[string]config.AgentCredential{agentcredtest.Name: c})
			warn, warnings := collect()
			got := agentcred.ForSession("home-ops@main", "worker", wt, config.Config{ProjectLocations: []string{l.Code}}, warn)
			if got.Enabled() || got.EnvVars() != nil {
				t.Errorf("ForSession = %+v, want nothing", got)
			}
			if len(*warnings) != 1 {
				t.Fatalf("warnings = %q, want one", *warnings)
			}
			w := (*warnings)[0]
			for _, want := range []string{"agent credential " + agentcredtest.Name + ":", tc.wantInWarning, agentcredtest.PasswordFileEnv, agentcredtest.HostEnv, agentcredtest.UsernameEnv} {
				if !strings.Contains(w, want) {
					t.Errorf("warning %q does not hold %q", w, want)
				}
			}
			for _, secret := range []string{agentcredtest.Password, shortPassword, agentcredtest.Host} {
				if strings.Contains(w, secret) {
					t.Errorf("warning holds a secret value: %q", w)
				}
			}
		})
	}
}

// A missing or short secret file of one credential removes that credential
// only. The session gets the variables of the other credentials.
func TestForSession_OneBadCredentialLeavesTheOthers(t *testing.T) {
	for name, content := range map[string]*string{
		"missing": nil,
		"short":   ptr("abc1234"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			wt := projecttrusttest.Worktree(t, f.l.Code, "home-ops", "main")
			badPath := filepath.Join(t.TempDir(), "broken_token")
			if content != nil {
				badPath = agentcredtest.Secret(t, "broken_token", *content)
			}
			agentcredtest.WriteFile(t, f.l.ConfigDir, map[string]config.AgentCredential{
				agentcredtest.Name: agentcredtest.Example(fourRepos, f.passwordPath, f.hostPath),
				"broken": {
					Repos:    []string{"home-ops"},
					Files:    map[string]config.AgentCredentialFile{"BROKEN_TOKEN_FILE": {Path: badPath, Marker: "BROKEN_TOKEN"}},
					Literals: map[string]string{"BROKEN_USER": "u"},
				},
			})
			warn, warnings := collect()
			got := agentcred.ForSession("home-ops@main", "worker", wt, f.cfg, warn)
			if !reflect.DeepEqual(got, f.exampleDelivery()) {
				t.Errorf("ForSession = %+v, want the example credential only", got)
			}
			if len(*warnings) != 1 {
				t.Fatalf("warnings = %q, want one", *warnings)
			}
			for _, want := range []string{"agent credential broken:", badPath, "BROKEN_TOKEN_FILE", "BROKEN_USER"} {
				if !strings.Contains((*warnings)[0], want) {
					t.Errorf("warning %q does not hold %q", (*warnings)[0], want)
				}
			}
		})
	}
}

// The nix option refuses two credentials that declare one variable. A
// hand-edited file that does it gives neither credential, and a warning that
// names both.
func TestForSession_TwoCredentialsWithOneVariableGiveNeither(t *testing.T) {
	f := newFixture(t)
	wt := projecttrusttest.Worktree(t, f.l.Code, "home-ops", "main")
	agentcredtest.WriteFile(t, f.l.ConfigDir, map[string]config.AgentCredential{
		"alpha":   {Repos: []string{"home-ops"}, Literals: map[string]string{"SHARED_NAME": "a"}},
		"bravo":   {Repos: []string{"home-ops"}, Literals: map[string]string{"SHARED_NAME": "b"}},
		"charlie": {Repos: []string{"home-ops"}, Literals: map[string]string{"CHARLIE_USER": "c"}},
		"delta":   {Repos: []string{"other-repo"}, Literals: map[string]string{"SHARED_NAME": "d"}},
	})
	warn, warnings := collect()
	got := agentcred.ForSession("home-ops@main", "worker", wt, f.cfg, warn)
	if want := map[string]string{"CHARLIE_USER": "c"}; !maps.Equal(got.EnvVars(), want) {
		t.Errorf("EnvVars = %v, want %v", got.EnvVars(), want)
	}
	if len(*warnings) != 2 {
		t.Fatalf("warnings = %q, want one for alpha and one for bravo", *warnings)
	}
	for i, pair := range [][2]string{{"alpha", "bravo"}, {"bravo", "alpha"}} {
		w := (*warnings)[i]
		if !strings.Contains(w, "agent credential "+pair[0]+":") || !strings.Contains(w, "credential "+pair[1]+" also") || !strings.Contains(w, "SHARED_NAME") {
			t.Errorf("warning %q does not name %s, %s, and the variable", w, pair[0], pair[1])
		}
	}
}

func TestDelivery_EnvVars(t *testing.T) {
	if got := (agentcred.Delivery{}).EnvVars(); got != nil {
		t.Errorf("zero Delivery EnvVars = %v, want nil", got)
	}
	d := agentcred.Delivery{Credentials: []agentcred.Credential{
		{
			Name:  "beta",
			Files: []agentcred.File{{Env: "BETA_TOKEN_FILE", Path: "/run/secrets/beta_token", Marker: "BETA_TOKEN"}},
		},
		{
			Name:  agentcredtest.Name,
			Files: []agentcred.File{{Env: agentcredtest.PasswordFileEnv, Path: "/run/secrets/example_password", Marker: agentcredtest.PasswordMarker}},
			Vars:  map[string]string{agentcredtest.HostEnv: agentcredtest.Host, agentcredtest.UsernameEnv: agentcredtest.Username},
		},
	}}
	want := map[string]string{
		"BETA_TOKEN_FILE":             "/run/secrets/beta_token",
		agentcredtest.PasswordFileEnv: "/run/secrets/example_password",
		agentcredtest.HostEnv:         agentcredtest.Host,
		agentcredtest.UsernameEnv:     agentcredtest.Username,
		payload.CredentialFilesEnvName: "BETA_TOKEN_FILE=BETA_TOKEN," +
			agentcredtest.PasswordFileEnv + "=" + agentcredtest.PasswordMarker,
	}
	if got := d.EnvVars(); !maps.Equal(got, want) {
		t.Errorf("EnvVars = %v, want %v", got, want)
	}
	if got := payload.ParseCredentialFiles(want[payload.CredentialFilesEnvName]); len(got) != 2 {
		t.Errorf("the registry parses to %v, want two entries", got)
	}

	// A credential with no file variable gives no registry.
	only := agentcred.Delivery{Credentials: []agentcred.Credential{{Name: "c", Vars: map[string]string{"C_USER": "u"}}}}
	if got := only.EnvVars(); !maps.Equal(got, map[string]string{"C_USER": "u"}) {
		t.Errorf("EnvVars with no file variable = %v", got)
	}
}

// Each role of the canonical review set gets no credential. If a review role is
// added to internal/review and not here, this test fails.
func TestForSession_EveryReviewRoleGetsNothing(t *testing.T) {
	f := newFixture(t)
	wt := projecttrusttest.Worktree(t, f.l.Code, "home-ops", "main")
	warn, _ := collect()
	for _, a := range review.Agents() {
		if got := agentcred.ForSession("home-ops@main~review-1-"+a.Name, a.Name, wt, f.cfg, warn); got.Enabled() {
			t.Errorf("review role %s got %+v", a.Name, got)
		}
	}
}

func removeFile(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}

func setFile(c *config.AgentCredential, path string) {
	f := c.Files[agentcredtest.PasswordFileEnv]
	f.Path = path
	c.Files[agentcredtest.PasswordFileEnv] = f
}

func ptr(s string) *string { return &s }
