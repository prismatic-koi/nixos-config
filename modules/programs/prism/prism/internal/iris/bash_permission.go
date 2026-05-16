package iris

// bash_permission.go — role-keyed bash command permission gate (D-10 parity).
//
// Background:
//
// Prism's coordinator and worker roles have different bash permission
// surfaces:
//
//   - Coordinator: `prism merge <pr>` is allowed — coordinators arbitrate
//     merge order and run the merge queue.
//   - Worker: `prism merge` is denied — workers must not enqueue merges
//     directly; merge decisions flow through the coordinator.
//
// In prism this distinction is enforced by two layers:
//
//   1. Claude Code's permissions config (~/.claude/settings.json) per
//      session, populated from modules/programs/prism/claude-code.nix;
//   2. The `prism merge` CLI's own AgentRole check (cmd/merge.go) which
//      refuses to run from non-coordinator sessions.
//
// iris currently has no settings.json-equivalent per-session config and
// must not invoke any prism code path. The parity test on issue #1641
// asserts the observable behaviour:
//
//   coordinator: `bash` tool call invoking `prism merge --help` permitted
//   worker:      `bash` tool call invoking `prism merge --help` denied
//
// CheckBashPermission encodes that contract entirely inside iris. The check
// runs in the iris tool dispatcher before the bash subprocess is started.
// When the command is denied, the dispatcher returns a tool_exec_result
// with success=false / is_error=true and a descriptive message — never
// invoking any prism binary.
//
// Scope is intentionally narrow: only `prism merge ...` is currently gated.
// The parity gate exercises the role-keyed permission mechanism; the full
// catalogue of role-keyed commands is a future concern tracked separately
// (this is not the prism-extension deny list, which is a tool-level filter
// inside pi).

import (
	"strings"
)

// CheckBashPermission reports whether the bash subprocess should be allowed
// to run a given command, given the session's agent role.
//
// Returns (allowed, reason). When allowed is false, reason is a
// human-readable string suitable for surfacing in the tool_exec_result
// output field. When allowed is true, reason is "".
//
// The policy is:
//
//	role=coordinator    → command containing a real `prism merge` invocation is allowed.
//	role!=coordinator   → `prism merge` is denied.
//	role=investigate    → mutation commands (gh mutate, prism spawn/review/merge,
//	                      git push/commit/add/rebase/reset, iris spawn/review/merge)
//	                      are denied. Other commands are allowed.
//	any role            → all other bash commands are allowed.
//
// Detection of restricted invocations is intentionally simple: the command
// string is tokenised on whitespace and matched against fixed binary +
// subcommand pairs. The check is whitespace-tolerant but does not attempt
// to strip quoted regions. False positives on benign strings like
// `echo "prism merge"` are accepted as the safe-by-default behaviour —
// the test surface exercised by D-10 uses unambiguous invocations.
func CheckBashPermission(role, command string) (bool, string) {
	// Investigator deny-list. Mirrors the prism investigator agent's deny
	// list (modules/programs/prism/agents/investigate.md). The investigator
	// is read-only: it must not spawn or steer other agents, mutate GitHub,
	// or mutate git history.
	if role == "investigate" {
		if denied, name := mentionsInvestigatorDenied(command); denied {
			return false, "iris: bash permission denied: investigator is read-only and may not run `" + name + "` (see modules/programs/prism/agents/investigate.md for the full deny list)"
		}
		return true, ""
	}
	if !mentionsPrismMerge(command) {
		return true, ""
	}
	if role == "coordinator" {
		return true, ""
	}
	return false, "iris: bash permission denied: `prism merge` is restricted to coordinator sessions (role=" + role + ")"
}

// mentionsPrismMerge reports whether command contains a `prism merge`
// invocation. Tokenises on whitespace; matches when consecutive non-empty
// tokens are exactly "prism" and "merge".
func mentionsPrismMerge(command string) bool {
	return mentionsCommandPair(command, "prism", "merge")
}

// investigatorDeniedPairs is the canonical investigator bash deny list.
// It mirrors `modules/programs/prism/agents/investigate.md` byte-for-byte:
// no GitHub mutations, no agent spawning, no merge enqueueing, no git
// history mutation. The list is intentionally restricted to invocations
// that change state — read-only operations (rg, grep, git log, gh issue
// view, etc.) are all allowed.
var investigatorDeniedPairs = [][2]string{
	// GitHub mutations.
	{"gh", "issue"},      // gh issue create/edit/close/comment
	{"gh", "pr"},         // gh pr create/edit/merge/close/review/comment
	// Note: gh issue view / gh pr view are read-only but share the
	// `gh issue` / `gh pr` prefix. We refine the match below so that
	// only mutating subcommands trip the deny list.

	// Prism agent control.
	{"prism", "spawn"},
	{"prism", "review"},
	{"prism", "merge"},
	{"prism", "merges"},

	// Iris agent control — mirrors the prism list for the iris world so
	// an investigator cannot spawn iris workers either.
	{"iris", "spawn"},
	{"iris", "review"},
	{"iris", "merge"},
	{"iris", "merges"},
	{"iris", "investigate"},

	// Git history mutation. `git push` is the only blanket-denied verb;
	// the rest are matched on the exact subcommand.
	{"git", "push"},
	{"git", "commit"},
	{"git", "add"},
	{"git", "rebase"},
	{"git", "reset"},
}

// investigatorReadOnlyGhSubcommands lists the read-only `gh issue` /
// `gh pr` subcommands that ARE allowed even though their `gh issue` /
// `gh pr` prefix matches the deny list above. Mirrors the
// "Allowed actions" section of investigate.md.
var investigatorReadOnlyGhSubcommands = map[string]bool{
	"view": true,
	"list": true,
	"diff": true,
}

// mentionsInvestigatorDenied reports whether command invokes any item in
// the investigator deny list. Returns (true, "<binary> <subcommand>") on
// a match, (false, "") otherwise. The match is exact on two consecutive
// non-empty whitespace-separated tokens.
//
// `gh issue` / `gh pr` are refined: a third token of `view`, `list`, or
// `diff` flips the match back to allowed (these are read-only).
func mentionsInvestigatorDenied(command string) (bool, string) {
	fields := strings.Fields(command)
	for i := 0; i+1 < len(fields); i++ {
		for _, pair := range investigatorDeniedPairs {
			if fields[i] != pair[0] || fields[i+1] != pair[1] {
				continue
			}
			// Refinement for `gh issue` / `gh pr`: allow read-only
			// subcommands.
			if pair[0] == "gh" && (pair[1] == "issue" || pair[1] == "pr") && i+2 < len(fields) {
				if investigatorReadOnlyGhSubcommands[fields[i+2]] {
					continue
				}
			}
			return true, pair[0] + " " + pair[1]
		}
	}
	return false, ""
}

// mentionsCommandPair reports whether command contains the two-token
// sequence (a, b) as consecutive non-empty whitespace-separated tokens.
func mentionsCommandPair(command, a, b string) bool {
	fields := strings.Fields(command)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == a && fields[i+1] == b {
			return true
		}
	}
	return false
}
