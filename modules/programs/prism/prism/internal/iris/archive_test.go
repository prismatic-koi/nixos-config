package iris_test

// archive_test.go — unit tests for the standalone iris.ArchiveSession path
// (#1697).
//
// These tests exercise the archive verb in isolation from cleanup so the
// "no row mutation, no run-dir touch, session keeps running" contract is
// asserted directly. Cleanup-archive parity (same path layout, same file
// contents) is covered by internal/iris/parity/archive_test.go and the
// regression check at the bottom of this file.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	piharness "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// seedSessionWithJSONL inserts a sessions row and writes a pi JSONL file
// under iso.PIAgentDir at the encoded-cwd path. Returns (sessionName,
// instanceID, harnessSessionID, jsonlBytes) for the test to assert on.
func seedSessionWithJSONL(t *testing.T, iso *iristest.Isolated, nameSuffix string) (string, string, string, []byte) {
	t.Helper()
	sessionName := iristest.SessionName(nameSuffix)
	worktree := filepath.Join(iso.Root, "wt-"+nameSuffix)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	instanceID := "iris-test-archive-" + nameSuffix
	hsid := "pi-" + nameSuffix + "-ULID"
	role := "worker"
	hsidPtr := hsid
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:       instanceID,
		SessionName:      sessionName,
		Worktree:         worktree,
		Harness:          "pi",
		AgentRole:        &role,
		HarnessSessionID: &hsidPtr,
		StartedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	encoded := piharness.EncodePiCWD(worktree)
	sessionsDir := filepath.Join(iso.PIAgentDir, "sessions", encoded)
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions dir: %v", err)
	}
	body := []byte(`{"type":"session_init","session_id":"` + hsid + `"}` + "\n" +
		`{"type":"msg_assistant","content":"hi from archive test"}` + "\n")
	piJSONL := filepath.Join(sessionsDir, "20260101T000000Z_"+hsid+".jsonl")
	if err := os.WriteFile(piJSONL, body, 0o644); err != nil {
		t.Fatalf("write pi jsonl: %v", err)
	}
	return sessionName, instanceID, hsid, body
}

// TestArchiveSession_HappyPath asserts the standalone archive verb copies
// the pi JSONL to the documented path and returns a non-skipped result.
func TestArchiveSession_HappyPath(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName, instanceID, _, body := seedSessionWithJSONL(t, iso, "happy")

	res, err := iris.ArchiveSession(context.Background(), iris.ArchiveConfig{
		Database:    iso.DB,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		PIAgentDir:  iso.PIAgentDir,
	}, sessionName)
	if err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}
	if res.Skipped {
		t.Fatalf("ArchiveSession: unexpected Skipped=true (reason=%q)", res.SkipReason)
	}
	want := filepath.Join(iso.Paths.ArchiveRoot, sessionName, instanceID, "raw", "session.jsonl")
	if res.ArchivePath != want {
		t.Errorf("ArchivePath = %q, want %q", res.ArchivePath, want)
	}
	if got, err := os.ReadFile(want); err != nil {
		t.Fatalf("read archive: %v", err)
	} else if string(got) != string(body) {
		t.Errorf("archive content mismatch:\n got: %q\nwant: %q", got, body)
	}
}

// TestArchiveSession_SessionRowUntouched asserts the standalone archive
// verb does NOT mark the session ended (the key contract distinguishing it
// from CleanupSession). This is the "session stays running" AC.
func TestArchiveSession_SessionRowUntouched(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName, instanceID, _, _ := seedSessionWithJSONL(t, iso, "untouched")

	if _, err := iris.ArchiveSession(context.Background(), iris.ArchiveConfig{
		Database:    iso.DB,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		PIAgentDir:  iso.PIAgentDir,
	}, sessionName); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}

	got, err := iso.DB.SessionByInstanceID(instanceID)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatalf("session row %q vanished after archive", instanceID)
	}
	if got.EndedAt != nil {
		t.Errorf("standalone archive set EndedAt=%v; want nil (session must keep running)", got.EndedAt)
	}
	if got.EndState != nil {
		t.Errorf("standalone archive set EndState=%v; want nil", *got.EndState)
	}
}

// TestArchiveSession_EmptyJSONL asserts that a session with no pi JSONL on
// disk produces Skipped=true (no error, no archive directory created). This
// is the "Empty JSONL → exit 0 with informative message, no empty archive"
// AC from the spec.
func TestArchiveSession_EmptyJSONL(t *testing.T) {
	iso := iristest.NewIsolated(t)

	sessionName := iristest.SessionName("empty-jsonl")
	worktree := filepath.Join(iso.Root, "wt-empty")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	hsid := "pi-empty-ULID"
	role := "worker"
	if err := iso.DB.InsertSession(db.Session{
		InstanceID:       "iris-test-archive-empty",
		SessionName:      sessionName,
		Worktree:         worktree,
		Harness:          "pi",
		AgentRole:        &role,
		HarnessSessionID: &hsid,
		StartedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	res, err := iris.ArchiveSession(context.Background(), iris.ArchiveConfig{
		Database:    iso.DB,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		PIAgentDir:  iso.PIAgentDir,
	}, sessionName)
	if err != nil {
		t.Fatalf("ArchiveSession (empty jsonl): %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped = false; want true on empty JSONL")
	}
	if res.SkipReason == "" {
		t.Errorf("SkipReason = empty; want a non-empty informative message")
	}
	if res.ArchivePath != "" {
		t.Errorf("ArchivePath = %q; want \"\" on skip", res.ArchivePath)
	}
	// "no empty archive" — the destination directory must NOT have been
	// created when there was nothing to copy.
	wantNoDir := filepath.Join(iso.Paths.ArchiveRoot, sessionName)
	if _, statErr := os.Stat(wantNoDir); !os.IsNotExist(statErr) {
		t.Errorf("archive dir %q should not exist on empty-JSONL skip (err=%v)", wantNoDir, statErr)
	}
}

// TestArchiveSession_ByInstanceID asserts the --instance-id lookup variant
// resolves to the same outcome as the by-name variant.
func TestArchiveSession_ByInstanceID(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName, instanceID, _, _ := seedSessionWithJSONL(t, iso, "by-instance")

	res, err := iris.ArchiveSessionByInstanceID(context.Background(), iris.ArchiveConfig{
		Database:    iso.DB,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		PIAgentDir:  iso.PIAgentDir,
	}, instanceID)
	if err != nil {
		t.Fatalf("ArchiveSessionByInstanceID: %v", err)
	}
	if res.Skipped {
		t.Fatalf("Skipped=true on happy by-instance path: %q", res.SkipReason)
	}
	want := filepath.Join(iso.Paths.ArchiveRoot, sessionName, instanceID, "raw", "session.jsonl")
	if res.ArchivePath != want {
		t.Errorf("ArchivePath = %q, want %q", res.ArchivePath, want)
	}
}

// TestArchiveSession_NotFound asserts a non-existent session returns a
// clear error rather than a result with Skipped=true.
func TestArchiveSession_NotFound(t *testing.T) {
	iso := iristest.NewIsolated(t)
	_, err := iris.ArchiveSession(context.Background(), iris.ArchiveConfig{
		Database:    iso.DB,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		PIAgentDir:  iso.PIAgentDir,
	}, iristest.SessionName("does-not-exist"))
	if err == nil {
		t.Fatalf("ArchiveSession on missing session: got nil error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not mention \"not found\"", err)
	}
}

// TestArchiveSession_ConfigValidation asserts missing required fields are
// rejected with a clear error.
func TestArchiveSession_ConfigValidation(t *testing.T) {
	iso := iristest.NewIsolated(t)

	if _, err := iris.ArchiveSession(context.Background(), iris.ArchiveConfig{
		ArchiveRoot: iso.Paths.ArchiveRoot,
	}, "x"); err == nil || !strings.Contains(err.Error(), "Database is required") {
		t.Errorf("missing Database: got %v, want \"Database is required\"", err)
	}
	if _, err := iris.ArchiveSession(context.Background(), iris.ArchiveConfig{
		Database: iso.DB,
	}, "x"); err == nil || !strings.Contains(err.Error(), "ArchiveRoot is required") {
		t.Errorf("missing ArchiveRoot: got %v, want \"ArchiveRoot is required\"", err)
	}
	if _, err := iris.ArchiveSession(context.Background(), iris.ArchiveConfig{
		Database:    iso.DB,
		ArchiveRoot: iso.Paths.ArchiveRoot,
	}, ""); err == nil || !strings.Contains(err.Error(), "session name is required") {
		t.Errorf("empty session name: got %v, want \"session name is required\"", err)
	}
}

// TestArchiveSession_Idempotent asserts that running archive twice on the
// same session writes the same destination both times (a snapshot verb,
// not an append-only one) and does not error on the second invocation.
func TestArchiveSession_Idempotent(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName, _, _, _ := seedSessionWithJSONL(t, iso, "idempotent")
	cfg := iris.ArchiveConfig{
		Database:    iso.DB,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		PIAgentDir:  iso.PIAgentDir,
	}
	r1, err := iris.ArchiveSession(context.Background(), cfg, sessionName)
	if err != nil {
		t.Fatalf("first ArchiveSession: %v", err)
	}
	r2, err := iris.ArchiveSession(context.Background(), cfg, sessionName)
	if err != nil {
		t.Fatalf("second ArchiveSession: %v", err)
	}
	if r1.ArchivePath != r2.ArchivePath {
		t.Errorf("idempotent archive: paths differ: %q vs %q", r1.ArchivePath, r2.ArchivePath)
	}
	if r1.Skipped || r2.Skipped {
		t.Errorf("idempotent archive: unexpected Skipped (r1=%v, r2=%v)", r1.Skipped, r2.Skipped)
	}
}

// TestCleanupArchive_PathParity asserts the regression invariant from the
// spec: CleanupSession's archive step still writes to the same path that
// ArchiveSession produces. This guards against the refactor accidentally
// diverging the two code paths.
func TestCleanupArchive_PathParity(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName, instanceID, _, _ := seedSessionWithJSONL(t, iso, "parity")

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:    iso.DB,
		RunDir:      iso.Paths.RunDir,
		ArchiveRoot: iso.Paths.ArchiveRoot,
		PIAgentDir:  iso.PIAgentDir,
	}, sessionName)
	if err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	want := filepath.Join(iso.Paths.ArchiveRoot, sessionName, instanceID, "raw", "session.jsonl")
	if res.ArchivePath != want {
		t.Errorf("CleanupSession.ArchivePath = %q, want %q", res.ArchivePath, want)
	}
	if _, statErr := os.Stat(want); statErr != nil {
		t.Fatalf("cleanup archive missing at %q: %v", want, statErr)
	}
}
