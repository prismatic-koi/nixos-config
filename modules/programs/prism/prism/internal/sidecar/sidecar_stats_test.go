package sidecar

// Tests for the host-API GET /stats endpoint added in #1463.
//
// These tests exercise the hostAPIHandler() method directly without spinning
// up a real Unix socket server. The shape mirrors the existing /merge, /merges
// tests in sidecar_merge_test.go.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// writeDoomLoopEventForSidecar writes a doom_loop_detected event for sidecar tests.
func writeDoomLoopEventForSidecar(t *testing.T, d *db.DB, session, tool, pattern string, count int, ts time.Time) {
	t.Helper()
	e := db.Event{
		ID:          fmt.Sprintf("dl-%s-%s", session, tool),
		SessionName: session,
		Type:        "doom_loop_detected",
		Payload:     fmt.Sprintf(`{"tool":%q,"pattern":%q,"count":%d,"timestampMs":%d}`, tool, pattern, count, ts.UnixMilli()),
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent doom_loop: %v", err)
	}
}

// writePermissionEventForSidecar writes a permission_denied or permission_ask event.
func writePermissionEventForSidecar(t *testing.T, d *db.DB, session, eventType, tool string, ts time.Time) {
	t.Helper()
	e := db.Event{
		ID:          fmt.Sprintf("perm-%s-%s-%s", session, eventType, tool),
		SessionName: session,
		Type:        eventType,
		Payload:     fmt.Sprintf(`{"tool":%q}`, tool),
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent %s: %v", eventType, err)
	}
}

// ── GET /stats?view=doomloops ─────────────────────────────────────────────────

// TestHostAPI_Stats_Doomloops_HappyPath verifies that GET /stats?view=doomloops
// returns doom_loop_detected events as a JSON array inside {"events":[...]}.
func TestHostAPI_Stats_Doomloops_HappyPath(t *testing.T) {
	d := openTestDB(t)

	// Seed status row so WriteEvent does not fail FK constraint.
	if err := d.UpsertStatus("nixos-config@main", "nixos-config", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	writeDoomLoopEventForSidecar(t, d, "nixos-config@main", "bash", "git status", 5, time.Now().Add(-1*time.Hour))

	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=doomloops&days=7", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp struct {
		Events []db.Event `json:"events"`
	}
	decodeJSONBody(t, rr, &resp)

	if len(resp.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(resp.Events))
	}
	if resp.Events[0].Type != "doom_loop_detected" {
		t.Errorf("event type = %q, want doom_loop_detected", resp.Events[0].Type)
	}
}

// TestHostAPI_Stats_Doomloops_EmptyResult verifies that an empty result returns
// {"events":[]} (not null).
func TestHostAPI_Stats_Doomloops_EmptyResult(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=doomloops&days=7", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp struct {
		Events []db.Event `json:"events"`
	}
	decodeJSONBody(t, rr, &resp)
	if resp.Events == nil {
		t.Error("events field should be an empty array, not null")
	}
	if len(resp.Events) != 0 {
		t.Errorf("got %d events, want 0", len(resp.Events))
	}
}

// ── GET /stats?view=denials ───────────────────────────────────────────────────

// TestHostAPI_Stats_Denials_HappyPath verifies that GET /stats?view=denials
// returns permission_denied events.
func TestHostAPI_Stats_Denials_HappyPath(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("nixos-config@main", "nixos-config", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	writePermissionEventForSidecar(t, d, "nixos-config@main", "permission_denied", "bash", time.Now().Add(-1*time.Hour))

	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=denials&days=7", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp struct {
		Events []db.Event `json:"events"`
	}
	decodeJSONBody(t, rr, &resp)
	if len(resp.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(resp.Events))
	}
	if resp.Events[0].Type != "permission_denied" {
		t.Errorf("event type = %q, want permission_denied", resp.Events[0].Type)
	}
}

// TestHostAPI_Stats_Denials_UnknownSession returns 404 when session filter
// matches no known session.
func TestHostAPI_Stats_Denials_UnknownSession(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=denials&session=nixos-config%40ghost&days=7", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q, want 404", rr.Code, rr.Body.String())
	}
}

// ── GET /stats?view=asks ──────────────────────────────────────────────────────

// TestHostAPI_Stats_Asks_HappyPath verifies that GET /stats?view=asks
// returns permission_ask events.
func TestHostAPI_Stats_Asks_HappyPath(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("nixos-config@main", "nixos-config", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	writePermissionEventForSidecar(t, d, "nixos-config@main", "permission_ask", "bash", time.Now().Add(-1*time.Hour))

	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=asks&days=7", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp struct {
		Events []db.Event `json:"events"`
	}
	decodeJSONBody(t, rr, &resp)
	if len(resp.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(resp.Events))
	}
}

// ── GET /stats?view=summary ───────────────────────────────────────────────────

// TestHostAPI_Stats_Summary_HappyPath verifies that GET /stats?view=summary
// returns a sessions array.
func TestHostAPI_Stats_Summary_HappyPath(t *testing.T) {
	d := openTestDB(t)

	instanceID := "aaaa1111-2222-3333-4444-555555555555"
	sess := db.Session{
		InstanceID:  instanceID,
		SessionName: "nixos-config@main",
		Repo:        "nixos-config",
		Worktree:    "/tmp/w",
		Harness:     "pi",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=summary", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp struct {
		Sessions []db.Session `json:"sessions"`
	}
	decodeJSONBody(t, rr, &resp)
	if len(resp.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(resp.Sessions))
	}
	if resp.Sessions[0].SessionName != "nixos-config@main" {
		t.Errorf("session name = %q, want nixos-config@main", resp.Sessions[0].SessionName)
	}
}

// TestHostAPI_Stats_Summary_EmptyResult verifies that an empty sessions table
// returns {"sessions":[]} (not null).
func TestHostAPI_Stats_Summary_EmptyResult(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=summary", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp struct {
		Sessions []db.Session `json:"sessions"`
	}
	decodeJSONBody(t, rr, &resp)
	if resp.Sessions == nil {
		t.Error("sessions field should be an empty array, not null")
	}
}

// ── GET /stats?view=detail ────────────────────────────────────────────────────

// TestHostAPI_Stats_Detail_HappyPath verifies that GET /stats?view=detail&session=<name>
// returns the most recent sessions row for that session name.
func TestHostAPI_Stats_Detail_HappyPath(t *testing.T) {
	d := openTestDB(t)

	instanceID := "bbbb1111-2222-3333-4444-555555555555"
	sess := db.Session{
		InstanceID:  instanceID,
		SessionName: "nixos-config@main",
		Repo:        "nixos-config",
		Worktree:    "/tmp/w",
		Harness:     "pi",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=detail&session=nixos-config%40main", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp struct {
		Session *db.Session `json:"session"`
	}
	decodeJSONBody(t, rr, &resp)
	if resp.Session == nil {
		t.Fatal("session field is nil, want non-nil")
	}
	if resp.Session.SessionName != "nixos-config@main" {
		t.Errorf("session name = %q, want nixos-config@main", resp.Session.SessionName)
	}
}

// TestHostAPI_Stats_Detail_MissingSession returns 400 when session param is absent.
func TestHostAPI_Stats_Detail_MissingSession(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=detail", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_Detail_UnknownSession returns 404 when the session is not found.
func TestHostAPI_Stats_Detail_UnknownSession(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=detail&session=nixos-config%40ghost", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q, want 404", rr.Code, rr.Body.String())
	}
}

// ── error cases ───────────────────────────────────────────────────────────────

// TestHostAPI_Stats_UnknownView returns 400 for an unknown view parameter.
func TestHostAPI_Stats_UnknownView(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=badview", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}

	var errResp struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, rr, &errResp)
	if errResp.Error == "" {
		t.Error("expected non-empty error field")
	}
}

// TestHostAPI_Stats_MethodNotAllowed verifies that POST /stats returns 405.
func TestHostAPI_Stats_MethodNotAllowed(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/stats", `{}`)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body = %q, want 405", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_WorkerCanAccessStats verifies that the /stats endpoint
// is accessible to worker sessions (read-only, no coordinator restriction).
func TestHostAPI_Stats_WorkerCanAccessStats(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "nixos-config@feature", "nixos-config", "worker", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=doomloops&days=7", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("worker should be able to access /stats: status = %d, body = %q", rr.Code, rr.Body.String())
	}
}

// ── JSON schema stability ─────────────────────────────────────────────────────

// TestHostAPI_Stats_Doomloops_JSONSchema verifies the JSON schema for
// view=doomloops: the "events" key must be present and contain Event objects
// with at least ID, SessionName, Type, and CreatedAt fields.
func TestHostAPI_Stats_Doomloops_JSONSchema(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("nixos-config@main", "nixos-config", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	writeDoomLoopEventForSidecar(t, d, "nixos-config@main", "bash", "git diff", 3, time.Now().Add(-30*time.Minute))

	sc := newSidecarWithRole(t, "nixos-config@main", "nixos-config", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=doomloops&days=1", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	eventsRaw, ok := resp["events"]
	if !ok {
		t.Fatal("response missing 'events' key")
	}

	var events []map[string]json.RawMessage
	if err := json.Unmarshal(eventsRaw, &events); err != nil {
		t.Fatalf("unmarshal events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least 1 event")
	}

	for _, field := range []string{"ID", "SessionName", "Type", "CreatedAt"} {
		if _, has := events[0][field]; !has {
			t.Errorf("event missing field %q", field)
		}
	}
}
