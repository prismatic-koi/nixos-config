package container

// env_role_test.go — isolator-side coverage for the role-filtered AgentEnvVars
// map (issue #2533).
//
// The filter itself lives upstream, in internal/config. The isolator stays
// role-agnostic: it emits every key present in the map it receives (the #2235
// invariant pinned by env_test.go, which these tests do not touch). What these
// tests pin is the consequence at the boundary — hand bwrap the map a review
// role now gets, and neither grafana gate key reaches the sandbox, and the
// grafana secret bind is not emitted either.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// roleEnvFixture mirrors the production agent.envVars attrset: shared paths
// plus the grafana pair the pi extension self-gates on.
func roleEnvFixture(grafanaPath string) map[string]string {
	return map[string]string{
		"GIT_EDITOR":              "true",
		"KUBECONFIG":              "/home/ben/.config/kube/agents-config",
		"NOTION_MCP_REPOS":        "nixos-config:prism",
		"GRAFANA_MCP_CONFIG_PATH": grafanaPath,
		"PI_GRAFANA_MCP_BIN":      "/nix/store/abc-mcp-grafana/bin/mcp-grafana",
	}
}

// hasSetenvKey reports whether args contains --setenv <key> with any value.
func hasSetenvKey(args []string, key string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == key {
			return true
		}
	}
	return false
}

// grafanaSecretFixture writes a concrete secret file plus the symlink that
// stands in for the sops path, and returns the symlink path.
func grafanaSecretFixture(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	concrete := filepath.Join(tmp, "concrete-grafana-config")
	if err := os.WriteFile(concrete, []byte("GRAFANA_URL=https://x\n"), 0o600); err != nil {
		t.Fatalf("WriteFile concrete: %v", err)
	}
	symlink := filepath.Join(tmp, "grafana_config_home")
	if err := os.Symlink(concrete, symlink); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	return symlink
}

// TestBwrapBuildArgs_ReviewRoleEnvMapHasNoGrafana verifies the bwrap arg
// builder emits no grafana --setenv and no grafana --ro-bind when it is handed
// the map a review role now resolves. The worker sub-case proves the negative
// is not a no-op: the same fixture, filtered for a worker, emits both.
func TestBwrapBuildArgs_ReviewRoleEnvMapHasNoGrafana(t *testing.T) {
	secret := grafanaSecretFixture(t)
	resolved, err := filepath.EvalSymlinks(secret)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", secret, err)
	}

	t.Run("review-goal", func(t *testing.T) {
		envVars := config.FilterAgentEnvVarsForRole("review-goal", roleEnvFixture(secret))
		// AC: the map handed to the isolator carries no grafana key.
		for _, k := range []string{"GRAFANA_MCP_CONFIG_PATH", "PI_GRAFANA_MCP_BIN"} {
			if _, present := envVars[k]; present {
				t.Fatalf("map handed to the isolator still carries %q: %v", k, envVars)
			}
		}

		m, _, cleanup := bwrapFixture(t, Config{
			SessionName:   "repo@main~review-1-review-goal",
			Worktree:      t.TempDir(),
			AllocatedPort: 14010,
			AgentRole:     "review-goal",
			AgentEnvVars:  envVars,
		})
		defer cleanup()

		b := &bwrapIsolator{name: m.name}
		args := b.BuildArgs(m)

		for _, k := range []string{"GRAFANA_MCP_CONFIG_PATH", "PI_GRAFANA_MCP_BIN"} {
			if hasSetenvKey(args, k) {
				t.Errorf("bwrap must not emit --setenv %s for a review role; args=%v", k, redactedArgs(args))
			}
		}
		if hasROBindSrcDst(args, resolved, secret) {
			t.Errorf("bwrap must not bind the grafana secret for a review role; args=%v", redactedArgs(args))
		}
		// The non-grafana keys still flow.
		if !hasSetenv(args, "KUBECONFIG", "/home/ben/.config/kube/agents-config") {
			t.Errorf("review role lost KUBECONFIG; args=%v", redactedArgs(args))
		}
		if !hasSetenv(args, "NOTION_MCP_REPOS", "nixos-config:prism") {
			t.Errorf("review role lost NOTION_MCP_REPOS; args=%v", redactedArgs(args))
		}
	})

	t.Run("worker", func(t *testing.T) {
		envVars := config.FilterAgentEnvVarsForRole("worker", roleEnvFixture(secret))

		m, _, cleanup := bwrapFixture(t, Config{
			SessionName:   "repo@main",
			Worktree:      t.TempDir(),
			AllocatedPort: 14011,
			AgentRole:     "worker",
			AgentEnvVars:  envVars,
		})
		defer cleanup()

		b := &bwrapIsolator{name: m.name}
		args := b.BuildArgs(m)

		if !hasSetenv(args, "GRAFANA_MCP_CONFIG_PATH", secret) {
			t.Errorf("worker must keep --setenv GRAFANA_MCP_CONFIG_PATH; args=%v", redactedArgs(args))
		}
		if !hasSetenv(args, "PI_GRAFANA_MCP_BIN", "/nix/store/abc-mcp-grafana/bin/mcp-grafana") {
			t.Errorf("worker must keep --setenv PI_GRAFANA_MCP_BIN; args=%v", redactedArgs(args))
		}
		if !hasROBindSrcDst(args, resolved, secret) {
			t.Errorf("worker must keep the grafana secret bind; args=%v", redactedArgs(args))
		}
	})
}

// TestAppendSandboxEnvVarsKV_ReviewRoleEnvMapHasNoGrafana verifies the same
// property on the sandbox-exec K=V dispatch path: the emitter is faithful, so
// a review role's filtered map produces no grafana entry, while a coordinator
// keeps every var it receives today.
func TestAppendSandboxEnvVarsKV_ReviewRoleEnvMapHasNoGrafana(t *testing.T) {
	fixture := roleEnvFixture("/run/secrets/grafana_config_home")

	review := AppendSandboxEnvVarsKV(nil, Config{
		AgentRole:    "review-security",
		AgentEnvVars: config.FilterAgentEnvVarsForRole("review-security", fixture),
	})
	for _, unwanted := range []string{
		"GRAFANA_MCP_CONFIG_PATH=/run/secrets/grafana_config_home",
		"PI_GRAFANA_MCP_BIN=/nix/store/abc-mcp-grafana/bin/mcp-grafana",
	} {
		for _, kv := range review {
			if kv == unwanted {
				t.Errorf("sandbox-exec must not emit %q for a review role; env=%v", unwanted, redactedArgs(review))
			}
		}
	}
	if !containsKV(review, "KUBECONFIG=/home/ben/.config/kube/agents-config") {
		t.Errorf("review role lost KUBECONFIG; env=%v", redactedArgs(review))
	}

	coordinator := AppendSandboxEnvVarsKV(nil, Config{
		AgentRole:    "coordinator",
		AgentEnvVars: config.FilterAgentEnvVarsForRole("coordinator", fixture),
	})
	for _, want := range []string{
		"GRAFANA_MCP_CONFIG_PATH=/run/secrets/grafana_config_home",
		"PI_GRAFANA_MCP_BIN=/nix/store/abc-mcp-grafana/bin/mcp-grafana",
		"NOTION_MCP_REPOS=nixos-config:prism",
	} {
		if !containsKV(coordinator, want) {
			t.Errorf("coordinator must keep %q; env=%v", want, redactedArgs(coordinator))
		}
	}
}

// containsKV reports whether env contains the exact K=V entry.
func containsKV(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
