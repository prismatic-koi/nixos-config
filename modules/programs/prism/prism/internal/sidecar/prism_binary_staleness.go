package sidecar

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

// prismBinaryStaleDiagnostic reports whether the sidecar's launch-time
// prism executable has diverged from the currently-installed prism binary,
// and returns a loud, named diagnostic when it has.
//
// Background (issue #2742). The sidecar resolves its own executable once, at
// launch time, via os.Executable() inside the prismBinary() closure
// (host_api.go). It execs that path for every delegated operation — 10 call
// sites today, covering /spawn, /review, /cleanup, /prompt, /investigate, and
// the rest. Nix store paths are immutable, so a `nixos-rebuild switch` leaves
// the old store path in place and the sidecar keeps exec'ing it: it silently
// runs pre-switch code for every delegated operation until `prism restart`
// replaces the process. Nothing errors — the switch "succeeds" and the old
// behaviour just persists, which looks identical to "the change did not
// work".
//
// This function turns that silent failure into a named one. Callers pass the
// sidecar's resolved launch-time path and a fresh resolution of the
// currently-installed prism binary; a non-empty return value is the
// diagnostic to log and surface to the caller.
//
// Fail-open contract:
//   - An empty value on EITHER side is treated as "unknown" and returns "".
//     Callers are expected to resolve symlinks before calling this function
//     and pass "" when resolution fails, so a broken or unusual environment
//     (no `prism` on PATH, a dangling symlink, a test stub) never produces a
//     spurious warning.
//   - Equal values return "" — a sidecar that has not survived a switch, or
//     a switch that did not move the prism store path, produces no warning
//     and no behaviour change.
//
// This function never blocks, delays, or fails the operation it is checked
// from — it only ever produces a string for the caller to log or surface.
func prismBinaryStaleDiagnostic(cached, current string) string {
	if cached == "" || current == "" {
		// Unknown on one side — fail open, say nothing.
		return ""
	}
	if cached == current {
		return ""
	}
	return fmt.Sprintf(
		"STALE PRISM BINARY: this sidecar launched from %q but the "+
			"currently-installed prism binary resolves to %q. A switch "+
			"replaced the prism binary after this sidecar started, so "+
			"delegated operations (spawn, review, cleanup, prompt, "+
			"investigate, ...) are still running pre-switch code. Run "+
			"`prism restart` to pick up the new binary. (issue #2742)",
		cached, current)
}

// currentInstalledPrismPath resolves the prism binary currently on PATH,
// following any symlink chain to the real underlying path (e.g. the nix
// store path a home-manager-rendered symlink ultimately points at). It
// returns ("", err) whenever resolution is not possible — no `prism` on
// PATH, or a symlink chain that cannot be fully resolved — so that callers
// can fail open per the staleness contract above rather than guessing.
func currentInstalledPrismPath() (string, error) {
	looked, err := exec.LookPath("prism")
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(looked)
	if err != nil {
		return "", err
	}
	return resolved, nil
}
