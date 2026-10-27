// Package nonworktreeroot holds the one rule that decides whether a session is
// the root session of a non-worktree project. internal/authz and internal/db
// both use it; it sits below both so neither keeps a copy (issue #3214).
package nonworktreeroot

import (
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/sessionname"
)

// Is reports whether prism started sessionName as the root session of a
// non-worktree project. startRole and startDir are the start record: the
// agent role the start path gave the session and the directory it started in.
// See authz.IsNonWorktreeRoot for the rationale of each check.
func Is(sessionName, startRole, startDir string) bool {
	if sessionname.IsMeta(sessionName) || sessionname.IsDescendant(sessionName) || sessionname.HasBranch(sessionName) {
		return false
	}
	if sessionname.Repo(sessionName) == "" {
		return false
	}
	if startRole != "" || startDir == "" {
		return false
	}
	return !git.IsBareRepo(startDir) && git.BareRoot(startDir) == ""
}
