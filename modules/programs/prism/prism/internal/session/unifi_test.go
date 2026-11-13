package session

// unifi_test.go — the UniFi variables in the agent pane env of a host-mode
// session (issue #3472). Host mode has no filesystem isolation, so the
// variables are the only control. bwrap and sandbox-exec panes carry no
// UniFi variable: `prism agent-run` sets them in the sandbox.
//
// SECURITY: the password value is synthetic.

import (
	"os"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
	"github.com/prismatic-koi/prism/internal/unifi/unifitest"
)

var uniFiTestRepos = []string{"home-ops", "nixos-config", "hass-config", "obsidian"}

func assertNoUniFiVars(t *testing.T, envs map[string]string) {
	t.Helper()
	for k, v := range envs {
		if strings.HasPrefix(k, "UNIFI_") {
			t.Errorf("pane env has %s", k)
		}
		if strings.Contains(v, unifitest.Password) {
			t.Errorf("pane env %s holds the password value", k)
		}
	}
}

func TestPaneEnvVars_UniFi(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	l := projecttrusttest.New(t)
	passwordPath, hostPath := unifitest.Secrets(t, unifitest.Password, unifitest.Host)
	unifitest.WriteFile(t, l.ConfigDir, uniFiTestRepos, passwordPath, hostPath)
	homeOps := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	hass := projecttrusttest.Worktree(t, l.Code, "hass-config", "main")
	other := projecttrusttest.Worktree(t, l.Code, "other-repo", "main")

	cases := []struct {
		name, session, agent, worktree string
		want                           bool
	}{
		{"home-ops worker", "home-ops@main", "worker", homeOps, true},
		{"hass-config coordinator", "hass-config@main", "coordinator", hass, true},
		{"obsidian root session", "obsidian", "", l.Obsidian, true},
		{"investigator", "home-ops@main~investigate-x", "investigate", homeOps, true},
		{"review agent", "home-ops@main~review-1-review-goal", "review-goal", homeOps, false},
		{"other repo", "other-repo@main", "worker", other, false},
		{"name of a listed repo in another repo", "home-ops@main", "worker", other, false},
	}
	for _, tc := range cases {
		for _, mode := range []string{"host", "bwrap", "sandbox-exec"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				full := agentPaneEnvVars(Opts{
					SessionName:   tc.session,
					Agent:         tc.agent,
					Worktree:      tc.worktree,
					IsolationMode: mode,
					Prompt:        "go",
				})
				agentOnly := agentOnlyPaneEnvVars(SpawnOpts{
					SessionName: tc.session,
					AgentRole:   tc.agent,
					Worktree:    tc.worktree,
					Prompt:      "go",
				}, mode)
				for layout, envs := range map[string]map[string]string{"full": full, "agent-only": agentOnly} {
					if !tc.want || mode != "host" {
						assertNoUniFiVars(t, envs)
						continue
					}
					if envs["UNIFI_PASSWORD_FILE"] != passwordPath || envs["UNIFI_USERNAME"] != unifitest.Username || envs["UNIFI_HOST"] != unifitest.Host {
						t.Errorf("%s layout: UNIFI_PASSWORD_FILE=%q UNIFI_USERNAME=%q UNIFI_HOST=%q, want %q, %q, and %q",
							layout, envs["UNIFI_PASSWORD_FILE"], envs["UNIFI_USERNAME"], envs["UNIFI_HOST"], passwordPath, unifitest.Username, unifitest.Host)
					}
					for k, v := range envs {
						if strings.Contains(v, unifitest.Password) {
							t.Errorf("%s layout: %s holds the password value", layout, k)
						}
					}
				}
			})
		}
	}
}

// A host-mode session whose password file is missing starts with no UniFi
// variable, and the agent-run log gives the reason.
func TestPaneEnvVars_UniFiMissingSecretIsLogged(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	l := projecttrusttest.New(t)
	passwordPath, hostPath := unifitest.Secrets(t, unifitest.Password, unifitest.Host)
	if err := os.Remove(passwordPath); err != nil {
		t.Fatal(err)
	}
	unifitest.WriteFile(t, l.ConfigDir, uniFiTestRepos, passwordPath, hostPath)
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	const name = "home-ops@main"

	envs := agentPaneEnvVars(Opts{SessionName: name, Agent: "worker", Worktree: wt, IsolationMode: "host"})
	assertNoUniFiVars(t, envs)

	logPath, err := AgentRunLogPath(name)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read agent-run log: %v", err)
	}
	for _, want := range []string{"[prism] warning: ", passwordPath, "UNIFI_PASSWORD_FILE"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("agent-run log = %q, want it to hold %q", log, want)
		}
	}
}

func TestMergeEnvVars(t *testing.T) {
	if got := mergeEnvVars(nil, map[string]string{}); got != nil {
		t.Errorf("mergeEnvVars of empty maps = %v, want nil", got)
	}
	got := mergeEnvVars(map[string]string{"A": "1", "B": "1"}, nil, map[string]string{"B": "2"})
	if len(got) != 2 || got["A"] != "1" || got["B"] != "2" {
		t.Errorf("mergeEnvVars = %v", got)
	}
}
