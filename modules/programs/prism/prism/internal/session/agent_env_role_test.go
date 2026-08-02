package session

// agent_env_role_test.go — coverage for the role-filtered AgentEnvVars map in
// the agent-only layout (issue #2533).
//
// Before #2533 the agent-only layout omitted AgentEnvVars entirely, so a
// host-mode review session got no profile env vars while the same session
// under bwrap or sandbox-exec got the full set, including the two keys that
// register the 65-tool grafana MCP surface. Both paths now resolve the map
// through config.AgentEnvVarsForRole, so a given role gets the same map in
// either mode.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// roleEnvFixtureVars mirrors the production agent.envVars attrset: shared
// paths plus the grafana pair the pi extension self-gates on.
func roleEnvFixtureVars() map[string]string {
	return map[string]string{
		"GIT_EDITOR":              "true",
		"KUBECONFIG":              "/home/ben/.config/kube/agents-config",
		"NOTION_MCP_REPOS":        "nixos-config:prism",
		"GRAFANA_MCP_CONFIG_PATH": "/run/secrets/grafana_config_home",
		"PI_GRAFANA_MCP_BIN":      "/nix/store/abc-mcp-grafana/bin/mcp-grafana",
	}
}

// seedProfilesFixture writes a profiles.json carrying roleEnvFixtureVars as
// agent_env_vars and points $XDG_CONFIG_HOME at its parent.
func seedProfilesFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prismDir := filepath.Join(dir, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %q: %v", prismDir, err)
	}
	body, err := json.Marshal(config.ProfilesFile{
		Default:      "anthropic",
		Profiles:     map[string]config.ProfileEntry{"anthropic": {}},
		AgentEnvVars: roleEnvFixtureVars(),
	})
	if err != nil {
		t.Fatalf("Marshal profiles fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prismDir, "profiles.json"), body, 0o644); err != nil {
		t.Fatalf("WriteFile profiles.json: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
}

// TestAgentOnlyAgentEnvVars_ReviewRoleLosesGrafana verifies the agent-only
// layout resolves a review role's map without either grafana gate key, and
// keeps every other key.
func TestAgentOnlyAgentEnvVars_ReviewRoleLosesGrafana(t *testing.T) {
	seedProfilesFixture(t)

	got := agentOnlyAgentEnvVars(SpawnOpts{AgentRole: "review-qa"})
	for _, k := range []string{"GRAFANA_MCP_CONFIG_PATH", "PI_GRAFANA_MCP_BIN"} {
		if _, present := got[k]; present {
			t.Errorf("review role must not receive %q; got %v", k, got)
		}
	}
	if got["KUBECONFIG"] != "/home/ben/.config/kube/agents-config" {
		t.Errorf("review role lost KUBECONFIG; got %v", got)
	}
	if got["NOTION_MCP_REPOS"] != "nixos-config:prism" {
		t.Errorf("review role lost NOTION_MCP_REPOS; got %v", got)
	}
}

// TestAgentOnlyAgentEnvVars_MatchesSandboxedResolver verifies host mode and
// the sandboxed dispatch paths produce the same map for a given role.
// config.AgentEnvVarsForRole is the expression cmd/agent_run.go and
// cmd/agent_run_sandbox_exec_darwin.go assign into container.Config.
func TestAgentOnlyAgentEnvVars_MatchesSandboxedResolver(t *testing.T) {
	seedProfilesFixture(t)

	for _, role := range []string{"review-goal", "review-code", "review-context", "review-qa", "review-security", "worker", "coordinator", "investigate"} {
		t.Run(role, func(t *testing.T) {
			host := agentOnlyAgentEnvVars(SpawnOpts{AgentRole: role})
			sandboxed := config.AgentEnvVarsForRole(role)
			if !reflect.DeepEqual(host, sandboxed) {
				t.Errorf("host mode and sandboxed mode disagree for role %q:\n host      = %v\n sandboxed = %v", role, host, sandboxed)
			}
		})
	}
}

// TestAgentOnlyAgentEnvVars_ExplicitMapIsFiltered verifies a caller-supplied
// map goes through the same role filter instead of bypassing it.
func TestAgentOnlyAgentEnvVars_ExplicitMapIsFiltered(t *testing.T) {
	got := agentOnlyAgentEnvVars(SpawnOpts{
		AgentRole:    "review-security",
		AgentEnvVars: roleEnvFixtureVars(),
	})
	if _, present := got["GRAFANA_MCP_CONFIG_PATH"]; present {
		t.Errorf("explicit map must still be filtered for a review role; got %v", got)
	}

	worker := agentOnlyAgentEnvVars(SpawnOpts{
		AgentRole:    "worker",
		AgentEnvVars: roleEnvFixtureVars(),
	})
	if !reflect.DeepEqual(worker, roleEnvFixtureVars()) {
		t.Errorf("worker must receive the caller map unchanged; got %v", worker)
	}
}

// TestAgentOnlyAgentEnvVars_MissingProfilesIsNil verifies env var injection
// stays best-effort: an unreadable profiles.json yields a nil map rather than
// failing the spawn.
func TestAgentOnlyAgentEnvVars_MissingProfilesIsNil(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "no-such-dir"))
	if got := agentOnlyAgentEnvVars(SpawnOpts{AgentRole: "review-goal"}); got != nil {
		t.Errorf("missing profiles.json: got %v, want nil", got)
	}
}

// TestSpawnSession_AgentOnlyHost_ReviewRoleLaunchCmdHasNoGrafana is the
// end-to-end shape: a host-mode review session's launch command carries the
// profile env vars but neither grafana gate key. The worker sub-case proves
// the assertion is not vacuous — the same fixture leaves both keys in a
// worker's launch command.
func TestSpawnSession_AgentOnlyHost_ReviewRoleLaunchCmdHasNoGrafana(t *testing.T) {
	cases := []struct {
		role        string
		sessionName string
		wantGrafana bool
	}{
		{role: "review-code", sessionName: "myrepo@branch~review-1-review-code", wantGrafana: false},
		{role: "worker", sessionName: "myrepo@branch~env-worker", wantGrafana: true},
	}

	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			d, _ := openSpawnTestDB(t)
			argsFile := spyTmuxBin(t)
			t.Setenv("PRISM_TEST_SUBPROCESS", "1")
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			seedProfilesFixture(t)

			opts := SpawnOpts{
				SessionName:    tc.sessionName,
				Repo:           "myrepo",
				Worktree:       "/worktrees/myrepo-branch",
				AgentRole:      tc.role,
				Prompt:         "go",
				Layout:         LayoutAgentOnly,
				IsolationMode:  "host",
				PIExtensionDir: testPIExtensionDir,
			}
			if err := SpawnSession(d, opts); err != nil {
				t.Fatalf("SpawnSession: %v", err)
			}

			args := readSpyArgs(argsFile)
			joined := strings.Join(args, "\n")
			if !strings.Contains(joined, "KUBECONFIG=") {
				t.Errorf("launch command must carry the profile env vars; tmux args=%v", args)
			}
			for _, k := range []string{"GRAFANA_MCP_CONFIG_PATH=", "PI_GRAFANA_MCP_BIN="} {
				present := strings.Contains(joined, k)
				if present != tc.wantGrafana {
					t.Errorf("role %q: %q present = %v, want %v; tmux args=%v", tc.role, k, present, tc.wantGrafana, args)
				}
			}
		})
	}
}
