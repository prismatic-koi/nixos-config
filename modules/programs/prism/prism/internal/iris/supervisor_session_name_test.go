package iris

// supervisor_session_name_test.go — unit tests for GenerateSessionName.
//
// These tests pin down the new <repo>/<branch> session-name convention
// introduced in issue #1738. The previous "iris-<role>@<basename>" shape
// was a tmux-coexistence holdover that dropped repo context and so
// collided across repos sharing a branch name (e.g. `main` in
// `nixos-config` vs. `main` in `hass-config`). The new shape always
// carries the repo so two spawns on the same branch in different repos
// produce two distinct session names.

import (
	"path/filepath"
	"testing"
)

func TestGenerateSessionName_RepoSlashBranch(t *testing.T) {
	cases := []struct {
		worktree string
		want     string
	}{
		// Canonical bare+worktree layout: parent dir = repo, leaf = branch.
		{"/home/user/code/my-project/main", "my-project/main"},
		{"/home/user/code/hass-config/test", "hass-config/test"},
		{"/home/user/code/nixos-config/feature-x", "nixos-config/feature-x"},
		// Trailing slash should not change the split.
		{"/home/user/code/hass-config/main/", "hass-config/main"},
		// Two repos sharing a branch name produce distinct session names —
		// this is the regression the issue is fixing.
		{"/x/repo-a/main", "repo-a/main"},
		{"/x/repo-b/main", "repo-b/main"},
	}
	for _, tc := range cases {
		got := GenerateSessionName(tc.worktree)
		if got != tc.want {
			t.Errorf("GenerateSessionName(%q) = %q, want %q", tc.worktree, got, tc.want)
		}
	}
}

// TestGenerateSessionName_NoCrossRepoCollision is the explicit AC check:
// two worktrees on the same branch in different repos must produce
// distinct session names (the symptom that motivated #1738).
func TestGenerateSessionName_NoCrossRepoCollision(t *testing.T) {
	a := GenerateSessionName("/home/x/repo-a/main")
	b := GenerateSessionName("/home/x/repo-b/main")
	if a == b {
		t.Fatalf("expected distinct session names for different repos sharing a branch; both = %q", a)
	}
	if a != "repo-a/main" {
		t.Errorf("a = %q, want %q", a, "repo-a/main")
	}
	if b != "repo-b/main" {
		t.Errorf("b = %q, want %q", b, "repo-b/main")
	}
}

// TestGenerateSessionName_NoIrisPrefix verifies the historical
// "iris-<role>@" prefix has been removed (tmux-coexistence holdover).
func TestGenerateSessionName_NoIrisPrefix(t *testing.T) {
	got := GenerateSessionName("/home/user/code/my-project/main")
	if got == "" {
		t.Fatal("got empty session name")
	}
	for _, badSubstr := range []string{"iris-", "@", "worker", "coordinator"} {
		if containsSubstring(got, badSubstr) {
			t.Errorf("session name %q must not contain %q (drop tmux-coexistence prefix and role)", got, badSubstr)
		}
	}
}

// TestGenerateSessionName_SinglePathComponent exercises the AC edge case:
// a one-component path like `/foo` must not panic and must produce a
// graceful fallback.
func TestGenerateSessionName_SinglePathComponent(t *testing.T) {
	got := GenerateSessionName("/foo")
	// With only one component, the parent is "/" — the fallback maps that
	// to "session" so the returned name is still well-formed.
	want := "session/foo"
	if got != want {
		t.Errorf("GenerateSessionName(\"/foo\") = %q, want %q", got, want)
	}
}

// TestGenerateSessionName_EmptyAndDotPaths covers degenerate inputs.
// They must not panic and must produce a non-empty, slash-shaped name.
func TestGenerateSessionName_EmptyAndDotPaths(t *testing.T) {
	cases := []string{"", ".", "/"}
	for _, in := range cases {
		got := GenerateSessionName(in)
		if got == "" {
			t.Errorf("GenerateSessionName(%q) returned empty string", in)
		}
		if !containsSubstring(got, "/") {
			t.Errorf("GenerateSessionName(%q) = %q, want a name containing '/'", in, got)
		}
	}
}

// TestGenerateSessionName_RelativePath verifies relative paths are
// resolved against the cwd before splitting. The exact repo name will
// depend on the cwd, but the result must end with "/<basename>" and
// contain no panic.
func TestGenerateSessionName_RelativePath(t *testing.T) {
	got := GenerateSessionName("some-branch")
	// We don't assert the repo half (it's cwd-dependent), but we do
	// assert the branch half is preserved and the shape is repo/branch.
	if got == "" {
		t.Fatal("got empty session name for relative path")
	}
	if filepath.Base(got) != "some-branch" {
		t.Errorf("GenerateSessionName(\"some-branch\") = %q, want a name ending in /some-branch", got)
	}
}

// containsSubstring is a tiny strings.Contains shim local to this file
// so the test does not pull in the `strings` import for one call.
func containsSubstring(s, substr string) bool {
	if substr == "" {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
