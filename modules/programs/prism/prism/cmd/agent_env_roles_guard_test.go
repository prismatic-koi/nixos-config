package cmd

// agent_env_roles_guard_test.go — wiring guard for the role-filtered
// AgentEnvVars map.
//
// The filter itself is unit-tested in internal/config, and its effect at the
// isolator boundary is tested in internal/container. Neither of those can see
// whether the two sandboxed dispatch paths actually call the filter: a revert
// to `pf.AgentEnvVars` here would hand the isolator the unfiltered map again
// and every other test would stay green.
//
// This guard reads the source of both dispatch files and asserts they resolve
// the map through config.AgentEnvVarsForRole. Reading source in a test follows
// the precedent set by internal/db/schema-version-guard_test.go. The
// sandbox-exec file is Darwin-only, so it is read as text rather than compiled
// — that keeps the guard effective on a Linux CI runner.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAgentRunDispatchPathsUseRoleFilter asserts that the bwrap dispatch
// (agent_run.go) and the sandbox-exec dispatch (agent_run_sandbox_exec_darwin.go)
// both build their AgentEnvVars map with config.AgentEnvVarsForRole, and that
// neither assigns the unfiltered profile map.
func TestAgentRunDispatchPathsUseRoleFilter(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: could not determine source file location")
	}
	dir := filepath.Dir(thisFile)

	for _, name := range []string{"agent_run.go", "agent_run_sandbox_exec_darwin.go"} {
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("ReadFile %q: %v", name, err)
			}
			body := string(src)

			const want = "agentEnvVars := config.AgentEnvVarsForRole(agentRole)"
			if !strings.Contains(body, want) {
				t.Errorf("%s must resolve AgentEnvVars with %q so the role filter runs upstream of the isolator (issue #2533)", name, want)
			}
			const banned = "pf.AgentEnvVars"
			if strings.Contains(body, banned) {
				t.Errorf("%s must not read %s directly — that hands the isolator the unfiltered map and re-opens issue #2533", name, banned)
			}
		})
	}
}
