// Package profile centralises the spawn-time profile lookup that connects
// the #2090 audit row (`spawn_inputs.profile_name`) to the #2092 runtime
// resolution chain.
//
// The helper here is the canonical "what profile did the user spawn this
// session with" reader. It is shared by:
//
//   - cmd/agent_run.go::populatePIConfig (runtime resolution, #2092)
//   - internal/review.Run / RunAsync     (review fan-out, #2097)
//   - cmd/investigate.go                 (investigate fan-out, #2097)
//
// Originally lived as `spawnTimeProfileForSession` in
// `cmd/agent_run_profile.go` (#2092). Promoted to internal/ in #2097
// so the two child-spawn front doors (review, investigate) can read the
// same audit column without taking an `internal/cmd` dependency (no such
// thing) or duplicating the SQL.
package profile

import (
	"github.com/prismatic-koi/prism/internal/db"
)

// SpawnTimeForSession returns the profile name recorded on the
// `spawn_inputs` row for the named session, or "" when the row is
// missing, has a NULL `profile_name`, or any lookup error occurs.
//
// This is the canonical post-#2090 source of "what profile did the user
// invoke `prism spawn --profile` with" — callers feed the value as the
// `flagValue` argument to `config.ResolveActiveProfile` (treated by
// that function as the highest-precedence source, beating the
// state-file > nix-default fallback).
//
// d may be nil — in that case the function returns "" without touching
// any DB. sessionName may be empty for the same short-circuit.
//
// Errors are intentionally swallowed (returned as ""). A transient DB
// problem, a missing sessions row (legacy / pre-#2090), a missing
// spawn_inputs row, or a NULL `profile_name` all collapse to "". The
// caller is expected to fall through to `config.ResolveActiveProfile`'s
// existing state-file > nix-default chain, which is the pre-#2092
// behaviour and the right safety net for legacy sessions and host-mode
// paths that never wrote a `spawn_inputs` row in the first place.
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
