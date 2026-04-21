package session

import (
	"log"
	"strings"

	"github.com/prismatic-koi/prism/internal/db"
)

// ScratchpadSession is the canonical name of the prism scratchpad tmux session.
const ScratchpadSession = "scratchpad"

// IsMetaSession reports whether a session name identifies a prism-internal
// meta-session (scratchpad, dashboard, etc.) rather than a user-spawned agent
// session. Meta-sessions must not appear in agent_status and are excluded from
// all session listings.
func IsMetaSession(name string) bool {
	switch name {
	case ScratchpadSession, "prism-dashboard":
		return true
	}
	return false
}

// IsCoordinatorSession reports whether the named session is a coordinator.
//
// Detection strategy (in order):
//  1. DB-backed: if d is non-nil and the session has a row with a non-NULL
//     root_agent_name, return root_agent_name == "coordinator". This is the
//     primary signal introduced by PR #928 (Issue F / #861).
//  2. NULL root_agent_name (pre-migration row): log a deprecation warning and
//     fall through to the name-suffix heuristic.
//  3. No DB, DB error, or no row: fall back to the name-suffix heuristic
//     (session name ends with "@main"), which matches the convention used by
//     isCoordinator() in sidecar.go.
//
// The d parameter may be nil; when nil the DB-backed lookup is skipped and
// only the heuristic is used.
func IsCoordinatorSession(sessionName string, d *db.DB) bool {
	if d != nil {
		name, rowExists, err := d.RootAgentName(sessionName)
		if err == nil && rowExists {
			if name != "" {
				dbBased := name == "coordinator"
				nameBased := strings.HasSuffix(sessionName, "@main")
				if dbBased != nameBased {
					log.Printf("[debug] session: IsCoordinatorSession(%q): DB says %v (root_agent_name=%q), name heuristic says %v — heuristic wins",
						sessionName, dbBased, name, nameBased)
				}
				return dbBased || nameBased
			}
			// Row exists but root_agent_name is NULL — pre-migration row.
			log.Printf("[deprecation] session: IsCoordinatorSession(%q): root_agent_name is NULL — pre-migration row, falling back to name heuristic", sessionName)
		} else if err != nil {
			log.Printf("session: IsCoordinatorSession: DB error for %q: %v — falling back to name heuristic", sessionName, err)
		}
		// rowExists=false: no row yet — fall through to heuristic silently.
	}
	// Pre-migration fallback or DB unavailable: use name-suffix heuristic.
	return strings.HasSuffix(sessionName, "@main")
}
