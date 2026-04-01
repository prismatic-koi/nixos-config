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
		opts        sessionOpts
		sessionName string
		directory   string
		want        string
	}{
		// With a stored opencode session ID — should use -s <id>.
		{
			sessionOpts{agent: "coordinator", opencodeSession: "ses_abc123"},
			"nixos-config@main",
			"/home/user/repos/nixos-config/main",
			"opencode --agent coordinator -s ses_abc123",
		},
		{
			sessionOpts{agent: "build", opencodeSession: "ses_xyz789"},
			"nixos-config@feature-foo",
			"/home/user/repos/nixos-config/feature-foo",
			"opencode --agent build -s ses_xyz789",
		},
		// No stored session — no session flag.
		{
			sessionOpts{agent: "coordinator"},
			"nixos-config@main",
			"/home/user/repos/nixos-config/main",
			"opencode --agent coordinator",
		},
		{
			sessionOpts{agent: "build"},
			"nixos-config@feature-foo",
			"/home/user/repos/nixos-config/feature-foo",
			"opencode --agent build",
		},
		// fresh=true suppresses the stored session ID even if set.
		{
			sessionOpts{agent: "build", fresh: true, opencodeSession: "ses_abc123"},
			"nixos-config@main",
			"/home/user/repos/nixos-config/main",
			"opencode --agent build",
		},
		// Safety-net fallback: empty agent still yields "build".
		{
			sessionOpts{opencodeSession: "ses_abc123"},
			"nixos-config@main",
			"/home/user/repos/nixos-config/main",
			"opencode --agent build -s ses_abc123",
		},
		// Safety-net fallback with no session and empty agent.
		{
			sessionOpts{},
			"nixos-config@main",
			"/home/user/repos/nixos-config/main",
			"opencode --agent build",
		},
	}
	for _, tc := range cases {
		got := buildOpencodeCmd(tc.opts, tc.sessionName, tc.directory)
		if got != tc.want {
			t.Errorf("buildOpencodeCmd(%+v, %q, %q) = %q, want %q", tc.opts, tc.sessionName, tc.directory, got, tc.want)
		}
	}
}
