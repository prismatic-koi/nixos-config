package git

import (
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/config"
)

// WorktreeRootIn returns listed, a path from `git worktree list` of the bare
// repo in root, as a path under root, and reports whether it is the root of a
// worktree of that repo. git can list a resolved path (for example
// /private/var on macOS), so the compare resolves symlinks.
//
// A session can make `git worktree list` name a path that it chose: a
// symlink in its worktree, or a path in its own git dir. So a start route
// that gets a path from that list, or a path in a bare+worktree layout,
// calls WorktreeRootIn and starts the session at the result. The result has
// no symlink below root, so a session cannot change the dir of the start
// after the check.
//
// It refuses a path in <root>/.bare. A session can write its own git dir
// there, and no worktree of prism is in .bare.
func WorktreeRootIn(root, listed string) (string, bool) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	realListed, err := filepath.EvalSymlinks(listed)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(realRoot, realListed)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	// The compare by file identity also refuses another letter case of .bare
	// on a case-insensitive volume.
	if first, _, _ := strings.Cut(rel, string(filepath.Separator)); first == ".bare" ||
		config.SameDir(filepath.Join(realRoot, first), filepath.Join(realRoot, ".bare")) {
		return "", false
	}
	wt := filepath.Join(root, rel)
	if OuterBareRoot(wt) != root {
		return "", false
	}
	r, err := OpenDir(wt)
	if err != nil || r.WorkTree() != wt {
		return "", false
	}
	return wt, true
}
