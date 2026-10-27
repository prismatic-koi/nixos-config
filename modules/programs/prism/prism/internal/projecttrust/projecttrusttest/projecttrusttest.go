// Package projecttrusttest builds a temporary project tree and prism config
// for tests of the project-trust decision (internal/projecttrust).
package projecttrusttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
)

// Layout is a temporary tree with one project location, one specific
// project outside every bare+worktree layout, and one directory outside every
// project (a stand-in for ~/.npm).
type Layout struct {
	Root      string
	Code      string // the project location in config.json
	Obsidian  string // the specific project in config.json
	Writable  string // a directory that is not a project root
	ConfigDir string // XDG_CONFIG_HOME
}

// New creates the tree. It writes config.json (project_locations: Code,
// project_specific: Obsidian) and project-trust.json (trusted_repos: trusted)
// under a temporary XDG_CONFIG_HOME, and points XDG_CONFIG_HOME at it with
// t.Setenv. PRISM_CONFIG_FILE is cleared.
func New(t *testing.T, trusted ...string) Layout {
	t.Helper()
	root := t.TempDir()
	l := Layout{
		Root:      root,
		Code:      filepath.Join(root, "code"),
		Obsidian:  filepath.Join(root, "documents", "obsidian"),
		Writable:  filepath.Join(root, "npm"),
		ConfigDir: filepath.Join(root, "config"),
	}
	for _, d := range []string{l.Code, l.Obsidian, l.Writable, filepath.Join(l.ConfigDir, "prism")} {
		mustMkdir(t, d)
	}
	writeJSON(t, filepath.Join(l.ConfigDir, "prism", "config.json"), map[string]any{
		"project_locations": []string{l.Code},
		"project_specific":  []string{l.Obsidian},
	})
	writeJSON(t, filepath.Join(l.ConfigDir, "prism", config.ProjectTrustFileName), config.ProjectTrustFile{TrustedRepos: trusted})
	t.Setenv("XDG_CONFIG_HOME", l.ConfigDir)
	t.Setenv("PRISM_CONFIG_FILE", "")
	return l
}

// Worktree creates a bare+worktree layout <parent>/<repo>/.bare with a
// worktree at <parent>/<repo>/<branch> and returns the worktree path. The
// .git file of the worktree points at <parent>/<repo>/.bare/worktrees/<name>,
// as `git worktree add` writes it. No git command runs.
func Worktree(t *testing.T, parent, repo, branch string) string {
	t.Helper()
	bare := filepath.Join(parent, repo)
	gitDir := filepath.Join(bare, ".bare", "worktrees", strings.ReplaceAll(branch, "/", "-"))
	wt := filepath.Join(bare, branch)
	mustMkdir(t, gitDir)
	mustMkdir(t, wt)
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	return wt
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
