package cmd

import (
	"path/filepath"
	"testing"
)

// resolveAgent mirrors the agent-defaulting logic in ensureAndSwitchSession so
// it can be tested independently of the tmux plumbing.
func resolveAgent(directory, explicitAgent string) string {
	if explicitAgent != "" {
		return explicitAgent
	}
	if filepath.Base(directory) == "main" {
		return "coordinator"
	}
	return "build"
}

func TestResolveAgent_DefaultsToCoordinatorForMain(t *testing.T) {
	got := resolveAgent("/home/user/repos/project/main", "")
	if got != "coordinator" {
		t.Errorf("resolveAgent(%q, %q) = %q, want %q", "/home/user/repos/project/main", "", got, "coordinator")
	}
}

func TestResolveAgent_DefaultsToBuildForNonMain(t *testing.T) {
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
			got := resolveAgent(dir, "")
			if got != "build" {
				t.Errorf("resolveAgent(%q, %q) = %q, want %q", dir, "", got, "build")
			}
		})
	}
}

func TestResolveAgent_ExplicitAgentOverridesDefault(t *testing.T) {
	cases := []struct {
		dir   string
		agent string
	}{
		{"/home/user/repos/project/main", "custom-agent"},
		{"/home/user/repos/project/feature", "custom-agent"},
		{"/home/user/repos/project/main", "build"},
	}
	for _, tc := range cases {
		got := resolveAgent(tc.dir, tc.agent)
		if got != tc.agent {
			t.Errorf("resolveAgent(%q, %q) = %q, want %q", tc.dir, tc.agent, got, tc.agent)
		}
	}
}

func TestBuildOpencodeCmd_UsesAgent(t *testing.T) {
	cases := []struct {
		opts sessionOpts
		want string
	}{
		{sessionOpts{agent: "coordinator"}, "opencode --agent coordinator"},
		{sessionOpts{agent: "build"}, "opencode --agent build"},
		{sessionOpts{agent: "custom"}, "opencode --agent custom"},
		// Safety-net fallback: empty agent still yields "build".
		{sessionOpts{}, "opencode --agent build"},
	}
	for _, tc := range cases {
		got := buildOpencodeCmd(tc.opts)
		if got != tc.want {
			t.Errorf("buildOpencodeCmd(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
}
