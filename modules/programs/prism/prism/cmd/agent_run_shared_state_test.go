package cmd

// agent_run_shared_state_test.go — prism agent-run (bwrap and sandbox-exec)
// makes the shared lock dir and the Notion store dir before pi starts, moves
// the Notion store into its dir, and records both paths for the isolators
// (#3289, #3290).

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

func TestPopulatePIConfig_SetsSharedStateDirs(t *testing.T) {
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
	const tokens = `{"accessToken":"a","refreshToken":"r","expiresAt":1,"clientId":"c"}`
	if err := os.WriteFile(filepath.Join(agentDir, "notion-mcp-oauth.json"), []byte(tokens), 0o600); err != nil {
		t.Fatal(err)
	}

	ctrCfg := container.Config{Worktree: filepath.Join(t.TempDir(), "wt")}
	if err := populatePIConfig(&ctrCfg, "prism-test@session", "worker", config.Config{PIExtensionDir: "/nix/store/fake-ext"}, piOverrides{}); err != nil {
		t.Fatalf("populatePIConfig: %v", err)
	}

	wantLock, wantNotion := container.PISharedStateDirs(home)
	if ctrCfg.PILockHostDir != wantLock || ctrCfg.PINotionHostDir != wantNotion {
		t.Fatalf("shared dirs = %q, %q, want %q, %q", ctrCfg.PILockHostDir, ctrCfg.PINotionHostDir, wantLock, wantNotion)
	}
	for _, d := range []string{wantLock, wantNotion} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s = (%v, %v), want a dir", d, fi, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(wantNotion, "notion-mcp-oauth.json")); err != nil || string(got) != tokens {
		t.Errorf("Notion token file in the store dir = (%q, %v), want the moved file", got, err)
	}
	if _, err := os.Lstat(filepath.Join(agentDir, "notion-mcp-oauth.json")); !os.IsNotExist(err) {
		t.Errorf("Notion token file still in the agent dir: %v", err)
	}
}
