// Package git provides helpers for querying git worktree state.
package git

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// DiffStat holds a summary of uncommitted changes in a worktree.
type DiffStat struct {
	Files      int
	Insertions int
	Deletions  int
}

// String returns a compact representation, e.g. "3 files +42 -7".
// Returns an empty string if there are no changes.
func (d DiffStat) String() string {
	if d.Files == 0 {
		return ""
	}
	fileWord := "files"
	if d.Files == 1 {
		fileWord = "file"
	}
	return fmt.Sprintf("%d %s +%d -%d", d.Files, fileWord, d.Insertions, d.Deletions)
}

// Stat returns a DiffStat for the given directory, combining unstaged and
// staged changes relative to HEAD. Returns a zero DiffStat on error or if
// the directory is not a git repo.
func Stat(dir string) DiffStat {
	if dir == "" {
		return DiffStat{}
	}

	// --numstat gives machine-readable "added\tdeleted\tfile" lines.
	// Collect both unstaged and staged, deduplicating by filename.
	seen := map[string]bool{}
	var total DiffStat

	for _, args := range [][]string{
		{"-C", dir, "diff", "--numstat", "HEAD"},
		{"-C", dir, "diff", "--numstat", "--cached"},
	} {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) < 3 {
				continue
			}
			filename := parts[2]
			ins, _ := strconv.Atoi(parts[0])
			del, _ := strconv.Atoi(parts[1])
			if !seen[filename] {
				seen[filename] = true
				total.Files++
				total.Insertions += ins
				total.Deletions += del
			}
		}
	}
	return total
}
