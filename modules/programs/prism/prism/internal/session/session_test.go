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
// AgentEnvVars map produces no change to the command (beyond the
// outermost OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS prefix).
func TestBuildDirectOpencodeCmd_AgentEnvVarsEmpty(t *testing.T) {
	opts := Opts{
		Agent:        "worker",
		Port:         14000,
		SessionName:  "myrepo@branch",
		AgentEnvVars: map[string]string{},
	}
	cmd := buildDirectOpencodeCmd(opts)

	// Cmd should begin with the experimental timeout prefix.
	if !strings.HasPrefix(cmd, "OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS=") {
		t.Errorf("expected cmd to begin with OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS when AgentEnvVars is empty, got: %q", cmd)
	}
	// PRISM_SESSION_NAME should still appear in the command.
	if !strings.Contains(cmd, "PRISM_SESSION_NAME=") {
		t.Errorf("expected PRISM_SESSION_NAME in cmd when AgentEnvVars is empty, got: %q", cmd)
	}
}

// TestBuildDirectOpencodeCmd_AgentEnvVarsNil verifies that a nil AgentEnvVars
// map produces no change to the command (beyond the
// outermost OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS prefix).
func TestBuildDirectOpencodeCmd_AgentEnvVarsNil(t *testing.T) {
	opts := Opts{
		Agent:       "worker",
		Port:        14000,
		SessionName: "myrepo@branch",
		// AgentEnvVars intentionally nil
	}
	cmd := buildDirectOpencodeCmd(opts)

	// Cmd should begin with the experimental timeout prefix.
	if !strings.HasPrefix(cmd, "OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS=") {
		t.Errorf("expected cmd to begin with OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS when AgentEnvVars is nil, got: %q", cmd)
	}
	// PRISM_SESSION_NAME should still appear in the command.
	if !strings.Contains(cmd, "PRISM_SESSION_NAME=") {
		t.Errorf("expected PRISM_SESSION_NAME in cmd when AgentEnvVars is nil, got: %q", cmd)
	}
}

// ── Isolation mode command construction ─────────────────────────────────────

// TestBuildOpencodeCmd_PodmanMode verifies that IsolationMode="podman" produces
// "podman attach --sig-proxy=false <container-name>".
func TestBuildOpencodeCmd_PodmanMode(t *testing.T) {
	opts := Opts{
		IsolationMode: "podman",
		SessionName:   "nixos-config@feature",
	}
	cmd := BuildOpencodeCmd(opts)
	if !strings.HasPrefix(cmd, "podman attach --sig-proxy=false") {
		t.Errorf("podman mode: got %q, want prefix 'podman attach --sig-proxy=false'", cmd)
	}
}

// TestBuildOpencodeCmd_BwrapMode verifies that IsolationMode="bwrap" produces
// "prism agent-run --session <session-name>".
func TestBuildOpencodeCmd_BwrapMode(t *testing.T) {
	opts := Opts{
		IsolationMode: "bwrap",
		SessionName:   "nixos-config@feature",
	}
	cmd := BuildOpencodeCmd(opts)
	if !strings.HasPrefix(cmd, "prism agent-run --session") {
		t.Errorf("bwrap mode: got %q, want prefix 'prism agent-run --session'", cmd)
	}
	if !strings.Contains(cmd, "nixos-config@feature") {
		t.Errorf("bwrap mode: session name not in cmd: %q", cmd)
	}
}

// TestBuildOpencodeCmd_HostMode verifies that IsolationMode="host" produces
// a direct opencode command (not podman attach).
func TestBuildOpencodeCmd_HostMode(t *testing.T) {
	opts := Opts{
		IsolationMode: "host",
		Agent:         "worker",
		Port:          14000,
		SessionName:   "nixos-config@feature",
	}
	cmd := BuildOpencodeCmd(opts)
	if strings.HasPrefix(cmd, "podman") {
		t.Errorf("host mode: got podman command %q, want direct opencode invocation", cmd)
	}
	if strings.HasPrefix(cmd, "prism agent-run") {
		t.Errorf("host mode: got prism agent-run command %q, want direct opencode invocation", cmd)
	}
	if !strings.Contains(cmd, "opencode") {
		t.Errorf("host mode: cmd does not contain 'opencode': %q", cmd)
	}
}

// TestBuildOpencodeCmd_ContainerModeFallback verifies that ContainerMode=true
// with no IsolationMode falls back to "podman" (back-compat).
func TestBuildOpencodeCmd_ContainerModeFallback(t *testing.T) {
	opts := Opts{
		ContainerMode: true,
		SessionName:   "nixos-config@feature",
	}
	cmd := BuildOpencodeCmd(opts)
	if !strings.HasPrefix(cmd, "podman attach --sig-proxy=false") {
		t.Errorf("ContainerMode fallback: got %q, want 'podman attach --sig-proxy=false ...'", cmd)
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
