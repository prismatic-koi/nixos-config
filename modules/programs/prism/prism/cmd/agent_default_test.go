package cmd

import (
	"path/filepath"
	"testing"
)

func TestDefaultAgent_CoordinatorForMain(t *testing.T) {
	got := defaultAgent("/home/user/repos/project/main", "")
	if got != "coordinator" {
		t.Errorf("defaultAgent(%q, %q) = %q, want %q", "/home/user/repos/project/main", "", got, "coordinator")
	}
}

func TestDefaultAgent_BuildForNonMain(t *testing.T) {
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
			got := defaultAgent(dir, "")
			if got != "build" {
				t.Errorf("defaultAgent(%q, %q) = %q, want %q", dir, "", got, "build")
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
		{"/home/user/repos/project/main", "build"},
	}
	for _, tc := range cases {
		got := defaultAgent(tc.dir, tc.agent)
		if got != tc.agent {
			t.Errorf("defaultAgent(%q, %q) = %q, want %q", tc.dir, tc.agent, got, tc.agent)
		}
	}
}

func TestBuildOpencodeCmd_UsesAgent(t *testing.T) {
	cases := []struct {
		opts sessionOpts
		want string
	}{
		// With a stored opencode session ID — should use -s <id>.
		{sessionOpts{agent: "coordinator", opencodeSession: "ses_abc123"}, "opencode --agent coordinator -s ses_abc123"},
		{sessionOpts{agent: "build", opencodeSession: "ses_xyz789"}, "opencode --agent build -s ses_xyz789"},
		// No stored session — no session flag.
		{sessionOpts{agent: "coordinator"}, "opencode --agent coordinator"},
		{sessionOpts{agent: "build"}, "opencode --agent build"},
		// fresh=true suppresses the stored session ID even if set.
		{sessionOpts{agent: "build", fresh: true, opencodeSession: "ses_abc123"}, "opencode --agent build"},
		// Safety-net fallback: empty agent still yields "build".
		{sessionOpts{opencodeSession: "ses_abc123"}, "opencode --agent build -s ses_abc123"},
		{sessionOpts{}, "opencode --agent build"},
		// Prompt with no special characters.
		{sessionOpts{agent: "build", prompt: "fix the login bug"}, "opencode --agent build --prompt 'fix the login bug'"},
		// Prompt containing a single quote — exercises shellQuote escaping.
		{sessionOpts{agent: "build", prompt: "it's broken"}, "opencode --agent build --prompt 'it'\\''s broken'"},
		// Prompt with shell metacharacters that are safe inside single quotes.
		{sessionOpts{agent: "build", prompt: "run `make test` and fix $ERRORS"}, "opencode --agent build --prompt 'run `make test` and fix $ERRORS'"},
		// Prompt + session ID — both flags present.
		{sessionOpts{agent: "coordinator", opencodeSession: "ses_abc", prompt: "review pr"}, "opencode --agent coordinator -s ses_abc --prompt 'review pr'"},
	}
	for _, tc := range cases {
		got := buildOpencodeCmd(tc.opts)
		if got != tc.want {
			t.Errorf("buildOpencodeCmd(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
}
