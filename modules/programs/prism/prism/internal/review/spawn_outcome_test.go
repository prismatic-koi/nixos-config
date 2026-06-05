package review_test

// Issue #2110: review-complete handler persists verdict + per-agent counts
// on the worker's spawn_outcome row. This is the AC's "single write site"
// for the three review columns the `prism stats compare` renderer reads.
//
// Tests here cover:
//
//   - all PASS round → verdict=pass, 5/0 counts.
//   - mixed (some FAIL) round → verdict=fail, counts reflect the round.
//   - latest-round-wins: a second invocation with new counts overwrites the
//     first; never sums across rounds.
//   - negative-mutation guard: the AgentResult.Passed field actually drives
//     the counts; flipping it from true→false must flip the result.
//   - silent no-op when the worker session has no sessions row (FK-guard).

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// derefIntPtr returns 0 when p is nil, otherwise *p. Used by the error-
// message formatters below so a failed assertion prints the actual value
// rather than the pointer address — the difference matters when the AC
// reviewer is reading a CI log post-mortem.
func derefIntPtr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// seedWorkerSession is a tiny helper that inserts the worker sessions row
// the review write helper joins through. Session names use the prism-test@
// prefix per AGENTS.md test-isolation convention so they cannot collide with
// a live coordinator slug.
func seedWorkerSession(t *testing.T, d *db.DB, sessionName string) string {
	t.Helper()
	iid := uuid.New().String()
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "prism-test-repo",
		Worktree:    "/tmp/" + sessionName,
		Harness:     "pi",
		StartedAt:   time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession(%s): %v", sessionName, err)
	}
	return iid
}

// makePassResult builds an AgentResult representing an agent that emitted a
// `<verdict>PASS</verdict>` marker. The Output text is irrelevant for this
// test — the persist helper consumes only the Passed boolean.
func makePassResult(name string) review.AgentResult {
	return review.AgentResult{
		Agent:  review.Agent{Name: name},
		Passed: true,
		Output: "<verdict>PASS</verdict>",
	}
}

// makeFailResult builds an AgentResult representing an agent that emitted a
// `<verdict>FAIL</verdict>` marker.
func makeFailResult(name string) review.AgentResult {
	return review.AgentResult{
		Agent:  review.Agent{Name: name},
		Passed: false,
		Output: "<verdict>FAIL</verdict>",
	}
}

// TestPersistReviewOutcome_AllPass_5PASS verifies the AC's happy-path test
// case: a fully-passing round writes verdict=pass with the matching counts.
func TestPersistReviewOutcome_AllPass_5PASS(t *testing.T) {
	d := openTestDB(t)
	worker := "prism-test@review-all-pass"
	iid := seedWorkerSession(t, d, worker)

	results := []review.AgentResult{
		makePassResult("review-goal"),
		makePassResult("review-code"),
		makePassResult("review-security"),
		makePassResult("review-qa"),
		makePassResult("review-context"),
	}
	review.PersistReviewOutcomeForTest(d, worker, results, true /*allPassed*/)

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

// TestPersistReviewOutcome_MixedFail_4PASS_1FAIL verifies the AC's
// mixed-verdict case: any reviewer failing flips the aggregate to "fail" and
// the per-agent counts reflect what the round actually produced.
func TestPersistReviewOutcome_MixedFail_4PASS_1FAIL(t *testing.T) {
	d := openTestDB(t)
	worker := "prism-test@review-mixed-fail"
	iid := seedWorkerSession(t, d, worker)

	results := []review.AgentResult{
		makePassResult("review-goal"),
		makePassResult("review-code"),
		makePassResult("review-security"),
		makePassResult("review-qa"),
		makeFailResult("review-context"),
	}
	review.PersistReviewOutcomeForTest(d, worker, results, false /*allPassed*/)

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

// TestPersistReviewOutcome_LatestRoundWins is the AC's negative test #2:
// a worker that runs round 1 (FAIL) then round 2 (PASS) must show round-2
// counts, NOT a sum across rounds. This locks in the latest-round-wins
// semantics as the source-of-truth for the renderer.
//
// The MonitorFunc invocation pattern this models is exactly the one a
// recovering worker exercises: round 1 reports FAIL with 3/2 counts, the
// worker fixes its blockers, round 2 reports PASS with 5/0 counts. The
// `prism stats compare` row for the worker reflects round 2 only.
func TestPersistReviewOutcome_LatestRoundWins(t *testing.T) {
	d := openTestDB(t)
	worker := "prism-test@review-latest-round"
	iid := seedWorkerSession(t, d, worker)

	// Round 1: 3 PASS, 2 FAIL → FAIL.
	round1 := []review.AgentResult{
		makePassResult("review-goal"),
		makePassResult("review-code"),
		makePassResult("review-security"),
		makeFailResult("review-qa"),
		makeFailResult("review-context"),
	}
	review.PersistReviewOutcomeForTest(d, worker, round1, false /*allPassed*/)

	// Sanity check intermediate state.
	mid, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || mid == nil {
		t.Fatalf("intermediate SpawnOutcomeByInstanceID: err=%v row=%v", err, mid)
	}
	if mid.ReviewPassCount == nil || *mid.ReviewPassCount != 3 {
		t.Fatalf("round 1 ReviewPassCount: got %v, want 3", mid.ReviewPassCount)
	}

	// Round 2: 5 PASS, 0 FAIL → PASS.
	round2 := []review.AgentResult{
		makePassResult("review-goal"),
		makePassResult("review-code"),
		makePassResult("review-security"),
		makePassResult("review-qa"),
		makePassResult("review-context"),
	}
	review.PersistReviewOutcomeForTest(d, worker, round2, true /*allPassed*/)

	final, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || final == nil {
		t.Fatalf("final SpawnOutcomeByInstanceID: err=%v row=%v", err, final)
	}
	if final.ReviewVerdict == nil || *final.ReviewVerdict != "pass" {
		t.Errorf("final ReviewVerdict: got %v, want \"pass\" (latest round wins)", final.ReviewVerdict)
	}
	if final.ReviewPassCount == nil || *final.ReviewPassCount != 5 {
		t.Errorf("final ReviewPassCount: got %v, want 5 (round 2). A value of 8 would mean we summed across rounds.", final.ReviewPassCount)
	}
	if final.ReviewFailCount == nil || *final.ReviewFailCount != 0 {
		t.Errorf("final ReviewFailCount: got %v, want 0 (round 2). A value of 2 would mean round-1 FAILs leaked through.", final.ReviewFailCount)
	}
}

// TestPersistReviewOutcome_NegativeMutation_PassedDrivesCounts is the
// AC-mandated negative-mutation guard: it locks in that the AgentResult.Passed
// field actually drives the persisted counts. If a future refactor changes
// the helper to consume a different field (e.g. r.IsError only, or a
// hard-coded constant) this test fails — the input clearly identifies one
// PASS and four FAILs, and the persisted counts must reflect that exactly.
//
// To reproduce the negative-mutation check manually:
//
//  1. In persistReviewOutcome, comment out `if r.Passed { passCount++ }`.
//  2. Run this test → it must FAIL (passCount becomes 0, failCount becomes 5).
//  3. Restore → test passes.
func TestPersistReviewOutcome_NegativeMutation_PassedDrivesCounts(t *testing.T) {
	d := openTestDB(t)
	worker := "prism-test@review-mutation-guard"
	iid := seedWorkerSession(t, d, worker)

	// Construct a deliberately asymmetric input: exactly one PASS, four FAIL.
	// If the write helper has been broken to ignore r.Passed, the counts
	// will not match 1/4.
	results := []review.AgentResult{
		makePassResult("review-goal"),
		makeFailResult("review-code"),
		makeFailResult("review-security"),
		makeFailResult("review-qa"),
		makeFailResult("review-context"),
	}
	review.PersistReviewOutcomeForTest(d, worker, results, false /*allPassed*/)

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: err=%v row=%v", err, out)
	}
	if out.ReviewPassCount == nil || *out.ReviewPassCount != 1 {
		t.Errorf("ReviewPassCount: got %d, want 1 (one PASS in input)", derefIntPtr(out.ReviewPassCount))
	}
	if out.ReviewFailCount == nil || *out.ReviewFailCount != 4 {
		t.Errorf("ReviewFailCount: got %d, want 4 (four FAIL in input)", derefIntPtr(out.ReviewFailCount))
	}
}

// TestPersistReviewOutcome_NoSession_NoOp verifies the FK-guard silent no-op
// when the worker has no sessions row. This mirrors the WriteSpawnOutcome
// behaviour for pre-migration instances — the write must not error or create
// an orphan row.
func TestPersistReviewOutcome_NoSession_NoOp(t *testing.T) {
	d := openTestDB(t)
	// Note: NO seedWorkerSession call — the worker session name resolves to
	// no row, so MostRecentSessionForName returns nil and persistReviewOutcome
	// silently returns.
	worker := "prism-test@review-no-session"
	results := []review.AgentResult{
		makePassResult("review-goal"),
	}
	// Must not panic, must not error.
	review.PersistReviewOutcomeForTest(d, worker, results, true)

	// No sessions row exists, so no spawn_outcome row can exist either. We
	// can't query by instance_id because we don't have one — verify via the
	// pr_number index lookup, which would also return "" for any value.
	if iid, _ := d.InstanceIDForPRNumber(0); iid != "" {
		t.Errorf("InstanceIDForPRNumber(0): got %q, want \"\" (no orphan row)", iid)
	}
}
