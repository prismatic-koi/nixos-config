package iris

// spawn_role.go — iris-side default-agent resolution.
//
// Mirrors session.DefaultAgent (internal/session/session.go) so that
// iris-managed sessions can resolve "coordinator" vs "worker" from a
// worktree path without depending on prism's tmux-session helpers. See
// daemon-mode-design.md §10.3 (parity checklist: "Spawn worker and
// coordinator sessions") and the D-10 acceptance criteria on issue #1641
// (the resolved-agent-from-branch behaviour the parity test asserts).
//
// The rule mirrors prism's: in a prism bare+worktree layout, the parent
// of the worktree contains a `.bare` marker. The worktree basename
// determines the role:
//
//   parent has .bare AND basename == "main"  → "coordinator"
//   parent has .bare AND basename ≠ "main"   → "worker"
//   parent does NOT have .bare               → "" (non-worktree path)
//
// When `explicit` is non-empty it is returned unchanged — explicit role
// always overrides the directory heuristic.

import (
	"path/filepath"

	"github.com/prismatic-koi/prism/internal/git"
)

// ResolveAgent returns the agent (role) name iris should use for the given
// worktree. If explicit is non-empty it is returned unchanged. Otherwise the
// parent directory is checked for the prism .bare marker; if present, basename
// == "main" → "coordinator" and any other basename → "worker". When the
// directory is not a prism worktree, "" is returned and the caller may apply
// its own fallback (cmd/iris/main.go falls back to "worker").
func ResolveAgent(worktree, explicit string) string {
	if explicit != "" {
		return explicit
	}
	parent := filepath.Dir(worktree)
	if git.IsBareRepo(parent) {
		if filepath.Base(worktree) == "main" {
			return "coordinator"
		}
		return "worker"
	}
	return ""
}
