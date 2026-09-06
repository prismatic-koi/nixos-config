package db_test

// Behaviour tests for the aggregated_at gate (issue #2936).
//
// aggregated_at replaces the value-inference predicate HasComputedAggregates
// on every read path:
//
//   - CompareRunOutcome returns the persisted row only when aggregated_at is
//     set, and recomputes from agent_events otherwise.
//   - --group-by (SpawnOutcomeGroupBy) excludes rows with a NULL aggregated_at
//     from its sums.
//   - --abtest (AbtestPairsAll) excludes rows with a NULL aggregated_at from
//     its sums.
//
// Several tests fabricate a stub row that carries a non-zero aggregate value
// but a NULL aggregated_at — the exact shape the value-inference predicate
// could not tell apart from a real aggregate (issue #2936, weakness 1). A
// gate keyed on aggregated_at must exclude it; a gate keyed on the values
// would fold it into the sums. That makes these tests non-vacuous: they fail
// against the old inference gate.

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// forceStubTokens sets tokens_output_total on an existing row while leaving
// aggregated_at NULL, via a raw connection. It models a future partial writer
// that happens to touch one aggregate column — the inference weakness the
// explicit column exists to defeat.
func forceStubTokens(t *testing.T, d *db.DB, iid string, tokensOut int64) {
	t.Helper()
	raw, err := sql.Open("sqlite", d.Path())
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer raw.Close()
	res, err := raw.Exec(
		`UPDATE spawn_outcome SET tokens_output_total = ? WHERE instance_id = ? AND aggregated_at IS NULL`,
		tokensOut, iid,
	)
	if err != nil {
		t.Fatalf("force stub tokens: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("force stub tokens: updated %d rows, want 1 (row must exist and be a stub)", n)
	}
}

// TestWriteSpawnOutcome_StampsAggregatedAt pins the writer side of the gate
// (AC: the cleanup-time writer sets aggregated_at). WriteSpawnOutcome stamps
// aggregated_at with computed_at.
func TestWriteSpawnOutcome_StampsAggregatedAt(t *testing.T) {
	d := openTestDB(t)
	const sess = "repo@2936-stamp"
	started := time.Now().Add(-20 * time.Minute)
	iid := seedTerminalSession(t, d, sess, started)
	writeEventAt(t, d, sess, iid, "msg_assistant", assistantPayload(100, 200, 0, 0, 0.5), started.Add(time.Minute))
	writeEventAt(t, d, sess, iid, "state_change", `{"state":"finished"}`, started.Add(2*time.Minute))

	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}
	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: out=%v err=%v", out, err)
	}
	if out.AggregatedAt == nil {
		t.Fatal("AggregatedAt: nil after WriteSpawnOutcome, want set")
	}
	if *out.AggregatedAt != out.ComputedAt {
		t.Errorf("AggregatedAt = %d, ComputedAt = %d; want equal on a fresh write", *out.AggregatedAt, out.ComputedAt)
	}
}

// TestCompareRunOutcome_GatesOnAggregatedAt covers the compare read-path AC.
// A stub row (aggregated_at NULL) with a fabricated non-zero token value must
// not be returned as-is: CompareRunOutcome recomputes from agent_events. Then,
// once WriteSpawnOutcome has stamped aggregated_at, the persisted row wins.
func TestCompareRunOutcome_GatesOnAggregatedAt(t *testing.T) {
	d := openTestDB(t)
	const sess = "repo@2936-compare-gate"
	started := time.Now().Add(-30 * time.Minute)
	iid := seedTerminalSession(t, d, sess, started)
	writeEventAt(t, d, sess, iid, "msg_assistant", assistantPayload(1000, 400, 0, 0, 0.25), started.Add(time.Minute))
	writeEventAt(t, d, sess, iid, "state_change", `{"state":"finished"}`, started.Add(2*time.Minute))

	// A partial writer creates the stub; a fabricated token value is then
	// forced onto it while aggregated_at stays NULL.
	if err := d.UpdateSpawnOutcomePR(iid, 2936); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR: %v", err)
	}
	forceStubTokens(t, d, iid, 99999)

	sess1, _ := d.SessionByInstanceID(iid)
	out := d.CompareRunOutcome(sess1)
	if out == nil {
		t.Fatal("CompareRunOutcome: nil for a terminal session")
	}
	if out.TokensOutputTotal != 400 {
		t.Errorf("TokensOutputTotal: got %d, want 400 (the recomputed value; the stub's fabricated 99999 must not win)", out.TokensOutputTotal)
	}
	if out.PRNumber == nil || *out.PRNumber != 2936 {
		t.Errorf("PRNumber: got %v, want 2936 (the stub's own column survives the recompute)", out.PRNumber)
	}

	// Cleanup fills and stamps the row; now the persisted aggregate wins.
	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}
	after := d.CompareRunOutcome(sess1)
	if after == nil || after.AggregatedAt == nil {
		t.Fatalf("CompareRunOutcome after cleanup: want persisted row with AggregatedAt set, got %+v", after)
	}
	if after.TokensOutputTotal != 400 {
		t.Errorf("TokensOutputTotal after cleanup: got %d, want 400", after.TokensOutputTotal)
	}
}

// TestCompareRunOutcome_ZeroEventCleanupRowNotRecomputed covers the edge-case
// AC: a cleanup-written row for a session that produced no events is marked
// aggregated (aggregated_at set) and is returned as the persisted row, not
// recomputed on every read.
func TestCompareRunOutcome_ZeroEventCleanupRowNotRecomputed(t *testing.T) {
	d := openTestDB(t)
	const sess = "repo@2936-zero-event"
	started := time.Now().Add(-15 * time.Minute)
	iid := seedTerminalSession(t, d, sess, started)
	// No token events; only the terminal transition.
	writeEventAt(t, d, sess, iid, "state_change", `{"state":"finished"}`, started.Add(time.Minute))

	if err := d.WriteSpawnOutcome(iid); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}
	persisted, _ := d.SpawnOutcomeByInstanceID(iid)
	if persisted == nil || persisted.AggregatedAt == nil {
		t.Fatalf("zero-event cleanup row: want aggregated_at set, got %+v", persisted)
	}

	sess1, _ := d.SessionByInstanceID(iid)
	out := d.CompareRunOutcome(sess1)
	if out == nil || out.AggregatedAt == nil {
		t.Fatalf("CompareRunOutcome: want the persisted zero-event row, got %+v (a recompute would drop aggregated_at)", out)
	}
}

// TestCompareRunOutcome_PartialStubIsNotAggregated covers the edge-case AC: a
// partial stub written by the PR-number or review-verdict writers has a NULL
// aggregated_at and is not treated as a complete aggregate. A stub on a LIVE
// session must surface as nil (not the stub's zeros or fabricated values).
func TestCompareRunOutcome_PartialStubIsNotAggregated(t *testing.T) {
	d := openTestDB(t)
	const sess = "repo@2936-live-stub"
	iid := seedSessionForCompare(t, d, sess, "active")

	if err := d.UpdateSpawnOutcomeReviewResult(iid, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}
	stub, _ := d.SpawnOutcomeByInstanceID(iid)
	if stub == nil || stub.AggregatedAt != nil {
		t.Fatalf("review stub: want aggregated_at NULL, got %+v", stub)
	}
	forceStubTokens(t, d, iid, 12345)

	sess1, _ := d.SessionByInstanceID(iid)
	if out := d.CompareRunOutcome(sess1); out != nil {
		t.Errorf("CompareRunOutcome on a live session with a stub: got %+v, want nil", out)
	}
}

// TestSpawnOutcomeGroupBy_ExcludesNullAggregatedAt covers the --group-by AC.
// A profile whose only session carries a stub row (aggregated_at NULL, with a
// fabricated token value) must not appear in the group-by breakdown; a profile
// with a real aggregated row appears with its real sums.
func TestSpawnOutcomeGroupBy_ExcludesNullAggregatedAt(t *testing.T) {
	d := openTestDB(t)
	now := time.Now().Add(-10 * time.Minute)

	// Real session: aggregated row under profile "prof-real".
	realIID := uuid.New().String()
	insertAbtestSessionForTest(t, d, "repo@gb-real", realIID, "", "prof-real", now)
	writeEventAt(t, d, "repo@gb-real", realIID, "msg_assistant", assistantPayload(500, 700, 0, 0, 1.0), now.Add(time.Minute))
	if err := d.WriteSpawnOutcome(realIID); err != nil {
		t.Fatalf("WriteSpawnOutcome(real): %v", err)
	}

	// Stub session: profile "prof-stub", stub row only, fabricated tokens.
	stubIID := uuid.New().String()
	insertAbtestSessionForTest(t, d, "repo@gb-stub", stubIID, "", "prof-stub", now)
	if err := d.UpdateSpawnOutcomePR(stubIID, 2936); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR(stub): %v", err)
	}
	forceStubTokens(t, d, stubIID, 88888)

	rows, err := d.SpawnOutcomeGroupBy("profile", 0)
	if err != nil {
		t.Fatalf("SpawnOutcomeGroupBy: %v", err)
	}
	byGroup := map[string]db.GroupByRow{}
	for _, r := range rows {
		byGroup[r.GroupValue] = r
	}
	if _, ok := byGroup["prof-stub"]; ok {
		t.Errorf("prof-stub appears in group-by, want excluded (its only row is a stub with NULL aggregated_at)")
	}
	real, ok := byGroup["prof-real"]
	if !ok {
		t.Fatal("prof-real missing from group-by")
	}
	if real.SessionCount != 1 {
		t.Errorf("prof-real SessionCount: got %d, want 1", real.SessionCount)
	}
	if real.TokensOutputTotal != 700 {
		t.Errorf("prof-real TokensOutputTotal: got %d, want 700", real.TokensOutputTotal)
	}
}

// TestAbtestPairsAll_ExcludesNullAggregatedAt covers the --abtest AC. In a
// pair whose B leg is a live stub with a fabricated token value, the B-slot
// metrics must not reflect the stub's persisted value: the join excludes the
// stub and, for a live session, no recompute is possible, so B reads zero.
func TestAbtestPairsAll_ExcludesNullAggregatedAt(t *testing.T) {
	d := openTestDB(t)
	const pairID = "pair-2936-aaaa-bbbb-cccc-dddddddddddd"
	now := time.Now().Add(-20 * time.Minute)

	// Leg A: terminal, real aggregated row.
	iidA := uuid.New().String()
	insertAbtestSessionForTest(t, d, "repo@ab-a", iidA, pairID, "profA", now)
	writeEventAt(t, d, "repo@ab-a", iidA, "msg_assistant", assistantPayload(300, 600, 0, 0, 0.5), now.Add(time.Minute))
	if err := d.UpdateSessionEnded(iidA, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded(A): %v", err)
	}
	if err := d.WriteSpawnOutcome(iidA); err != nil {
		t.Fatalf("WriteSpawnOutcome(A): %v", err)
	}

	// Leg B: live stub with a fabricated token value.
	iidB := uuid.New().String()
	insertAbtestSessionForTest(t, d, "repo@ab-b", iidB, pairID, "profB", now.Add(time.Second))
	if err := d.UpdateSpawnOutcomePR(iidB, 2936); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR(B): %v", err)
	}
	forceStubTokens(t, d, iidB, 77777)

	pairs, err := d.AbtestPairsAll()
	if err != nil {
		t.Fatalf("AbtestPairsAll: %v", err)
	}
	var pair *db.AbtestPairRow
	for i := range pairs {
		if pairs[i].PairID == pairID {
			pair = &pairs[i]
		}
	}
	if pair == nil {
		t.Fatal("pair missing from AbtestPairsAll")
	}
	if pair.TokensOutputA == nil || *pair.TokensOutputA != 600 {
		t.Errorf("A-slot TokensOutput: got %v, want 600 (the real aggregated row)", pair.TokensOutputA)
	}
	if pair.TokensOutputB == nil || *pair.TokensOutputB != 0 {
		t.Errorf("B-slot TokensOutput: got %v, want 0 (stub excluded; live session has no recompute; 77777 must not appear)", pair.TokensOutputB)
	}
}
