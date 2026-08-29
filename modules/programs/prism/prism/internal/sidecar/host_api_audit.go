package sidecar

// Host-API endpoint for the `prism audit` read surface.
//
// `$XDG_STATE_HOME/prism` holds prism.db and is deliberately never bound into
// a sandbox (see internal/container/mounts.go), so a sandboxed caller cannot
// open the database directly. Every read verb needs a host-API proxy branch.
// This endpoint is that branch for `prism audit`, so a sandboxed coordinator
// can read the audit trail it writes.
//
// Endpoint:
//
//	GET /audit[?session=&since=&pattern=&limit=]
//
// Rendering stays on the CLI side: this endpoint returns the same []db.Event
// slice the direct-DB path gets, and cmd/audit.go renders both identically.
// This mirrors the /db/query and /stats convention.
//
// # Security
//
// Two properties hold this endpoint inside the boundary that already exists:
//
//  1. Role gate. The mux wires this route behind requireCoordinator, matching
//     /db/query. /db/query already serves arbitrary read-only SQL to a
//     coordinator, so it already reads every agent_events row. A
//     coordinator-gated audit read therefore grants no capability that is not
//     reachable today.
//
//  2. Server-side type filter. Audit rows are not a separate table —
//     db.QueryAuditEvents selects from agent_events with a hard-coded
//     `type = 'audit'` predicate. That predicate is NOT a parameter of this
//     endpoint and no caller input reaches it. The four query parameters below
//     map 1:1 onto the four QueryAuditEvents arguments, and each is a further
//     restriction ANDed onto the type filter. A caller cannot widen or drop
//     it, so this endpoint can never become a general cross-session
//     conversation-payload reader.
//
// TestHostAPIAudit_NeverReturnsNonAuditRows pins property 2 against hostile
// values for every parameter.

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/prismatic-koi/prism/internal/db"
)

// hostAPIAudit handles GET /audit.
//
// Parameters (all optional; an absent parameter disables its filter):
//
//	session — restrict to this session name (exact match).
//	since   — Unix milliseconds; restrict to events created at or after it.
//	pattern — case-insensitive substring of the payload command field.
//	limit   — return at most this many events.
//
// Response body on success: {"events": [...db.Event...]}. `events` is always
// an array, never null, so the CLI's no-results path behaves the same on both
// routes.
//
// A malformed `since` or `limit` is a 400 rather than a silently ignored
// filter: dropping a filter the caller asked for would widen the result set.
func (s *Sidecar) hostAPIAudit(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DB == nil {
		writeDBErr(w, http.StatusInternalServerError, "audit: no database handle")
		return
	}

	q := r.URL.Query()

	var sinceMs int64
	if raw := q.Get("since"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeDBErr(w, http.StatusBadRequest, "invalid since: "+err.Error())
			return
		}
		sinceMs = parsed
	}

	limit := 0
	if raw := q.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeDBErr(w, http.StatusBadRequest, "invalid limit: "+err.Error())
			return
		}
		limit = parsed
	}

	// The `type = 'audit'` predicate lives inside QueryAuditEvents. Only these
	// four values cross the boundary, and each one narrows the result set.
	events, err := s.cfg.DB.QueryAuditEvents(q.Get("session"), sinceMs, q.Get("pattern"), limit)
	if err != nil {
		writeDBErr(w, http.StatusInternalServerError, "audit query failed: "+err.Error())
		return
	}
	if events == nil {
		events = []db.Event{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"events": events}); err != nil {
		// Best-effort error log; the response is already partially written.
		s.logger().Printf("sidecar: host-API /audit: encode response failed: %v", err)
	}
}
