package pi_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	harnessarchive "github.com/prismatic-koi/prism/internal/harness/archive"
	"github.com/prismatic-koi/prism/internal/harness/pi"
)

// encodePiCWDForTest mirrors the encodePiCWD logic in archive.go for use in
// test assertions without exporting the function.
func encodePiCWDForTest(cwd string) string {
	stripped := strings.TrimLeft(cwd, "/\\")
	r := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return "--" + r.Replace(stripped) + "--"
}

// TestEncodePiCWD verifies the encoding formula matches pi 0.75.3
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
	// Redirect $HOME so the test does not touch the real home or fail under
	// /homeless-shelter in the Nix sandbox CI build.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	a := pi.NewArchiveAdapter()
	home := fakeHome

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
	// Redirect $HOME so the test does not touch the real home or fail under
	// /homeless-shelter in the Nix sandbox CI build.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	a := pi.NewArchiveAdapter()
	home := fakeHome

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
	// Redirect $HOME so the test does not touch the real home or fail under
	// /homeless-shelter in the Nix sandbox CI build.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	a := pi.NewArchiveAdapter()
	home := fakeHome
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
	// Redirect $HOME so the test does not touch the real home or fail under
	// /homeless-shelter in the Nix sandbox CI build.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	a := pi.NewArchiveAdapter()
	home := fakeHome
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
// non-sandbox-exec, non-bwrap isolation modes (podman, host, "") use the real
// home directory for the sessions root. The bwrap branch is covered
// separately by TestArchiveAdapter_SourcePath_Bwrap* below.
func TestArchiveAdapter_SourcePath_NonSandboxExec_UsesRealHome(t *testing.T) {
	// Redirect $HOME so the test does not touch the real home or fail under
	// /homeless-shelter in the Nix sandbox CI build.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	home := fakeHome
	const sessionID = "pi-ses-xyz-nonse"
	const worktree = "/tmp/test-non-sandbox"

	for _, mode := range []string{"podman", "host", ""} {
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

// stageBwrapSession writes a fake pi JSONL file at the post-#1985 bwrap
// layout path — the host's global ~/.pi/agent/sessions/ tree, same as host
// mode — and returns (filePath, content). It assumes $HOME has already been
// redirected by the caller (t.Setenv("HOME", t.TempDir())).
//
// Pre-#1985 this helper planted files under
// <XDG_STATE_HOME>/prism/run/<dirHash>/pi-agent/sessions/. That staging-dir
// layout was torn down with the prism session, taking the per-cwd history
// with it. The host-side path is now identical to host mode.
func stageBwrapSession(t *testing.T, _ /*stateHome*/, _ /*sessionName*/, worktree, harnessSessionID string) (string, string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatalf("stageBwrapSession: resolve HOME: %v", err)
	}
	encodedCWD := encodePiCWDForTest(worktree)
	sessDir := filepath.Join(home, ".pi", "agent", "sessions", encodedCWD)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	filePath := filepath.Join(sessDir, "2026-05-20T07-27-30-806Z_"+harnessSessionID+".jsonl")
	content := `{"type":"session","version":3,"id":"` + harnessSessionID + `"}` + "\n" +
		`{"type":"msg_user","content":"hello bwrap"}` + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return filePath, content
}

// TestArchiveAdapter_SourcePath_Bwrap_FindsMatchingFile verifies that under
// bwrap mode, SourcePath locates the JSONL inside
// <home>/.pi/agent/sessions/<encoded-cwd>/ and returns the matching file
// path. Post-#1985 the bwrap branch resolves to the same host-global path as
// host mode (the sandbox overlays it onto $PI_CODING_AGENT_DIR/sessions/).
func TestArchiveAdapter_SourcePath_Bwrap_FindsMatchingFile(t *testing.T) {
	// Redirect both $HOME and $XDG_STATE_HOME so no real-host paths are
	// touched (works in both dev shell and Nix sandbox).
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	const sessionName = "nixos-config@bwrap-test"
	const worktree = "/tmp/test-bwrap-worktree"
	const harnessSessionID = "ses_01HQXY7Z8ABCDEFG"

	wantPath, _ := stageBwrapSession(t, stateHome, sessionName, worktree, harnessSessionID)

	a := pi.NewArchiveAdapter()
	got, err := a.SourcePath(harnessarchive.SourceParams{
		SessionName:      sessionName,
		IsolationMode:    "bwrap",
		HarnessSessionID: harnessSessionID,
		Worktree:         worktree,
	})
	if err != nil {
		t.Fatalf("SourcePath: %v", err)
	}
	if got != wantPath {
		t.Errorf("SourcePath (bwrap): got %q, want %q", got, wantPath)
	}

	// The path must be under the host-global ~/.pi/agent/sessions/<encoded-cwd>/
	// layout — the same root host mode uses.
	wantPrefix := filepath.Join(
		fakeHome, ".pi", "agent", "sessions",
		encodePiCWDForTest(worktree),
	)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("SourcePath (bwrap): got %q, expected prefix %q", got, wantPrefix)
	}
	// Defensive: must NOT point under the old per-session staging dir.
	oldStaging := filepath.Join(stateHome, "prism", "run")
	if strings.HasPrefix(got, oldStaging) {
		t.Errorf("SourcePath (bwrap) %q must not point under the pre-#1985 per-session staging dir %q",
			got, oldStaging)
	}
}

// TestArchiveAdapter_Archive_Bwrap_EndToEnd verifies that for a staged bwrap
// session, composing SourcePath then Archive produces rawDir/session.jsonl
// byte-identical to the source file (AC #8).
func TestArchiveAdapter_Archive_Bwrap_EndToEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	const sessionName = "nixos-config@bwrap-e2e"
	const worktree = "/tmp/test-bwrap-e2e-worktree"
	const harnessSessionID = "ses_e2e_01HQXY7Z9ABCDEFG"

	srcPath, content := stageBwrapSession(t, stateHome, sessionName, worktree, harnessSessionID)

	a := pi.NewArchiveAdapter()
	p := harnessarchive.SourceParams{
		SessionName:      sessionName,
		IsolationMode:    "bwrap",
		HarnessSessionID: harnessSessionID,
		Worktree:         worktree,
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath: %v", err)
	}
	if got != srcPath {
		t.Fatalf("SourcePath: got %q, want %q", got, srcPath)
	}

	rawDir := t.TempDir()
	if err := a.Archive(context.Background(), got, rawDir, p); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	dst := filepath.Join(rawDir, "session.jsonl")
	gotContent, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read rawDir/session.jsonl: %v", err)
	}
	if string(gotContent) != content {
		t.Errorf("rawDir/session.jsonl content mismatch:\n got: %q\nwant: %q", gotContent, content)
	}
}

// TestArchiveAdapter_SourcePath_Bwrap_EmptySessionName verifies that when
// IsolationMode is "bwrap" but SessionName is empty, SourcePath still
// resolves (post-#1985 the bwrap branch no longer needs the SessionName —
// it resolves to ~/.pi/agent/sessions/ like host mode). Archive on the
// resulting sentinel must still be a no-op when no matching transcript
// exists, matching the contract for the other modes.
func TestArchiveAdapter_SourcePath_Bwrap_EmptySessionName(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	a := pi.NewArchiveAdapter()
	p := harnessarchive.SourceParams{
		SessionName:      "", // empty — post-#1985 bwrap no longer needs dirHash
		IsolationMode:    "bwrap",
		HarnessSessionID: "ses_01HQXY",
		Worktree:         "/tmp/test-no-session-name",
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath (bwrap empty SessionName): %v", err)
	}

	// Archive on that sentinel must be a no-op (no matching transcript exists).
	rawDir := t.TempDir()
	if err := a.Archive(context.Background(), got, rawDir, p); err != nil {
		t.Fatalf("Archive on bwrap-empty-SessionName sentinel: %v", err)
	}
	entries, _ := os.ReadDir(rawDir)
	if len(entries) != 0 {
		t.Errorf("rawDir must be empty after no-op Archive; got %d entry/entries", len(entries))
	}
}

// TestArchiveAdapter_SourcePath_Bwrap_NoMatchingFile verifies that when the
// bwrap encoded-cwd directory exists but contains no file matching
// *_<HarnessSessionID>.jsonl, SourcePath returns a sentinel path inside the
// encoded-cwd dir and Archive on that sentinel is a no-op.
func TestArchiveAdapter_SourcePath_Bwrap_NoMatchingFile(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "nixos-config@bwrap-nomatch"
	const worktree = "/tmp/test-bwrap-nomatch-worktree"
	const harnessSessionID = "ses_NOTPRESENT"

	// Post-#1985: bwrap pi sessions write to the host's ~/.pi/agent/sessions/
	// tree. Create the encoded-cwd dir there, with a non-matching transcript.
	encodedCWD := encodePiCWDForTest(worktree)
	sessDir := filepath.Join(fakeHome, ".pi", "agent", "sessions", encodedCWD)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(sessDir, "2026-05-20T07-27-30-806Z_some-other-id.jsonl"),
		[]byte(`{"type":"session"}`+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	a := pi.NewArchiveAdapter()
	p := harnessarchive.SourceParams{
		SessionName:      sessionName,
		IsolationMode:    "bwrap",
		HarnessSessionID: harnessSessionID,
		Worktree:         worktree,
	}
	got, err := a.SourcePath(p)
	if err != nil {
		t.Fatalf("SourcePath (bwrap no match): %v", err)
	}
	// The sentinel must live inside the encoded-cwd dir (so future Archive
	// calls land on a non-existent file path under it).
	if !strings.HasPrefix(got, sessDir+string(filepath.Separator)) {
		t.Errorf("SourcePath sentinel must be inside %q; got %q", sessDir, got)
	}
	// Archive on the sentinel is a no-op (the file does not exist).
	rawDir := t.TempDir()
	if err := a.Archive(context.Background(), got, rawDir, p); err != nil {
		t.Fatalf("Archive on bwrap-no-match sentinel: %v", err)
	}
	entries, _ := os.ReadDir(rawDir)
	if len(entries) != 0 {
		t.Errorf("rawDir must be empty after no-op Archive; got %d entry/entries", len(entries))
	}
}

// TestArchiveAdapter_SourcePath_CrossMode_NoContamination verifies that
// SourcePath produces paths anchored in the right per-mode root:
//
//   - host and bwrap collapse to the SAME ~/.pi/agent/sessions/ root
//     (post-#1985, bwrap overlays this dir onto the sandbox path so the
//     host-side resolution is identical).
//   - sandbox-exec is anchored in the per-instance staging HOME and must
//     NOT collide with the host/bwrap path.
func TestArchiveAdapter_SourcePath_CrossMode_NoContamination(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	const sessionName = "nixos-config@cross-mode"
	const worktree = "/tmp/test-cross-mode"
	const harnessSessionID = "ses_CROSS"
	const instanceID = "inst-cross-1234"

	a := pi.NewArchiveAdapter()
	base := harnessarchive.SourceParams{
		SessionName:      sessionName,
		HarnessSessionID: harnessSessionID,
		Worktree:         worktree,
		InstanceID:       instanceID,
	}

	hostParams := base
	hostParams.IsolationMode = "host"
	hostPath, err := a.SourcePath(hostParams)
	if err != nil {
		t.Fatalf("SourcePath (host): %v", err)
	}

	bwrapParams := base
	bwrapParams.IsolationMode = "bwrap"
	bwrapPath, err := a.SourcePath(bwrapParams)
	if err != nil {
		t.Fatalf("SourcePath (bwrap): %v", err)
	}

	sandboxParams := base
	sandboxParams.IsolationMode = "sandbox-exec"
	sandboxPath, err := a.SourcePath(sandboxParams)
	if err != nil {
		t.Fatalf("SourcePath (sandbox-exec): %v", err)
	}

	// host and bwrap now collapse to the same root (#1985). sandbox-exec
	// must still differ.
	if hostPath != bwrapPath {
		t.Errorf("host and bwrap should collapse to the same root post-#1985; host=%q bwrap=%q",
			hostPath, bwrapPath)
	}
	if hostPath == sandboxPath {
		t.Errorf("host and sandbox-exec returned the same path: %q", hostPath)
	}
	if bwrapPath == sandboxPath {
		t.Errorf("bwrap and sandbox-exec returned the same path: %q", bwrapPath)
	}

	// host: under <fakeHome>/.pi/agent/sessions/
	hostPrefix := filepath.Join(fakeHome, ".pi", "agent", "sessions")
	if !strings.HasPrefix(hostPath, hostPrefix) {
		t.Errorf("host path %q does not have prefix %q", hostPath, hostPrefix)
	}

	// bwrap: also under <fakeHome>/.pi/agent/sessions/ (post-#1985).
	// MUST NOT point under the pre-#1985 per-prism-session staging dir.
	if !strings.HasPrefix(bwrapPath, hostPrefix) {
		t.Errorf("bwrap path %q does not have prefix %q (post-#1985 collapses to host root)",
			bwrapPath, hostPrefix)
	}
	oldBwrapPrefix := filepath.Join(stateHome, "prism", "run")
	if strings.HasPrefix(bwrapPath, oldBwrapPrefix) {
		t.Errorf("bwrap path %q must not point under pre-#1985 staging dir %q",
			bwrapPath, oldBwrapPrefix)
	}
	// sandbox-exec: under <fakeHome>/.local/state/prism/sessions/<instanceID>/home/.pi/agent/sessions/
	sandboxPrefix := filepath.Join(
		fakeHome, ".local", "state", "prism", "sessions", instanceID, "home",
		".pi", "agent", "sessions",
	)
	if !strings.HasPrefix(sandboxPath, sandboxPrefix) {
		t.Errorf("sandbox-exec path %q does not have prefix %q", sandboxPath, sandboxPrefix)
	}
}
