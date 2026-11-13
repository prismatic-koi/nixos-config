package session

// agent_credentials_test.go — the agent credential variables in the agent
// pane env of a host-mode session. Host mode has no filesystem isolation, so
// the variables are the only control. bwrap and sandbox-exec panes carry no
// credential variable: `prism agent-run` sets them in the sandbox.
//
// SECURITY: every name and value is synthetic.

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/agentcred/agentcredtest"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
)

var credentialTestRepos = []string{"home-ops", "nixos-config", "hass-config", "obsidian"}

var credentialTestEnvNames = []string{agentcredtest.PasswordFileEnv, agentcredtest.HostEnv, agentcredtest.UsernameEnv, payload.CredentialFilesEnvName}

func assertNoCredentialVars(t *testing.T, envs map[string]string) {
	t.Helper()
	for k, v := range envs {
		if slices.Contains(credentialTestEnvNames, k) {
			t.Errorf("pane env has %s", k)
		}
		if strings.Contains(v, agentcredtest.Password) {
			t.Errorf("pane env %s holds the password value", k)
		}
	}
}

func TestPaneEnvVars_AgentCredentials(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	l := projecttrusttest.New(t)
	passwordPath, hostPath := agentcredtest.ExampleSecrets(t)
	agentcredtest.WriteFile(t, l.ConfigDir, map[string]config.AgentCredential{
		agentcredtest.Name: agentcredtest.Example(credentialTestRepos, passwordPath, hostPath),
	})
	homeOps := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	hass := projecttrusttest.Worktree(t, l.Code, "hass-config", "main")
	other := projecttrusttest.Worktree(t, l.Code, "other-repo", "main")
	want := map[string]string{
		agentcredtest.PasswordFileEnv:  passwordPath,
		agentcredtest.UsernameEnv:      agentcredtest.Username,
		agentcredtest.HostEnv:          agentcredtest.Host,
		payload.CredentialFilesEnvName: agentcredtest.PasswordFileEnv + "=" + agentcredtest.PasswordMarker,
	}

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
						assertNoCredentialVars(t, envs)
						continue
					}
					for k, v := range want {
						if envs[k] != v {
							t.Errorf("%s layout: %s = %q, want %q", layout, k, envs[k], v)
						}
					}
					for k, v := range envs {
						if strings.Contains(v, agentcredtest.Password) {
							t.Errorf("%s layout: %s holds the password value", layout, k)
						}
					}
				}
			})
		}
	}
}

// A host-mode session whose credential file is missing starts with no
// variable of that credential, and the agent-run log names the credential
// and the cause.
func TestPaneEnvVars_AgentCredentialMissingSecretIsLogged(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	l := projecttrusttest.New(t)
	passwordPath, hostPath := agentcredtest.ExampleSecrets(t)
	if err := os.Remove(passwordPath); err != nil {
		t.Fatal(err)
	}
	agentcredtest.WriteFile(t, l.ConfigDir, map[string]config.AgentCredential{
		agentcredtest.Name: agentcredtest.Example(credentialTestRepos, passwordPath, hostPath),
	})
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	const name = "home-ops@main"

	envs := agentPaneEnvVars(Opts{SessionName: name, Agent: "worker", Worktree: wt, IsolationMode: "host"})
	assertNoCredentialVars(t, envs)

	logPath, err := AgentRunLogPath(name)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read agent-run log: %v", err)
	}
	for _, want := range []string{"[prism] warning: ", "agent credential " + agentcredtest.Name + ":", passwordPath, agentcredtest.PasswordFileEnv} {
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
