package pi_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/harness/pi"
	harnessarchive "github.com/prismatic-koi/prism/internal/harness/archive"
)

func TestArchiveAdapter_SourcePath_WithHarnessSessionID(t *testing.T) {
	a := pi.NewArchiveAdapter()
	home, _ := os.UserHomeDir()

	p := harnessarchive.SourceParams{
		HarnessSessionID: "sess-abc-123",
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "pi", "sessions", "sess-abc-123")
	if got != want {
		t.Errorf("SourcePath: want %q got %q", want, got)
	}
}

func TestArchiveAdapter_SourcePath_EmptyHarnessSessionID(t *testing.T) {
	a := pi.NewArchiveAdapter()
	home, _ := os.UserHomeDir()

	p := harnessarchive.SourceParams{}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "pi", "sessions")
	if got != want {
		t.Errorf("SourcePath (empty ID): want %q got %q", want, got)
	}
}

func TestArchiveAdapter_Archive_CopiesJSONLFiles(t *testing.T) {
	tmpSrc := t.TempDir()
	tmpDst := t.TempDir()

	// Create some JSONL files and a non-JSONL file in the source.
	if err := os.WriteFile(filepath.Join(tmpSrc, "session.jsonl"), []byte(`{"type":"state_change"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpSrc, "branch-1.jsonl"), []byte(`{"type":"msg_assistant"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpSrc, "README.txt"), []byte("not jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := pi.NewArchiveAdapter()
	if err := a.Archive(context.Background(), tmpSrc, tmpDst, harnessarchive.SourceParams{}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// session.jsonl and branch-1.jsonl should be copied; README.txt should not.
	for _, name := range []string{"session.jsonl", "branch-1.jsonl"} {
		if _, err := os.Stat(filepath.Join(tmpDst, name)); err != nil {
			t.Errorf("expected %q in rawDir: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(tmpDst, "README.txt")); !os.IsNotExist(err) {
		t.Error("expected README.txt NOT to be copied")
	}
}

func TestArchiveAdapter_Archive_MissingSrcPath_NoError(t *testing.T) {
	tmpDst := t.TempDir()
	a := pi.NewArchiveAdapter()

	// Non-existent source directory should be a no-op, not an error.
	err := a.Archive(context.Background(), "/nonexistent/pi/sessions/sess-xyz", tmpDst, harnessarchive.SourceParams{})
	if err != nil {
		t.Fatalf("Archive with missing src: expected nil error, got %v", err)
	}
}

func TestArchiveAdapter_Export_CopiesSessionJSONL(t *testing.T) {
	archiveDir := t.TempDir()
	rawDir := filepath.Join(archiveDir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}

	content := `{"type":"state_change","state":"finished"}` + "\n"
	if err := os.WriteFile(filepath.Join(rawDir, "session.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	a := pi.NewArchiveAdapter()
	if err := a.Export(context.Background(), archiveDir, harnessarchive.SourceParams{}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	dst := filepath.Join(archiveDir, "session.jsonl")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read session.jsonl: %v", err)
	}
	if string(got) != content {
		t.Errorf("session.jsonl content: want %q got %q", content, got)
	}
}

func TestArchiveAdapter_Export_NoRawSessionJSONL_NoError(t *testing.T) {
	archiveDir := t.TempDir()
	rawDir := filepath.Join(archiveDir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}

	a := pi.NewArchiveAdapter()
	// No session.jsonl in raw/ — should return nil, not error.
	if err := a.Export(context.Background(), archiveDir, harnessarchive.SourceParams{}); err != nil {
		t.Fatalf("Export with missing session.jsonl: expected nil, got %v", err)
	}
	// No session.jsonl should be written at archive root.
	if _, err := os.Stat(filepath.Join(archiveDir, "session.jsonl")); !os.IsNotExist(err) {
		t.Error("expected session.jsonl NOT to be created when raw/session.jsonl is absent")
	}
}
