package session

// Tests for the isolation-mode argv branching in muxPaneOptsFor (#2176).
//
// The pre-#2176 implementation returned Argv = ["/bin/sh"] for every
// pane regardless of isolation mode, so the agent harness never
// launched and SpawnSession's WaitForReady gate timed out after 30s.
// These tests pin the corrected branch: the `agent` pane's argv runs
// the harness (host) or `prism agent-run` (bwrap / sandbox-exec)
// wrapped in `sh -c …`; the `edit` / `term` / `shell` panes stay on
// plain `/bin/sh`.

import (
	"strings"
	"testing"
)

// TestMuxPaneOptsFor_AgentPane_BwrapMode_RunsAgentRun asserts that
// the bwrap branch's agent argv invokes `prism agent-run --session
// <name>`. This is the pre-#2176 spawn-hang fix: without this
// branch, the agent pane runs /bin/sh and pi never starts.
func TestMuxPaneOptsFor_AgentPane_BwrapMode_RunsAgentRun(t *testing.T) {
	opts := SpawnOpts{
		SessionName: "repo@feat",
		Worktree:    "/tmp/repo/feat",
		AgentRole:   "worker",
	}
	got := muxPaneOptsFor(opts, 0, "agent", "bwrap")

	if len(got.Argv) < 3 {
		t.Fatalf("agent argv too short: %v (want at least 3 elements)", got.Argv)
	}
	if !isShellExecArgv(got.Argv) {
		t.Fatalf("agent argv = %v, want [sh, -c, <cmd>] shape", got.Argv)
	}
	cmd := got.Argv[2]
	if !strings.Contains(cmd, "prism agent-run") {
		t.Errorf("bwrap agent cmd = %q, want it to invoke `prism agent-run`", cmd)
	}
	if !strings.Contains(cmd, "repo@feat") {
		t.Errorf("bwrap agent cmd = %q, want --session repo@feat", cmd)
	}
}

// TestMuxPaneOptsFor_AgentPane_SandboxExecMode_RunsAgentRun mirrors
// the bwrap test for the sandbox-exec branch. Both modes route the
// agent through `prism agent-run`, so the assertion is the same
// shape.
func TestMuxPaneOptsFor_AgentPane_SandboxExecMode_RunsAgentRun(t *testing.T) {
	opts := SpawnOpts{
		SessionName: "repo@review",
		Worktree:    "/tmp/repo/review",
		AgentRole:   "review-code",
	}
	got := muxPaneOptsFor(opts, 0, "agent", "sandbox-exec")

	if !isShellExecArgv(got.Argv) {
		t.Fatalf("sandbox-exec agent argv = %v, want [sh, -c, <cmd>] shape", got.Argv)
	}
	cmd := got.Argv[2]
	if !strings.Contains(cmd, "prism agent-run") {
		t.Errorf("sandbox-exec agent cmd = %q, want `prism agent-run`", cmd)
	}
}

// TestMuxPaneOptsFor_AgentPane_HostMode_RunsHarnessDirectly verifies
// that host mode bypasses `prism agent-run` and execs the harness
// (pi) directly. The exact flag shape comes from buildDirectAgentCmd
// which is tested in session_test.go; here we just assert the cmd
// is NOT `prism agent-run`.
func TestMuxPaneOptsFor_AgentPane_HostMode_RunsHarnessDirectly(t *testing.T) {
	opts := SpawnOpts{
		SessionName: "repo@feat",
		Worktree:    "/tmp/repo/feat",
		AgentRole:   "worker",
		Prompt:      "hello",
	}
	got := muxPaneOptsFor(opts, 0, "agent", "host")

	if !isShellExecArgv(got.Argv) {
		t.Fatalf("host agent argv = %v, want [sh, -c, <cmd>] shape", got.Argv)
	}
	cmd := got.Argv[2]
	if strings.Contains(cmd, "prism agent-run") {
		t.Errorf("host agent cmd = %q, MUST NOT route through `prism agent-run` (host execs the harness directly)", cmd)
	}
	// The pi harness is the default; the cmd should reference it.
	if !strings.HasPrefix(cmd, "pi") && !strings.Contains(cmd, " pi ") {
		t.Errorf("host agent cmd = %q, want it to invoke pi", cmd)
	}
}

// TestMuxPaneOptsFor_NonAgentPanes_StayShell pins the non-regression
// path: the edit / term / shell panes should NOT run the agent
// harness. They are plain shells regardless of isolation mode.
func TestMuxPaneOptsFor_NonAgentPanes_StayShell(t *testing.T) {
	opts := SpawnOpts{
		SessionName: "repo@feat",
		Worktree:    "/tmp/repo/feat",
	}
	for _, name := range []string{"edit", "term", "shell"} {
		t.Run(name, func(t *testing.T) {
			for _, mode := range []string{"bwrap", "sandbox-exec", "host", "podman"} {
				got := muxPaneOptsFor(opts, 0, name, mode)
				if len(got.Argv) != 1 {
					t.Errorf("(%s, %s) argv = %v, want single-element [sh]", name, mode, got.Argv)
				}
				if len(got.Argv) > 0 && !strings.HasSuffix(got.Argv[0], "sh") {
					t.Errorf("(%s, %s) argv[0] = %q, want a shell path", name, mode, got.Argv[0])
				}
			}
		})
	}
}

// TestMuxPaneOptsFor_AgentPane_CwdIsWorktree pins the per-pane
// working directory invariant. The agent harness needs to start in
// the worktree so relative path references in the prompt and the
// agent's tool calls resolve to the right repo.
func TestMuxPaneOptsFor_AgentPane_CwdIsWorktree(t *testing.T) {
	opts := SpawnOpts{
		SessionName: "repo@feat",
		Worktree:    "/abs/path/to/worktree",
	}
	for _, mode := range []string{"host", "bwrap", "sandbox-exec"} {
		got := muxPaneOptsFor(opts, 0, "agent", mode)
		if got.Cwd != "/abs/path/to/worktree" {
			t.Errorf("(agent, %s) Cwd = %q, want /abs/path/to/worktree", mode, got.Cwd)
		}
	}
}

// TestMuxPaneOptsFor_AgentPane_HostMode_HarnessPipeEnv pins the
// PRISM_HARNESS_PIPE env-var plumbing for host mode + socket-pipe
// harness. agentPaneEnvVars writes this var for host mode when
// HarnessPipeSockPath is non-empty; muxPaneOptsFor must propagate
// it onto the pane's env so the PI extension can find the sidecar.
func TestMuxPaneOptsFor_AgentPane_HostMode_HarnessPipeEnv(t *testing.T) {
	opts := SpawnOpts{
		SessionName:         "repo@feat",
		Worktree:            "/tmp/repo/feat",
		HarnessPipeSockPath: "/tmp/sidecar.sock",
	}
	got := muxPaneOptsFor(opts, 0, "agent", "host")
	if got.Env == nil {
		t.Fatal("Env is nil; expected non-nil env carrying PRISM_HARNESS_PIPE")
	}
	v, ok := got.Env["PRISM_HARNESS_PIPE"]
	if !ok {
		t.Fatalf("PRISM_HARNESS_PIPE missing from env; got keys: %v", envKeys(got.Env))
	}
	want := "unix:///tmp/sidecar.sock"
	if v != want {
		t.Errorf("PRISM_HARNESS_PIPE = %q, want %q", v, want)
	}
}

// TestMuxPaneOptsFor_AgentPane_SandboxedMode_InitialPromptFileEnv
// pins the per-session prompt-file plumbing. Sandboxed modes
// (bwrap / sandbox-exec) carry the prompt body in a file path via
// PRISM_INITIAL_PROMPT_FILE so the launch command stays O(1) in
// prompt size (#1092 / #1195).
func TestMuxPaneOptsFor_AgentPane_SandboxedMode_InitialPromptFileEnv(t *testing.T) {
	opts := SpawnOpts{
		SessionName:    "repo@feat",
		Worktree:       "/tmp/repo/feat",
		Prompt:         "hello",
		PromptFilePath: "/tmp/run/repo@feat/initial-prompt.txt",
	}
	for _, mode := range []string{"bwrap", "sandbox-exec"} {
		got := muxPaneOptsFor(opts, 0, "agent", mode)
		v, ok := got.Env["PRISM_INITIAL_PROMPT_FILE"]
		if !ok {
			t.Errorf("(%s) PRISM_INITIAL_PROMPT_FILE missing from env; got keys: %v", mode, envKeys(got.Env))
			continue
		}
		if v != "/tmp/run/repo@feat/initial-prompt.txt" {
			t.Errorf("(%s) PRISM_INITIAL_PROMPT_FILE = %q, want the prompt-file path", mode, v)
		}
	}
}

// isShellExecArgv returns true when argv looks like ["<shell>", "-c",
// "<cmd>"] — the canonical wrap muxPaneOptsFor uses for the agent
// pane so shell features (env expansion, $(cat <file>), the podman
// readiness loop) work the same way they do under tmux's `sh -c`.
func isShellExecArgv(argv []string) bool {
	if len(argv) != 3 {
		return false
	}
	if !strings.HasSuffix(argv[0], "sh") {
		return false
	}
	return argv[1] == "-c"
}

// envKeys is a tiny helper to surface a missing-key failure with a
// readable list of what env vars WERE present.
func envKeys(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	return out
}
