package cmd

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/session"
)

func TestDefaultAgent_CoordinatorForMain(t *testing.T) {
	got := session.DefaultAgent("/home/user/repos/project/main", "")
	if got != "coordinator" {
		t.Errorf("DefaultAgent(%q, %q) = %q, want %q", "/home/user/repos/project/main", "", got, "coordinator")
	}
}

func TestDefaultAgent_WorkerForNonMain(t *testing.T) {
	cases := []string{
		"/home/user/repos/project/feature-foo",
		"/home/user/repos/project/maintain",
		"/home/user/repos/project/main-branch",
		"/home/user/repos/project/MAIN",
		"/home/user/repos/project/Main",
		"/home/user",
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

func TestDefaultAgent_ExplicitOverridesDefault(t *testing.T) {
	cases := []struct {
		dir   string
		agent string
	}{
		{"/home/user/repos/project/main", "custom-agent"},
		{"/home/user/repos/project/feature", "custom-agent"},
		{"/home/user/repos/project/main", "worker"},
	}
	for _, tc := range cases {
		got := session.DefaultAgent(tc.dir, tc.agent)
		if got != tc.agent {
			t.Errorf("DefaultAgent(%q, %q) = %q, want %q", tc.dir, tc.agent, got, tc.agent)
		}
	}
}

// bashTimeoutPrefix is the env-var prefix injected into all host-mode opencode commands.
const bashTimeoutPrefix = "OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS=900000 "

func TestBuildOpencodeCmd_UsesAgent(t *testing.T) {
	cases := []struct {
		opts session.Opts
		want string
	}{
		// With a stored opencode session ID — should use -s <id>.
		{session.Opts{Agent: "coordinator", OpencodeSession: "ses_abc123"}, bashTimeoutPrefix + "opencode --agent coordinator -s ses_abc123"},
		{session.Opts{Agent: "worker", OpencodeSession: "ses_xyz789"}, bashTimeoutPrefix + "opencode --agent worker -s ses_xyz789"},
		// No stored session — no session flag.
		{session.Opts{Agent: "coordinator"}, bashTimeoutPrefix + "opencode --agent coordinator"},
		{session.Opts{Agent: "worker"}, bashTimeoutPrefix + "opencode --agent worker"},
		// Fresh=true suppresses the stored session ID even if set.
		{session.Opts{Agent: "worker", Fresh: true, OpencodeSession: "ses_abc123"}, bashTimeoutPrefix + "opencode --agent worker"},
		// Safety-net fallback: empty agent still yields "worker".
		{session.Opts{OpencodeSession: "ses_abc123"}, bashTimeoutPrefix + "opencode --agent worker -s ses_abc123"},
		{session.Opts{}, bashTimeoutPrefix + "opencode --agent worker"},
		// Prompt with no special characters.
		{session.Opts{Agent: "worker", Prompt: "fix the login bug"}, bashTimeoutPrefix + "opencode --agent worker --prompt 'fix the login bug'"},
		// Prompt containing a single quote — exercises shellQuote escaping.
		{session.Opts{Agent: "worker", Prompt: "it's broken"}, bashTimeoutPrefix + "opencode --agent worker --prompt 'it'\\''s broken'"},
		// Prompt with shell metacharacters that are safe inside single quotes.
		{session.Opts{Agent: "worker", Prompt: "run `make test` and fix $ERRORS"}, bashTimeoutPrefix + "opencode --agent worker --prompt 'run `make test` and fix $ERRORS'"},
		// Prompt + session ID — both flags present.
		{session.Opts{Agent: "coordinator", OpencodeSession: "ses_abc", Prompt: "review pr"}, bashTimeoutPrefix + "opencode --agent coordinator -s ses_abc --prompt 'review pr'"},
		// SessionName set — PRISM_SESSION_NAME is prepended (after the timeout prefix).
		{session.Opts{Agent: "worker", SessionName: "myrepo@main"}, bashTimeoutPrefix + "PRISM_SESSION_NAME='myrepo@main' opencode --agent worker"},
		{session.Opts{Agent: "coordinator", OpencodeSession: "ses_abc123", SessionName: "myrepo@main"}, bashTimeoutPrefix + "PRISM_SESSION_NAME='myrepo@main' opencode --agent coordinator -s ses_abc123"},
		// SessionName with special characters (dots and dashes are common).
		{session.Opts{Agent: "worker", SessionName: "nixos_config@feature--my-branch"}, bashTimeoutPrefix + "PRISM_SESSION_NAME='nixos_config@feature--my-branch' opencode --agent worker"},
		// SessionName + Prompt — env var prefix appears before the prompt flag.
		{session.Opts{Agent: "worker", SessionName: "myrepo@main", Prompt: "do the thing"}, bashTimeoutPrefix + "PRISM_SESSION_NAME='myrepo@main' opencode --agent worker --prompt 'do the thing'"},
		// No SessionName — no PRISM_SESSION_NAME, only the timeout prefix.
		{session.Opts{Agent: "worker", SessionName: ""}, bashTimeoutPrefix + "opencode --agent worker"},
		// Port set — includes --port and --hostname.
		{session.Opts{Agent: "worker", Port: 14000}, bashTimeoutPrefix + "opencode --agent worker --port 14000 --hostname 127.0.0.1"},
		// Port + session ID + SessionName — all flags together.
		{session.Opts{Agent: "coordinator", Port: 14042, OpencodeSession: "ses_abc", SessionName: "myrepo@main"}, bashTimeoutPrefix + "PRISM_SESSION_NAME='myrepo@main' opencode --agent coordinator --port 14042 --hostname 127.0.0.1 -s ses_abc"},
		// Port + Prompt.
		{session.Opts{Agent: "worker", Port: 14001, Prompt: "fix it"}, bashTimeoutPrefix + "opencode --agent worker --port 14001 --hostname 127.0.0.1 --prompt 'fix it'"},
		// Port zero — no port flags.
		{session.Opts{Agent: "worker", Port: 0}, bashTimeoutPrefix + "opencode --agent worker"},
	}
	for _, tc := range cases {
		got := session.BuildOpencodeCmd(tc.opts)
		if got != tc.want {
			t.Errorf("BuildOpencodeCmd(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
}

// TestBuildOpencodeCmd_ContainerMode verifies that container mode produces the
// correct "podman attach --sig-proxy=false <container-name>" command (RFC #691, Phase 1a / Issue #715).
func TestBuildOpencodeCmd_ContainerMode(t *testing.T) {
	cases := []struct {
		opts session.Opts
		want string
	}{
		// Container mode with SessionName — should use podman attach with the
		// stable container name derived from the session name (single-quoted for
		// shell safety when embedded in the readiness wait script).
		{session.Opts{ContainerMode: true, SessionName: "repo@main", Port: 14000}, "podman attach --sig-proxy=false 'prism-repo-main'"},
		// Different session name — container name is derived correctly.
		{session.Opts{ContainerMode: true, SessionName: "nixos-config@feature", Port: 14042}, "podman attach --sig-proxy=false 'prism-nixos-config-feature'"},
		// Agent role is irrelevant in container mode (podman attach is role-agnostic).
		{session.Opts{ContainerMode: true, SessionName: "repo@branch", Port: 14001, Agent: "coordinator"}, "podman attach --sig-proxy=false 'prism-repo-branch'"},
		// Container mode with no SessionName falls back to direct launch (safety net).
		// The fallback calls buildDirectOpencodeCmd, which now prepends the timeout prefix.
		{session.Opts{ContainerMode: true, SessionName: "", Port: 0, Agent: "worker"}, bashTimeoutPrefix + "opencode --agent worker"},
		// Non-container mode includes the timeout prefix.
		{session.Opts{ContainerMode: false, Port: 14000, Agent: "worker"}, bashTimeoutPrefix + "opencode --agent worker --port 14000 --hostname 127.0.0.1"},
	}
	for _, tc := range cases {
		got := session.BuildOpencodeCmd(tc.opts)
		if got != tc.want {
			t.Errorf("BuildOpencodeCmd(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
}
