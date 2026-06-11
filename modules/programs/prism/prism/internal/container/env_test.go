package container

// env_test.go — unit coverage for the sandboxMountedByDefault suppression
// maps in AppendStandardEnv and AppendSandboxEnvVarsKV (issue #2234, Step 3a
// of #2132): AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE must flow into
// both isolators' env (the canonical-path delivery was dropped — the aws CLI
// resolves the files via these env vars at the host XDG paths), while
// KUBECONFIG remains suppressed until Step 3b.

import (
	"strings"
	"testing"
)

// envSuppressionFixtureVars returns an AgentEnvVars map carrying the three
// historically-suppressed keys plus a plain key.
func envSuppressionFixtureVars() map[string]string {
	return map[string]string{
		"GIT_EDITOR":                  "true",
		"KUBECONFIG":                  "/home/ben/.config/kube/agents-config",
		"AWS_CONFIG_FILE":             "/home/ben/.config/aws/readonly-config",
		"AWS_SHARED_CREDENTIALS_FILE": "/home/ben/.config/aws/credentials",
	}
}

// TestAppendStandardEnv_AWSEnvVarsFlow_KubeconfigSuppressed verifies the
// suppression-map shape used by the bwrap arg builder.
func TestAppendStandardEnv_AWSEnvVarsFlow_KubeconfigSuppressed(t *testing.T) {
	cfg := Config{AgentEnvVars: envSuppressionFixtureVars()}

	var got []string
	_ = AppendStandardEnv(nil, cfg, func(args []string, k, v string) []string {
		got = append(got, k+"="+v)
		return args
	})

	want := []string{
		"AWS_CONFIG_FILE=/home/ben/.config/aws/readonly-config",
		"AWS_SHARED_CREDENTIALS_FILE=/home/ben/.config/aws/credentials",
		"GIT_EDITOR=true",
	}
	for _, w := range want {
		found := false
		for _, kv := range got {
			if kv == w {
				found = true
			}
		}
		if !found {
			t.Errorf("AppendStandardEnv must emit %q (AWS vars flow since #2234); got: %v", w, got)
		}
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "KUBECONFIG=") {
			t.Errorf("AppendStandardEnv must suppress KUBECONFIG (canonical-path delivery until Step 3b of #2132); got: %v", got)
		}
	}
}

// TestAppendSandboxEnvVarsKV_AWSEnvVarsFlow_KubeconfigSuppressed verifies the
// same shape for the sandbox-exec K=V dispatch path.
func TestAppendSandboxEnvVarsKV_AWSEnvVarsFlow_KubeconfigSuppressed(t *testing.T) {
	cfg := Config{AgentEnvVars: envSuppressionFixtureVars()}

	env := AppendSandboxEnvVarsKV(nil, cfg)

	want := []string{
		"AWS_CONFIG_FILE=/home/ben/.config/aws/readonly-config",
		"AWS_SHARED_CREDENTIALS_FILE=/home/ben/.config/aws/credentials",
		"GIT_EDITOR=true",
	}
	for _, w := range want {
		found := false
		for _, kv := range env {
			if kv == w {
				found = true
			}
		}
		if !found {
			t.Errorf("AppendSandboxEnvVarsKV must emit %q (AWS vars flow since #2234); got: %v", w, env)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "KUBECONFIG=") {
			t.Errorf("AppendSandboxEnvVarsKV must suppress KUBECONFIG (canonical-path delivery until Step 3b of #2132); got: %v", env)
		}
	}
}
