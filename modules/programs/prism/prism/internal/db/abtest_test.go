package db_test

// abtest_test.go — tests for SessionsByAbtestPairID, AbtestPairsAll,
// and SpawnInputsByInstanceID.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// insertAbtestSessionForTest inserts the minimal set of rows needed by the
// abtest query surface:
//   - agent_status (for AbtestPairsForSessions compatibility)
//   - sessions
//   - spawn_inputs (with optional abtest_pair_id and profile_name)
func insertAbtestSessionForTest(t *testing.T, d *db.DB, sessionName, iid, pairIDVal, profile string, startedAt time.Time) {
	t.Helper()
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus %q: %v", sessionName, err)
	}
	sess := db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   startedAt,
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession %q: %v", sessionName, err)
	}
	si := db.SpawnInputs{
		InstanceID: iid,
		CreatedAt:  startedAt.UnixMilli(),
	}
	if pairIDVal != "" {
		si.AbtestPairID = strPtr(pairIDVal)
	}
	if profile != "" {
		si.ProfileName = strPtr(profile)
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs %q: %v", sessionName, err)
	}
}

// TestSessionsByAbtestPairID verifies that SessionsByAbtestPairID returns only
// the sessions sharing the given abtest_pair_id, in started_at ASC order.
func TestSessionsByAbtestPairID(t *testing.T) {
	d := openTestDB(t)

	const pairID = "test-pair-1111-2222-3333-444444444444"
	const otherPairID = "other-pair-5555-6666-7777-888888888888"

	now := time.Now()
	iidA := uuid.New().String()
	iidB := uuid.New().String()
	iidOther := uuid.New().String()

	insertAbtestSessionForTest(t, d, "repo@branch-A", iidA, pairID, "", now.Add(-2*time.Second))
	insertAbtestSessionForTest(t, d, "repo@branch-B", iidB, pairID, "", now.Add(-1*time.Second))
	insertAbtestSessionForTest(t, d, "repo@branch-other", iidOther, otherPairID, "", now)

	sessions, err := d.SessionsByAbtestPairID(pairID)
	if err != nil {
		t.Fatalf("SessionsByAbtestPairID: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	if sessions[0].SessionName != "repo@branch-A" {
		t.Errorf("sessions[0] = %q, want repo@branch-A", sessions[0].SessionName)
	}
	if sessions[1].SessionName != "repo@branch-B" {
		t.Errorf("sessions[1] = %q, want repo@branch-B", sessions[1].SessionName)
	}

	// Non-existent pair — should return empty slice, not error.
	none, err := d.SessionsByAbtestPairID("no-such-pair")
	if err != nil {
		t.Fatalf("SessionsByAbtestPairID(none): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("got %d sessions for non-existent pair, want 0", len(none))
	}
}

// TestAbtestPairsAll verifies the aggregated pair listing with metrics.
func TestAbtestPairsAll(t *testing.T) {
	d := openTestDB(t)

	const pairID = "pairs-all-test-aaaa-bbbb-cccc-dddddddddddd"
	now := time.Now()
	iidA := uuid.New().String()
	iidB := uuid.New().String()

	insertAbtestSessionForTest(t, d, "r@a-profileA", iidA, pairID, "profileA", now.Add(-2*time.Second))
	insertAbtestSessionForTest(t, d, "r@a-profileB", iidB, pairID, "profileB", now.Add(-1*time.Second))

	pairs, err := d.AbtestPairsAll()
	if err != nil {
		t.Fatalf("AbtestPairsAll: %v", err)
	}

	var found *db.AbtestPairRow
	for i := range pairs {
		if pairs[i].PairID == pairID {
			found = &pairs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("pair %q not found in AbtestPairsAll results", pairID)
	}
	if found.SessionNameA != "r@a-profileA" {
		t.Errorf("SessionNameA = %q, want r@a-profileA", found.SessionNameA)
	}
	if found.SessionNameB != "r@a-profileB" {
		t.Errorf("SessionNameB = %q, want r@a-profileB", found.SessionNameB)
	}
	if found.ProfileA != "profileA" {
		t.Errorf("ProfileA = %q, want profileA", found.ProfileA)
	}
	if found.ProfileB != "profileB" {
		t.Errorf("ProfileB = %q, want profileB", found.ProfileB)
	}
}

// TestSpawnInputsByInstanceID verifies that SpawnInputsByInstanceID returns the
// correct row and nil for unknown instance IDs.
func TestSpawnInputsByInstanceID(t *testing.T) {
	d := openTestDB(t)

	iid := uuid.New().String()
	const pairID = "spawn-inputs-test-pair-id"
	const profile = "test-profile"

	if err := d.UpsertStatus("r@si-test", "r", "/wt/si-test", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	sess := db.Session{
		InstanceID:  iid,
		SessionName: "r@si-test",
		Repo:        "r",
		Worktree:    "/wt/si-test",
		Harness:     "pi",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	si := db.SpawnInputs{
		InstanceID:   iid,
		AbtestPairID: strPtr(pairID),
		ProfileName:  strPtr(profile),
		CreatedAt:    time.Now().UnixMilli(),
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID(iid)
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want non-nil SpawnInputs")
	}
	if got.AbtestPairID == nil || *got.AbtestPairID != pairID {
		t.Errorf("AbtestPairID = %v, want %q", got.AbtestPairID, pairID)
	}
	if got.ProfileName == nil || *got.ProfileName != profile {
		t.Errorf("ProfileName = %v, want %q", got.ProfileName, profile)
	}

	// Missing.
	missing, err := d.SpawnInputsByInstanceID("non-existent-iid")
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID(missing): %v", err)
	}
	if missing != nil {
		t.Errorf("expected nil for non-existent instance_id, got %+v", missing)
	}
}
