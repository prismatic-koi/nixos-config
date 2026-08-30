package cmd

// audit_permission.go — the coordinator-only gate on the DIRECT CLI route of
// `prism audit`.
//
// `prism audit` reaches the DB by one of two routes:
//
//   - Proxy route (PRISM_HOST_API set — a sandboxed caller): the host-API
//     GET /audit handler in internal/sidecar/host_api_audit.go calls
//     requireCoordinator and answers HTTP 403 for a non-coordinator.
//   - Direct route (PRISM_HOST_API unset — a `host`-isolation caller):
//     fetchAuditEventsLocal below calls requireAuditCoordinator, which
//     applies the same rule and returns a non-zero exit.
//
// Both routes must stay gated. When you change either one, change the other
// in the same edit.
//
// # This is not a security boundary
//
// A `host`-mode session runs with no sandbox. It can read the same rows
// without the verb at all:
//
//	sqlite3 ~/.local/state/prism/prism.db "select * from agent_events where type = 'audit'"
//
// The gate is justified on two narrower grounds: correct behaviour for a
// cooperative caller, and route consistency with `prism investigate`,
// `prism merges`, and `prism checkin` — the audit verb is the fourth member
// of that set.
//
// # Shape
//
// This mirrors requireMergesCoordinator in cmd/merges.go exactly: the same
// session.IsCoordinatorSession call, keyed on the resolved caller session,
// and the same fail-closed behaviour for an unresolvable caller. `prism
// audit` has no bare-shell bootstrap flow to preserve and no keybind that
// invokes it, so the three properties that make a caller-keyed guard safe
// here, but not for `prism spawn`, all hold, exactly as they do for `merges`
// and `checkin`.
//
// Caller identity follows the same resolution as requireMergesCoordinator:
// review.LookupParentSession() tries PRISM_SESSION_NAME first, else the
// current tmux session. An unresolvable caller is refused with an error
// naming PRISM_SESSION_NAME, before QueryAuditEvents is ever reached.

import (
	"fmt"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// requireAuditCoordinator is the direct-CLI half of the coordinator-only gate
// on `prism audit`. See the package-level doc comment above for
// the full rationale.
func requireAuditCoordinator(callerSession string, d *db.DB) error {
	if callerSession == "" {
		return fmt.Errorf("prism audit: cannot determine caller session — run from inside a prism tmux session or set PRISM_SESSION_NAME")
	}
	if session.IsCoordinatorSession(callerSession, d) {
		return nil
	}
	return fmt.Errorf(`prism audit: this command is for coordinator sessions only (caller: %s).

The audit trail belongs to the coordinator's oversight of high-impact tool
calls. Workers, review agents, and investigators must not read it. Ask your
coordinator to run:

  prism audit

See: modules/programs/prism/agents/coordinator.md`, callerSession)
}
