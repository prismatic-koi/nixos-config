package opencode_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/prismatic-koi/prism/internal/opencode"
)

// createTestDB creates a temporary SQLite database with the opencode session
// schema under the expected path layout: <baseDir>/opencode/opencode-stable.db.
// It returns the base directory (suitable for use as XDG_DATA_HOME) and the DB
// file path.
func createTestDB(t *testing.T) (xdgDataHome, dbFile string) {
	t.Helper()
	baseDir := t.TempDir()
	dbDir := filepath.Join(baseDir, "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir opencode dir: %v", err)
	}
	dbFile = filepath.Join(dbDir, "opencode-stable.db")

	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE session (
			id           TEXT PRIMARY KEY,
			directory    TEXT NOT NULL,
			time_updated INTEGER NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create session table: %v", err)
	}

	return baseDir, dbFile
}

// insertSession inserts a row into the session table.
func insertSession(t *testing.T, dbFile, id, directory string, timeUpdated int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("open db for insert: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(
		`INSERT INTO session (id, directory, time_updated) VALUES (?, ?, ?)`,
		id, directory, timeUpdated,
	)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

func TestLatestSessionForDir_ReturnsLatest(t *testing.T) {
	xdgHome, dbFile := createTestDB(t)
	dir := "/home/user/code/nixos-config/main"

	insertSession(t, dbFile, "ses_older", dir, 1000)
	insertSession(t, dbFile, "ses_newer", dir, 2000)

	t.Setenv("XDG_DATA_HOME", xdgHome)

	got := opencode.LatestSessionForDir(dir)
	if got != "ses_newer" {
		t.Errorf("LatestSessionForDir(%q) = %q, want %q", dir, got, "ses_newer")
	}
}

func TestLatestSessionForDir_UnknownDir(t *testing.T) {
	xdgHome, dbFile := createTestDB(t)
	insertSession(t, dbFile, "ses_abc", "/some/other/dir", 1000)

	t.Setenv("XDG_DATA_HOME", xdgHome)

	got := opencode.LatestSessionForDir("/does/not/exist")
	if got != "" {
		t.Errorf("LatestSessionForDir(unknown dir) = %q, want %q", got, "")
	}
}

func TestLatestSessionForDir_MissingDB(t *testing.T) {
	// Point XDG_DATA_HOME at an empty temp dir so no DB file exists.
	emptyDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", emptyDir)

	got := opencode.LatestSessionForDir("/home/user/code/nixos-config/main")
	if got != "" {
		t.Errorf("LatestSessionForDir(missing db) = %q, want %q", got, "")
	}
}

func TestLatestSessionForDir_XDGFallback(t *testing.T) {
	// Create a DB under the default ~/.local/share path by overriding HOME.
	homeDir := t.TempDir()
	dbDir := filepath.Join(homeDir, ".local", "share", "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	dbFile := filepath.Join(dbDir, "opencode-stable.db")

	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE session (
			id           TEXT PRIMARY KEY,
			directory    TEXT NOT NULL,
			time_updated INTEGER NOT NULL
		)
	`)
	if err != nil {
		db.Close()
		t.Fatalf("create table: %v", err)
	}
	_, err = db.Exec(
		`INSERT INTO session (id, directory, time_updated) VALUES (?, ?, ?)`,
		"ses_fallback", "/home/user/repo/main", int64(1000),
	)
	db.Close()
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Unset XDG_DATA_HOME and override HOME so the fallback path resolves correctly.
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", homeDir)

	got := opencode.LatestSessionForDir("/home/user/repo/main")
	if got != "ses_fallback" {
		t.Errorf("LatestSessionForDir (fallback path) = %q, want %q", got, "ses_fallback")
	}
}
