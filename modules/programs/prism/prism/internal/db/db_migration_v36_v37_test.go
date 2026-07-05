package db_test

// Tests for the v36→v37 migration that adds two columns in a single
// migration (#2317 / #2319):
//
//   - agent_status.containers_enabled — runtime gate read by the sidecar
//     to decide whether to start the per-session filtering podman API
//     socket proxy.
//   - spawn_inputs.containers_flag — audit symmetry with host_mode_flag
//     and isolation_flag, capturing whether the user passed --containers
//     at spawn time.
//
// The migration follows the muted-column template (v33→v34): two
// idempotent ALTER TABLE statements, each guarded by a pragma_table_info
// check, with INTEGER NOT NULL DEFAULT 0 shape.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedV36DB creates a v36 DB. The flags allow simulating "the declarative
// schema block already added the column" — in which case the migration's
// pragma_table_info guard must detect the column and skip the ALTER TABLE.
func seedV36DB(t *testing.T, dbPath string, withContainersEnabled, withContainersFlag bool) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v36 db: %v", err)
	}
	defer rawConn.Close()

	containersEnabledCol := ""
	if withContainersEnabled {
		containersEnabledCol = ",\n\t\t  containers_enabled INTEGER NOT NULL DEFAULT 0"
	}
	containersFlagCol := ""
	if withContainersFlag {
		containersFlagCol = "containers_flag INTEGER NOT NULL DEFAULT 0,"
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
		  round INTEGER,
		  delivered_at INTEGER
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
		  muted INTEGER NOT NULL DEFAULT 0` + containersEnabledCol + `
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
		    ` + containersFlagCol + `
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
		INSERT INTO schema_version (version) VALUES (36);
	`)
	if err != nil {
		t.Fatalf("seed v36 db: %v", err)
	}
}

// columnShape pulls the pragma_table_info row that the ACs assert on:
// type, notnull, dflt_value. Returns ok=false when the column does not
// exist on the named table.
type columnShape struct {
	cType     string
	notNull   int
	dfltValue string
}

func readColumnShape(t *testing.T, d *db.DB, table, column string) (columnShape, bool) {
	t.Helper()
	var info columnShape
	var dflt sql.NullString
	err := d.QueryRow(
		`SELECT type, "notnull", dflt_value FROM pragma_table_info(?) WHERE name = ?`,
		table, column,
	).Scan(&info.cType, &info.notNull, &dflt)
	if err == sql.ErrNoRows {
		return columnShape{}, false
	}
	if err != nil {
		t.Fatalf("pragma_table_info(%s).%s: %v", table, column, err)
	}
	if dflt.Valid {
		info.dfltValue = dflt.String
	}
	return info, true
}

// assertColumnShape verifies the column has type INTEGER, notnull=1, and
// default 0 — the shape required by the issue ACs (#1, #2).
func assertColumnShape(t *testing.T, d *db.DB, table, column string) {
	t.Helper()
	info, ok := readColumnShape(t, d, table, column)
	if !ok {
		t.Fatalf("%s.%s: column missing after migration", table, column)
	}
	if info.cType != "INTEGER" {
		t.Errorf("%s.%s.type: got %q, want INTEGER", table, column, info.cType)
	}
	if info.notNull != 1 {
		t.Errorf("%s.%s.notnull: got %d, want 1", table, column, info.notNull)
	}
	if info.dfltValue != "0" {
		t.Errorf("%s.%s.dflt_value: got %q, want %q", table, column, info.dfltValue, "0")
	}
}

// countColumn returns the number of pragma_table_info rows for a column,
// which is 0 (missing) or 1 (present). Used to assert ALTER TABLE was not
// double-applied (a duplicate-ADD would error before reaching this assert,
// but the count check is the natural shape of the AC).
func countColumn(t *testing.T, d *db.DB, table, column string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info COUNT(%s.%s): %v", table, column, err)
	}
	return n
}

// readVersion reads the schema_version row.
func readVersion(t *testing.T, d *db.DB) int {
	t.Helper()
	var v int
	if err := d.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	return v
}

// TestMigration_V36ToV37_BodyRuns_AddsBothColumns exercises the body-runs
// branch (#2319 ACs #1, #2, #3): a v36 DB without either new column.
// Both ALTER TABLE statements execute, both columns end up at INTEGER
// NOT NULL DEFAULT 0, and schema_version is bumped to 37.
func TestMigration_V36ToV37_BodyRuns_AddsBothColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v36_body_runs.db")
	seedV36DB(t, dbPath, false /*withContainersEnabled*/, false /*withContainersFlag*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := readVersion(t, d); v != 38 {
		t.Errorf("schema_version after migration: got %d, want 38", v)
	}
	assertColumnShape(t, d, "agent_status", "containers_enabled")
	assertColumnShape(t, d, "spawn_inputs", "containers_flag")
}

// TestMigration_V36ToV37_BodySkips_BothColumnsPreExist exercises the
// body-skips branch: both new columns already exist (declarative schema
// already added them). The migration's pragma_table_info guards detect
// both and skip both ALTER TABLE statements. Each column must appear
// exactly once — no duplicate-add and no error.
func TestMigration_V36ToV37_BodySkips_BothColumnsPreExist(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v36_body_skips_both.db")
	seedV36DB(t, dbPath, true /*withContainersEnabled*/, true /*withContainersFlag*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := readVersion(t, d); v != 38 {
		t.Errorf("schema_version after migration: got %d, want 38", v)
	}
	if n := countColumn(t, d, "agent_status", "containers_enabled"); n != 1 {
		t.Errorf("agent_status.containers_enabled count after body-skips: got %d, want 1", n)
	}
	if n := countColumn(t, d, "spawn_inputs", "containers_flag"); n != 1 {
		t.Errorf("spawn_inputs.containers_flag count after body-skips: got %d, want 1", n)
	}
}

// TestMigration_V36ToV37_BodyMixed_OnlyContainersEnabledPreExists exercises
// the mixed branch: agent_status.containers_enabled is pre-existing but
// spawn_inputs.containers_flag is not. The migration must skip the first
// ALTER TABLE and execute the second — both columns end up present.
//
// This guards against the failure mode where one pragma check accidentally
// short-circuits the entire migration body.
func TestMigration_V36ToV37_BodyMixed_OnlyContainersEnabledPreExists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v36_body_mixed_a.db")
	seedV36DB(t, dbPath, true /*withContainersEnabled*/, false /*withContainersFlag*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := readVersion(t, d); v != 38 {
		t.Errorf("schema_version after migration: got %d, want 38", v)
	}
	if n := countColumn(t, d, "agent_status", "containers_enabled"); n != 1 {
		t.Errorf("agent_status.containers_enabled count: got %d, want 1", n)
	}
	assertColumnShape(t, d, "spawn_inputs", "containers_flag")
}

// TestMigration_V36ToV37_BodyMixed_OnlyContainersFlagPreExists is the
// mirror case: spawn_inputs.containers_flag pre-exists but
// agent_status.containers_enabled does not.
func TestMigration_V36ToV37_BodyMixed_OnlyContainersFlagPreExists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v36_body_mixed_b.db")
	seedV36DB(t, dbPath, false /*withContainersEnabled*/, true /*withContainersFlag*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := readVersion(t, d); v != 38 {
		t.Errorf("schema_version after migration: got %d, want 38", v)
	}
	assertColumnShape(t, d, "agent_status", "containers_enabled")
	if n := countColumn(t, d, "spawn_inputs", "containers_flag"); n != 1 {
		t.Errorf("spawn_inputs.containers_flag count: got %d, want 1", n)
	}
}

// TestMigration_V36ToV37_Idempotent verifies the AC: running the migration
// twice in a row (via a second Open) is a no-op. Both columns remain
// present exactly once and schema_version stays at 37.
func TestMigration_V36ToV37_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v36_idem.db")
	seedV36DB(t, dbPath, false, false)

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

	if v := readVersion(t, d2); v != 38 {
		t.Errorf("schema_version after second open: got %d, want 38", v)
	}
	if n := countColumn(t, d2, "agent_status", "containers_enabled"); n != 1 {
		t.Errorf("agent_status.containers_enabled count after idempotent open: got %d, want 1", n)
	}
	if n := countColumn(t, d2, "spawn_inputs", "containers_flag"); n != 1 {
		t.Errorf("spawn_inputs.containers_flag count after idempotent open: got %d, want 1", n)
	}
}

// TestMigration_V36ToV37_FreshDB verifies the AC: opening a fresh empty
// database ends at v37 with both columns present (declarative schema
// covers both columns; the migration's pragma guards detect the
// pre-existing columns and do not double-add).
func TestMigration_V36ToV37_FreshDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fresh.db")

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open (fresh): %v", err)
	}
	defer d.Close()

	if v := readVersion(t, d); v != 38 {
		t.Errorf("schema_version on fresh DB: got %d, want 38", v)
	}
	assertColumnShape(t, d, "agent_status", "containers_enabled")
	assertColumnShape(t, d, "spawn_inputs", "containers_flag")
}

// TestMigration_V36ToV37_PopulatedV36RowsDefaultToZero verifies the AC:
// migrating from a v36 DB with existing populated rows leaves every
// existing row with containers_enabled=0 and containers_flag=0. The
// columns are NOT NULL DEFAULT 0 so existing rows must take the default
// (no row is left NULL and no row is mistakenly backfilled to 1).
func TestMigration_V36ToV37_PopulatedV36RowsDefaultToZero(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v36_populated.db")
	seedV36DB(t, dbPath, false, false)

	// Seed live rows on the pre-migration shape via a raw connection.
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	// Two agent_status rows.
	if _, err := rawConn.Exec(
		`INSERT INTO agent_status (session_name, repo, worktree, state, last_seen, instance_id)
		 VALUES (?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?)`,
		"nixos-config@main", "nixos-config", "/repo", "active", 1234, "inst-a",
		"nixos-config@feat", "nixos-config", "/repo-feat", "active", 1234, "inst-b",
	); err != nil {
		t.Fatalf("seed agent_status rows: %v", err)
	}
	// Matching sessions rows are required by spawn_inputs's FK.
	if _, err := rawConn.Exec(
		`INSERT INTO sessions (instance_id, session_name, repo, worktree, harness, started_at)
		 VALUES (?, ?, ?, ?, 'pi', 0), (?, ?, ?, ?, 'pi', 0)`,
		"inst-a", "nixos-config@main", "nixos-config", "/repo",
		"inst-b", "nixos-config@feat", "nixos-config", "/repo-feat",
	); err != nil {
		t.Fatalf("seed sessions rows: %v", err)
	}
	// Two spawn_inputs rows on the pre-v37 shape (no containers_flag).
	if _, err := rawConn.Exec(
		`INSERT INTO spawn_inputs (instance_id, created_at) VALUES (?, ?), (?, ?)`,
		"inst-a", 1234, "inst-b", 5678,
	); err != nil {
		t.Fatalf("seed spawn_inputs rows: %v", err)
	}
	rawConn.Close()

	// Run the migration.
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Verify every existing agent_status row has containers_enabled = 0.
	var nonZeroAgentStatus int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM agent_status WHERE containers_enabled != 0`,
	).Scan(&nonZeroAgentStatus); err != nil {
		t.Fatalf("count agent_status rows with containers_enabled != 0: %v", err)
	}
	if nonZeroAgentStatus != 0 {
		t.Errorf("post-migration: %d agent_status rows have containers_enabled != 0; want 0", nonZeroAgentStatus)
	}

	// Verify every existing spawn_inputs row has containers_flag = 0.
	var nonZeroSpawnInputs int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM spawn_inputs WHERE containers_flag != 0`,
	).Scan(&nonZeroSpawnInputs); err != nil {
		t.Fatalf("count spawn_inputs rows with containers_flag != 0: %v", err)
	}
	if nonZeroSpawnInputs != 0 {
		t.Errorf("post-migration: %d spawn_inputs rows have containers_flag != 0; want 0", nonZeroSpawnInputs)
	}

	// And confirm the rows are still readable through the Go API with the
	// new fields defaulting to false.
	st, err := d.CurrentStatus("nixos-config@main")
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: row missing")
	}
	if st.ContainersEnabled {
		t.Error("CurrentStatus.ContainersEnabled = true on pre-migration row; want false")
	}

	si, err := d.SpawnInputsByInstanceID("inst-a")
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if si == nil {
		t.Fatal("SpawnInputsByInstanceID: row missing")
	}
	if si.ContainersFlag {
		t.Error("SpawnInputsByInstanceID.ContainersFlag = true on pre-migration row; want false")
	}
}

// TestStatus_ContainersEnabledDefaultsFalse verifies that the Status
// roundtrip through CurrentStatus returns ContainersEnabled = false for
// a fresh row created via the existing UpsertStatus writer (which does
// not set containers_enabled — the column takes its DEFAULT of 0).
//
// AC: db.Status has a ContainersEnabled bool field; CurrentStatus
// populates it from the column (defaulting to false).
func TestStatus_ContainersEnabledDefaultsFalse(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "status_default.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	const session = "prism-test@main"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus: nil status for freshly inserted row")
	}
	if st.ContainersEnabled {
		t.Errorf("fresh status default ContainersEnabled = true, want false")
	}
}

// seedSessionForFK inserts a sessions row so a subsequent spawn_inputs
// INSERT can satisfy the FK. Returns the instance_id used.
func seedSessionForFK(t *testing.T, d *db.DB, instanceID, sessionName string) {
	t.Helper()
	sess := db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "nixos-config",
		Worktree:    "/repo",
		Harness:     "pi",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
}

// TestSpawnInputs_ContainersFlagRoundtripDefaultsFalse verifies the AC:
// InsertSpawnInputs writes containers_flag, defaulting to false when the
// caller does not set it. We insert a SpawnInputs value with the field
// unset (zero-value bool == false) and verify the round-trip returns
// false.
func TestSpawnInputs_ContainersFlagRoundtripDefaultsFalse(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "roundtrip_default.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	seedSessionForFK(t, d, "inst-default", "nixos-config@main")
	// Insert spawn_inputs with ContainersFlag unset (zero-value false).
	si := db.SpawnInputs{InstanceID: "inst-default", CreatedAt: 1234}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID("inst-default")
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("SpawnInputsByInstanceID: row missing")
	}
	if got.ContainersFlag {
		t.Errorf("ContainersFlag roundtrip default: got true, want false")
	}
}

// TestSpawnInputs_ContainersFlagRoundtripTrue verifies that setting
// ContainersFlag = true on the input persists and round-trips back as
// true. This is the positive-case bookend to
// TestSpawnInputs_ContainersFlagRoundtripDefaultsFalse: proves the
// boolean is actually wired through both directions of InsertSpawnInputs
// / SpawnInputsByInstanceID, not just defaulting in lockstep.
func TestSpawnInputs_ContainersFlagRoundtripTrue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "roundtrip_true.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	seedSessionForFK(t, d, "inst-true", "nixos-config@main")
	si := db.SpawnInputs{InstanceID: "inst-true", CreatedAt: 1234, ContainersFlag: true}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	got, err := d.SpawnInputsByInstanceID("inst-true")
	if err != nil {
		t.Fatalf("SpawnInputsByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("SpawnInputsByInstanceID: row missing")
	}
	if !got.ContainersFlag {
		t.Errorf("ContainersFlag roundtrip true: got false, want true")
	}
}
