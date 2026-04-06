package cmd

// Unit tests for `prism reset` DB cleanup path.
//
// TestResetMarkDBEnded verifies that resetMarkDBEnded marks every non-ended row
// in agent_status as ended with a non-zero ended_at value, and that rows which
// were already ended are left untouched.

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestResetMarkDBEnded_MarksAllNonEndedRows is the primary AC-10 test:
// all non-ended rows in agent_status are updated to ended with a non-zero ended_at.
func TestResetMarkDBEnded_MarksAllNonEndedRows(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Seed three active sessions with different states.
	sessions := []struct {
		name  string
		state string
	}{
		{"repo@main", "idle"},
		{"repo@feature", "running"},
		{"repo@bugfix", "waiting"},
	}
	for _, s := range sessions {
		if err := d.UpsertStatus(s.name, "repo", "/worktree/"+s.name, s.state, nil, nil); err != nil {
			t.Fatalf("UpsertStatus %q: %v", s.name, err)
		}
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Run the DB cleanup step.
	if err := resetMarkDBEnded(); err != nil {
		t.Fatalf("resetMarkDBEnded returned error: %v", err)
	}

	// Verify every row is now ended with a non-zero ended_at.
	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	for _, s := range sessions {
		status, err := d2.CurrentStatus(s.name)
		if err != nil {
			t.Fatalf("CurrentStatus %q: %v", s.name, err)
		}
		if status == nil {
			t.Fatalf("CurrentStatus %q: row missing", s.name)
		}
		if status.EndedAt == nil {
			t.Errorf("session %q: ended_at is nil — should have been set by reset", s.name)
		} else if status.EndedAt.UnixMilli() == 0 {
			t.Errorf("session %q: ended_at is zero — should be non-zero", s.name)
		}
	}
}

// TestResetMarkDBEnded_NoActiveRows verifies the no-op case:
// when agent_status contains no non-ended rows, resetMarkDBEnded returns nil.
func TestResetMarkDBEnded_NoActiveRows(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Seed one session then immediately mark it as ended.
	session := "repo@already-ended"
	if err := d.UpsertStatus(session, "repo", "/worktree", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetEnded(session); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Should succeed even with nothing to update.
	if err := resetMarkDBEnded(); err != nil {
		t.Fatalf("resetMarkDBEnded returned error on empty active set: %v", err)
	}
}

// TestResetMarkDBEnded_EmptyDB verifies the edge case:
// when agent_status is empty, resetMarkDBEnded returns nil without error.
func TestResetMarkDBEnded_EmptyDB(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	if err := resetMarkDBEnded(); err != nil {
		t.Fatalf("resetMarkDBEnded returned error on empty DB: %v", err)
	}
}

// TestResetMarkDBEnded_AlreadyEndedRowsUntouched verifies that rows with
// ended_at already set are left alone when resetMarkDBEnded runs.
func TestResetMarkDBEnded_AlreadyEndedRowsUntouched(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	endedSession := "repo@done"
	activeSession := "repo@active"

	if err := d.UpsertStatus(endedSession, "repo", "/wt/done", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus ended: %v", err)
	}
	if err := d.SetEnded(endedSession); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	// Record the original ended_at so we can confirm it didn't change.
	origStatus, err := d.CurrentStatus(endedSession)
	if err != nil || origStatus == nil || origStatus.EndedAt == nil {
		t.Fatalf("could not read original ended_at for %q", endedSession)
	}
	origEndedAt := origStatus.EndedAt.UnixMilli()

	if err := d.UpsertStatus(activeSession, "repo", "/wt/active", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus active: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	if err := resetMarkDBEnded(); err != nil {
		t.Fatalf("resetMarkDBEnded: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()

	// The active session must now be ended.
	active, err := d2.CurrentStatus(activeSession)
	if err != nil || active == nil {
		t.Fatalf("CurrentStatus active: %v", err)
	}
	if active.EndedAt == nil {
		t.Errorf("active session %q was not marked ended", activeSession)
	}

	// The already-ended row must retain its original ended_at value (not updated).
	ended, err := d2.CurrentStatus(endedSession)
	if err != nil || ended == nil {
		t.Fatalf("CurrentStatus ended: %v", err)
	}
	if ended.EndedAt == nil {
		t.Fatalf("ended session %q lost its ended_at", endedSession)
	}
	if ended.EndedAt.UnixMilli() != origEndedAt {
		t.Errorf("ended session %q ended_at changed: got %d, want %d",
			endedSession, ended.EndedAt.UnixMilli(), origEndedAt)
	}
}
