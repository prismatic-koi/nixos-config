package sidecartest

// templatedb.go — a fast opener for isolated test databases (issue #2598).
//
// # Why this exists
//
// db.Open applies the declarative schema, seeds schema_version, and then runs
// every migration in order. Each of those statements commits in autocommit
// mode, and the connection DSN sets journal_mode=WAL while SQLite's default
// synchronous=FULL stays in force, so every commit costs one fsync of the
// WAL. A single db.Open on a fresh file costs 73 fsyncs.
//
// That is irrelevant on a developer host where the test tempdir is a tmpfs
// (fsync is a no-op) but it is not irrelevant on a CI runner, where the
// tempdir is on a real disk. internal/sidecar alone opened ~700 test
// databases per run, so the package paid ~51,000 fsyncs before it ran a
// single assertion. Package wall time was therefore
// (CPU work) + (fsync count x per-fsync latency), and the second term is set
// by runner IO health rather than by anything in the code under test. When a
// hosted runner degraded, every SQLite-backed package inflated about 4x
// (cmd 137s -> 494s, internal/db 124s -> 491s, internal/session 26s -> 86s)
// while packages with no database were flat. internal/sidecar is the longest
// of them, so it was the first to cross the default 10-minute `go test`
// timeout and fail the go-tests job. See issue #2598.
//
// # What this does
//
// A database that is already at the current schema version costs ZERO fsyncs
// to open: every statement db.Open runs is idempotent (CREATE ... IF NOT
// EXISTS, guarded ALTER TABLE, no-op migrations), so SQLite starts no write
// transaction and writes no WAL frame.
//
// OpenDB therefore builds one fully-migrated database per test binary, keeps
// its bytes in memory, and stamps a copy at the caller's path before calling
// db.Open. The first call pays the 73 fsyncs; every later call pays none.
// The database the caller receives is byte-for-byte equivalent to one from a
// plain db.Open — same schema, same schema_version, same sqlite_master —
// which templatedb_test.go pins.
//
// # Scope
//
// This is a test-support helper. Production code is unchanged: the prism
// database keeps synchronous=FULL and keeps its per-commit durability.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

var (
	templateOnce  sync.Once
	templateBytes []byte
	templateErr   error
)

// templateDB returns the bytes of a fully-migrated, empty prism database. It
// is built once per test binary and cached in memory; the scratch directory
// used to build it is removed before the bytes are returned.
func templateDB() ([]byte, error) {
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "prism-db-template-")
		if err != nil {
			templateErr = fmt.Errorf("sidecartest: create template dir: %w", err)
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()

		path := filepath.Join(dir, "template.db")
		d, err := db.Open(path)
		if err != nil {
			templateErr = fmt.Errorf("sidecartest: build template DB: %w", err)
			return
		}

		// Fold the WAL back into the main database file so the bytes we
		// cache carry the whole schema. SQLite checkpoints on the last
		// connection close as well, but doing it explicitly keeps the
		// cached bytes correct even if that behaviour changes.
		var busy, logFrames, checkpointed int
		if err := d.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
			d.Close()
			templateErr = fmt.Errorf("sidecartest: checkpoint template DB: %w", err)
			return
		}
		d.Close()

		b, err := os.ReadFile(path)
		if err != nil {
			templateErr = fmt.Errorf("sidecartest: read template DB: %w", err)
			return
		}
		if len(b) == 0 {
			templateErr = fmt.Errorf("sidecartest: template DB at %s is empty", path)
			return
		}
		templateBytes = b
	})
	return templateBytes, templateErr
}

// seedFromTemplate stamps a fully-migrated database at path. The parent
// directory is created if it does not exist. path must not already exist —
// callers open databases at fresh paths, and silently overwriting one would
// hide a test bug.
func seedFromTemplate(path string) error {
	b, err := templateDB()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("sidecartest: create DB parent dir: %w", err)
	}
	// Mode 0o644 matches the file db.Open creates in its pre-flight probe,
	// so a seeded database is indistinguishable from a freshly opened one.
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("sidecartest: write seeded DB: %w", err)
	}
	return nil
}

// OpenDB opens an isolated test database at path. The database is identical
// to one returned by db.Open but costs no fsync to prepare — see the file
// comment for why that matters.
//
// The caller owns the returned handle and must close it. OpenDB deliberately
// does not register a t.Cleanup: both call sites close the database in a
// cleanup that must also remove the enclosing directory, and that ordering is
// theirs to control.
//
// If the template cannot be built for any reason, OpenDB falls back to a
// plain db.Open so a test can never fail because of the optimisation.
func OpenDB(t testing.TB, path string) *db.DB {
	t.Helper()

	if _, err := os.Stat(path); err == nil {
		t.Fatalf("sidecartest.OpenDB: %s already exists; pass a fresh path", path)
	}

	if err := seedFromTemplate(path); err != nil {
		// Non-fatal: db.Open below still produces a correct database, it
		// just pays the full schema + migration cost. Report it so a
		// broken template does not silently become the norm.
		t.Logf("sidecartest.OpenDB: template unavailable, falling back to a full db.Open: %v", err)
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("sidecartest.OpenDB: open %s: %v", path, err)
	}
	return d
}
