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
	if err := d.UpsertStatus("test-repo@main", "test-repo", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	writeDoomLoopEventForSidecar(t, d, "test-repo@main", "bash", "git status", 5, time.Now().Add(-1*time.Hour))

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

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
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

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

	if err := d.UpsertStatus("test-repo@main", "test-repo", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	writePermissionEventForSidecar(t, d, "test-repo@main", "permission_denied", "bash", time.Now().Add(-1*time.Hour))

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

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
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=denials&session=test-repo%40ghost&days=7", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q, want 404", rr.Code, rr.Body.String())
	}
}

// ── GET /stats?view=asks ──────────────────────────────────────────────────────

// TestHostAPI_Stats_Asks_HappyPath verifies that GET /stats?view=asks
// returns permission_ask events.
func TestHostAPI_Stats_Asks_HappyPath(t *testing.T) {
	d := openTestDB(t)

	if err := d.UpsertStatus("test-repo@main", "test-repo", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	writePermissionEventForSidecar(t, d, "test-repo@main", "permission_ask", "bash", time.Now().Add(-1*time.Hour))

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

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
		SessionName: "test-repo@main",
		Repo:        "test-repo",
		Worktree:    "/tmp/w",
		Harness:     "pi",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

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
	if resp.Sessions[0].SessionName != "test-repo@main" {
		t.Errorf("session name = %q, want test-repo@main", resp.Sessions[0].SessionName)
	}
}

// TestHostAPI_Stats_Summary_EmptyResult verifies that an empty sessions table
// returns {"sessions":[]} (not null).
func TestHostAPI_Stats_Summary_EmptyResult(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

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
		SessionName: "test-repo@main",
		Repo:        "test-repo",
		Worktree:    "/tmp/w",
		Harness:     "pi",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=detail&session=test-repo%40main", "")
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
	if resp.Session.SessionName != "test-repo@main" {
		t.Errorf("session name = %q, want test-repo@main", resp.Session.SessionName)
	}
}

// TestHostAPI_Stats_Detail_MissingSession returns 400 when session param is absent.
func TestHostAPI_Stats_Detail_MissingSession(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=detail", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_Detail_UnknownSession returns 404 when the session is not found.
func TestHostAPI_Stats_Detail_UnknownSession(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=detail&session=test-repo%40ghost", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q, want 404", rr.Code, rr.Body.String())
	}
}

// ── error cases ───────────────────────────────────────────────────────────────

// TestHostAPI_Stats_UnknownView returns 400 for an unknown view parameter.
func TestHostAPI_Stats_UnknownView(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

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
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/stats", `{}`)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body = %q, want 405", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_WorkerCanAccessStats verifies that the /stats endpoint
// is accessible to worker sessions (read-only, no coordinator restriction).
func TestHostAPI_Stats_WorkerCanAccessStats(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "nixos-config@feature", "test-repo", "worker", d)

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

	if err := d.UpsertStatus("test-repo@main", "test-repo", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	writeDoomLoopEventForSidecar(t, d, "test-repo@main", "bash", "git diff", 3, time.Now().Add(-30*time.Minute))

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)
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

// ── GET /stats?view=compare / abtest / abtest_list (#2098) ────────────────────
//
// These exercise the host-API views that back `prism stats compare`,
// `prism stats abtest <group>`, and `prism stats --abtest` from sandboxed
// sessions. The handler reuses the same db helpers as the CLI direct path
// (db.ResolveSessionArg, db.AssembleCompareRun, db.AbtestGroupSessions,
// db.AbtestPairsAll) so the CLI renders byte-identical output on both paths.

// seedCompareSessionForSidecar seeds the rows the compare/abtest views read:
// agent_status (finished), sessions (terminal end_state), and a spawn_inputs
// row optionally carrying an abtest_pair_id.
func seedCompareSessionForSidecar(t *testing.T, d *db.DB, sessionName, instanceID, pairID string) {
	t.Helper()
	if err := d.UpsertStatus(sessionName, "test-repo", "/tmp/w", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus %q: %v", sessionName, err)
	}
	if err := d.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID %q: %v", sessionName, err)
	}
	sess := db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "test-repo",
		Worktree:    "/tmp/w",
		Harness:     "pi",
		StartedAt:   time.Now().Add(-2 * time.Minute),
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession %q: %v", sessionName, err)
	}
	if err := d.UpdateSessionEnded(instanceID, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded %q: %v", sessionName, err)
	}
	si := db.SpawnInputs{InstanceID: instanceID, CreatedAt: time.Now().UnixMilli()}
	if pairID != "" {
		si.AbtestPairID = &pairID
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs %q: %v", sessionName, err)
	}
}

// TestHostAPI_Stats_Compare_HappyPath verifies that GET
// /stats?view=compare&id=A&id=B returns one run per id, in request order.
func TestHostAPI_Stats_Compare_HappyPath(t *testing.T) {
	d := openTestDB(t)
	idA := "aaaa1111-2222-3333-4444-555555555555"
	idB := "bbbb1111-2222-3333-4444-555555555555"
	seedCompareSessionForSidecar(t, d, "test-repo@run-a", idA, "")
	seedCompareSessionForSidecar(t, d, "test-repo@run-b", idB, "")

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=compare&id="+idA+"&id="+idB, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp struct {
		Runs []db.CompareRunData `json:"runs"`
	}
	decodeJSONBody(t, rr, &resp)
	if len(resp.Runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(resp.Runs))
	}
	if resp.Runs[0].Session == nil || resp.Runs[0].Session.InstanceID != idA {
		t.Errorf("run[0] instance = %v, want %s", resp.Runs[0].Session, idA)
	}
	if resp.Runs[1].Session == nil || resp.Runs[1].Session.InstanceID != idB {
		t.Errorf("run[1] instance = %v, want %s", resp.Runs[1].Session, idB)
	}
}

// TestHostAPI_Stats_Compare_UnknownID returns 404 when any id fails to resolve
// (atomic — no partial runs are returned).
func TestHostAPI_Stats_Compare_UnknownID(t *testing.T) {
	d := openTestDB(t)
	idA := "aaaa1111-2222-3333-4444-555555555555"
	seedCompareSessionForSidecar(t, d, "test-repo@run-a", idA, "")

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=compare&id="+idA+"&id=cccc1111-2222-3333-4444-555555555555", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q, want 404", rr.Code, rr.Body.String())
	}
	var errResp struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, rr, &errResp)
	if errResp.Error == "" {
		t.Error("expected non-empty error field for unknown id")
	}
}

// TestHostAPI_Stats_Compare_TooFewIDs returns 400 when fewer than 2 ids are
// supplied (compare requires at least two runs).
func TestHostAPI_Stats_Compare_TooFewIDs(t *testing.T) {
	d := openTestDB(t)
	idA := "aaaa1111-2222-3333-4444-555555555555"
	seedCompareSessionForSidecar(t, d, "test-repo@run-a", idA, "")

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=compare&id="+idA, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_Compare_WorkerAllowed verifies the compare view honours
// the /stats all-roles policy (workers may read it).
func TestHostAPI_Stats_Compare_WorkerAllowed(t *testing.T) {
	d := openTestDB(t)
	idA := "aaaa1111-2222-3333-4444-555555555555"
	idB := "bbbb1111-2222-3333-4444-555555555555"
	seedCompareSessionForSidecar(t, d, "test-repo@run-a", idA, "")
	seedCompareSessionForSidecar(t, d, "test-repo@run-b", idB, "")

	sc := newSidecarWithRole(t, "test-repo@feature", "test-repo", "worker", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=compare&id="+idA+"&id="+idB, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("worker should access view=compare: status = %d, body = %q", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_Abtest_HappyPath verifies that GET /stats?view=abtest&group=<id>
// resolves group members and returns one run per member.
func TestHostAPI_Stats_Abtest_HappyPath(t *testing.T) {
	d := openTestDB(t)
	idA := "aaaa1111-2222-3333-4444-555555555555"
	idB := "bbbb1111-2222-3333-4444-555555555555"
	seedCompareSessionForSidecar(t, d, "test-repo@run-a", idA, "")
	seedCompareSessionForSidecar(t, d, "test-repo@run-b", idB, "")

	groupID, err := d.RegisterGroup("test-repo@main")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if err := d.SetGroupID("test-repo@run-a", groupID); err != nil {
		t.Fatalf("SetGroupID run-a: %v", err)
	}
	if err := d.SetGroupID("test-repo@run-b", groupID); err != nil {
		t.Fatalf("SetGroupID run-b: %v", err)
	}

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=abtest&group="+groupID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp struct {
		Runs []db.CompareRunData `json:"runs"`
	}
	decodeJSONBody(t, rr, &resp)
	if len(resp.Runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(resp.Runs))
	}
	// Sorted by session_name: run-a before run-b.
	if resp.Runs[0].Session.SessionName != "test-repo@run-a" {
		t.Errorf("run[0] = %q, want test-repo@run-a (sorted)", resp.Runs[0].Session.SessionName)
	}
}

// TestHostAPI_Stats_Abtest_MissingGroup returns 400 when no group param is given.
func TestHostAPI_Stats_Abtest_MissingGroup(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=abtest", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_Abtest_UnknownGroup returns 404 when the group has no members.
func TestHostAPI_Stats_Abtest_UnknownGroup(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=abtest&group=no-such-group", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q, want 404", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_Stats_AbtestList_HappyPath verifies that GET /stats?view=abtest_list
// returns the recorded A/B pairs.
func TestHostAPI_Stats_AbtestList_HappyPath(t *testing.T) {
	d := openTestDB(t)
	idA := "aaaa1111-2222-3333-4444-555555555555"
	idB := "bbbb1111-2222-3333-4444-555555555555"
	pairID := "pair-1111-2222-3333-4444-555555555555"
	seedCompareSessionForSidecar(t, d, "test-repo@run-a", idA, pairID)
	seedCompareSessionForSidecar(t, d, "test-repo@run-b", idB, pairID)

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=abtest_list", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp struct {
		Pairs []db.AbtestPairRow `json:"pairs"`
	}
	decodeJSONBody(t, rr, &resp)
	if len(resp.Pairs) != 1 {
		t.Fatalf("got %d pairs, want 1", len(resp.Pairs))
	}
	if resp.Pairs[0].PairID != pairID {
		t.Errorf("pair id = %q, want %q", resp.Pairs[0].PairID, pairID)
	}
}

// TestHostAPI_Stats_AbtestList_Empty verifies an empty result returns
// {"pairs":[]} (not null).
func TestHostAPI_Stats_AbtestList_Empty(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/stats?view=abtest_list", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp struct {
		Pairs []db.AbtestPairRow `json:"pairs"`
	}
	decodeJSONBody(t, rr, &resp)
	if resp.Pairs == nil {
		t.Error("pairs field should be an empty array, not null")
	}
}
