package sidecar

import "fmt"

// piExtensionStaleDiagnostic reports whether the sidecar's startup-cached PI
// extension directory has diverged from the value config.json holds now, and
// returns a loud, named diagnostic when it has.
//
// config.Load() memoises its result for the life of
// the process (sync.Once). The sidecar is a long-running process, so the
// pi_extension_dir it read at startup is frozen for its lifetime. A
// `nixos-rebuild switch` that changes the prism PI extension rewrites
// config.json's pi_extension_dir on disk, but the running sidecar keeps the
// pre-switch store path and hands it to every session it spawns thereafter.
// Nothing reports the mismatch: the machine ends in a split state (new sidecar
// code, old extension) that is expensive to diagnose because it looks
// identical to "the change did not work". `prism restart` clears it by
// replacing the process.
//
// This function turns that silent failure into a named one. Callers pass the
// startup-cached value and a fresh re-read of config.json; a non-empty return
// value is the diagnostic to log.
//
// Fail-open contract:
//   - An empty value on EITHER side is treated as "unknown" and returns "".
//     A missing or unreadable config.json makes config.LoadFresh() fall back
//     to defaults (empty pi_extension_dir), so a spawn under a broken or
//     absent config produces no warning and is never blocked.
//   - Equal values return "" — a switch that does not change pi_extension_dir
//     produces no warning and no behaviour change.
//
// The function never blocks a spawn; it only produces a log line. The /spawn
// handler proceeds regardless of the return value.
func piExtensionStaleDiagnostic(cached, current string) string {
	if cached == "" || current == "" {
		// Unknown on one side — fail open, say nothing.
		return ""
	}
	if cached == current {
		return ""
	}
	return fmt.Sprintf(
		"STALE PI EXTENSION: this sidecar started with pi_extension_dir=%q but "+
			"config.json now holds %q. A switch changed the prism PI extension "+
			"after this sidecar started, so sessions it spawns may load the "+
			"pre-switch extension. Run `prism restart` to pick up the new "+
			"extension. (issue #2739)",
		cached, current)
}
