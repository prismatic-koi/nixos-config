package db

// schema-version-guard_test.go — tests for the "schema version too new" guard
// introduced in Open/runMigrations (#1869).
//
// These tests live in the internal `package db` so they can access the
// unexported currentSchemaVersion constant.

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// seedRawDBWithVersion creates a minimal but valid SQLite database at dbPath
// with schema_version set to the given value. All tables required by
// runMigrations are created so that Open's applySchema step is a no-op
// (CREATE TABLE IF NOT EXISTS) and seedSchemaVersionIfEmpty skips insertion
// (count > 0). This lets runMigrations reach the version guard.
func seedRawDBWithVersion(t *testing.T, dbPath string, version int) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("seedRawDBWithVersion: open: %v", err)
	}
	defer rawConn.Close()

	_, err = rawConn.Exec(`
		CREATE TABLE IF NOT EXISTS agent_events (
		  id                 TEXT PRIMARY KEY,
		  session_name       TEXT NOT NULL,
		  repo               TEXT NOT NULL,
		  worktree           TEXT NOT NULL,
		  harness_session_id TEXT,
		  type               TEXT NOT NULL,
		  payload            TEXT NOT NULL,
		  created_at         INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS agent_status (
		  session_name TEXT PRIMARY KEY,
		  repo         TEXT NOT NULL,
		  worktree     TEXT NOT NULL,
		  state        TEXT NOT NULL,
		  last_seen    INTEGER NOT NULL,
		  ended_at     INTEGER
		);
		CREATE TABLE IF NOT EXISTS bus_messages (
		  id           TEXT PRIMARY KEY,
		  from_session TEXT NOT NULL,
		  to_session   TEXT NOT NULL,
		  repo         TEXT NOT NULL,
		  text         TEXT NOT NULL,
		  urgency      TEXT NOT NULL DEFAULT 'normal',
		  sent_at      INTEGER NOT NULL,
		  delivered_at INTEGER
		);
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
	`)
	if err != nil {
		t.Fatalf("seedRawDBWithVersion: create tables: %v", err)
	}

	_, err = rawConn.Exec(`INSERT INTO schema_version (version) VALUES (?)`, version)
	if err != nil {
		t.Fatalf("seedRawDBWithVersion: insert version %d: %v", version, err)
	}
}

// TestOpen_SchemaVersionTooNew asserts that Open returns a non-nil error
// containing both the on-disk version and the binary's max when the DB's
// schema_version exceeds currentSchemaVersion.
func TestOpen_SchemaVersionTooNew(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "too-new.db")
	seedRawDBWithVersion(t, dbPath, 999)

	_, err := Open(dbPath)
	if err == nil {
		t.Fatal("Open: expected error for schema_version=999, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "999") {
		t.Errorf("error message must contain on-disk version 999: got %q", msg)
	}
	if !strings.Contains(msg, "newer than this prism binary") {
		t.Errorf("error message must say 'newer than this prism binary': got %q", msg)
	}
	// The message should also name the binary's max so the user knows what
	// version they need to upgrade to or roll back from.
	want := "upgrade prism, or restore"
	if !strings.Contains(msg, want) {
		t.Errorf("error message must contain %q: got %q", want, msg)
	}
}

// TestOpen_SchemaVersionAtMax asserts that Open succeeds normally when the
// on-disk schema_version equals currentSchemaVersion (no false positive).
// A freshly created DB is migrated to currentSchemaVersion on Open, so
// opening it a second time exercises the "already at max" path.
func TestOpen_SchemaVersionAtMax(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "at-max.db")

	// First Open: creates and migrates the DB to currentSchemaVersion.
	d1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	d1.Close()

	// Confirm the version is indeed currentSchemaVersion.
	rawConn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	var version int
	if err := rawConn.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		rawConn.Close()
		t.Fatalf("read schema_version: %v", err)
	}
	rawConn.Close()
	if version != currentSchemaVersion {
		t.Fatalf("schema_version after first Open: got %d, want %d", version, currentSchemaVersion)
	}

	// Second Open: DB is already at currentSchemaVersion — must succeed.
	d2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: unexpected error for schema_version=%d: %v", currentSchemaVersion, err)
	}
	d2.Close()
}

// TestCurrentSchemaVersion_MatchesMigrationCount asserts that currentSchemaVersion
// equals the count of migrateVNtoVN+1 functions in db.go plus one (because
// version numbering starts at 1 and the count of migrations equals
// currentSchemaVersion - 1). This ensures that adding a migration without
// bumping currentSchemaVersion fails CI.
func TestCurrentSchemaVersion_MatchesMigrationCount(t *testing.T) {
	// Locate db.go relative to this test file.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: could not determine source file location")
	}
	dbGoPath := filepath.Join(filepath.Dir(thisFile), "db.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, dbGoPath, nil, 0)
	if err != nil {
		t.Fatalf("parse db.go: %v", err)
	}

	var migrateCount int
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		// Match migrateV<N>to<M> (case-insensitive middle 'to'/'To')
		name := fn.Name.Name
		if strings.HasPrefix(name, "migrateV") && strings.Contains(strings.ToLower(name), "to") {
			migrateCount++
		}
	}

	// There should be exactly currentSchemaVersion - 1 migrations
	// (v1→v2, v2→v3, …, v(N-1)→vN).
	want := currentSchemaVersion - 1
	if migrateCount != want {
		t.Errorf(
			"currentSchemaVersion=%d but found %d migration functions in db.go (want %d = currentSchemaVersion-1); "+
				"did you add a migration without bumping currentSchemaVersion?",
			currentSchemaVersion, migrateCount, want,
		)
	}
}
