package container

// env_test.go — unit coverage for the AgentEnvVars emission in
// AppendStandardEnv and AppendSandboxEnvVarsKV. Every AgentEnvVars key flows
// into both isolators' env — there is no suppression map. KUBECONFIG and the
// AWS pair are delivered at the host XDG paths; kubectl resolves the kube
// config via KUBECONFIG and the canonical-path ($HOME/.kube/config) delivery
// is not used. CLAUDE_CONFIG_DIR flows the same way: claude-code resolves its
// config dir at the host XDG path ~/.config/claude.
// PLAYWRIGHT_MCP_SANDBOX is delivered identically by both
// isolators; it suppresses chromium's nested seatbelt sandbox so the outer
// SBPL profile (and bwrap mount table on Linux) remains the sole boundary.

import (
	"testing"
)

// envSuppressionFixtureVars returns an AgentEnvVars map carrying the
// KUBECONFIG and AWS keys, the claude XDG relocation key, the
// playwright nested-sandbox suppression key, and a plain key —
// mirroring the production agent.envVars set declared by the nix module.
func envSuppressionFixtureVars() map[string]string {
	return map[string]string{
		"GIT_EDITOR":                  "true",
		"KUBECONFIG":                  "/home/ben/.config/kube/agents-config",
		"AWS_CONFIG_FILE":             "/home/ben/.config/aws/readonly-config",
		"AWS_SHARED_CREDENTIALS_FILE": "/home/ben/.config/aws/credentials",
		"CLAUDE_CONFIG_DIR":           "/home/ben/.config/claude",
		"PLAYWRIGHT_MCP_SANDBOX":      "false",
	}
}

// envFixtureWantPairs is the full K=V emission expected from the fixture —
// every AgentEnvVars key flows through, including KUBECONFIG, the AWS pair,
// the claude config dir, and the playwright nested-sandbox suppression.
var envFixtureWantPairs = []string{
	"AWS_CONFIG_FILE=/home/ben/.config/aws/readonly-config",
	"AWS_SHARED_CREDENTIALS_FILE=/home/ben/.config/aws/credentials",
	"CLAUDE_CONFIG_DIR=/home/ben/.config/claude",
	"GIT_EDITOR=true",
	"KUBECONFIG=/home/ben/.config/kube/agents-config",
	"PLAYWRIGHT_MCP_SANDBOX=false",
}

// TestAppendStandardEnv_AllAgentEnvVarsFlow verifies the bwrap arg builder
// emits every AgentEnvVars key — no suppression map remains.
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
			t.Errorf("AppendStandardEnv must emit %q (all AgentEnvVars flow since #2235); got: %v", w, redactedArgs(got))
		}
	}
	if len(got) != len(envFixtureWantPairs) {
		t.Errorf("AppendStandardEnv emitted %d vars, want exactly %d: %v", len(got), len(envFixtureWantPairs), redactedArgs(got))
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
			t.Errorf("AppendSandboxEnvVarsKV must emit %q (all AgentEnvVars flow since #2235); got: %v", w, redactedArgs(env))
		}
	}
	if len(env) != len(envFixtureWantPairs) {
		t.Errorf("AppendSandboxEnvVarsKV emitted %d vars, want exactly %d: %v", len(env), len(envFixtureWantPairs), redactedArgs(env))
	}
}
