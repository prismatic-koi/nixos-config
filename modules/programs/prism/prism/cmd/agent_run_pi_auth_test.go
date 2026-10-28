package cmd

// agent_run_pi_auth_test.go — prism agent-run (bwrap and sandbox-exec)
// writes the per-session auth.json from the host file before pi starts, and
// records its dir for the isolators (#3295). prism cleanup removes it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

func TestPopulatePIConfig_WritesPerSessionAuthFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	agentDir := filepath.Join(os.Getenv("HOME"), ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const host = `{"anthropic":{"type":"oauth","access":"a","refresh":"r","expires":1},"openrouter":{"type":"api_key","key":"sk-or"}}`
	if err := os.WriteFile(filepath.Join(agentDir, "auth.json"), []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}

	const instanceID = "prism-test-instance-3295"
	ctrCfg := container.Config{Worktree: filepath.Join(t.TempDir(), "wt"), InstanceID: instanceID}
	if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", config.Config{PIExtensionDir: "/nix/store/fake-ext"}, piOverrides{}); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	want, err := container.PIAuthSessionAgentDir(instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if ctrCfg.PIAuthSessionDir != want {
		t.Fatalf("PIAuthSessionDir = %q, want %q", ctrCfg.PIAuthSessionDir, want)
	}
	data, err := os.ReadFile(filepath.Join(want, "auth.json"))
	if err != nil {
		t.Fatalf("read the session auth.json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil || got["anthropic"] == nil || got["openrouter"] == nil {
		t.Errorf("session auth.json = (%s, %v), want the host entries", data, err)
	}

	// prism cleanup removes the per-session auth dir.
	removeSessionInstanceDirs(instanceID)
	if _, err := os.Lstat(filepath.Dir(want)); !os.IsNotExist(err) {
		t.Errorf("per-session auth dir after cleanup: %v", err)
	}
}

// With no instance id, the sandbox gets no auth.json from the host.
func TestPopulatePIConfig_NoInstanceIDNoAuthFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	ctrCfg := container.Config{Worktree: filepath.Join(t.TempDir(), "wt")}
	if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", config.Config{PIExtensionDir: "/nix/store/fake-ext"}, piOverrides{}); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}
	if ctrCfg.PIAuthSessionDir != "" {
		t.Errorf("PIAuthSessionDir = %q, want empty", ctrCfg.PIAuthSessionDir)
	}
}
