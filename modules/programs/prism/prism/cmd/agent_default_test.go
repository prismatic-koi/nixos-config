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
		{sessionOpts{agent: "coordinator"}, "opencode --agent coordinator --continue"},
		{sessionOpts{agent: "build"}, "opencode --agent build --continue"},
		{sessionOpts{agent: "custom"}, "opencode --agent custom --continue"},
		// Safety-net fallback: empty agent still yields "build".
		{sessionOpts{}, "opencode --agent build --continue"},
		// Fresh start
		{sessionOpts{agent: "build", fresh: true}, "opencode --agent build"},
	}
	for _, tc := range cases {
		got := buildOpencodeCmd(tc.opts)
		if got != tc.want {
			t.Errorf("buildOpencodeCmd(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
}
