package sidecar

// Tests for the host-API /merge, /merges, and /merges/cancel endpoints
// added in #1043 to fix the bwrap shadow-DB issue.
//
// These tests exercise the hostAPIHandler() method directly without spinning
// up a real Unix socket server. The shape mirrors the existing /spawn,
// /cleanup, /prompt tests in sidecar_test.go.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	opencode "github.com/prismatic-koi/prism/internal/harness/opencode"
)

// newSidecarCoordinatorWithInstance builds a coordinator sidecar with a
// non-empty InstanceID so the merge endpoints have an identity to enqueue
// rows under. Mirrors newSidecarWithRole but adds the InstanceID field.
func newSidecarCoordinatorWithInstance(t *testing.T, sessionName, repo, instanceID string, d *db.DB) *Sidecar {
	t.Helper()
	clk := newTestClock()
	cfg := Config{
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    "/tmp/" + sessionName,
		OpencodeURL: "http://localhost:14000",
		DB:          d,
		Clock:       clk,
		AgentRole:   "coordinator",
		InstanceID:  instanceID,
		Harness:     opencode.New("http://localhost:14000", nil, "coordinator", ""),
	}
	// Seed the DB so isCoordinatorSession() recognises this session as a
	// coordinator (the merge handlers go through requireCoordinator).
	if err := d.UpsertStatusSeedRootAgentName(sessionName, repo, "/tmp/"+sessionName, "active", nil, nil, "coordinator"); err != nil {
		t.Fatalf("seed coordinator status: %v", err)
	}
	return New(cfg)
}

// ── /merge ────────────────────────────────────────────────────────────────────

// TestHostAPI_Merge_EnqueuesRowWithSidecarIdentity is the headline test for the
// fix in #1043. It verifies that when /merge is called with just the PR number
// and title, the resulting pending_merges row uses the sidecar's own
// session_name and instance_id — the values the watcher queries against — so
// the watcher will find the row on its next tick (AC #7).
func TestHostAPI_Merge_EnqueuesRowWithSidecarIdentity(t *testing.T) {
	d := openTestDB(t)
	const (
		sess     = "nixos-config@main"
		instance = "11111111-2222-3333-4444-555555555555"
	)
	sc := newSidecarCoordinatorWithInstance(t, sess, "nixos-config", instance, d)

	rr := doHostAPI(t, sc, http.MethodPost, "/merge",
		`{"pr": 1234, "title": "do the mahi"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp db.PendingMerge
	decodeJSONBody(t, rr, &resp)

	if resp.PR != 1234 {
		t.Errorf("response PR = %d, want 1234", resp.PR)
	}
	if resp.SessionName != sess {
		t.Errorf("response SessionName = %q, want %q (the sidecar's session_name is what the watcher queries against)", resp.SessionName, sess)
	}
	if resp.InstanceID != instance {
		t.Errorf("response InstanceID = %q, want %q (the sidecar's instance_id is what the watcher queries against)", resp.InstanceID, instance)
	}
	if resp.Status != "watching" {
		t.Errorf("response Status = %q, want %q", resp.Status, "watching")
	}

	// Confirm the row landed in the host-side DB with the expected identity —
	// this is the actual fix for the shadow-DB bug.
	row, err := d.PendingMergeByPR(1234)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row == nil {
		t.Fatal("PendingMergeByPR(1234) = nil — row was not persisted")
	}
	if row.SessionName != sess || row.InstanceID != instance {
		t.Errorf("DB row identity (session=%q, instance=%q), want (session=%q, instance=%q)",
			row.SessionName, row.InstanceID, sess, instance)
	}
}

// TestHostAPI_Merge_ClientIdentityIsIgnored verifies that the proxy contract
// is "trust the sidecar, not the client": the request body intentionally has
// no session_name / instance_id field, so even if a misbehaving client tried
// to set them, the sidecar would still use its own values. (Asserted
// indirectly by passing an arbitrary title and verifying that DB identity
// matches the sidecar config.)
func TestHostAPI_Merge_ClientIdentityIsIgnored(t *testing.T) {
	d := openTestDB(t)
	const (
		sess     = "nixos-config@main"
		instance = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	sc := newSidecarCoordinatorWithInstance(t, sess, "nixos-config", instance, d)

	// Body contains stray fields that are not part of the schema; they must
	// be ignored.
	rr := doHostAPI(t, sc, http.MethodPost, "/merge",
		`{"pr": 99, "title": null, "session_name": "evil@spoofed", "instance_id": "spoofed"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	row, err := d.PendingMergeByPR(99)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row == nil || row.SessionName != sess || row.InstanceID != instance {
		t.Errorf("row identity = (%v, %v), want (%q, %q) — sidecar must ignore client-supplied identity",
			row.SessionName, row.InstanceID, sess, instance)
	}
}

func TestHostAPI_Merge_WorkerForbidden(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@feature", "myrepo", "worker", d)
	rr := doHostAPI(t, sc, http.MethodPost, "/merge", `{"pr": 1, "title": null}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q, want 403", rr.Code, rr.Body.String())
	}
}

func TestHostAPI_Merge_RejectsInvalidPR(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarCoordinatorWithInstance(t, "nixos-config@main", "nixos-config", "i1", d)

	for _, body := range []string{
		`{"pr": 0, "title": null}`,
		`{"pr": -1, "title": null}`,
		`{"title": "no pr"}`,
	} {
		rr := doHostAPI(t, sc, http.MethodPost, "/merge", body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rr.Code)
		}
	}
}

func TestHostAPI_Merge_NoInstanceIDIsServerError(t *testing.T) {
	d := openTestDB(t)
	// Coordinator sidecar with no InstanceID configured — we should refuse to
	// enqueue rather than write a row keyed on an empty instance_id (which
	// would be invisible to a watcher that has a real instance_id).
	sc := newSidecarCoordinatorWithInstance(t, "nixos-config@main", "nixos-config", "", d)
	rr := doHostAPI(t, sc, http.MethodPost, "/merge", `{"pr": 5, "title": null}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for missing instance_id", rr.Code)
	}
}

func TestHostAPI_Merge_RejectsNonPOST(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarCoordinatorWithInstance(t, "nixos-config@main", "nixos-config", "i1", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/merge", "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

// ── /merges ───────────────────────────────────────────────────────────────────

func TestHostAPI_Merges_ListWatching(t *testing.T) {
	d := openTestDB(t)
	const (
		sess     = "nixos-config@main"
		instance = "deadbeef-1111-2222-3333-444444444444"
	)
	sc := newSidecarCoordinatorWithInstance(t, sess, "nixos-config", instance, d)

	// Seed a couple of watching rows directly via the DB.
	t1 := "first"
	t2 := "second"
	if _, err := d.EnqueueMerge(101, sess, instance, &t1); err != nil {
		t.Fatalf("seed EnqueueMerge: %v", err)
	}
	if _, err := d.EnqueueMerge(102, sess, instance, &t2); err != nil {
		t.Fatalf("seed EnqueueMerge: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodGet, "/merges", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var rows []db.PendingMerge
	decodeJSONBody(t, rr, &rows)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2: %+v", len(rows), rows)
	}
	prs := map[int]bool{}
	for _, r := range rows {
		prs[r.PR] = true
	}
	if !prs[101] || !prs[102] {
		t.Errorf("got PRs = %v, want both 101 and 102", prs)
	}
}

func TestHostAPI_Merges_EmptyArrayWhenEmpty(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarCoordinatorWithInstance(t, "nixos-config@main", "nixos-config", "i1", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/merges", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	// Body must parse as a JSON array (not null), even when empty — clients
	// rely on this to render "queue is empty" rather than crashing.
	var raw json.RawMessage
	decodeJSONBody(t, rr, &raw)
	if string(raw) != "[]" {
		t.Errorf("body = %q, want []", string(raw))
	}
}

func TestHostAPI_Merges_WorkerForbidden(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@feature", "myrepo", "worker", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/merges", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

// ── /merges/cancel ────────────────────────────────────────────────────────────

func TestHostAPI_MergesCancel_HappyPath(t *testing.T) {
	d := openTestDB(t)
	const (
		sess     = "nixos-config@main"
		instance = "11111111-1111-1111-1111-111111111111"
	)
	sc := newSidecarCoordinatorWithInstance(t, sess, "nixos-config", instance, d)

	if _, err := d.EnqueueMerge(77, sess, instance, nil); err != nil {
		t.Fatalf("seed EnqueueMerge: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodPost, "/merges/cancel", `{"pr": 77}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var resp struct {
		Cancelled bool             `json:"cancelled"`
		Row       *db.PendingMerge `json:"row"`
	}
	decodeJSONBody(t, rr, &resp)
	if !resp.Cancelled {
		t.Errorf("response cancelled = false, want true")
	}

	// DB row should be in 'cancelled' state.
	row, err := d.PendingMergeByPR(77)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row == nil || row.Status != "cancelled" {
		t.Errorf("row status after cancel = %v, want cancelled", row)
	}
}

func TestHostAPI_MergesCancel_NonExistentReturnsFalse(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarCoordinatorWithInstance(t, "nixos-config@main", "nixos-config", "i1", d)

	rr := doHostAPI(t, sc, http.MethodPost, "/merges/cancel", `{"pr": 9999}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for no-op cancel", rr.Code)
	}
	var resp struct {
		Cancelled bool             `json:"cancelled"`
		Row       *db.PendingMerge `json:"row"`
	}
	decodeJSONBody(t, rr, &resp)
	if resp.Cancelled {
		t.Error("cancelled = true, want false for non-existent PR")
	}
	if resp.Row != nil {
		t.Errorf("row = %+v, want nil for non-existent PR", resp.Row)
	}
}

func TestHostAPI_MergesCancel_DifferentInstanceReturnsRowWatching(t *testing.T) {
	d := openTestDB(t)
	const (
		sess          = "nixos-config@main"
		ourInstance   = "instance-A"
		theirInstance = "instance-B"
	)
	sc := newSidecarCoordinatorWithInstance(t, sess, "nixos-config", ourInstance, d)

	// Seed a row owned by a DIFFERENT incarnation.
	if _, err := d.EnqueueMerge(55, sess, theirInstance, nil); err != nil {
		t.Fatalf("seed EnqueueMerge: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodPost, "/merges/cancel", `{"pr": 55}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Cancelled bool             `json:"cancelled"`
		Row       *db.PendingMerge `json:"row"`
	}
	decodeJSONBody(t, rr, &resp)
	if resp.Cancelled {
		t.Error("cancelled = true, want false (owned by different incarnation)")
	}
	if resp.Row == nil {
		t.Fatal("row = nil, want existing watching row so client can render correct message")
	}
	if resp.Row.Status != "watching" {
		t.Errorf("row.Status = %q, want watching", resp.Row.Status)
	}
}

func TestHostAPI_MergesCancel_WorkerForbidden(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "myrepo@feature", "myrepo", "worker", d)
	rr := doHostAPI(t, sc, http.MethodPost, "/merges/cancel", `{"pr": 1}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestHostAPI_MergesCancel_RejectsInvalidPR(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarCoordinatorWithInstance(t, "nixos-config@main", "nixos-config", "i1", d)
	rr := doHostAPI(t, sc, http.MethodPost, "/merges/cancel", `{"pr": 0}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
