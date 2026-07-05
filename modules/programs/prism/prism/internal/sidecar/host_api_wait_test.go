package sidecar

// Tests for the three read-only wait-probe endpoints (#1500):
//
//   GET /merges/by-pr      — single pending_merges row lookup
//   GET /sessions/status   — single agent_status row lookup
//   GET /groups/poll       — group completion + members + results
//
// These exist so the in-sandbox `--wait` poll loops can read host-side
// terminal state through the host-API rather than opening a shadow DB.
// Each endpoint must return 404 for missing rows (so the CLI can keep
// polling) and 200 with stable JSON shapes for present rows.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// ── /merges/by-pr ─────────────────────────────────────────────────────────────

func TestHostAPI_MergesByPR_ReturnsRow(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.EnqueueMerge(42, "repo", "repo@main", "inst-1", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(42, "repo", "merged", ""); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/merges/by-pr?pr=42", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var row db.PendingMerge
	decodeJSONBody(t, rr, &row)
	if row.PR != 42 {
		t.Errorf("pr: want 42, got %d", row.PR)
	}
	if row.Status != "merged" {
		t.Errorf("status: want merged, got %q", row.Status)
	}
}

func TestHostAPI_MergesByPR_ReturnsNotFoundWhenAbsent(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/merges/by-pr?pr=12345", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHostAPI_MergesByPR_RejectsMissingPR(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/merges/by-pr", "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHostAPI_MergesByPR_RejectsBadPR(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/merges/by-pr?pr=foo", "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for non-numeric pr; body = %s", rr.Code, rr.Body.String())
	}
}

// ── /sessions/status ──────────────────────────────────────────────────────────

func TestHostAPI_SessionsStatus_ReturnsRow(t *testing.T) {
	d := openTestDB(t)
	if err := d.UpsertStatus("repo@feature", "repo", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/sessions/status?session=repo@feature", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var st db.Status
	decodeJSONBody(t, rr, &st)
	if st.SessionName != "repo@feature" {
		t.Errorf("session: want repo@feature, got %q", st.SessionName)
	}
	if st.State != "finished" {
		t.Errorf("state: want finished, got %q", st.State)
	}
}

func TestHostAPI_SessionsStatus_ReturnsNotFoundWhenAbsent(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/sessions/status?session=does-not-exist", "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
}

// ── /groups/poll ──────────────────────────────────────────────────────────────

func TestHostAPI_GroupsPoll_CompletedTrueAfterAllTerminal(t *testing.T) {
	d := openTestDB(t)
	groupID, err := d.RegisterGroup("repo@pr-1500")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	const sess = "repo@pr-1500~review-1-review-goal"
	if err := d.UpsertStatusSeedRootAgentName(sess, "repo", "/wt", "finished", nil, nil, "review-goal", "", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	// Link to group via raw SQL (mirrors the pattern in db_test.go).
	var one int
	if err := d.QueryRow(
		"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
		groupID, sess,
	).Scan(&one); err != nil {
		t.Fatalf("link group_id: %v", err)
	}
	// Seed a msg_assistant event so GroupResults has output to surface.
	if err := d.WriteEvent(db.Event{
		ID:          "evt-1",
		SessionName: sess,
		Repo:        "repo",
		Worktree:    "/wt",
		Type:        "msg_assistant",
		Payload:     `{"text":"<verdict>PASS</verdict>"}`,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	sc := newSidecarWithRole(t, "repo@pr-1500", "repo", "worker", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/groups/poll?group_id="+groupID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Completed bool                            `json:"completed"`
		Members   []db.Status                     `json:"members"`
		Results   map[string]db.GroupMemberResult `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rr.Body.String())
	}
	if !resp.Completed {
		t.Errorf("completed: want true, got false")
	}
	if len(resp.Members) != 1 || resp.Members[0].SessionName != sess {
		t.Errorf("members: want [%q], got %v", sess, resp.Members)
	}
	if mr, ok := resp.Results[sess]; !ok || mr.State != "finished" {
		t.Errorf("results[%q]: want state=finished, got %v", sess, mr)
	}
}

func TestHostAPI_GroupsPoll_RejectsMissingGroupID(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "worker", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/groups/poll", "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

// ── /groups/list ──────────────────────────────────────────────────────────────

// TestHostAPI_GroupsList_ReturnsGroupsNewestFirst exercises the new
// /groups/list endpoint added in #1500 round-3 to fix the shadow-DB
// regression for `prism reviews list` from inside a sandbox.
func TestHostAPI_GroupsList_ReturnsGroupsNewestFirst(t *testing.T) {
	d := openTestDB(t)
	// Two groups; ReviewGroupsList sorts by created_at DESC (newest
	// first). Both register on the same DATETIME tick so we cannot
	// assert ordering precisely — but we can assert both rows are
	// returned and carry the right parent.
	g1, err := d.RegisterGroup("repo@pr-1")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	g2, err := d.RegisterGroup("repo@pr-2")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/groups/list", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var groups []db.ReviewGroupSummary
	decodeJSONBody(t, rr, &groups)
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d: %v", len(groups), groups)
	}
	seen := map[string]string{}
	for _, g := range groups {
		seen[g.GroupID] = g.ParentSession
	}
	if seen[g1] != "repo@pr-1" {
		t.Errorf("group %s: want parent repo@pr-1, got %q", g1, seen[g1])
	}
	if seen[g2] != "repo@pr-2" {
		t.Errorf("group %s: want parent repo@pr-2, got %q", g2, seen[g2])
	}
}

func TestHostAPI_GroupsList_EmptyReturnsEmptyArray(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/groups/list", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	// Must be `[]` literally, not `null` (the empty-list contract that
	// prism CLI consumers depend on across `--json` paths).
	if strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Errorf("empty groups list: want '[]', got %q", rr.Body.String())
	}
}

func TestHostAPI_GroupsList_RejectsBadLimit(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodGet, "/groups/list?limit=abc", "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_WaitProbeEndpoints_RejectPost ensures the wait-probe endpoints
// only accept GET (read-only). A misconfigured client should not be able to
// POST through them and silently change state.
func TestHostAPI_WaitProbeEndpoints_RejectPost(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "repo@main", "repo", "coordinator", d)
	for _, path := range []string{"/merges/by-pr?pr=1", "/sessions/status?session=x", "/groups/poll?group_id=x", "/groups/list"} {
		rr := doHostAPI(t, sc, http.MethodPost, path, "{}")
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status = %d, want 405; body = %s", path, rr.Code, rr.Body.String())
		}
	}
}
