package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// makeStorageTree creates a minimal opencode storage tree under root for the
// given session ID and project ID. Returns a map of relative paths → content
// that the caller can compare against the archive raw/ contents.
func makeStorageTree(t *testing.T, root, projectID, sessionID string) map[string]string {
	t.Helper()

	// storage/session/<projectID>/ses_<id>.json
	sessDir := filepath.Join(root, "session", projectID)
	mustMkdir(t, sessDir)
	sessContent := `{"id":"` + sessionID + `","projectID":"` + projectID + `"}`
	mustWriteFile(t, filepath.Join(sessDir, sessionID+".json"), sessContent)

	// storage/message/<sessionID>/msg_aaa.json
	msgDir := filepath.Join(root, "message", sessionID)
	mustMkdir(t, msgDir)
	msg1Content := `{"id":"msg_aaa","type":"text"}`
	mustWriteFile(t, filepath.Join(msgDir, "msg_aaa.json"), msg1Content)

	// storage/part/msg_aaa/prt_001.json (text part — no tool-output ref)
	partDir := filepath.Join(root, "part", "msg_aaa")
	mustMkdir(t, partDir)
	part1Content := `{"id":"prt_001","type":"text","text":"hello"}`
	mustWriteFile(t, filepath.Join(partDir, "prt_001.json"), part1Content)

	return map[string]string{
		"session.json":              sessContent,
		"messages/msg_aaa.json":     msg1Content,
		"parts/msg_aaa/prt_001.json": part1Content,
	}
}

// makeStorageTreeWithToolOutput extends makeStorageTree with a tool part that
// references a tool-output sidecar file. Returns the file map and toolID.
func makeStorageTreeWithToolOutput(t *testing.T, root, projectID, sessionID string) (map[string]string, string) {
	t.Helper()
	files := makeStorageTree(t, root, projectID, sessionID)

	toolID := "tool_abc123"
	toolContent := "tool output bytes"

	// Add a second part in msg_aaa that references the tool output.
	partDir := filepath.Join(root, "part", "msg_aaa")
	part2Content := `{"id":"prt_002","type":"tool","asset":"` + toolID + `"}`
	mustWriteFile(t, filepath.Join(partDir, "prt_002.json"), part2Content)
	files["parts/msg_aaa/prt_002.json"] = part2Content

	// Write the tool-output sidecar.
	toDir := filepath.Join(root, "tool-output")
	mustMkdir(t, toDir)
	mustWriteFile(t, filepath.Join(toDir, toolID), toolContent)
	files["tool-output/"+toolID] = toolContent

	return files, toolID
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
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

func baseParams(archiveRoot, storageRoot string) Params {
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
		StorageRoot:      storageRoot,
		ArchiveRoot:      archiveRoot,
	}
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestRunHappyPath verifies that a successful archive creates the correct
// directory layout with byte-for-byte identical file contents.
func TestRunHappyPath(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	storageRoot := filepath.Join(tmpDir, "storage")

	projectID := "proj123"
	sessionID := "ses_test001"
	wantFiles := makeStorageTree(t, storageRoot, projectID, sessionID)

	p := baseParams(archiveRoot, storageRoot)

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

// TestRunWithToolOutput verifies that tool-output sidecar files referenced by
// parts are copied into raw/tool-output/.
func TestRunWithToolOutput(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	storageRoot := filepath.Join(tmpDir, "storage")

	projectID := "proj456"
	sessionID := "ses_test002"
	wantFiles, toolID := makeStorageTreeWithToolOutput(t, storageRoot, projectID, sessionID)

	p := baseParams(archiveRoot, storageRoot)
	p.HarnessSessionID = sessionID
	p.InstanceID = "bbbbbbbb-1234-5678-9abc-def012345678"

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	rawDir := filepath.Join(archivePath, "raw")
	for rel, want := range wantFiles {
		got := readFile(t, filepath.Join(rawDir, rel))
		if got != want {
			t.Errorf("raw/%s content = %q, want %q", rel, got, want)
		}
	}

	// Verify the tool-output file is present.
	toolPath := filepath.Join(rawDir, "tool-output", toolID)
	if _, err := os.Stat(toolPath); err != nil {
		t.Errorf("tool-output/%s not found: %v", toolID, err)
	}
}

// TestRunNoHarnessSessionID verifies that a session with no harness_session_id
// still produces an archive dir with manifest.json and an empty raw/.
func TestRunNoHarnessSessionID(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	storageRoot := filepath.Join(tmpDir, "storage")

	p := baseParams(archiveRoot, storageRoot)
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

// TestRunIdempotencyError verifies that calling Run a second time for the same
// instance returns an error and leaves the existing archive intact.
func TestRunIdempotencyError(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	storageRoot := filepath.Join(tmpDir, "storage")

	projectID := "proj789"
	sessionID := "ses_test003"
	makeStorageTree(t, storageRoot, projectID, sessionID)

	p := baseParams(archiveRoot, storageRoot)
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

// TestRunCopyFailureAtomicity verifies that a copy failure leaves no partial
// archive directory (temp dir is cleaned up).
func TestRunCopyFailureAtomicity(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	// Point storageRoot at a directory that exists but has no session files.
	storageRoot := filepath.Join(tmpDir, "empty-storage")
	if err := os.MkdirAll(storageRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll storageRoot: %v", err)
	}
	// Create the session base dir but NO project subdir, so findSessionFile fails.
	if err := os.MkdirAll(filepath.Join(storageRoot, "session"), 0o755); err != nil {
		t.Fatalf("MkdirAll session dir: %v", err)
	}

	p := baseParams(archiveRoot, storageRoot)
	p.HarnessSessionID = "ses_missing"
	p.InstanceID = "eeeeeeee-1234-5678-9abc-def012345678"

	_, err := Run(p)
	if err == nil {
		t.Fatal("Run() succeeded, want error (session file not found)")
	}

	// No temp dir should remain.
	repoDir := filepath.Join(archiveRoot, p.Repo)
	entries, _ := os.ReadDir(repoDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp dir left behind: %s", e.Name())
		}
	}

	// The final directory must not exist either.
	dirName := p.StartedAt.UTC().Format("20060102T150405Z") + "_" + p.InstanceID
	if _, statErr := os.Stat(filepath.Join(repoDir, dirName)); statErr == nil {
		t.Errorf("final archive dir exists after failed Run()")
	}
}

// TestFilePermissions verifies that archive dirs are 0700 and files are 0600.
func TestFilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	storageRoot := filepath.Join(tmpDir, "storage")

	projectID := "permproj"
	sessionID := "ses_perm001"
	makeStorageTree(t, storageRoot, projectID, sessionID)

	p := baseParams(archiveRoot, storageRoot)
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
	storageRoot := filepath.Join(tmpDir, "storage")

	projectID := "versionproj"
	sessionID := "ses_ver001"
	makeStorageTree(t, storageRoot, projectID, sessionID)

	p := baseParams(archiveRoot, storageRoot)
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
	storageRoot := filepath.Join(tmpDir, "storage")

	projectID := "groupproj"
	sessionID := "ses_grp001"
	makeStorageTree(t, storageRoot, projectID, sessionID)

	p := baseParams(archiveRoot, storageRoot)
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

// TestRunSessionWithNoMessages verifies that a session with a session.json but
// no messages produces an archive with raw/session.json and raw/messages/ empty.
func TestRunSessionWithNoMessages(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	storageRoot := filepath.Join(tmpDir, "storage")

	projectID := "nomsgproj"
	sessionID := "ses_nomsg"
	// Only create the session.json, no message/ dir.
	sessDir := filepath.Join(storageRoot, "session", projectID)
	mustMkdir(t, sessDir)
	sessContent := `{"id":"` + sessionID + `","projectID":"` + projectID + `"}`
	mustWriteFile(t, filepath.Join(sessDir, sessionID+".json"), sessContent)

	p := baseParams(archiveRoot, storageRoot)
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
	if got := readFile(t, sessFilePath); got != sessContent {
		t.Errorf("raw/session.json = %q, want %q", got, sessContent)
	}

	// messages/ must exist and be empty.
	msgDir := filepath.Join(archivePath, "raw", "messages")
	if _, err := os.Stat(msgDir); err != nil {
		t.Fatalf("raw/messages/ not found: %v", err)
	}
	entries, err := os.ReadDir(msgDir)
	if err != nil {
		t.Fatalf("ReadDir raw/messages/: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("raw/messages/ has %d entries, want 0", len(entries))
	}
}

// TestResolveStorageRootPodman verifies the podman path uses the container name.
func TestResolveStorageRootPodman(t *testing.T) {
	// We don't want to call os.UserHomeDir in a way that makes the test
	// platform-dependent — just verify the function doesn't return an error
	// and includes "prism-sessions" in the path.
	got, err := resolveStorageRoot("podman", "nixos-config@feature")
	if err != nil {
		t.Fatalf("resolveStorageRoot podman: %v", err)
	}
	if !strings.Contains(got, "prism-sessions") {
		t.Errorf("podman storage root = %q, want path containing 'prism-sessions'", got)
	}
	if !strings.Contains(got, "prism-nixos-config-feature") {
		t.Errorf("podman storage root = %q, want path containing container name 'prism-nixos-config-feature'", got)
	}
}

// TestResolveStorageRootHost verifies the host/bwrap path does not use
// prism-sessions.
func TestResolveStorageRootHost(t *testing.T) {
	for _, mode := range []string{"host", "bwrap"} {
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

// TestResolveStorageRootUnknown verifies that an unsupported isolation mode
// returns an error.
func TestResolveStorageRootUnknown(t *testing.T) {
	_, err := resolveStorageRoot("docker", "nixos-config@main")
	if err == nil {
		t.Fatal("resolveStorageRoot with unknown mode: expected error, got nil")
	}
}

// TestRunRepoTraversalRejected verifies that a p.Repo containing path traversal
// components is rejected before any filesystem operations.
func TestRunRepoTraversalRejected(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")
	storageRoot := filepath.Join(tmpDir, "storage")

	cases := []string{
		"../../evil",
		"../up",
		"..",
		"good/sub", // slash inside repo name is also invalid
	}

	for _, repo := range cases {
		p := baseParams(archiveRoot, storageRoot)
		p.Repo = repo
		p.InstanceID = "aaaa1111-1111-1111-1111-111111111111"

		_, err := Run(p)
		if err == nil {
			t.Errorf("Run() with repo=%q succeeded, want error", repo)
		}
	}
}

// TestToolOutputIDValidation verifies that toolOutputIDsFromPart rejects
// asset values containing path traversal sequences.
func TestToolOutputIDValidation(t *testing.T) {
	tmpDir := t.TempDir()

	cases := []struct {
		partContent string
		wantIDs     []string
	}{
		// Normal tool part.
		{`{"type":"tool","asset":"tool_abc123"}`, []string{"tool_abc123"}},
		// Traversal attempt — must be rejected.
		{`{"type":"tool","asset":"tool_/../../etc/shadow"}`, nil},
		{`{"type":"tool","asset":"tool_../secret"}`, nil},
		// Wrong prefix — ignored.
		{`{"type":"tool","asset":"output_abc"}`, nil},
		// Wrong type — ignored.
		{`{"type":"text","asset":"tool_abc"}`, nil},
	}

	for i, tc := range cases {
		partPath := filepath.Join(tmpDir, fmt.Sprintf("prt_%02d.json", i))
		mustWriteFile(t, partPath, tc.partContent)
		got := toolOutputIDsFromPart(partPath)
		if len(got) != len(tc.wantIDs) {
			t.Errorf("case %d (%s): got %v, want %v", i, tc.partContent, got, tc.wantIDs)
			continue
		}
		for j := range got {
			if got[j] != tc.wantIDs[j] {
				t.Errorf("case %d: got[%d] = %q, want %q", i, j, got[j], tc.wantIDs[j])
			}
		}
	}
}

// TestContainerNameForSession verifies the container name mapping matches
// the logic in internal/container.NameForSession.
func TestContainerNameForSession(t *testing.T) {
	cases := []struct {
		sessionName   string
		wantContainer string
	}{
		{"nixos-config@feature", "prism-nixos-config-feature"},
		{"nixos-config@main~review-1-review-code", "prism-nixos-config-main-review-1-review-code"},
		{"my.repo@feat/slash", "prism-my-repo-feat-slash"},
	}
	for _, tc := range cases {
		got := containerNameForSession(tc.sessionName)
		if got != tc.wantContainer {
			t.Errorf("containerNameForSession(%q) = %q, want %q", tc.sessionName, got, tc.wantContainer)
		}
	}
}
