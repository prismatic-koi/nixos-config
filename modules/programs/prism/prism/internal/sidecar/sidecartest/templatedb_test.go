package sidecartest

// templatedb_test.go — guards for the seeded-template test-database opener
// (issue #2598).
//
// Two properties matter and each has a test here:
//
//  1. Equivalence — a database from OpenDB is indistinguishable from one from
//     a plain db.Open. If it were not, every test in internal/sidecar would be
//     running against a subtly different schema.
//  2. The fast path is live — the file OpenDB hands to db.Open already carries
//     the full schema. Without this, the template could silently degrade to an
//     empty file, db.Open would re-run the schema and every migration, the
//     fsync cost would come back, and property 1 would still hold, so nothing
//     would fail.

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// schemaFingerprint returns the sorted CREATE statements of every object in
// the database at path, plus the recorded schema_version.
func schemaFingerprint(t *testing.T, path string) ([]string, int) {
	t.Helper()

	ro, err := db.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only %s: %v", path, err)
	}
	defer ro.Close()

	rows, err := ro.Query("SELECT COALESCE(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		t.Fatalf("read sqlite_master from %s: %v", path, err)
	}
	defer rows.Close()

	var ddl []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan sqlite_master row: %v", err)
		}
		if s != "" {
			ddl = append(ddl, s)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	sort.Strings(ddl)

	var version int
	if err := ro.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version from %s: %v", path, err)
	}
	return ddl, version
}

// TestOpenDB_MatchesAPlainOpen pins the equivalence property: the schema and
// schema_version of a seeded database match a database db.Open built from
// nothing.
func TestOpenDB_MatchesAPlainOpen(t *testing.T) {
	dir := t.TempDir()

	directPath := filepath.Join(dir, "direct.db")
	direct, err := db.Open(directPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	direct.Close()

	seededPath := filepath.Join(dir, "seeded.db")
	seeded := OpenDB(t, seededPath)
	seeded.Close()

	wantDDL, wantVersion := schemaFingerprint(t, directPath)
	gotDDL, gotVersion := schemaFingerprint(t, seededPath)

	if gotVersion != wantVersion {
		t.Errorf("seeded schema_version = %d, want %d (same as a plain db.Open)", gotVersion, wantVersion)
	}
	if len(gotDDL) != len(wantDDL) {
		t.Fatalf("seeded DB has %d schema objects, plain db.Open has %d", len(gotDDL), len(wantDDL))
	}
	for i := range wantDDL {
		if gotDDL[i] != wantDDL[i] {
			t.Errorf("schema object %d differs:\n seeded: %s\n direct: %s", i, gotDDL[i], wantDDL[i])
		}
	}
}

// TestSeedFromTemplate_WritesAMigratedDatabase pins the fast path: the file
// written before db.Open runs is already a migrated database. If the template
// ever degrades to an empty or partial file, db.Open would quietly repair it
// and only this test would notice.
func TestSeedFromTemplate_WritesAMigratedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seeded.db")
	if err := seedFromTemplate(path); err != nil {
		t.Fatalf("seedFromTemplate: %v", err)
	}

	// Read the file as it stands before any db.Open touches it.
	ro, err := db.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open seeded file read-only: %v", err)
	}
	defer ro.Close()

	var version int
	if err := ro.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("seeded file has no usable schema_version: %v", err)
	}

	// agent_status is the table every sidecar test writes to. Its presence
	// before db.Open runs is what makes the open a no-op.
	var name string
	if err := ro.QueryRow(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'agent_status'",
	).Scan(&name); err != nil {
		t.Fatalf("seeded file is missing the agent_status table: %v", err)
	}

	// A migrated database must be at the version a plain db.Open reaches, not
	// at the seed value db.Open starts from.
	directPath := filepath.Join(t.TempDir(), "direct.db")
	direct, err := db.Open(directPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	direct.Close()
	_, wantVersion := schemaFingerprint(t, directPath)
	if version != wantVersion {
		t.Errorf("seeded file schema_version = %d, want %d — the template is not fully migrated", version, wantVersion)
	}
}

// TestOpenDB_RejectsAnExistingPath pins the guard against reusing a path: a
// caller that passes a live database would otherwise have it overwritten.
func TestOpenDB_RejectsAnExistingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exists.db")
	if err := os.WriteFile(path, []byte("not a database"), 0o644); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}

	fake := &fatalRecorder{TB: t}
	func() {
		defer func() { _ = recover() }()
		OpenDB(fake, path)
	}()

	if !fake.failed {
		t.Error("OpenDB accepted an existing path, want a fatal error")
	}
}

// fatalRecorder is a testing.TB that records Fatalf instead of ending the
// test, so the rejection path can be asserted.
type fatalRecorder struct {
	testing.TB
	failed bool
}

func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.failed = true
	panic("fatal")
}

func (f *fatalRecorder) Helper() {}
