// Package reposkills gives the skill paths that prism passes to pi with
// --skill for the repo of a session (issue #3463).
//
// pi loads a --skill path also with --no-approve, because that path is not a
// project resource. So a repo can have skills that only its sessions see, and
// the project-trust decision (internal/projecttrust) does not change.
//
// The paths come only from repo-skills.json (config.LoadRepoSkills), never
// from the worktree or the branch. The repo comes from
// projecttrust.Policy.SessionProject: the session name, the start directory,
// and the project roots in config.json. No sandbox can read or write
// repo-skills.json or config.json. A review agent, an investigator, and a
// PR-branch session of a repo get the skills of that repo: the skill content
// is in the nix store, not on the branch.
//
// Each path must be in the nix store, and must exist. If a path is not, it is
// dropped with a warning, so a value in the file cannot point pi at a
// worktree.
package reposkills

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/projecttrust"
)

// storeDir is the nix store. Tests change it with SetStoreDirForTest.
var storeDir = "/nix/store"

// SetStoreDirForTest makes dir the nix store for Paths, and returns a
// function that restores the old value. Only for use in tests: the CI runner
// of go-tests has no /nix.
func SetStoreDirForTest(dir string) (restore func()) {
	old := storeDir
	storeDir = dir
	return func() { storeDir = old }
}

// Paths returns the --skill paths for the session sessionName that starts in
// dir. cfg gives the project roots. Paths calls warn once for each problem:
// a file that it cannot read or parse, and each path that it drops. It returns
// nil when the file has no entry for the repo, or when SessionProject gives no
// repo.
func Paths(sessionName, dir string, cfg config.Config, warn func(string)) []string {
	byRepo, err := config.LoadRepoSkills()
	if err != nil {
		warn(fmt.Sprintf("%v — pi starts with no --skill flag", err))
		return nil
	}
	if len(byRepo) == 0 {
		return nil
	}
	policy := projecttrust.Policy{ProjectLocations: cfg.ProjectLocations, ProjectSpecific: cfg.ProjectSpecific}
	root, ok := policy.SessionProject(sessionName, dir)
	if !ok {
		return nil
	}
	var out []string
	for _, p := range byRepo[filepath.Base(root)] {
		if err := checkStorePath(p); err != nil {
			warn(fmt.Sprintf("repo skills: %v — pi starts without this skill", err))
			continue
		}
		out = append(out, p)
	}
	return out
}

// checkStorePath returns an error unless p is a clean absolute path in the
// nix store that exists and resolves to a path in the nix store. A store path
// can hold a symlink to a path outside the store, so the resolved path must
// be in the store too.
func checkStorePath(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || !inStore(p) {
		return fmt.Errorf("skill path %q is not a path in %s", p, storeDir)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("skill path %s: %w", p, err)
	}
	if !inStore(resolved) {
		return fmt.Errorf("skill path %s resolves to %s, which is not in %s", p, resolved, storeDir)
	}
	return nil
}

func inStore(p string) bool {
	return strings.HasPrefix(p, storeDir+string(filepath.Separator))
}
