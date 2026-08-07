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

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sessionname"
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
	// The name-parse fallback applies to @-bearing names only, and issue #2658
	// deliberately did not widen it. A name with no "@" and no DB row is an
	// unknown session, and the caller must see "not found" (404) rather than a
	// repo invented from the name. If this fallback answered for every name,
	// `prism prompt <typo>` would resolve to a repo of its own, pass the
	// cross-repo gate as a bare-name root session, and fail later with an
	// opaque delivery error — the exact confusion #2658 reports.
	if sessionname.HasBranch(sessionName) {
		return sessionname.Repo(sessionName), nil
	}
	return "", fmt.Errorf("session %q not found", sessionName)
}

// RepoFromSessionName returns the repo of a session name, using the name
// alone. It never reads the database and never fails.
//
// Prefer RepoFromSession where a DB handle exists: agent_status.repo is
// authoritative. Use this function where the input is a name and there is no
// row to read — for example when a repo is derived for a session that is
// about to be created.
func RepoFromSessionName(sessionName string) string {
	return sessionname.Repo(sessionName)
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
	// A descendant session — a review agent or an investigator — is never a
	// coordinator, whatever its root_agent_name column says (issue #2658).
	// The guard is on the name, so a wrong DB value cannot promote it. The
	// same guard is applied by session.IsCoordinatorSession, by
	// db.retroIsCoordinator, and by IsRootSession below; all four read the
	// rule from sessionname.IsDescendant so they cannot drift apart.
	if sessionname.IsDescendant(sessionName) {
		return false
	}
	if d != nil {
		name, rowExists, err := d.RootAgentName(sessionName)
		if err == nil && rowExists {
			if name != "" {
				nameBased := sessionname.HasCoordinatorSuffix(sessionName)
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
	return sessionname.HasCoordinatorSuffix(sessionName)
}

// IsRootSession reports whether sessionName is the root session of its own
// project — the session an operator addresses when they mean "the top-level
// session over there" (issue #2658).
//
// A session is a root session when ALL of the following hold:
//
//   - it is not a meta-session (scratchpad, prism-dashboard);
//   - it is not a descendant (the name carries no "~");
//   - its repo is non-empty; and
//   - it is either a coordinator (a "<repo>@main" worktree session, per
//     IsCoordinatorSession), or a bare name that carries no branch at all.
//
// # Why this is not IsCoordinatorSession
//
// The bug in #2658 is that a non-worktree session such as `obsidian` can never
// satisfy the "@main" heuristic — it has no worktree, so it has no branch —
// and so a single wrong root_agent_name value made it permanently unreachable
// and invisible. The obvious repair is to call such a session a coordinator.
// That repair is too broad. Coordinator status also grants the merge queue
// (cmd/merge.go, cmd/merges.go), `prism investigate` (cmd/investigate.go),
// `prism review` on another session's PR (cmd/review.go), the profile
// override (cmd/profile.go), the permission audit (cmd/audit_permission.go),
// and the wide tier-2/tier-3 `prism checkin` scope (authz/checkin.go). None of
// those is needed to make a session reachable by name.
//
// So this predicate is deliberately narrow. It is read at exactly two places:
// the cross-repo arm of the host-API /prompt gate, and the cross-repo arm of
// the `prism sessions list` query. Everything else keeps reading
// IsCoordinatorSession and is unchanged.
//
// # Why a bare name is admitted on its name alone
//
// The bare-name arm has no DB corroboration inside this function, which keeps
// it testable with a nil handle. The /prompt route corroborates it upstream
// anyway: it resolves the target through RepoFromSession first, and that call
// requires a live agent_status row for any name with no "@". An unknown bare
// name is therefore refused with 404 before this predicate is consulted.
//
// logger must be non-nil; callers normalise it first.
func IsRootSession(sessionName string, d *db.DB, logger *log.Logger) bool {
	if sessionname.IsMeta(sessionName) {
		return false
	}
	if sessionname.IsDescendant(sessionName) {
		return false
	}
	// A name that yields no repo ("", "@main", "@") is malformed. Fail closed
	// rather than admit it on the strength of a suffix match.
	if sessionname.Repo(sessionName) == "" {
		return false
	}
	if sessionname.HasBranch(sessionName) {
		// Worktree session: the root of the repo is its coordinator, and the
		// coordinator rule is unchanged by #2658.
		return IsCoordinatorSession(sessionName, d, logger)
	}
	// No branch means no worktree, so there is no "@main" sibling that could
	// be the root instead. The session is the root of its own directory.
	return true
}
