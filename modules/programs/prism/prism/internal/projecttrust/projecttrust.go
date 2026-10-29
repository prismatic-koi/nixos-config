// Package projecttrust decides whether prism starts pi with --approve or with
// --no-approve (issue #3216).
//
// With --approve, pi loads the project resources of its working directory:
// .pi/extensions, .pi/skills, .pi/settings.json, .pi/mcp.json, .pi/SYSTEM.md,
// and .agents/skills in the directory and its ancestors. An extension runs
// with the rights of the session. A command-line flag wins over every other
// trust source of pi, so trust.json, a project_trust extension handler, and
// defaultProjectTrust do not change the decision.
//
// The decision runs on the host, in the process that builds the pi command
// line. The repo identity comes from the session name that prism gave the
// session and from the project roots in config.json. The repo list comes from
// project-trust.json. No sandbox can read or write either file. The start
// directory only narrows the decision: it must be a worktree root, or the
// project directory, of that same project root. So a name alone or a
// directory alone cannot give --approve, and a worktree value in agent_status
// cannot point the decision at another repo.
//
// prism cannot know who wrote the branch of an existing PR. So a session that
// `prism pr` or `prism spawn --pr` started, each of its descendants, and each
// session in its worktree get --no-approve, also in a trusted repo (issue
// #3287). The record of such a session is in pr_branch_sessions in prism.db.
// Only `prism pr` and `prism spawn` write it, on the host, and no sandbox can
// reach prism.db.
//
// SessionProject is the repo identity part of the decision. Another host
// decision that must not use a value that a sandbox can write uses it too:
// the grant of the Notion store (issue #3319).
package projecttrust

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/sessionname"
)

// Policy holds the inputs of the decision that do not depend on the session.
type Policy struct {
	// TrustedRepos holds repo directory names (project-trust.json).
	TrustedRepos []string
	// ProjectLocations and ProjectSpecific are the project roots of prism
	// (config.json). Each directory in a location is a project root, and each
	// specific path is a project root. A leading "~/" is expanded.
	ProjectLocations []string
	ProjectSpecific  []string
	// PRBranches reads the PR-branch records. Approve calls it only when
	// every other condition holds, so a session of an untrusted repo opens no
	// database. A nil value or an error gives false.
	PRBranches func() ([]PRBranch, error)
}

// PRBranch is a root session that prism started on the branch of an existing
// PR, and the worktree that it started in.
type PRBranch struct {
	SessionName string
	Worktree    string
}

// Load reads the policy from project-trust.json and config.json, and takes
// prBranches as the reader of the PR-branch records. If project-trust.json
// cannot be read, the policy trusts no repo.
func Load(prBranches func() ([]PRBranch, error)) Policy {
	repos, err := config.LoadProjectTrustedRepos()
	if err != nil {
		proglog.Warnf("[prism] %v — every pi starts with --no-approve\n", err)
		repos = nil
	}
	cfg := config.LoadFresh()
	return Policy{
		TrustedRepos:     repos,
		ProjectLocations: cfg.ProjectLocations,
		ProjectSpecific:  cfg.ProjectSpecific,
		PRBranches:       prBranches,
	}
}

// Approve reports whether pi that prism starts for sessionName in dir gets
// --approve. All of these must be true:
//
//   - SessionProject(sessionName, dir) gives a project root.
//   - The basename of that project root is in TrustedRepos.
//   - No PR-branch record names the root session of sessionName
//     (sessionname.Root), and no record names dir as its worktree.
//
// Any other case gives false.
func (p Policy) Approve(sessionName, dir string) bool {
	if len(p.TrustedRepos) == 0 {
		return false
	}
	root, ok := p.SessionProject(sessionName, dir)
	if !ok || !slices.Contains(p.TrustedRepos, filepath.Base(root)) {
		return false
	}
	return !p.onPRBranch(sessionName, filepath.Clean(dir))
}

// SessionProject returns the project root of the repo that sessionName
// names, and reports whether dir is a start directory of that repo. All of
// these must be true:
//
//   - dir is a worktree root of a bare+worktree layout, and the session name
//     has a branch ("<repo>@<branch>", or a descendant of one). Or dir is a
//     project directory outside every bare+worktree layout, and the session
//     name has no branch ("obsidian", "obsidian~investigate-x").
//   - The project root (the directory that holds .bare, or dir itself) is a
//     prism project root in config.json.
//   - The repo part of the session name matches the basename of the project
//     root.
//
// Any other case gives false, an empty or relative dir and a meta-session
// name included. The name alone and the directory alone cannot give true: a
// project root under a writable directory such as ~/.npm is not a prism
// project root. SessionProject reads no TrustedRepos and no PR-branch
// record.
func (p Policy) SessionProject(sessionName, dir string) (string, bool) {
	if !filepath.IsAbs(dir) || sessionname.IsMeta(sessionName) {
		return "", false
	}
	dir = filepath.Clean(dir)
	repo := sessionname.Repo(sessionName)
	if repo == "" {
		return "", false
	}
	root, ok := projectRoot(sessionName, dir)
	if !ok || !p.isProjectRoot(root) {
		return "", false
	}
	if sessionname.RepoComponent(filepath.Base(root)) != repo {
		return "", false
	}
	return root, true
}

// onPRBranch reports whether a PR-branch record covers sessionName or dir. It
// fails closed: a nil reader or a read error gives true.
//
// The worktree match covers a session in the PR worktree under another name.
// session.NameFor takes the branch from the HEAD of the worktree, so after a
// `git checkout -b` in the worktree, `prism switch` names the session after
// the new branch.
func (p Policy) onPRBranch(sessionName, dir string) bool {
	if p.PRBranches == nil {
		return true
	}
	records, err := p.PRBranches()
	if err != nil {
		proglog.Warnf("[prism] read PR-branch sessions: %v — pi starts with --no-approve\n", err)
		return true
	}
	root := sessionname.Root(sessionName)
	for _, r := range records {
		if r.SessionName == root || filepath.Clean(r.Worktree) == dir || config.SameDir(dir, r.Worktree) {
			return true
		}
	}
	return false
}

// projectRoot returns the project root of dir and reports whether dir has the
// shape that the session name claims.
func projectRoot(sessionName, dir string) (string, bool) {
	// The directory that holds .bare is the project root of a layout, not a
	// worktree. git.BareRoot starts at the parent, so check it here.
	if git.IsBareRepo(dir) {
		return "", false
	}
	if bare := git.BareRoot(dir); bare != "" {
		if !sessionname.HasBranch(sessionName) || !isWorktreeOf(dir, bare) {
			return "", false
		}
		return bare, true
	}
	if sessionname.HasBranch(sessionName) {
		return "", false
	}
	return dir, true
}

// isWorktreeOf reports whether dir is the root of a worktree of the bare repo
// in bare/.bare. A subdirectory of a worktree has no .git file, so it fails.
func isWorktreeOf(dir, bare string) bool {
	gitDir, err := git.ResolveWorktreeGitDir(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Join(bare, ".bare", "worktrees"), gitDir)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (p Policy) isProjectRoot(root string) bool {
	return config.IsProjectRoot(p.ProjectLocations, p.ProjectSpecific, root)
}
