package sidecar

// Tests for the host-API GET /db/query, /db/schema, /db/tables endpoints
// added in #1467.
//
// These exercise the hostAPIHandler() method directly (no live socket),
// matching the shape of sidecar_stats_test.go. The fixtures use openTestDB(t)
// from sidecar_test.go which provides a real on-disk SQLite file — required
// because the new endpoints re-open the DB in read-only mode (?mode=ro)
// rather than sharing the writable handle.
//
// All three endpoints are coordinator-only (#1467 round-3 review): /db/query
// exposes a strict superset of /checkin's data, so it inherits /checkin's
// gating. Tests that exercise post-auth happy / error paths use a coordinator
// sidecar; the *_WorkerForbidden tests at the bottom verify the role gate
// itself (matching the TestHostAPI_Checkin_WorkerForbidden precedent in
// sidecar_test.go).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// ── GET /db/tables ────────────────────────────────────────────────────────────

func TestHostAPI_DBTables_HappyPath(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/db/tables", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp struct {
		Tables []string `json:"tables"`
	}
	decodeJSONBody(t, rr, &resp)
	if len(resp.Tables) == 0 {
		t.Fatal("Tables empty; want at least the prism schema")
	}
	for _, name := range resp.Tables {
		if strings.HasPrefix(name, "sqlite_") {
			t.Fatalf("Tables included internal %q", name)
		}
	}
	// Sorted check.
	for i := 1; i < len(resp.Tables); i++ {
		if resp.Tables[i-1] > resp.Tables[i] {
			t.Fatalf("Tables not sorted: %v", resp.Tables)
		}
	}
}

func TestHostAPI_DBTables_PostRejected(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/db/tables", "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

// ── GET /db/schema ────────────────────────────────────────────────────────────

func TestHostAPI_DBSchema_All(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/db/schema", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp struct {
		Entries []db.SchemaEntry `json:"entries"`
	}
	decodeJSONBody(t, rr, &resp)
	if len(resp.Entries) == 0 {
		t.Fatal("Entries empty")
	}
	// First entry must be a table (tables come before indexes).
	if resp.Entries[0].Type != "table" {
		t.Errorf("first entry type = %q, want table", resp.Entries[0].Type)
	}
}

func TestHostAPI_DBSchema_FilteredTable(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/db/schema?table=agent_events", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp struct {
		Entries []db.SchemaEntry `json:"entries"`
	}
	decodeJSONBody(t, rr, &resp)
	if len(resp.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(resp.Entries))
	}
	if resp.Entries[0].Name != "agent_events" {
		t.Errorf("got name %q, want agent_events", resp.Entries[0].Name)
	}
}

func TestHostAPI_DBSchema_UnknownTable(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/db/schema?table=nope_does_not_exist", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	var errResp struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, rr, &errResp)
	if !strings.Contains(errResp.Error, "not found") {
		t.Errorf("error %q does not mention 'not found'", errResp.Error)
	}
}

// ── GET /db/query — happy path ────────────────────────────────────────────────

func TestHostAPI_DBQuery_HappyPath(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet,
		"/db/query?sql="+queryEscape("SELECT 1 AS one, 'hi' AS greeting"), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp db.QueryResult
	decodeJSONBody(t, rr, &resp)
	if len(resp.Columns) != 2 || resp.Columns[0] != "one" || resp.Columns[1] != "greeting" {
		t.Errorf("Columns = %v, want [one greeting]", resp.Columns)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(resp.Rows))
	}
	// JSON unmarshal turns numbers into float64 for any → cast through that.
	if got := resp.Rows[0][0]; got == nil {
		t.Errorf("col 0 = nil, want 1")
	}
	if got, _ := resp.Rows[0][1].(string); got != "hi" {
		t.Errorf("col 1 = %v, want hi", resp.Rows[0][1])
	}
}

func TestHostAPI_DBQuery_EmptyResult(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet,
		"/db/query?sql="+queryEscape("SELECT name FROM sqlite_master WHERE 1=0"), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	// Verify the JSON literally has "rows":[] — never "rows":null.
	body := rr.Body.String()
	if !strings.Contains(body, `"rows":[]`) {
		t.Fatalf("response body did not contain `\"rows\":[]`: %s", body)
	}
}

// ── GET /db/query — error paths ───────────────────────────────────────────────

func TestHostAPI_DBQuery_RejectsWrite(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet,
		"/db/query?sql="+queryEscape(
			"INSERT INTO sessions (instance_id, session_name, repo, worktree, harness, started_at) VALUES ('x','x','x','x','pi',1)"),
		"")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	var errResp struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, rr, &errResp)
	low := strings.ToLower(errResp.Error)
	if !strings.Contains(low, "read") {
		t.Errorf("error %q does not mention read-only", errResp.Error)
	}
}

func TestHostAPI_DBQuery_RejectsMultiStatement(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet,
		"/db/query?sql="+queryEscape("SELECT 1; SELECT 2"), "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	var errResp struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, rr, &errResp)
	if !strings.Contains(strings.ToLower(errResp.Error), "single statement") &&
		!strings.Contains(strings.ToLower(errResp.Error), "exactly one statement") {
		t.Errorf("error %q does not mention single-statement", errResp.Error)
	}
}

func TestHostAPI_DBQuery_MalformedSQL(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet,
		"/db/query?sql="+queryEscape("SELECTX from where !!!"), "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	var errResp struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, rr, &errResp)
	if errResp.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestHostAPI_DBQuery_MissingSQL(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@main", "myrepo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/db/query", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// ── GET /db/* — role gating (#1467 round-3 review) ──────────────────────
//
// All three endpoints are coordinator-only because /db/query exposes a strict
// superset of /checkin's data. These mirror TestHostAPI_Checkin_WorkerForbidden
// in sidecar_test.go: a worker-role sidecar must receive 403, with a non-empty
// error message.

func TestHostAPI_DBQuery_WorkerForbidden(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@feature", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodGet,
		"/db/query?sql="+queryEscape("SELECT 1"), "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403", rr.Code, rr.Body.String())
	}
	var errResp map[string]string
	decodeJSONBody(t, rr, &errResp)
	if errResp["error"] == "" {
		t.Error("expected error field in 403 response")
	}
}

func TestHostAPI_DBSchema_WorkerForbidden(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@feature", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/db/schema", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403", rr.Code, rr.Body.String())
	}
	var errResp map[string]string
	decodeJSONBody(t, rr, &errResp)
	if errResp["error"] == "" {
		t.Error("expected error field in 403 response")
	}
}

func TestHostAPI_DBTables_WorkerForbidden(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@feature", "myrepo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/db/tables", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403", rr.Code, rr.Body.String())
	}
	var errResp map[string]string
	decodeJSONBody(t, rr, &errResp)
	if errResp["error"] == "" {
		t.Error("expected error field in 403 response")
	}
}

// queryEscape percent-encodes a SQL string for placement in a query parameter.
func queryEscape(sqlText string) string {
	// We could pull in net/url, but our tests need only minimal escaping —
	// space, semicolon, single-quote, equals — none of which are reserved
	// for the URI authority parsing of httptest. Use json marshalling's
	// trick: marshal as a string and hand-encode the few critical chars.
	b, _ := json.Marshal(sqlText)
	// strip surrounding quotes
	s := string(b[1 : len(b)-1])
	// percent-encode the chars that mess with query strings.
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ':
			out.WriteString("%20")
		case '=':
			out.WriteString("%3D")
		case '&':
			out.WriteString("%26")
		case '#':
			out.WriteString("%23")
		case '+':
			out.WriteString("%2B")
		case ';':
			out.WriteString("%3B")
		case '\'':
			out.WriteString("%27")
		case '"':
			out.WriteString("%22")
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}
