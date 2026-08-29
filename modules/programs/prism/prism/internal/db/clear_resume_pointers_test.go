package db_test

// Tests for ClearAllResumePointers.
//
// ClearAllResumePointers wipes agent_status.harness_session_id on every row
// so that the next `prism switch` into a previously-active project does not
// resume the pre-reset pi conversation. It is the DB-side half of the
// `prism reset` resume-wipe; the FS-side half lives in cmd/reset.go.

import (
	"testing"
)

// TestClearAllResumePointers_ClearsEveryRow verifies that ClearAllResumePointers
// sets harness_session_id to NULL on every row that previously held a value,
// regardless of whether the row is active or already ended.
func TestClearAllResumePointers_ClearsEveryRow(t *testing.T) {
	d := openTestDB(t)

	// Three sessions: two active (one with a SID, one without), one ended (with a SID).
	// All three must come out with harness_session_id == nil.
	active1 := "repo@active1"
	active2 := "repo@active2"
	ended := "repo@ended"

	_ = insertActiveSession(t, d, active1, "repo", "/wt/active1")
	_ = insertActiveSession(t, d, active2, "repo", "/wt/active2")
	_ = insertActiveSession(t, d, ended, "repo", "/wt/ended")

	// Plant a HarnessSessionID on active1 and ended; leave active2 NULL.
	if err := d.UpdateHarnessSessionID(active1, "019e00ed-aaaa-bbbb-cccc-111111111111"); err != nil {
		t.Fatalf("UpdateHarnessSessionID active1: %v", err)
	}
	if err := d.UpdateHarnessSessionID(ended, "019e00ed-aaaa-bbbb-cccc-222222222222"); err != nil {
		t.Fatalf("UpdateHarnessSessionID ended: %v", err)
	}

	// Pre-condition assertions.
	st1, _ := d.CurrentStatus(active1)
	if st1 == nil || st1.HarnessSessionID == nil || *st1.HarnessSessionID == "" {
		t.Fatalf("pre-condition: active1 HarnessSessionID = %v, want non-empty", st1)
	}
	st2, _ := d.CurrentStatus(active2)
	if st2 == nil || st2.HarnessSessionID != nil {
		t.Fatalf("pre-condition: active2 HarnessSessionID = %v, want nil", st2)
	}
	stE, _ := d.CurrentStatus(ended)
	if stE == nil || stE.HarnessSessionID == nil {
		t.Fatalf("pre-condition: ended HarnessSessionID nil, want non-empty")
	}

	// Run the clear.
	n, err := d.ClearAllResumePointers()
	if err != nil {
		t.Fatalf("ClearAllResumePointers: %v", err)
	}
	// Two rows actually had a value; the count reports those.
	if n != 2 {
		t.Errorf("ClearAllResumePointers returned n=%d, want 2 (only previously-non-NULL rows are counted)", n)
	}

	// Post-condition: every row has harness_session_id == nil.
	for _, name := range []string{active1, active2, ended} {
		st, err := d.CurrentStatus(name)
		if err != nil {
			t.Fatalf("CurrentStatus %q: %v", name, err)
		}
		if st == nil {
			t.Fatalf("CurrentStatus %q: row missing", name)
		}
		if st.HarnessSessionID != nil {
			t.Errorf("session %q: HarnessSessionID = %q, want nil after ClearAllResumePointers",
				name, *st.HarnessSessionID)
		}
	}
}

// TestClearAllResumePointers_EmptyTable verifies the no-op case: when no rows
// exist, ClearAllResumePointers returns (0, nil).
func TestClearAllResumePointers_EmptyTable(t *testing.T) {
	d := openTestDB(t)

	n, err := d.ClearAllResumePointers()
	if err != nil {
		t.Fatalf("ClearAllResumePointers on empty table: %v", err)
	}
	if n != 0 {
		t.Errorf("ClearAllResumePointers on empty table returned n=%d, want 0", n)
	}
}

// TestClearAllResumePointers_AllNullTable verifies that when every row already
// has harness_session_id NULL, ClearAllResumePointers returns (0, nil) — the
// WHERE clause filters out rows with nothing to clear.
func TestClearAllResumePointers_AllNullTable(t *testing.T) {
	d := openTestDB(t)

	_ = insertActiveSession(t, d, "repo@no-sid-1", "repo", "/wt/1")
	_ = insertActiveSession(t, d, "repo@no-sid-2", "repo", "/wt/2")

	n, err := d.ClearAllResumePointers()
	if err != nil {
		t.Fatalf("ClearAllResumePointers: %v", err)
	}
	if n != 0 {
		t.Errorf("ClearAllResumePointers returned n=%d, want 0 when all rows already NULL", n)
	}
}

// TestMarkAllEnded_DoesNotClearHarnessSessionID is a contract test for the
// MarkAllEnded ↔ ClearAllResumePointers separation. MarkAllEnded
// must keep its narrow responsibility (end every row); it must NOT mutate
// harness_session_id — that is ClearAllResumePointers' job. If a future
// refactor folds the two together, that is a deliberate design decision and
// this test should be deleted with explicit acknowledgement; an accidental
// change should fail here.
func TestMarkAllEnded_DoesNotClearHarnessSessionID(t *testing.T) {
	d := openTestDB(t)

	const name = "repo@end-but-keep-sid"
	_ = insertActiveSession(t, d, name, "repo", "/wt/end")
	const sid = "019e00ed-7777-8888-9999-aaaaaaaaaaaa"
	if err := d.UpdateHarnessSessionID(name, sid); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}

	if _, err := d.MarkAllEnded(); err != nil {
		t.Fatalf("MarkAllEnded: %v", err)
	}

	st, _ := d.CurrentStatus(name)
	if st == nil {
		t.Fatalf("CurrentStatus: row missing")
	}
	if st.EndedAt == nil {
		t.Errorf("EndedAt = nil after MarkAllEnded, want non-nil")
	}
	if st.HarnessSessionID == nil {
		t.Errorf("HarnessSessionID = nil after MarkAllEnded; MarkAllEnded must NOT clear it (issue #1947)")
	} else if *st.HarnessSessionID != sid {
		t.Errorf("HarnessSessionID = %q, want %q (unchanged by MarkAllEnded)", *st.HarnessSessionID, sid)
	}
}

// TestClearAllResumePointers_LeavesOtherColumnsAlone verifies the targeted
// nature of the wipe: state, ended_at, instance_id, last_seen, etc. are all
// preserved. Only harness_session_id changes.
func TestClearAllResumePointers_LeavesOtherColumnsAlone(t *testing.T) {
	d := openTestDB(t)

	const name = "repo@only-sid"
	iid := insertActiveSession(t, d, name, "repo", "/wt/only-sid")
	if err := d.UpdateHarnessSessionID(name, "019e00ed-1111-2222-3333-444444444444"); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}

	before, _ := d.CurrentStatus(name)
	if before == nil {
		t.Fatalf("CurrentStatus before: row missing")
	}

	if _, err := d.ClearAllResumePointers(); err != nil {
		t.Fatalf("ClearAllResumePointers: %v", err)
	}

	after, _ := d.CurrentStatus(name)
	if after == nil {
		t.Fatalf("CurrentStatus after: row missing")
	}

	if after.HarnessSessionID != nil {
		t.Errorf("HarnessSessionID = %q, want nil", *after.HarnessSessionID)
	}
	if after.State != before.State {
		t.Errorf("State changed: before=%q, after=%q", before.State, after.State)
	}
	if after.SessionName != before.SessionName {
		t.Errorf("SessionName changed: before=%q, after=%q", before.SessionName, after.SessionName)
	}
	if after.Repo != before.Repo {
		t.Errorf("Repo changed: before=%q, after=%q", before.Repo, after.Repo)
	}
	if after.Worktree != before.Worktree {
		t.Errorf("Worktree changed: before=%q, after=%q", before.Worktree, after.Worktree)
	}
	if after.InstanceID == nil || *after.InstanceID != iid {
		t.Errorf("InstanceID = %v, want %q", after.InstanceID, iid)
	}
	// EndedAt must remain nil (session was active).
	if after.EndedAt != nil {
		t.Errorf("EndedAt = %v, want nil (session was active)", after.EndedAt)
	}
}
