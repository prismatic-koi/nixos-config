package cmd

// agent_run_profile.go — issue #2092 fix, refactored in #2097.
//
// `prism spawn --profile <X>` and `prism spawn --abtest <A> <B>` carry the
// per-spawn profile name through to `spawn_inputs.profile_name` (#2090's
// audit row). Before #2092 the agent-run path discarded that information:
// populatePIConfig called config.ResolveActiveProfile(pf, "") and the active
// profile silently won over the spawn-time choice — so both legs of an
// --abtest pair ran on the same slot.
//
// spawnTimeProfileForSession closes the loop: it reads the canonical
// post-#2090 source ("what profile did the user spawn this session with")
// off the audit row and returns the value, which populatePIConfig then
// passes as the `flagValue` argument to ResolveActiveProfile (treated by
// that function as the highest-precedence source).
//
// In #2097 the SQL-shaped half of this helper was promoted to
// `internal/profile.SpawnTimeForSession` so the same lookup can be
// reused by the child-spawn surfaces (`internal/review` and
// `cmd/investigate.go`) without a `cmd`-package import cycle. This
// thin wrapper is kept so the cmd-side opens its own short-lived DB
// handle (populatePIConfig does not get a *db.DB threaded through).
//
// The lookup is best-effort: any error — DB unreachable, sessions row
// missing, spawn_inputs row missing, profile_name NULL — is collapsed to
// the empty string. Callers fall through to the existing state-file /
// nix-default resolution unchanged, which is the pre-#2092 behaviour.
// This keeps the launch path robust on legacy sessions (pre-#2090 rows
// that have no spawn_inputs entry), on host-mode paths that don't write
// spawn_inputs, and on transient DB problems where wedging the agent
// startup would be the wrong failure mode.

import (
	"github.com/prismatic-koi/prism/internal/profile"
)

// spawnTimeProfileForSession is the cmd-side wrapper around
// profile.SpawnTimeForSession. It opens a short-lived DB handle and
// delegates the actual lookup. See the package-level doc in
// internal/profile for the precedence rationale.
//
// Errors opening the DB are intentionally swallowed (returned as "")
// for the same reason as the inner helper: a transient DB problem must
// not wedge agent startup.
func spawnTimeProfileForSession(sessionName string) string {
	if sessionName == "" {
		return ""
	}
	d, err := openDB()
	if err != nil {
		return ""
	}
	defer d.Close()
	return profile.SpawnTimeForSession(d, sessionName)
}
