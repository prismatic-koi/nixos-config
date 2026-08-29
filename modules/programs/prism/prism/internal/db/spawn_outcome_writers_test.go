// Package db_test — write-path coverage for spawn_outcome's
// agent-level columns (pr_number, pr_merged_at, review_verdict,
// review_pass_count, review_fail_count).
//
// The `prism stats compare` Agent-level outcomes block historically rendered
// `—` for every one of these columns on every session because no code path
// wrote them: the schema had them, the renderer read them, but nothing
// connected source to column. Tests here lock in the four dedicated write
// paths and the latest-round-wins semantics required by AC:
//
//   - UpdateSpawnOutcomePR              (worker-side capture from gh pr create)
//   - UpdateSpawnOutcomePRMergedAt      (merge-queue watcher's merge handler)
//   - UpdateSpawnOutcomeReviewResult    (review-complete handler)
//   - InstanceIDForPRNumber             (worker lookup at merge time)
//
// Plus the negative-test guards from the AC:
//
//   - a session that never opens a PR keeps pr_number=NULL → renders as —;
//   - a worker running round 1 (FAIL) then round 2 (PASS) shows round-2
//     counts, NOT a sum across rounds.
//
// Tests use the package-local openTestDB helper (see db_test.go).
package db_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedSession is a tiny helper for the writers tests: it inserts a sessions
// row (the FK target of spawn_outcome.instance_id) and returns the
// instance_id. The session_name uses the `prism-test@` prefix per the
// AGENTS.md test-isolation convention so any accidental host-side
// notification cannot collide with a real coordinator slug.
func seedSession(t *testing.T, d *db.DB, suffix string) string {
	t.Helper()
	iid := uuid.New().String()
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: "prism-test@" + suffix,
		Repo:        "prism-test-repo",
		Worktree:    "/tmp/test-wt",
		Harness:     "pi",
		StartedAt:   time.Now().Add(-5 * time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession(%s): %v", suffix, err)
	}
	return iid
}

// TestUpdateSpawnOutcomePR_CreatesPartialRow exercises the worker-side
// capture path: when no spawn_outcome row exists yet (the natural state at
// the moment `gh pr create` returns — long before cleanup writes the full
// row), the UPSERT must create a minimal row with pr_number set and all
// other columns at their schema defaults.
func TestUpdateSpawnOutcomePR_CreatesPartialRow(t *testing.T) {
	d := openTestDB(t)
	iid := seedSession(t, d, "writer-pr-create")

	if err := d.UpdateSpawnOutcomePR(iid, 4242); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR: %v", err)
	}

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil {
		t.Fatalf("SpawnOutcomeByInstanceID: %v", err)
	}
	if out == nil {
		t.Fatal("SpawnOutcomeByInstanceID: got nil, want partial row")
	}
	if out.PRNumber == nil || *out.PRNumber != 4242 {
		t.Errorf("PRNumber: got %v, want 4242", out.PRNumber)
	}
	// The other agent-level columns must remain NULL — the partial write
	// must not stamp them with zero values that the renderer would misread
	// as "review ran with 0 PASS, 0 FAIL".
	if out.ReviewVerdict != nil {
		t.Errorf("ReviewVerdict: got %v, want nil after PR-only write", out.ReviewVerdict)
	}
	if out.ReviewPassCount != nil {
		t.Errorf("ReviewPassCount: got %v, want nil after PR-only write", out.ReviewPassCount)
	}
	if out.PRMergedAt != nil {
		t.Errorf("PRMergedAt: got %v, want nil after PR-only write", out.PRMergedAt)
	}
}

// TestUpdateSpawnOutcomePR_PreservesOtherColumns verifies the AC's
// "no regression in adjacent paths" guard. When WriteSpawnOutcome has
// already persisted a fully-populated row (the cleanup case), a subsequent
// partial UpdateSpawnOutcomePR must only touch pr_number — the rolling
// aggregates must stay intact.
func TestUpdateSpawnOutcomePR_PreservesOtherColumns(t *testing.T) {
	d := openTestDB(t)
	iid := seedSession(t, d, "writer-pr-preserve")

	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}

	before, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || before == nil {
		t.Fatalf("SpawnOutcomeByInstanceID (before): err=%v row=%v", err, before)
	}
	if before.EndState == nil || *before.EndState != "finished" {
		t.Fatalf("pre-state: EndState got %v, want finished", before.EndState)
	}

	if err := d.UpdateSpawnOutcomePR(iid, 99); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR: %v", err)
	}

	after, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || after == nil {
		t.Fatalf("SpawnOutcomeByInstanceID (after): err=%v row=%v", err, after)
	}
	if after.PRNumber == nil || *after.PRNumber != 99 {
		t.Errorf("PRNumber: got %v, want 99", after.PRNumber)
	}
	// EndState (the canonical rolling-aggregation marker)
	// must still equal what WriteSpawnOutcome put down.
	if after.EndState == nil || *after.EndState != "finished" {
		t.Errorf("EndState: got %v, want finished (preserved across PR write)", after.EndState)
	}
}

// TestUpdateSpawnOutcomePR_NoSession verifies the FK-guard no-op: writing
// for an unknown instance_id must NOT error and must NOT create an orphan
// spawn_outcome row. This mirrors WriteSpawnOutcome's pre-migration
// tolerance.
func TestUpdateSpawnOutcomePR_NoSession(t *testing.T) {
	d := openTestDB(t)
	missing := "missing-" + uuid.New().String()
	if err := d.UpdateSpawnOutcomePR(missing, 1); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR(missing): %v (want nil)", err)
	}
	out, err := d.SpawnOutcomeByInstanceID(missing)
	if err != nil {
		t.Fatalf("SpawnOutcomeByInstanceID: %v", err)
	}
	if out != nil {
		t.Errorf("SpawnOutcomeByInstanceID(missing): got %+v, want nil (no orphan row)", out)
	}
}

// TestUpdateSpawnOutcomePRMergedAt_RoundTrip verifies the merge-queue
// watcher's write path: after Update… is called with a wall-clock ms,
// SpawnOutcomeByInstanceID returns a row whose PRMergedAt matches.
func TestUpdateSpawnOutcomePRMergedAt_RoundTrip(t *testing.T) {
	d := openTestDB(t)
	iid := seedSession(t, d, "writer-merged-at")

	// Seed pr_number first so the row exists at the time mergedAt fires —
	// this matches the real ordering (worker captures pr_number, then later
	// the merge-queue watcher fires).
	if err := d.UpdateSpawnOutcomePR(iid, 555); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR: %v", err)
	}

	mergedAt := time.Now().UnixMilli()
	if err := d.UpdateSpawnOutcomePRMergedAt(iid, mergedAt); err != nil {
		t.Fatalf("UpdateSpawnOutcomePRMergedAt: %v", err)
	}

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: err=%v row=%v", err, out)
	}
	if out.PRNumber == nil || *out.PRNumber != 555 {
		t.Errorf("PRNumber: got %v, want 555 (must survive PRMergedAt write)", out.PRNumber)
	}
	if out.PRMergedAt == nil || *out.PRMergedAt != mergedAt {
		t.Errorf("PRMergedAt: got %v, want %d", out.PRMergedAt, mergedAt)
	}
}

// TestUpdateSpawnOutcomeReviewResult_Pass verifies the all-pass case from
// the AC: 5 PASS, 0 FAIL → review_verdict=pass, counts populated.
func TestUpdateSpawnOutcomeReviewResult_Pass(t *testing.T) {
	d := openTestDB(t)
	iid := seedSession(t, d, "writer-review-pass")

	if err := d.UpdateSpawnOutcomeReviewResult(iid, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: err=%v row=%v", err, out)
	}
	if out.ReviewVerdict == nil || *out.ReviewVerdict != "pass" {
		t.Errorf("ReviewVerdict: got %v, want \"pass\"", out.ReviewVerdict)
	}
	if out.ReviewPassCount == nil || *out.ReviewPassCount != 5 {
		t.Errorf("ReviewPassCount: got %v, want 5", out.ReviewPassCount)
	}
	if out.ReviewFailCount == nil || *out.ReviewFailCount != 0 {
		t.Errorf("ReviewFailCount: got %v, want 0", out.ReviewFailCount)
	}
}

// TestUpdateSpawnOutcomeReviewResult_MixedFails verifies the mixed-verdict
// case: 4 PASS, 1 FAIL → review_verdict=fail, counts reflect the round.
// This locks in the "any reviewer failed → FAIL" semantics from the AC.
func TestUpdateSpawnOutcomeReviewResult_MixedFails(t *testing.T) {
	d := openTestDB(t)
	iid := seedSession(t, d, "writer-review-mixed")

	if err := d.UpdateSpawnOutcomeReviewResult(iid, "fail", 4, 1); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: err=%v row=%v", err, out)
	}
	if out.ReviewVerdict == nil || *out.ReviewVerdict != "fail" {
		t.Errorf("ReviewVerdict: got %v, want \"fail\"", out.ReviewVerdict)
	}
	if out.ReviewPassCount == nil || *out.ReviewPassCount != 4 {
		t.Errorf("ReviewPassCount: got %v, want 4", out.ReviewPassCount)
	}
	if out.ReviewFailCount == nil || *out.ReviewFailCount != 1 {
		t.Errorf("ReviewFailCount: got %v, want 1", out.ReviewFailCount)
	}
}

// TestUpdateSpawnOutcomeReviewResult_LatestRoundWins is the AC's negative
// test #2: a worker that runs round 1 (FAIL, 3 PASS / 2 FAIL) then round 2
// (PASS, 5 PASS / 0 FAIL) must show the round-2 values, NOT a sum across
// rounds. This locks in the latest-round-wins semantics — any future change
// that turns this into an accumulator will fail here.
func TestUpdateSpawnOutcomeReviewResult_LatestRoundWins(t *testing.T) {
	d := openTestDB(t)
	iid := seedSession(t, d, "writer-review-latest-round")

	// Round 1: 3 PASS, 2 FAIL → FAIL.
	if err := d.UpdateSpawnOutcomeReviewResult(iid, "fail", 3, 2); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult (round 1): %v", err)
	}

	// Round 2: 5 PASS, 0 FAIL → PASS.
	if err := d.UpdateSpawnOutcomeReviewResult(iid, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult (round 2): %v", err)
	}

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: err=%v row=%v", err, out)
	}
	if out.ReviewVerdict == nil || *out.ReviewVerdict != "pass" {
		t.Errorf("ReviewVerdict after round 2: got %v, want \"pass\" (NOT \"fail\" from round 1)", out.ReviewVerdict)
	}
	// Most explicit assertion: counts must be round-2 values, NOT 8/2 (sum).
	if out.ReviewPassCount == nil || *out.ReviewPassCount != 5 {
		t.Errorf("ReviewPassCount: got %v, want 5 (round 2). A value of 8 would mean we summed across rounds.", out.ReviewPassCount)
	}
	if out.ReviewFailCount == nil || *out.ReviewFailCount != 0 {
		t.Errorf("ReviewFailCount: got %v, want 0 (round 2). A non-zero value would mean round-1 fails leaked through.", out.ReviewFailCount)
	}
}

// TestSpawnOutcome_NoPR_RendersNull is the AC's negative test #1
// (over-broad-fix guard): a session that exits without opening a PR must
// keep pr_number=NULL. The compare renderer reads NULL as `—`; this test
// verifies the column stays NULL when no write path fires, so we never
// surface stale data from a prior incarnation as a "real" PR number.
func TestSpawnOutcome_NoPR_RendersNull(t *testing.T) {
	d := openTestDB(t)
	iid := seedSession(t, d, "writer-no-pr")

	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: err=%v row=%v", err, out)
	}
	if out.PRNumber != nil {
		t.Errorf("PRNumber: got %v, want nil (no gh pr create fired). Non-nil means stale data leaked.", out.PRNumber)
	}
	if out.PRMergedAt != nil {
		t.Errorf("PRMergedAt: got %v, want nil (no merge fired)", out.PRMergedAt)
	}
	if out.ReviewVerdict != nil {
		t.Errorf("ReviewVerdict: got %v, want nil (no review fired)", out.ReviewVerdict)
	}
	if out.ReviewPassCount != nil {
		t.Errorf("ReviewPassCount: got %v, want nil", out.ReviewPassCount)
	}
	if out.ReviewFailCount != nil {
		t.Errorf("ReviewFailCount: got %v, want nil", out.ReviewFailCount)
	}
}

// TestInstanceIDForPRNumber_Roundtrip verifies the worker-lookup helper the
// merge-queue watcher relies on: a row that has pr_number set is findable by
// PR number; a PR that no row carries returns ("", nil).
func TestInstanceIDForPRNumber_Roundtrip(t *testing.T) {
	d := openTestDB(t)
	iid := seedSession(t, d, "writer-instance-by-pr")

	if err := d.UpdateSpawnOutcomePR(iid, 7777); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR: %v", err)
	}

	gotIID, err := d.InstanceIDForPRNumber(7777)
	if err != nil {
		t.Fatalf("InstanceIDForPRNumber: %v", err)
	}
	if gotIID != iid {
		t.Errorf("InstanceIDForPRNumber(7777): got %q, want %q", gotIID, iid)
	}

	gotMissing, err := d.InstanceIDForPRNumber(9999)
	if err != nil {
		t.Fatalf("InstanceIDForPRNumber(9999): %v", err)
	}
	if gotMissing != "" {
		t.Errorf("InstanceIDForPRNumber(9999) for unknown PR: got %q, want empty string", gotMissing)
	}
}

// TestComputeSpawnOutcome_PrefersPersistedAgentLevel verifies that the
// agent-level merge in ComputeSpawnOutcome respects the dedicated-write
// columns: when a partial UPSERT has set pr_number, the value survives a
// subsequent WriteSpawnOutcome call (which re-runs ComputeSpawnOutcome and
// INSERT OR REPLACEs the row). Without this guarantee the agent-level
// write paths would be undone at cleanup time.
func TestComputeSpawnOutcome_PrefersPersistedAgentLevel(t *testing.T) {
	d := openTestDB(t)
	iid := seedSession(t, d, "writer-compute-prefers")

	// Worker-side capture writes pr_number.
	if err := d.UpdateSpawnOutcomePR(iid, 1234); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR: %v", err)
	}
	// Review-complete handler writes verdict.
	if err := d.UpdateSpawnOutcomeReviewResult(iid, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}

	// End the session and run the cleanup-time recompute.
	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome (cleanup): %v", err)
	}

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: err=%v row=%v", err, out)
	}
	if out.PRNumber == nil || *out.PRNumber != 1234 {
		t.Errorf("PRNumber after cleanup recompute: got %v, want 1234 (dedicated write must not be clobbered)", out.PRNumber)
	}
	if out.ReviewVerdict == nil || *out.ReviewVerdict != "pass" {
		t.Errorf("ReviewVerdict after cleanup recompute: got %v, want pass", out.ReviewVerdict)
	}
	if out.ReviewPassCount == nil || *out.ReviewPassCount != 5 {
		t.Errorf("ReviewPassCount after cleanup recompute: got %v, want 5", out.ReviewPassCount)
	}
}
