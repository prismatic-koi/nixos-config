package sidecar

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sessionname"
)

// errNoSwitchTarget is returned when no dir in a prism project root has the
// session name of a /switch request.
var errNoSwitchTarget = errors.New("no dir in a prism project root has this session name")

// switchPathForSession returns the dir that /switch gives to `prism switch
// --path` for sessionName (issue #3330).
//
// It reads no agent_status row. The /event proxy writes the worktree column
// of the row of its own session, and a value that a sandboxed session can
// set must not choose the dir where the host runs prism switch and git. The
// project root comes from config.json, which no sandbox can read or write.
// The worktree comes from the worktree list of the bare repo of that root,
// through host git. A session can change the gitdir file of its own entry in
// that list, so each listed path must also be a worktree root in the project
// root: OpenDir must give it as the work tree.
//
// The result is the dir that session.NameFor names sessionName:
//
//   - "<repo>@<branch>": the worktree of a bare+worktree project root named
//     <repo>, whose HEAD gives <branch>.
//   - "<name>": a project root named <name> that has no .bare.
//
// A descendant (a review agent, an investigator) runs in the dir of its root
// session, so it gets that dir.
func switchPathForSession(sessionName string) (string, error) {
	name := sessionname.Root(sessionName)
	if sessionname.IsMeta(name) {
		return "", fmt.Errorf("session %q: %w", sessionName, errNoSwitchTarget)
	}
	cfg := config.LoadFresh()
	var matches []string
	for _, root := range projectRootsNamed(cfg, sessionname.Repo(name)) {
		if !git.IsBareRepo(root) {
			if !sessionname.HasBranch(name) {
				if n, err := session.NameFor(root, ""); err == nil && n == name {
					matches = append(matches, root)
				}
			}
			continue
		}
		if !sessionname.HasBranch(name) {
			continue
		}
		for _, listed := range git.Worktrees(root) {
			wt, ok := worktreeRootIn(root, listed)
			if !ok {
				continue
			}
			if n, err := session.NameFor(wt, root); err == nil && n == name {
				matches = appendDir(matches, wt)
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("session %q: %w", sessionName, errNoSwitchTarget)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("session %q: %d dirs in the prism project roots have this session name (%s), so prism does not choose one", sessionName, len(matches), strings.Join(matches, ", "))
	}
}

// projectRootsNamed returns each prism project root of cfg whose session
// name part (sessionname.RepoComponent of its basename) is repo: a dir in a
// project location, or a project_specific path. A root is listed one time,
// also when two entries name it.
func projectRootsNamed(cfg config.Config, repo string) []string {
	var roots []string
	add := func(root string) {
		for _, r := range roots {
			if config.SameDir(r, root) {
				return
			}
		}
		roots = append(roots, root)
	}
	for _, loc := range cfg.ProjectLocations {
		dir := config.ExpandHomePath(loc)
		if !filepath.IsAbs(dir) {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if sessionname.RepoComponent(e.Name()) != repo {
				continue
			}
			if root := filepath.Join(dir, e.Name()); isDir(root) {
				add(root)
			}
		}
	}
	for _, spec := range cfg.ProjectSpecific {
		root := filepath.Clean(config.ExpandHomePath(spec))
		if filepath.IsAbs(root) && sessionname.RepoComponent(filepath.Base(root)) == repo && isDir(root) {
			add(root)
		}
	}
	return roots
}

// appendDir appends dir to dirs unless dirs already holds that dir. `git
// worktree list` lists a dir two times when a session points the gitdir file
// of its own entry at another worktree.
func appendDir(dirs []string, dir string) []string {
	for _, d := range dirs {
		if d == dir || config.SameDir(d, dir) {
			return dirs
		}
	}
	return append(dirs, dir)
}

// worktreeRootIn returns listed, a path from `git worktree list` of the bare
// repo in root, as a path under root, and reports whether it is the root of a
// worktree of that repo. git can list a resolved path (for example
// /private/var on macOS), so the compare resolves symlinks.
//
// It refuses a path in <root>/.bare. A session can write its own git dir
// there, and no worktree of prism is in .bare.
func worktreeRootIn(root, listed string) (string, bool) {
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
	if git.OuterBareRoot(wt) != root {
		return "", false
	}
	r, err := git.OpenDir(wt)
	if err != nil || r.WorkTree() != wt {
		return "", false
	}
	return wt, true
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
