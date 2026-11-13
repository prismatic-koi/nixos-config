package cmd

// agent_run_agent_credentials_test.go — `prism agent-run` (bwrap and
// sandbox-exec) sets the agent credential delivery of the session on the
// container config. The decision is unit-tested in internal/agentcred, and
// its effect at the isolator boundary in internal/container. This file pins
// the wiring.
//
// SECURITY: every name and value is synthetic.

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/agentcred"
	"github.com/prismatic-koi/prism/internal/agentcred/agentcredtest"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
)

func TestAgentCredentialDelivery_ForTheRepoOfTheSession(t *testing.T) {
	l := projecttrusttest.New(t)
	passwordPath, hostPath := agentcredtest.ExampleSecrets(t)
	agentcredtest.WriteFile(t, l.ConfigDir, map[string]config.AgentCredential{
		agentcredtest.Name: agentcredtest.Example([]string{"home-ops"}, passwordPath, hostPath),
	})
	homeOps := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	other := projecttrusttest.Worktree(t, l.Code, "other-repo", "main")
	cfg := config.Config{ProjectLocations: []string{l.Code}}
	logFile, logPath := tempAgentRunLog(t)

	want := agentcred.Delivery{Credentials: []agentcred.Credential{{
		Name:  agentcredtest.Name,
		Files: []agentcred.File{{Env: agentcredtest.PasswordFileEnv, Path: passwordPath, Marker: agentcredtest.PasswordMarker}},
		Vars:  map[string]string{agentcredtest.HostEnv: agentcredtest.Host, agentcredtest.UsernameEnv: agentcredtest.Username},
	}}}
	if got := agentCredentialDelivery("home-ops@main", "worker", homeOps, cfg, logFile); !reflect.DeepEqual(got, want) {
		t.Errorf("home-ops worker: %+v, want %+v", got, want)
	}
	for _, tc := range []struct{ session, role, worktree string }{
		{"home-ops@main~review-1-review-goal", "review-goal", homeOps},
		{"other-repo@main", "worker", other},
	} {
		if got := agentCredentialDelivery(tc.session, tc.role, tc.worktree, cfg, logFile); got.Enabled() {
			t.Errorf("%s (%s): %+v, want nothing", tc.session, tc.role, got)
		}
	}
	if data, _ := os.ReadFile(logPath); len(data) != 0 {
		t.Errorf("agent-run log = %q, want no warning", data)
	}
}

// A secret that is missing gives no delivery of that credential, and the
// agent-run log names the credential and the cause. The log holds no secret
// value.
func TestAgentCredentialDelivery_MissingSecretIsLogged(t *testing.T) {
	l := projecttrusttest.New(t)
	passwordPath, hostPath := agentcredtest.ExampleSecrets(t)
	if err := os.Remove(hostPath); err != nil {
		t.Fatal(err)
	}
	agentcredtest.WriteFile(t, l.ConfigDir, map[string]config.AgentCredential{
		agentcredtest.Name: agentcredtest.Example([]string{"home-ops"}, passwordPath, hostPath),
	})
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	logFile, logPath := tempAgentRunLog(t)

	if got := agentCredentialDelivery("home-ops@main", "worker", wt, config.Config{ProjectLocations: []string{l.Code}}, logFile); got.Enabled() {
		t.Errorf("delivery = %+v, want nothing", got)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, want := range []string{"[agent-run] warning: ", "agent credential " + agentcredtest.Name + ":", hostPath, agentcredtest.PasswordFileEnv, agentcredtest.UsernameEnv, agentcredtest.HostEnv} {
		if !strings.Contains(log, want) {
			t.Errorf("agent-run log = %q, want it to hold %q", log, want)
		}
	}
	if strings.Contains(log, agentcredtest.Password) {
		t.Error("the agent-run log holds the password value")
	}
}

// TestAgentRunDispatchPathsSetAgentCredentials is a source guard. Both
// sandboxed dispatch paths must set Config.AgentCredentials from
// agentCredentialDelivery, or a session of a listed repo starts with no
// credential. The sandbox-exec file is Darwin-only, so it is read as text.
func TestAgentRunDispatchPathsSetAgentCredentials(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	const want = "AgentCredentials: agentCredentialDelivery(sessionName, agentRole, worktree, cfg, logFile),"
	for _, name := range []string{"agent_run.go", "agent_run_sandbox_exec_darwin.go"} {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(strings.Join(strings.Fields(string(src)), " "), want); n != 1 {
			t.Errorf("%s sets Config.AgentCredentials with %q %d times, want 1", name, want, n)
		}
	}
}

func tempAgentRunLog(t *testing.T) (*os.File, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent-run.log")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, p
}
