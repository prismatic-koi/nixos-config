package sidecar

// Host-API endpoints for the `prism db` read-only surface.
//
// These endpoints proxy `prism db query`, `prism db schema`, and
// `prism db tables` from inside a sandbox out to the host. Each opens its
// own SQLite read-only handle (`?mode=ro`) per request rather than sharing
// the sidecar's writable handle, so the safety boundary stays obvious in
// the code.
//
// Endpoints:
//
//	GET /db/query?sql=&timeout=  — runs a single read-only statement.
//	GET /db/schema[?table=]      — returns CREATE TABLE / CREATE INDEX DDL.
//	GET /db/tables               — returns user table names sorted.
//
// Rendering stays on the CLI side. These endpoints return structured JSON;
// the CLI renders to aligned tables or JSON depending on --json. Rendering
// stays in one place, as the other read endpoints do.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// hostAPIDBQuery handles GET /db/query.
//
// Parameters:
//
//	sql      — the SQL text (required). Must be a single statement; the CLI
//	           rejects multi-statement input before issuing this request, but
//	           we re-check here as a defensive measure.
//	timeout  — Go duration string (e.g. "5s", "100ms"). Defaults to 5s.
//
// Response body on success: db.QueryResult marshalled directly. Errors are
// reported as HTTP 4xx with `{"error": "..."}` and a clear message.
func (s *Sidecar) hostAPIDBQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sqlText := q.Get("sql")
	if sqlText == "" {
		writeDBErr(w, http.StatusBadRequest, "sql is required")
		return
	}

	// Single-statement guard. The CLI also enforces this, but we re-check
	// here so direct host-API callers (e.g. agent debugging) get the same
	// safety net.
	ok, err := db.IsSingleStatement(sqlText)
	if err != nil {
		writeDBErr(w, http.StatusBadRequest, "sql parse error: "+err.Error())
		return
	}
	if !ok {
		writeDBErr(w, http.StatusBadRequest, "sql must contain exactly one statement")
		return
	}

	timeout := 5 * time.Second
	if t := q.Get("timeout"); t != "" {
		parsed, perr := time.ParseDuration(t)
		if perr != nil {
			writeDBErr(w, http.StatusBadRequest, "invalid timeout: "+perr.Error())
			return
		}
		if parsed > 0 {
			timeout = parsed
		}
	}

	conn, err := db.OpenReadOnly(s.cfg.DB.Path())
	if err != nil {
		writeDBErr(w, http.StatusInternalServerError, "open read-only db: "+err.Error())
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	start := time.Now()
	res, err := db.RunQuery(ctx, conn, sqlText)
	if err != nil {
		// Read-only rejection is a clear 4xx — the user wrote a write query.
		if db.IsReadOnlyError(err) {
			writeDBErr(w, http.StatusBadRequest, "read-only: "+err.Error())
			return
		}
		// Timeout / cancellation surfaces via context.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeDBErr(w, http.StatusRequestTimeout, fmt.Sprintf("query timeout exceeded (%s)", timeout))
			return
		}
		writeDBErr(w, http.StatusBadRequest, "query failed: "+err.Error())
		return
	}
	res.ElapsedMs = time.Since(start).Milliseconds()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		// Best-effort error log; the response is already partially written.
		s.logger().Printf("sidecar: host-API /db/query: encode response failed: %v", err)
	}
}

// hostAPIDBSchema handles GET /db/schema[?table=].
//
// Response body on success: {"entries": [...db.SchemaEntry...]}.
func (s *Sidecar) hostAPIDBSchema(w http.ResponseWriter, r *http.Request) {
	tableFilter := r.URL.Query().Get("table")

	conn, err := db.OpenReadOnly(s.cfg.DB.Path())
	if err != nil {
		writeDBErr(w, http.StatusInternalServerError, "open read-only db: "+err.Error())
		return
	}
	defer conn.Close()

	entries, err := db.Schema(r.Context(), conn, tableFilter)
	if err != nil {
		writeDBErr(w, http.StatusInternalServerError, "schema query failed: "+err.Error())
		return
	}
	if tableFilter != "" && len(entries) == 0 {
		writeDBErr(w, http.StatusNotFound, fmt.Sprintf("table %q not found", tableFilter))
		return
	}
	if entries == nil {
		entries = []db.SchemaEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
}

// hostAPIDBTables handles GET /db/tables.
//
// Response body on success: {"tables": [...string...]}.
func (s *Sidecar) hostAPIDBTables(w http.ResponseWriter, r *http.Request) {
	conn, err := db.OpenReadOnly(s.cfg.DB.Path())
	if err != nil {
		writeDBErr(w, http.StatusInternalServerError, "open read-only db: "+err.Error())
		return
	}
	defer conn.Close()

	names, err := db.Tables(r.Context(), conn)
	if err != nil {
		writeDBErr(w, http.StatusInternalServerError, "tables query failed: "+err.Error())
		return
	}
	if names == nil {
		names = []string{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tables": names})
}

// writeDBErr writes a standard {"error": ...} JSON body with the given status.
// Mirrors the writeError closure inside hostAPIHandler() so the new file does
// not need access to those closures.
func writeDBErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
