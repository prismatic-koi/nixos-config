package profile

// inherit.go — child-spawn profile-inheritance helper (issue #2097).
//
// `prism review` and `prism investigate` spawn child agents whose
// profile must inherit the parent worker's spawn-time profile. Before
// #2097 both front doors resolved the child's profile via
// `ResolveActiveProfile(pf, "")` — empty flag value — so the resolution
// silently fell through to state-file > nix-default regardless of the
// parent's `--profile` choice. This was the same shape of bug #2092
// fixed for the worker layer; this file extends the precedence chain
// to the child-spawn surfaces.

import (
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
)

// InheritFromParent returns the profile name a child session should be
// spawned with, given the parent session it was invoked from
// (issue #2097).
//
// Precedence (matches the worker-layer chain established in #2092):
//
//  1. Parent's `spawn_inputs.profile_name` — highest. Carries the
//     `prism spawn --profile X` / `--abtest A B` choice forward to
//     the child.
//  2. Runtime state file (`$XDG_STATE_HOME/prism/active-profile`).
//  3. `pf.Default` — the nix-configured default, lowest.
//
// The returned name is suitable for both downstream uses on the
// child-spawn path:
//
//   - As the `activeProfile` for the review round's `config.RequireSlot`
//     gate, so the slot each reviewer runs on is the parent's profile
//     slot rather than the host default.
//   - As `session.SpawnOpts.ProfileName` so the child's own
//     `spawn_inputs.profile_name` row is populated, downstream
//     `prism stats` / archive queries reflect the inherited profile,
//     and the child's runtime `populatePIConfig` resolves to the
//     same value via the #2092 lookup.
//
// pf may be nil — in that case the function returns the parent's raw
// spawn-time profile (which may itself be "") and the caller falls
// through to whatever default behaviour applies in their context.
// ResolveActiveProfile also tolerates nil pf, so no extra guard is
// needed here.
//
// State-file read errors are surfaced (matching the worker-layer
// semantic from #2092) — a corrupt active-profile file is a real
// problem, not a silent fallthrough condition.
func InheritFromParent(d *db.DB, parentSession string, pf *config.ProfilesFile) (string, error) {
	spawnProfile := SpawnTimeForSession(d, parentSession)
	resolved, _, err := config.ResolveActiveProfile(pf, spawnProfile)
	return resolved, err
}
