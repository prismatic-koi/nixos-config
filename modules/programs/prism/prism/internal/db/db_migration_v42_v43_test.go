package db_test

// Tests for the v42→v43 migration that adds spawn_inputs.provider_flag.
// The new column records the raw `prism spawn --provider`
// value, so an alternative routing provider is auditable alongside
// model_flag and variant_flag.
//
// The seed starts at v42, so only migrateV42ToV43 has work to do. Every
// other table is created by db.Open's declarative schema block; the seed
// supplies only schema_version and the pre-v43 spawn_inputs shape, because
// `CREATE TABLE IF NOT EXISTS` leaves an existing spawn_inputs alone and the
// migration must then be the thing that adds the column.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedV42DB creates a v42 DB whose spawn_inputs table lacks provider_flag.
// When withProviderFlag is true the column is already present, which
// simulates a database whose declarative schema block created it — the
// migration's pragma guard must detect that and skip the ALTER TABLE.
func seedV42DB(t *testing.T, dbPath string, withProviderFlag bool) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v42 db: %v", err)
	}
	defer rawConn.Close()

	providerCol := ""
	if withProviderFlag {
		providerCol = "provider_flag TEXT,"
	}

	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS spawn_inputs (
		    instance_id TEXT PRIMARY KEY,
		    profile_name TEXT, model_flag TEXT, variant_flag TEXT, agent_flag TEXT,
		    harness_flag TEXT, ` + providerCol + ` isolation_flag TEXT,
		    host_mode_flag INTEGER NOT NULL DEFAULT 0,
		    containers_flag INTEGER NOT NULL DEFAULT 0,
		    pr_number INTEGER, branch_flag TEXT,
		    ignore_concurrency_cap INTEGER NOT NULL DEFAULT 0,
		    isolation_mode TEXT,
		    model_variant_overrides TEXT, skills_manifest_hash TEXT,
		    prompt_template_hash TEXT, agent_prompt_hash TEXT,
		    prompt_text TEXT, prompt_source TEXT,
		    abtest_pair_id TEXT,
		    extras TEXT,
		    account_name TEXT,
		    created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (42);
	`)
	if err != nil {
		t.Fatalf("seed v42 db: %v", err)
	}
}

// providerFlagColumnCount returns the pragma_table_info count for
// spawn_inputs.provider_flag.
func providerFlagColumnCount(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('spawn_inputs') WHERE name = 'provider_flag'`,
	).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info provider_flag: %v", err)
	}
	return n
}

// schemaVersionOf reads the on-disk schema version.
func schemaVersionOf(t *testing.T, d *db.DB) int {
	t.Helper()
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	return version
}

// TestMigration_V42ToV43_BodyRuns_AddsProviderFlag exercises the body-runs
// branch: a v42 DB without spawn_inputs.provider_flag. The pragma guard
// returns 0 and the ALTER TABLE ADD COLUMN executes.
func TestMigration_V42ToV43_BodyRuns_AddsProviderFlag(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v42_body_runs.db")
	seedV42DB(t, dbPath, false /*withProviderFlag*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := schemaVersionOf(t, d); v < 43 {
		t.Errorf("schema_version after migration: got %d, want >= 43", v)
	}
	if n := providerFlagColumnCount(t, d); n != 1 {
		t.Errorf("spawn_inputs.provider_flag missing after v42→v43 (body-runs branch): got %d, want 1", n)
	}
}

// TestMigration_V42ToV43_BodySkips_PreExistingColumn exercises the body-skips
// branch: a v42 DB that already has the column. The pragma guard returns 1,
// the ALTER TABLE is skipped, and the end state is still v43 with exactly one
// provider_flag column.
func TestMigration_V42ToV43_BodySkips_PreExistingColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v42_body_skips.db")
	seedV42DB(t, dbPath, true /*withProviderFlag*/)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := schemaVersionOf(t, d); v < 43 {
		t.Errorf("schema_version after migration: got %d, want >= 43", v)
	}
	if n := providerFlagColumnCount(t, d); n != 1 {
		t.Errorf("spawn_inputs.provider_flag count after v42→v43 (body-skips branch): got %d, want 1", n)
	}
}

// TestMigration_V42ToV43_Idempotent verifies that re-opening a DB already at
// v43 does not error and the column remains exactly once.
func TestMigration_V42ToV43_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v42_idem.db")
	seedV42DB(t, dbPath, false)

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

	if v := schemaVersionOf(t, d2); v < 43 {
		t.Errorf("schema_version after second open: got %d, want >= 43", v)
	}
	if n := providerFlagColumnCount(t, d2); n != 1 {
		t.Errorf("spawn_inputs.provider_flag count after idempotent open: got %d, want 1", n)
	}
}

// TestMigration_V42ToV43_PreMigrationRowsKeepNullProvider pins the no-backfill
// policy: a row written before the migration reads back with a NULL
// provider_flag, which is the truthful value (the flag did not exist).
func TestMigration_V42ToV43_PreMigrationRowsKeepNullProvider(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v42_no_backfill.db")
	seedV42DB(t, dbPath, false)

	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := rawConn.Exec(
		`INSERT INTO spawn_inputs (instance_id, model_flag, created_at) VALUES (?, ?, ?)`,
		"legacy-instance", "anthropic/claude-opus-4-7", 1700000000000,
	); err != nil {
		rawConn.Close()
		t.Fatalf("seed pre-migration row: %v", err)
	}
	rawConn.Close()

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	var provider sql.NullString
	if err := d.QueryRow(
		`SELECT provider_flag FROM spawn_inputs WHERE instance_id = ?`, "legacy-instance",
	).Scan(&provider); err != nil {
		t.Fatalf("read provider_flag: %v", err)
	}
	if provider.Valid {
		t.Errorf("provider_flag on a pre-migration row: got %q, want NULL", provider.String)
	}
}
