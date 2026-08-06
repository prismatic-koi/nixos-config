package db_test

// Tests for AssembleRetro — the single data-assembly behind `prism retro`
// (issue #2583). These cover train grouping (worker + review children,
// coordinator + investigators, solo investigators, A/B legs), window totals
// with the cache-read share, waste signals (available/zero/unavailable), and
// the live-session and empty-window edge cases.

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

const retroRepo = "retrorepo"

// retroTokens carries the per-session token counts a seeded session records.
type retroTokens struct {
	input, output, cacheRead, cacheWrite int
}

// retroWaste carries the per-session waste-event counts a seeded session
// records.
type retroWaste struct {
	doomLoops, permAsks, permDenials, toolErrors int
}

// seedRetroSession inserts a sessions row (and, for terminal states, marks it
// ended so CompareRunOutcome computes an outcome from the seeded agent_events).
// It writes one msg_assistant event carrying the token counts, plus one event
// per waste occurrence. A live (non-terminal) session is left with no
// end_state, so CompareRunOutcome returns nil for it.
func seedRetroSession(t *testing.T, d *db.DB, name string, started time.Time, groupID, parent *string, live bool, tok retroTokens, waste retroWaste) string {
	t.Helper()
	instanceID := uuid.New().String()
	if err := d.InsertSession(db.Session{
		InstanceID:    instanceID,
		SessionName:   name,
		Repo:          retroRepo,
		Worktree:      "/wt/" + name,
		Harness:       "pi",
		GroupID:       groupID,
		ParentSession: parent,
		StartedAt:     started,
	}); err != nil {
		t.Fatalf("InsertSession %q: %v", name, err)
	}
	if !live {
		if err := d.UpdateSessionEnded(instanceID, "finished"); err != nil {
			t.Fatalf("UpdateSessionEnded %q: %v", name, err)
		}
	}

	iid := instanceID
	writeRetroEvent(t, d, name, iid, "msg_assistant", started,
		tokenPayload(tok))
	for i := 0; i < waste.doomLoops; i++ {
		writeRetroEvent(t, d, name, iid, "doom_loop_detected", started, `{"tool":"bash"}`)
	}
	for i := 0; i < waste.permAsks; i++ {
		writeRetroEvent(t, d, name, iid, "permission_ask", started, `{"tool":"bash"}`)
	}
	for i := 0; i < waste.permDenials; i++ {
		writeRetroEvent(t, d, name, iid, "permission_denied", started, `{"tool":"bash"}`)
	}
	for i := 0; i < waste.toolErrors; i++ {
		writeRetroEvent(t, d, name, iid, "tool_error", started, `{"tool":"bash"}`)
	}
	return instanceID
}

func tokenPayload(tok retroTokens) string {
	return `{"inputTokens":` + strconv.Itoa(tok.input) +
		`,"outputTokens":` + strconv.Itoa(tok.output) +
		`,"cacheReadTokens":` + strconv.Itoa(tok.cacheRead) +
		`,"cacheWriteTokens":` + strconv.Itoa(tok.cacheWrite) +
		`,"cost":0}`
}

func writeRetroEvent(t *testing.T, d *db.DB, name, instanceID, typ string, ts time.Time, payload string) {
	t.Helper()
	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: name,
		Repo:        retroRepo,
		Worktree:    "/wt/" + name,
		InstanceID:  &instanceID,
		Type:        typ,
		Payload:     payload,
		CreatedAt:   ts,
	}
	if err := d.WriteEvent(e); err != nil {
		t.Fatalf("WriteEvent %s for %q: %v", typ, name, err)
	}
}

// findTrain returns the train with the given root, or fails the test.
func findTrain(t *testing.T, r *db.RetroReport, root string) db.RetroTrain {
	t.Helper()
	for _, tr := range r.Trains {
		if tr.Root == root {
			return tr
		}
	}
	t.Fatalf("no train with root %q; trains: %+v", root, r.Trains)
	return db.RetroTrain{}
}

// TestAssembleRetro_WorkerTrainRollsUpReviewChildren covers the core train:
// a worker plus its ~review-N-<agent> children roll up into one row, resolved
// through session_groups.parent_session via sessions.group_id.
func TestAssembleRetro_WorkerTrainRollsUpReviewChildren(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()

	worker := "retrorepo@feature"
	seedRetroSession(t, d, worker, now.Add(-3*time.Hour), nil, nil, false,
		retroTokens{input: 100, output: 200, cacheRead: 1000, cacheWrite: 50}, retroWaste{})

	// Two review rounds, each its own session_groups row.
	round1, err := d.RegisterGroup(worker)
	if err != nil {
		t.Fatalf("RegisterGroup round1: %v", err)
	}
	round2, err := d.RegisterGroup(worker)
	if err != nil {
		t.Fatalf("RegisterGroup round2: %v", err)
	}
	seedRetroSession(t, d, worker+"~review-1-review-code", now.Add(-2*time.Hour), &round1, &worker, false,
		retroTokens{output: 10, cacheRead: 500}, retroWaste{})
	seedRetroSession(t, d, worker+"~review-2-review-code", now.Add(-1*time.Hour), &round2, &worker, false,
		retroTokens{output: 20, cacheRead: 300}, retroWaste{})

	r, err := d.AssembleRetro(retroRepo, sinceMs)
	if err != nil {
		t.Fatalf("AssembleRetro: %v", err)
	}
	if len(r.Trains) != 1 {
		t.Fatalf("got %d trains, want 1: %+v", len(r.Trains), r.Trains)
	}
	tr := r.Trains[0]
	if tr.Root != worker {
		t.Errorf("train root = %q, want %q", tr.Root, worker)
	}
	if tr.Kind != "worker" {
		t.Errorf("train kind = %q, want worker", tr.Kind)
	}
	if tr.MemberCount != 3 {
		t.Errorf("member count = %d, want 3 (worker + 2 review)", tr.MemberCount)
	}
	if tr.ReviewCycles != 2 {
		t.Errorf("review cycles = %d, want 2", tr.ReviewCycles)
	}
	// output 200 + 10 + 20; cacheRead 1000 + 500 + 300.
	if tr.OutputTokens != 230 {
		t.Errorf("output tokens = %d, want 230", tr.OutputTokens)
	}
	if tr.CacheReadTokens != 1800 {
		t.Errorf("cache-read tokens = %d, want 1800", tr.CacheReadTokens)
	}
	// total = input 100 + output 230 + cacheRead 1800 + cacheWrite 50 = 2180.
	if tr.TotalTokens != 2180 {
		t.Errorf("total tokens = %d, want 2180", tr.TotalTokens)
	}
	if tr.WindowShare < 0.999 || tr.WindowShare > 1.001 {
		t.Errorf("window share = %f, want ~1.0 (only train)", tr.WindowShare)
	}
}

// TestAssembleRetro_InvestigatorIsSoloTrain covers AC: an investigator whose
// invoker is a worker is a train of one, never attributed to that worker.
func TestAssembleRetro_InvestigatorIsSoloTrain(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()

	worker := "retrorepo@feature"
	investigator := worker + "~investigate-analysis"
	seedRetroSession(t, d, worker, now.Add(-3*time.Hour), nil, nil, false,
		retroTokens{output: 100}, retroWaste{})
	seedRetroSession(t, d, investigator, now.Add(-2*time.Hour), nil, &worker, false,
		retroTokens{output: 50}, retroWaste{})

	r, err := d.AssembleRetro(retroRepo, sinceMs)
	if err != nil {
		t.Fatalf("AssembleRetro: %v", err)
	}
	if len(r.Trains) != 2 {
		t.Fatalf("got %d trains, want 2 (worker + solo investigator): %+v", len(r.Trains), r.Trains)
	}
	inv := findTrain(t, r, investigator)
	if inv.Kind != "investigator" {
		t.Errorf("investigator kind = %q, want investigator", inv.Kind)
	}
	if inv.MemberCount != 1 {
		t.Errorf("investigator member count = %d, want 1", inv.MemberCount)
	}
	w := findTrain(t, r, worker)
	if w.MemberCount != 1 {
		t.Errorf("worker member count = %d, want 1 (investigator must not roll in)", w.MemberCount)
	}
}

// TestAssembleRetro_CoordinatorTrainAbsorbsInvestigators covers AC: a
// coordinator plus the investigators it spawned form one train, separate from
// any worker train.
func TestAssembleRetro_CoordinatorTrainAbsorbsInvestigators(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()

	coord := "retrorepo@main"
	coordInvestigator := coord + "~investigate-triage"
	worker := "retrorepo@feature"

	seedRetroSession(t, d, coord, now.Add(-4*time.Hour), nil, nil, false,
		retroTokens{cacheRead: 5000}, retroWaste{})
	seedRetroSession(t, d, coordInvestigator, now.Add(-3*time.Hour), nil, &coord, false,
		retroTokens{output: 100}, retroWaste{})
	seedRetroSession(t, d, worker, now.Add(-2*time.Hour), nil, nil, false,
		retroTokens{output: 300}, retroWaste{})

	r, err := d.AssembleRetro(retroRepo, sinceMs)
	if err != nil {
		t.Fatalf("AssembleRetro: %v", err)
	}
	if len(r.Trains) != 2 {
		t.Fatalf("got %d trains, want 2 (coordinator + worker): %+v", len(r.Trains), r.Trains)
	}
	c := findTrain(t, r, coord)
	if c.Kind != "coordinator" {
		t.Errorf("coordinator kind = %q, want coordinator", c.Kind)
	}
	if c.MemberCount != 2 {
		t.Errorf("coordinator member count = %d, want 2 (coordinator + investigator)", c.MemberCount)
	}
	w := findTrain(t, r, worker)
	if w.MemberCount != 1 {
		t.Errorf("worker member count = %d, want 1 (coordinator train must not merge in)", w.MemberCount)
	}
}

// TestAssembleRetro_AbtestLegsAreSeparateTrains covers AC: each A/B leg is its
// own train row, not merged with its partner.
func TestAssembleRetro_AbtestLegsAreSeparateTrains(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()

	seedRetroSession(t, d, "retrorepo@ab-a", now.Add(-2*time.Hour), nil, nil, false,
		retroTokens{output: 100}, retroWaste{})
	seedRetroSession(t, d, "retrorepo@ab-b", now.Add(-2*time.Hour), nil, nil, false,
		retroTokens{output: 200}, retroWaste{})

	r, err := d.AssembleRetro(retroRepo, sinceMs)
	if err != nil {
		t.Fatalf("AssembleRetro: %v", err)
	}
	if len(r.Trains) != 2 {
		t.Fatalf("got %d trains, want 2 (both legs): %+v", len(r.Trains), r.Trains)
	}
}

// TestAssembleRetro_WindowTotalsAndCacheReadShare covers AC: window totals
// report cache-read/-write/output volumes and the context-re-read share.
func TestAssembleRetro_WindowTotalsAndCacheReadShare(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()

	seedRetroSession(t, d, "retrorepo@a", now.Add(-2*time.Hour), nil, nil, false,
		retroTokens{input: 100, output: 100, cacheRead: 700, cacheWrite: 100}, retroWaste{})

	r, err := d.AssembleRetro(retroRepo, sinceMs)
	if err != nil {
		t.Fatalf("AssembleRetro: %v", err)
	}
	wt := r.WindowTotals
	if wt.OutputTokens != 100 || wt.CacheReadTokens != 700 || wt.CacheWriteTokens != 100 {
		t.Errorf("window totals = %+v", wt)
	}
	// total = 100+100+700+100 = 1000; cache-read share = 0.7.
	if wt.TotalTokens != 1000 {
		t.Errorf("total tokens = %d, want 1000", wt.TotalTokens)
	}
	if wt.CacheReadShare < 0.699 || wt.CacheReadShare > 0.701 {
		t.Errorf("cache-read share = %f, want 0.7", wt.CacheReadShare)
	}
}

// TestAssembleRetro_WasteSignalsExplicitZeros covers AC: with outcome rows but
// no occurrences, the waste counts render as explicit zeros (Available true).
func TestAssembleRetro_WasteSignalsExplicitZeros(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()

	seedRetroSession(t, d, "retrorepo@clean", now.Add(-1*time.Hour), nil, nil, false,
		retroTokens{output: 100}, retroWaste{})

	r, err := d.AssembleRetro(retroRepo, sinceMs)
	if err != nil {
		t.Fatalf("AssembleRetro: %v", err)
	}
	if !r.WasteSignals.Available {
		t.Fatal("waste signals should be available (an outcome row exists)")
	}
	ws := r.WasteSignals
	if ws.DoomLoopCount != 0 || ws.ToolErrorCount != 0 || ws.PermissionAskCount != 0 || ws.PermissionDeniedCount != 0 {
		t.Errorf("waste counts = %+v, want all zero", ws)
	}
}

// TestAssembleRetro_WasteSignalsCounted covers AC: waste occurrences are summed
// across the window.
func TestAssembleRetro_WasteSignalsCounted(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()

	seedRetroSession(t, d, "retrorepo@noisy", now.Add(-1*time.Hour), nil, nil, false,
		retroTokens{output: 100},
		retroWaste{doomLoops: 2, permAsks: 3, permDenials: 1, toolErrors: 4})

	r, err := d.AssembleRetro(retroRepo, sinceMs)
	if err != nil {
		t.Fatalf("AssembleRetro: %v", err)
	}
	ws := r.WasteSignals
	if !ws.Available {
		t.Fatal("waste signals should be available")
	}
	if ws.DoomLoopCount != 2 || ws.PermissionAskCount != 3 || ws.PermissionDeniedCount != 1 || ws.ToolErrorCount != 4 {
		t.Errorf("waste counts = %+v, want doom=2 ask=3 deny=1 toolerr=4", ws)
	}
}

// TestAssembleRetro_WasteUnavailableWhenNoOutcomes covers AC: when no session
// in the window has an outcome row (all live), the waste section renders as
// unavailable, distinct from a recorded zero.
func TestAssembleRetro_WasteUnavailableWhenNoOutcomes(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()

	// Live session (no end_state) → CompareRunOutcome returns nil → no outcome.
	seedRetroSession(t, d, "retrorepo@live", now.Add(-1*time.Hour), nil, nil, true,
		retroTokens{output: 100}, retroWaste{doomLoops: 5})

	r, err := d.AssembleRetro(retroRepo, sinceMs)
	if err != nil {
		t.Fatalf("AssembleRetro: %v", err)
	}
	if r.WasteSignals.Available {
		t.Error("waste signals must be unavailable when no outcome rows back them")
	}
	if r.WindowTotals.LiveSessionCount != 1 {
		t.Errorf("live session count = %d, want 1", r.WindowTotals.LiveSessionCount)
	}
	// A live session contributes no tokens.
	if r.WindowTotals.TotalTokens != 0 {
		t.Errorf("total tokens = %d, want 0 (live session contributes nothing)", r.WindowTotals.TotalTokens)
	}
}

// TestAssembleRetro_EmptyWindow covers the edge-case AC: an empty window yields
// an empty (non-nil) trains slice and zeroed totals.
func TestAssembleRetro_EmptyWindow(t *testing.T) {
	d := openTestDB(t)
	sinceMs := time.Now().Add(-24 * time.Hour).UnixMilli()

	r, err := d.AssembleRetro(retroRepo, sinceMs)
	if err != nil {
		t.Fatalf("AssembleRetro: %v", err)
	}
	if r.Trains == nil {
		t.Error("Trains must be a non-nil slice (marshals as [])")
	}
	if len(r.Trains) != 0 {
		t.Errorf("got %d trains, want 0", len(r.Trains))
	}
	if r.WindowTotals.SessionCount != 0 {
		t.Errorf("session count = %d, want 0", r.WindowTotals.SessionCount)
	}
	if r.WasteSignals.Available {
		t.Error("waste signals must be unavailable for an empty window")
	}
}

// TestAssembleRetro_RepoScope covers AC: --repo scoping selects only the named
// repo's sessions.
func TestAssembleRetro_RepoScope(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	sinceMs := now.Add(-24 * time.Hour).UnixMilli()

	seedRetroSession(t, d, "retrorepo@a", now.Add(-1*time.Hour), nil, nil, false,
		retroTokens{output: 100}, retroWaste{})
	// A session in a different repo, seeded via the raw helper with a distinct
	// repo, must not appear.
	otherIID := uuid.New().String()
	if err := d.InsertSession(db.Session{
		InstanceID:  otherIID,
		SessionName: "otherrepo@x",
		Repo:        "otherrepo",
		Worktree:    "/wt/other",
		Harness:     "pi",
		StartedAt:   now.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("InsertSession other: %v", err)
	}

	r, err := d.AssembleRetro(retroRepo, sinceMs)
	if err != nil {
		t.Fatalf("AssembleRetro: %v", err)
	}
	if len(r.Trains) != 1 || r.Trains[0].Root != "retrorepo@a" {
		t.Errorf("repo scope leaked: %+v", r.Trains)
	}
}
