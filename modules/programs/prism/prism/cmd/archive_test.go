package cmd

// Tests for prism archive (issue #999 ACs):
//   - prism archive <instance-id> → prints archive_path
//   - prism archive <session-name> → most recent incarnation's archive_path
//   - prism archive <session-name> --all → all paths, newest first
//   - prism archive --instance <uuid> → force UUID lookup
//   - prism archive <id> where archive_path IS NULL → "session not yet archived" + exit non-zero
//   - prism archive <unknown> → exit non-zero with error

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- archive_path tests ---

// TestRunArchive_ByInstanceID verifies that a full UUID returns the archive_path.
func TestRunArchive_ByInstanceID(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "/archive/testrepo/path")

	out := captureStdout(t, func() {
		if err := printArchivePath(d, iid, true); err != nil {
			t.Errorf("printArchivePath: %v", err)
		}
	})

	if !strings.Contains(out, "/archive/testrepo/path") {
		t.Errorf("expected archive path in output\ngot:\n%s", out)
	}
}

// TestRunArchive_BySessionName verifies that a session name resolves to the
// most recent incarnation's archive_path.
func TestRunArchive_BySessionName(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	// Two incarnations — most recent should win.
	iid1 := uuid.New().String()
	iid2 := uuid.New().String()
	insertTestSession(t, d, iid1, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-3*time.Hour), base.Add(-2*time.Hour), "finished", "/archive/old")
	insertTestSession(t, d, iid2, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "/archive/new")

	out := captureStdout(t, func() {
		if err := printArchivePath(d, "testrepo@main", false); err != nil {
			t.Errorf("printArchivePath: %v", err)
		}
	})

	if !strings.Contains(out, "/archive/new") {
		t.Errorf("expected most recent archive path '/archive/new'\ngot:\n%s", out)
	}
	if strings.Contains(out, "/archive/old") {
		t.Errorf("expected old archive path to NOT appear\ngot:\n%s", out)
	}
}

// TestRunArchive_NullArchivePath verifies that a NULL archive_path returns an error.
func TestRunArchive_NullArchivePath(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	// No archive path (empty string treated as unarchived).
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "")

	err := printArchivePath(d, iid, true)
	if err == nil {
		t.Fatal("expected error for NULL archive_path, got nil")
	}
	if !strings.Contains(err.Error(), "not yet archived") {
		t.Errorf("expected 'not yet archived' in error\ngot: %v", err)
	}
}

// TestRunArchive_UnknownID verifies that an unknown instance_id returns an error.
func TestRunArchive_UnknownID(t *testing.T) {
	d := openIncarnationTestDB(t)

	unknownID := uuid.New().String()
	err := printArchivePath(d, unknownID, true)
	if err == nil {
		t.Fatal("expected error for unknown instance_id, got nil")
	}
}

// TestRunArchive_AllFlag verifies that --all prints one path per incarnation,
// newest first.
func TestRunArchive_AllFlag(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid1 := uuid.New().String()
	iid2 := uuid.New().String()
	iid3 := uuid.New().String()
	// Insert in chronological order; --all should return newest first.
	insertTestSession(t, d, iid1, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-3*time.Hour), base.Add(-2*time.Hour), "finished", "/archive/1")
	insertTestSession(t, d, iid2, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-2*time.Hour), base.Add(-1*time.Hour), "finished", "/archive/2")
	insertTestSession(t, d, iid3, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "/archive/3")

	out := captureStdout(t, func() {
		if err := printArchivePathAll(d, "testrepo@main"); err != nil {
			t.Errorf("printArchivePathAll: %v", err)
		}
	})

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d\n%s", len(lines), out)
	}
	// Newest first: /archive/3, /archive/2, /archive/1.
	if !strings.Contains(lines[0], "/archive/3") {
		t.Errorf("first line should be newest (/archive/3)\ngot: %q", lines[0])
	}
	if !strings.Contains(lines[1], "/archive/2") {
		t.Errorf("second line should be /archive/2\ngot: %q", lines[1])
	}
	if !strings.Contains(lines[2], "/archive/1") {
		t.Errorf("third line should be oldest (/archive/1)\ngot: %q", lines[2])
	}
}

// TestRunArchive_AllFlag_NotYetArchived verifies that --all shows "(not yet archived)"
// for incarnations with NULL archive_path.
func TestRunArchive_AllFlag_NotYetArchived(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "opencode",
		base.Add(-1*time.Hour), base, "finished", "") // no archive path

	out := captureStdout(t, func() {
		if err := printArchivePathAll(d, "testrepo@main"); err != nil {
			t.Errorf("printArchivePathAll: %v", err)
		}
	})

	if !strings.Contains(out, "not yet archived") {
		t.Errorf("expected '(not yet archived)' for NULL archive_path\ngot:\n%s", out)
	}
}

// TestRunArchive_AllFlag_UnknownSession verifies that --all with an unknown session
// name returns an error.
func TestRunArchive_AllFlag_UnknownSession(t *testing.T) {
	d := openIncarnationTestDB(t)

	err := printArchivePathAll(d, "nonexistent@branch")
	if err == nil {
		t.Fatal("expected error for unknown session name, got nil")
	}
}
