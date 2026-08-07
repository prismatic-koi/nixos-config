package cmd

// cleanup_reap_cause_2613_test.go — applyDBLifecycleClears records why it
// closed the row, and only when it actually closed it (issue #2613).
//
// `prism cleanup` on an already-ended session is an explicitly supported
// idempotent no-op: SetEnded's `AND ended_at IS NULL` guard suppresses the
// UPDATE. The reap record carries the same guard, because SessionEndCauses
// returns the LATEST record — an unguarded write on a re-run, or on a review
// child that CleanupReviewSessionsForParent already closed, would replace the
// row's true cause with `cleanup_command`.

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

func openReapCleanupDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestApplyDBLifecycleClears_RecordsTheCleanupCause(t *testing.T) {
	d := openReapCleanupDB(t)
	const session = "prism-test@2613-cleanup"
	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	var result cleanupResult
	applyDBLifecycleClears(d, session, &result)

	causes, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if causes[session].Cause != db.ReapCauseCleanupCommand {
		t.Errorf("Cause = %q, want %q", causes[session].Cause, db.ReapCauseCleanupCommand)
	}
}

func TestApplyDBLifecycleClears_DoesNotOverwriteAnEarlierCause(t *testing.T) {
	d := openReapCleanupDB(t)
	const session = "prism-test@2613-cleanup-rerun~review-1-review-qa"
	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "error", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// A review path closed the row first and recorded its own cause.
	d.RecordReapBestEffort(session, db.ReapCauseMonitorTimeout, "deadline fired")
	if err := d.SetEnded(session); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	// The operator's cleanup then runs over the already-closed row.
	var result cleanupResult
	applyDBLifecycleClears(d, session, &result)

	causes, err := d.SessionEndCauses([]string{session})
	if err != nil {
		t.Fatalf("SessionEndCauses: %v", err)
	}
	if causes[session].Cause != db.ReapCauseMonitorTimeout {
		t.Errorf("Cause = %q, want %q — cleanup did not close this row, so it must not claim it did",
			causes[session].Cause, db.ReapCauseMonitorTimeout)
	}

	// The idempotent-cleanup contract is unchanged: the call still succeeds.
	if result.EndedAtStamped != true {
		t.Errorf("EndedAtStamped = %v, want true (idempotent no-op success)", result.EndedAtStamped)
	}
}
