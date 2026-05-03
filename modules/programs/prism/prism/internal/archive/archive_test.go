package archive

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// makeOpenCodeDB creates a minimal opencode-stable.db at dbPath with the given
// session, messages, and parts. Returns a map of relative raw/ paths → content
// that the caller can compare against the archive raw/ contents.
func makeOpenCodeDB(t *testing.T, dbPath, sessionID string) map[string]string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll for DB dir: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	defer db.Close()

	// Create minimal schema matching opencode-stable.db.
	_, err = db.Exec(`
		CREATE TABLE project (
			id TEXT PRIMARY KEY,
			worktree TEXT NOT NULL,
			vcs TEXT,
			name TEXT,
			icon_url TEXT,
			icon_color TEXT,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			time_initialized INTEGER,
			sandboxes TEXT NOT NULL,
			commands TEXT
		);
		CREATE TABLE session (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			parent_id TEXT,
			slug TEXT NOT NULL,
			directory TEXT NOT NULL,
			title TEXT NOT NULL,
			version TEXT NOT NULL,
			share_url TEXT,
			summary_additions INTEGER,
			summary_deletions INTEGER,
			summary_files INTEGER,
			summary_diffs TEXT,
			revert TEXT,
			permission TEXT,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			time_compacting INTEGER,
			time_archived INTEGER
		);
		CREATE TABLE message (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL
		);
		CREATE TABLE part (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}

	// Insert a project row (required by session FK).
	projectID := "proj_" + sessionID
	_, err = db.Exec(`INSERT INTO project (id, worktree, time_created, time_updated, sandboxes)
		VALUES (?, '/home/test', 1, 1, '[]')`, projectID)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	// Insert the session row.
	_, err = db.Exec(`INSERT INTO session
		(id, project_id, slug, directory, title, version, time_created, time_updated)
		VALUES (?, ?, 'test-session', '/home/test', 'Test Session', '1.0', 1000, 1000)`,
		sessionID, projectID)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Insert a message.
	msg1Content := `{"id":"msg_aaa","type":"user","role":"user"}`
	_, err = db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data)
		VALUES ('msg_aaa', ?, 1000, 1000, ?)`, sessionID, msg1Content)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	// Insert a part for that message.
	part1Content := `{"id":"prt_001","type":"text","text":"hello"}`
	_, err = db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
		VALUES ('prt_001', 'msg_aaa', ?, 1000, 1000, ?)`, sessionID, part1Content)
	if err != nil {
		t.Fatalf("insert part: %v", err)
	}

	return map[string]string{
		"messages/msg_aaa.json":      msg1Content,
		"parts/msg_aaa/prt_001.json": part1Content,
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(data)
}

func baseParams(archiveRoot, dbPath string) Params {
	startedAt := time.Date(2026, 4, 25, 14, 30, 22, 0, time.UTC)
	endedAt := time.Date(2026, 4, 25, 15, 47, 11, 0, time.UTC)
	return Params{
		InstanceID:       "a3f1c9e8-1234-5678-9abc-def012345678",
		SessionName:      "nixos-config@feature",
		AgentRole:        "worker",
		RootAgentName:    "worker",
		Harness:          "opencode",
		HarnessSessionID: "ses_test001",
		HarnessVersion:   "1.1.30",
		Repo:             "nixos-config",
		Worktree:         "/home/ben/code/nixos-config/feature",
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		EndState:         "finished",
		GroupID:          "",
		PrismVersion:     "abc123",
		IsolationMode:    "host",
		DBPath:           dbPath,
		ArchiveRoot:      archiveRoot,
	}
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestRunHappyPath verifies that a successful archive creates the correct
// directory layout with byte-for-byte identical file contents.
func TestRunHappyPath(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	sessionID := "ses_test001"
	wantFiles := makeOpenCodeDB(t, dbPath, sessionID)

	p := baseParams(archiveRoot, dbPath)

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Verify archive path format: <archiveRoot>/<repo>/<startedAtISO>_<instanceID>
	wantName := "20260425T143022Z_" + p.InstanceID
	if !strings.HasSuffix(archivePath, filepath.Join(p.Repo, wantName)) {
		t.Errorf("archivePath = %q, want suffix %q", archivePath, filepath.Join(p.Repo, wantName))
	}

	// Verify the archive directory exists.
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("archive dir not found: %v", err)
	}

	// Verify raw/ directory exists.
	rawDir := filepath.Join(archivePath, "raw")
	if _, err := os.Stat(rawDir); err != nil {
		t.Fatalf("raw/ dir not found: %v", err)
	}

	// Verify each file is present and byte-for-byte identical.
	for rel, want := range wantFiles {
		got := readFile(t, filepath.Join(rawDir, rel))
		if got != want {
			t.Errorf("raw/%s content = %q, want %q", rel, got, want)
		}
	}

	// Verify session.json is present and contains expected fields.
	sessData := readFile(t, filepath.Join(rawDir, "session.json"))
	var sessObj map[string]any
	if err := json.Unmarshal([]byte(sessData), &sessObj); err != nil {
		t.Fatalf("session.json parse error: %v", err)
	}
	if sessObj["id"] != sessionID {
		t.Errorf("session.json id = %v, want %q", sessObj["id"], sessionID)
	}

	// Verify manifest.json exists and parses.
	var m manifest
	mData := readFile(t, filepath.Join(archivePath, "manifest.json"))
	if err := json.Unmarshal([]byte(mData), &m); err != nil {
		t.Fatalf("manifest.json parse error: %v", err)
	}

	// Verify key manifest fields.
	if m.ArchiveVersion != ArchiveVersion {
		t.Errorf("manifest.archiveVersion = %d, want %d", m.ArchiveVersion, ArchiveVersion)
	}
	if m.PiMonoVersion != PiMonoVersion {
		t.Errorf("manifest.piMonoVersion = %d, want %d", m.PiMonoVersion, PiMonoVersion)
	}
	if m.InstanceID != p.InstanceID {
		t.Errorf("manifest.instanceId = %q, want %q", m.InstanceID, p.InstanceID)
	}
	if m.SessionName != p.SessionName {
		t.Errorf("manifest.sessionName = %q, want %q", m.SessionName, p.SessionName)
	}
	if m.Repo != p.Repo {
		t.Errorf("manifest.repo = %q, want %q", m.Repo, p.Repo)
	}
	if m.Worktree != p.Worktree {
		t.Errorf("manifest.worktree = %q, want %q", m.Worktree, p.Worktree)
	}
	if m.HarnessSessionID != p.HarnessSessionID {
		t.Errorf("manifest.harnessSessionId = %q, want %q", m.HarnessSessionID, p.HarnessSessionID)
	}
	if m.HarnessVersion != p.HarnessVersion {
		t.Errorf("manifest.harnessVersion = %q, want %q", m.HarnessVersion, p.HarnessVersion)
	}
	if m.EndState != p.EndState {
		t.Errorf("manifest.endState = %q, want %q", m.EndState, p.EndState)
	}
	if m.PrismVersion != p.PrismVersion {
		t.Errorf("manifest.prismVersion = %q, want %q", m.PrismVersion, p.PrismVersion)
	}
	if m.GroupID != nil {
		t.Errorf("manifest.groupId = %v, want nil", m.GroupID)
	}

	// Verify timestamps.
	wantStarted := p.StartedAt.UTC().Format(time.RFC3339)
	if m.StartedAt != wantStarted {
		t.Errorf("manifest.startedAt = %q, want %q", m.StartedAt, wantStarted)
	}
}

// TestRunNoHarnessSessionID verifies that a session with no harness_session_id
// still produces an archive dir with manifest.json and an empty raw/.
func TestRunNoHarnessSessionID(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	p := baseParams(archiveRoot, dbPath)
	p.HarnessSessionID = "" // opencode failed to start
	p.InstanceID = "cccccccc-1234-5678-9abc-def012345678"
	p.EndState = "interrupted"

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// raw/ must exist.
	rawDir := filepath.Join(archivePath, "raw")
	if _, err := os.Stat(rawDir); err != nil {
		t.Fatalf("raw/ dir not found: %v", err)
	}

	// raw/ must be empty.
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		t.Fatalf("ReadDir raw/: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("raw/ has %d entries, want 0", len(entries))
	}

	// manifest.json must be present with correct endState.
	var m manifest
	mData := readFile(t, filepath.Join(archivePath, "manifest.json"))
	if err := json.Unmarshal([]byte(mData), &m); err != nil {
		t.Fatalf("manifest.json parse error: %v", err)
	}
	if m.EndState != "interrupted" {
		t.Errorf("manifest.endState = %q, want %q", m.EndState, "interrupted")
	}
	if m.HarnessSessionID != "" {
		t.Errorf("manifest.harnessSessionId = %q, want empty", m.HarnessSessionID)
	}
}

// TestRunDBAbsent verifies that a missing opencode-stable.db is a graceful no-op
// (empty raw/, no error).
func TestRunDBAbsent(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "nonexistent-opencode-stable.db")

	p := baseParams(archiveRoot, dbPath)
	p.HarnessSessionID = "ses_ghost"
	p.InstanceID = "bbbbbbbb-1111-2222-3333-444444444444"

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error (want success): %v", err)
	}

	rawDir := filepath.Join(archivePath, "raw")
	if _, err := os.Stat(rawDir); err != nil {
		t.Fatalf("raw/ dir not found: %v", err)
	}
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		t.Fatalf("ReadDir raw/: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("raw/ has %d entries, want 0 (DB absent)", len(entries))
	}
}

// TestRunSessionIDAbsentFromDB verifies that a session ID not present in the DB
// is a graceful no-op (empty raw/, no error).
func TestRunSessionIDAbsentFromDB(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	// Create DB with a different session, not the one we'll archive.
	makeOpenCodeDB(t, dbPath, "ses_other001")

	p := baseParams(archiveRoot, dbPath)
	p.HarnessSessionID = "ses_nothere"
	p.InstanceID = "cccccccc-2222-3333-4444-555555555555"

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error (want success): %v", err)
	}

	rawDir := filepath.Join(archivePath, "raw")
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		t.Fatalf("ReadDir raw/: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("raw/ has %d entries, want 0 (session not in DB)", len(entries))
	}
}

// TestRunIdempotencyError verifies that calling Run a second time for the same
// instance returns an error and leaves the existing archive intact.
func TestRunIdempotencyError(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	sessionID := "ses_test003"
	makeOpenCodeDB(t, dbPath, sessionID)

	p := baseParams(archiveRoot, dbPath)
	p.HarnessSessionID = sessionID
	p.InstanceID = "dddddddd-1234-5678-9abc-def012345678"

	// First run — should succeed.
	firstPath, err := Run(p)
	if err != nil {
		t.Fatalf("first Run() error: %v", err)
	}

	// Write a sentinel file to verify the dir is not overwritten.
	sentinelPath := filepath.Join(firstPath, "sentinel.txt")
	if writeErr := os.WriteFile(sentinelPath, []byte("original"), 0o600); writeErr != nil {
		t.Fatalf("write sentinel: %v", writeErr)
	}

	// Second run — should fail with ErrAlreadyExists.
	_, err = Run(p)
	if err == nil {
		t.Fatal("second Run() succeeded, want error")
	}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("second Run() error = %v, want errors.Is(err, ErrAlreadyExists)", err)
	}

	// The existing directory must be intact.
	if _, statErr := os.Stat(sentinelPath); statErr != nil {
		t.Errorf("sentinel file missing after failed second run: %v", statErr)
	}
}

// TestRunCopyFailureAtomicity verifies that when temp dir creation fails (due to
// a bad archive root path), no partial directories are left behind.
func TestRunCopyFailureAtomicity(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	// Use a regular file as a directory component in the archive root path so
	// that MkdirAll fails with ENOTDIR — forcing Run() to return an error
	// before any temp directory is created.
	blockingFile := filepath.Join(tmpDir, "blocking-file")
	if err := os.WriteFile(blockingFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}
	badArchiveRoot := filepath.Join(blockingFile, "archive")

	p := baseParams(badArchiveRoot, dbPath)
	p.HarnessSessionID = "ses_test004"
	p.InstanceID = "eeeeeeee-1234-5678-9abc-def012345678"

	_, err := Run(p)
	if err == nil {
		t.Fatal("Run() with bad archiveRoot succeeded, want error")
	}

	// No temp dir should remain anywhere under our real tmpDir.
	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp dir left behind: %s", e.Name())
		}
	}
}

// TestFilePermissions verifies that archive dirs are 0700 and files are 0600.
func TestFilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	sessionID := "ses_perm001"
	makeOpenCodeDB(t, dbPath, sessionID)

	p := baseParams(archiveRoot, dbPath)
	p.HarnessSessionID = sessionID
	p.InstanceID = "ffffffff-1234-5678-9abc-def012345678"

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Check archive directory mode.
	dirInfo, err := os.Stat(archivePath)
	if err != nil {
		t.Fatalf("stat archive dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("archive dir mode = %04o, want %04o", dirInfo.Mode().Perm(), 0o700)
	}

	// Check a file mode.
	manifestInfo, err := os.Stat(filepath.Join(archivePath, "manifest.json"))
	if err != nil {
		t.Fatalf("stat manifest.json: %v", err)
	}
	if manifestInfo.Mode().Perm() != 0o600 {
		t.Errorf("manifest.json mode = %04o, want %04o", manifestInfo.Mode().Perm(), 0o600)
	}

	// Check raw/ directory mode.
	rawInfo, err := os.Stat(filepath.Join(archivePath, "raw"))
	if err != nil {
		t.Fatalf("stat raw/: %v", err)
	}
	if rawInfo.Mode().Perm() != 0o700 {
		t.Errorf("raw/ dir mode = %04o, want %04o", rawInfo.Mode().Perm(), 0o700)
	}

	// Check a raw file mode.
	sessFileInfo, err := os.Stat(filepath.Join(archivePath, "raw", "session.json"))
	if err != nil {
		t.Fatalf("stat raw/session.json: %v", err)
	}
	if sessFileInfo.Mode().Perm() != 0o600 {
		t.Errorf("raw/session.json mode = %04o, want %04o", sessFileInfo.Mode().Perm(), 0o600)
	}
}

// TestRunManifestVersions verifies archiveVersion=1 and piMonoVersion=3.
func TestRunManifestVersions(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	sessionID := "ses_ver001"
	makeOpenCodeDB(t, dbPath, sessionID)

	p := baseParams(archiveRoot, dbPath)
	p.HarnessSessionID = sessionID
	p.InstanceID = "11111111-2222-3333-4444-555555555555"

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	var m manifest
	mData := readFile(t, filepath.Join(archivePath, "manifest.json"))
	if err := json.Unmarshal([]byte(mData), &m); err != nil {
		t.Fatalf("manifest.json parse error: %v", err)
	}

	if m.ArchiveVersion != 1 {
		t.Errorf("archiveVersion = %d, want 1", m.ArchiveVersion)
	}
	if m.PiMonoVersion != 3 {
		t.Errorf("piMonoVersion = %d, want 3", m.PiMonoVersion)
	}
}

// TestRunGroupID verifies that a non-empty GroupID is written to
// manifest.groupId as a non-nil pointer.
func TestRunGroupID(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	sessionID := "ses_grp001"
	makeOpenCodeDB(t, dbPath, sessionID)

	p := baseParams(archiveRoot, dbPath)
	p.HarnessSessionID = sessionID
	p.InstanceID = "22222222-3333-4444-5555-666666666666"
	p.GroupID = "my-group-id"

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	var m manifest
	mData := readFile(t, filepath.Join(archivePath, "manifest.json"))
	if err := json.Unmarshal([]byte(mData), &m); err != nil {
		t.Fatalf("manifest.json parse error: %v", err)
	}

	if m.GroupID == nil {
		t.Fatal("manifest.groupId = nil, want non-nil")
	}
	if *m.GroupID != p.GroupID {
		t.Errorf("manifest.groupId = %q, want %q", *m.GroupID, p.GroupID)
	}
}

// TestRunSessionWithNoMessages verifies that a session with no messages produces
// an archive with raw/session.json and no raw/messages/ directory.
func TestRunSessionWithNoMessages(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	// Create DB with session but no messages.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	sessionID := "ses_nomsg"
	_, err = db.Exec(`
		CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT NOT NULL,
			vcs TEXT, name TEXT, icon_url TEXT, icon_color TEXT,
			time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL,
			time_initialized INTEGER, sandboxes TEXT NOT NULL, commands TEXT);
		CREATE TABLE session (id TEXT PRIMARY KEY, project_id TEXT NOT NULL,
			parent_id TEXT, slug TEXT NOT NULL, directory TEXT NOT NULL,
			title TEXT NOT NULL, version TEXT NOT NULL, share_url TEXT,
			summary_additions INTEGER, summary_deletions INTEGER,
			summary_files INTEGER, summary_diffs TEXT, revert TEXT,
			permission TEXT, time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL, time_compacting INTEGER,
			time_archived INTEGER);
		CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
		CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL,
			session_id TEXT NOT NULL, time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL, data TEXT NOT NULL);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	_, err = db.Exec(`INSERT INTO project (id, worktree, time_created, time_updated, sandboxes)
		VALUES ('proj_nomsg', '/home/test', 1, 1, '[]')`)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	_, err = db.Exec(`INSERT INTO session
		(id, project_id, slug, directory, title, version, time_created, time_updated)
		VALUES (?, 'proj_nomsg', 'test', '/home/test', 'Test', '1.0', 1, 1)`, sessionID)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	db.Close()

	p := baseParams(archiveRoot, dbPath)
	p.HarnessSessionID = sessionID
	p.InstanceID = "33333333-4444-5555-6666-777777777777"

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// session.json must be present.
	sessFilePath := filepath.Join(archivePath, "raw", "session.json")
	if _, err := os.Stat(sessFilePath); err != nil {
		t.Fatalf("raw/session.json not found: %v", err)
	}
	var sessObj map[string]any
	if err := json.Unmarshal([]byte(readFile(t, sessFilePath)), &sessObj); err != nil {
		t.Fatalf("parse session.json: %v", err)
	}
	if sessObj["id"] != sessionID {
		t.Errorf("session.json id = %v, want %q", sessObj["id"], sessionID)
	}

	// No messages/ dir should be created when there are no messages.
	msgDir := filepath.Join(archivePath, "raw", "messages")
	if _, err := os.Stat(msgDir); !os.IsNotExist(err) {
		t.Errorf("raw/messages/ should not exist for session with no messages, but: %v", err)
	}
}

// TestResolveDBPathPodman verifies that "podman" is now an unsupported mode and
// returns an error.
func TestResolveDBPathPodman(t *testing.T) {
	_, err := resolveDBPath("podman", "nixos-config@feature")
	if err == nil {
		t.Fatal("resolveDBPath podman: expected error for removed podman mode, got nil")
	}
}

// TestResolveDBPathHost verifies the host/bwrap/sandbox-exec path does not
// use prism-sessions.
func TestResolveDBPathHost(t *testing.T) {
	for _, mode := range []string{"host", "bwrap", "sandbox-exec"} {
		got, err := resolveDBPath(mode, "nixos-config@main")
		if err != nil {
			t.Fatalf("resolveDBPath %s: %v", mode, err)
		}
		if strings.Contains(got, "prism-sessions") {
			t.Errorf("mode=%s DB path = %q, should not contain 'prism-sessions'", mode, got)
		}
		if !strings.HasSuffix(got, "opencode-stable.db") {
			t.Errorf("mode=%s DB path = %q, want suffix 'opencode-stable.db'", mode, got)
		}
	}
}

// TestResolveStorageRootPodman verifies that "podman" is now an unsupported
// mode and returns an error.
func TestResolveStorageRootPodman(t *testing.T) {
	_, err := resolveStorageRoot("podman", "nixos-config@feature")
	if err == nil {
		t.Fatal("resolveStorageRoot podman: expected error for removed podman mode, got nil")
	}
}

// TestResolveStorageRootHost verifies the host/bwrap/sandbox-exec storage root.
func TestResolveStorageRootHost(t *testing.T) {
	for _, mode := range []string{"host", "bwrap", "sandbox-exec"} {
		got, err := resolveStorageRoot(mode, "nixos-config@main")
		if err != nil {
			t.Fatalf("resolveStorageRoot %s: %v", mode, err)
		}
		if strings.Contains(got, "prism-sessions") {
			t.Errorf("mode=%s storage root = %q, should not contain 'prism-sessions'", mode, got)
		}
		if !strings.HasSuffix(got, filepath.Join("opencode", "storage")) {
			t.Errorf("mode=%s storage root = %q, want suffix opencode/storage", mode, got)
		}
	}
}

// TestRunSandboxExec verifies that a session with isolation_mode "sandbox-exec"
// archives successfully.
func TestRunSandboxExec(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	sessionID := "ses_sbx001"
	makeOpenCodeDB(t, dbPath, sessionID)

	p := baseParams(archiveRoot, dbPath)
	p.HarnessSessionID = sessionID
	p.InstanceID = "44444444-5555-6666-7777-888888888888"
	p.IsolationMode = "sandbox-exec"

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() with sandbox-exec error: %v", err)
	}

	// Verify archive directory exists.
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("archive dir not found: %v", err)
	}

	// Verify raw/ directory exists.
	rawDir := filepath.Join(archivePath, "raw")
	if _, err := os.Stat(rawDir); err != nil {
		t.Fatalf("raw/ dir not found: %v", err)
	}

	// Verify session.json is present and valid.
	sessData := readFile(t, filepath.Join(rawDir, "session.json"))
	var sessObj map[string]any
	if err := json.Unmarshal([]byte(sessData), &sessObj); err != nil {
		t.Fatalf("session.json parse error: %v", err)
	}
	if sessObj["id"] != sessionID {
		t.Errorf("session.json id = %v, want %q", sessObj["id"], sessionID)
	}

	// Verify manifest.json parses correctly.
	var m manifest
	mData := readFile(t, filepath.Join(archivePath, "manifest.json"))
	if err := json.Unmarshal([]byte(mData), &m); err != nil {
		t.Fatalf("manifest.json parse error: %v", err)
	}
	if m.InstanceID != p.InstanceID {
		t.Errorf("manifest.instanceId = %q, want %q", m.InstanceID, p.InstanceID)
	}
}

// TestResolveDBPathUnknown verifies that an unsupported isolation mode returns
// an error.
func TestResolveDBPathUnknown(t *testing.T) {
	_, err := resolveDBPath("docker", "nixos-config@main")
	if err == nil {
		t.Fatal("resolveDBPath with unknown mode: expected error, got nil")
	}
}

// TestRunRepoTraversalRejected verifies that a p.Repo containing path traversal
// components is rejected before any filesystem operations.
func TestRunRepoTraversalRejected(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	cases := []string{
		"../../evil",
		"../up",
		"..",
		"good/sub", // slash inside repo name is also invalid
	}

	for _, repo := range cases {
		p := baseParams(archiveRoot, dbPath)
		p.Repo = repo
		p.InstanceID = "aaaa1111-1111-1111-1111-111111111111"

		_, err := Run(p)
		if err == nil {
			t.Errorf("Run() with repo=%q succeeded, want error", repo)
		}
	}
}

// TestRunMultipleMessages verifies that multiple messages and their parts are
// all exported correctly.
func TestRunMultipleMessages(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	dbPath := filepath.Join(tmpDir, "opencode-stable.db")

	// Build DB with two messages, each with a part.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	sessionID := "ses_multi01"
	_, err = db.Exec(`
		CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT NOT NULL,
			vcs TEXT, name TEXT, icon_url TEXT, icon_color TEXT,
			time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL,
			time_initialized INTEGER, sandboxes TEXT NOT NULL, commands TEXT);
		CREATE TABLE session (id TEXT PRIMARY KEY, project_id TEXT NOT NULL,
			parent_id TEXT, slug TEXT NOT NULL, directory TEXT NOT NULL,
			title TEXT NOT NULL, version TEXT NOT NULL, share_url TEXT,
			summary_additions INTEGER, summary_deletions INTEGER,
			summary_files INTEGER, summary_diffs TEXT, revert TEXT,
			permission TEXT, time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL, time_compacting INTEGER,
			time_archived INTEGER);
		CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
		CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL,
			session_id TEXT NOT NULL, time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL, data TEXT NOT NULL);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	_, err = db.Exec(`INSERT INTO project VALUES ('proj_multi', '/home/test', NULL, NULL, NULL, NULL, 1, 1, NULL, '[]', NULL)`)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	_, err = db.Exec(`INSERT INTO session VALUES (?, 'proj_multi', NULL, 'slug', '/home/test', 'T', '1.0', NULL, NULL, NULL, NULL, NULL, NULL, NULL, 1, 1, NULL, NULL)`, sessionID)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	msg1 := `{"id":"msg_111","role":"user"}`
	msg2 := `{"id":"msg_222","role":"assistant"}`
	part1 := `{"id":"prt_aaa","type":"text","text":"hello"}`
	part2 := `{"id":"prt_bbb","type":"text","text":"world"}`

	for _, row := range []struct{ id, data string }{
		{"msg_111", msg1}, {"msg_222", msg2},
	} {
		if _, err := db.Exec(`INSERT INTO message VALUES (?, ?, 1, 1, ?)`, row.id, sessionID, row.data); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO part VALUES ('prt_aaa', 'msg_111', ?, 1, 1, ?)`, sessionID, part1); err != nil {
		t.Fatalf("insert part1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO part VALUES ('prt_bbb', 'msg_222', ?, 1, 1, ?)`, sessionID, part2); err != nil {
		t.Fatalf("insert part2: %v", err)
	}
	db.Close()

	p := baseParams(archiveRoot, dbPath)
	p.HarnessSessionID = sessionID
	p.InstanceID = "55555555-6666-7777-8888-999999999999"

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	rawDir := filepath.Join(archivePath, "raw")

	// Check both messages exist.
	if got := readFile(t, filepath.Join(rawDir, "messages", "msg_111.json")); got != msg1 {
		t.Errorf("messages/msg_111.json = %q, want %q", got, msg1)
	}
	if got := readFile(t, filepath.Join(rawDir, "messages", "msg_222.json")); got != msg2 {
		t.Errorf("messages/msg_222.json = %q, want %q", got, msg2)
	}

	// Check both parts exist in their respective dirs.
	if got := readFile(t, filepath.Join(rawDir, "parts", "msg_111", "prt_aaa.json")); got != part1 {
		t.Errorf("parts/msg_111/prt_aaa.json = %q, want %q", got, part1)
	}
	if got := readFile(t, filepath.Join(rawDir, "parts", "msg_222", "prt_bbb.json")); got != part2 {
		t.Errorf("parts/msg_222/prt_bbb.json = %q, want %q", got, part2)
	}
}

// TestResolvePathsStorageRootDerived verifies that when StorageRoot is set (as
// cleanup.go does for podman sessions via ArchivePaths), the DB path is derived
// as the parent of storage/ plus opencode-stable.db.
func TestResolvePathsStorageRootDerived(t *testing.T) {
	tmpDir := t.TempDir()
	// Simulate a podman-style storage root: .../prism-sessions/<container>/storage
	storageRoot := filepath.Join(tmpDir, "prism-sessions", "prism-myrepo-feat", "storage")

	p := Params{
		Repo:          "myrepo",
		IsolationMode: "podman",
		StorageRoot:   storageRoot,
		ArchiveRoot:   filepath.Join(tmpDir, "archive"),
		InstanceID:    "aaaaaaaa-0000-0000-0000-000000000000",
		StartedAt:     time.Now(),
		EndedAt:       time.Now(),
	}
	_, dbPath, err := resolvePaths(p)
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	wantDB := filepath.Join(tmpDir, "prism-sessions", "prism-myrepo-feat", "opencode-stable.db")
	if dbPath != wantDB {
		t.Errorf("dbPath = %q, want %q", dbPath, wantDB)
	}
}
