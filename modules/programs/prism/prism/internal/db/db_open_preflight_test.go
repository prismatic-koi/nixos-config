package db_test

// Tests for the db.Open pre-flight probe.
//
// The probe converts the misleading modernc.org/sqlite text "unable to open
// database file: out of memory (14)" into a clear filesystem error naming the
// exact DB path and the underlying OS error.
//
// The read-only entry point OpenReadOnly() must remain unaffected: a read-only
// open of an existing DB in an unwritable directory is a legitimate use case
// (`prism db query` under `?mode=ro`) and the probe must never create the DB
// file as a side effect on that path.

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestOpen_UnwritableStateDir_ClearError verifies that when the state directory
// exists but is unwritable, db.Open returns a clear error naming the DB file
// path and the underlying OS error — no "out of memory" text.
func TestOpen_UnwritableStateDir_ClearError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics not applicable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions; probe cannot fail via mode bits")
	}

	stateDir := filepath.Join(t.TempDir(), "prism")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Restore mode on cleanup so t.TempDir can remove the tree.
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	dbPath := filepath.Join(stateDir, "prism.db")
	_, err := db.Open(dbPath)
	if err == nil {
		t.Fatalf("db.Open on unwritable state dir: got nil error, want failure")
	}

	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "out of memory") {
		t.Errorf("error must not contain 'out of memory'; got: %s", msg)
	}
	if !strings.Contains(msg, dbPath) {
		t.Errorf("error must name the DB file path %q; got: %s", dbPath, msg)
	}
	// Underlying OS error should be a permission-class errno (EACCES).
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error should wrap a permission error (errors.Is os.ErrPermission); got: %v", err)
	}
}

// TestOpen_UncreatableStateDir_ClearError verifies that when the state
// directory does not exist and cannot be created (parent unwritable), the
// error names the directory path and the underlying OS error.
func TestOpen_UncreatableStateDir_ClearError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics not applicable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions; MkdirAll cannot fail via mode bits")
	}

	// parent/ is writable; parent/locked/ is 0555. Trying to create
	// parent/locked/prism/ therefore fails at MkdirAll time.
	parent := t.TempDir()
	locked := filepath.Join(parent, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("MkdirAll locked: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	stateDir := filepath.Join(locked, "prism")
	dbPath := filepath.Join(stateDir, "prism.db")
	_, err := db.Open(dbPath)
	if err == nil {
		t.Fatalf("db.Open on uncreatable state dir: got nil error, want failure")
	}

	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "out of memory") {
		t.Errorf("error must not contain 'out of memory'; got: %s", msg)
	}
	if !strings.Contains(msg, stateDir) {
		t.Errorf("error must name the state directory path %q; got: %s", stateDir, msg)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error should wrap a permission error; got: %v", err)
	}
}

// TestOpenReadOnly_UnwritableDir_ProbeNotOnPath verifies that OpenReadOnly
// against an existing DB in an unwritable directory does NOT surface the
// probe's error text. The probe lives in Open() only — it
// must NOT be gated onto the read-only path.
//
// SQLite's WAL journal mode requires SHM writability, so the actual query
// may still fail for that separate reason; what this test guards is
// specifically the probe error class — the misleading modernc.org/sqlite
// "out of memory" text must not appear, and the caller's error must not
// contain the probe's "db: cannot open" prefix (which would prove the probe
// leaked onto the read-only code path).
func TestOpenReadOnly_UnwritableDir_ProbeNotOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics not applicable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem permissions")
	}

	stateDir := filepath.Join(t.TempDir(), "prism")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dbPath := filepath.Join(stateDir, "prism.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("initial db.Open (writable): %v", err)
	}
	d.Close()

	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	ro, err := db.OpenReadOnly(dbPath)
	if err == nil {
		// Best case (non-WAL DBs would land here): the read-only handle
		// works. Close it and we're done.
		ro.Close()
		return
	}
	// If there IS a failure, it must not carry the probe's fingerprint. The
	// probe only runs in Open(); if this error contains its prefix, the
	// probe has been mistakenly added to the read-only path.
	msg := err.Error()
	if strings.Contains(msg, "db: cannot open") {
		t.Errorf("OpenReadOnly must not emit the write-path probe error "+
			"(probe leaked onto read-only code path); got: %s", msg)
	}
	if strings.Contains(strings.ToLower(msg), "out of memory") {
		t.Errorf("error must not contain 'out of memory'; got: %s", msg)
	}
}

// TestOpenReadOnly_NoSideEffectDBFileCreation verifies that OpenReadOnly does
// NOT create the DB file (or its journal/WAL siblings) as a side effect. This
// is asserted directly by pointing OpenReadOnly at a path that does not exist
// and confirming no new file appears in the parent directory. A regression
// would mean the probe was accidentally executed on the read-only path.
func TestOpenReadOnly_NoSideEffectDBFileCreation(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "prism")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dbPath := filepath.Join(stateDir, "nonexistent.db")

	// Expect a failure — the file does not exist. What matters is the file
	// system state afterwards: no db-file created.
	if conn, err := db.OpenReadOnly(dbPath); err == nil {
		conn.Close()
		t.Fatalf("OpenReadOnly against missing file: got nil error, want failure")
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("OpenReadOnly leaked file %q into %s (probe leaked onto read-only path)",
			e.Name(), stateDir)
	}
}

// TestOpen_InMemory_NoStrayFile verifies that db.Open(":memory:") — SQLite's
// in-memory-database sentinel used by archive/abtest tests — does not create
// a stray file called ":memory:" in the working directory as a side effect
// of the pre-flight probe.
func TestOpen_InMemory_NoStrayFile(t *testing.T) {
	// Run in a temp CWD so any accidental ":memory:" file lands somewhere
	// we can observe (and clean up) instead of polluting the source tree.
	tmp := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open(:memory:): %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if _, err := os.Stat(filepath.Join(tmp, ":memory:")); err == nil {
		t.Errorf("pre-flight probe leaked a stray :memory: file into CWD")
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected Stat error: %v", err)
	}
}

// TestOpen_HappyPath verifies that the pre-flight probe does not disturb the
// normal writable-directory case: Open still succeeds and the returned DB is
// usable.
func TestOpen_HappyPath(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "prism")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dbPath := filepath.Join(stateDir, "prism.db")

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open (happy path): %v", err)
	}
	defer d.Close()

	// Sanity: schema_version row exists (schema was applied).
	var version int
	if err := d.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version <= 0 {
		t.Errorf("schema_version: got %d, want > 0", version)
	}

	// Sanity: the DB file exists at the expected path.
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("expected DB file at %s: %v", dbPath, err)
	}
}
