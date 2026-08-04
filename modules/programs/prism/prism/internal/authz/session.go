package authz

// session.go — the session-identity helpers the permission predicates in this
// package rest on.
//
// Both function bodies were moved here verbatim from
// internal/sidecar/helpers.go by issue #2619. They had to move with the
// predicate: `cmd/` cannot reach an unexported helper in `internal/sidecar`,
// and a second copy in `cmd/` would let the two routes of one verb disagree
// about who a caller is, which is the exact defect #2619 exists to close.
//
// internal/sidecar keeps `repoFromSession` and `isCoordinatorSession` as thin
// wrappers over these two functions, so the fifteen-odd host-API handlers that
// scope on repo or role are untouched by the move and keep one implementation.
//
// A near-duplicate of IsCoordinatorSession already lives at
// internal/session.IsCoordinatorSession, which `cmd/merges.go` uses. It takes
// no logger and writes its diagnostics through the global `log` package. The
// two are NOT merged here: the sidecar variant must write to the sidecar's own
// logger, and rerouting fifteen call sites' diagnostics is a behaviour change
// #2619 does not authorise. Merging them is a separate change.

import (
	"fmt"
	"log"
	"strings"

	"github.com/prismatic-koi/prism/internal/db"
)

// RepoFromSession returns the repo prefix for a session.
//
// When d is non-nil and an agent_status row exists for sessionName with a
// non-empty repo column, that value is returned. This is the authoritative
// path and resolves both `<repo>@<branch>` sessions and @-less host-mode
// sessions (e.g. "obsidian" against ~/Documents/obsidian via
// ProjectIsolationOverrides — issue #2112). Without the DB lookup, host-mode
// non-git sessions whose names lack an `@<branch>` suffix could not be
// resolved and every host-API permission check that called this helper
// rejected them with a "contains no '@' — cannot derive repo" parse error.
//
// When the DB lookup misses (no row, empty repo, DB error, or d == nil), the
// helper falls back to parsing the session name as `<repo>@<branch>`. The
// fallback keeps RepoFromSession usable in unit tests that do not seed a row
// and preserves the original behaviour for the dominant @-bearing case when
// the DB has not yet recorded a row for a freshly-minted session.
//
// Returns an error only when both paths fail: no DB row (or no DB) AND the
// name contains no "@". The error message names the unknown session so the
// caller can surface a clear "session not found" error rather than a parse-
// failure description (AC: prism checkin <unknown-name> should not say
// "cannot derive repo").
func RepoFromSession(sessionName string, d *db.DB) (string, error) {
	if d != nil {
		if status, err := d.CurrentStatus(sessionName); err == nil && status != nil && status.Repo != "" {
			return status.Repo, nil
		}
	}
	if idx := strings.Index(sessionName, "@"); idx >= 0 {
		return sessionName[:idx], nil
	}
	return "", fmt.Errorf("session %q not found", sessionName)
}

// IsCoordinatorSession returns true when the session is a coordinator. When d
// is non-nil and the session has a row with a non-NULL root_agent_name, it
// computes dbBased = (root_agent_name == "coordinator") and nameBased =
// (sessionName ends with "@main") and returns dbBased || nameBased. This means
// the @main heuristic wins when it disagrees with a stale or incorrect DB value
// (e.g. "worker" written during an SSE inference race); a [debug] log is emitted
// on mismatch. Falls back to the name-suffix heuristic for pre-migration rows
// (NULL root_agent_name) and when d is nil.
//
// logger must be non-nil. Every caller in this package normalises it first;
// the sidecar wrapper passes the sidecar's own logger.
func IsCoordinatorSession(sessionName string, d *db.DB, logger *log.Logger) bool {
	if d != nil {
		name, rowExists, err := d.RootAgentName(sessionName)
		if err == nil && rowExists {
			if name != "" {
				nameBased := strings.HasSuffix(sessionName, "@main")
				dbBased := name == "coordinator"
				if dbBased != nameBased {
					logger.Printf("[debug] sidecar: isCoordinatorSession(%q): DB says %v (root_agent_name=%q), name heuristic says %v — heuristic wins",
						sessionName, dbBased, name, nameBased)
				}
				return dbBased || nameBased
			}
			// Row exists but root_agent_name is NULL — pre-migration row.
			logger.Printf("[deprecation] sidecar: isCoordinatorSession(%q): root_agent_name is NULL — pre-migration row, using name heuristic", sessionName)
		} else if err != nil {
			logger.Printf("sidecar: isCoordinatorSession: DB error for %q: %v — falling back to name heuristic", sessionName, err)
		}
		// rowExists=false means no row — no log needed, just use heuristic.
	}
	// Pre-migration fallback or DB unavailable: use name-suffix heuristic.
	return strings.HasSuffix(sessionName, "@main")
}
