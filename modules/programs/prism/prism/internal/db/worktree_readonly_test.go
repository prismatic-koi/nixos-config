package db_test

// Tests for agent_status.worktree_readonly (#3079): the v45→v46 migration
// that adds the column, and SetWorktreeReadOnly / WorktreeReadOnly.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedV45DB creates a v45 DB with one agent_status row and no
// worktree_readonly column.
func seedV45DB(t *testing.T, dbPath string) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v45 db: %v", err)
	}
	defer rawConn.Close()
	if _, err := rawConn.Exec(`
		CREATE TABLE agent_status (
		  session_name TEXT PRIMARY KEY, repo TEXT NOT NULL, worktree TEXT NOT NULL,
		  state TEXT NOT NULL, title TEXT, title_source TEXT, issue_ref TEXT,
		  agent_name TEXT, model_id TEXT, root_agent_name TEXT, root_model_id TEXT,
		  isolation_mode TEXT, instance_id TEXT, last_seen INTEGER NOT NULL,
		  ended_at INTEGER, harness TEXT NOT NULL DEFAULT 'pi', harness_session_id TEXT,
		  harness_port INTEGER, group_id TEXT, muted INTEGER NOT NULL DEFAULT 0,
		  containers_enabled INTEGER NOT NULL DEFAULT 0, pending_disagreement TEXT
		);
		INSERT INTO agent_status (session_name, repo, worktree, state, last_seen)
		  VALUES ('prism-test@pre-v46', 'prism-test', '/tmp/w', 'idle', 1);
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (45);
	`); err != nil {
		t.Fatalf("seed v45 db: %v", err)
	}
}

func TestMigration_V45ToV46_AddsWorktreeReadOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v45.db")
	seedV45DB(t, dbPath)

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	if v := schemaVersionOf(t, d); v < 46 {
		t.Errorf("schema_version after migration: got %d, want >= 46", v)
	}
	ro, err := d.WorktreeReadOnly("prism-test@pre-v46")
	if err != nil {
		t.Fatalf("WorktreeReadOnly on a migrated row: %v", err)
	}
	if ro {
		t.Error("a row that existed before the migration reads as read-only, want the default 0")
	}
}

func openWorktreeReadOnlyTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "worktree-readonly.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestWorktreeReadOnly_Roundtrip(t *testing.T) {
	d := openWorktreeReadOnlyTestDB(t)
	const session = "prism-test@worktree-readonly"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	if ro, err := d.WorktreeReadOnly(session); err != nil || ro {
		t.Fatalf("new row: WorktreeReadOnly = (%v, %v), want (false, nil)", ro, err)
	}
	if err := d.SetWorktreeReadOnly(session, true); err != nil {
		t.Fatalf("SetWorktreeReadOnly(true): %v", err)
	}
	if ro, err := d.WorktreeReadOnly(session); err != nil || !ro {
		t.Fatalf("after set true: WorktreeReadOnly = (%v, %v), want (true, nil)", ro, err)
	}
	if err := d.SetWorktreeReadOnly(session, false); err != nil {
		t.Fatalf("SetWorktreeReadOnly(false): %v", err)
	}
	if ro, err := d.WorktreeReadOnly(session); err != nil || ro {
		t.Fatalf("after set false: WorktreeReadOnly = (%v, %v), want (false, nil)", ro, err)
	}
}

// TestSetWorktreeReadOnly_MissingRowFails: SpawnSession must not go on with
// a session whose flag it could not record.
func TestSetWorktreeReadOnly_MissingRowFails(t *testing.T) {
	d := openWorktreeReadOnlyTestDB(t)
	if err := d.SetWorktreeReadOnly("prism-test@no-such-row", true); err == nil {
		t.Fatal("SetWorktreeReadOnly on a missing row returned nil, want an error")
	}
	if ro, err := d.WorktreeReadOnly("prism-test@no-such-row"); err != nil || ro {
		t.Fatalf("missing row: WorktreeReadOnly = (%v, %v), want (false, nil)", ro, err)
	}
}
