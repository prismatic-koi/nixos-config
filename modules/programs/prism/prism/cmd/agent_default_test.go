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

func TestBuildOpencodeCmd_UsesAgent(t *testing.T) {
	cases := []struct {
		opts session.Opts
		want string
	}{
		// With a stored opencode session ID — should use -s <id>.
		{session.Opts{Agent: "coordinator", OpencodeSession: "ses_abc123"}, "opencode --agent coordinator -s ses_abc123"},
		{session.Opts{Agent: "worker", OpencodeSession: "ses_xyz789"}, "opencode --agent worker -s ses_xyz789"},
		// No stored session — no session flag.
		{session.Opts{Agent: "coordinator"}, "opencode --agent coordinator"},
		{session.Opts{Agent: "worker"}, "opencode --agent worker"},
		// Fresh=true suppresses the stored session ID even if set.
		{session.Opts{Agent: "worker", Fresh: true, OpencodeSession: "ses_abc123"}, "opencode --agent worker"},
		// Safety-net fallback: empty agent still yields "worker".
		{session.Opts{OpencodeSession: "ses_abc123"}, "opencode --agent worker -s ses_abc123"},
		{session.Opts{}, "opencode --agent worker"},
		// Prompt with no special characters.
		{session.Opts{Agent: "worker", Prompt: "fix the login bug"}, "opencode --agent worker --prompt 'fix the login bug'"},
		// Prompt containing a single quote — exercises shellQuote escaping.
		{session.Opts{Agent: "worker", Prompt: "it's broken"}, "opencode --agent worker --prompt 'it'\\''s broken'"},
		// Prompt with shell metacharacters that are safe inside single quotes.
		{session.Opts{Agent: "worker", Prompt: "run `make test` and fix $ERRORS"}, "opencode --agent worker --prompt 'run `make test` and fix $ERRORS'"},
		// Prompt + session ID — both flags present.
		{session.Opts{Agent: "coordinator", OpencodeSession: "ses_abc", Prompt: "review pr"}, "opencode --agent coordinator -s ses_abc --prompt 'review pr'"},
		// SessionName set — PRISM_SESSION_NAME is prepended.
		{session.Opts{Agent: "worker", SessionName: "myrepo@main"}, "PRISM_SESSION_NAME='myrepo@main' opencode --agent worker"},
		{session.Opts{Agent: "coordinator", OpencodeSession: "ses_abc123", SessionName: "myrepo@main"}, "PRISM_SESSION_NAME='myrepo@main' opencode --agent coordinator -s ses_abc123"},
		// SessionName with special characters (dots and dashes are common).
		{session.Opts{Agent: "worker", SessionName: "nixos_config@feature--my-branch"}, "PRISM_SESSION_NAME='nixos_config@feature--my-branch' opencode --agent worker"},
		// No SessionName — no env var prefix.
		{session.Opts{Agent: "worker", SessionName: ""}, "opencode --agent worker"},
	}
	for _, tc := range cases {
		got := session.BuildOpencodeCmd(tc.opts)
		if got != tc.want {
			t.Errorf("BuildOpencodeCmd(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
}
