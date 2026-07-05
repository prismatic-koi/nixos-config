package db_test

// Tests for the repo scoping added in issue #2354.
//
// Background — the incident.
//
// Before v37→v38, pending_merges was keyed by (pr INTEGER PRIMARY KEY)
// alone and every WHERE clause was `WHERE pr = ?` with no repo scope.
// On 2026-07-06 a coordinator running `prism merge 47` for its own
// repo's PR #47 short-circuited on a DIFFERENT repo's terminal `merged`
// row for its (unrelated) PR #47. The short-circuit printed
// `PR #47 merged.`, the coordinator followed the merged-notification
// flow and destructively cleaned up an unmerged worker.
//
// These tests pin down the fix at the DB layer: rows are now keyed on
// (repo, pr), lookups take a repo argument, and the terminal + heartbeat
// writes cannot touch a same-numbered row from another repo.
//
// The tests live in the db_test package (not the internal db package) so
// they exercise only the exported API — the same surface any downstream
// caller uses.

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestCrossRepoPRNumberCollision_Enqueue_FreshRowNotShortCircuited is the
// canonical regression test for the incident of 2026-07-06. Reproduce the
// exact shape:
//
//  1. Insert a terminal `merged` row for repo A's PR N (this is the stale
//     github-actions@main / dependabot bump equivalent).
//  2. From repo B, call EnqueueMerge for its own PR N.
//  3. Assert the DB now holds TWO rows — repo A's terminal row unchanged,
//     and a fresh `watching` row for repo B — and PendingMergeByPR returns
//     the repo-B row when called with (N, repoB), not repo A's terminal
//     row.
//
// A regression here would re-open the exact incident: repo B's coordinator
// would short-circuit on repo A's terminal row and never enqueue its own
// merge.
func TestCrossRepoPRNumberCollision_Enqueue_FreshRowNotShortCircuited(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "collision.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Repo A already has a merged row for PR 47 (the incident shape:
	// github-actions@main / dependabot dependency bump).
	const pr = 47
	titleA := "chore: bump deps"
	if _, err := d.EnqueueMerge(pr, "repo-a", "repo-a@main", "inst-a", &titleA); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	if err := d.TerminateMerge(pr, "repo-a", "merged", ""); err != nil {
		t.Fatalf("TerminateMerge repo-a: %v", err)
	}

	// Now repo B tries to enqueue its own PR 47 (unrelated open PR).
	titleB := "feat: real work"
	row, err := d.EnqueueMerge(pr, "repo-b", "repo-b@main", "inst-b", &titleB)
	if err != nil {
		t.Fatalf("EnqueueMerge repo-b: %v", err)
	}
	if row == nil {
		t.Fatal("EnqueueMerge repo-b: got nil row")
	}
	if row.Status != "watching" {
		t.Errorf("EnqueueMerge repo-b: status = %q, want watching (a fresh watching row must be inserted, not the merged row from repo-a)", row.Status)
	}
	if row.Repo != "repo-b" {
		t.Errorf("EnqueueMerge repo-b: repo = %q, want repo-b", row.Repo)
	}
	if row.SessionName != "repo-b@main" {
		t.Errorf("EnqueueMerge repo-b: session_name = %q, want repo-b@main", row.SessionName)
	}
	if row.InstanceID != "inst-b" {
		t.Errorf("EnqueueMerge repo-b: instance_id = %q, want inst-b", row.InstanceID)
	}

	// Repo A's terminal row must be intact and separate.
	rowA, err := d.PendingMergeByPR(pr, "repo-a")
	if err != nil {
		t.Fatalf("PendingMergeByPR repo-a: %v", err)
	}
	if rowA == nil {
		t.Fatal("PendingMergeByPR repo-a: got nil, want the pre-existing merged row")
	}
	if rowA.Status != "merged" {
		t.Errorf("PendingMergeByPR repo-a: status = %q, want merged (enqueue-on-repo-b must not disturb repo-a's row)", rowA.Status)
	}
	if rowA.SessionName != "repo-a@main" {
		t.Errorf("PendingMergeByPR repo-a: session_name = %q, want repo-a@main (foreign-repo enqueue must not overwrite session_name)", rowA.SessionName)
	}

	// Repo B's watching row must be visible via its own repo scope.
	rowB, err := d.PendingMergeByPR(pr, "repo-b")
	if err != nil {
		t.Fatalf("PendingMergeByPR repo-b: %v", err)
	}
	if rowB == nil {
		t.Fatal("PendingMergeByPR repo-b: got nil, want the fresh watching row")
	}
	if rowB.Status != "watching" {
		t.Errorf("PendingMergeByPR repo-b: status = %q, want watching", rowB.Status)
	}
	if rowB.Repo != "repo-b" {
		t.Errorf("PendingMergeByPR repo-b: repo = %q, want repo-b", rowB.Repo)
	}
}

// TestPendingMergeByPR_RepoScoped verifies the core lookup semantics: the
// same PR number in two repos returns two distinct rows.
func TestPendingMergeByPR_RepoScoped(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "by_pr.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if _, err := d.EnqueueMerge(100, "repo-a", "repo-a@main", "inst-a", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	if _, err := d.EnqueueMerge(100, "repo-b", "repo-b@main", "inst-b", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-b: %v", err)
	}

	rowA, err := d.PendingMergeByPR(100, "repo-a")
	if err != nil {
		t.Fatalf("PendingMergeByPR repo-a: %v", err)
	}
	rowB, err := d.PendingMergeByPR(100, "repo-b")
	if err != nil {
		t.Fatalf("PendingMergeByPR repo-b: %v", err)
	}
	if rowA == nil || rowB == nil {
		t.Fatal("expected both rows to be present")
	}
	if rowA.InstanceID != "inst-a" {
		t.Errorf("repo-a row: instance_id = %q, want inst-a", rowA.InstanceID)
	}
	if rowB.InstanceID != "inst-b" {
		t.Errorf("repo-b row: instance_id = %q, want inst-b", rowB.InstanceID)
	}

	// Looking up in an unrelated repo returns nil.
	rowC, err := d.PendingMergeByPR(100, "repo-c")
	if err != nil {
		t.Fatalf("PendingMergeByPR repo-c: %v", err)
	}
	if rowC != nil {
		t.Errorf("PendingMergeByPR repo-c: got row for foreign repo, want nil")
	}
}

// TestTerminateMerge_RepoScoped verifies the AC:
//
//	"The merge-queue watcher's terminal writes (`merged`/`failed`/etc. and
//	 `last_checked_at`) only ever touch the row belonging to its own repo."
//
// Seed a watching row for the same PR in two repos, terminate one, and
// confirm the other is untouched.
func TestTerminateMerge_RepoScoped(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "terminate.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if _, err := d.EnqueueMerge(50, "repo-a", "repo-a@main", "inst-a", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	if _, err := d.EnqueueMerge(50, "repo-b", "repo-b@main", "inst-b", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-b: %v", err)
	}

	// Terminate only repo-a's row.
	if err := d.TerminateMerge(50, "repo-a", "merged", ""); err != nil {
		t.Fatalf("TerminateMerge repo-a: %v", err)
	}

	rowA, _ := d.PendingMergeByPR(50, "repo-a")
	rowB, _ := d.PendingMergeByPR(50, "repo-b")
	if rowA == nil || rowB == nil {
		t.Fatal("both rows must still exist")
	}
	if rowA.Status != "merged" {
		t.Errorf("repo-a row: status = %q, want merged", rowA.Status)
	}
	if rowB.Status != "watching" {
		t.Errorf("repo-b row: status = %q, want watching (terminate on repo-a must not touch repo-b)", rowB.Status)
	}
}

// TestUpdateMergeLastChecked_RepoScoped verifies the AC that the watcher's
// per-tick heartbeat is repo-scoped: touching (pr=N, repo=A) must not
// bump last_checked_at on (pr=N, repo=B).
func TestUpdateMergeLastChecked_RepoScoped(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "heartbeat.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if _, err := d.EnqueueMerge(60, "repo-a", "repo-a@main", "inst-a", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	if _, err := d.EnqueueMerge(60, "repo-b", "repo-b@main", "inst-b", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-b: %v", err)
	}

	if err := d.UpdateMergeLastChecked(60, "repo-a"); err != nil {
		t.Fatalf("UpdateMergeLastChecked repo-a: %v", err)
	}

	rowA, _ := d.PendingMergeByPR(60, "repo-a")
	rowB, _ := d.PendingMergeByPR(60, "repo-b")
	if rowA == nil || rowB == nil {
		t.Fatal("both rows must exist")
	}
	if rowA.LastCheckedAt == nil {
		t.Errorf("repo-a row: LastCheckedAt is nil after heartbeat")
	}
	if rowB.LastCheckedAt != nil {
		t.Errorf("repo-b row: LastCheckedAt = %v after heartbeat on repo-a (foreign-repo heartbeat must not touch repo-b)", *rowB.LastCheckedAt)
	}
}

// TestCancelMerge_RepoScoped verifies CancelMerge is triple-scoped by
// (pr, repo, instance_id). A cancel from repo A's coordinator cannot
// touch a same-numbered row in repo B, even if the two coordinators
// share (impossibly, but for safety) the same instance_id.
func TestCancelMerge_RepoScoped(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Same instance_id in both rows to prove repo scoping alone is
	// sufficient. In production instance_ids are UUIDs per coordinator,
	// but a defence-in-depth check is warranted.
	const iid = "inst-shared"
	if _, err := d.EnqueueMerge(70, "repo-a", "repo-a@main", iid, nil); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	if _, err := d.EnqueueMerge(70, "repo-b", "repo-b@main", iid, nil); err != nil {
		t.Fatalf("EnqueueMerge repo-b: %v", err)
	}

	// Cancel only repo-a's row.
	ok, err := d.CancelMerge(70, "repo-a", iid)
	if err != nil {
		t.Fatalf("CancelMerge repo-a: %v", err)
	}
	if !ok {
		t.Error("CancelMerge repo-a: returned false, want true")
	}

	rowA, _ := d.PendingMergeByPR(70, "repo-a")
	rowB, _ := d.PendingMergeByPR(70, "repo-b")
	if rowA == nil || rowB == nil {
		t.Fatal("both rows must exist")
	}
	if rowA.Status != "cancelled" {
		t.Errorf("repo-a row: status = %q, want cancelled", rowA.Status)
	}
	if rowB.Status != "watching" {
		t.Errorf("repo-b row: status = %q, want watching (cancel on repo-a must not touch repo-b)", rowB.Status)
	}
}

// TestEnqueueMerge_SameRepoAlreadyMergedIsIdempotent verifies edge-case AC:
//
//	"Re-running `prism merge <pr>` on a PR already merged in the same repo
//	 prints the recorded merged status without re-enqueueing (#1875
//	 behaviour preserved)."
//
// The DB layer's contribution to this AC is that EnqueueMerge on a merged
// row overwrites via ON CONFLICT — the #1875 short-circuit lives in the
// cmd layer. The invariant we test here: EnqueueMerge is well-defined for
// the (repo, pr) already-merged case and returns a fresh watching row
// (the cmd layer's short-circuit is what prevents that call in practice).
func TestEnqueueMerge_SameRepoAlreadyMergedReEnqueuesFresh(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "idem.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if _, err := d.EnqueueMerge(80, "repo-a", "repo-a@main", "inst-a", nil); err != nil {
		t.Fatalf("first EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(80, "repo-a", "merged", ""); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}

	// Re-enqueue in the same repo: the ON CONFLICT branch fires, row
	// flips back to watching (the terminal-row-is-gone semantic).
	row, err := d.EnqueueMerge(80, "repo-a", "repo-a@main", "inst-a", nil)
	if err != nil {
		t.Fatalf("second EnqueueMerge: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("re-enqueue in same repo: status = %q, want watching (ON CONFLICT branch must reset the terminal row)", row.Status)
	}
}

// TestEnqueueMerge_SameRepoAlreadyWatchingIsIdempotent verifies edge-case AC:
//
//	"Re-running `prism merge <pr>` on a `watching` row in the same repo
//	 prints the 'already in queue' message without duplicate enqueue."
//
// The DB layer's contribution is EnqueueMerge returning the SAME row
// (same queue_position) when a watching row exists.
func TestEnqueueMerge_SameRepoAlreadyWatchingIsIdempotent(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "idem_watch.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	m1, err := d.EnqueueMerge(90, "repo-a", "repo-a@main", "inst-a", nil)
	if err != nil {
		t.Fatalf("first EnqueueMerge: %v", err)
	}

	m2, err := d.EnqueueMerge(90, "repo-a", "repo-a@main", "inst-a", nil)
	if err != nil {
		t.Fatalf("second EnqueueMerge: %v", err)
	}

	// Second call must return the SAME row (idempotent) — queue_position
	// unchanged, status still watching.
	if m1.QueuePosition != m2.QueuePosition {
		t.Errorf("queue_position changed across idempotent enqueues: pre=%d post=%d", m1.QueuePosition, m2.QueuePosition)
	}
	if m2.Status != "watching" {
		t.Errorf("status: got %q, want watching", m2.Status)
	}

	// And only one row exists in the repo.
	rows, err := d.MergeQueueForInstance("inst-a", "repo-a@main", "watching")
	if err != nil {
		t.Fatalf("MergeQueueForInstance: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 watching row, got %d", len(rows))
	}
}

// TestMigrationBackfill_SessionNameSplitAtFirstAt verifies the AC:
//
//	"Existing `pending_merges` rows are backfilled with a repo derived
//	 from the `<repo>@<branch>` session-name convention, and remain
//	 visible in `prism merges list --all` after migration."
//
// The migration seeds a v37 DB with rows carrying `session_name` values
// like `nixos-config@main` and `myrepo@feature-branch`, runs the
// migration by opening the DB, and asserts every row now has the correct
// short repo slug on its `repo` column.
func TestMigrationBackfill_SessionNameSplitAtFirstAt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "backfill.db")
	seedV37DBWithPendingMergesRows(t, dbPath, []seedRow{
		{PR: 100, SessionName: "nixos-config@main", InstanceID: "inst-a", Status: "watching"},
		{PR: 200, SessionName: "myrepo@feature-branch", InstanceID: "inst-b", Status: "merged"},
		{PR: 300, SessionName: "aws-databases@main", InstanceID: "inst-c", Status: "failed"},
		// An @-in-branch case (branches can contain @): the split must
		// happen at the FIRST @, not the last, so the repo remains the
		// short slug and the "@" ends up in the branch part.
		{PR: 400, SessionName: "repo-x@user@feature", InstanceID: "inst-d", Status: "watching"},
	})

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open after migration: %v", err)
	}
	defer d.Close()

	wantRepos := map[int]string{
		100: "nixos-config",
		200: "myrepo",
		300: "aws-databases",
		400: "repo-x",
	}
	for pr, wantRepo := range wantRepos {
		row, err := d.PendingMergeByPR(pr, wantRepo)
		if err != nil {
			t.Fatalf("PendingMergeByPR(%d, %q): %v", pr, wantRepo, err)
		}
		if row == nil {
			t.Errorf("PendingMergeByPR(%d, %q): row missing after migration \u2014 backfill should have set repo=%q", pr, wantRepo, wantRepo)
			continue
		}
		if row.Repo != wantRepo {
			t.Errorf("PR %d: repo = %q, want %q", pr, row.Repo, wantRepo)
		}
	}
}

// TestMigrationBackfill_SessionNameWithoutAtSignSentinel verifies the AC:
//
//	"Migration does not fail on rows whose `session_name` contains no
//	 `@`; such rows receive a sentinel repo that never matches a scoped
//	 lookup."
//
// We use empty string as the sentinel: real callers always resolve to a
// non-empty repo, so an empty-repo lookup can never match by accident.
func TestMigrationBackfill_SessionNameWithoutAtSignSentinel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "backfill_sentinel.db")
	seedV37DBWithPendingMergesRows(t, dbPath, []seedRow{
		{PR: 500, SessionName: "malformed-no-at-sign", InstanceID: "inst-x", Status: "watching"},
	})

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open after migration: %v", err)
	}
	defer d.Close()

	// The sentinel row must NOT be discoverable via any real repo slug.
	// "malformed-no-at-sign" here is what someone might reasonably guess
	// as the repo \u2014 it must not match.
	row, err := d.PendingMergeByPR(500, "malformed-no-at-sign")
	if err != nil {
		t.Fatalf("PendingMergeByPR (guessed repo): %v", err)
	}
	if row != nil {
		t.Errorf("PendingMergeByPR(500, %q): got row, want nil (a sentinel row must never match a real repo lookup)", "malformed-no-at-sign")
	}

	// The row IS visible on the empty-string sentinel scope \u2014 this is
	// what `prism merges list --all` uses to enumerate rows regardless
	// of repo (via MergeQueueForInstance, keyed by instance_id).
	rows, err := d.MergeQueueForInstance("inst-x", "malformed-no-at-sign", "all")
	if err != nil {
		t.Fatalf("MergeQueueForInstance --all: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("MergeQueueForInstance --all: got %d rows, want 1 (the sentinel row must still be enumerable via instance_id)", len(rows))
	}
	if rows[0].Repo != "" {
		t.Errorf("sentinel row: repo = %q, want empty string", rows[0].Repo)
	}
}

// TestMigrationBackfill_PreservesPrismMergesListAllVisibility verifies the
// AC that existing rows remain visible in `prism merges list --all` after
// migration. `prism merges list --all` calls MergeQueueForInstance with
// filter="all", which is keyed by instance_id \u2014 the repo scoping fix
// does not touch that selector.
func TestMigrationBackfill_PreservesPrismMergesListAllVisibility(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "backfill_list_all.db")
	seedV37DBWithPendingMergesRows(t, dbPath, []seedRow{
		{PR: 601, SessionName: "myrepo@main", InstanceID: "inst-list", Status: "watching"},
		{PR: 602, SessionName: "myrepo@main", InstanceID: "inst-list", Status: "merged"},
		{PR: 603, SessionName: "myrepo@main", InstanceID: "inst-list", Status: "failed"},
	})

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	rows, err := d.MergeQueueForInstance("inst-list", "myrepo@main", "all")
	if err != nil {
		t.Fatalf("MergeQueueForInstance --all: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("MergeQueueForInstance --all: got %d rows, want 3 (all pre-migration rows must remain visible)", len(rows))
	}
	// All rows must have repo=myrepo populated by the backfill.
	for _, r := range rows {
		if r.Repo != "myrepo" {
			t.Errorf("PR %d: repo = %q, want myrepo (backfill did not set repo)", r.PR, r.Repo)
		}
	}
}

// TestMigrationV37ToV38_SchemaVersionAndCompositePK verifies the migration
// completes to v38 and the resulting table shape is (repo, pr) primary key.
func TestMigrationV37ToV38_SchemaVersionAndCompositePK(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "schema.db")
	seedV37DBWithPendingMergesRows(t, dbPath, nil)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	var version int
	if err := d.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 38 {
		t.Errorf("schema_version: got %d, want 38", version)
	}

	// pragma_table_info exposes the repo column.
	var repoNotNull int
	err = d.QueryRow(
		`SELECT "notnull" FROM pragma_table_info('pending_merges') WHERE name = 'repo'`,
	).Scan(&repoNotNull)
	if err != nil {
		t.Fatalf("pragma_table_info(pending_merges).repo: %v", err)
	}
	if repoNotNull != 1 {
		t.Errorf("repo column: notnull = %d, want 1", repoNotNull)
	}

	// The primary key must be composite (repo, pr). Query pragma to
	// count the number of PK member columns; two = composite.
	var pkColCount int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('pending_merges') WHERE pk > 0`,
	).Scan(&pkColCount); err != nil {
		t.Fatalf("count PK columns: %v", err)
	}
	if pkColCount != 2 {
		t.Errorf("primary key columns: got %d, want 2 (repo, pr composite)", pkColCount)
	}
}

// seedRow describes a v37-shape pending_merges row for the migration
// backfill tests. Only the fields we care about in these tests are
// exposed; the rest use safe defaults.
type seedRow struct {
	PR          int
	SessionName string
	InstanceID  string
	Status      string
}

// seedV37DBWithPendingMergesRows creates a database in the pre-migration
// v37 shape and seeds pending_merges rows on that shape. The migration
// then runs on the next db.Open call in the test, and the backfill can
// be observed.
func seedV37DBWithPendingMergesRows(t *testing.T, dbPath string, rows []seedRow) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v37 db: %v", err)
	}
	defer rawConn.Close()

	// Recreate the minimum tables the migration + Open() will touch. We
	// only need pending_merges in its v37 shape and schema_version = 37;
	// the migration itself does not depend on any other table.
	if _, err := rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS pending_merges (
		  pr              INTEGER PRIMARY KEY,
		  session_name    TEXT NOT NULL,
		  instance_id     TEXT NOT NULL,
		  queue_position  INTEGER NOT NULL,
		  status          TEXT NOT NULL,
		  title           TEXT,
		  error           TEXT,
		  queued_at       INTEGER NOT NULL,
		  last_checked_at INTEGER,
		  merged_at       INTEGER,
		  ended_at        INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (37);
	`); err != nil {
		t.Fatalf("seed v37 pending_merges schema: %v", err)
	}

	// Use a recent timestamp so the `--all` filter in MergeQueueForInstance
	// (which restricts to the last 7 days) still surfaces these rows.
	nowMs := time.Now().UnixMilli()
	for i, r := range rows {
		if _, err := rawConn.Exec(
			`INSERT INTO pending_merges (pr, session_name, instance_id, queue_position, status, queued_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			r.PR, r.SessionName, r.InstanceID, nowMs+int64(i), r.Status, nowMs-int64(i),
		); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
}
