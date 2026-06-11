package container

// env_test.go — unit coverage for the AgentEnvVars emission in
// AppendStandardEnv and AppendSandboxEnvVarsKV. The former
// sandboxMountedByDefault suppression maps are gone (issue #2235, Step 3b of
// #2132): KUBECONFIG — their last entry — now flows into both isolators'
// env alongside the AWS pair (un-suppressed in #2234, Step 3a). kubectl
// resolves the kube config via KUBECONFIG at the host XDG path; the
// canonical-path ($HOME/.kube/config) delivery was dropped from both
// isolators. CLAUDE_CONFIG_DIR (issue #2243, Step 3c) flows the same way:
// claude-code resolves its config dir at the host XDG path ~/.config/claude.

import (
	"testing"
)

// envSuppressionFixtureVars returns an AgentEnvVars map carrying the
// historically-suppressed keys, the claude XDG relocation key (#2243), and
// a plain key — mirroring the production agent.envVars set declared by the
// nix module.
func envSuppressionFixtureVars() map[string]string {
	return map[string]string{
		"GIT_EDITOR":                  "true",
		"KUBECONFIG":                  "/home/ben/.config/kube/agents-config",
		"AWS_CONFIG_FILE":             "/home/ben/.config/aws/readonly-config",
		"AWS_SHARED_CREDENTIALS_FILE": "/home/ben/.config/aws/credentials",
		"CLAUDE_CONFIG_DIR":           "/home/ben/.config/claude",
	}
}

// envFixtureWantPairs is the full K=V emission expected from the fixture —
// every AgentEnvVars key flows through, including the historically
// suppressed KUBECONFIG (#2235), the AWS pair (#2234), and the claude
// config dir (#2243).
var envFixtureWantPairs = []string{
	"AWS_CONFIG_FILE=/home/ben/.config/aws/readonly-config",
	"AWS_SHARED_CREDENTIALS_FILE=/home/ben/.config/aws/credentials",
	"CLAUDE_CONFIG_DIR=/home/ben/.config/claude",
	"GIT_EDITOR=true",
	"KUBECONFIG=/home/ben/.config/kube/agents-config",
}

// TestAppendStandardEnv_AllAgentEnvVarsFlow verifies the bwrap arg builder
// emits every AgentEnvVars key — no suppression map remains (issue #2235).
func TestAppendStandardEnv_AllAgentEnvVarsFlow(t *testing.T) {
	cfg := Config{AgentEnvVars: envSuppressionFixtureVars()}

	var got []string
	_ = AppendStandardEnv(nil, cfg, func(args []string, k, v string) []string {
		got = append(got, k+"="+v)
		return args
	})

	for _, w := range envFixtureWantPairs {
		found := false
		for _, kv := range got {
			if kv == w {
				found = true
			}
		}
		if !found {
			t.Errorf("AppendStandardEnv must emit %q (all AgentEnvVars flow since #2235); got: %v", w, got)
		}
	}
	if len(got) != len(envFixtureWantPairs) {
		t.Errorf("AppendStandardEnv emitted %d vars, want exactly %d: %v", len(got), len(envFixtureWantPairs), got)
	}
}

// TestAppendSandboxEnvVarsKV_AllAgentEnvVarsFlow verifies the same shape for
// the sandbox-exec K=V dispatch path.
func TestAppendSandboxEnvVarsKV_AllAgentEnvVarsFlow(t *testing.T) {
	cfg := Config{AgentEnvVars: envSuppressionFixtureVars()}

	env := AppendSandboxEnvVarsKV(nil, cfg)

	for _, w := range envFixtureWantPairs {
		found := false
		for _, kv := range env {
			if kv == w {
				found = true
			}
		}
		if !found {
			t.Errorf("AppendSandboxEnvVarsKV must emit %q (all AgentEnvVars flow since #2235); got: %v", w, env)
		}
	}
	if len(env) != len(envFixtureWantPairs) {
		t.Errorf("AppendSandboxEnvVarsKV emitted %d vars, want exactly %d: %v", len(env), len(envFixtureWantPairs), env)
	}
}
