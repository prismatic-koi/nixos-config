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

// TestBuildOpencodeCmd_UsesAgent verifies that BuildOpencodeCmd generates the
// correct pi invocation for various Opts combinations.
func TestBuildOpencodeCmd_UsesAgent(t *testing.T) {
	cases := []struct {
		opts session.Opts
		want string
	}{
		// Basic agent flag.
		{session.Opts{Agent: "coordinator"}, "pi --agent coordinator"},
		{session.Opts{Agent: "worker"}, "pi --agent worker"},
		// Empty agent — no --agent flag.
		{session.Opts{}, "pi"},
		// Prompt with no special characters.
		{session.Opts{Agent: "worker", Prompt: "fix the login bug"}, "pi --agent worker --prompt 'fix the login bug'"},
		// Prompt containing a single quote — exercises shellQuote escaping.
		{session.Opts{Agent: "worker", Prompt: "it's broken"}, "pi --agent worker --prompt 'it'\\''s broken'"},
		// Prompt with shell metacharacters that are safe inside single quotes.
		{session.Opts{Agent: "worker", Prompt: "run `make test` and fix $ERRORS"}, "pi --agent worker --prompt 'run `make test` and fix $ERRORS'"},
		// SessionName set — PRISM_SESSION_NAME is prepended.
		{session.Opts{Agent: "worker", SessionName: "myrepo@main"}, "PRISM_SESSION_NAME='myrepo@main' pi --agent worker"},
		// SessionName with special characters.
		{session.Opts{Agent: "worker", SessionName: "nixos_config@feature--my-branch"}, "PRISM_SESSION_NAME='nixos_config@feature--my-branch' pi --agent worker"},
		// SessionName + Prompt.
		{session.Opts{Agent: "worker", SessionName: "myrepo@main", Prompt: "do the thing"}, "PRISM_SESSION_NAME='myrepo@main' pi --agent worker --prompt 'do the thing'"},
		// No SessionName — no PRISM_SESSION_NAME.
		{session.Opts{Agent: "worker", SessionName: ""}, "pi --agent worker"},
	}
	for _, tc := range cases {
		got := session.BuildOpencodeCmd(tc.opts)
		if got != tc.want {
			t.Errorf("BuildOpencodeCmd(%+v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
}

