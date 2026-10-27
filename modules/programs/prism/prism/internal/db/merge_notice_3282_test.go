package db_test

// Issue #3282: the pending notice of a merge-queue row. The terminal write
// of the watcher stores the coordinator notification in the row. The row
// keeps it until a watcher claims it, so a stop of the sidecar during the
// send does not lose it.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

const (
	noticeRepo    = "repo-a"
	noticeSession = "repo-a@main"
	noticeInst    = "inst-3282"
)

func pendingNoticePRs(t *testing.T, d *db.DB, session string) []int {
	t.Helper()
	notices, err := d.PendingMergeNotices(session)
	if err != nil {
		t.Fatalf("PendingMergeNotices: %v", err)
	}
	prs := make([]int, 0, len(notices))
	for _, n := range notices {
		prs = append(prs, n.PR)
	}
	return prs
}

func enqueueNoticeRow(t *testing.T, d *db.DB, pr int) {
	t.Helper()
	if _, err := d.EnqueueMerge(pr, noticeRepo, noticeSession, noticeInst, nil); err != nil {
		t.Fatalf("EnqueueMerge(%d): %v", pr, err)
	}
}

// seedV46DB creates a v46 DB with a merged pending_merges row and no
// pending_notice column.
func seedV46DB(t *testing.T, dbPath string) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v46 db: %v", err)
	}
	defer rawConn.Close()
	if _, err := rawConn.Exec(`
		CREATE TABLE pending_merges (
		  repo TEXT NOT NULL, pr INTEGER NOT NULL, session_name TEXT NOT NULL,
		  instance_id TEXT NOT NULL, queue_position INTEGER NOT NULL, status TEXT NOT NULL,
		  title TEXT, error TEXT, queued_at INTEGER NOT NULL, last_checked_at INTEGER,
		  merged_at INTEGER, ended_at INTEGER,
		  PRIMARY KEY (repo, pr)
		);
		INSERT INTO pending_merges (repo, pr, session_name, instance_id, queue_position, status, queued_at, merged_at, ended_at)
		  VALUES ('repo-a', 1, 'repo-a@main', 'inst-3282', 1, 'merged', 1, 2, 2);
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (46);
	`); err != nil {
		t.Fatalf("seed v46 db: %v", err)
	}
}

func TestMigration_V46ToV47_AddsPendingNotice(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v46.db")
	seedV46DB(t, dbPath)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := schemaVersionOf(t, d); v < 47 {
		t.Errorf("schema_version after migration: got %d, want >= 47", v)
	}
	if got := pendingNoticePRs(t, d, noticeSession); len(got) != 0 {
		t.Errorf("a row from before the migration has a pending notice: %v", got)
	}
	enqueueNoticeRow(t, d, 2)
	if changed, err := d.TerminateMergeWithNotice(2, noticeRepo, "merged", "", "PR #2 merged."); err != nil || !changed {
		t.Fatalf("TerminateMergeWithNotice on a migrated DB = %v, %v, want true, nil", changed, err)
	}
	if got := pendingNoticePRs(t, d, noticeSession); len(got) != 1 || got[0] != 2 {
		t.Errorf("pending notices = %v, want [2]", got)
	}
}

// TestMergeNotice_ClaimOnce checks that only one claim of a notice
// succeeds, so that two watchers do not both send it.
func TestMergeNotice_ClaimOnce(t *testing.T) {
	d := openTestDB(t)
	enqueueNoticeRow(t, d, 10)
	const text = "PR #10 merged. Please clean up the branch and worktree."
	if changed, err := d.TerminateMergeWithNotice(10, noticeRepo, "merged", "", text); err != nil || !changed {
		t.Fatalf("TerminateMergeWithNotice = %v, %v, want true, nil", changed, err)
	}
	notices, err := d.PendingMergeNotices(noticeSession)
	if err != nil || len(notices) != 1 {
		t.Fatalf("PendingMergeNotices = %+v, %v, want one notice", notices, err)
	}
	want := db.MergeNotice{Repo: noticeRepo, PR: 10, SessionName: noticeSession, InstanceID: noticeInst, Text: text}
	if notices[0] != want {
		t.Errorf("notice = %+v, want %+v", notices[0], want)
	}

	if ok, err := d.ClaimMergeNotice(10, noticeRepo, text); err != nil || !ok {
		t.Fatalf("first claim = %v, %v, want true, nil", ok, err)
	}
	if ok, err := d.ClaimMergeNotice(10, noticeRepo, text); err != nil || ok {
		t.Errorf("second claim = %v, %v, want false, nil", ok, err)
	}
	if got := pendingNoticePRs(t, d, noticeSession); len(got) != 0 {
		t.Errorf("pending notices after the claim = %v, want none", got)
	}
}

// TestMergeNotice_ReturnAfterClaim checks that a claimed notice can go back
// to its row one time, for the next watcher.
func TestMergeNotice_ReturnAfterClaim(t *testing.T) {
	for _, status := range []string{"merged", "failed"} {
		t.Run(status, func(t *testing.T) {
			d := openTestDB(t)
			enqueueNoticeRow(t, d, 11)
			const text = "PR #11 outcome."
			if _, err := d.TerminateMergeWithNotice(11, noticeRepo, status, "", text); err != nil {
				t.Fatalf("TerminateMergeWithNotice: %v", err)
			}
			if ok, err := d.ClaimMergeNotice(11, noticeRepo, text); err != nil || !ok {
				t.Fatalf("claim = %v, %v, want true, nil", ok, err)
			}
			if ok, err := d.ReturnMergeNotice(11, noticeRepo, text); err != nil || !ok {
				t.Fatalf("return = %v, %v, want true, nil", ok, err)
			}
			if ok, err := d.ReturnMergeNotice(11, noticeRepo, text); err != nil || ok {
				t.Errorf("second return = %v, %v, want false, nil", ok, err)
			}
			if got := pendingNoticePRs(t, d, noticeSession); len(got) != 1 || got[0] != 11 {
				t.Errorf("pending notices = %v, want [11]", got)
			}
		})
	}
}

// TestMergeNotice_RowThatDidNotChangeHasNoNotice covers a row that another
// path ended first: a cancel, or the abandon of a stop. The late write of
// the watcher stores no notice, so no watcher notifies an outcome that the
// row does not hold.
func TestMergeNotice_RowThatDidNotChangeHasNoNotice(t *testing.T) {
	for _, first := range []string{"cancelled", "abandoned"} {
		t.Run(first, func(t *testing.T) {
			d := openTestDB(t)
			enqueueNoticeRow(t, d, 12)
			if _, err := d.TerminateMerge(12, noticeRepo, first, ""); err != nil {
				t.Fatalf("TerminateMerge(%s): %v", first, err)
			}
			if changed, err := d.TerminateMergeWithNotice(12, noticeRepo, "merged", "", "PR #12 merged."); err != nil || changed {
				t.Fatalf("TerminateMergeWithNotice on a %s row = %v, %v, want false, nil", first, changed, err)
			}
			if got := pendingNoticePRs(t, d, noticeSession); len(got) != 0 {
				t.Errorf("pending notices = %v, want none", got)
			}
			if ok, err := d.ReturnMergeNotice(12, noticeRepo, "PR #12 merged."); err != nil || ok {
				t.Errorf("return on a %s row = %v, %v, want false, nil", first, ok, err)
			}
		})
	}
}

// TestMergeNotice_EnqueueClearsNotice covers a PR that the coordinator
// enqueues again after its outcome. The old notice does not stay, and it
// cannot come back to the new watching row.
func TestMergeNotice_EnqueueClearsNotice(t *testing.T) {
	d := openTestDB(t)
	enqueueNoticeRow(t, d, 13)
	const text = "PR #13 CI failed: pr-gate."
	if _, err := d.TerminateMergeWithNotice(13, noticeRepo, "failed", "CI failed: pr-gate", text); err != nil {
		t.Fatalf("TerminateMergeWithNotice: %v", err)
	}
	if ok, err := d.ClaimMergeNotice(13, noticeRepo, text); err != nil || !ok {
		t.Fatalf("claim = %v, %v, want true, nil", ok, err)
	}
	enqueueNoticeRow(t, d, 13)
	if ok, err := d.ReturnMergeNotice(13, noticeRepo, text); err != nil || ok {
		t.Errorf("return on the enqueued row = %v, %v, want false, nil", ok, err)
	}

	if _, err := d.TerminateMergeWithNotice(13, noticeRepo, "failed", "CI failed: pr-gate", text); err != nil {
		t.Fatalf("TerminateMergeWithNotice: %v", err)
	}
	enqueueNoticeRow(t, d, 13)
	if got := pendingNoticePRs(t, d, noticeSession); len(got) != 0 {
		t.Errorf("pending notices after the enqueue = %v, want none", got)
	}
}

// TestMergeNotice_SessionScope checks that a watcher reads only the notices
// of its own session.
func TestMergeNotice_SessionScope(t *testing.T) {
	d := openTestDB(t)
	enqueueNoticeRow(t, d, 14)
	if _, err := d.EnqueueMerge(14, "repo-b", "repo-b@main", "inst-b", nil); err != nil {
		t.Fatalf("EnqueueMerge(repo-b): %v", err)
	}
	for _, repo := range []string{noticeRepo, "repo-b"} {
		if _, err := d.TerminateMergeWithNotice(14, repo, "merged", "", "PR #14 merged."); err != nil {
			t.Fatalf("TerminateMergeWithNotice(%s): %v", repo, err)
		}
	}
	notices, err := d.PendingMergeNotices(noticeSession)
	if err != nil || len(notices) != 1 || notices[0].Repo != noticeRepo {
		t.Errorf("PendingMergeNotices(%s) = %+v, %v, want the repo-a notice only", noticeSession, notices, err)
	}
}
