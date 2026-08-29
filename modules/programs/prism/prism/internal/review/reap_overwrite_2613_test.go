package review_test

// reap_overwrite_2613_test.go — a cleanup path must not claim a close it did
// not perform.
//
// `db.SessionEndCauses` returns the LATEST session_reaped event for a session.
// `db.SetEnded` guards with `AND ended_at IS NULL`, so a second cleanup over an
// already-closed row does not close it. If the reap record were unguarded, that
// second cleanup would still write an event claiming it did, and the reader
// would return the false cause as THE cause.
//
// The reachable sequence, all on the routine path:
//
//  1. review-qa fails its readiness gate. The gate records `readiness_gate`
//     and closes the row in state "error".
//  2. The work completes and the coordinator runs
//     `prism cleanup --yes --session <worker>`.
//  3. That cascades to CleanupReviewSessionsForParent, whose
//     GroupMembersForParent returns every member row across EVERY round —
//     including the row closed in step 1.
//
// Without the guard the row then reports "force-terminated — a cleanup of the
// parent worker session cascaded to this review agent", which is false. State
// "error" is not self-explaining, so the state-precedence branch does not
// rescue it. A wrong cause is worse than the disjunction the recorded cause
// replaced.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

func TestCleanupAgentSession_DoesNotOverwriteAnEarlierCause(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@2613-overwrite~review-1-review-qa"

	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Step 1: the readiness gate closes the row with its own cause.
	review.CleanupAgentSessionForTest(d, session, db.ReapCauseReadinessGate, "not ready within 30s")

	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus after the gate: %v", err)
	}
	if st == nil || st.EndedAt == nil || st.State != "error" {
		t.Fatalf("row after the readiness gate: want state=error with ended_at set, got %+v", st)
	}

	// Steps 2 and 3: the parent-cleanup cascade runs over the same row.
	review.CleanupAgentSessionForTest(d, session, db.ReapCauseParentCleanup)

	causes, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	got := causes[session]
	if got.Cause != db.ReapCauseReadinessGate {
		t.Errorf("Cause = %q, want %q — a later cleanup that did not close the row must not overwrite the true cause",
			got.Cause, db.ReapCauseReadinessGate)
	}
	if got.Detail != "not ready within 30s" {
		t.Errorf("Detail = %q, want the readiness-gate detail", got.Detail)
	}

	// And the report must still name the readiness gate, not the cascade.
	sessions := []string{session}
	status := review.ClassifyRoundWithCauses(
		review.AgentsFromSessionsForTest(sessions), sessions,
		map[string]db.GroupMemberResult{}, // GroupResults drops the closed row
		map[string]db.Status{session: *mustStatus(t, d, session)},
		causes,
	)
	if len(status.Missing) != 1 {
		t.Fatalf("Missing = %d entries, want 1", len(status.Missing))
	}
	if status.Missing[0].Class != review.NoVerdictNotReady {
		t.Errorf("Class = %q, want %q", status.Missing[0].Class, review.NoVerdictNotReady)
	}
}

// TestCleanupAgentSession_RecordsWhenItActuallyCloses is the positive
// counterpart: the guard must not suppress the record on the path that really
// does close the row.
func TestCleanupAgentSession_RecordsWhenItActuallyCloses(t *testing.T) {
	d := openTestDB(t)
	const session = "prism-test@2613-firstclose~review-1-review-code"

	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	review.CleanupAgentSessionForTest(d, session, db.ReapCauseParentCleanup)

	causes, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if causes[session].Cause != db.ReapCauseParentCleanup {
		t.Errorf("Cause = %q, want %q — the guard must not suppress the record on the closing path",
			causes[session].Cause, db.ReapCauseParentCleanup)
	}
}

func mustStatus(t *testing.T, d *db.DB, session string) *db.Status {
	t.Helper()
	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus(%s): %v", session, err)
	}
	if st == nil {
		t.Fatalf("CurrentStatus(%s): no row", session)
	}
	return st
}
