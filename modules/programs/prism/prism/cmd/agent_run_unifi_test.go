package cmd

// agent_run_unifi_test.go — `prism agent-run` (bwrap and sandbox-exec) sets
// the UniFi API key delivery of the session on the container config (issue
// #3472). The decision is unit-tested in internal/unifi, and its effect at
// the isolator boundary in internal/container. This file pins the wiring.
//
// SECURITY: the key value is synthetic.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/projecttrust/projecttrusttest"
	"github.com/prismatic-koi/prism/internal/unifi"
	"github.com/prismatic-koi/prism/internal/unifi/unifitest"
)

func TestUniFiDelivery_ForTheRepoOfTheSession(t *testing.T) {
	l := projecttrusttest.New(t)
	keyPath, hostPath := unifitest.Secrets(t, unifitest.Key, unifitest.Host)
	unifitest.WriteFile(t, l.ConfigDir, []string{"home-ops"}, keyPath, hostPath)
	homeOps := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	other := projecttrusttest.Worktree(t, l.Code, "other-repo", "main")
	cfg := config.Config{ProjectLocations: []string{l.Code}}
	logFile, logPath := tempAgentRunLog(t)

	want := unifi.Delivery{APIKeyPath: keyPath, Host: unifitest.Host}
	if got := uniFiDelivery("home-ops@main", "worker", homeOps, cfg, logFile); got != want {
		t.Errorf("home-ops worker: %+v, want %+v", got, want)
	}
	for _, tc := range []struct{ session, role, worktree string }{
		{"home-ops@main~review-1-review-goal", "review-goal", homeOps},
		{"other-repo@main", "worker", other},
	} {
		if got := uniFiDelivery(tc.session, tc.role, tc.worktree, cfg, logFile); got.Enabled() {
			t.Errorf("%s (%s): %+v, want nothing", tc.session, tc.role, got)
		}
	}
	if data, _ := os.ReadFile(logPath); len(data) != 0 {
		t.Errorf("agent-run log = %q, want no warning", data)
	}
}

// A secret that is missing gives no delivery, and the agent-run log states
// the reason. The log holds no key value.
func TestUniFiDelivery_MissingSecretIsLogged(t *testing.T) {
	l := projecttrusttest.New(t)
	keyPath, hostPath := unifitest.Secrets(t, unifitest.Key, unifitest.Host)
	if err := os.Remove(hostPath); err != nil {
		t.Fatal(err)
	}
	unifitest.WriteFile(t, l.ConfigDir, []string{"home-ops"}, keyPath, hostPath)
	wt := projecttrusttest.Worktree(t, l.Code, "home-ops", "main")
	logFile, logPath := tempAgentRunLog(t)

	if got := uniFiDelivery("home-ops@main", "worker", wt, config.Config{ProjectLocations: []string{l.Code}}, logFile); got.Enabled() {
		t.Errorf("delivery = %+v, want nothing", got)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, want := range []string{"[agent-run] warning: ", hostPath, "UNIFI_API_KEY_FILE", "UNIFI_HOST"} {
		if !strings.Contains(log, want) {
			t.Errorf("agent-run log = %q, want it to hold %q", log, want)
		}
	}
	if strings.Contains(log, unifitest.Key) {
		t.Error("the agent-run log holds the key value")
	}
}

// TestAgentRunDispatchPathsSetUniFiDelivery is a source guard. Both
// sandboxed dispatch paths must set Config.UniFi from uniFiDelivery, or a
// session of a listed repo starts with no key. The sandbox-exec file is
// Darwin-only, so it is read as text.
func TestAgentRunDispatchPathsSetUniFiDelivery(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	const want = "UniFi: uniFiDelivery(sessionName, agentRole, worktree, cfg, logFile),"
	for _, name := range []string{"agent_run.go", "agent_run_sandbox_exec_darwin.go"} {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(strings.Join(strings.Fields(string(src)), " "), want); n != 1 {
			t.Errorf("%s sets Config.UniFi with %q %d times, want 1", name, want, n)
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
