package db_test

// Fsync measurement harness for db.Open (issue #2612).
//
// TestProbeFreshOpen performs exactly one db.Open against a fresh database
// file and nothing else. It is the probe the fsync measurement in
// docs/test-database-fsync.md and issue #2612 refer to:
//
//	go test -c -o /tmp/db.test ./internal/db/
//	TMPDIR=<dir on a real filesystem> \
//	  strace -f -c -e trace=fsync /tmp/db.test -test.run '^TestProbeFreshOpen$'
//
// Measure on a real filesystem. On tmpfs fsync is a no-op, so the count is
// zero and the measurement is meaningless. Set TMPDIR to a directory on a
// disk-backed filesystem before you run the probe.
//
// The test itself asserts only that the open works; its value is the syscall
// count an external tracer observes, not an in-process assertion.

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestProbeFreshOpen opens one fresh database and closes it. See the file
// comment for how to measure its fsync count.
func TestProbeFreshOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
}
