// Package db_test — read-path coverage for spawn_outcome (issue #2932).
//
// Two guards share this file:
//
//   - If CompareRunOutcome returns a partial-writer stub (review result,
//     pr_number, pr_merged_at) as the session's outcome, the stub tests
//     fail. Every worker that ran a review round has such a stub before
//     cleanup, and its aggregate columns are schema defaults.
//   - If ComputeSpawnOutcome measures duration_ms against sessions.ended_at,
//     which `prism cleanup` stamps, the duration tests fail.
//
// Tests use the package-local openTestDB and seedSession helpers.
package db_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// writeReadPathEvent writes one agent_events row for iid at ts.
func writeReadPathEvent(t *testing.T, d *db.DB, sessionName, iid, typ, payload string, ts time.Time) {
	t.Helper()
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        "prism-test-repo",
		Worktree:    "/tmp/test-wt",
		InstanceID:  &iid,
		Type:        typ,
		Payload:     payload,
		CreatedAt:   ts,
	}); err != nil {
		t.Fatalf("WriteEvent(%s): %v", typ, err)
	}
}

// seedFinishedWorker seeds the state a worker is in after its last turn and
// before any cleanup: an agent_status row in state finished, a sessions row
// with ended_at and end_state still NULL, two token-bearing assistant turns,
// and the sidecar's state_change{finished} event at finishedAt.
func seedFinishedWorker(t *testing.T, d *db.DB, suffix string, startedAt, finishedAt time.Time) (sessionName, iid string) {
	t.Helper()
	sessionName = "prism-test@" + suffix
	iid = uuid.New().String()
	if err := d.UpsertStatus(sessionName, "prism-test-repo", "/tmp/test-wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "prism-test-repo",
		Worktree:    "/tmp/test-wt",
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	writeReadPathEvent(t, d, sessionName, iid, "state_change", `{"state":"active"}`, startedAt.Add(time.Second))
	writeReadPathEvent(t, d, sessionName, iid, "msg_assistant",
		`{"text":"a","model":"anthropic/claude-sonnet-4-6","inputTokens":1000,"outputTokens":400,"cacheReadTokens":50,"cacheWriteTokens":20,"cost":0.25}`,
		startedAt.Add(10*time.Second))
	writeReadPathEvent(t, d, sessionName, iid, "msg_assistant",
		`{"text":"b","model":"anthropic/claude-sonnet-4-6","inputTokens":2000,"outputTokens":600,"cacheReadTokens":70,"cacheWriteTokens":30,"cost":0.5}`,
		startedAt.Add(20*time.Second))
	writeReadPathEvent(t, d, sessionName, iid, "state_change", `{"state":"finished"}`, finishedAt)
	return sessionName, iid
}

// TestCompareRunOutcome_ReviewStubDoesNotHideAggregates is the regression
// test for the compare-path defect. A finished worker whose only
// spawn_outcome row is the review-result stub must still surface the
// aggregates computed from agent_events, and must keep the stub's verdict.
func TestCompareRunOutcome_ReviewStubDoesNotHideAggregates(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-30 * time.Minute)
	_, iid := seedFinishedWorker(t, d, "review-stub", startedAt, startedAt.Add(28*time.Minute))

	if err := d.UpdateSpawnOutcomeReviewResult(iid, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}
	stub, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || stub == nil {
		t.Fatalf("pre-condition: stub row missing: err=%v row=%v", err, stub)
	}
	if stub.AggregatedAt != nil {
		t.Fatalf("pre-condition: a partial writer must not stamp aggregated_at, got %d", *stub.AggregatedAt)
	}

	sess, _ := d.SessionByInstanceID(iid)
	out := d.CompareRunOutcome(sess)
	if out == nil {
		t.Fatal("CompareRunOutcome: nil for a finished session")
	}
	if out.TokensInputTotal != 3000 || out.TokensOutputTotal != 1000 {
		t.Errorf("tokens: got in=%d out=%d, want in=3000 out=1000 (the stub's zeros were returned instead of the computed aggregates)",
			out.TokensInputTotal, out.TokensOutputTotal)
	}
	if out.CostUSDTotal < 0.749 || out.CostUSDTotal > 0.751 {
		t.Errorf("CostUSDTotal: got %.4f, want 0.75", out.CostUSDTotal)
	}
	if out.EndState == nil || *out.EndState != "finished" {
		t.Errorf("EndState: got %v, want finished", out.EndState)
	}
	if out.DurationMs == nil {
		t.Error("DurationMs: nil, want the start→finished interval")
	}
	if out.ReviewVerdict == nil || *out.ReviewVerdict != "pass" {
		t.Errorf("ReviewVerdict: got %v, want pass (the stub's agent-level columns must survive the compute path)", out.ReviewVerdict)
	}
}

// TestCompareRunOutcome_PRStubDoesNotHideAggregates covers the other
// pre-cleanup stub writer: a captured pr_number.
func TestCompareRunOutcome_PRStubDoesNotHideAggregates(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-30 * time.Minute)
	_, iid := seedFinishedWorker(t, d, "pr-stub", startedAt, startedAt.Add(28*time.Minute))

	if err := d.UpdateSpawnOutcomePR(iid, 2931); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR: %v", err)
	}
	sess, _ := d.SessionByInstanceID(iid)
	out := d.CompareRunOutcome(sess)
	if out == nil {
		t.Fatal("CompareRunOutcome: nil for a finished session")
	}
	if out.TokensOutputTotal != 1000 {
		t.Errorf("TokensOutputTotal: got %d, want 1000", out.TokensOutputTotal)
	}
	if out.PRNumber == nil || *out.PRNumber != 2931 {
		t.Errorf("PRNumber: got %v, want 2931", out.PRNumber)
	}
}

// TestCompareRunOutcome_AggregatedRowIsReturnedAsIs verifies the other side
// of the gate: once WriteSpawnOutcome has filled the row, CompareRunOutcome
// returns that row (AggregatedAt set) rather than recomputing.
func TestCompareRunOutcome_AggregatedRowIsReturnedAsIs(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-30 * time.Minute)
	_, iid := seedFinishedWorker(t, d, "aggregated", startedAt, startedAt.Add(28*time.Minute))

	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}
	sess, _ := d.SessionByInstanceID(iid)
	out := d.CompareRunOutcome(sess)
	if out == nil || out.AggregatedAt == nil {
		t.Fatalf("CompareRunOutcome: want the persisted row with AggregatedAt set, got %+v", out)
	}
	if out.TokensOutputTotal != 1000 {
		t.Errorf("TokensOutputTotal: got %d, want 1000", out.TokensOutputTotal)
	}

	// A later partial write (the merge-queue watcher stamping pr_merged_at
	// after cleanup) updates its column in place and keeps the row filled.
	if err := d.UpdateSpawnOutcomePRMergedAt(iid, time.Now().UnixMilli()); err != nil {
		t.Fatalf("UpdateSpawnOutcomePRMergedAt: %v", err)
	}
	after := d.CompareRunOutcome(sess)
	if after == nil || after.AggregatedAt == nil {
		t.Fatalf("CompareRunOutcome after pr_merged_at: want the persisted row, got %+v", after)
	}
	if after.PRMergedAt == nil {
		t.Error("PRMergedAt: nil after UpdateSpawnOutcomePRMergedAt")
	}
	if after.TokensOutputTotal != 1000 {
		t.Errorf("TokensOutputTotal after partial write: got %d, want 1000", after.TokensOutputTotal)
	}
}

// TestCompareRunOutcome_LiveSessionWithStubReturnsNil: a stub on a session
// that is still active must not be surfaced as an outcome. The renderer
// treats nil as "not yet available"; a stub would read as zero usage.
func TestCompareRunOutcome_LiveSessionWithStubReturnsNil(t *testing.T) {
	d := openTestDB(t)
	sessionName := "prism-test@live-stub"
	iid := uuid.New().String()
	if err := d.UpsertStatus(sessionName, "prism-test-repo", "/tmp/test-wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID: iid, SessionName: sessionName, Repo: "prism-test-repo",
		Worktree: "/tmp/test-wt", Harness: "pi", StartedAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := d.UpdateSpawnOutcomePR(iid, 77); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR: %v", err)
	}
	sess, _ := d.SessionByInstanceID(iid)
	if out := d.CompareRunOutcome(sess); out != nil {
		t.Errorf("CompareRunOutcome on a live session with a stub: got %+v, want nil", out)
	}
}

// TestCompareRunOutcome_OldIncarnationUnderReusedLiveName covers the
// pre-migration row of a past incarnation whose session name is live again
// (every earlier coordinator `@main` run, a re-spawned worker branch). The
// agent_status row for the name now belongs to the new incarnation and is
// active. If SessionIsTerminal trusts that row for the old instance_id, the
// old incarnation reads as live, CompareRunOutcome returns nil, and its
// aggregates vanish.
func TestCompareRunOutcome_OldIncarnationUnderReusedLiveName(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-3 * time.Hour)
	sessionName, oldIID := seedFinishedWorker(t, d, "reused-name", startedAt, startedAt.Add(28*time.Minute))

	// Cleanup fills the row, then the column is cleared to mirror a row
	// written before the aggregated_at migration.
	if err := d.SetEnded(sessionName); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	if err := d.UpdateSessionEnded(oldIID, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}
	if err := d.WriteSpawnOutcome(oldIID); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}
	raw, err := sql.Open("sqlite", d.Path())
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(`UPDATE spawn_outcome SET aggregated_at = NULL WHERE instance_id = ?`, oldIID); err != nil {
		raw.Close()
		t.Fatalf("clear aggregated_at: %v", err)
	}
	raw.Close()

	// The name is spawned again: a new incarnation, active, owns the
	// agent_status row.
	newIID := uuid.New().String()
	if err := d.UpsertStatus(sessionName, "prism-test-repo", "/tmp/test-wt", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (respawn): %v", err)
	}
	if err := d.ClearEnded(sessionName); err != nil {
		t.Fatalf("ClearEnded: %v", err)
	}
	if err := d.SetInstanceID(sessionName, newIID); err != nil {
		t.Fatalf("SetInstanceID (respawn): %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID: newIID, SessionName: sessionName, Repo: "prism-test-repo",
		Worktree: "/tmp/test-wt", Harness: "pi", StartedAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession (respawn): %v", err)
	}

	old, _ := d.SessionByInstanceID(oldIID)
	if !d.SessionIsTerminal(old) {
		t.Fatal("SessionIsTerminal(old incarnation) = false; the live row belongs to another instance_id and sessions.end_state is finished")
	}
	out := d.CompareRunOutcome(old)
	if out == nil {
		t.Fatal("CompareRunOutcome(old incarnation): nil; its aggregates vanished behind the reused name")
	}
	if out.TokensOutputTotal != 1000 {
		t.Errorf("TokensOutputTotal: got %d, want 1000", out.TokensOutputTotal)
	}
	if out.EndState == nil || *out.EndState != "finished" {
		t.Errorf("EndState: got %v, want finished", out.EndState)
	}

	current, _ := d.SessionByInstanceID(newIID)
	if d.SessionIsTerminal(current) {
		t.Error("SessionIsTerminal(live incarnation) = true, want false")
	}
	if got := d.CompareRunOutcome(current); got != nil {
		t.Errorf("CompareRunOutcome(live incarnation): got %+v, want nil", got)
	}
}

// TestComputeSpawnOutcome_EndStateAndDurationBeforeCleanup covers the
// pre-cleanup window: sessions.end_state and ended_at are NULL, and the
// only record of the end is the sidecar's state_change{finished} event.
func TestComputeSpawnOutcome_EndStateAndDurationBeforeCleanup(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-30 * time.Minute).Truncate(time.Millisecond)
	finishedAt := startedAt.Add(28*time.Minute + 24*time.Second)
	_, iid := seedFinishedWorker(t, d, "pre-cleanup-end", startedAt, finishedAt)

	out, err := d.ComputeSpawnOutcome(iid)
	if err != nil || out == nil {
		t.Fatalf("ComputeSpawnOutcome: out=%v err=%v", out, err)
	}
	if out.EndState == nil || *out.EndState != "finished" {
		t.Errorf("EndState: got %v, want finished", out.EndState)
	}
	want := finishedAt.UnixMilli() - startedAt.UnixMilli()
	if out.DurationMs == nil || *out.DurationMs != want {
		t.Errorf("DurationMs: got %v, want %d", out.DurationMs, want)
	}
	if out.TimeToFinishedMs == nil || *out.TimeToFinishedMs != want {
		t.Errorf("TimeToFinishedMs: got %v, want %d", out.TimeToFinishedMs, want)
	}
}

// TestComputeSpawnOutcome_DurationDoesNotMoveWithCleanup is the regression
// test for the second defect. Two cleanups stamp sessions.ended_at at two
// different times; the duration must equal start→finished both times.
func TestComputeSpawnOutcome_DurationDoesNotMoveWithCleanup(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-60 * time.Minute).Truncate(time.Millisecond)
	finishedAt := startedAt.Add(28 * time.Minute)
	sessionName, iid := seedFinishedWorker(t, d, "duration-cleanup", startedAt, finishedAt)
	want := finishedAt.UnixMilli() - startedAt.UnixMilli()

	// First cleanup, 16 minutes of idle after the finish.
	if err := d.SetEnded(sessionName); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}
	first, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || first == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: out=%v err=%v", first, err)
	}
	if first.DurationMs == nil || *first.DurationMs != want {
		t.Errorf("DurationMs after first cleanup: got %v, want %d (start→finished, not start→cleanup)", first.DurationMs, want)
	}
	if first.TimeToFinishedMs == nil || *first.TimeToFinishedMs != want {
		t.Errorf("TimeToFinishedMs after first cleanup: got %v, want %d", first.TimeToFinishedMs, want)
	}
	if first.EndState == nil || *first.EndState != "finished" {
		t.Errorf("EndState after first cleanup: got %v, want finished", first.EndState)
	}

	// A second cleanup pass re-stamps sessions.ended_at; the row must not move.
	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded (second): %v", err)
	}
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome (second): %v", err)
	}
	second, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || second == nil {
		t.Fatalf("SpawnOutcomeByInstanceID (second): out=%v err=%v", second, err)
	}
	if second.DurationMs == nil || *second.DurationMs != want {
		t.Errorf("DurationMs after second cleanup: got %v, want %d", second.DurationMs, want)
	}
}

// TestComputeSpawnOutcome_LatestStateChangeWins: a worker that finished,
// was prompted again, and finished a second time is measured to the second
// finish, not the first.
func TestComputeSpawnOutcome_LatestStateChangeWins(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-60 * time.Minute).Truncate(time.Millisecond)
	sessionName, iid := seedFinishedWorker(t, d, "second-turn", startedAt, startedAt.Add(20*time.Minute))
	writeReadPathEvent(t, d, sessionName, iid, "state_change", `{"state":"active"}`, startedAt.Add(30*time.Minute))
	secondFinish := startedAt.Add(45 * time.Minute)
	writeReadPathEvent(t, d, sessionName, iid, "state_change", `{"state":"finished"}`, secondFinish)

	out, err := d.ComputeSpawnOutcome(iid)
	if err != nil || out == nil {
		t.Fatalf("ComputeSpawnOutcome: out=%v err=%v", out, err)
	}
	want := secondFinish.UnixMilli() - startedAt.UnixMilli()
	if out.DurationMs == nil || *out.DurationMs != want {
		t.Errorf("DurationMs: got %v, want %d (the second finish)", out.DurationMs, want)
	}
}

// TestComputeSpawnOutcome_FallsBackToSessionsRow: when the last state_change
// event is not terminal (the session was closed while active and no sidecar
// wrote the interrupted transition), the sessions row is the only record of
// the end and is used as-is.
func TestComputeSpawnOutcome_FallsBackToSessionsRow(t *testing.T) {
	d := openTestDB(t)
	startedAt := time.Now().Add(-60 * time.Minute).Truncate(time.Millisecond)
	sessionName, iid := seedFinishedWorker(t, d, "fallback", startedAt, startedAt.Add(20*time.Minute))
	writeReadPathEvent(t, d, sessionName, iid, "state_change", `{"state":"active"}`, startedAt.Add(30*time.Minute))

	// No record of an end at all: no duration, no end state.
	out, err := d.ComputeSpawnOutcome(iid)
	if err != nil || out == nil {
		t.Fatalf("ComputeSpawnOutcome: out=%v err=%v", out, err)
	}
	if out.EndState != nil || out.DurationMs != nil {
		t.Errorf("before any end record: EndState=%v DurationMs=%v, want both nil", out.EndState, out.DurationMs)
	}

	// Cleanup stamps the sessions row; that is now the end record.
	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}
	sess, _ := d.SessionByInstanceID(iid)
	out, err = d.ComputeSpawnOutcome(iid)
	if err != nil || out == nil {
		t.Fatalf("ComputeSpawnOutcome (after cleanup): out=%v err=%v", out, err)
	}
	if out.EndState == nil || *out.EndState != "finished" {
		t.Errorf("EndState: got %v, want finished from the sessions row", out.EndState)
	}
	want := sess.EndedAt.UnixMilli() - startedAt.UnixMilli()
	if out.DurationMs == nil || *out.DurationMs != want {
		t.Errorf("DurationMs: got %v, want %d from sessions.ended_at", out.DurationMs, want)
	}
}

// TestWriteSpawnOutcome_StampsAggregatedAt pins the writer side of the gate.
func TestWriteSpawnOutcome_StampsAggregatedAt(t *testing.T) {
	d := openTestDB(t)
	iid := seedSession(t, d, "stamp")

	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}
	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: out=%v err=%v", out, err)
	}
	if out.AggregatedAt == nil {
		t.Fatal("AggregatedAt: nil after WriteSpawnOutcome")
	}
	if *out.AggregatedAt != out.ComputedAt {
		t.Errorf("AggregatedAt = %d, ComputedAt = %d; want equal on a fresh write", *out.AggregatedAt, out.ComputedAt)
	}
}
