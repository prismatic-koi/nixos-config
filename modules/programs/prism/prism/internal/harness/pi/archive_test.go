package pi_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/harness/pi"
	harnessarchive "github.com/prismatic-koi/prism/internal/harness/archive"
)

// sandboxExecStagingHome returns the expected staging HOME path for a given
// instanceID, mirroring container.SandboxExecStagingHomePath without importing
// the container package (avoids circular imports in tests).
func sandboxExecStagingHome(t *testing.T, instanceID string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	return filepath.Join(home, ".local", "state", "prism", "sessions", instanceID, "home")
}

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
	want := filepath.Join(home, ".pi", "agent", "sessions", "sess-abc-123")
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
	want := filepath.Join(home, ".pi", "agent", "sessions")
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

// TestArchiveAdapter_SourcePath_SandboxExec verifies that when IsolationMode
// is "sandbox-exec", SourcePath uses the per-session staging HOME (not the
// real home directory). This is bug #1538 fix #2: sandbox-exec sessions write
// PI data to <stagingHome>/.pi/agent/sessions/<sessionID>/, not ~/.pi/.
func TestArchiveAdapter_SourcePath_SandboxExec(t *testing.T) {
	const instanceID = "test-instance-uuid-1234"
	const sessionID = "pi-ses-abc-sandbox"

	a := pi.NewArchiveAdapter()
	p := harnessarchive.SourceParams{
		HarnessSessionID: sessionID,
		IsolationMode:    "sandbox-exec",
		InstanceID:       instanceID,
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath: %v", err)
	}

	// Expected path: <stagingHome>/.pi/agent/sessions/<sessionID>
	stagingHome := sandboxExecStagingHome(t, instanceID)
	want := filepath.Join(stagingHome, ".pi", "agent", "sessions", sessionID)
	if got != want {
		t.Errorf("SourcePath (sandbox-exec): got %q, want %q", got, want)
	}

	// Must NOT contain the real home directory.
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(got, filepath.Join(home, ".pi")) {
		t.Errorf("SourcePath (sandbox-exec) returned real home path %q; expected staging home", got)
	}
}

// TestArchiveAdapter_SourcePath_SandboxExec_EmptyInstanceID verifies that when
// IsolationMode is "sandbox-exec" but InstanceID is empty (unusual edge case),
// SourcePath falls back to the real home directory path rather than returning
// an error that would abort cleanup entirely.
func TestArchiveAdapter_SourcePath_SandboxExec_EmptyInstanceID(t *testing.T) {
	a := pi.NewArchiveAdapter()
	p := harnessarchive.SourceParams{
		HarnessSessionID: "some-session",
		IsolationMode:    "sandbox-exec",
		InstanceID:       "", // empty — forces fallback
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath with empty InstanceID: %v", err)
	}
	// Fallback: uses real home directory.
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".pi", "agent", "sessions", "some-session")
	if got != want {
		t.Errorf("SourcePath (sandbox-exec, empty InstanceID): got %q, want %q", got, want)
	}
}

// TestArchiveAdapter_SourcePath_NonSandboxExec_UsesRealHome verifies that
// non-sandbox-exec isolation modes (podman, bwrap, host) continue to use the
// real home directory, not the staging HOME.
func TestArchiveAdapter_SourcePath_NonSandboxExec_UsesRealHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	const sessionID = "pi-ses-xyz"

	for _, mode := range []string{"podman", "bwrap", "host", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			a := pi.NewArchiveAdapter()
			p := harnessarchive.SourceParams{
				HarnessSessionID: sessionID,
				IsolationMode:    mode,
				InstanceID:       "some-instance-id",
			}
			got, err := a.SourcePath(p)
			if err != nil {
				t.Fatalf("SourcePath (mode=%q): %v", mode, err)
			}
			want := filepath.Join(home, ".pi", "agent", "sessions", sessionID)
			if got != want {
				t.Errorf("SourcePath (mode=%q): got %q, want %q", mode, got, want)
			}
		})
	}
}
