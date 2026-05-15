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
//	role=coordinator → command containing a real `prism merge` invocation is allowed.
//	role!=coordinator (worker, review-*, "" etc.) → `prism merge` is denied.
//	any role         → all other bash commands are allowed.
//
// Detection of `prism merge` is intentionally simple: the command string
// is searched for the token sequence "prism" followed (after whitespace)
// by "merge". The check is whitespace-tolerant but does not attempt to
// strip quoted regions. False positives on benign strings like
// `echo "prism merge"` are accepted as the safe-by-default behaviour —
// the test surface exercised by D-10 uses unambiguous invocations.
func CheckBashPermission(role, command string) (bool, string) {
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
	fields := strings.Fields(command)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "prism" && fields[i+1] == "merge" {
			return true
		}
	}
	return false
}
