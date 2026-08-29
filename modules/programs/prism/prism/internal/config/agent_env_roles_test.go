package config_test

// agent_env_roles_test.go — coverage for the role-aware AgentEnvVars filter.
//
// The filter is the capability boundary that keeps the pi grafana MCP surface
// (65 tools, about 26400 cached tokens) out of review agents, which never call
// a grafana tool. Every test here is hermetic: profiles.json fixtures live
// under a per-test $XDG_CONFIG_HOME.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// grafanaEnvKeys are the two keys the pi grafana extension self-gates on.
var grafanaEnvKeys = []string{"GRAFANA_MCP_CONFIG_PATH", "PI_GRAFANA_MCP_BIN"}

// reviewRoles is the canonical five-agent review set.
var reviewRoles = []string{
	"review-goal",
	"review-code",
	"review-context",
	"review-qa",
	"review-security",
}

// agentEnvFixture mirrors the production agent.envVars attrset rendered by the
// nix module into profiles.json: the shared paths, the notion repo allowlist,
// an atlassian entry, and the grafana pair.
func agentEnvFixture() map[string]string {
	return map[string]string{
		"GIT_EDITOR":                  "true",
		"KUBECONFIG":                  "/home/ben/.config/kube/agents-config",
		"AWS_CONFIG_FILE":             "/home/ben/.config/aws/readonly-config",
		"AWS_SHARED_CREDENTIALS_FILE": "/home/ben/.config/aws/credentials",
		"CLAUDE_CONFIG_DIR":           "/home/ben/.config/claude",
		"PLAYWRIGHT_MCP_SANDBOX":      "false",
		"NOTION_MCP_REPOS":            "nixos-config:prism",
		"ATLASSIAN_DEFAULT_CLOUD_ID":  "cloud-id",
		"GRAFANA_MCP_CONFIG_PATH":     "/run/secrets/grafana_config_home",
		"PI_GRAFANA_MCP_BIN":          "/nix/store/abc-mcp-grafana/bin/mcp-grafana",
	}
}

// writeProfilesFixture writes a profiles.json carrying vars as agent_env_vars
// and points $XDG_CONFIG_HOME at its parent.
func writeProfilesFixture(t *testing.T, vars map[string]string) {
	t.Helper()
	dir := t.TempDir()
	prismDir := filepath.Join(dir, "prism")
	if err := os.MkdirAll(prismDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %q: %v", prismDir, err)
	}
	body, err := json.Marshal(config.ProfilesFile{
		Default:      "anthropic",
		Profiles:     map[string]config.ProfileEntry{"anthropic": {}},
		AgentEnvVars: vars,
	})
	if err != nil {
		t.Fatalf("Marshal profiles fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prismDir, "profiles.json"), body, 0o644); err != nil {
		t.Fatalf("WriteFile profiles.json: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", dir)
}

// assertNoGrafanaKeys fails when got carries either grafana gate key.
func assertNoGrafanaKeys(t *testing.T, context string, got map[string]string) {
	t.Helper()
	for _, k := range grafanaEnvKeys {
		if _, present := got[k]; present {
			t.Errorf("%s: key %q must not reach this role (issue #2533); got keys: %v", context, k, sortedKeys(got))
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestFilterAgentEnvVarsForRole_ReviewRolesLoseGrafana verifies that each of
// the five review roles loses both grafana gate keys and keeps every other
// key unchanged.
func TestFilterAgentEnvVarsForRole_ReviewRolesLoseGrafana(t *testing.T) {
	for _, role := range reviewRoles {
		t.Run(role, func(t *testing.T) {
			got := config.FilterAgentEnvVarsForRole(role, agentEnvFixture())
			assertNoGrafanaKeys(t, "FilterAgentEnvVarsForRole("+role+")", got)

			want := agentEnvFixture()
			for _, k := range grafanaEnvKeys {
				delete(want, k)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("FilterAgentEnvVarsForRole(%q) = %v, want %v", role, got, want)
			}
		})
	}
}

// TestFilterAgentEnvVarsForRole_NonReviewRolesUnfiltered verifies that every
// non-review role — including a role name outside the known set — receives the
// unfiltered map. An unrecognised role must never silently lose capability.
func TestFilterAgentEnvVarsForRole_NonReviewRolesUnfiltered(t *testing.T) {
	roles := []string{
		"coordinator",
		"worker",
		"investigate",
		"ac",
		"retro",
		"",
		"review",             // prefix of a review role, but not one of them
		"review-performance", // plausible future role, not yet known
		"totally-made-up",
	}
	for _, role := range roles {
		t.Run("role="+role, func(t *testing.T) {
			got := config.FilterAgentEnvVarsForRole(role, agentEnvFixture())
			if !reflect.DeepEqual(got, agentEnvFixture()) {
				t.Errorf("FilterAgentEnvVarsForRole(%q) = %v, want the unfiltered map %v",
					role, got, agentEnvFixture())
			}
		})
	}
}

// TestFilterAgentEnvVarsForRole_DoesNotMutateInput verifies the filter copies
// rather than deleting in place. The input map is the shared
// ProfilesFile.AgentEnvVars; mutating it would strip grafana for every later
// caller in the same process.
func TestFilterAgentEnvVarsForRole_DoesNotMutateInput(t *testing.T) {
	in := agentEnvFixture()
	_ = config.FilterAgentEnvVarsForRole("review-goal", in)
	if !reflect.DeepEqual(in, agentEnvFixture()) {
		t.Errorf("input map was modified: got %v, want %v", in, agentEnvFixture())
	}
}

// TestFilterAgentEnvVarsForRole_NilAndEmpty verifies the nil and empty inputs
// stay nil and empty for both a review role and a worker role.
func TestFilterAgentEnvVarsForRole_NilAndEmpty(t *testing.T) {
	if got := config.FilterAgentEnvVarsForRole("review-goal", nil); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
	if got := config.FilterAgentEnvVarsForRole("worker", nil); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
	if got := config.FilterAgentEnvVarsForRole("review-goal", map[string]string{}); len(got) != 0 {
		t.Errorf("empty input: got %v, want an empty map", got)
	}
}

// TestAgentEnvVarsForRole_LoadsAndFilters verifies the loader resolves
// profiles.json and applies the same filter: a review role loses grafana, a
// worker keeps it.
func TestAgentEnvVarsForRole_LoadsAndFilters(t *testing.T) {
	writeProfilesFixture(t, agentEnvFixture())

	review := config.AgentEnvVarsForRole("review-security")
	assertNoGrafanaKeys(t, "AgentEnvVarsForRole(review-security)", review)
	if review["KUBECONFIG"] != "/home/ben/.config/kube/agents-config" {
		t.Errorf("review role lost a non-grafana key: KUBECONFIG = %q", review["KUBECONFIG"])
	}

	worker := config.AgentEnvVarsForRole("worker")
	for _, k := range grafanaEnvKeys {
		if worker[k] == "" {
			t.Errorf("worker must keep %q; got map %v", k, sortedKeys(worker))
		}
	}
}

// TestAgentEnvVarsForRole_MissingProfilesIsNil verifies that an unreadable
// profiles.json yields a nil map rather than an error — env var injection is
// best-effort for every caller.
func TestAgentEnvVarsForRole_MissingProfilesIsNil(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "no-such-dir"))
	if got := config.AgentEnvVarsForRole("worker"); got != nil {
		t.Errorf("missing profiles.json: got %v, want nil", got)
	}
	if got := config.AgentEnvVarsForRole("review-goal"); got != nil {
		t.Errorf("missing profiles.json: got %v, want nil", got)
	}
}
