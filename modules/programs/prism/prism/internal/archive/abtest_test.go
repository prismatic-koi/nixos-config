package archive_test

// abtest_test.go — tests for LoadAbtestPair.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/archive"
	"github.com/prismatic-koi/prism/internal/db"
)

// openTestDBForArchive opens an in-memory DB for archive tests.
// It mirrors the pattern used in the db package test helpers.
func openTestDBForArchive(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func strPtrArchive(s string) *string { return &s }

func insertMinimalSession(t *testing.T, d *db.DB, sessionName, iid, pairID, profile string, startedAt time.Time) {
	t.Helper()
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus %q: %v", sessionName, err)
	}
	sess := db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "opencode",
		StartedAt:   startedAt,
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession %q: %v", sessionName, err)
	}
	si := db.SpawnInputs{
		InstanceID:   iid,
		AbtestPairID: strPtrArchive(pairID),
		CreatedAt:    startedAt.UnixMilli(),
	}
	if profile != "" {
		si.ProfileName = strPtrArchive(profile)
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs %q: %v", sessionName, err)
	}
}

// TestLoadAbtestPair_BothPresent verifies that LoadAbtestPair returns both
// siblings when both are present in the DB.
func TestLoadAbtestPair_BothPresent(t *testing.T) {
	d := openTestDBForArchive(t)

	const pairID = "load-abtest-pair-both-present"
	now := time.Now()
	iidA := uuid.New().String()
	iidB := uuid.New().String()

	insertMinimalSession(t, d, "repo@branch-A", iidA, pairID, "profileA", now.Add(-2*time.Second))
	insertMinimalSession(t, d, "repo@branch-B", iidB, pairID, "profileB", now.Add(-1*time.Second))

	pair, err := archive.LoadAbtestPair(d, pairID)
	if err != nil {
		t.Fatalf("LoadAbtestPair: %v", err)
	}
	if pair.PairID != pairID {
		t.Errorf("PairID = %q, want %q", pair.PairID, pairID)
	}
	if pair.MissingA {
		t.Error("MissingA should be false when session A is present")
	}
	if pair.MissingB {
		t.Error("MissingB should be false when session B is present")
	}
	if pair.SessionA == nil {
		t.Fatal("SessionA is nil")
	}
	if pair.SessionB == nil {
		t.Fatal("SessionB is nil")
	}
	if pair.SessionA.SessionName != "repo@branch-A" {
		t.Errorf("SessionA.SessionName = %q, want repo@branch-A", pair.SessionA.SessionName)
	}
	if pair.SessionB.SessionName != "repo@branch-B" {
		t.Errorf("SessionB.SessionName = %q, want repo@branch-B", pair.SessionB.SessionName)
	}
	if pair.InputsA == nil || pair.InputsA.AbtestPairID == nil || *pair.InputsA.AbtestPairID != pairID {
		t.Errorf("InputsA.AbtestPairID = %v, want %q", pair.InputsA, pairID)
	}
}

// TestLoadAbtestPair_OneSibling verifies that LoadAbtestPair handles the case
// where only one sibling exists (the other was cleaned up). MissingB is set.
func TestLoadAbtestPair_OneSibling(t *testing.T) {
	d := openTestDBForArchive(t)

	const pairID = "load-abtest-pair-one-sibling"
	iidA := uuid.New().String()

	insertMinimalSession(t, d, "repo@branch-A-only", iidA, pairID, "", time.Now())
	// Note: no session B inserted.

	pair, err := archive.LoadAbtestPair(d, pairID)
	if err != nil {
		t.Fatalf("LoadAbtestPair: %v", err)
	}
	if pair.SessionA == nil {
		t.Fatal("SessionA is nil")
	}
	if !pair.MissingB {
		t.Error("MissingB should be true when session B is absent")
	}
	if pair.SessionB != nil {
		t.Errorf("SessionB should be nil when B is missing, got %+v", pair.SessionB)
	}
}

// TestLoadAbtestPair_NotFound verifies that LoadAbtestPair returns an error
// when the pairID does not exist.
func TestLoadAbtestPair_NotFound(t *testing.T) {
	d := openTestDBForArchive(t)

	_, err := archive.LoadAbtestPair(d, "no-such-pair-id")
	if err == nil {
		t.Error("expected error for non-existent pair, got nil")
	}
}
