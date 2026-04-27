package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/session"
)

// makeBareWorktree creates a temporary directory layout simulating a prism
// bare+worktree project:
//
//	<tmp>/
//	  .bare     ← IsBareRepo marker
//	  main/     ← coordinator worktree
//	  <branch>/ ← worker worktree
//
// Returns the project root path. The caller's t.TempDir() is used so cleanup
// is automatic.
func makeBareWorktree(t *testing.T, branches ...string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".bare"), []byte("gitdir"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, b := range append([]string{"main"}, branches...) {
		if err := os.MkdirAll(filepath.Join(root, b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDefaultAgent_CoordinatorForMain(t *testing.T) {
	root := makeBareWorktree(t)
	mainDir := filepath.Join(root, "main")
	got := session.DefaultAgent(mainDir, "")
	if got != "coordinator" {
		t.Errorf("DefaultAgent(%q, %q) = %q, want %q", mainDir, "", got, "coordinator")
	}
}

func TestDefaultAgent_WorkerForNonMain(t *testing.T) {
	root := makeBareWorktree(t, "feature-foo", "maintain", "main-branch")
	cases := []string{
		filepath.Join(root, "feature-foo"),
		filepath.Join(root, "maintain"),
		filepath.Join(root, "main-branch"),
	}
	for _, dir := range cases {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			got := session.DefaultAgent(dir, "")
			if got != "worker" {
				t.Errorf("DefaultAgent(%q, %q) = %q, want %q", dir, "", got, "worker")
			}
		})
	}
}

// TestDefaultAgent_NonWorktreePath verifies that directories whose parent does
// NOT contain a .bare entry return "" (non-worktree path).
func TestDefaultAgent_NonWorktreePath(t *testing.T) {
	tmp := t.TempDir()
	cases := []string{
		tmp,
		filepath.Join(tmp, "regular-repo"),
		filepath.Join(tmp, "main"), // named "main" but parent has no .bare
	}
	for _, dir := range cases {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			got := session.DefaultAgent(dir, "")
			if got != "" {
				t.Errorf("DefaultAgent(%q, %q) = %q, want %q (empty)", dir, "", got, "")
			}
		})
	}
}

func TestDefaultAgent_ExplicitOverridesDefault(t *testing.T) {
	root := makeBareWorktree(t, "feature")
	tmp := t.TempDir()
	cases := []struct {
		dir   string
		agent string
	}{
		{filepath.Join(root, "main"), "custom-agent"},
		{filepath.Join(root, "feature"), "custom-agent"},
		{filepath.Join(root, "main"), "worker"},
		{tmp, "coordinator"}, // non-worktree, explicit wins
	}
	for _, tc := range cases {
		got := session.DefaultAgent(tc.dir, tc.agent)
		if got != tc.agent {
			t.Errorf("DefaultAgent(%q, %q) = %q, want %q", tc.dir, tc.agent, got, tc.agent)
		}
	}
}

// bashTimeoutPrefix is the env-var prefix injected into all host-mode opencode commands
// when RuntimeEnvVars is populated from the opencode harness adapter.
const bashTimeoutPrefix = "OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS='900000' "

// opencodeRuntimeEnv returns the RuntimeEnvVars map matching what the opencode
// harness adapter provides. Used by tests that exercise buildDirectOpencodeCmd
// to populate Opts.RuntimeEnvVars.
func opencodeRuntimeEnv() map[string]string {
	return map[string]string{
		"OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS": "900000",
	}
}

func TestBuildOpencodeCmd_UsesAgent(t *testing.T) {
	re := opencodeRuntimeEnv()
	cases := []struct {
		opts session.Opts
		want string
	}{
		// With a stored opencode session ID — should use -s <id>.
		{session.Opts{Agent: "coordinator", OpencodeSession: "ses_abc123", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent coordinator -s ses_abc123"},
		{session.Opts{Agent: "worker", OpencodeSession: "ses_xyz789", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker -s ses_xyz789"},
		// No stored session — no session flag.
		{session.Opts{Agent: "coordinator", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent coordinator"},
		{session.Opts{Agent: "worker", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker"},
		// Fresh=true suppresses the stored session ID even if set.
		{session.Opts{Agent: "worker", Fresh: true, OpencodeSession: "ses_abc123", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker"},
		// Empty agent (non-worktree path) — no --agent flag at all.
		{session.Opts{OpencodeSession: "ses_abc123", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode -s ses_abc123"},
		{session.Opts{RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode"},
		// Prompt with no special characters.
		{session.Opts{Agent: "worker", Prompt: "fix the login bug", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker --prompt 'fix the login bug'"},
		// Prompt containing a single quote — exercises shellQuote escaping.
		{session.Opts{Agent: "worker", Prompt: "it's broken", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker --prompt 'it'\\''s broken'"},
		// Prompt with shell metacharacters that are safe inside single quotes.
		{session.Opts{Agent: "worker", Prompt: "run `make test` and fix $ERRORS", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker --prompt 'run `make test` and fix $ERRORS'"},
		// Prompt + session ID — both flags present.
		{session.Opts{Agent: "coordinator", OpencodeSession: "ses_abc", Prompt: "review pr", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent coordinator -s ses_abc --prompt 'review pr'"},
		// SessionName set — PRISM_SESSION_NAME is prepended (after the timeout prefix).
		{session.Opts{Agent: "worker", SessionName: "myrepo@main", RuntimeEnvVars: re}, bashTimeoutPrefix + "PRISM_SESSION_NAME='myrepo@main' opencode --agent worker"},
		{session.Opts{Agent: "coordinator", OpencodeSession: "ses_abc123", SessionName: "myrepo@main", RuntimeEnvVars: re}, bashTimeoutPrefix + "PRISM_SESSION_NAME='myrepo@main' opencode --agent coordinator -s ses_abc123"},
		// SessionName with special characters (dots and dashes are common).
		{session.Opts{Agent: "worker", SessionName: "nixos_config@feature--my-branch", RuntimeEnvVars: re}, bashTimeoutPrefix + "PRISM_SESSION_NAME='nixos_config@feature--my-branch' opencode --agent worker"},
		// SessionName + Prompt — env var prefix appears before the prompt flag.
		{session.Opts{Agent: "worker", SessionName: "myrepo@main", Prompt: "do the thing", RuntimeEnvVars: re}, bashTimeoutPrefix + "PRISM_SESSION_NAME='myrepo@main' opencode --agent worker --prompt 'do the thing'"},
		// No SessionName — no PRISM_SESSION_NAME, only the timeout prefix.
		{session.Opts{Agent: "worker", SessionName: "", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker"},
		// Port set — includes --port and --hostname.
		{session.Opts{Agent: "worker", Port: 14000, RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker --port 14000 --hostname 127.0.0.1"},
		// Port + session ID + SessionName — all flags together.
		{session.Opts{Agent: "coordinator", Port: 14042, OpencodeSession: "ses_abc", SessionName: "myrepo@main", RuntimeEnvVars: re}, bashTimeoutPrefix + "PRISM_SESSION_NAME='myrepo@main' opencode --agent coordinator --port 14042 --hostname 127.0.0.1 -s ses_abc"},
		// Port + Prompt.
		{session.Opts{Agent: "worker", Port: 14001, Prompt: "fix it", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker --port 14001 --hostname 127.0.0.1 --prompt 'fix it'"},
		// Port zero — no port flags.
		{session.Opts{Agent: "worker", Port: 0, RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker"},
	}
	for _, tc := range cases {
		got := session.BuildOpencodeCmd(tc.opts)
		if got != tc.want {
			t.Errorf("BuildOpencodeCmd(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
}

// TestBuildOpencodeCmd_PodmanIsolationMode verifies that IsolationMode="podman"
// produces the correct "podman attach --sig-proxy=false <container-name>" command.
func TestBuildOpencodeCmd_PodmanIsolationMode(t *testing.T) {
	re := opencodeRuntimeEnv()
	cases := []struct {
		opts session.Opts
		want string
	}{
		// Podman mode with SessionName — should use podman attach with the
		// stable container name derived from the session name (single-quoted for
		// shell safety when embedded in the readiness wait script).
		{session.Opts{IsolationMode: "podman", SessionName: "repo@main", Port: 14000}, "podman attach --sig-proxy=false 'prism-repo-main'"},
		// Different session name — container name is derived correctly.
		{session.Opts{IsolationMode: "podman", SessionName: "nixos-config@feature", Port: 14042}, "podman attach --sig-proxy=false 'prism-nixos-config-feature'"},
		// Agent role is irrelevant in podman mode (podman attach is role-agnostic).
		{session.Opts{IsolationMode: "podman", SessionName: "repo@branch", Port: 14001, Agent: "coordinator"}, "podman attach --sig-proxy=false 'prism-repo-branch'"},
		// Podman mode with no SessionName falls back to direct launch (safety net).
		// The fallback calls buildDirectOpencodeCmd, which prepends RuntimeEnvVars.
		{session.Opts{IsolationMode: "podman", SessionName: "", Port: 0, Agent: "worker", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker"},
		// Host mode includes the timeout prefix (via RuntimeEnvVars).
		{session.Opts{IsolationMode: "host", Port: 14000, Agent: "worker", RuntimeEnvVars: re}, bashTimeoutPrefix + "opencode --agent worker --port 14000 --hostname 127.0.0.1"},
	}
	for _, tc := range cases {
		got := session.BuildOpencodeCmd(tc.opts)
		if got != tc.want {
			t.Errorf("BuildOpencodeCmd(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
}
