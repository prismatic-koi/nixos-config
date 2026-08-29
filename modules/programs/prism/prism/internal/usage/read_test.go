// Tests for the read path behind `prism account usage`.
package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSnapshotFile(t *testing.T, dir, account string, snap Snapshot) {
	t.Helper()
	if err := NewStore(dir).Write(snap); err != nil {
		t.Fatalf("Write %s: %v", account, err)
	}
}

func TestReadAll_MarksActiveFromCurrentJSON(t *testing.T) {
	dir := t.TempDir()
	// Write "personal" first, then "work" — Write() always mirrors the most
	// recently written snapshot into current.json, so "work" ends up active.
	writeSnapshotFile(t, dir, "personal", Snapshot{
		CapturedAt: FormatCapturedAt(time.Now()),
		Account:    "personal",
	})
	writeSnapshotFile(t, dir, "work", fullSnapshot())

	rows, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	// sorted by name: personal, work
	if rows[0].Name != "personal" || rows[0].Active {
		t.Errorf("personal row = %+v, want inactive", rows[0])
	}
	if rows[1].Name != "work" || !rows[1].Active {
		t.Errorf("work row = %+v, want active", rows[1])
	}
}

func TestReadAll_UsageDirMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := ReadAll(dir)
	if err == nil {
		t.Fatalf("expected an error for a missing dir")
	}
	var missing *ErrUsageDirMissing
	if !asErrUsageDirMissing(err, &missing) {
		t.Fatalf("expected *ErrUsageDirMissing, got %T: %v", err, err)
	}
	if missing.Dir != dir {
		t.Errorf("missing.Dir = %q, want %q", missing.Dir, dir)
	}
}

func asErrUsageDirMissing(err error, out **ErrUsageDirMissing) bool {
	m, ok := err.(*ErrUsageDirMissing)
	if ok {
		*out = m
	}
	return ok
}

func TestReadAll_MalformedFileReportedButOthersStillReturned(t *testing.T) {
	dir := t.TempDir()
	writeSnapshotFile(t, dir, "work", fullSnapshot())

	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write broken.json: %v", err)
	}

	rows, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	var broken, work *AccountSnapshot
	for i := range rows {
		switch rows[i].Name {
		case "broken":
			broken = &rows[i]
		case "work":
			work = &rows[i]
		}
	}
	if broken == nil || broken.ReadErr == nil {
		t.Fatalf("expected broken.json to report a ReadErr, got %+v", broken)
	}
	if work == nil || work.ReadErr != nil || work.Snapshot == nil {
		t.Fatalf("expected work.json to parse cleanly, got %+v", work)
	}
}

func TestReadAll_EmptyDirReturnsEmptySlice(t *testing.T) {
	dir := t.TempDir()
	rows, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0", len(rows))
	}
}

func TestReadAll_CurrentJSONIsNotTreatedAsAnAccount(t *testing.T) {
	dir := t.TempDir()
	writeSnapshotFile(t, dir, "work", fullSnapshot())

	rows, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (current.json must be excluded): %+v", len(rows), rows)
	}
	if rows[0].Name != "work" {
		t.Errorf("row name = %q, want %q", rows[0].Name, "work")
	}
}

func TestAccountSnapshot_Stale(t *testing.T) {
	now := time.Now()

	fresh := AccountSnapshot{Snapshot: &Snapshot{CapturedAt: FormatCapturedAt(now.Add(-1 * time.Minute))}}
	if fresh.Stale(now) {
		t.Errorf("a 1-minute-old snapshot must not be stale")
	}

	old := AccountSnapshot{Snapshot: &Snapshot{CapturedAt: FormatCapturedAt(now.Add(-16 * time.Minute))}}
	if !old.Stale(now) {
		t.Errorf("a 16-minute-old snapshot must be stale")
	}

	justUnder := AccountSnapshot{Snapshot: &Snapshot{CapturedAt: FormatCapturedAt(now.Add(-14 * time.Minute))}}
	if justUnder.Stale(now) {
		t.Errorf("14 minutes must not be stale")
	}
}

func TestDefaultDir_HonoursXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state-example")
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	want := filepath.Join("/tmp/xdg-state-example", "prism", "usage")
	if dir != want {
		t.Errorf("DefaultDir() = %q, want %q", dir, want)
	}
}
