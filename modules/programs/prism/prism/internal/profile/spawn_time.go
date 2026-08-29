// Package profile centralises the spawn-time profile lookup that connects
// the audit row (`spawn_inputs.profile_name`) to the runtime resolution
// chain.
//
// The helper here is the canonical "what profile did the user spawn this
// session with" reader. It is shared by:
//
//   - cmd/agent_run.go::populatePIConfig (runtime resolution)
//   - internal/review.Run / RunAsync     (review fan-out)
//   - cmd/investigate.go                 (investigate fan-out)
//
// It lives in internal/ so the two child-spawn front doors (review,
// investigate) can read the same audit column without taking an
// `internal/cmd` dependency (no such thing) or duplicating the SQL.
package profile

import (
	"github.com/prismatic-koi/prism/internal/db"
)

// SpawnTimeForSession returns the profile name recorded on the
// `spawn_inputs` row for the named session, or "" when the row is
// missing, has a NULL `profile_name`, or any lookup error occurs.
//
// This is the canonical source of "what profile did the user
// invoke `prism spawn --profile` with" — callers feed the value as the
// `flagValue` argument to `config.ResolveActiveProfile` (treated by
// that function as the highest-precedence source, beating the
// state-file > nix-default fallback).
//
// d may be nil — in that case the function returns "" without touching
// any DB. sessionName may be empty for the same short-circuit.
//
// Errors are intentionally swallowed (returned as ""). A transient DB
// problem, a missing sessions row (legacy), a missing spawn_inputs row,
// or a NULL `profile_name` all collapse to "". The caller is expected to
// fall through to `config.ResolveActiveProfile`'s existing
// state-file > nix-default chain, which is the right safety net for
// legacy sessions and host-mode paths that never wrote a `spawn_inputs`
// row in the first place.
func SpawnTimeForSession(d *db.DB, sessionName string) string {
	if d == nil || sessionName == "" {
		return ""
	}
	sess, err := d.MostRecentSessionForName(sessionName)
	if err != nil || sess == nil {
		return ""
	}
	si, err := d.SpawnInputsByInstanceID(sess.InstanceID)
	if err != nil || si == nil || si.ProfileName == nil {
		return ""
	}
	return *si.ProfileName
}
