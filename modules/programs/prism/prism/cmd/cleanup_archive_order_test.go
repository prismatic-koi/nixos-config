package cmd

// cleanup_archive_order_test.go — regression tests for issue #2219.
//
// All four cleanup paths (doCleanup, headlessCleanupWithJSON, closeSession,
// headlessCloseSessionWithJSON) must archive the pi transcript JSONL BEFORE
// severing the pi resume linkage — the sever deletes the same host-root file
// the archive copies (post-#2210 both resolve the same path in every
// isolation mode), so the inverted ordering produced manifest-only archives:
// lossy cleanup.
//
// Each path is exercised end-to-end against a temp HOME / temp XDG_DATA_HOME
// and a temp DB, asserting behaviourally (not by inspection) that:
//
//  1. the archive directory contains session.jsonl with the transcript bytes,
//  2. the transcript is then removed from the host sessions root (the sever
//     still runs, post-archive), and
//  3. agent_status.harness_session_id is cleared.
//
// The archive-failure semantic is also covered: when the archive step fails,
// the sever must be skipped entirely — the transcript stays on disk and
// harness_session_id stays populated so a re-run can archive-then-sever.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

const (
	archiveOrderSID  = "019e72d2-446a-712f-baea-7abc9e7ce7df"
	archiveOrderIID  = "11111111-2222-4333-8444-555555555555"
	archiveOrderRepo = "prism-test"
)

// archiveOrderFixture carries the paths and identifiers seeded by
// setupArchiveOrderFixture for post-cleanup assertions.
type archiveOrderFixture struct {
	session    string
	worktree   string
	dbFile     string
	transcript string
}

// setupArchiveOrderFixture seeds a complete pi session for cleanup-ordering
// tests: a temp HOME (with XDG_DATA_HOME under it so the archive root is
// test-local), an on-disk pi transcript at the host sessions root, an
// agent_status row carrying instance_id + isolation_mode "host" +
// harness_session_id, and a matching sessions row (harness "pi") so
// runSessionArchive has a full incarnation record to archive.
func setupArchiveOrderFixture(t *testing.T, branch, worktree string) archiveOrderFixture {
	t.Helper()
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)
	clearPICodingAgentDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	session := archiveOrderRepo + "@" + branch
	if worktree == "" {
		worktree = filepath.Join(home, "code", archiveOrderRepo, branch)
	}
	transcript := writeFakePiResumeJSONL(t, home, worktree, archiveOrderSID)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus(session, archiveOrderRepo, worktree, "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID(session, archiveOrderIID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.SetIsolationMode(session, "host"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  archiveOrderIID,
		SessionName: session,
		Repo:        archiveOrderRepo,
		Worktree:    worktree,
		Harness:     "pi",
		StartedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	// UpdateHarnessSessionID writes to BOTH agent_status and sessions
	// (matched via instance_id), mirroring the live sidecar write path.
	if err := d.UpdateHarnessSessionID(session, archiveOrderSID); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	return archiveOrderFixture{
		session:    session,
		worktree:   worktree,
		dbFile:     dbFile,
		transcript: transcript,
	}
}

// assertTranscriptArchived asserts that the sessions row for the fixture's
// instance_id has archive_path recorded and that <archive_path>/session.jsonl
// exists with the same bytes as the original transcript. Returns the archive
// directory path for further assertions.
func assertTranscriptArchived(t *testing.T, f archiveOrderFixture) string {
	t.Helper()
	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d.Close()
	sess, err := d.SessionByInstanceID(archiveOrderIID)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess == nil {
		t.Fatal("sessions row missing after cleanup")
	}
	if sess.ArchivePath == nil || *sess.ArchivePath == "" {
		t.Fatal("archive_path not recorded after cleanup; want a populated archive directory")
	}
	archived := filepath.Join(*sess.ArchivePath, "session.jsonl")
	got, readErr := os.ReadFile(archived)
	if readErr != nil {
		t.Fatalf("archive must contain session.jsonl (issue #2219 — archive must read the transcript before the sever deletes it): %v", readErr)
	}
	want := []byte("{\"type\":\"session\"}\n")
	if !bytes.Equal(got, want) {
		t.Errorf("archived session.jsonl content = %q, want %q", got, want)
	}
	return *sess.ArchivePath
}

// assertSeveredAfterArchive asserts the post-archive sever ran: the host-root
// transcript is gone and agent_status.harness_session_id is NULL.
func assertSeveredAfterArchive(t *testing.T, f archiveOrderFixture) {
	t.Helper()
	if _, err := os.Stat(f.transcript); !os.IsNotExist(err) {
		t.Errorf("host transcript %s still exists after successful archive (err=%v); want removed (sever must still run post-archive)",
			f.transcript, err)
	}
	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d.Close()
	st, err := d.CurrentStatus(f.session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("agent_status row missing after cleanup")
	}
	if st.HarnessSessionID != nil {
		t.Errorf("HarnessSessionID = %q after cleanup; want nil (resume linkage must be severed)", *st.HarnessSessionID)
	}
}

// TestHeadlessCleanup_ArchivesTranscriptBeforeSever covers the
// headlessCleanupWithJSON path (prism cleanup --yes).
func TestHeadlessCleanup_ArchivesTranscriptBeforeSever(t *testing.T) {
	f := setupArchiveOrderFixture(t, "archive-order-headless", "")

	if err := headlessCleanup(f.session, "archive-order-headless", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	assertTranscriptArchived(t, f)
	assertSeveredAfterArchive(t, f)
}

// TestHeadlessCloseSession_ArchivesTranscriptBeforeSever covers the
// headlessCloseSessionWithJSON path (prism close --yes / coordinator close).
func TestHeadlessCloseSession_ArchivesTranscriptBeforeSever(t *testing.T) {
	f := setupArchiveOrderFixture(t, "archive-order-soft", "")

	if err := headlessCloseSession(f.session); err != nil {
		t.Fatalf("headlessCloseSession: %v", err)
	}

	assertTranscriptArchived(t, f)
	assertSeveredAfterArchive(t, f)
}

// TestCloseSession_ArchivesTranscriptBeforeSever covers the interactive
// closeSession path (@main / non-worktree sessions).
func TestCloseSession_ArchivesTranscriptBeforeSever(t *testing.T) {
	f := setupArchiveOrderFixture(t, "archive-order-close", "")

	if err := closeSession(f.session); err != nil {
		t.Fatalf("closeSession: %v", err)
	}

	assertTranscriptArchived(t, f)
	assertSeveredAfterArchive(t, f)
}

// TestDoCleanup_ArchivesTranscriptBeforeSever covers the interactive TUI
// doCleanup path. Requires git for the worktree-removal step that precedes
// the DB block, so it builds a minimal bare repo + feature worktree and
// invokes the returned tea.Cmd directly.
func TestDoCleanup_ArchivesTranscriptBeforeSever(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH — skipping integration test")
	}

	bareRoot, worktreePath, branchName := setupMinimalBareRepo(t)
	f := setupArchiveOrderFixture(t, "archive-order-tui", worktreePath)

	m := cleanupModel{
		session:      f.session,
		worktreeName: branchName,
		worktreePath: worktreePath,
		bareRoot:     bareRoot,
	}
	msg := m.doCleanup()()
	done, ok := msg.(cleanupDoneMsg)
	if !ok {
		t.Fatalf("doCleanup returned %T, want cleanupDoneMsg", msg)
	}
	if done.err != nil {
		t.Fatalf("doCleanup: %v", done.err)
	}

	assertTranscriptArchived(t, f)
	assertSeveredAfterArchive(t, f)
}

// TestHeadlessCleanup_ArchiveFailureLeavesTranscript covers the
// archive-failure semantic (issue #2219 AC): when the archive step fails, the
// sever must be skipped — the transcript JSONL stays on disk and
// agent_status.harness_session_id stays populated (so a re-run of cleanup can
// archive-then-sever; clearing the pointer would orphan the file forever) —
// and cleanup reports the archive failure in the JSON envelope.
//
// The failure is forced by planting a regular FILE at the archive root path,
// which makes archive.Run's MkdirAll fail with ENOTDIR regardless of the uid
// running the tests.
func TestHeadlessCleanup_ArchiveFailureLeavesTranscript(t *testing.T) {
	f := setupArchiveOrderFixture(t, "archive-order-fail", "")

	// Plant a file where the archive root directory should be.
	dataHome := os.Getenv("XDG_DATA_HOME")
	if err := os.MkdirAll(filepath.Join(dataHome, "prism"), 0o700); err != nil {
		t.Fatalf("mkdir prism data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataHome, "prism", "archive"), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("plant archive-root blocker file: %v", err)
	}

	out := captureStdoutDuringFn(t, func() {
		// Archive failures other than ErrAlreadyExists are non-fatal —
		// cleanup must still complete.
		if err := headlessCleanupWithJSON(f.session, "archive-order-fail", "", "", true); err != nil {
			t.Errorf("headlessCleanupWithJSON: %v, want nil (archive failure is non-fatal)", err)
		}
	})

	// The transcript must NOT have been deleted.
	if _, err := os.Stat(f.transcript); err != nil {
		t.Errorf("host transcript %s missing after failed archive (err=%v); want left in place (no data loss)", f.transcript, err)
	}

	// The DB-side sever must also have been skipped — the resume pointer is
	// what a re-run needs to locate the transcript.
	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d.Close()
	st, err := d.CurrentStatus(f.session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("agent_status row missing after cleanup")
	}
	if st.HarnessSessionID == nil || *st.HarnessSessionID != archiveOrderSID {
		t.Errorf("HarnessSessionID = %v after failed archive; want %q retained (sever skipped so cleanup is re-runnable)",
			st.HarnessSessionID, archiveOrderSID)
	}

	// The JSON envelope must report the archive failure on the
	// harness_session_id_cleared field (the sever-was-skipped outcome).
	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(out)), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	v, ok := env["harness_session_id_cleared"]
	if !ok {
		t.Fatalf("envelope missing harness_session_id_cleared; got: %v", env)
	}
	s, isString := v.(string)
	if !isString || !strings.Contains(s, "archive") {
		t.Errorf("harness_session_id_cleared = %v (%T), want a string reporting the archive failure", v, v)
	}
}

// TestHeadlessCleanup_NoTranscript_ManifestOnlyArchive covers the no-transcript
// edge case (issue #2219 AC): a session whose pi transcript never existed must
// clean up without error, producing a manifest-only archive (no session.jsonl)
// with the sever reduced to a DB-clear no-op.
func TestHeadlessCleanup_NoTranscript_ManifestOnlyArchive(t *testing.T) {
	f := setupArchiveOrderFixture(t, "archive-order-empty", "")
	// Remove the transcript the fixture seeded — this test wants the
	// "pi never wrote anything" shape.
	if err := os.Remove(f.transcript); err != nil {
		t.Fatalf("remove seeded transcript: %v", err)
	}

	if err := headlessCleanup(f.session, "archive-order-empty", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v, want nil (no-transcript cleanup must succeed)", err)
	}

	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d.Close()
	sess, err := d.SessionByInstanceID(archiveOrderIID)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess == nil || sess.ArchivePath == nil || *sess.ArchivePath == "" {
		t.Fatal("archive_path not recorded — manifest-only archive must still be written")
	}
	if _, err := os.Stat(filepath.Join(*sess.ArchivePath, "manifest.json")); err != nil {
		t.Errorf("manifest.json missing from archive dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(*sess.ArchivePath, "session.jsonl")); !os.IsNotExist(err) {
		t.Errorf("session.jsonl unexpectedly present in manifest-only archive (err=%v)", err)
	}
	st, err := d.CurrentStatus(f.session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("agent_status row missing after cleanup")
	}
	if st.HarnessSessionID != nil {
		t.Errorf("HarnessSessionID = %q after cleanup; want nil (sever still runs after a successful archive)", *st.HarnessSessionID)
	}
}
