// Package sessionname holds the grammar of a prism session name.
//
// A session name has one of these shapes:
//
//	<repo>                              a non-worktree session (host mode)
//	<repo>@<branch>                     a worktree session
//	<repo>@main                         the coordinator of a worktree repo
//	<parent>~review-<n>-<role>          a review agent of <parent>
//	<parent>~investigate-<slug>         an investigator of <parent>
//
// This package is pure. It reads no database and it imports nothing but
// `strings`, so every layer — internal/db, internal/authz, internal/session,
// internal/review — can share one answer. A predicate that must also consult
// the database (for example authz.IsRootSession) builds on these functions
// rather than restating the grammar.
package sessionname

import "strings"

// Separators of the session-name grammar.
const (
	// BranchSeparator divides the repo from the branch: `<repo>@<branch>`.
	BranchSeparator = "@"

	// DescendantSeparator divides a parent session from a session it
	// spawned: `<parent>~review-1-review-goal`.
	DescendantSeparator = "~"

	// CoordinatorSuffix is the name shape of a worktree repo's coordinator.
	CoordinatorSuffix = BranchSeparator + "main"
)

// Meta-session names. A meta-session is a prism-internal tmux session, not an
// agent. It must never appear in agent_status and must never be classified as
// a coordinator or as a root session.
const (
	Scratchpad = "scratchpad"
	Dashboard  = "prism-dashboard"
)

// MetaNames lists every meta-session name. SQL that has to exclude them binds
// this slice rather than repeating the literals.
func MetaNames() []string { return []string{Scratchpad, Dashboard} }

// IsMeta reports whether name is a prism-internal meta-session.
func IsMeta(name string) bool {
	return name == Scratchpad || name == Dashboard
}

// IsDescendant reports whether name is a session that another session
// spawned — a review agent or an investigator.
//
// A descendant is never a coordinator and never a root session, whatever its
// agent_status row says. The check is on the name alone and so cannot be
// defeated by a wrong root_agent_name value.
//
// The "~" is looked for in the BRANCH part only, not over the whole name. A
// repo directory may itself hold "~", so `weird~repo@main` is the coordinator
// of repo `weird~repo`, not a descendant of anything. Testing the whole name
// would demote it and take its merge queue away — a regression, not a
// tightening. Repo makes the same distinction, and for the same reason.
//
// When the name has no branch, the whole name is searched, because a
// descendant of a non-worktree parent is `<parent>~<label>` with no "@" in it.
// One ambiguity is inherent to the grammar and is not resolvable from the name
// alone: a bare repo directory whose own name holds "~" reads as a descendant.
// That resolves toward refusing privilege, which is the safe direction.
func IsDescendant(name string) bool {
	return strings.Contains(branchPart(name), DescendantSeparator)
}

// branchPart returns the part of a session name in which a "~" marks a
// descendant: everything after the first "@", or the whole name when the name
// carries no branch.
func branchPart(name string) string {
	if i := strings.Index(name, BranchSeparator); i >= 0 {
		return name[i+len(BranchSeparator):]
	}
	return name
}

// Repo returns the repo that a session name belongs to.
//
// The repo is the prefix before the first "@". When the name has no "@" — a
// non-worktree session — the repo is the prefix before the first "~". A name
// with neither separator is its own repo.
//
//	nixos-config@main                        → nixos-config
//	nixos-config@feat~review-1-review-goal   → nixos-config
//	obsidian                                 → obsidian
//	obsidian~investigate-v2                  → obsidian
//
// "@" is tested first on purpose. A repo directory may itself contain "~", so
// `weird~name@main` must resolve to `weird~name`, not to `weird`. Cutting at
// "~" first would shorten the repo and could make a cross-repo target look
// like a same-repo one.
//
// This function reads the name only. It is the fallback rule; the
// authoritative repo for a live session is the agent_status.repo column, which
// authz.RepoFromSession reads first.
func Repo(name string) string {
	if i := strings.Index(name, BranchSeparator); i >= 0 {
		return name[:i]
	}
	if i := strings.Index(name, DescendantSeparator); i >= 0 {
		return name[:i]
	}
	return name
}

// HasBranch reports whether name carries a "@<branch>" part, which means the
// session runs in a git worktree.
func HasBranch(name string) bool {
	return strings.Contains(name, BranchSeparator)
}

// HasCoordinatorSuffix reports whether name ends with "@main".
//
// This is the name heuristic that authz.IsCoordinatorSession applies, and it
// is the designed defence against a wrong root_agent_name value in the
// database. It is structurally unavailable to a name with no branch: for a
// non-worktree session, a wrong root_agent_name value cannot be caught by this
// heuristic.
func HasCoordinatorSuffix(name string) bool {
	return strings.HasSuffix(name, CoordinatorSuffix)
}
