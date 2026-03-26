// Package git provides helpers for querying git worktree state.
package git

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// ChangedFiles returns the list of files changed (unstaged + staged) relative
// to HEAD in the given directory. Returns nil if the directory is not a git repo
// or has no changes.
func ChangedFiles(dir string) []string {
	if dir == "" {
		return nil
	}

	seen := map[string]bool{}
	var files []string

	for _, args := range [][]string{
		{"diff", "--name-only", "HEAD"},
		{"diff", "--name-only", "--cached"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f = strings.TrimSpace(f)
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			files = append(files, filepath.Base(f))
		}
	}
	return files
}
