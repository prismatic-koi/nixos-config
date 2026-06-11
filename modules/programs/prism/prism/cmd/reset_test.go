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
	if _, err := resetMarkDBEnded(); err != nil {
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
		// state is intentionally NOT updated by MarkAllEnded — ended_at IS NULL
		// is the canonical "active session" filter; state retains its last known
		// value. Verify the original state is preserved, not overwritten.
		if status.State != s.state {
			t.Errorf("session %q: state changed from %q to %q — MarkAllEnded must not modify state",
				s.name, s.state, status.State)
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
	if _, err := resetMarkDBEnded(); err != nil {
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

	if _, err := resetMarkDBEnded(); err != nil {
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

	if _, err := resetMarkDBEnded(); err != nil {
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

// TestResetMarkDBEnded_SnapshotsResumePointersBeforeClear verifies the issue
// #2220 capture-before-clear contract: resetMarkDBEnded returns the
// (sessionName, worktree, harness_session_id) snapshot of every row that
// carried a resume pointer — including already-ended rows, since
// ClearAllResumePointers wipes the column on every row — and the DB column is
// NULL afterwards. Rows with no harness_session_id are excluded from the
// snapshot.
func TestResetMarkDBEnded_SnapshotsResumePointersBeforeClear(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Active row with a resume pointer.
	if err := d.UpsertStatus("repo@active", "repo", "/wt/active", "running", nil, strPtr("sid-active")); err != nil {
		t.Fatalf("UpsertStatus active: %v", err)
	}
	// Already-ended row with a resume pointer — must still be snapshotted.
	if err := d.UpsertStatus("repo@ended", "repo", "/wt/ended", "idle", nil, strPtr("sid-ended")); err != nil {
		t.Fatalf("UpsertStatus ended: %v", err)
	}
	if err := d.SetEnded("repo@ended"); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	// Row with no resume pointer — must be excluded from the snapshot.
	if err := d.UpsertStatus("repo@nosid", "repo", "/wt/nosid", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus nosid: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	pointers, err := resetMarkDBEnded()
	if err != nil {
		t.Fatalf("resetMarkDBEnded: %v", err)
	}

	got := map[string]piResumePointer{}
	for _, p := range pointers {
		got[p.sessionName] = p
	}
	if len(got) != 2 {
		t.Fatalf("snapshot has %d pointer(s), want 2: %+v", len(got), pointers)
	}
	if p := got["repo@active"]; p.worktree != "/wt/active" || p.harnessSessionID != "sid-active" {
		t.Errorf("repo@active pointer = %+v, want worktree=/wt/active sid=sid-active", p)
	}
	if p := got["repo@ended"]; p.worktree != "/wt/ended" || p.harnessSessionID != "sid-ended" {
		t.Errorf("repo@ended pointer = %+v, want worktree=/wt/ended sid=sid-ended", p)
	}
	if _, ok := got["repo@nosid"]; ok {
		t.Errorf("repo@nosid must not appear in the snapshot (no resume pointer): %+v", pointers)
	}

	// And the DB-side pointers are cleared AFTER the snapshot was taken.
	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	for _, name := range []string{"repo@active", "repo@ended"} {
		st, err := d2.CurrentStatus(name)
		if err != nil || st == nil {
			t.Fatalf("CurrentStatus %q: %v", name, err)
		}
		if st.HarnessSessionID != nil {
			t.Errorf("session %q: harness_session_id = %q, want NULL after reset",
				name, *st.HarnessSessionID)
		}
	}
}
