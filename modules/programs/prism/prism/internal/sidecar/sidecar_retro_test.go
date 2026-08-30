package sidecar

// Tests for the host-API GET /retro endpoint. The endpoint runs
// db.AssembleRetro and returns the report JSON. It is all-roles read, matching
// /stats — a worker session must be able to read it inside a sandbox. The shape
// mirrors the /stats tests in sidecar_stats_test.go.

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedRetroSessionForSidecar inserts a terminal sessions row plus one
// msg_assistant event carrying token counts, so CompareRunOutcome computes an
// outcome for it.
func seedRetroSessionForSidecar(t *testing.T, d *db.DB, name, repo string, started time.Time, output, cacheRead int) {
	t.Helper()
	iid := uuid.New().String()
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: name,
		Repo:        repo,
		Worktree:    "/wt/" + name,
		Harness:     "pi",
		StartedAt:   started,
	}); err != nil {
		t.Fatalf("InsertSession %q: %v", name, err)
	}
	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded %q: %v", name, err)
	}
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: name,
		Repo:        repo,
		Worktree:    "/wt/" + name,
		InstanceID:  &iid,
		Type:        "msg_assistant",
		Payload:     `{"inputTokens":0,"outputTokens":` + itoaSidecar(output) + `,"cacheReadTokens":` + itoaSidecar(cacheRead) + `,"cacheWriteTokens":0,"cost":0}`,
		CreatedAt:   started,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent %q: %v", name, err)
	}
}

func itoaSidecar(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestHostAPI_Retro_HappyPath verifies GET /retro returns the assembled report
// with the seeded token totals.
func TestHostAPI_Retro_HappyPath(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()

	seedRetroSessionForSidecar(t, d, "test-repo@feature", "test-repo", now.Add(-1*time.Hour), 200, 1000)

	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/retro?repo=test-repo&since="+itoaSidecar(int(sinceMs)), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	var report db.RetroReport
	decodeJSONBody(t, rr, &report)

	if len(report.Trains) != 1 {
		t.Fatalf("got %d trains, want 1: %+v", len(report.Trains), report.Trains)
	}
	if report.Trains[0].Root != "test-repo@feature" {
		t.Errorf("train root = %q, want test-repo@feature", report.Trains[0].Root)
	}
	if report.WindowTotals.OutputTokens != 200 || report.WindowTotals.CacheReadTokens != 1000 {
		t.Errorf("window totals = %+v", report.WindowTotals)
	}
	if !report.WasteSignals.Available {
		t.Error("waste signals should be available (an outcome row exists)")
	}
}

// TestHostAPI_Retro_AllRolesRead verifies GET /retro is not gated behind a
// coordinator role — a worker session inside a sandbox must read it (AC: no
// host-DB error inside a sandbox).
func TestHostAPI_Retro_AllRolesRead(t *testing.T) {
	d := openTestDB(t)
	sinceMs := time.Now().Add(-24 * time.Hour).UnixMilli()

	sc := newSidecarWithRole(t, "test-repo@feature", "test-repo", "worker", d)

	rr := doHostAPI(t, sc, http.MethodGet, "/retro?repo=test-repo&since="+itoaSidecar(int(sinceMs)), "")
	if rr.Code != http.StatusOK {
		t.Fatalf("worker read status = %d, body = %q, want 200 (all-roles read)", rr.Code, rr.Body.String())
	}
	var report db.RetroReport
	decodeJSONBody(t, rr, &report)
	if report.Trains == nil {
		t.Error("Trains must be a non-nil slice (marshals as [])")
	}
}

// TestHostAPI_Retro_RejectsPost verifies the endpoint is GET-only.
func TestHostAPI_Retro_RejectsPost(t *testing.T) {
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "test-repo@main", "test-repo", "coordinator", d)
	rr := doHostAPI(t, sc, http.MethodPost, "/retro", "")
	if rr.Code == http.StatusOK {
		t.Errorf("POST /retro returned 200, want a method-not-allowed rejection")
	}
}
