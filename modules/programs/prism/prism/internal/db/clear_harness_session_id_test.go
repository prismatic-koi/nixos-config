package db_test

// Tests for ClearHarnessSessionID (issue #2035).
//
// ClearHarnessSessionID nulls agent_status.harness_session_id for a specific
// session AND for any ~review-N-* child sessions, mirroring SetEnded's
// LIKE-escape semantics. It is called from the `prism cleanup` paths so that
// re-spawning a NEW session on the SAME branch name does not pick up the
// cleaned session's stale harness_session_id and silently resume the dead
// pi conversation.

import (
	"testing"
)

// TestClearHarnessSessionID_NullsTargetRow verifies the basic positive case:
// after ClearHarnessSessionID, the named session's harness_session_id is NULL.
func TestClearHarnessSessionID_NullsTargetRow(t *testing.T) {
	d := openTestDB(t)

	const name = "repo@branch"
	_ = insertActiveSession(t, d, name, "repo", "/wt/branch")
	const sid = "019e72d2-446a-712f-baea-7abc9e7ce7df"
	if err := d.UpdateHarnessSessionID(name, sid); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}

	// Pre-condition.
	pre, _ := d.CurrentStatus(name)
	if pre == nil || pre.HarnessSessionID == nil || *pre.HarnessSessionID != sid {
		t.Fatalf("pre-condition: HarnessSessionID = %v, want %q", pre, sid)
	}

	if err := d.ClearHarnessSessionID(name); err != nil {
		t.Fatalf("ClearHarnessSessionID: %v", err)
	}

	post, err := d.CurrentStatus(name)
	if err != nil {
		t.Fatalf("CurrentStatus after: %v", err)
	}
	if post == nil {
		t.Fatalf("CurrentStatus after: row missing")
	}
	if post.HarnessSessionID != nil {
		t.Errorf("HarnessSessionID = %q, want nil", *post.HarnessSessionID)
	}
}

// TestClearHarnessSessionID_CascadesToReviewChildren verifies the LIKE pattern
// parity with SetEnded: a session and its `~review-N-<agent>` children must
// all have their harness_session_id cleared in one call, so cleanup of a
// worker also forgets the pi resume pointer for any review children that
// were spawned underneath it.
func TestClearHarnessSessionID_CascadesToReviewChildren(t *testing.T) {
	d := openTestDB(t)

	const parent = "repo@feature"
	child1 := parent + "~review-1-architect"
	child2 := parent + "~review-2-skeptic"

	_ = insertActiveSession(t, d, parent, "repo", "/wt/feature")
	_ = insertActiveSession(t, d, child1, "repo", "/wt/feature")
	_ = insertActiveSession(t, d, child2, "repo", "/wt/feature")

	if err := d.UpdateHarnessSessionID(parent, "019e0000-0000-0000-0000-000000000001"); err != nil {
		t.Fatalf("UpdateHarnessSessionID parent: %v", err)
	}
	if err := d.UpdateHarnessSessionID(child1, "019e0000-0000-0000-0000-000000000002"); err != nil {
		t.Fatalf("UpdateHarnessSessionID child1: %v", err)
	}
	if err := d.UpdateHarnessSessionID(child2, "019e0000-0000-0000-0000-000000000003"); err != nil {
		t.Fatalf("UpdateHarnessSessionID child2: %v", err)
	}

	if err := d.ClearHarnessSessionID(parent); err != nil {
		t.Fatalf("ClearHarnessSessionID: %v", err)
	}

	for _, name := range []string{parent, child1, child2} {
		st, err := d.CurrentStatus(name)
		if err != nil {
			t.Fatalf("CurrentStatus %q: %v", name, err)
		}
		if st == nil {
			t.Fatalf("CurrentStatus %q: row missing", name)
		}
		if st.HarnessSessionID != nil {
			t.Errorf("session %q: HarnessSessionID = %q, want nil (LIKE cascade)", name, *st.HarnessSessionID)
		}
	}
}

// TestClearHarnessSessionID_DoesNotTouchSiblings verifies the targeted nature
// of the clear: other sessions whose names do NOT match the literal name or
// the `~review-%` pattern must be left alone.
func TestClearHarnessSessionID_DoesNotTouchSiblings(t *testing.T) {
	d := openTestDB(t)

	const target = "repo@branch"
	const other = "repo@other-branch"
	_ = insertActiveSession(t, d, target, "repo", "/wt/branch")
	_ = insertActiveSession(t, d, other, "repo", "/wt/other")

	const targetSID = "019e0000-aaaa-aaaa-aaaa-000000000001"
	const otherSID = "019e0000-bbbb-bbbb-bbbb-000000000002"
	if err := d.UpdateHarnessSessionID(target, targetSID); err != nil {
		t.Fatalf("UpdateHarnessSessionID target: %v", err)
	}
	if err := d.UpdateHarnessSessionID(other, otherSID); err != nil {
		t.Fatalf("UpdateHarnessSessionID other: %v", err)
	}

	if err := d.ClearHarnessSessionID(target); err != nil {
		t.Fatalf("ClearHarnessSessionID: %v", err)
	}

	stOther, _ := d.CurrentStatus(other)
	if stOther == nil || stOther.HarnessSessionID == nil || *stOther.HarnessSessionID != otherSID {
		t.Errorf("sibling session %q HarnessSessionID = %v, want %q (must not be touched)", other, stOther.HarnessSessionID, otherSID)
	}
}

// TestClearHarnessSessionID_Idempotent verifies that calling against a row
// whose harness_session_id is already NULL is a no-op and returns nil.
func TestClearHarnessSessionID_Idempotent(t *testing.T) {
	d := openTestDB(t)

	const name = "repo@no-sid"
	_ = insertActiveSession(t, d, name, "repo", "/wt/no-sid")

	if err := d.ClearHarnessSessionID(name); err != nil {
		t.Errorf("ClearHarnessSessionID on NULL-already row: %v, want nil", err)
	}
	if err := d.ClearHarnessSessionID(name); err != nil {
		t.Errorf("ClearHarnessSessionID second call: %v, want nil", err)
	}

	st, _ := d.CurrentStatus(name)
	if st == nil {
		t.Fatal("row vanished")
	}
	if st.HarnessSessionID != nil {
		t.Errorf("HarnessSessionID = %q, want nil", *st.HarnessSessionID)
	}
}

// TestClearHarnessSessionID_MissingSessionIsNoOp verifies that calling
// against a session that does not exist in the DB returns nil — cleanup must
// not error out when there is nothing to clear.
func TestClearHarnessSessionID_MissingSessionIsNoOp(t *testing.T) {
	d := openTestDB(t)

	if err := d.ClearHarnessSessionID("repo@does-not-exist"); err != nil {
		t.Errorf("ClearHarnessSessionID on missing row: %v, want nil", err)
	}
}

// TestClearHarnessSessionID_LeavesOtherColumnsAlone verifies the targeted
// nature of the wipe: state, ended_at, instance_id, last_seen, etc. are all
// preserved. Only harness_session_id changes.
func TestClearHarnessSessionID_LeavesOtherColumnsAlone(t *testing.T) {
	d := openTestDB(t)

	const name = "repo@only-sid"
	iid := insertActiveSession(t, d, name, "repo", "/wt/only-sid")
	if err := d.UpdateHarnessSessionID(name, "019e0000-1111-2222-3333-444444444444"); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}

	before, _ := d.CurrentStatus(name)
	if before == nil {
		t.Fatalf("CurrentStatus before: row missing")
	}

	if err := d.ClearHarnessSessionID(name); err != nil {
		t.Fatalf("ClearHarnessSessionID: %v", err)
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
	if after.Worktree != before.Worktree {
		t.Errorf("Worktree changed: before=%q, after=%q", before.Worktree, after.Worktree)
	}
	if after.InstanceID == nil || *after.InstanceID != iid {
		t.Errorf("InstanceID = %v, want %q", after.InstanceID, iid)
	}
	if after.EndedAt != nil {
		t.Errorf("EndedAt = %v, want nil (session was active)", after.EndedAt)
	}
}

// TestClearHarnessSessionID_DoesNotTouchEndedAt verifies the orthogonality
// between SetEnded and ClearHarnessSessionID: the cleanup helper that calls
// both must see them independent of each other. In particular, calling
// ClearHarnessSessionID on a row that has not been ended must not stamp
// ended_at, and vice versa (covered by SetEnded's own tests).
func TestClearHarnessSessionID_DoesNotTouchEndedAt(t *testing.T) {
	d := openTestDB(t)

	const name = "repo@orthogonal"
	_ = insertActiveSession(t, d, name, "repo", "/wt/orth")
	if err := d.UpdateHarnessSessionID(name, "019e0000-cccc-cccc-cccc-cccccccccccc"); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}

	if err := d.ClearHarnessSessionID(name); err != nil {
		t.Fatalf("ClearHarnessSessionID: %v", err)
	}

	st, _ := d.CurrentStatus(name)
	if st == nil {
		t.Fatal("row missing")
	}
	if st.EndedAt != nil {
		t.Errorf("EndedAt = %v, want nil (ClearHarnessSessionID must not stamp ended_at)", st.EndedAt)
	}
}
