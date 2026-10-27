package cmd

// agent_run_auth_check_test.go — prism agent-run (bwrap and sandbox-exec)
// refuses to start pi when the host auth.json holds a command value (#3284).

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

func TestPopulatePIConfig_RefusesCommandValueInAuthJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("populatePIConfig depends on POSIX exec semantics")
	}
	configHome, _ := isolateForPopulatePIConfig(t)
	writeProfiles(t, configHome, "test", "anthropic/claude-opus-4-7", "medium")

	home := os.Getenv("HOME")
	agentDir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(agentDir, "auth.json")
	run := func() error {
		ctrCfg := container.Config{Worktree: filepath.Join(t.TempDir(), "wt")}
		return populatePIConfig(&ctrCfg, "prism-test@session", "worker", config.Config{PIExtensionDir: "/nix/store/fake-ext"}, piOverrides{})
	}

	if err := os.WriteFile(authPath, []byte(`{"anthropic":{"type":"oauth","access":"a","refresh":"r","expires":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatalf("populatePIConfig with a clean auth.json: %v", err)
	}

	if err := os.WriteFile(authPath, []byte(`{"openai":{"type":"api_key","key":"!touch /tmp/x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run()
	if err == nil {
		t.Fatal("populatePIConfig = nil error for a command value in auth.json")
	}
	if !strings.Contains(err.Error(), authPath) || !strings.Contains(err.Error(), `"openai"`) || strings.Contains(err.Error(), "touch") {
		t.Errorf("error %q must name the file and the provider, and hold no value", err)
	}
}
