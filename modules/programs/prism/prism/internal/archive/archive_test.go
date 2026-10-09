package archive

import (
	"context"
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(data)
}

// noopCopier is a Copier that does nothing — simulates a session with no files.
func noopCopier(_ context.Context, _ string) error { return nil }

// fileCopier returns a Copier that writes the given filename→content pairs
// into the archive directory it receives. Post-fix the Copier receives the
// per-session archive directory itself (no `raw/` subdir).
func fileCopier(files map[string]string) func(ctx context.Context, archiveDir string) error {
	return func(_ context.Context, archiveDir string) error {
		for rel, content := range files {
			dst := filepath.Join(archiveDir, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
			}
			if err := os.WriteFile(dst, []byte(content), 0o600); err != nil {
				return fmt.Errorf("write %s: %w", dst, err)
			}
		}
		return nil
	}
}

func baseParams(archiveRoot string) Params {
	startedAt := time.Date(2026, 4, 25, 14, 30, 22, 0, time.UTC)
	endedAt := time.Date(2026, 4, 25, 15, 47, 11, 0, time.UTC)
	return Params{
		InstanceID:       "a3f1c9e8-1234-5678-9abc-def012345678",
		SessionName:      "nixos-config@feature",
		AgentRole:        "worker",
		RootAgentName:    "worker",
		Harness:          "pi",
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
		ArchiveRoot:      archiveRoot,
		Copier:           noopCopier,
	}
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestRunHappyPath verifies that a successful archive creates the correct
// directory layout with byte-for-byte identical file contents.
func TestRunHappyPath(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	wantFiles := map[string]string{
		"session.jsonl": `{"id":"turn_001","role":"user","text":"hello"}` + "\n",
	}

	p := baseParams(archiveRoot)
	p.Copier = fileCopier(wantFiles)

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

	// Post-fix layout: no raw/ subdirectory. Verify it does NOT exist.
	if _, statErr := os.Stat(filepath.Join(archivePath, "raw")); !os.IsNotExist(statErr) {
		t.Errorf("archive dir must not contain raw/ post-fix; stat returned: %v", statErr)
	}

	// Verify each file is present (directly in the archive dir, no raw/ prefix)
	// and byte-for-byte identical.
	for rel, want := range wantFiles {
		got := readFile(t, filepath.Join(archivePath, rel))
		if got != want {
			t.Errorf("%s content = %q, want %q", rel, got, want)
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

// TestRunNoHarnessSessionID verifies that a session with no harness_session_id
// still produces an archive dir with manifest.json and an empty raw/.
func TestRunNoHarnessSessionID(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	p := baseParams(archiveRoot)
	p.HarnessSessionID = "" // harness failed to start
	p.InstanceID = "cccccccc-1234-5678-9abc-def012345678"
	p.EndState = "interrupted"
	p.Copier = noopCopier // no-op: harness never produced files

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Post-fix layout: no raw/ subdirectory must exist. The archive dir
	// itself contains only manifest.json (and nothing else when the harness
	// failed to start).
	if _, statErr := os.Stat(filepath.Join(archivePath, "raw")); !os.IsNotExist(statErr) {
		t.Errorf("archive dir must not contain raw/ post-fix; stat returned: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(archivePath, "session.jsonl")); !os.IsNotExist(statErr) {
		t.Errorf("archive dir must not contain session.jsonl when Copier was a no-op; stat returned: %v", statErr)
	}

	// Sanity: archive dir contains manifest.json (only).
	entries, err := os.ReadDir(archivePath)
	if err != nil {
		t.Fatalf("ReadDir archivePath: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "manifest.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("archive dir must contain only manifest.json post-fix; got %v", names)
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

// TestRunNilCopierError verifies that Run returns a clear error when Copier is nil.
func TestRunNilCopierError(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	p := baseParams(archiveRoot)
	p.Copier = nil

	_, err := Run(p)
	if err == nil {
		t.Fatal("Run() with nil Copier succeeded, want error")
	}
	if !strings.Contains(err.Error(), "Copier") {
		t.Errorf("Run() error = %q, want it to mention 'Copier'", err.Error())
	}
}

// TestRunCopierError verifies that a Copier that returns an error causes Run
// to fail and leaves no temp directory behind.
func TestRunCopierError(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	p := baseParams(archiveRoot)
	p.InstanceID = "eeeeeeee-1111-2222-3333-444444444444"
	p.Copier = func(_ context.Context, _ string) error {
		return fmt.Errorf("copy failed: disk full")
	}

	_, err := Run(p)
	if err == nil {
		t.Fatal("Run() with failing Copier succeeded, want error")
	}

	// No temp dir should remain.
	repoDir := filepath.Join(archiveRoot, p.Repo)
	entries, _ := os.ReadDir(repoDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp dir left behind: %s", e.Name())
		}
	}
}

// TestRunIdempotencyError verifies that calling Run a second time for the same
// instance returns an error and leaves the existing archive intact.
func TestRunIdempotencyError(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	p := baseParams(archiveRoot)
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

	// Use a regular file as a directory component in the archive root path so
	// that MkdirAll fails with ENOTDIR — forcing Run() to return an error
	// before any temp directory is created.
	blockingFile := filepath.Join(tmpDir, "blocking-file")
	if err := os.WriteFile(blockingFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}
	badArchiveRoot := filepath.Join(blockingFile, "archive")

	p := baseParams(badArchiveRoot)
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

	p := baseParams(archiveRoot)
	p.InstanceID = "ffffffff-1234-5678-9abc-def012345678"
	p.Copier = fileCopier(map[string]string{
		"session.jsonl": `{"id":"turn_001"}` + "\n",
	})

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

	// Post-fix layout: no raw/ subdirectory exists. session.jsonl is
	// written directly into the archive dir.
	if _, statErr := os.Stat(filepath.Join(archivePath, "raw")); !os.IsNotExist(statErr) {
		t.Errorf("archive dir must not contain raw/ post-fix; stat returned: %v", statErr)
	}
	sessionInfo, err := os.Stat(filepath.Join(archivePath, "session.jsonl"))
	if err != nil {
		t.Fatalf("stat session.jsonl: %v", err)
	}
	if sessionInfo.Mode().Perm() != 0o600 {
		t.Errorf("session.jsonl mode = %04o, want %04o", sessionInfo.Mode().Perm(), 0o600)
	}
}

// TestRunManifestVersions verifies archiveVersion=1 and piMonoVersion=3.
func TestRunManifestVersions(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	p := baseParams(archiveRoot)
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

	p := baseParams(archiveRoot)
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

// TestRunRepoTraversalRejected verifies that a p.Repo containing path traversal
// components is rejected before any filesystem operations.
func TestRunRepoTraversalRejected(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	cases := []string{
		"../../evil",
		"../up",
		"..",
		"good/sub", // slash inside repo name is also invalid
	}

	for _, repo := range cases {
		p := baseParams(archiveRoot)
		p.Repo = repo
		p.InstanceID = "aaaa1111-1111-1111-1111-111111111111"

		_, err := Run(p)
		if err == nil {
			t.Errorf("Run() with repo=%q succeeded, want error", repo)
		}
	}
}

// TestRunAgentRunLog verifies that when AgentRunLogPath points to an existing
// file, it is copied into the archive as agent-run.log.
func TestRunAgentRunLog(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	// Create a fake agent-run.log.
	logContent := "agent started\nagent finished\n"
	logPath := filepath.Join(tmpDir, "agent-run.log")
	if err := os.WriteFile(logPath, []byte(logContent), 0o600); err != nil {
		t.Fatalf("write agent-run.log: %v", err)
	}

	p := baseParams(archiveRoot)
	p.InstanceID = "99999999-aaaa-bbbb-cccc-dddddddddddd"
	p.AgentRunLogPath = logPath

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// agent-run.log must be present and identical.
	got := readFile(t, filepath.Join(archivePath, "agent-run.log"))
	if got != logContent {
		t.Errorf("agent-run.log = %q, want %q", got, logContent)
	}
}

// TestRunAgentRunLogMissing verifies that a missing AgentRunLogPath is silently
// skipped (not an error).
func TestRunAgentRunLogMissing(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	p := baseParams(archiveRoot)
	p.InstanceID = "aaaabbbb-cccc-dddd-eeee-ffffffffffff"
	p.AgentRunLogPath = filepath.Join(tmpDir, "nonexistent-agent-run.log")

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// No agent-run.log should be present.
	if _, statErr := os.Stat(filepath.Join(archivePath, "agent-run.log")); !os.IsNotExist(statErr) {
		t.Errorf("agent-run.log should not exist when source is missing, but stat returned: %v", statErr)
	}
}

// TestRunPodmanProxyAuditLog verifies that when PodmanProxyAuditLogPath points
// to an existing file, it is copied into the archive as podman-proxy.log,
// byte-identical to the source.
func TestRunPodmanProxyAuditLog(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	logContent := `{"ts":"2026-04-25T14:31:00Z","method":"POST","endpoint":"/containers/create","decision":"allow"}` + "\n"
	logPath := filepath.Join(tmpDir, "podman-proxy.log")
	if err := os.WriteFile(logPath, []byte(logContent), 0o600); err != nil {
		t.Fatalf("write podman-proxy.log: %v", err)
	}

	p := baseParams(archiveRoot)
	p.InstanceID = "11112222-3333-4444-5555-666677778888"
	p.PodmanProxyAuditLogPath = logPath

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	got := readFile(t, filepath.Join(archivePath, "podman-proxy.log"))
	if got != logContent {
		t.Errorf("podman-proxy.log = %q, want %q", got, logContent)
	}
}

// TestRunPodmanProxyAuditLogMissing verifies that a missing
// PodmanProxyAuditLogPath (a session that never enabled --containers) is
// silently skipped, with no error and no empty placeholder file created.
func TestRunPodmanProxyAuditLogMissing(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	p := baseParams(archiveRoot)
	p.InstanceID = "22223333-4444-5555-6666-777788889999"
	p.PodmanProxyAuditLogPath = filepath.Join(tmpDir, "nonexistent-podman-proxy.log")

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(archivePath, "podman-proxy.log")); !os.IsNotExist(statErr) {
		t.Errorf("podman-proxy.log should not exist when source is missing, but stat returned: %v", statErr)
	}
}

// TestRunPodmanProxyAuditLogUnreadable verifies that an existing but
// unreadable PodmanProxyAuditLogPath does not fail Run and does not block
// the rest of the archive (manifest still written) — the failure is reported
// via proglog, not swallowed, but it is not fatal to the archive step.
func TestRunPodmanProxyAuditLogUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not block reads")
	}

	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	logPath := filepath.Join(tmpDir, "podman-proxy.log")
	if err := os.WriteFile(logPath, []byte("unreadable\n"), 0o000); err != nil {
		t.Fatalf("write podman-proxy.log: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(logPath, 0o600) })

	p := baseParams(archiveRoot)
	p.InstanceID = "33334444-5555-6666-7777-888899990000"
	p.PodmanProxyAuditLogPath = logPath

	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() should not fail when podman-proxy.log is unreadable, got: %v", err)
	}

	// manifest.json must still have been written — the rest of the archive
	// completed despite the audit-log copy failure.
	if _, statErr := os.Stat(filepath.Join(archivePath, "manifest.json")); statErr != nil {
		t.Errorf("manifest.json should still be written: %v", statErr)
	}

	// The copy must not have silently "succeeded" with a truncated/empty
	// file — it should be entirely absent since copyFile fails before
	// writing any bytes it can't read (or leaves a partial file, either way
	// this is not a claim we make byte-identical since the read failed).
	if _, statErr := os.Stat(filepath.Join(archivePath, "podman-proxy.log")); statErr == nil {
		data, _ := os.ReadFile(filepath.Join(archivePath, "podman-proxy.log"))
		if len(data) > 0 {
			t.Errorf("podman-proxy.log should not contain data copied from an unreadable source, got %q", data)
		}
	}
}

// TestRunPrismContainerAuditLog verifies that the prism-container audit log
// is copied byte-identical, and that a missing log is skipped.
func TestRunPrismContainerAuditLog(t *testing.T) {
	tmpDir := t.TempDir()
	archiveRoot := filepath.Join(tmpDir, "archive")

	logContent := `{"time":"2026-10-10T01:02:03Z","command":"run","image":"docker.io/library/alpine","decision":"allowed","exit_code":0}` + "\n"
	logPath := filepath.Join(tmpDir, "audit.log")
	if err := os.WriteFile(logPath, []byte(logContent), 0o600); err != nil {
		t.Fatalf("write audit.log: %v", err)
	}

	p := baseParams(archiveRoot)
	p.InstanceID = "44445555-6666-7777-8888-999900001111"
	p.PrismContainerAuditLogPath = logPath
	archivePath, err := Run(p)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if got := readFile(t, filepath.Join(archivePath, "prism-container-audit.log")); got != logContent {
		t.Errorf("prism-container-audit.log = %q, want %q", got, logContent)
	}

	p = baseParams(filepath.Join(tmpDir, "archive2"))
	p.InstanceID = "55556666-7777-8888-9999-000011112222"
	p.PrismContainerAuditLogPath = filepath.Join(tmpDir, "missing.log")
	archivePath, err = Run(p)
	if err != nil {
		t.Fatalf("Run() with a missing log: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(archivePath, "prism-container-audit.log")); !os.IsNotExist(statErr) {
		t.Errorf("prism-container-audit.log should not exist when the source is missing: %v", statErr)
	}
}
