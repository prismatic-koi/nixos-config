// Tests for the aggregate "restore complete: ..." summary line emitted at
// the end of `prism restore` (issue #1527). The line must always be printed,
// even when there are zero sessions to restore, so callers can grep for the
// totals reliably.

package cmd

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// captureStdout is shared with checkin_test.go (defined there).

// TestRestore_AggregateSummary_NoSessions verifies that the summary line is
// emitted even when there are zero sessions in the DB. This is the
// agent-friendly shape: empty stdout would force callers to special-case
// "no output means success".
func TestRestore_AggregateSummary_NoSessions(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	out := captureStdout(t, func() {
		if err := Restore(true); err != nil {
			t.Fatalf("Restore(dryRun=true): %v", err)
		}
	})

	// The summary line must be present and report 0/0/0.
	want := "restore complete: 0 restored, 0 skipped, 0 failed\n"
	if !strings.Contains(out, want) {
		t.Errorf("summary line missing or wrong:\n  got:  %q\n  want substring: %q", out, want)
	}
}

// TestRestore_AggregateSummary_DryRun verifies the counts are correct in
// dry-run mode when there are real rows. We use dryRun=true so no tmux/DB
// side effects are required.
func TestRestore_AggregateSummary_DryRun(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Seed three sessions; in dry-run mode all of them count as "would restore".
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("testrepo@feature-%d", i)
		wt := t.TempDir()
		if err := d.UpsertStatus(name, "testrepo", wt, "idle", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %q: %v", name, err)
		}
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	out := captureStdout(t, func() {
		if err := Restore(true); err != nil {
			t.Fatalf("Restore(dryRun=true): %v", err)
		}
	})

	// Summary line: 3 restored, 0 skipped, 0 failed.
	want := "restore complete: 3 restored, 0 skipped, 0 failed\n"
	if !strings.Contains(out, want) {
		t.Errorf("summary line missing or wrong:\n  got:  %q\n  want substring: %q", out, want)
	}

	// Per-session "would restore" lines must still be present alongside the
	// summary (not replaced by it).
	matches := regexp.MustCompile(`would restore: testrepo@feature-\d`).FindAllString(out, -1)
	if len(matches) != 3 {
		t.Errorf("expected 3 'would restore' lines, got %d:\n%s", len(matches), out)
	}
}
