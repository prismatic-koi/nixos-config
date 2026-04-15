package session

import (
	"strings"
	"testing"
)

// TestBuildDirectOpencodeCmd_AgentEnvVars verifies that AgentEnvVars are
// prepended to the command string before PRISM_SESSION_NAME in host-mode
// (ContainerMode = false) sessions.
func TestBuildDirectOpencodeCmd_AgentEnvVars(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		Port:        14000,
		SessionName: "myrepo@branch",
		AgentEnvVars: map[string]string{
			"AWS_CONFIG_FILE": "/Users/bensherman/.config/aws/readonly-config",
			"GIT_EDITOR":      "true",
			"KUBECONFIG":      "/Users/bensherman/.config/kube/agents-config",
		},
	}
	cmd := buildDirectOpencodeCmd(opts)

	// All three env vars should appear in the command.
	for _, envVar := range []string{"AWS_CONFIG_FILE", "GIT_EDITOR", "KUBECONFIG"} {
		if !strings.Contains(cmd, envVar) {
			t.Errorf("expected env var %q in cmd, got: %q", envVar, cmd)
		}
	}

	// PRISM_SESSION_NAME should appear in the command.
	sessionIdx := strings.Index(cmd, "PRISM_SESSION_NAME")
	if sessionIdx == -1 {
		t.Fatalf("PRISM_SESSION_NAME not found in cmd: %q", cmd)
	}

	// Each env var should appear before PRISM_SESSION_NAME.
	for _, envVar := range []string{"AWS_CONFIG_FILE", "GIT_EDITOR", "KUBECONFIG"} {
		envIdx := strings.Index(cmd, envVar)
		if envIdx == -1 {
			t.Errorf("env var %q not found in cmd: %q", envVar, cmd)
			continue
		}
		if envIdx > sessionIdx {
			t.Errorf("env var %q (at %d) should appear before PRISM_SESSION_NAME (at %d) in cmd: %q",
				envVar, envIdx, sessionIdx, cmd)
		}
	}

	// PRISM_SESSION_NAME should appear before the opencode binary.
	opencodeIdx := strings.Index(cmd, "opencode ")
	if opencodeIdx == -1 {
		t.Fatalf("opencode command not found in cmd: %q", cmd)
	}
	if sessionIdx > opencodeIdx {
		t.Errorf("PRISM_SESSION_NAME (at %d) should appear before opencode (at %d) in cmd: %q",
			sessionIdx, opencodeIdx, cmd)
	}

	// Keys should be in sorted order (AWS < GIT < KUBECONFIG).
	awsIdx := strings.Index(cmd, "AWS_CONFIG_FILE")
	gitIdx := strings.Index(cmd, "GIT_EDITOR")
	kubeIdx := strings.Index(cmd, "KUBECONFIG")
	if awsIdx > gitIdx || gitIdx > kubeIdx {
		t.Errorf("env vars not in sorted order (AWS=%d, GIT=%d, KUBE=%d) in cmd: %q",
			awsIdx, gitIdx, kubeIdx, cmd)
	}
}

// TestBuildDirectOpencodeCmd_AgentEnvVars_ContainerMode verifies that
// AgentEnvVars are NOT injected when ContainerMode is true, even via the
// buildDirectOpencodeCmd fallback path (ContainerMode=true, Port=0).
func TestBuildDirectOpencodeCmd_AgentEnvVars_ContainerMode(t *testing.T) {
	opts := Opts{
		Agent:         "worker",
		Port:          0, // Port=0 triggers buildDirectOpencodeCmd fallback in BuildOpencodeCmd
		SessionName:   "myrepo@branch",
		ContainerMode: true,
		AgentEnvVars: map[string]string{
			"AWS_CONFIG_FILE": "/Users/bensherman/.config/aws/readonly-config",
		},
	}
	cmd := buildDirectOpencodeCmd(opts)

	if strings.Contains(cmd, "AWS_CONFIG_FILE") {
		t.Errorf("AgentEnvVars should not be injected when ContainerMode=true, got: %q", cmd)
	}
}

// TestBuildDirectOpencodeCmd_AgentEnvVarsEmpty verifies that an empty
// AgentEnvVars map produces no change to the command.
func TestBuildDirectOpencodeCmd_AgentEnvVarsEmpty(t *testing.T) {
	opts := Opts{
		Agent:        "worker",
		Port:         14000,
		SessionName:  "myrepo@branch",
		AgentEnvVars: map[string]string{},
	}
	cmd := buildDirectOpencodeCmd(opts)

	// Should still begin with PRISM_SESSION_NAME (no env vars prepended).
	if !strings.HasPrefix(cmd, "PRISM_SESSION_NAME=") {
		t.Errorf("expected cmd to begin with PRISM_SESSION_NAME when AgentEnvVars is empty, got: %q", cmd)
	}
}

// TestBuildDirectOpencodeCmd_AgentEnvVarsNil verifies that a nil AgentEnvVars
// map produces no change to the command.
func TestBuildDirectOpencodeCmd_AgentEnvVarsNil(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		Port:        14000,
		SessionName: "myrepo@branch",
		// AgentEnvVars intentionally nil
	}
	cmd := buildDirectOpencodeCmd(opts)

	if !strings.HasPrefix(cmd, "PRISM_SESSION_NAME=") {
		t.Errorf("expected cmd to begin with PRISM_SESSION_NAME when AgentEnvVars is nil, got: %q", cmd)
	}
}

// TestBuildDirectOpencodeCmd_AgentEnvVars_ValuesQuoted verifies that env var
// values containing spaces or special characters are properly shell-quoted.
func TestBuildDirectOpencodeCmd_AgentEnvVars_ValuesQuoted(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		Port:        14000,
		SessionName: "myrepo@branch",
		AgentEnvVars: map[string]string{
			"GIT_EDITOR": "true",
		},
	}
	cmd := buildDirectOpencodeCmd(opts)

	// Value should be single-quoted.
	if !strings.Contains(cmd, "GIT_EDITOR='true'") {
		t.Errorf("expected GIT_EDITOR='true' in cmd, got: %q", cmd)
	}
}
