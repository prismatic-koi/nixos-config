package db_test

// Fsync measurement harness for db.Open (issue #2612).
//
// TestProbeFreshOpen performs exactly one db.Open against a fresh database
// file and nothing else. It is the probe the fsync measurement in
// docs/test-database-fsync.md and issue #2612 refer to:
//
//	go test -c -o /tmp/db.test ./internal/db/
//	strace -f -c -e trace=fsync /tmp/db.test -test.run '^TestProbeFreshOpen$'
//
// strace counts the fsync syscall on any filesystem, so the count is identical
// on tmpfs and on a real disk. fsync latency is near zero on tmpfs but real on
// a CI runner; the latency is what makes the cost visible in wall time.
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
