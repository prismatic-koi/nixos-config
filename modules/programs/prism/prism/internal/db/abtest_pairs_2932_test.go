package db_test

// `prism stats --abtest` lists every A/B pair with its per-leg metrics. It is
// the third comparison surface, alongside `prism stats compare` and
// `prism checkin --compare`, and it reads through db.AbtestPairsAll.
//
// AbtestPairsAll took its metrics from a raw LEFT JOIN on spawn_outcome with
// no fallback, so for two finished, not-yet-cleaned-up legs it returned zero
// turns, zero tokens, and no duration — the whole of issue #2932, on the
// listing the A/B workflow starts from.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedAbtestLeg creates one leg of an A/B pair in the pre-cleanup shape: an
// agent_status row in a terminal state, a sessions row with no end_state and
// no ended_at (those are cleanup's writes), a spawn_inputs row carrying the
// shared pair id, and token-bearing events.
func seedAbtestLeg(t *testing.T, d *db.DB, sessionName, pairID, profile string, startedAt time.Time, outputTokens int) string {
	t.Helper()
	iid := seedTerminalSession(t, d, sessionName, startedAt)
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:   iid,
		AbtestPairID: &pairID,
		ProfileName:  &profile,
		CreatedAt:    startedAt.UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs %q: %v", sessionName, err)
	}
	writeEventAt(t, d, sessionName, iid, "msg_assistant",
		assistantPayload(100, outputTokens, 0, 0, 1.25), startedAt.Add(time.Minute))
	writeEventAt(t, d, sessionName, iid, "state_change",
		`{"state":"finished"}`, startedAt.Add(10*time.Minute))
	return iid
}

// TestAbtestPairsAll_ReportsAggregatesBeforeCleanup is the regression test:
// both legs are finished and reviewed, neither is cleaned up, and the listing
// must still carry their metrics.
func TestAbtestPairsAll_ReportsAggregatesBeforeCleanup(t *testing.T) {
	d := openTestDB(t)
	const pairID = "pair-2932-listing"
	startedAt := time.Now().Add(-40 * time.Minute)

	iidA := seedAbtestLeg(t, d, "repo@2932-list-a", pairID, "max", startedAt, 17024)
	seedAbtestLeg(t, d, "repo@2932-list-b", pairID, "standard", startedAt.Add(time.Second), 9803)

	// The review-complete write creates the stub row on leg A.
	if err := d.UpdateSpawnOutcomeReviewResult(iidA, "pass", 5, 0); err != nil {
		t.Fatalf("UpdateSpawnOutcomeReviewResult: %v", err)
	}

	pairs, err := d.AbtestPairsAll()
	if err != nil {
		t.Fatalf("AbtestPairsAll: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs, want 1", len(pairs))
	}
	p := pairs[0]

	if p.TokensOutputA == nil || *p.TokensOutputA != 17024 {
		t.Errorf("TokensOutputA = %v, want 17024", p.TokensOutputA)
	}
	if p.TokensOutputB == nil || *p.TokensOutputB != 9803 {
		t.Errorf("TokensOutputB = %v, want 9803", p.TokensOutputB)
	}
	if p.TurnsA == nil || *p.TurnsA != 1 {
		t.Errorf("TurnsA = %v, want 1", p.TurnsA)
	}
	if p.EndStateA == nil || *p.EndStateA != "finished" {
		t.Errorf("EndStateA = %v, want \"finished\"", p.EndStateA)
	}
	if p.DurationMsA == nil {
		t.Fatal("DurationMsA = nil, want the start→terminal-transition interval")
	}
	if want := (10 * time.Minute).Milliseconds(); *p.DurationMsA != want {
		t.Errorf("DurationMsA = %d, want %d", *p.DurationMsA, want)
	}
}

// TestAbtestPairsAll_PersistedRowWins pins the fast path: once cleanup has
// written the aggregates, the join answers and no recomputation replaces it.
// agent_events is pruned at 90 days and spawn_outcome is not, so for an old
// pair the persisted row is the only surviving record.
func TestAbtestPairsAll_PersistedRowWins(t *testing.T) {
	d := openTestDB(t)
	const pairID = "pair-2932-persisted"
	startedAt := time.Now().Add(-40 * time.Minute)

	iidA := seedAbtestLeg(t, d, "repo@2932-persist-a", pairID, "max", startedAt, 500)
	seedAbtestLeg(t, d, "repo@2932-persist-b", pairID, "standard", startedAt.Add(time.Second), 400)
	if err := d.WriteSpawnOutcome(iidA); err != nil {
		t.Fatalf("WriteSpawnOutcome: %v", err)
	}

	// An event the persisted row does not know about stands in for the
	// inverse case (rows pruned out from under a persisted row).
	writeEventAt(t, d, "repo@2932-persist-a", iidA, "msg_assistant",
		assistantPayload(1, 999, 0, 0, 9.5), startedAt.Add(11*time.Minute))

	pairs, err := d.AbtestPairsAll()
	if err != nil {
		t.Fatalf("AbtestPairsAll: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs, want 1", len(pairs))
	}
	if pairs[0].TokensOutputA == nil || *pairs[0].TokensOutputA != 500 {
		t.Errorf("TokensOutputA = %v, want 500 (the persisted row must win)", pairs[0].TokensOutputA)
	}
}

// TestAbtestPairsAll_LiveLegHasNoMetrics is the over-broad-fix negative: a leg
// that is still running must not report a half-finished total as if it were
// final.
func TestAbtestPairsAll_LiveLegHasNoMetrics(t *testing.T) {
	d := openTestDB(t)
	const pairID = "pair-2932-live"
	startedAt := time.Now().Add(-5 * time.Minute)

	sessionName := "repo@2932-live-leg"
	iid := uuid.New().String()
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   startedAt,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	pid := pairID
	profile := "max"
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:   iid,
		AbtestPairID: &pid,
		ProfileName:  &profile,
		CreatedAt:    startedAt.UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}
	writeEventAt(t, d, sessionName, iid, "msg_assistant",
		assistantPayload(10, 20, 0, 0, 0.01), startedAt.Add(time.Minute))

	pairs, err := d.AbtestPairsAll()
	if err != nil {
		t.Fatalf("AbtestPairsAll: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs, want 1", len(pairs))
	}
	if pairs[0].EndStateA != nil {
		t.Errorf("EndStateA = %v for a live leg, want nil", pairs[0].EndStateA)
	}
	if pairs[0].DurationMsA != nil {
		t.Errorf("DurationMsA = %v for a live leg, want nil", pairs[0].DurationMsA)
	}
}
