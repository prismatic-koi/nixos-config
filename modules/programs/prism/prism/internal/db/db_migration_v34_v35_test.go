package db_test

// Tests for the v34→v35 migration that adds spawn_inputs.isolation_mode
// (issue #2105). The new column carries the resolved effective isolation
// mode the session ran under — distinct from isolation_flag (the raw
// --isolation CLI value, NULL when omitted).

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedV34DB creates a v34 DB with the pre-v35 spawn_inputs shape (no
// isolation_mode column). If withIsoMode is true, spawn_inputs already has
// the column (simulates a DB whose declarative schema block added it — the
// migration's pragma guard must detect this and skip the ALTER TABLE).
func seedV34DB(t *testing.T, dbPath string, withIsoMode bool) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v34 db: %v", err)
	}
	defer rawConn.Close()

	isoModeCol := ""
	if withIsoMode {
		isoModeCol = "isolation_mode TEXT,"
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
		  pr_number INTEGER,
		  round INTEGER
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
		  prism_version      TEXT
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
		    ` + isoModeCol + `
		    model_variant_overrides TEXT, skills_manifest_hash TEXT, prompt_template_hash TEXT,
		    agent_prompt_hash TEXT, prompt_text TEXT, prompt_source TEXT,
		    abtest_pair_id TEXT,
		    extras TEXT,
		    created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS spawn_outcome (
		    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,
		    end_state TEXT,
		    exit_code INTEGER,
		    duration_ms INTEGER,
		    interrupted_count INTEGER NOT NULL DEFAULT 0,
		    compaction_count INTEGER NOT NULL DEFAULT 0,
		    error_event_count INTEGER NOT NULL DEFAULT 0,
		    permission_ask_count INTEGER NOT NULL DEFAULT 0,
		    permission_denied_count INTEGER NOT NULL DEFAULT 0,
		    doom_loop_count INTEGER NOT NULL DEFAULT 0,
		    pr_number INTEGER,
		    pr_merged_at INTEGER,
		    review_group_id TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
		    review_verdict TEXT,
		    review_pass_count INTEGER,
		    review_fail_count INTEGER,
		    review_none_count INTEGER,
		    rubric_verdict TEXT,
		    rubric_score REAL,
		    rubric_breakdown TEXT,
		    rubric_grader TEXT,
		    tokens_input_total INTEGER NOT NULL DEFAULT 0,
		    tokens_output_total INTEGER NOT NULL DEFAULT 0,
		    tokens_cache_read_total INTEGER NOT NULL DEFAULT 0,
		    tokens_cache_write_total INTEGER NOT NULL DEFAULT 0,
		    cost_usd_total REAL NOT NULL DEFAULT 0,
		    tool_call_count INTEGER NOT NULL DEFAULT 0,
		    tool_error_count INTEGER NOT NULL DEFAULT 0,
		    msg_assistant_count INTEGER NOT NULL DEFAULT 0,
		    time_to_first_event_ms INTEGER,
		    time_to_finished_ms INTEGER,
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
		INSERT INTO schema_version (version) VALUES (34);
	`)
	if err != nil {
		t.Fatalf("seed v34 db: %v", err)
	}
}

// TestMigration_V34ToV35_BodyRuns_AddsIsolationMode exercises the
// body-runs branch of the v34→v35 migration (issue #2105): a v34 DB
// without the spawn_inputs.isolation_mode column. The pragma_table_info
// guard returns 0 and the ALTER TABLE ADD COLUMN executes.
func TestMigration_V34ToV35_BodyRuns_AddsIsolationMode(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v34_body_runs.db")
	seedV34DB(t, dbPath, false /*withIsoMode*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 38 {
		t.Errorf("schema_version after migration: got %d, want 38", version)
	}

	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('spawn_inputs') WHERE name = 'isolation_mode'`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info isolation_mode: %v", err)
	}
	if n != 1 {
		t.Errorf("spawn_inputs.isolation_mode missing after v34→v35 (body-runs branch): got %d, want 1", n)
	}
}

// TestMigration_V34ToV35_BodySkips_PreExistingColumn exercises the
// body-skips branch: a v34 DB that ALREADY has the spawn_inputs.isolation_mode
// column (simulating a DB whose declarative schema block added it). The
// pragma_table_info guard returns 1 and the ALTER TABLE ADD COLUMN is
// skipped. The end state is still v35 with the column present (not
// duplicated by a re-ADD).
func TestMigration_V34ToV35_BodySkips_PreExistingColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v34_body_skips.db")
	seedV34DB(t, dbPath, true /*withIsoMode*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 38 {
		t.Errorf("schema_version after migration: got %d, want 38", version)
	}

	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('spawn_inputs') WHERE name = 'isolation_mode'`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info isolation_mode: %v", err)
	}
	if n != 1 {
		t.Errorf("spawn_inputs.isolation_mode count after v34→v35 (body-skips branch): got %d, want 1", n)
	}
}

// TestMigration_V34ToV35_Idempotent verifies that re-opening a DB already
// at v35+ does not error and the column remains.
func TestMigration_V34ToV35_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v34_idem.db")
	seedV34DB(t, dbPath, false)

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
	if version != 38 {
		t.Errorf("schema_version after second open: got %d, want 38", version)
	}

	var n int
	if err := d2.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('spawn_inputs') WHERE name = 'isolation_mode'`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info isolation_mode on second open: %v", err)
	}
	if n != 1 {
		t.Errorf("spawn_inputs.isolation_mode count after idempotent open: got %d, want 1", n)
	}
}
