package session

// auth_check_test.go — a host-mode pi start refuses an auth.json with a
// command value (#3284).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func plantAuthJSON(t *testing.T, content string) string {
	t.Helper()
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	path := filepath.Join(agentDir, "auth.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	return path
}

// Host mode starts pi from the BuildAgentCmd command, so BuildAgentCmd
// refuses it. bwrap and sandbox-exec refuse in prism agent-run
// (cmd/agent_run_auth_check_test.go), so their pane command builds.
func TestBuildAgentCmd_RefusesCommandValueInAuthJSON(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := plantAuthJSON(t, `{"openai":{"type":"api_key","key":"!touch /tmp/x"}}`)

	opts := Opts{SessionName: "prism-test@feat", Agent: "worker", Worktree: t.TempDir(), HarnessName: "pi"}
	opts.IsolationMode = "host"
	_, err := BuildAgentCmd(opts)
	if err == nil {
		t.Fatal("BuildAgentCmd = nil error in host mode for a command value in auth.json")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), `"openai"`) || strings.Contains(err.Error(), "touch") {
		t.Errorf("error %q must name the file and the provider, and hold no value", err)
	}

	for _, mode := range []string{"bwrap", "sandbox-exec"} {
		opts.IsolationMode = mode
		if _, err := BuildAgentCmd(opts); err != nil {
			t.Errorf("BuildAgentCmd(%s) = %v; agent-run does the check for this mode", mode, err)
		}
	}
}

func TestBuildAgentCmd_HostModeAcceptsCleanAuthJSON(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	plantAuthJSON(t, `{"anthropic":{"type":"oauth","access":"a","refresh":"r","expires":1}}`)

	opts := Opts{SessionName: "prism-test@feat", Agent: "worker", Worktree: t.TempDir(), HarnessName: "pi", IsolationMode: "host"}
	if _, err := BuildAgentCmd(opts); err != nil {
		t.Fatalf("BuildAgentCmd = %v, want nil", err)
	}
}
