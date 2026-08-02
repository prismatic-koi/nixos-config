// Package config — agent_tool_roles.go
//
// Role-aware restriction of the pi builtin tool surface (issue #2531).
//
// pi registers four builtin tools by default: read, bash, edit, write (grep,
// find, and ls are off by default and are never enabled by prism). Every
// role currently gets all four. Review agents are read-only reviewers — the
// review-*.md role files instruct them never to call write or edit, and the
// PR they review lives in a worktree they are told to treat as read-only
// (internal/review/review.go WorktreePath doc comment). No review role file
// contains a legitimate write or edit call. Excluding both tools from the
// review roles' schema removes dead schema weight and turns an
// instruction-level convention into an enforced restriction (a reviewer that
// tries to write now gets pi's "Unknown tool" error instead of silently
// having the capability).
//
// This mirrors the shape of agent_env_roles.go: a literal map keyed by the
// five canonical review role names (internal/review Agents()), applied
// upstream of the isolator so bwrap, sandbox-exec, and host-mode dispatch
// all resolve the same exclusion list for the same role.
package config

// reviewToolExclusions are the pi builtin tool names excluded from the five
// review roles via pi's --exclude-tools flag. Both tools are write-capable;
// neither is used by any review-*.md role file.
var reviewToolExclusions = []string{"write", "edit"}

// roleToolExclusions maps a session role to the pi builtin tool names that
// role must not receive.
//
// `investigate` is deliberately absent, matching the reviewRoleEnvExclusions
// precedent (issue #2533): it is read-only in intent but not in the same way
// review agents are — an investigator has no PR-review-specific instruction
// against write/edit, so it keeps the full builtin set like `coordinator`
// and `worker`.
var roleToolExclusions = map[string][]string{
	"review-goal":     reviewToolExclusions,
	"review-code":     reviewToolExclusions,
	"review-context":  reviewToolExclusions,
	"review-qa":       reviewToolExclusions,
	"review-security": reviewToolExclusions,
}

// ExcludedToolsForRole returns the pi builtin tool names that must be
// excluded (via --exclude-tools) for the given session role. A role outside
// the known set (including "" and "coordinator") returns nil, so an
// unrecognised or coordinator role receives the full builtin tool set
// unchanged.
func ExcludedToolsForRole(role string) []string {
	excl, ok := roleToolExclusions[role]
	if !ok {
		return nil
	}
	// Return a copy so callers cannot mutate the shared slice.
	out := make([]string, len(excl))
	copy(out, excl)
	return out
}
