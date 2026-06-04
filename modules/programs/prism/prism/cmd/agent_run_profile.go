package cmd

// agent_run_profile.go — issue #2092 fix.
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
// The lookup is best-effort: any error — DB unreachable, sessions row
// missing, spawn_inputs row missing, profile_name NULL — is collapsed to
// the empty string. Callers fall through to the existing state-file /
// nix-default resolution unchanged, which is the pre-#2092 behaviour.
// This keeps the launch path robust on legacy sessions (pre-#2090 rows
// that have no spawn_inputs entry), on host-mode paths that don't write
// spawn_inputs, and on transient DB problems where wedging the agent
// startup would be the wrong failure mode.

// spawnTimeProfileForSession returns the profile name recorded on the
// spawn_inputs row for the named session, or "" when the row is missing,
// has a NULL profile_name, or any lookup error occurs.
//
// This is the canonical post-#2090 source of "what profile did the user
// invoke `prism spawn --profile` with" — populatePIConfig calls it to
// recover the spawn-time profile choice that ResolveActiveProfile alone
// would silently substitute the active profile for (issue #2092).
//
// Errors are intentionally swallowed (returned as ""): a transient DB
// problem must not wedge the agent startup. The caller falls through to
// the active-profile resolution, which is the pre-#2092 behaviour and is
// the right safety net for legacy sessions / host-mode / etc.
func spawnTimeProfileForSession(sessionName string) string {
	if sessionName == "" {
		return ""
	}
	d, err := openDB()
	if err != nil {
		return ""
	}
	defer d.Close()

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
