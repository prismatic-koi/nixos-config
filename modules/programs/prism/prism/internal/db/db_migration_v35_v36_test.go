package db_test

// Tests for the v35→v36 migration that adds session_groups.delivered_at
// (issue #2259). The new column is the authoritative end-of-life signal
// for a review group: GroupCompleted short-circuits to true and
// ActiveReviewGroupForParent skips the group once delivered_at is set.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedV35DB creates a v35 DB with the pre-v36 session_groups shape (no
// delivered_at column). If withDeliveredAt is true, session_groups already
// has the column (simulates a DB whose declarative schema block added it —
// the migration's pragma guard must detect this and skip the ALTER TABLE).
func seedV35DB(t *testing.T, dbPath string, withDeliveredAt bool) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v35 db: %v", err)
	}
	defer rawConn.Close()

	deliveredAtCol := ""
	if withDeliveredAt {
		deliveredAtCol = ",\n\t\t  delivered_at INTEGER"
	}

	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS agent_events (
		  id TEXT PRIMARY KEY, session_name TEXT NOT NULL, repo TEXT NOT NULL,
		  worktree TEXT NOT NULL, harness_session_id TEXT, type TEXT NOT NULL,
		  payload TEXT NOT NULL, created_at INTEGER NOT NULL,
		  instance_id TEXT
		);
		CREATE TABLE IF NOT EXISTS session_groups (
		  group_id TEXT PRIMARY KEY,
		  parent_session TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		  pr_number TEXT,
		  round INTEGER` + deliveredAtCol + `
		);
		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY, repo TEXT NOT NULL, worktree TEXT NOT NULL,
		  state TEXT NOT NULL, title TEXT,
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  isolation_mode TEXT,
		  instance_id TEXT, last_seen INTEGER NOT NULL, ended_at INTEGER,
		  harness TEXT NOT NULL DEFAULT 'pi',
		  harness_session_id TEXT, harness_port INTEGER,
		  group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
		  muted INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id TEXT PRIMARY KEY, from_session TEXT NOT NULL, to_session TEXT NOT NULL,
		  to_instance_id TEXT,
		  repo TEXT NOT NULL, text TEXT NOT NULL, urgency TEXT NOT NULL DEFAULT 'normal',
		  sent_at INTEGER NOT NULL, delivered_at INTEGER, failed_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS sessions (
		  instance_id        TEXT PRIMARY KEY,
		  session_name       TEXT NOT NULL,
		  agent_role         TEXT,
		  root_agent_name    TEXT,
		  repo               TEXT NOT NULL,
		  worktree           TEXT NOT NULL,
		  harness            TEXT NOT NULL,
		  harness_session_id TEXT,
		  group_id           TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
		  started_at         INTEGER NOT NULL,
		  ended_at           INTEGER,
		  end_state          TEXT,
		  archive_path       TEXT,
		  prism_version      TEXT,
		  parent_session     TEXT
		);
		CREATE TABLE IF NOT EXISTS pending_merges (
		  pr INTEGER PRIMARY KEY, session_name TEXT NOT NULL, instance_id TEXT NOT NULL,
		  queue_position INTEGER NOT NULL, status TEXT NOT NULL, title TEXT, error TEXT,
		  queued_at INTEGER NOT NULL, last_checked_at INTEGER, merged_at INTEGER, ended_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS spawn_inputs (
		    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,
		    profile_name TEXT, model_flag TEXT, variant_flag TEXT, agent_flag TEXT,
		    harness_flag TEXT, isolation_flag TEXT, host_mode_flag INTEGER NOT NULL DEFAULT 0,
		    pr_number INTEGER, branch_flag TEXT, ignore_concurrency_cap INTEGER NOT NULL DEFAULT 0,
		    isolation_mode TEXT,
		    model_variant_overrides TEXT, skills_manifest_hash TEXT, prompt_template_hash TEXT,
		    agent_prompt_hash TEXT, prompt_text TEXT, prompt_source TEXT,
		    abtest_pair_id TEXT,
		    extras TEXT,
		    created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS spawn_outcome (
		    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,
		    end_state TEXT, exit_code INTEGER, duration_ms INTEGER,
		    interrupted_count INTEGER NOT NULL DEFAULT 0,
		    compaction_count INTEGER NOT NULL DEFAULT 0,
		    error_event_count INTEGER NOT NULL DEFAULT 0,
		    permission_ask_count INTEGER NOT NULL DEFAULT 0,
		    permission_denied_count INTEGER NOT NULL DEFAULT 0,
		    doom_loop_count INTEGER NOT NULL DEFAULT 0,
		    pr_number INTEGER, pr_merged_at INTEGER,
		    review_group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
		    review_verdict TEXT, review_pass_count INTEGER,
		    review_fail_count INTEGER, review_none_count INTEGER,
		    rubric_verdict TEXT, rubric_score REAL,
		    rubric_breakdown TEXT, rubric_grader TEXT,
		    tokens_input_total INTEGER NOT NULL DEFAULT 0,
		    tokens_output_total INTEGER NOT NULL DEFAULT 0,
		    tokens_cache_read_total INTEGER NOT NULL DEFAULT 0,
		    tokens_cache_write_total INTEGER NOT NULL DEFAULT 0,
		    cost_usd_total REAL NOT NULL DEFAULT 0,
		    tool_call_count INTEGER NOT NULL DEFAULT 0,
		    tool_error_count INTEGER NOT NULL DEFAULT 0,
		    msg_assistant_count INTEGER NOT NULL DEFAULT 0,
		    time_to_first_event_ms INTEGER, time_to_finished_ms INTEGER,
		    computed_at INTEGER NOT NULL,
		    schema_version INTEGER NOT NULL DEFAULT 1
		);
		CREATE TABLE IF NOT EXISTS harness_frames (
		  id TEXT PRIMARY KEY, session_name TEXT NOT NULL, instance_id TEXT,
		  direction TEXT NOT NULL, type TEXT, payload TEXT NOT NULL, created_at INTEGER NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_active_coordinator_per_repo
		   ON agent_status (repo)
		   WHERE root_agent_name = 'coordinator' AND ended_at IS NULL;
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (35);
	`)
	if err != nil {
		t.Fatalf("seed v35 db: %v", err)
	}
}

// TestMigration_V35ToV36_BodyRuns_AddsDeliveredAt exercises the body-runs
// branch of the v35→v36 migration (issue #2259): a v35 DB without the
// session_groups.delivered_at column. The pragma_table_info guard returns
// 0 and the ALTER TABLE ADD COLUMN executes.
func TestMigration_V35ToV36_BodyRuns_AddsDeliveredAt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v35_body_runs.db")
	seedV35DB(t, dbPath, false /*withDeliveredAt*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 37 {
		t.Errorf("schema_version after migration: got %d, want 37", version)
	}

	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('session_groups') WHERE name = 'delivered_at'`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info delivered_at: %v", err)
	}
	if n != 1 {
		t.Errorf("session_groups.delivered_at missing after v35→v36 (body-runs branch): got %d, want 1", n)
	}

	// The column must default to NULL on existing rows. Seed a group via
	// RegisterGroup and check delivered_at is NULL (no backfill).
	gid, err := d.RegisterGroup("nixos-config@feat-v36")
	if err != nil {
		t.Fatalf("RegisterGroup post-migration: %v", err)
	}
	var deliveredAt sql.NullInt64
	if err := d.QueryRow(
		`SELECT delivered_at FROM session_groups WHERE group_id = ?`, gid,
	).Scan(&deliveredAt); err != nil {
		t.Fatalf("read delivered_at on fresh group: %v", err)
	}
	if deliveredAt.Valid {
		t.Errorf("delivered_at on fresh group: got %d, want NULL (no backfill required)", deliveredAt.Int64)
	}
}

// TestMigration_V35ToV36_BodySkips_PreExistingColumn exercises the
// body-skips branch: a v35 DB that ALREADY has the session_groups.delivered_at
// column. The pragma_table_info guard returns 1 and the ALTER TABLE ADD
// COLUMN is skipped.
func TestMigration_V35ToV36_BodySkips_PreExistingColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v35_body_skips.db")
	seedV35DB(t, dbPath, true /*withDeliveredAt*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 37 {
		t.Errorf("schema_version after migration: got %d, want 37", version)
	}

	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('session_groups') WHERE name = 'delivered_at'`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info delivered_at: %v", err)
	}
	if n != 1 {
		t.Errorf("session_groups.delivered_at count after v35→v36 (body-skips branch): got %d, want 1", n)
	}
}

// TestMigration_V35ToV36_Idempotent verifies that re-opening a DB already
// at v36+ does not error and the column remains.
func TestMigration_V35ToV36_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v35_idem.db")
	seedV35DB(t, dbPath, false)

	d1, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("first db.Open: %v", err)
	}
	d1.Close()

	d2, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("second db.Open: %v", err)
	}
	defer d2.Close()

	var version int
	if err := d2.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version after second open: %v", err)
	}
	if version != 37 {
		t.Errorf("schema_version after second open: got %d, want 37", version)
	}

	var n int
	if err := d2.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('session_groups') WHERE name = 'delivered_at'`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info delivered_at on second open: %v", err)
	}
	if n != 1 {
		t.Errorf("session_groups.delivered_at count after idempotent open: got %d, want 1", n)
	}
}

// TestMigration_V35ToV36_NoBackfill verifies the #2259 edge-case AC:
// pre-migration session_groups rows (NULL delivered_at) continue to be
// classified by the existing agent_status-based predicate. A group with
// all-finished members AND delivered_at NULL must still report done=true
// via the fallback path — i.e. the new short-circuit does not regress the
// existing rollup for legacy rows.
func TestMigration_V35ToV36_NoBackfill(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v35_no_backfill.db")
	seedV35DB(t, dbPath, false)

	// Pre-seed a v35 group row with a pre-existing finished member.
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := rawConn.Exec(
		`INSERT INTO session_groups (group_id, parent_session, pr_number, round) VALUES (?, ?, ?, ?)`,
		"legacy-group", "nixos-config@legacy", "1", 1,
	); err != nil {
		t.Fatalf("seed legacy session_groups row: %v", err)
	}
	if _, err := rawConn.Exec(
		`INSERT INTO agent_status (session_name, repo, worktree, state, last_seen, group_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"nixos-config@legacy~review-1-goal", "nixos-config", "/wt", "finished", 0, "legacy-group",
	); err != nil {
		t.Fatalf("seed legacy agent_status row: %v", err)
	}
	rawConn.Close()

	// Open via the real migration path — this will add delivered_at but
	// leave the legacy row's column as NULL.
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	var deliveredAt sql.NullInt64
	if err := d.QueryRow(
		`SELECT delivered_at FROM session_groups WHERE group_id = ?`, "legacy-group",
	).Scan(&deliveredAt); err != nil {
		t.Fatalf("read legacy delivered_at: %v", err)
	}
	if deliveredAt.Valid {
		t.Errorf("legacy delivered_at: got %d, want NULL (no backfill required)", deliveredAt.Int64)
	}

	// The legacy group must still be classified as completed via the
	// agent_status-based predicate (member is in state=finished).
	done, err := d.GroupCompleted("legacy-group")
	if err != nil {
		t.Fatalf("GroupCompleted on legacy group: %v", err)
	}
	if !done {
		t.Error("GroupCompleted on legacy group: got false, want true (delivered_at NULL must fall back to agent_status predicate)")
	}
}
