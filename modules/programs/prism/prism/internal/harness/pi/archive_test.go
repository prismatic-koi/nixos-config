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

// encodePiCWDForTest mirrors the encodePiCWD logic in archive.go for use in
// test assertions without exporting the function.
func encodePiCWDForTest(cwd string) string {
	stripped := strings.TrimLeft(cwd, "/\\")
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return "--" + r.Replace(stripped) + "--"
}

// TestEncodePiCWD verifies the encoding formula matches pi 0.72.1
// dist/core/session-manager.js line 213.
func TestEncodePiCWD(t *testing.T) {
	cases := []struct {
		cwd  string
		want string
	}{
		{"/home/ben/code/nixos-config/main", "--home-ben-code-nixos-config-main--"},
		{"/home/ben", "--home-ben--"},
		{"/tmp/test-foo", "--tmp-test-foo--"},
		{"/", "----"},
	}
	for _, tc := range cases {
		got := encodePiCWDForTest(tc.cwd)
		if got != tc.want {
			t.Errorf("encodePiCWD(%q): got %q, want %q", tc.cwd, got, tc.want)
		}
	}
}

// TestArchiveAdapter_SourcePath_HostMode verifies that SourcePath resolves to
// the correct file inside the encoded-cwd directory for a host-mode session.
func TestArchiveAdapter_SourcePath_HostMode(t *testing.T) {
	// Redirect $HOME to a temp dir so this test does not write to the real
	// home directory (or /homeless-shelter in the Nix sandbox CI build).
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	home := fakeHome

	const worktree = "/tmp/test-my-repo"
	const sessionID = "d13cc856-f919-4ce9-b733-cf0e25493e62"

	// Create the directory and a matching session file on disk.
	encodedDir := encodePiCWDForTest(worktree) // --tmp-test-my-repo--
	sessDir := filepath.Join(home, ".pi", "agent", "sessions", encodedDir)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	fileName := "2026-04-13T09-58-35-623Z_" + sessionID + ".jsonl"
	filePath := filepath.Join(sessDir, fileName)
	if err := os.WriteFile(filePath, []byte(`{"type":"session"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := pi.NewArchiveAdapter()
	p := harnessarchive.SourceParams{
		HarnessSessionID: sessionID,
		Worktree:         worktree,
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath: %v", err)
	}
	if got != filePath {
		t.Errorf("SourcePath: got %q, want %q", got, filePath)
	}
}

// TestArchiveAdapter_SourcePath_EmptyHarnessSessionID verifies that when
// HarnessSessionID is empty (harness failed to start), SourcePath returns the
// sessions root, which Archive's os.IsNotExist path treats as a no-op.
func TestArchiveAdapter_SourcePath_EmptyHarnessSessionID(t *testing.T) {
	a := pi.NewArchiveAdapter()
	home, _ := os.UserHomeDir()

	p := harnessarchive.SourceParams{
		Worktree: "/tmp/test-my-repo",
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath: %v", err)
	}
	want := filepath.Join(home, ".pi", "agent", "sessions")
	if got != want {
		t.Errorf("SourcePath (empty ID): got %q, want %q", got, want)
	}
}

// TestArchiveAdapter_SourcePath_EmptyWorktree verifies that when Worktree is
// empty (non-worktree session), SourcePath returns the sessions root.
func TestArchiveAdapter_SourcePath_EmptyWorktree(t *testing.T) {
	a := pi.NewArchiveAdapter()
	home, _ := os.UserHomeDir()

	p := harnessarchive.SourceParams{
		HarnessSessionID: "some-uuid",
		Worktree:         "",
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath: %v", err)
	}
	want := filepath.Join(home, ".pi", "agent", "sessions")
	if got != want {
		t.Errorf("SourcePath (empty worktree): got %q, want %q", got, want)
	}
}

// TestArchiveAdapter_SourcePath_NoCWDDir verifies that when the encoded-cwd
// directory does not exist on disk, SourcePath returns a sentinel path (not an
// error), and Archive treats it as a no-op via os.IsNotExist.
func TestArchiveAdapter_SourcePath_NoCWDDir(t *testing.T) {
	a := pi.NewArchiveAdapter()
	home, _ := os.UserHomeDir()
	const sessionID = "aabbccdd-0000-0000-0000-000000000000"
	// Use a worktree path unlikely to have a real session directory.
	const worktree = "/tmp/nonexistent-prism-test-worktree-xyzzy"

	p := harnessarchive.SourceParams{
		HarnessSessionID: sessionID,
		Worktree:         worktree,
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath: unexpected error %v", err)
	}
	// Should be inside the encoded-cwd dir (which doesn't exist), not the sessions root.
	encodedDir := encodePiCWDForTest(worktree)
	expectedPrefix := filepath.Join(home, ".pi", "agent", "sessions", encodedDir)
	if !strings.HasPrefix(got, expectedPrefix) {
		t.Errorf("SourcePath (no dir): got %q, expected prefix %q", got, expectedPrefix)
	}

	// Archive should treat the sentinel path as a no-op.
	rawDir := t.TempDir()
	archiveAdapter := pi.NewArchiveAdapter()
	if err := archiveAdapter.Archive(context.Background(), got, rawDir, p); err != nil {
		t.Fatalf("Archive with sentinel path: expected nil error, got %v", err)
	}
	// rawDir must remain empty.
	entries, _ := os.ReadDir(rawDir)
	if len(entries) != 0 {
		t.Errorf("rawDir must be empty after no-op Archive; got %d entry/entries", len(entries))
	}
}

// TestArchiveAdapter_Archive_Directory_NoOp verifies that when srcPath is a
// directory (the AC4 case: HarnessSessionID empty → SourcePath returns sessionsRoot),
// Archive returns nil without attempting to copy and leaves rawDir empty.
func TestArchiveAdapter_Archive_Directory_NoOp(t *testing.T) {
	srcDir := t.TempDir() // a real, existing directory
	rawDir := t.TempDir()
	a := pi.NewArchiveAdapter()
	if err := a.Archive(context.Background(), srcDir, rawDir, harnessarchive.SourceParams{}); err != nil {
		t.Fatalf("Archive with directory srcPath: expected nil error, got %v", err)
	}
	entries, _ := os.ReadDir(rawDir)
	if len(entries) != 0 {
		t.Errorf("rawDir must be empty after directory no-op; got %d entry/entries", len(entries))
	}
}

// TestArchiveAdapter_Archive_CopiesFileAsSessionJSONL verifies that Archive
// copies the single source file into rawDir/session.jsonl.
func TestArchiveAdapter_Archive_CopiesFileAsSessionJSONL(t *testing.T) {
	// Create a fake pi session file (pi layout: <ts>_<uuid>.jsonl).
	tmpSrc := t.TempDir()
	sessionFile := filepath.Join(tmpSrc, "2026-04-13T09-58-35-623Z_d13cc856-f919-4ce9-b733-cf0e25493e62.jsonl")
	content := `{"type":"session","version":3}` + "\n" + `{"type":"msg_assistant"}` + "\n"
	if err := os.WriteFile(sessionFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	rawDir := t.TempDir()
	a := pi.NewArchiveAdapter()
	if err := a.Archive(context.Background(), sessionFile, rawDir, harnessarchive.SourceParams{}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Must produce rawDir/session.jsonl with the correct content.
	dst := filepath.Join(rawDir, "session.jsonl")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read rawDir/session.jsonl: %v", err)
	}
	if string(got) != content {
		t.Errorf("rawDir/session.jsonl content: got %q, want %q", got, content)
	}
}

// TestArchiveAdapter_Archive_MissingSrcPath_NoError verifies that a
// non-existent source path is a no-op (preserving existing tolerance for
// sessions where pi never produced output).
func TestArchiveAdapter_Archive_MissingSrcPath_NoError(t *testing.T) {
	rawDir := t.TempDir()
	a := pi.NewArchiveAdapter()

	err := a.Archive(context.Background(), "/nonexistent/pi/sessions/--tmp-test-foo--/session_xyz.jsonl", rawDir, harnessarchive.SourceParams{})
	if err != nil {
		t.Fatalf("Archive with missing src: expected nil error, got %v", err)
	}
}

// TestArchiveAdapter_Export_CopiesSessionJSONL verifies that Export produces
// archiveDir/session.jsonl from raw/session.jsonl.
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
		t.Errorf("session.jsonl content: got %q, want %q", content, got)
	}
}

// TestArchiveAdapter_Export_NoRawSessionJSONL_NoError verifies that Export
// returns nil when raw/session.jsonl is absent, and writes nothing.
func TestArchiveAdapter_Export_NoRawSessionJSONL_NoError(t *testing.T) {
	archiveDir := t.TempDir()
	rawDir := filepath.Join(archiveDir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}

	a := pi.NewArchiveAdapter()
	if err := a.Export(context.Background(), archiveDir, harnessarchive.SourceParams{}); err != nil {
		t.Fatalf("Export with missing session.jsonl: expected nil, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveDir, "session.jsonl")); !os.IsNotExist(err) {
		t.Error("expected session.jsonl NOT to be created when raw/session.jsonl is absent")
	}
}

// TestArchiveAdapter_SourcePath_SandboxExec verifies that when IsolationMode
// is "sandbox-exec", SourcePath uses the per-session staging HOME (not the
// real home directory). This is bug #1538 fix: sandbox-exec sessions write PI
// data under <stagingHome>/.pi/agent/sessions/<encoded-cwd>/, not ~/.pi/.
//
// The encoded-cwd is derived from the host worktree path because sandbox-exec
// mounts the worktree at its native path (only $HOME is remapped to the staging
// HOME inside the sandbox).
func TestArchiveAdapter_SourcePath_SandboxExec(t *testing.T) {
	// Redirect $HOME to a temp dir so that SandboxExecStagingHomePath (which
	// calls os.UserHomeDir internally) writes into a temp dir rather than the
	// real home directory or /homeless-shelter in the Nix sandbox CI build.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	const instanceID = "test-instance-uuid-1234"
	const sessionID = "d13cc856-f919-4ce9-b733-cf0e25493e62"
	const worktree = "/tmp/test-sandbox-worktree"

	// Compute where SandboxExecStagingHomePath will resolve, now that $HOME
	// points at fakeHome.
	stagingHome := filepath.Join(fakeHome, ".local", "state", "prism", "sessions", instanceID, "home")

	// Create the encoded-cwd directory inside the staging HOME and plant a
	// matching session file so SourcePath can find it.
	encodedDir := encodePiCWDForTest(worktree) // --tmp-test-sandbox-worktree--
	sessDir := filepath.Join(stagingHome, ".pi", "agent", "sessions", encodedDir)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	fileName := "2026-05-01T12-00-00-000Z_" + sessionID + ".jsonl"
	filePath := filepath.Join(sessDir, fileName)
	if err := os.WriteFile(filePath, []byte(`{"type":"session"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := pi.NewArchiveAdapter()
	p := harnessarchive.SourceParams{
		HarnessSessionID: sessionID,
		IsolationMode:    "sandbox-exec",
		InstanceID:       instanceID,
		Worktree:         worktree,
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath (sandbox-exec): %v", err)
	}
	if got != filePath {
		t.Errorf("SourcePath (sandbox-exec): got %q, want %q", got, filePath)
	}

	// Must NOT point into the real (fakeHome) .pi directory (only the staging
	// home inside fakeHome is acceptable).
	if strings.HasPrefix(got, filepath.Join(fakeHome, ".pi")) {
		t.Errorf("SourcePath (sandbox-exec) returned real home path %q; expected staging home", got)
	}
}

// TestArchiveAdapter_SourcePath_SandboxExec_EmptyInstanceID verifies that when
// IsolationMode is "sandbox-exec" but InstanceID is empty, SourcePath falls
// back to the real home directory rather than returning an error.
func TestArchiveAdapter_SourcePath_SandboxExec_EmptyInstanceID(t *testing.T) {
	a := pi.NewArchiveAdapter()
	home, _ := os.UserHomeDir()
	const worktree = "/tmp/test-fallback"
	const sessionID = "some-session-uuid"

	p := harnessarchive.SourceParams{
		HarnessSessionID: sessionID,
		IsolationMode:    "sandbox-exec",
		InstanceID:       "", // empty — forces fallback to host home
		Worktree:         worktree,
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath (sandbox-exec, empty InstanceID): %v", err)
	}
	// Fallback: resolves under the real home.
	encodedDir := encodePiCWDForTest(worktree)
	expectedPrefix := filepath.Join(home, ".pi", "agent", "sessions", encodedDir)
	if !strings.HasPrefix(got, expectedPrefix) {
		t.Errorf("SourcePath (sandbox-exec, empty InstanceID): got %q, expected prefix %q", got, expectedPrefix)
	}
}

// TestArchiveAdapter_SourcePath_NonSandboxExec_UsesRealHome verifies that
// non-sandbox-exec isolation modes (podman, bwrap, host) use the real home
// directory for the sessions root.
func TestArchiveAdapter_SourcePath_NonSandboxExec_UsesRealHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	const sessionID = "pi-ses-xyz-nonse"
	const worktree = "/tmp/test-non-sandbox"

	for _, mode := range []string{"podman", "bwrap", "host", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			a := pi.NewArchiveAdapter()
			p := harnessarchive.SourceParams{
				HarnessSessionID: sessionID,
				IsolationMode:    mode,
				InstanceID:       "some-instance-id",
				Worktree:         worktree,
			}
			got, err := a.SourcePath(p)
			if err != nil {
				t.Fatalf("SourcePath (mode=%q): %v", mode, err)
			}
			// Must be rooted in the real home, not a staging home.
			encodedDir := encodePiCWDForTest(worktree)
			expectedPrefix := filepath.Join(home, ".pi", "agent", "sessions", encodedDir)
			if !strings.HasPrefix(got, expectedPrefix) {
				t.Errorf("SourcePath (mode=%q): got %q, expected prefix %q", mode, got, expectedPrefix)
			}
		})
	}
}

// TestArchiveAdapter_EndToEnd_HostMode is an integration-style test that
// exercises the full Archive → Export pipeline with a realistic pi session
// file (encoded-cwd layout with <ts>_<uuid>.jsonl filename).
func TestArchiveAdapter_EndToEnd_HostMode(t *testing.T) {
	// Redirect $HOME to a temp dir to avoid writing to the real home
	// directory or /homeless-shelter in the Nix sandbox CI build.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	home := fakeHome

	const worktree = "/tmp/prism-e2e-test-worktree"
	const sessionID = "ffffffff-aaaa-bbbb-cccc-dddddddddddd"

	// Plant a pi session file at the real layout path.
	encodedDir := encodePiCWDForTest(worktree)
	sessDir := filepath.Join(home, ".pi", "agent", "sessions", encodedDir)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	sessionContent := `{"type":"session","version":3,"id":"` + sessionID + `"}` + "\n" +
		`{"type":"msg_user","content":"hello"}` + "\n"
	piFile := filepath.Join(sessDir, "2026-05-15T10-00-00-000Z_"+sessionID+".jsonl")
	if err := os.WriteFile(piFile, []byte(sessionContent), 0o600); err != nil {
		t.Fatal(err)
	}

	a := pi.NewArchiveAdapter()
	p := harnessarchive.SourceParams{
		HarnessSessionID: sessionID,
		Worktree:         worktree,
	}

	srcPath, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath: %v", err)
	}
	if srcPath != piFile {
		t.Errorf("SourcePath: got %q, want %q", srcPath, piFile)
	}

	archiveDir := t.TempDir()
	rawDir := filepath.Join(archiveDir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := a.Archive(context.Background(), srcPath, rawDir, p); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	rawJSONL := filepath.Join(rawDir, "session.jsonl")
	if _, err := os.Stat(rawJSONL); err != nil {
		t.Fatalf("raw/session.jsonl missing after Archive: %v", err)
	}

	if err := a.Export(context.Background(), archiveDir, p); err != nil {
		t.Fatalf("Export: %v", err)
	}

	exportedJSONL := filepath.Join(archiveDir, "session.jsonl")
	got, err := os.ReadFile(exportedJSONL)
	if err != nil {
		t.Fatalf("read exported session.jsonl: %v", err)
	}
	if string(got) != sessionContent {
		t.Errorf("exported session.jsonl: got %q, want %q", got, sessionContent)
	}
}
