package db_test

// profile_names_test.go — tests for AllProfileNames, the dashboard's batch
// lookup of spawn_inputs.profile_name keyed by instance_id (issue #2640).

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// TestAllProfileNames_ReturnsOnlyNonNullProfiles verifies that AllProfileNames
// returns an instance_id → profile_name map containing only rows with a
// non-NULL profile_name, and that a session spawned without --profile (or
// with no spawn_inputs row at all) is simply absent from the map.
func TestAllProfileNames_ReturnsOnlyNonNullProfiles(t *testing.T) {
	d := openTestDB(t)

	now := time.Now()

	iidHeavy := uuid.New().String()
	insertAbtestSessionForTest(t, d, "repo@heavy", iidHeavy, "", "heavy", now)

	iidNullProfile := uuid.New().String()
	insertAbtestSessionForTest(t, d, "repo@no-profile", iidNullProfile, "", "", now)

	// A session with no spawn_inputs row at all (pre-#2087, or the fixture
	// simply never inserts one).
	iidNoRow := uuid.New().String()
	if err := d.UpsertStatus("repo@no-spawn-inputs-row", "repo", "/wt/no-row", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  iidNoRow,
		SessionName: "repo@no-spawn-inputs-row",
		Repo:        "repo",
		Worktree:    "/wt/no-row",
		Harness:     "pi",
		StartedAt:   now,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	got, err := d.AllProfileNames()
	if err != nil {
		t.Fatalf("AllProfileNames: %v", err)
	}

	if want := "heavy"; got[iidHeavy] != want {
		t.Errorf("AllProfileNames()[%q] = %q, want %q", iidHeavy, got[iidHeavy], want)
	}
	if _, ok := got[iidNullProfile]; ok {
		t.Errorf("AllProfileNames() unexpectedly contains an entry for a NULL profile_name row: %v", got[iidNullProfile])
	}
	if _, ok := got[iidNoRow]; ok {
		t.Errorf("AllProfileNames() unexpectedly contains an entry for an instance with no spawn_inputs row: %v", got[iidNoRow])
	}
}
