package cmd

// agent_run_profile.go resolves the spawn-time profile for a session.
//
// `prism spawn --profile <X>` and `prism spawn --abtest <A> <B>` carry the
// per-spawn profile name through to `spawn_inputs.profile_name` (the audit
// row). spawnTimeProfileForSession reads that value off the audit row and
// returns it, which populatePIConfig then passes as the `flagValue` argument
// to ResolveActiveProfile (treated by that function as the
// highest-precedence source). Without it, the active profile would win over
// the spawn-time choice and both legs of an --abtest pair would run on the
// same slot.
//
// The SQL-shaped half of this helper lives in
// `internal/profile.SpawnTimeForSession` so the same lookup can be reused by
// the child-spawn surfaces (`internal/review` and `cmd/investigate.go`)
// without a `cmd`-package import cycle. This thin wrapper opens its own
// short-lived DB handle (populatePIConfig does not get a *db.DB threaded
// through).
//
// The lookup is best-effort: any error — DB unreachable, sessions row
// missing, spawn_inputs row missing, profile_name NULL — is collapsed to
// the empty string. Callers fall through to the state-file / nix-default
// resolution. This keeps the launch path robust on a session with no
// spawn_inputs entry, on host-mode paths that do not write spawn_inputs, and
// on transient DB problems where wedging the agent startup is the wrong
// failure mode.

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
