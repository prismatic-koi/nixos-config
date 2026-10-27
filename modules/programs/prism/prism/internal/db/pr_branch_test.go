package db_test

// Tests for pr_branch_sessions (#3287): the v46→v47 migration that adds the
// table, and MarkPRBranchSession / ClearPRBranchSession / PRBranchSessions.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestOpen_V46Database_GetsPRBranchSessions: a database from a binary
// before #3287 opens at v47 with a usable table.
func TestOpen_V46Database_GetsPRBranchSessions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v46.db")
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open v46 db: %v", err)
	}
	if _, err := rawConn.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (46);
	`); err != nil {
		t.Fatalf("seed v46 db: %v", err)
	}
	rawConn.Close()

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()
	if v := schemaVersionOf(t, d); v < 47 {
		t.Errorf("schema_version after migration: got %d, want >= 47", v)
	}
	if err := d.MarkPRBranchSession("prism-test@pr", "/tmp/pr", "12"); err != nil {
		t.Fatalf("MarkPRBranchSession on a migrated db: %v", err)
	}
}

func openPRBranchTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "pr-branch.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestPRBranchSessions_Roundtrip(t *testing.T) {
	d := openPRBranchTestDB(t)

	if got, err := d.PRBranchSessions(); err != nil || len(got) != 0 {
		t.Fatalf("empty table: PRBranchSessions = (%v, %v), want (none, nil)", got, err)
	}
	if err := d.MarkPRBranchSession("prism-test@pr", "/w/pr", "12"); err != nil {
		t.Fatalf("MarkPRBranchSession: %v", err)
	}
	if err := d.MarkPRBranchSession("prism-test@other", "/w/other", "https://github.com/o/r/pull/13"); err != nil {
		t.Fatalf("MarkPRBranchSession: %v", err)
	}
	// A second mark of the same name replaces the first.
	if err := d.MarkPRBranchSession("prism-test@pr", "/w/pr2", "14"); err != nil {
		t.Fatalf("MarkPRBranchSession (again): %v", err)
	}
	got, err := d.PRBranchSessions()
	if err != nil {
		t.Fatalf("PRBranchSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("PRBranchSessions = %+v, want 2 rows", got)
	}
	if got[0].SessionName != "prism-test@other" || got[1].SessionName != "prism-test@pr" ||
		got[1].Worktree != "/w/pr2" || got[1].PR != "14" || got[1].CreatedAt == 0 {
		t.Errorf("PRBranchSessions = %+v", got)
	}

	if err := d.ClearPRBranchSession("prism-test@pr"); err != nil {
		t.Fatalf("ClearPRBranchSession: %v", err)
	}
	if err := d.ClearPRBranchSession("prism-test@no-record"); err != nil {
		t.Fatalf("ClearPRBranchSession of a name with no record: %v", err)
	}
	got, err = d.PRBranchSessions()
	if err != nil {
		t.Fatalf("PRBranchSessions: %v", err)
	}
	if len(got) != 1 || got[0].SessionName != "prism-test@other" {
		t.Errorf("after clear: PRBranchSessions = %+v, want only prism-test@other", got)
	}
}

// TestMarkPRBranchSession_RequiresEveryField: a record with no worktree or
// no PR cannot match the decision, so the write fails and the spawn stops.
func TestMarkPRBranchSession_RequiresEveryField(t *testing.T) {
	d := openPRBranchTestDB(t)
	for _, tc := range [][3]string{
		{"", "/w/pr", "12"},
		{"prism-test@pr", "", "12"},
		{"prism-test@pr", "/w/pr", ""},
	} {
		if err := d.MarkPRBranchSession(tc[0], tc[1], tc[2]); err == nil {
			t.Errorf("MarkPRBranchSession(%q, %q, %q) = nil, want an error", tc[0], tc[1], tc[2])
		}
	}
}

// TestPrune_KeepsPRBranchSessions: Prune does not remove a record. A record
// that Prune removes gives a long-lived PR session --approve at the next
// restore.
func TestPrune_KeepsPRBranchSessions(t *testing.T) {
	d := openPRBranchTestDB(t)
	if err := d.MarkPRBranchSession("prism-test@pr", "/w/pr", "12"); err != nil {
		t.Fatalf("MarkPRBranchSession: %v", err)
	}
	if err := d.Prune(0); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got, err := d.PRBranchSessions(); err != nil || len(got) != 1 {
		t.Errorf("after Prune: PRBranchSessions = (%+v, %v), want the record", got, err)
	}
}
