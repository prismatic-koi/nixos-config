package db_test

// Tests for the v43→v44 migration that adds spawn_outcome.aggregated_at and
// backfills it for every pre-existing row that HasComputedAggregates() reports
// true for. The column marks a row that WriteSpawnOutcome has filled; a row
// that only a partial writer (pr_number, pr_merged_at, review result) created
// keeps it NULL.
//
// The backfill is the reason this migration differs from the closed PR #2934,
// which shipped the same column with no backfill. Without a backfill a
// pre-migration row reads as un-aggregated and is recomputed; for a session
// whose agent_events have passed the 90-day prune the recomputation returns
// zeros and discards the real historical totals (issue #2936).
//
// The seed starts at v43, so only migrateV43ToV44 has work to do. Every
// other table is created by db.Open's declarative schema block; the seed
// supplies only schema_version and the pre-v44 spawn_outcome shape, because
// `CREATE TABLE IF NOT EXISTS` leaves an existing spawn_outcome alone and
// the migration must then be the thing that adds the column.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedV43DB creates a v43 DB whose spawn_outcome table lacks aggregated_at.
// When withAggregatedAt is true the column is already present, which
// simulates a database whose declarative schema block created it — the
// migration's pragma guard must detect that and skip the ALTER TABLE.
func seedV43DB(t *testing.T, dbPath string, withAggregatedAt bool) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v43 db: %v", err)
	}
	defer rawConn.Close()

	aggregatedCol := ""
	if withAggregatedAt {
		aggregatedCol = ", aggregated_at INTEGER"
	}

	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
		    instance_id TEXT PRIMARY KEY,
		    session_name TEXT NOT NULL, agent_role TEXT, root_agent_name TEXT,
		    repo TEXT NOT NULL, worktree TEXT NOT NULL, harness TEXT NOT NULL DEFAULT 'pi',
		    harness_session_id TEXT, group_id TEXT,
		    started_at INTEGER NOT NULL, ended_at INTEGER, end_state TEXT,
		    archive_path TEXT, prism_version TEXT, parent_session TEXT
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
		    review_group_id TEXT, review_verdict TEXT,
		    review_pass_count INTEGER, review_fail_count INTEGER, review_none_count INTEGER,
		    rubric_verdict TEXT, rubric_score REAL, rubric_breakdown TEXT, rubric_grader TEXT,
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
		    schema_version INTEGER NOT NULL DEFAULT 1` + aggregatedCol + `
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (43);
	`)
	if err != nil {
		t.Fatalf("seed v43 db: %v", err)
	}
}

// aggregatedAtColumnCount returns the pragma_table_info count for
// spawn_outcome.aggregated_at.
func aggregatedAtColumnCount(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('spawn_outcome') WHERE name = 'aggregated_at'`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info aggregated_at: %v", err)
	}
	return n
}

// seedPreMigrationSpawnOutcome inserts a sessions row and a spawn_outcome row
// into a v43 (pre-migration) DB via a raw connection. tokensOut and durationMs
// are the only aggregate-block columns varied by the callers; a NULL
// durationMs is passed as -1.
func seedPreMigrationSpawnOutcome(t *testing.T, dbPath, iid string, tokensOut int64, durationMs int64, prNumber int) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer rawConn.Close()
	if _, err := rawConn.Exec(
		`INSERT INTO sessions (instance_id, session_name, repo, worktree, started_at) VALUES (?, ?, ?, ?, ?)`,
		iid, "repo@"+iid, "repo", "/wt/"+iid, 1700000000000,
	); err != nil {
		t.Fatalf("seed sessions row %s: %v", iid, err)
	}
	var dur any
	if durationMs >= 0 {
		dur = durationMs
	}
	var pr any
	if prNumber > 0 {
		pr = prNumber
	}
	if _, err := rawConn.Exec(
		`INSERT INTO spawn_outcome (instance_id, tokens_output_total, duration_ms, pr_number, computed_at)
		 VALUES (?, ?, ?, ?, ?)`,
		iid, tokensOut, dur, pr, 1700000001000,
	); err != nil {
		t.Fatalf("seed spawn_outcome row %s: %v", iid, err)
	}
}

// TestMigration_V43ToV44_BodyRuns_AddsAggregatedAt exercises the body-runs
// branch: a v43 DB without spawn_outcome.aggregated_at. The pragma guard
// returns 0 and the ALTER TABLE ADD COLUMN executes.
func TestMigration_V43ToV44_BodyRuns_AddsAggregatedAt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v43_body_runs.db")
	seedV43DB(t, dbPath, false /*withAggregatedAt*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := schemaVersionOf(t, d); v < 44 {
		t.Errorf("schema_version after migration: got %d, want >= 44", v)
	}
	if n := aggregatedAtColumnCount(t, d); n != 1 {
		t.Errorf("spawn_outcome.aggregated_at missing after v43→v44 (body-runs branch): got %d, want 1", n)
	}
}

// TestMigration_V43ToV44_BodySkips_PreExistingColumn exercises the body-skips
// branch: a v43 DB that already has the column. The pragma guard returns 1,
// the ALTER TABLE is skipped, and the end state is still v44 with exactly one
// aggregated_at column.
func TestMigration_V43ToV44_BodySkips_PreExistingColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v43_body_skips.db")
	seedV43DB(t, dbPath, true /*withAggregatedAt*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := schemaVersionOf(t, d); v < 44 {
		t.Errorf("schema_version after migration: got %d, want >= 44", v)
	}
	if n := aggregatedAtColumnCount(t, d); n != 1 {
		t.Errorf("spawn_outcome.aggregated_at count after v43→v44 (body-skips branch): got %d, want 1", n)
	}
}

// TestMigration_V43ToV44_Idempotent verifies that re-opening a DB already at
// v44 does not error and the column remains exactly once.
func TestMigration_V43ToV44_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v43_idem.db")
	seedV43DB(t, dbPath, false)

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

	if v := schemaVersionOf(t, d2); v < 44 {
		t.Errorf("schema_version after second open: got %d, want >= 44", v)
	}
	if n := aggregatedAtColumnCount(t, d2); n != 1 {
		t.Errorf("spawn_outcome.aggregated_at count after idempotent open: got %d, want 1", n)
	}
}

// TestMigration_V43ToV44_BackfillsComputedRow_PreservesPrunedValues covers the
// central AC: a pre-migration row that HasComputedAggregates() reports true
// for is backfilled (aggregated_at set to computed_at) and every persisted
// value survives — including the case where the session's agent_events have
// been pruned, so a recompute would return zeros.
func TestMigration_V43ToV44_BackfillsComputedRow_PreservesPrunedValues(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v43_backfill_pruned.db")
	seedV43DB(t, dbPath, false)
	const iid = "pruned-aggregate"
	// A full-aggregate row with no agent_events at all (pruned): tokens and
	// duration are the only surviving record of the run cost.
	seedPreMigrationSpawnOutcome(t, dbPath, iid, 4242, 1_680_000, 0)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: out=%v err=%v", out, err)
	}
	if out.AggregatedAt == nil {
		t.Fatal("AggregatedAt: nil on a backfilled aggregate row, want set")
	}
	if *out.AggregatedAt != out.ComputedAt {
		t.Errorf("AggregatedAt = %d, want computed_at = %d (the backfill stamp)", *out.AggregatedAt, out.ComputedAt)
	}
	if out.TokensOutputTotal != 4242 {
		t.Errorf("TokensOutputTotal after migration: got %d, want 4242 (persisted totals must survive)", out.TokensOutputTotal)
	}
	if out.DurationMs == nil || *out.DurationMs != 1_680_000 {
		t.Errorf("DurationMs after migration: got %v, want 1680000", out.DurationMs)
	}
}

// TestMigration_V43ToV44_LeavesStubRowNull pins the other side of the backfill:
// a partial-writer stub (only pr_number set, every aggregate column a default)
// reports HasComputedAggregates() false, so the migration leaves its
// aggregated_at NULL.
func TestMigration_V43ToV44_LeavesStubRowNull(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v43_backfill_stub.db")
	seedV43DB(t, dbPath, false)
	const iid = "pr-stub"
	// A pr_number stub: no tokens, no duration.
	seedPreMigrationSpawnOutcome(t, dbPath, iid, 0, -1 /*NULL duration*/, 2934)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	out, err := d.SpawnOutcomeByInstanceID(iid)
	if err != nil || out == nil {
		t.Fatalf("SpawnOutcomeByInstanceID: out=%v err=%v", out, err)
	}
	if out.AggregatedAt != nil {
		t.Errorf("AggregatedAt on a stub row: got %d, want nil", *out.AggregatedAt)
	}
	if out.PRNumber == nil || *out.PRNumber != 2934 {
		t.Errorf("PRNumber: got %v, want 2934 (the stub's own column must survive)", out.PRNumber)
	}
}

// TestMigration_V43ToV44_BackfillMatchesHasComputedAggregates asserts the SQL
// backfill predicate agrees with the Go HasComputedAggregates predicate on a
// range of rows: a duration-only row (a zero-event cleanup row) is backfilled,
// a cost-only row is backfilled, and a bare stub is not.
func TestMigration_V43ToV44_BackfillMatchesHasComputedAggregates(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v43_backfill_predicate.db")
	seedV43DB(t, dbPath, false)
	// duration-only: a terminal session that produced no token events. The
	// cleanup writer still set duration_ms, so HasComputedAggregates is true.
	seedPreMigrationSpawnOutcome(t, dbPath, "duration-only", 0, 900_000, 0)
	// bare stub: no aggregate column set at all.
	seedPreMigrationSpawnOutcome(t, dbPath, "bare-stub", 0, -1, 0)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	durOnly, _ := d.SpawnOutcomeByInstanceID("duration-only")
	if durOnly == nil || durOnly.AggregatedAt == nil {
		t.Errorf("duration-only row: AggregatedAt=%v, want set (HasComputedAggregates=%v)",
			durOnly, durOnly.HasComputedAggregates())
	}
	bare, _ := d.SpawnOutcomeByInstanceID("bare-stub")
	if bare == nil || bare.AggregatedAt != nil {
		t.Errorf("bare-stub row: AggregatedAt=%v, want nil (HasComputedAggregates=%v)",
			bare, bare.HasComputedAggregates())
	}
}
