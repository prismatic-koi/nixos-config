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
		{sessionOpts{agent: "coordinator"}, "nixos-config@main", "/home/user/repos/nixos-config/main", "opencode --agent coordinator --continue"},
		{sessionOpts{agent: "build"}, "nixos-config@feature-foo", "/home/user/repos/nixos-config/feature-foo", "opencode --agent build --session nixos-config@feature-foo"},
		{sessionOpts{agent: "custom"}, "nixos-config@custom", "/home/user/repos/nixos-config/custom", "opencode --agent custom --session nixos-config@custom"},
		// Safety-net fallback: empty agent still yields "build".
		{sessionOpts{}, "nixos-config@main", "/home/user/repos/nixos-config/main", "opencode --agent build --continue"},
		// Fresh start
		{sessionOpts{agent: "build", fresh: true}, "nixos-config@main", "/home/user/repos/nixos-config/main", "opencode --agent build --continue"},
	}
	for _, tc := range cases {
		got := buildOpencodeCmd(tc.opts, tc.sessionName, tc.directory)
		if got != tc.want {
			t.Errorf("buildOpencodeCmd(%+v, %q, %q) = %q, want %q", tc.opts, tc.sessionName, tc.directory, got, tc.want)
		}
	}
}
