// Package db provides the prism SQLite database layer.
//
// The database is located at $XDG_STATE_HOME/prism/prism.db, falling back to
// $HOME/.local/state/prism/prism.db. All tables (agent_events, agent_status,
// bus_messages, session_groups, sessions, schema_version) are created on Open
// if they do not already exist.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // register sqlite3 driver
)

const (
	// PortRangeStart is the first port in the allocation range (inclusive).
	PortRangeStart = 14000
	// PortRangeEnd is the last port in the allocation range (inclusive).
	PortRangeEnd = 14999
)

// DB wraps a SQLite connection.
type DB struct {
	conn *sql.DB
	path string
}

// Path returns the filesystem path of the database file.
func (d *DB) Path() string { return d.path }

// QueryRow executes a query that returns at most one row. Exposed for testing.
func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.conn.QueryRow(query, args...)
}


const schema = `
CREATE TABLE IF NOT EXISTS agent_events (
  id                 TEXT PRIMARY KEY,
  session_name       TEXT NOT NULL,
  repo               TEXT NOT NULL,
  worktree           TEXT NOT NULL,
  harness_session_id TEXT,
  type               TEXT NOT NULL,
  payload            TEXT NOT NULL,
  created_at         INTEGER NOT NULL,
  instance_id        TEXT REFERENCES sessions(instance_id)
);
CREATE INDEX IF NOT EXISTS idx_events_session ON agent_events(session_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_repo    ON agent_events(repo, type, created_at DESC);

CREATE TABLE IF NOT EXISTS sessions (
  instance_id         TEXT PRIMARY KEY,
  session_name        TEXT NOT NULL,
  agent_role          TEXT,
  root_agent_name     TEXT,
  repo                TEXT NOT NULL,
  worktree            TEXT NOT NULL,
  harness             TEXT NOT NULL,
  harness_session_id  TEXT,
  group_id            TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
  started_at          INTEGER NOT NULL,
  ended_at            INTEGER,
  end_state           TEXT,
  archive_path        TEXT,
  prism_version       TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_repo_started ON sessions(repo, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_name         ON sessions(session_name, started_at DESC);

CREATE TABLE IF NOT EXISTS session_groups (
  group_id       TEXT PRIMARY KEY,
  parent_session TEXT NOT NULL,
  created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS agent_status (
  session_name      TEXT PRIMARY KEY,
  repo              TEXT NOT NULL,
  worktree          TEXT NOT NULL,
  state             TEXT NOT NULL,
  title             TEXT,
  agent_name        TEXT,
  model_id          TEXT,
  root_agent_name   TEXT,
  root_model_id     TEXT,
  isolation_mode    TEXT,
  instance_id       TEXT,
  last_seen         INTEGER NOT NULL,
  ended_at          INTEGER,
  harness           TEXT NOT NULL DEFAULT 'opencode',
  harness_session_id TEXT,
  harness_port      INTEGER,
  group_id          TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS bus_messages (
  id               TEXT PRIMARY KEY,
  from_session     TEXT NOT NULL,
  to_session       TEXT NOT NULL,
  to_instance_id   TEXT,
  repo             TEXT NOT NULL,
  text             TEXT NOT NULL,
  urgency          TEXT NOT NULL DEFAULT 'normal',
  sent_at          INTEGER NOT NULL,
  delivered_at     INTEGER,
  failed_at        INTEGER
);
CREATE INDEX IF NOT EXISTS idx_bus_pending ON bus_messages(to_session, delivered_at)
  WHERE delivered_at IS NULL;

CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER NOT NULL
);

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
CREATE INDEX IF NOT EXISTS idx_pending_merges_status_instance ON pending_merges(instance_id, status, queue_position);
CREATE INDEX IF NOT EXISTS idx_pending_merges_status_session  ON pending_merges(session_name, status, queue_position);

CREATE TABLE IF NOT EXISTS spawn_outcome (
    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,
    -- Process-level
    end_state              TEXT,
    exit_code              INTEGER,
    duration_ms            INTEGER,
    interrupted_count      INTEGER NOT NULL DEFAULT 0,
    compaction_count       INTEGER NOT NULL DEFAULT 0,
    error_event_count      INTEGER NOT NULL DEFAULT 0,
    permission_ask_count   INTEGER NOT NULL DEFAULT 0,
    permission_denied_count INTEGER NOT NULL DEFAULT 0,
    doom_loop_count        INTEGER NOT NULL DEFAULT 0,
    -- Agent-level
    pr_number              INTEGER,
    pr_merged_at           INTEGER,
    review_group_id        TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
    review_verdict         TEXT,
    review_pass_count      INTEGER,
    review_fail_count      INTEGER,
    review_none_count      INTEGER,
    -- Rubric-level (reserved for future grader, NULL until then)
    rubric_verdict         TEXT,
    rubric_score           REAL,
    rubric_breakdown       TEXT,
    rubric_grader          TEXT,
    -- Per-axis aggregations (pre-computed at session-end)
    tokens_input_total       INTEGER NOT NULL DEFAULT 0,
    tokens_output_total      INTEGER NOT NULL DEFAULT 0,
    tokens_cache_read_total  INTEGER NOT NULL DEFAULT 0,
    tokens_cache_write_total INTEGER NOT NULL DEFAULT 0,
    cost_usd_total           REAL    NOT NULL DEFAULT 0,
    tool_call_count          INTEGER NOT NULL DEFAULT 0,
    tool_error_count         INTEGER NOT NULL DEFAULT 0,
    msg_assistant_count      INTEGER NOT NULL DEFAULT 0,
    time_to_first_event_ms   INTEGER,
    time_to_finished_ms      INTEGER,
    -- Audit
    computed_at            INTEGER NOT NULL,
    schema_version         INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_spawn_outcome_end_state    ON spawn_outcome(end_state);
CREATE INDEX IF NOT EXISTS idx_spawn_outcome_pr_number    ON spawn_outcome(pr_number);
CREATE INDEX IF NOT EXISTS idx_spawn_outcome_review_group ON spawn_outcome(review_group_id);

CREATE TABLE IF NOT EXISTS spawn_inputs (
    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,

    -- Inputs as the user passed them (NULL = flag not passed; default in effect).
    profile_name           TEXT,
    model_flag             TEXT,
    variant_flag           TEXT,
    agent_flag             TEXT,
    harness_flag           TEXT,
    isolation_flag         TEXT,
    host_mode_flag         INTEGER NOT NULL DEFAULT 0,
    pr_number              INTEGER,
    branch_flag            TEXT,
    ignore_concurrency_cap INTEGER NOT NULL DEFAULT 0,

    -- C.2: per-role model-variant overrides (JSON; NULL if no overrides).
    model_variant_overrides TEXT,

    -- C4.SK: hash of the skills directory at spawn time.
    -- Shape: "nix:<store-basename>" for nix-managed dirs, "sha256:<hex>" otherwise.
    skills_manifest_hash    TEXT,

    -- C4.PT: reserved for prompt-template hash (not yet populated).
    prompt_template_hash    TEXT,

    -- C4.AP: hash of the agent role file at spawn time.
    -- Shape: "nix:<store-basename>" for nix-managed files, "sha256:<hex>" otherwise.
    -- NULL when no --agent flag was passed or the role file does not exist.
    agent_prompt_hash       TEXT,

    -- Prompt delivered to the harness, captured at spawn.
    prompt_text            TEXT,
    prompt_source          TEXT,

    -- P4.ABTEST: UUID shared between the two sibling sessions of an --abtest pair.
    -- NULL for non-abtest sessions.
    abtest_pair_id         TEXT,

    -- Free-form JSON blob for forward-compat.
    extras                 TEXT,

    created_at             INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_spawn_inputs_profile         ON spawn_inputs(profile_name);
CREATE INDEX IF NOT EXISTS idx_spawn_inputs_harness_profile ON spawn_inputs(harness_flag, profile_name);

CREATE TABLE IF NOT EXISTS harness_frames (
  id            TEXT PRIMARY KEY,
  session_name  TEXT NOT NULL,
  instance_id   TEXT,
  direction     TEXT NOT NULL,
  type          TEXT,
  payload       TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_harness_frames_session ON harness_frames(session_name, created_at);
CREATE INDEX IF NOT EXISTS idx_harness_frames_session_dir ON harness_frames(session_name, direction, created_at);
`

// Open opens (or creates) the prism database at path.
// It creates parent directories as needed, enables WAL mode, enforces foreign
// keys, runs the full schema, and sets schema_version=11 if the table is empty.
// Pending migrations are applied in order: v1→v2 adds agent_name/model_id;
// v2→v3 adds root_agent_name/root_model_id; v3→v4 adds opencode_port to
// agent_status; v4→v5 adds host_mode to agent_status; v5→v6 adds instance_id
// to agent_status and to_instance_id to bus_messages; v6→v7 adds failed_at to
// bus_messages; v7→v8 adds harness, harness_session_id, and harness_port to
// agent_status; v8→v9 adds the session_groups table and group_id FK column
// (with ON DELETE SET NULL) to agent_status via rename-and-recreate so that
// the REFERENCES clause is present in the schema metadata and fully enforced
// by PRAGMA foreign_keys = ON on both fresh and migrated databases;
// v9→v10 adds isolation_mode TEXT to agent_status (nullable, back-compat);
// v10→v11 drops the legacy opencode_port and opencode_sid columns from
// agent_status (harness-agnostic equivalents harness_port and harness_session_id
// have been the canonical columns since v8; the legacy names were dual-written
// for back-compat and are now removed);
// v11→v12 adds a partial unique index to enforce at most one active coordinator
// per repo (§6.1 from #849): UNIQUE (repo) WHERE root_agent_name='coordinator'
// AND ended_at IS NULL. The IF NOT EXISTS guard makes this idempotent so that
// databases already at v12 (e.g. from a re-run) do not fail.
// v12→v13 is a one-shot maintenance migration that ends (sets ended_at=now,
// in milliseconds) any agent_status rows whose session_name matches legacy
// malformed review-session patterns from a historical recursive-review bug
// (#826): doubled ~review, back-to-back ~review~review, or bare ~review-N-review
// (no role suffix). Only rows where ended_at IS NULL and last_seen IS NULL,
// zero, or older than 7 days are touched. The 7-day threshold is also expressed
// in milliseconds ((unixepoch('now') - 604800) * 1000) to match the column unit.
// Rows already with ended_at set are left alone (idempotent).
// v13→v14 is a one-shot backfill that populates agent_status.last_seen from
// MAX(agent_events.created_at) for sessions where last_seen IS NULL or 0 (i.e.
// the column was never populated by a live WriteEvent call). It is idempotent:
// sessions that already have a non-zero last_seen are left untouched. Rows with
// no matching agent_events remain at 0 (COALESCE preserves the NOT NULL
// constraint). This fixes the gap described in issue #824 for pre-existing rows.
// v14→v15 renames the agent_events.opencode_sid column to harness_session_id
// to match the harness-agnostic naming convention used on agent_status. SQLite
// supports ALTER TABLE ... RENAME COLUMN ... since 3.25 (2018). The migration
// is idempotent: it checks whether opencode_sid still exists before acting, so
// running it twice against an already-migrated DB is safe.
// v15→v16 introduces the sessions table (immutable-per-incarnation, keyed by
// instance_id) and adds a nullable instance_id TEXT column to agent_events
// (FK to sessions.instance_id). It also backfills one sessions row per
// currently-live agent_status row (ended_at IS NULL AND instance_id IS NOT NULL
// AND instance_id != '') so in-flight sessions are queryable immediately after
// migration. Rows with empty instance_id are skipped (a warning is printed).
// This migration is idempotent: CREATE TABLE IF NOT EXISTS and the ALTER TABLE
// guard (pragma_table_info check) make it safe to run on an already-migrated DB.
// v16→v17 is a no-op bridge that reserves the slot occupied by PR #1014
// (local serial merge queue / pending_merges table). When #1014 lands first
// the DB already arrives at v17 and this block is skipped; when this PR lands
// first the bridge bumps the version so the v17→v18 block below is reachable.
// v17→v18 is a one-shot backfill that fixes sessions rows whose started_at was
// persisted as -62135596800000 (Go's zero time.Time{} marshalled via UnixMilli)
// due to a wrong zero-value guard in InsertSession. For each such row it sets
// started_at to MIN(agent_events.created_at) for the matching instance_id.
// Rows with no matching agent_events are left unchanged (they display as "—"
// via the formatDurationLong defence-in-depth fallback). The migration is
// idempotent: a second run finds no rows with negative started_at and is a
// no-op. Fresh databases have no such rows so the migration is trivially safe.
// v18→v19 adds the pending_merges table for the local serial merge queue
// (#783). Uses CREATE TABLE IF NOT EXISTS and CREATE INDEX IF NOT EXISTS so the
// migration is idempotent (safe to run twice). Both the declarative schema
// block and the migration produce identical sqlite_master output on a fresh
// database — fresh databases have the table from the schema block and skip the
// CREATE silently.
// v19→v20 adds idx_pending_merges_status_session ON
// pending_merges(session_name, status, queue_position) to cover the
// MergeQueueHead query, which was changed from filtering by instance_id to
// filtering by session_name (#1039). The old index
// idx_pending_merges_status_instance is preserved (it covers
// AbandonWatchingMerges and CancelMerge). CREATE INDEX IF NOT EXISTS makes
// this idempotent.
// v20→v21 is a one-shot backfill that sets sessions.harness_session_id from
// agent_status.harness_session_id for rows where sessions.harness_session_id
// IS NULL. This fixes sessions created before UpdateHarnessSessionID was
// changed to write to both tables (#1126). The join is on instance_id, which
// is present in both tables. Rows with no matching agent_status row, or where
// agent_status.harness_session_id is also NULL, are left unchanged — their
// raw/ directories will remain empty (the harness never ran for them).
// The migration is idempotent: a second run matches no rows (all NULL sessions
// rows either got backfilled or have no agent_status counterpart).
// v21→v22 extends the v17→v18 started_at backfill to also cover rows where
// started_at = 0 (literal Unix epoch zero). The earlier migration only fixed
// rows with started_at < 0 (Go zero-time as -62135596800000 ms); a separate
// code path could insert started_at = 0 directly, producing
// "00010101T000000Z_" archive directory names (#1127). Recovery strategy is
// the same: set started_at = MIN(agent_events.created_at) for the matching
// instance_id. Idempotent: rows with started_at > 0 are skipped.
// v22→v23 is a one-shot backfill that populates agent_status.isolation_mode
// for every row where it is currently NULL (pre-v10 archived rows that were
// added before the column existed). The value mirrors the runtime fallback in
// the old EffectiveIsolationMode() (since deleted): host_mode=1 → 'host', otherwise →
// 'podman'. The WHERE clause makes the migration idempotent: rows already set
// are skipped on any subsequent open. This is the Phase A prerequisite for the
// A4 deprecation-removal sequence (#1129).
// v23→v24 drops sessions.outcome_summary (reserved by C.1 as a JSON
// placeholder with zero writers) and adds the spawn_outcome table (C.3 §4).
// The outcome_summary column is dropped with a rebuild-via-rename strategy
// because SQLite does not support ALTER TABLE ... DROP COLUMN on columns with
// foreign-key references (and to maintain compatibility with older SQLite
// builds). The spawn_outcome table is created with CREATE TABLE IF NOT EXISTS
// and the three indexes are created with CREATE INDEX IF NOT EXISTS, making
// the migration idempotent. Fresh databases skip the column-drop (the column
// never existed in the declarative schema) and the table creation is a no-op
// because the schema block above already created it.
// v24→v25 adds the spawn_inputs table (C.1/C.4 §4.1) and the agent_prompt_hash
// column within it (C4.AP). The table is created with CREATE TABLE IF NOT
// EXISTS and its indexes with CREATE INDEX IF NOT EXISTS, making the migration
// idempotent. Fresh databases already have the table from the declarative
// schema block above, so the CREATE is a no-op.
// v25→v26 drops the host_mode column from agent_status. All rows already have
// isolation_mode set (guaranteed by the v22→v23 backfill, #1129), so the
// host_mode column is redundant. The column is removed via a table-rebuild
// migration: the table is recreated without host_mode and all rows are copied
// across. isolation_mode remains nullable TEXT in the new schema. The migration
// is conditional: it checks whether host_mode still exists before rebuilding,
// making it idempotent (#1137).
// v26→v27 adds the harness_frames table for the raw PI JSONL frame archive
// (P5.LOGS / #1218). The table stores every inbound and outbound frame on a
// socket-pipe session keyed by session_name + created_at, with a denormalised
// type column for fast --types filtering. CREATE TABLE IF NOT EXISTS and
// CREATE INDEX IF NOT EXISTS make the migration idempotent; fresh databases
// already have the table from the declarative schema block above.
// v27→v28 adds the abtest_pair_id column to spawn_inputs (P4.ABTEST, #1216).
// The column is nullable TEXT; NULL means the session is not part of an A/B
// test pair. A partial index on non-NULL values enables efficient pair lookup.
// The ALTER TABLE is guarded by a pragma_table_info check so the migration is
// idempotent on fresh databases where the base schema already includes the
// column.
//
// PRAGMA foreign_keys = ON: SQLite foreign-key enforcement is off by default.
// It is set explicitly here — the single constructor through which all prism
// DB connections are opened — so every connection benefits automatically.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("db: create parent dirs: %w", err)
	}

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}

	conn, err = openAndConfigure(conn, path)
	if err != nil {
		return nil, err
	}

	return &DB{conn: conn, path: path}, nil
}

// openAndConfigure sets pragmas, applies the schema, seeds the schema version,
// and runs pending migrations on an already-opened connection. It closes conn
// on any error and returns the same conn on success.
func openAndConfigure(conn *sql.DB, path string) (*sql.DB, error) {
	// Enable WAL mode for better concurrent read/write performance.
	if _, err := conn.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: set WAL mode: %w", err)
	}

	// Wait up to 5 seconds before returning SQLITE_BUSY when the DB is locked
	// by another process (e.g. the plugin writing concurrently).
	if _, err := conn.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: set busy timeout: %w", err)
	}

	// Enforce foreign-key constraints. SQLite disables FK enforcement by
	// default; this must be set per connection, which is why it lives here
	// (the single constructor used for every prism DB connection).
	if _, err := conn.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: enable foreign keys: %w", err)
	}

	// Create all tables.
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: apply schema: %w", err)
	}

	if err := seedSchemaVersionIfEmpty(conn); err != nil {
		conn.Close()
		return nil, err
	}

	if err := runMigrations(conn); err != nil {
		conn.Close()
		return nil, err
	}

	return conn, nil
}

// seedSchemaVersionIfEmpty inserts schema_version=11 when the table is empty.
// Fresh databases have all current columns so starting at 11 is safe — the
// v11→v24 migrations are all no-ops on a fresh DB.
func seedSchemaVersionIfEmpty(conn *sql.DB) error {
	var count int
	if err := conn.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count); err != nil {
		return fmt.Errorf("db: check schema_version: %w", err)
	}
	if count == 0 {
		if _, err := conn.Exec("INSERT INTO schema_version (version) VALUES (11)"); err != nil {
			return fmt.Errorf("db: set schema_version: %w", err)
		}
	}
	return nil
}

// runMigrations reads the current schema_version and applies all pending
// migrations in order from v1 to v27.
func runMigrations(conn *sql.DB) error {
	var version int
	if err := conn.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		return fmt.Errorf("db: read schema_version: %w", err)
	}
	if err := migrateV1toV2(conn, &version); err != nil {
		return err
	}
	if err := migrateV2toV3(conn, &version); err != nil {
		return err
	}
	if err := migrateV3toV4(conn, &version); err != nil {
		return err
	}
	if err := migrateV4toV5(conn, &version); err != nil {
		return err
	}
	if err := migrateV5toV6(conn, &version); err != nil {
		return err
	}
	if err := migrateV6toV7(conn, &version); err != nil {
		return err
	}
	if err := migrateV7toV8(conn, &version); err != nil {
		return err
	}
	if err := migrateV8toV9(conn, &version); err != nil {
		return err
	}
	if err := migrateV9toV10(conn, &version); err != nil {
		return err
	}
	if err := migrateV10toV11(conn, &version); err != nil {
		return err
	}
	if err := migrateV11toV12(conn, &version); err != nil {
		return err
	}
	if err := migrateV12toV13(conn, &version); err != nil {
		return err
	}
	if err := migrateV13toV14(conn, &version); err != nil {
		return err
	}
	if err := migrateV14toV15(conn, &version); err != nil {
		return err
	}
	if err := migrateV15toV16(conn, &version); err != nil {
		return err
	}
	if err := migrateV16toV17(conn, &version); err != nil {
		return err
	}
	if err := migrateV17toV18(conn, &version); err != nil {
		return err
	}
	if err := migrateV18toV19(conn, &version); err != nil {
		return err
	}
	if err := migrateV19toV20(conn, &version); err != nil {
		return err
	}
	if err := migrateV20toV21(conn, &version); err != nil {
		return err
	}
	if err := migrateV21toV22(conn, &version); err != nil {
		return err
	}
	if err := migrateV22toV23(conn, &version); err != nil {
		return err
	}
	if err := migrateV23toV24(conn, &version); err != nil {
		return err
	}
	if err := migrateV24toV25(conn, &version); err != nil {
		return err
	}
	if err := migrateV25toV26(conn, &version); err != nil {
		return err
	}
	if err := migrateV26toV27(conn, &version); err != nil {
		return err
	}
	if err := migrateV27ToV28(conn, &version); err != nil {
		return err
	}
	return nil
}

func migrateV1toV2(conn *sql.DB, version *int) error {
	if *version != 1 {
		return nil
	}
	// Migration v1 → v2: add agent_name and model_id to agent_status.
	migrations := []string{
		"ALTER TABLE agent_status ADD COLUMN agent_name TEXT",
		"ALTER TABLE agent_status ADD COLUMN model_id TEXT",
		"UPDATE schema_version SET version = 2",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v1→v2: %w", err)
		}
	}
	*version = 2
	return nil
}

func migrateV2toV3(conn *sql.DB, version *int) error {
	if *version != 2 {
		return nil
	}
	// Migration v2 → v3: add root_agent_name and root_model_id to agent_status.
	migrations := []string{
		"ALTER TABLE agent_status ADD COLUMN root_agent_name TEXT",
		"ALTER TABLE agent_status ADD COLUMN root_model_id TEXT",
		"UPDATE schema_version SET version = 3",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v2→v3: %w", err)
		}
	}
	*version = 3
	return nil
}

func migrateV3toV4(conn *sql.DB, version *int) error {
	if *version != 3 {
		return nil
	}
	// Migration v3 → v4: add opencode_port to agent_status.
	migrations := []string{
		"ALTER TABLE agent_status ADD COLUMN opencode_port INTEGER",
		"UPDATE schema_version SET version = 4",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v3→v4: %w", err)
		}
	}
	*version = 4
	return nil
}

func migrateV4toV5(conn *sql.DB, version *int) error {
	if *version != 4 {
		return nil
	}
	// Migration v4 → v5: add host_mode to agent_status.
	migrations := []string{
		"ALTER TABLE agent_status ADD COLUMN host_mode INTEGER NOT NULL DEFAULT 0",
		"UPDATE schema_version SET version = 5",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v4→v5: %w", err)
		}
	}
	*version = 5
	return nil
}

func migrateV5toV6(conn *sql.DB, version *int) error {
	if *version != 5 {
		return nil
	}
	// Migration v5 → v6: add instance_id to agent_status and
	// to_instance_id to bus_messages for session instance isolation.
	// Both columns are nullable so existing rows are unaffected.
	migrations := []string{
		"ALTER TABLE agent_status ADD COLUMN instance_id TEXT",
		"ALTER TABLE bus_messages ADD COLUMN to_instance_id TEXT",
		"UPDATE schema_version SET version = 6",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v5→v6: %w", err)
		}
	}
	*version = 6
	return nil
}

func migrateV6toV7(conn *sql.DB, version *int) error {
	if *version != 6 {
		return nil
	}
	// Migration v6 → v7: add failed_at to bus_messages for honest delivery
	// tracking. NULL means not yet attempted or delivered; a non-NULL value
	// records the ms timestamp when delivery exhausted all retries.
	// Additive — existing rows are unaffected.
	migrations := []string{
		"ALTER TABLE bus_messages ADD COLUMN failed_at INTEGER",
		"UPDATE schema_version SET version = 7",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v6→v7: %w", err)
		}
	}
	*version = 7
	return nil
}

func migrateV7toV8(conn *sql.DB, version *int) error {
	if *version != 7 {
		return nil
	}
	// Migration v7 → v8: add harness columns to agent_status for multi-harness
	// support (RFC #691). harness defaults to 'opencode' so existing rows
	// retain their implicit harness assignment without data loss.
	// harness_session_id and harness_port are nullable parallels of
	// opencode_sid and opencode_port; both old and new columns are written
	// simultaneously (dual-write) from this schema version onward.
	// Additive — existing rows are unaffected.
	migrations := []string{
		"ALTER TABLE agent_status ADD COLUMN harness TEXT NOT NULL DEFAULT 'opencode'",
		"ALTER TABLE agent_status ADD COLUMN harness_session_id TEXT",
		"ALTER TABLE agent_status ADD COLUMN harness_port INTEGER",
		"UPDATE schema_version SET version = 8",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v7→v8: %w", err)
		}
	}
	*version = 8
	return nil
}

func migrateV8toV9(conn *sql.DB, version *int) error {
	if *version != 8 {
		return nil
	}
	// Migration v8 → v9: introduce session_groups table and add group_id FK
	// column to agent_status. group_id is nullable so existing rows are
	// unaffected (they receive NULL). The FK is enforced with ON DELETE SET
	// NULL so that deleting a session_groups row clears group_id on member
	// sessions without removing their history.
	//
	// SQLite does not support adding a column with a REFERENCES clause via
	// ALTER TABLE ADD COLUMN. We therefore use the recommended rename-and-
	// recreate pattern: create a new table with the REFERENCES clause, copy
	// all rows across, drop the old table, and rename the new one. This is
	// wrapped in a transaction with PRAGMA foreign_keys = OFF (required by
	// the SQLite docs for schema changes) so the intermediate state — where
	// the old table has no FK and the new table exists alongside it — is
	// never visible to concurrent readers and does not trigger spurious FK
	// violations. foreign_keys is re-enabled immediately after.
	//
	// See https://www.sqlite.org/lang_altertable.html#otheralter
	if err := func() error {
		if _, err := conn.Exec("PRAGMA foreign_keys = OFF"); err != nil {
			return fmt.Errorf("disable FK: %w", err)
		}
		tx, err := conn.Begin()
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		steps := []string{
			// Create the session_groups table first (the FK target must
			// exist before we can reference it).
			`CREATE TABLE IF NOT EXISTS session_groups (
			  group_id       TEXT PRIMARY KEY,
			  parent_session TEXT NOT NULL,
			  created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`,
			// Recreate agent_status with the REFERENCES clause.
			`CREATE TABLE agent_status_new (
			  session_name      TEXT PRIMARY KEY,
			  repo              TEXT NOT NULL,
			  worktree          TEXT NOT NULL,
			  state             TEXT NOT NULL,
			  title             TEXT,
			  opencode_sid      TEXT,
			  agent_name        TEXT,
			  model_id          TEXT,
			  root_agent_name   TEXT,
			  root_model_id     TEXT,
			  opencode_port     INTEGER,
			  host_mode         INTEGER NOT NULL DEFAULT 0,
			  instance_id       TEXT,
			  last_seen         INTEGER NOT NULL,
			  ended_at          INTEGER,
			  harness           TEXT NOT NULL DEFAULT 'opencode',
			  harness_session_id TEXT,
			  harness_port      INTEGER,
			  group_id          TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
			)`,
			// Copy all existing rows; new group_id column gets NULL.
			`INSERT INTO agent_status_new
			  SELECT session_name, repo, worktree, state, title,
			         opencode_sid, agent_name, model_id,
			         root_agent_name, root_model_id, opencode_port,
			         host_mode, instance_id, last_seen, ended_at,
			         harness, harness_session_id, harness_port, NULL
			  FROM agent_status`,
			"DROP TABLE agent_status",
			"ALTER TABLE agent_status_new RENAME TO agent_status",
			"UPDATE schema_version SET version = 9",
		}
		for _, s := range steps {
			if _, err := tx.Exec(s); err != nil {
				return fmt.Errorf("step %q: %w", s[:min(40, len(s))], err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
			return fmt.Errorf("re-enable FK: %w", err)
		}
		return nil
	}(); err != nil {
		return fmt.Errorf("db: migration v8→v9: %w", err)
	}
	*version = 9
	return nil
}

func migrateV9toV10(conn *sql.DB, version *int) error {
	if *version != 9 {
		return nil
	}
	// Migration v9 → v10: add isolation_mode TEXT to agent_status.
	// Nullable so existing rows receive NULL (back-compat with pre-10 data).
	// When NULL, callers derive the mode from host_mode for back-compat.
	migrations := []string{
		"ALTER TABLE agent_status ADD COLUMN isolation_mode TEXT",
		"UPDATE schema_version SET version = 10",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v9→v10: %w", err)
		}
	}
	*version = 10
	return nil
}

func migrateV10toV11(conn *sql.DB, version *int) error {
	if *version != 10 {
		return nil
	}
	// Migration v10 → v11: drop the legacy opencode_port and opencode_sid
	// columns from agent_status. Data in these columns was dual-written to
	// harness_port and harness_session_id since v8; however for databases
	// that were not actively used after v8 the harness columns may still be
	// NULL while the legacy columns carry data. Back-fill first so that no
	// data is lost, then drop the legacy columns.
	// SQLite supports ALTER TABLE DROP COLUMN since 3.35 (2021).
	migrations := []string{
		// Back-fill harness_session_id from opencode_sid where not already set.
		`UPDATE agent_status SET harness_session_id = opencode_sid
		  WHERE harness_session_id IS NULL AND opencode_sid IS NOT NULL`,
		// Back-fill harness_port from opencode_port where not already set.
		`UPDATE agent_status SET harness_port = opencode_port
		  WHERE harness_port IS NULL AND opencode_port IS NOT NULL`,
		"ALTER TABLE agent_status DROP COLUMN opencode_port",
		"ALTER TABLE agent_status DROP COLUMN opencode_sid",
		"UPDATE schema_version SET version = 11",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v10→v11: %w", err)
		}
	}
	*version = 11
	return nil
}

func migrateV11toV12(conn *sql.DB, version *int) error {
	if *version != 11 {
		return nil
	}
	// Migration v11 → v12: add a partial unique index enforcing at most one
	// active coordinator per repo (§6.1 from #849). The index is partial so
	// that:
	//   - ended coordinators (ended_at IS NOT NULL) are excluded, allowing
	//     a new coordinator to start for the same repo after the previous one ends.
	//   - sessions without root_agent_name='coordinator' are unaffected.
	// CREATE INDEX IF NOT EXISTS makes this idempotent.
	migrations := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_active_coordinator_per_repo
		   ON agent_status (repo)
		   WHERE root_agent_name = 'coordinator' AND ended_at IS NULL`,
		"UPDATE schema_version SET version = 12",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v11→v12: %w", err)
		}
	}
	*version = 12
	return nil
}

func migrateV12toV13(conn *sql.DB, version *int) error {
	if *version != 12 {
		return nil
	}
	// Migration v12 → v13: one-shot maintenance cleanup of agent_status rows
	// whose session_name matches legacy malformed review-agent patterns
	// produced by a historical recursive-review bug (#826).
	//
	// Patterns matched (three LIKE clauses cover all observed shapes):
	//   %~review-%~review%  — doubled ~review with no role suffix
	//                         e.g. ~review-1-review~review-1-review
	//   %~review~review%    — back-to-back ~review (older variant)
	//                         e.g. ~review-3~review
	//   %~review-%-review   — bare review suffix with no role component
	//                         e.g. ~review-1-review (trailing, no ~prefix)
	//
	// The current valid shape, <parent>~review-<N>-review-<role>, has a
	// non-empty role suffix (e.g. "-code", "-goal") and does NOT end in
	// "-review" with nothing after it, so it is NOT matched by the third
	// pattern.  It also contains no back-to-back ~review~review or
	// doubled ~review-%~review, so it is not matched by the first two.
	//
	// ended_at and last_seen are both stored in Unix milliseconds throughout
	// the codebase (time.Now().UnixMilli() / time.UnixMilli()), so:
	//   - ended_at is set to unixepoch('now') * 1000 (ms)
	//   - the 7-day staleness threshold is (unixepoch('now') - 604800) * 1000
	//     where 604800 is 7 × 86400 seconds.
	//
	// Only rows where ended_at IS NULL and last_seen is NULL, zero, or older
	// than 7 days are touched — this avoids accidentally closing any session
	// that might still be active.  Rows that already have ended_at set are
	// left alone (the WHERE ended_at IS NULL guard makes this idempotent).
	migrations := []string{
		`UPDATE agent_status
		   SET ended_at = unixepoch('now') * 1000
		 WHERE ended_at IS NULL
		   AND (last_seen IS NULL OR last_seen = 0
		        OR last_seen < ((unixepoch('now') - 604800) * 1000))
		   AND (session_name LIKE '%~review-%~review%'
		    OR  session_name LIKE '%~review~review%'
		    OR  session_name LIKE '%~review-%-review')`,
		"UPDATE schema_version SET version = 13",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v12→v13: %w", err)
		}
	}
	*version = 13
	return nil
}

func migrateV13toV14(conn *sql.DB, version *int) error {
	if *version != 13 {
		return nil
	}
	// Migration v13 → v14: one-shot backfill of agent_status.last_seen for
	// rows where last_seen is NULL or 0 (column was never populated by a live
	// WriteEvent call). We set last_seen = MAX(agent_events.created_at) for
	// the owning session. The WHERE guard (last_seen IS NULL OR last_seen = 0)
	// makes this idempotent — sessions that already have a real last_seen
	// value are left untouched. Rows with no matching agent_events get NULL
	// from the subquery; COALESCE(..., 0) keeps them at 0 so the NOT NULL
	// constraint is satisfied.
	migrations := []string{
		`UPDATE agent_status
		   SET last_seen = COALESCE(
		         (SELECT MAX(created_at) FROM agent_events
		           WHERE agent_events.session_name = agent_status.session_name),
		         0)
		 WHERE last_seen IS NULL OR last_seen = 0`,
		"UPDATE schema_version SET version = 14",
	}
	for _, m := range migrations {
		if _, err := conn.Exec(m); err != nil {
			return fmt.Errorf("db: migration v13→v14: %w", err)
		}
	}
	*version = 14
	return nil
}

func migrateV14toV15(conn *sql.DB, version *int) error {
	if *version != 14 {
		return nil
	}
	// Migration v14 → v15: rename agent_events.opencode_sid to
	// harness_session_id to match the harness-agnostic naming convention
	// already used on agent_status. SQLite supports ALTER TABLE ... RENAME
	// COLUMN ... since 3.25 (2018); the modernc.org/sqlite driver embeds a
	// recent enough SQLite version.
	//
	// Idempotency: the migration first checks whether the opencode_sid column
	// still exists using pragma_table_info. If the column is already named
	// harness_session_id (i.e. the migration ran before), the RENAME is
	// skipped and only the schema_version bump is applied. This makes it safe
	// to run twice against the same database without error.
	var colExists int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_events') WHERE name = 'opencode_sid'`,
	).Scan(&colExists); err != nil {
		return fmt.Errorf("db: migration v14→v15: check column: %w", err)
	}
	if colExists > 0 {
		if _, err := conn.Exec(`ALTER TABLE agent_events RENAME COLUMN opencode_sid TO harness_session_id`); err != nil {
			return fmt.Errorf("db: migration v14→v15: rename column: %w", err)
		}
	}
	if _, err := conn.Exec("UPDATE schema_version SET version = 15"); err != nil {
		return fmt.Errorf("db: migration v14→v15: bump version: %w", err)
	}
	*version = 15
	return nil
}

func migrateV15toV16(conn *sql.DB, version *int) error {
	if *version != 15 {
		return nil
	}
	// Migration v15 → v16: introduce the sessions table (immutable per
	// incarnation, keyed by instance_id) and add instance_id TEXT to
	// agent_events. Also backfill one sessions row per live agent_status row
	// so in-flight sessions are queryable immediately post-migration.
	//
	// Idempotency:
	//  - CREATE TABLE IF NOT EXISTS is always safe.
	//  - The ALTER TABLE is guarded by a pragma_table_info check (same
	//    pattern as v14→v15) so running twice is harmless.
	//  - The backfill INSERT uses INSERT OR IGNORE so duplicate instance_ids
	//    do not fail.
	//
	// FK ordering: sessions.group_id → session_groups, which must already
	// exist (created in the declarative schema block and in v8→v9). When FK
	// enforcement is ON this would fail if session_groups didn't exist, but
	// the migration runs after the schema block so it is always present.
	steps := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
		  instance_id         TEXT PRIMARY KEY,
		  session_name        TEXT NOT NULL,
		  agent_role          TEXT,
		  root_agent_name     TEXT,
		  repo                TEXT NOT NULL,
		  worktree            TEXT NOT NULL,
		  harness             TEXT NOT NULL,
		  harness_session_id  TEXT,
		  group_id            TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
		  started_at          INTEGER NOT NULL,
		  ended_at            INTEGER,
		  end_state           TEXT,
		  archive_path        TEXT,
		  prism_version       TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_repo_started ON sessions(repo, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_name         ON sessions(session_name, started_at DESC)`,
	}
	for _, s := range steps {
		if _, err := conn.Exec(s); err != nil {
			return fmt.Errorf("db: migration v15→v16: create sessions: %w", err)
		}
	}

	// Add instance_id to agent_events if it doesn't exist yet.
	var aeColExists int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_events') WHERE name = 'instance_id'`,
	).Scan(&aeColExists); err != nil {
		return fmt.Errorf("db: migration v15→v16: check agent_events.instance_id: %w", err)
	}
	if aeColExists == 0 {
		if _, err := conn.Exec(`ALTER TABLE agent_events ADD COLUMN instance_id TEXT REFERENCES sessions(instance_id)`); err != nil {
			return fmt.Errorf("db: migration v15→v16: add agent_events.instance_id: %w", err)
		}
	}

	// Backfill sessions from currently-live agent_status rows. A "live" row
	// is one where ended_at IS NULL and instance_id is non-empty. Rows with
	// empty or NULL instance_id are skipped (a warning is emitted). We use
	// the current time as started_at since the real start time is not stored.
	// INSERT OR IGNORE makes this idempotent.
	backfillRows, err := conn.Query(`
		SELECT session_name, instance_id, repo, worktree,
		       COALESCE(harness, 'opencode'),
		       harness_session_id, group_id, root_agent_name
		  FROM agent_status
		 WHERE ended_at IS NULL
		   AND instance_id IS NOT NULL
		   AND instance_id != ''`)
	if err != nil {
		return fmt.Errorf("db: migration v15→v16: query live sessions: %w", err)
	}
	type backfillRow struct {
		sessionName      string
		instanceID       string
		repo             string
		worktree         string
		harness          string
		harnessSessionID *string
		groupID          *string
		rootAgentName    *string
	}
	var rows []backfillRow
	for backfillRows.Next() {
		var r backfillRow
		if err := backfillRows.Scan(&r.sessionName, &r.instanceID, &r.repo, &r.worktree,
			&r.harness, &r.harnessSessionID, &r.groupID, &r.rootAgentName); err != nil {
			backfillRows.Close()
			return fmt.Errorf("db: migration v15→v16: scan live session: %w", err)
		}
		rows = append(rows, r)
	}
	backfillRows.Close()
	if err := backfillRows.Err(); err != nil {
		return fmt.Errorf("db: migration v15→v16: iterate live sessions: %w", err)
	}
	nowMs := time.Now().UnixMilli()
	for _, r := range rows {
		if _, err := conn.Exec(`
			INSERT OR IGNORE INTO sessions
			  (instance_id, session_name, repo, worktree, harness,
			   harness_session_id, group_id, root_agent_name, started_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.instanceID, r.sessionName, r.repo, r.worktree, r.harness,
			r.harnessSessionID, r.groupID, r.rootAgentName, nowMs,
		); err != nil {
			return fmt.Errorf("db: migration v15→v16: backfill sessions for %q: %w", r.sessionName, err)
		}
	}

	if _, err := conn.Exec("UPDATE schema_version SET version = 16"); err != nil {
		return fmt.Errorf("db: migration v15→v16: bump version: %w", err)
	}
	*version = 16
	return nil
}

func migrateV16toV17(conn *sql.DB, version *int) error {
	if *version != 16 {
		return nil
	}
	// Migration v16 → v17: no-op bridge. This slot is occupied by PR #1014
	// (local serial merge queue / pending_merges table). When #1014 lands
	// before this PR, the DB arrives here already at v17 and this block is
	// skipped. When this PR lands first (or standalone), this bridge bumps
	// the schema to v17 so the v17→v18 backfill block below is always
	// reachable regardless of merge order.
	if _, err := conn.Exec("UPDATE schema_version SET version = 17"); err != nil {
		return fmt.Errorf("db: migration v16→v17: bump version: %w", err)
	}
	*version = 17
	return nil
}

func migrateV17toV18(conn *sql.DB, version *int) error {
	if *version != 17 {
		return nil
	}
	// Migration v17 → v18: backfill sessions rows whose started_at was
	// persisted as -62135596800000 (Go's zero time.Time{} marshalled via
	// UnixMilli). For each broken row, use MIN(agent_events.created_at)
	// for the matching instance_id as a best-effort recovery. Rows with
	// no matching events are left unchanged.
	//
	// Idempotency: the WHERE clause filters on started_at < 0, so rows
	// already fixed by a previous run (or rows on a fresh DB) are not
	// touched.

	// Collect broken instance_ids.
	brokenRows, err := conn.Query(`
		SELECT instance_id FROM sessions WHERE started_at < 0`)
	if err != nil {
		return fmt.Errorf("db: migration v17→v18: query broken sessions: %w", err)
	}
	var brokenIDs []string
	for brokenRows.Next() {
		var iid string
		if err := brokenRows.Scan(&iid); err != nil {
			brokenRows.Close()
			return fmt.Errorf("db: migration v17→v18: scan broken session: %w", err)
		}
		brokenIDs = append(brokenIDs, iid)
	}
	brokenRows.Close()
	if err := brokenRows.Err(); err != nil {
		return fmt.Errorf("db: migration v17→v18: iterate broken sessions: %w", err)
	}

	for _, iid := range brokenIDs {
		var minTs *int64
		if err := conn.QueryRow(`
			SELECT MIN(created_at) FROM agent_events WHERE instance_id = ?`, iid,
		).Scan(&minTs); err != nil {
			return fmt.Errorf("db: migration v17→v18: min timestamp for %q: %w", iid, err)
		}
		if minTs == nil || *minTs <= 0 {
			// No usable events — leave the row unchanged.
			continue
		}
		if _, err := conn.Exec(`
			UPDATE sessions SET started_at = ? WHERE instance_id = ?`, *minTs, iid,
		); err != nil {
			return fmt.Errorf("db: migration v17→v18: update started_at for %q: %w", iid, err)
		}
	}

	if _, err := conn.Exec("UPDATE schema_version SET version = 18"); err != nil {
		return fmt.Errorf("db: migration v17→v18: bump version: %w", err)
	}
	*version = 18
	return nil
}

func migrateV18toV19(conn *sql.DB, version *int) error {
	if *version != 18 {
		return nil
	}
	// Migration v18 → v19: introduce the pending_merges table for the
	// local serial merge queue (#783). Uses CREATE TABLE IF NOT EXISTS and
	// CREATE INDEX IF NOT EXISTS so this migration is fully idempotent —
	// running it against a fresh database that already has the table from
	// the declarative schema block above is a safe no-op.
	steps := []string{
		`CREATE TABLE IF NOT EXISTS pending_merges (
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
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_merges_status_instance ON pending_merges(instance_id, status, queue_position)`,
		`UPDATE schema_version SET version = 19`,
	}
	for _, s := range steps {
		if _, err := conn.Exec(s); err != nil {
			return fmt.Errorf("db: migration v18→v19: %w", err)
		}
	}
	*version = 19
	return nil
}

func migrateV19toV20(conn *sql.DB, version *int) error {
	if *version != 19 {
		return nil
	}
	// Migration v19 → v20: add idx_pending_merges_status_session on
	// pending_merges(session_name, status, queue_position) to cover the
	// MergeQueueHead query, which was changed to filter by session_name
	// instead of instance_id (#1039). The old instance-keyed index is
	// preserved — it is still used by AbandonWatchingMerges and
	// CancelMerge. CREATE INDEX IF NOT EXISTS makes this idempotent.
	steps := []string{
		`CREATE INDEX IF NOT EXISTS idx_pending_merges_status_session ON pending_merges(session_name, status, queue_position)`,
		`UPDATE schema_version SET version = 20`,
	}
	for _, s := range steps {
		if _, err := conn.Exec(s); err != nil {
			return fmt.Errorf("db: migration v19→v20: %w", err)
		}
	}
	*version = 20
	return nil
}

func migrateV20toV21(conn *sql.DB, version *int) error {
	if *version != 20 {
		return nil
	}
	// Migration v20 → v21: backfill sessions.harness_session_id from
	// agent_status for rows where sessions.harness_session_id IS NULL.
	// UpdateHarnessSessionID historically only wrote to agent_status; this
	// one-shot backfill ensures that sessions created before the fix for
	// #1126 also have harness_session_id set in the sessions table, so that
	// cleanup.runSessionArchive can archive them on the next run.
	//
	// The join is on instance_id (present in both tables). Rows where
	// agent_status.harness_session_id is also NULL, or where there is no
	// matching agent_status row, are left unchanged.
	//
	// Idempotency: sessions rows already having a non-NULL harness_session_id
	// are excluded by the WHERE clause, so a second run is a no-op.
	if _, err := conn.Exec(`
		UPDATE sessions
		   SET harness_session_id = (
		         SELECT harness_session_id
		           FROM agent_status
		          WHERE agent_status.instance_id = sessions.instance_id
		            AND agent_status.harness_session_id IS NOT NULL
		       )
		 WHERE harness_session_id IS NULL
		   AND EXISTS (
		         SELECT 1 FROM agent_status
		          WHERE agent_status.instance_id = sessions.instance_id
		            AND agent_status.harness_session_id IS NOT NULL
		       )`); err != nil {
		return fmt.Errorf("db: migration v20→v21: backfill harness_session_id: %w", err)
	}
	if _, err := conn.Exec("UPDATE schema_version SET version = 21"); err != nil {
		return fmt.Errorf("db: migration v20→v21: bump version: %w", err)
	}
	*version = 21
	return nil
}

func migrateV21toV22(conn *sql.DB, version *int) error {
	if *version != 21 {
		return nil
	}
	// Migration v21 → v22: extend the v17→v18 started_at backfill to also
	// cover rows where started_at = 0 (literal Unix epoch). The v17→v18
	// migration fixed rows where started_at < 0 (Go zero-time marshalled as
	// -62135596800000 ms), but a separate code path could store started_at = 0
	// directly, producing "00010101T000000Z_" directory names in the archive
	// (#1127). The same recovery strategy is used: set started_at to
	// MIN(agent_events.created_at) for the matching instance_id. Rows with no
	// matching events are left with started_at = 0 and will display as
	// "00010101T000000Z_<instanceID>" in archive listings (the same
	// formatDurationLong fallback applies).
	//
	// Idempotency: rows with started_at = 0 are either fixed or have no
	// usable events; a second run is a no-op.
	brokenRows2, err := conn.Query(`SELECT instance_id FROM sessions WHERE started_at = 0`)
	if err != nil {
		return fmt.Errorf("db: migration v21→v22: query zero sessions: %w", err)
	}
	var zeroIDs []string
	for brokenRows2.Next() {
		var iid string
		if err := brokenRows2.Scan(&iid); err != nil {
			brokenRows2.Close()
			return fmt.Errorf("db: migration v21→v22: scan zero session: %w", err)
		}
		zeroIDs = append(zeroIDs, iid)
	}
	brokenRows2.Close()
	if err := brokenRows2.Err(); err != nil {
		return fmt.Errorf("db: migration v21→v22: iterate zero sessions: %w", err)
	}

	for _, iid := range zeroIDs {
		var minTs *int64
		if err := conn.QueryRow(`
			SELECT MIN(created_at) FROM agent_events WHERE instance_id = ?`, iid,
		).Scan(&minTs); err != nil {
			return fmt.Errorf("db: migration v21→v22: min timestamp for %q: %w", iid, err)
		}
		if minTs == nil || *minTs <= 0 {
			continue
		}
		if _, err := conn.Exec(`
			UPDATE sessions SET started_at = ? WHERE instance_id = ?`, *minTs, iid,
		); err != nil {
			return fmt.Errorf("db: migration v21→v22: update started_at for %q: %w", iid, err)
		}
	}

	if _, err := conn.Exec("UPDATE schema_version SET version = 22"); err != nil {
		return fmt.Errorf("db: migration v21→v22: bump version: %w", err)
	}
	*version = 22
	return nil
}

func migrateV22toV23(conn *sql.DB, version *int) error {
	if *version != 22 {
		return nil
	}
	// Migration v22 → v23: backfill isolation_mode for pre-v10 rows.
	// Rows inserted before v9→v10 landed (the migration that added the
	// isolation_mode column as NULLABLE) have isolation_mode IS NULL.
	// The CASE expression mirrors the runtime fallback in the old
	// (db.Status).EffectiveIsolationMode() (since deleted): host_mode=1 → 'host', else →
	// 'podman'. The WHERE clause skips rows already set, making the
	// migration idempotent. Fresh databases have no rows so this is a
	// no-op. (#1129)
	//
	// Skip the UPDATE when host_mode no longer exists in the schema (the
	// v25→v26 migration drops it). On a fresh database created after that
	// migration the column never existed, so the UPDATE would fail. Since
	// there are no rows with isolation_mode IS NULL on such a database, the
	// UPDATE is a no-op anyway.
	var hmColExists int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_status') WHERE name = 'host_mode'`,
	).Scan(&hmColExists); err != nil {
		return fmt.Errorf("db: migration v22→v23: check host_mode column: %w", err)
	}
	if hmColExists > 0 {
		if _, err := conn.Exec(`UPDATE agent_status SET isolation_mode = CASE WHEN host_mode = 1 THEN 'host' ELSE 'podman' END WHERE isolation_mode IS NULL`); err != nil {
			return fmt.Errorf("db: migration v22→v23: %w", err)
		}
	}
	if _, err := conn.Exec("UPDATE schema_version SET version = 23"); err != nil {
		return fmt.Errorf("db: migration v22→v23: bump version: %w", err)
	}
	*version = 23
	return nil
}

func migrateV23toV24(conn *sql.DB, version *int) error {
	if *version != 23 {
		return nil
	}
	// Migration v23 → v24: drop sessions.outcome_summary (reserved by C.1
	// as a JSON placeholder with zero writers) and add the spawn_outcome
	// table (C.3 §4, issue #1130).
	//
	// Dropping a column from sessions requires recreating the table because
	// SQLite < 3.35 does not support ALTER TABLE ... DROP COLUMN and because
	// the column may have been added via ALTER TABLE on an existing database
	// (in which case it has no FK references that would block a simple DROP
	// COLUMN even on newer SQLite).  We use the standard rename-copy-drop
	// strategy only when the outcome_summary column actually exists, to
	// preserve idempotency.
	//
	// The spawn_outcome table and its indexes use IF NOT EXISTS guards so
	// that on a fresh database (where the schema block above already created
	// them) this migration is a no-op.

	// Check whether sessions.outcome_summary exists.
	var osColExists int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'outcome_summary'`,
	).Scan(&osColExists); err != nil {
		return fmt.Errorf("db: migration v23→v24: check outcome_summary column: %w", err)
	}
	if osColExists > 0 {
		// Rebuild sessions without outcome_summary using the rename-copy-drop
		// idiom. Foreign keys are temporarily disabled during the rebuild to
		// allow rename without cascading FK errors.
		rebuildSteps := []string{
			`PRAGMA foreign_keys = OFF`,
			`ALTER TABLE sessions RENAME TO sessions_old_v24`,
			`CREATE TABLE sessions (
			  instance_id         TEXT PRIMARY KEY,
			  session_name        TEXT NOT NULL,
			  agent_role          TEXT,
			  root_agent_name     TEXT,
			  repo                TEXT NOT NULL,
			  worktree            TEXT NOT NULL,
			  harness             TEXT NOT NULL,
			  harness_session_id  TEXT,
			  group_id            TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
			  started_at          INTEGER NOT NULL,
			  ended_at            INTEGER,
			  end_state           TEXT,
			  archive_path        TEXT,
			  prism_version       TEXT
			)`,
			`INSERT INTO sessions SELECT instance_id, session_name, agent_role,
			  root_agent_name, repo, worktree, harness, harness_session_id,
			  group_id, started_at, ended_at, end_state, archive_path, prism_version
			  FROM sessions_old_v24`,
			`DROP TABLE sessions_old_v24`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_repo_started ON sessions(repo, started_at DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_name         ON sessions(session_name, started_at DESC)`,
			`PRAGMA foreign_keys = ON`,
		}
		for _, step := range rebuildSteps {
			if _, err := conn.Exec(step); err != nil {
				return fmt.Errorf("db: migration v23→v24: rebuild sessions: %w", err)
			}
		}
	}
	// Add spawn_outcome table and indexes (idempotent: IF NOT EXISTS).
	spawnOutcomeSteps := []string{
		`CREATE TABLE IF NOT EXISTS spawn_outcome (
		    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,
		    end_state              TEXT,
		    exit_code              INTEGER,
		    duration_ms            INTEGER,
		    interrupted_count      INTEGER NOT NULL DEFAULT 0,
		    compaction_count       INTEGER NOT NULL DEFAULT 0,
		    error_event_count      INTEGER NOT NULL DEFAULT 0,
		    permission_ask_count   INTEGER NOT NULL DEFAULT 0,
		    permission_denied_count INTEGER NOT NULL DEFAULT 0,
		    doom_loop_count        INTEGER NOT NULL DEFAULT 0,
		    pr_number              INTEGER,
		    pr_merged_at           INTEGER,
		    review_group_id        TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL,
		    review_verdict         TEXT,
		    review_pass_count      INTEGER,
		    review_fail_count      INTEGER,
		    review_none_count      INTEGER,
		    rubric_verdict         TEXT,
		    rubric_score           REAL,
		    rubric_breakdown       TEXT,
		    rubric_grader          TEXT,
		    tokens_input_total       INTEGER NOT NULL DEFAULT 0,
		    tokens_output_total      INTEGER NOT NULL DEFAULT 0,
		    tokens_cache_read_total  INTEGER NOT NULL DEFAULT 0,
		    tokens_cache_write_total INTEGER NOT NULL DEFAULT 0,
		    cost_usd_total           REAL    NOT NULL DEFAULT 0,
		    tool_call_count          INTEGER NOT NULL DEFAULT 0,
		    tool_error_count         INTEGER NOT NULL DEFAULT 0,
		    msg_assistant_count      INTEGER NOT NULL DEFAULT 0,
		    time_to_first_event_ms   INTEGER,
		    time_to_finished_ms      INTEGER,
		    computed_at            INTEGER NOT NULL,
		    schema_version         INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE INDEX IF NOT EXISTS idx_spawn_outcome_end_state    ON spawn_outcome(end_state)`,
		`CREATE INDEX IF NOT EXISTS idx_spawn_outcome_pr_number    ON spawn_outcome(pr_number)`,
		`CREATE INDEX IF NOT EXISTS idx_spawn_outcome_review_group ON spawn_outcome(review_group_id)`,
		`UPDATE schema_version SET version = 24`,
	}
	for _, step := range spawnOutcomeSteps {
		if _, err := conn.Exec(step); err != nil {
			return fmt.Errorf("db: migration v23→v24: spawn_outcome: %w", err)
		}
	}
	*version = 24
	return nil
}

func migrateV24toV25(conn *sql.DB, version *int) error {
	if *version != 24 {
		return nil
	}
	// Migration v24 → v25: add the spawn_inputs table (C.1/C.4 §4.1).
	// The table holds the intent of a spawn — every flag value the user
	// passed — keyed on instance_id (FK → sessions). Also includes
	// agent_prompt_hash (C4.AP) and skills_manifest_hash (C4.SK) columns.
	// CREATE TABLE IF NOT EXISTS and CREATE INDEX IF NOT EXISTS make the
	// migration idempotent; fresh databases already have the table from
	// the declarative schema block above, so these are no-ops there.
	spawnInputsSteps := []string{
		`CREATE TABLE IF NOT EXISTS spawn_inputs (
		    instance_id TEXT PRIMARY KEY REFERENCES sessions(instance_id) ON DELETE CASCADE,
		    profile_name           TEXT,
		    model_flag             TEXT,
		    variant_flag           TEXT,
		    agent_flag             TEXT,
		    harness_flag           TEXT,
		    isolation_flag         TEXT,
		    host_mode_flag         INTEGER NOT NULL DEFAULT 0,
		    pr_number              INTEGER,
		    branch_flag            TEXT,
		    ignore_concurrency_cap INTEGER NOT NULL DEFAULT 0,
		    model_variant_overrides TEXT,
		    skills_manifest_hash    TEXT,
		    prompt_template_hash    TEXT,
		    agent_prompt_hash       TEXT,
		    prompt_text            TEXT,
		    prompt_source          TEXT,
		    extras                 TEXT,
		    created_at             INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_spawn_inputs_profile         ON spawn_inputs(profile_name)`,
		`CREATE INDEX IF NOT EXISTS idx_spawn_inputs_harness_profile ON spawn_inputs(harness_flag, profile_name)`,
		`UPDATE schema_version SET version = 25`,
	}
	for _, step := range spawnInputsSteps {
		if _, err := conn.Exec(step); err != nil {
			return fmt.Errorf("db: migration v24→v25: spawn_inputs: %w", err)
		}
	}
	*version = 25
	return nil
}

func migrateV25toV26(conn *sql.DB, version *int) error {
	if *version != 25 {
		return nil
	}
	// Migration v25 → v26: drop host_mode column from agent_status.
	// All rows already have isolation_mode set (guaranteed by v22→v23
	// backfill, #1129), so host_mode is redundant. We use the
	// rename-copy-drop idiom because SQLite does not support
	// ALTER TABLE ... DROP COLUMN portably. The migration is conditional
	// on the column still existing, making it idempotent. (#1137)
	var hmColExists int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_status') WHERE name = 'host_mode'`,
	).Scan(&hmColExists); err != nil {
		return fmt.Errorf("db: migration v25→v26: check host_mode column: %w", err)
	}
	if hmColExists > 0 {
		// Wrap the rename-copy-drop in a transaction so the intermediate
		// state is never visible to concurrent readers. PRAGMA foreign_keys
		// must be set outside the transaction (SQLite requirement).
		if _, err := conn.Exec("PRAGMA foreign_keys = OFF"); err != nil {
			return fmt.Errorf("db: migration v25→v26: disable FK: %w", err)
		}
		if err := func() error {
			tx, err := conn.Begin()
			if err != nil {
				return fmt.Errorf("begin tx: %w", err)
			}
			defer tx.Rollback() //nolint:errcheck
			steps := []string{
				`ALTER TABLE agent_status RENAME TO agent_status_old_v25`,
				`CREATE TABLE agent_status (
				  session_name      TEXT PRIMARY KEY,
				  repo              TEXT NOT NULL,
				  worktree          TEXT NOT NULL,
				  state             TEXT NOT NULL,
				  title             TEXT,
				  agent_name        TEXT,
				  model_id          TEXT,
				  root_agent_name   TEXT,
				  root_model_id     TEXT,
				  isolation_mode    TEXT,
				  instance_id       TEXT,
				  last_seen         INTEGER NOT NULL,
				  ended_at          INTEGER,
				  harness           TEXT NOT NULL DEFAULT 'opencode',
				  harness_session_id TEXT,
				  harness_port      INTEGER,
				  group_id          TEXT REFERENCES session_groups(group_id) ON DELETE SET NULL
				)`,
				`INSERT INTO agent_status
				  SELECT session_name, repo, worktree, state, title,
				         agent_name, model_id, root_agent_name, root_model_id,
				         COALESCE(isolation_mode, 'podman'),
				         instance_id, last_seen, ended_at,
				         harness, harness_session_id, harness_port, group_id
				  FROM agent_status_old_v25`,
				`DROP TABLE agent_status_old_v25`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_active_coordinator_per_repo
				   ON agent_status (repo)
				   WHERE root_agent_name = 'coordinator' AND ended_at IS NULL`,
			}
			for _, s := range steps {
				if _, err := tx.Exec(s); err != nil {
					return fmt.Errorf("step %q: %w", s[:min(40, len(s))], err)
				}
			}
			return tx.Commit()
		}(); err != nil {
			return fmt.Errorf("db: migration v25→v26: rebuild agent_status: %w", err)
		}
		if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
			return fmt.Errorf("db: migration v25→v26: re-enable FK: %w", err)
		}
	}
	if _, err := conn.Exec("UPDATE schema_version SET version = 26"); err != nil {
		return fmt.Errorf("db: migration v25→v26: bump version: %w", err)
	}
	*version = 26
	return nil
}

func migrateV26toV27(conn *sql.DB, version *int) error {
	if *version != 26 {
		return nil
	}
	// Migration v26 → v27: introduce the harness_frames table for the PI
	// raw JSONL frame archive (P5.LOGS / #1218). The table stores every
	// inbound (extension→sidecar) and outbound (sidecar→extension) frame
	// for socket-pipe sessions, keyed by session_name and created_at, so
	// `prism logs --harness-events <session>` can replay the wire-protocol
	// stream for debugging without grepping the sidecar log.
	//
	// Direction is one of 'in' (extension→sidecar) or 'out' (sidecar→
	// extension). type is the JSON "type" field of the frame, denormalised
	// so --types filtering can be a simple WHERE clause without parsing
	// payload JSON. Payload is the raw JSONL bytes (excluding the trailing
	// newline) so consumers can pipe the output of `prism logs --harness-events`
	// directly into a JSONL parser.
	//
	// CREATE TABLE IF NOT EXISTS and CREATE INDEX IF NOT EXISTS make this
	// migration idempotent. Fresh databases already have the table from the
	// declarative schema block, so the CREATE statements are no-ops there.
	//
	// Rollback: DROP TABLE harness_frames. The table holds no data referenced
	// by other tables (the optional instance_id column has no FK to keep frame
	// writes cheap and to allow legacy frames whose instance_id is NULL), so a
	// drop is non-destructive to the rest of the schema.
	steps := []string{
		`CREATE TABLE IF NOT EXISTS harness_frames (
		  id            TEXT PRIMARY KEY,
		  session_name  TEXT NOT NULL,
		  instance_id   TEXT,
		  direction     TEXT NOT NULL,
		  type          TEXT,
		  payload       TEXT NOT NULL,
		  created_at    INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_harness_frames_session ON harness_frames(session_name, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_harness_frames_session_dir ON harness_frames(session_name, direction, created_at)`,
		`UPDATE schema_version SET version = 27`,
	}
	for _, s := range steps {
		if _, err := conn.Exec(s); err != nil {
			return fmt.Errorf("db: migration v26→v27: %w", err)
		}
	}
	*version = 27
	return nil
}

// migrateV27ToV28 adds the abtest_pair_id column to spawn_inputs (P4.ABTEST,
// issue #1216). The column is nullable TEXT; NULL means the session is not
// part of an A/B test pair. A partial index on the non-NULL values allows
// efficient lookup of both sessions in a pair.
// The ALTER TABLE is guarded by a pragma_table_info check so the migration
// is idempotent on fresh databases where the base schema already includes the
// column.
func migrateV27ToV28(conn *sql.DB, version *int) error {
	if *version >= 28 {
		return nil
	}
	// Only ALTER TABLE when the column does not yet exist (idempotent).
	var colExists int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('spawn_inputs') WHERE name = 'abtest_pair_id'`,
	).Scan(&colExists); err != nil {
		return fmt.Errorf("db: migration v27→v28: check abtest_pair_id column: %w", err)
	}
	if colExists == 0 {
		if _, err := conn.Exec(`ALTER TABLE spawn_inputs ADD COLUMN abtest_pair_id TEXT`); err != nil {
			return fmt.Errorf("db: migration v27→v28: add abtest_pair_id column: %w", err)
		}
	}
	steps := []string{
		`CREATE INDEX IF NOT EXISTS idx_spawn_inputs_abtest_pair ON spawn_inputs(abtest_pair_id) WHERE abtest_pair_id IS NOT NULL`,
		`UPDATE schema_version SET version = 28`,
	}
	for _, s := range steps {
		if _, err := conn.Exec(s); err != nil {
			return fmt.Errorf("db: migration v27→v28: %w", err)
		}
	}
	*version = 28
	return nil
}

// Close closes the underlying database connection.
func (d *DB) Close() error {
	return d.conn.Close()
}

// SpawnInputs holds the values inserted into spawn_inputs at spawn time.
// All pointer fields are nullable in the DB; nil means NULL.
type SpawnInputs struct {
	InstanceID string

	// Flag values as the user passed them (nil = flag not passed).
	ProfileName   *string
	ModelFlag     *string
	VariantFlag   *string
	AgentFlag     *string
	HarnessFlag   *string
	IsolationFlag *string
	HostModeFlag  bool
	PRNumber      *int
	BranchFlag    *string

	IgnoreConcurrencyCap bool

	// C.2 hook: per-role model-variant overrides (JSON).
	ModelVariantOverrides *string

	// C4.SK: skills directory hash at spawn time.
	SkillsManifestHash *string

	// C4.PT: reserved for prompt-template hash (not yet populated).
	PromptTemplateHash *string

	// C4.AP: agent role file hash at spawn time.
	AgentPromptHash *string

	// Prompt delivered to the harness.
	PromptText   *string
	PromptSource *string

	// P4.ABTEST: UUID shared between the two sessions in an --abtest pair.
	// Nil for non-abtest sessions.
	AbtestPairID *string

	// Free-form JSON blob for forward-compat.
	Extras *string

	// ms epoch, mirrors sessions.started_at.
	CreatedAt int64
}

// InsertSpawnInputs writes a row to spawn_inputs for the given instance.
// The function is idempotent via INSERT OR IGNORE — a second call with the
// same instance_id is a no-op (spawn is a one-shot event; the row, once
// written, is immutable). A missing FK (sessions row not yet committed) will
// cause the insert to fail with a foreign-key error; callers must ensure the
// sessions row exists first.
func (d *DB) InsertSpawnInputs(si SpawnInputs) error {
	_, err := d.conn.Exec(`
INSERT OR IGNORE INTO spawn_inputs (
    instance_id,
    profile_name, model_flag, variant_flag, agent_flag, harness_flag,
    isolation_flag, host_mode_flag,
    pr_number, branch_flag, ignore_concurrency_cap,
    model_variant_overrides,
    skills_manifest_hash, prompt_template_hash, agent_prompt_hash,
    prompt_text, prompt_source, abtest_pair_id, extras,
    created_at
) VALUES (
    ?,
    ?, ?, ?, ?, ?,
    ?, ?,
    ?, ?, ?,
    ?,
    ?, ?, ?,
    ?, ?, ?, ?,
    ?
)`,
		si.InstanceID,
		si.ProfileName, si.ModelFlag, si.VariantFlag, si.AgentFlag, si.HarnessFlag,
		si.IsolationFlag, boolToInt(si.HostModeFlag),
		si.PRNumber, si.BranchFlag, boolToInt(si.IgnoreConcurrencyCap),
		si.ModelVariantOverrides,
		si.SkillsManifestHash, si.PromptTemplateHash, si.AgentPromptHash,
		si.PromptText, si.PromptSource, si.AbtestPairID, si.Extras,
		si.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("db: insert spawn_inputs for %q: %w", si.InstanceID, err)
	}
	return nil
}

// SpawnInputsByInstanceID returns the spawn_inputs row for the given
// instance_id, or nil when not found.
func (d *DB) SpawnInputsByInstanceID(instanceID string) (*SpawnInputs, error) {
	const q = `
SELECT
    instance_id,
    profile_name, model_flag, variant_flag, agent_flag, harness_flag,
    isolation_flag, host_mode_flag,
    pr_number, branch_flag, ignore_concurrency_cap,
    model_variant_overrides,
    skills_manifest_hash, prompt_template_hash, agent_prompt_hash,
    prompt_text, prompt_source, abtest_pair_id, extras,
    created_at
FROM spawn_inputs
WHERE instance_id = ?`
	row := d.conn.QueryRow(q, instanceID)
	var si SpawnInputs
	var profileName, modelFlag, variantFlag, agentFlag, harnessFlag, isolationFlag sql.NullString
	var prNumber sql.NullInt64
	var branchFlag, modelVariantOverrides, skillsManifestHash, promptTemplateHash, agentPromptHash sql.NullString
	var promptText, promptSource, abtestPairID, extras sql.NullString
	var hostModeFlag, ignoreConcurrencyCap int
	err := row.Scan(
		&si.InstanceID,
		&profileName, &modelFlag, &variantFlag, &agentFlag, &harnessFlag,
		&isolationFlag, &hostModeFlag,
		&prNumber, &branchFlag, &ignoreConcurrencyCap,
		&modelVariantOverrides,
		&skillsManifestHash, &promptTemplateHash, &agentPromptHash,
		&promptText, &promptSource, &abtestPairID, &extras,
		&si.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: spawn_inputs by instance_id %q: %w", instanceID, err)
	}
	if profileName.Valid {
		si.ProfileName = &profileName.String
	}
	if modelFlag.Valid {
		si.ModelFlag = &modelFlag.String
	}
	if variantFlag.Valid {
		si.VariantFlag = &variantFlag.String
	}
	if agentFlag.Valid {
		si.AgentFlag = &agentFlag.String
	}
	if harnessFlag.Valid {
		si.HarnessFlag = &harnessFlag.String
	}
	if isolationFlag.Valid {
		si.IsolationFlag = &isolationFlag.String
	}
	si.HostModeFlag = hostModeFlag != 0
	if prNumber.Valid {
		n := int(prNumber.Int64)
		si.PRNumber = &n
	}
	if branchFlag.Valid {
		si.BranchFlag = &branchFlag.String
	}
	si.IgnoreConcurrencyCap = ignoreConcurrencyCap != 0
	if modelVariantOverrides.Valid {
		si.ModelVariantOverrides = &modelVariantOverrides.String
	}
	if skillsManifestHash.Valid {
		si.SkillsManifestHash = &skillsManifestHash.String
	}
	if promptTemplateHash.Valid {
		si.PromptTemplateHash = &promptTemplateHash.String
	}
	if agentPromptHash.Valid {
		si.AgentPromptHash = &agentPromptHash.String
	}
	if promptText.Valid {
		si.PromptText = &promptText.String
	}
	if promptSource.Valid {
		si.PromptSource = &promptSource.String
	}
	if abtestPairID.Valid {
		si.AbtestPairID = &abtestPairID.String
	}
	if extras.Valid {
		si.Extras = &extras.String
	}
	return &si, nil
}

// boolToInt converts a bool to 0/1 for SQLite INTEGER columns.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
