package tailcursor_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/tailcursor"
)

func TestStore_LoadReportsNoStateWhenFileIsAbsent(t *testing.T) {
	s := tailcursor.NewStore(filepath.Join(t.TempDir(), "exporter-state.json"))
	_, err := s.Load()
	if !errors.Is(err, tailcursor.ErrNoState) {
		t.Fatalf("Load on a missing file returned %v, want ErrNoState", err)
	}
}

func TestStore_SaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "exporter-state.json")
	s := tailcursor.NewStore(path)

	want := tailcursor.NewState()
	want.SetCursor("agent_events", 4711)
	want.Counters["prism_agent_events_total"] = map[string]float64{
		`["tool_call"]`:  12,
		`["turn_start"]`: 3,
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c, ok := got.Cursor("agent_events"); !ok || c != 4711 {
		t.Errorf("cursor = %d (found=%v), want 4711", c, ok)
	}
	if v := got.Counters["prism_agent_events_total"][`["tool_call"]`]; v != 12 {
		t.Errorf("counter = %v, want 12", v)
	}
	if got.Version != tailcursor.StateVersion {
		t.Errorf("version = %d, want %d", got.Version, tailcursor.StateVersion)
	}
}

func TestStore_SaveWritesAtomicallyAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exporter-state.json")
	s := tailcursor.NewStore(path)

	for i := int64(1); i <= 5; i++ {
		st := tailcursor.NewState()
		st.SetCursor("agent_events", i)
		if err := s.Save(st); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Errorf("Save left a stray file behind: %s", e.Name())
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode %o, want 600 — it records this host's activity", perm)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c, _ := got.Cursor("agent_events"); c != 5 {
		t.Errorf("cursor = %d after 5 saves, want 5", c)
	}
}

func TestStore_LoadReportsCorruption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"truncated JSON", `{"version":1,"cursors":{"agent_events":1`},
		{"not JSON at all", "this is not json"},
		{"wrong type for cursor", `{"version":1,"cursors":{"agent_events":"nope"}}`},
		{"unknown version", `{"version":9999,"cursors":{}}`},
		{"negative cursor", `{"version":1,"cursors":{"agent_events":-1}}`},
		{"negative counter", `{"version":1,"cursors":{},"counters":{"m":{"[\"a\"]":-1}}}`},
		{"binary noise", "\x00\x01\x02\x03"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exporter-state.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			_, err := tailcursor.NewStore(path).Load()
			if err == nil {
				t.Fatal("Load accepted a corrupt state file")
			}
			var corrupt *tailcursor.CorruptError
			if !errors.As(err, &corrupt) {
				t.Fatalf("Load returned %T (%v), want a *CorruptError so the caller can tell it apart from ErrNoState", err, err)
			}
			if !strings.Contains(corrupt.Error(), path) {
				t.Errorf("CorruptError does not name the file: %v", corrupt)
			}
			if errors.Is(err, tailcursor.ErrNoState) {
				t.Error("a corrupt file must not be reported as a missing file")
			}
		})
	}
}

// A truncated file is what a crash mid-write would produce if Save were not
// atomic. Load must survive reading one of any length.
func TestStore_LoadSurvivesEveryTruncationOfAValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exporter-state.json")
	st := tailcursor.NewState()
	st.SetCursor("agent_events", 12345)
	st.Counters["prism_agent_events_total"] = map[string]float64{`["tool_call"]`: 99}
	if err := tailcursor.NewStore(path).Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	for n := 0; n < len(full); n++ {
		truncPath := filepath.Join(dir, "truncated.json")
		if err := os.WriteFile(truncPath, full[:n], 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		// The only requirement is that Load never panics and always
		// reports the problem. A prefix of valid JSON is never itself
		// valid JSON of the same object, so every truncation must fail.
		if _, err := tailcursor.NewStore(truncPath).Load(); err == nil {
			t.Fatalf("Load accepted a %d-byte truncation of a %d-byte file", n, len(full))
		}
	}
}

func TestStore_SaveRejectsNil(t *testing.T) {
	s := tailcursor.NewStore(filepath.Join(t.TempDir(), "exporter-state.json"))
	if err := s.Save(nil); err == nil {
		t.Fatal("Save(nil) succeeded, want an error")
	}
}

func TestState_CursorOnZeroValue(t *testing.T) {
	var s *tailcursor.State
	if _, ok := s.Cursor("agent_events"); ok {
		t.Error("nil State reported a cursor")
	}
	empty := &tailcursor.State{}
	if _, ok := empty.Cursor("agent_events"); ok {
		t.Error("empty State reported a cursor")
	}
	empty.SetCursor("agent_events", 3)
	if c, ok := empty.Cursor("agent_events"); !ok || c != 3 {
		t.Errorf("cursor = %d (found=%v), want 3", c, ok)
	}
}
