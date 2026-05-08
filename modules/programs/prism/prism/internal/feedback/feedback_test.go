package feedback

// Tests for the local feedback JSONL store. All tests use t.TempDir() for
// the storage path so the suite passes inside the nix-build sandbox where
// HOME=/homeless-shelter is unwritable.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "feedback.jsonl"))
}

func TestStore_Append_CreatesFileAndAppendsJSONL(t *testing.T) {
	s := newTestStore(t)

	e := Entry{
		Timestamp:    "2026-01-02T03:04:05Z",
		Text:         "the --tier flag rejects 'enterprise'",
		Session:      "nixos-config@feature",
		PrismVersion: "abcdef0",
	}
	if err := s.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	line := strings.TrimRight(string(got), "\n")
	if strings.Contains(line, "\n") {
		t.Errorf("expected single line, got: %q", line)
	}
	var parsed Entry
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("unmarshal: %v (raw: %s)", err, line)
	}
	if parsed != e {
		t.Errorf("got %+v, want %+v", parsed, e)
	}
}

func TestStore_Append_AppendsMultipleEntries(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := s.Append(Entry{
			Timestamp:    "2026-01-02T03:04:05Z",
			Text:         "line " + string(rune('a'+i)),
			PrismVersion: "v1",
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	entries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	for i, e := range entries {
		want := "line " + string(rune('a'+i))
		if e.Text != want {
			t.Errorf("entries[%d].Text = %q, want %q", i, e.Text, want)
		}
	}
}

func TestStore_List_MissingFile_ReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	entries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestStore_List_SkipsBlankAndMalformedLines(t *testing.T) {
	s := newTestStore(t)
	// Hand-craft a file containing one good line, one blank, one malformed.
	content := `{"timestamp":"2026-01-01T00:00:00Z","text":"ok","prism_version":"v"}` + "\n" +
		"\n" +
		"this is not JSON\n" +
		`{"timestamp":"2026-01-02T00:00:00Z","text":"also ok","prism_version":"v"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Text != "ok" || entries[1].Text != "also ok" {
		t.Errorf("got %+v", entries)
	}
}

func TestFilterSince_KeepsEqualOrAfterCutoff(t *testing.T) {
	cutoff := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	entries := []Entry{
		{Timestamp: "2026-01-09T23:59:59Z", Text: "old"},
		{Timestamp: "2026-01-10T00:00:00Z", Text: "boundary"},
		{Timestamp: "2026-01-11T00:00:00Z", Text: "new"},
	}
	got := FilterSince(entries, cutoff)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Text != "boundary" || got[1].Text != "new" {
		t.Errorf("got %+v", got)
	}
}

func TestFilterSince_KeepsUnparseableTimestamps(t *testing.T) {
	cutoff := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	entries := []Entry{
		{Timestamp: "not-a-time", Text: "weird"},
	}
	got := FilterSince(entries, cutoff)
	if len(got) != 1 {
		t.Errorf("expected 1, got %d", len(got))
	}
}

func TestStore_Prune_DropsOldEntriesAtomically(t *testing.T) {
	s := newTestStore(t)
	mustAppend(t, s, "2026-01-01T00:00:00Z", "old1")
	mustAppend(t, s, "2026-01-02T00:00:00Z", "old2")
	mustAppend(t, s, "2026-01-15T00:00:00Z", "kept1")
	mustAppend(t, s, "2026-01-20T00:00:00Z", "kept2")

	cutoff := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	kept, removed, err := s.Prune(cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if kept != 2 || removed != 2 {
		t.Errorf("kept=%d removed=%d, want 2,2", kept, removed)
	}
	entries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 || entries[0].Text != "kept1" || entries[1].Text != "kept2" {
		t.Errorf("got %+v", entries)
	}

	// Atomic rewrite: no leftover tempfile.
	dirEntries, _ := os.ReadDir(filepath.Dir(s.Path))
	for _, de := range dirEntries {
		if strings.HasPrefix(de.Name(), ".feedback-prune-") {
			t.Errorf("tempfile leftover: %s", de.Name())
		}
	}
}

func TestStore_Prune_NoOpWhenNothingToRemove(t *testing.T) {
	s := newTestStore(t)
	mustAppend(t, s, "2026-02-01T00:00:00Z", "kept")
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	kept, removed, err := s.Prune(cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if kept != 1 || removed != 0 {
		t.Errorf("kept=%d removed=%d, want 1,0", kept, removed)
	}
}

func TestStore_Prune_EmptyStore(t *testing.T) {
	s := newTestStore(t)
	kept, removed, err := s.Prune(time.Now())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if kept != 0 || removed != 0 {
		t.Errorf("kept=%d removed=%d, want 0,0", kept, removed)
	}
}

// DefaultPath honours XDG_STATE_HOME when set. We deliberately avoid
// asserting the HOME-fallback branch here because the nix sandbox sets
// HOME=/homeless-shelter; the production fallback is exercised by the
// non-sandbox dev shell only.
func TestDefaultPath_HonoursXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-test")
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := "/tmp/xdg-test/prism/feedback.jsonl"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// When XDG_STATE_HOME is empty and HOME is set to a normal value, the path
// resolves to $HOME/.local/state/prism/feedback.jsonl. We force HOME via
// t.Setenv so this test works inside the nix sandbox too.
func TestDefaultPath_FallsBackToHomeLocalState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/tmp/home-test")
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := "/tmp/home-test/.local/state/prism/feedback.jsonl"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func mustAppend(t *testing.T, s *Store, ts, text string) {
	t.Helper()
	if err := s.Append(Entry{
		Timestamp:    ts,
		Text:         text,
		PrismVersion: "v",
	}); err != nil {
		t.Fatalf("Append %q: %v", text, err)
	}
}
