package cmd

// cleanup_pi_resume_test.go — regression tests for the pi conversation-resume
// severance during cleanup.
//
// `prism cleanup` must sever the pi conversation-resume linkage so that a
// re-spawn on the SAME branch name does not silently resume the cleaned
// session's pi conversation. Two surfaces:
//
//   1. agent_status.harness_session_id (DB) — must be NULL after cleanup.
//   2. ~/.pi/agent/sessions/<encodePiCWD(worktree)>/*_<id>.jsonl (FS) — must
//      be removed for the cleaned session's id.
//
// These tests exercise headlessCleanup against a temp DB and a temp $HOME so
// they verify the cleanup-side severance end-to-end without touching the
// host's real state. ~review-N-* child sessions are also covered.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// clearPICodingAgentDir clears PI_CODING_AGENT_DIR for the duration of the
// test. Required by tests that exercise the host-fallback branch of
// piResumeSessionsRoot / piSessionsRoot — the developer host sets the env
// var system-wide (the resolver honours it), and without clearing it tests
// that set up a temp HOME would silently fall through to
// /run/prism/pi-agent/sessions/ and fail.
func clearPICodingAgentDir(t *testing.T) {
	t.Helper()
	t.Setenv("PI_CODING_AGENT_DIR", "")
}

// piEncodeCWD mirrors internal/container.encodePiCWD / internal/harness/pi.EncodePiCWD.
// Duplicated here so cmd-package tests do not need to import internal/container.
func piEncodeCWD(cwd string) string {
	stripped := strings.TrimLeft(cwd, "/\\")
	replaced := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(stripped)
	return "--" + replaced + "--"
}

// writeFakePiResumeJSONL creates a synthetic pi transcript at the host-mode
// sessions root and returns its absolute path. The file's content is not
// inspected by cleanup — only its name suffix is matched.
func writeFakePiResumeJSONL(t *testing.T, home, worktree, harnessSessionID string) string {
	t.Helper()
	dir := filepath.Join(home, ".pi", "agent", "sessions", piEncodeCWD(worktree))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir resume dir: %v", err)
	}
	path := filepath.Join(dir, "2026-01-02T03-04-05-000Z_"+harnessSessionID+".jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session\"}\n"), 0o600); err != nil {
		t.Fatalf("write resume jsonl: %v", err)
	}
	return path
}

// TestHeadlessCleanup_ClearsHarnessSessionID is the regression test for the
// DB-side half of the resume severance. After headlessCleanup runs against a
// session row carrying a harness_session_id, the column must be NULL —
// otherwise the next spawn on the same branch name would read the stale id
// back and pi would resume the dead conversation.
//
// The sever is gated on the archive step actually copying a transcript, so
// this test seeds a full archivable pi session via seedRowWithLifecycleFields.
func TestHeadlessCleanup_ClearsHarnessSessionID(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	const session = "myrepo@feature"
	const sid = "019e72d2-446a-712f-baea-7abc9e7ce7df"
	_ = seedRowWithLifecycleFields(t, dbFile, session, "", sid)

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	if err := headlessCleanup(session, "feature", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("row missing after cleanup")
	}
	if status.HarnessSessionID != nil {
		t.Errorf("HarnessSessionID = %q after cleanup; want nil (resume linkage must be severed — issue #2035)",
			*status.HarnessSessionID)
	}
	// Sanity: SetEnded still ran.
	if status.EndedAt == nil {
		t.Errorf("EndedAt = nil after cleanup; want non-nil")
	}
}

// TestHeadlessCleanup_RemovesPiResumeJSONL is the regression test for the
// FS-side half of the resume severance. After headlessCleanup runs against a
// session whose pi transcript is on disk, the *_<id>.jsonl file under the
// worktree's encoded-cwd directory must be removed.
//
// The sever runs only after the archive step reports
// copied == true, so the seed is a full archivable pi session via
// seedRowWithLifecycleFields (which plants the transcript inside a
// test-owned HOME so the sever path can locate and remove it).
func TestHeadlessCleanup_RemovesPiResumeJSONL(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	const session = "myrepo@feature"
	const sid = "019e72d2-446a-712f-baea-7abc9e7ce7df"
	_ = seedRowWithLifecycleFields(t, dbFile, session, "", sid)

	// Locate the transcript file the seed helper planted so we can assert
	// its post-cleanup absence. The helper plants under
	// <HOME>/.pi/agent/sessions/<encodePiCWD(worktree)>/*_<sid>.jsonl.
	home := os.Getenv("HOME")
	worktree := filepath.Join(home, "worktree", strings.ReplaceAll(session, "/", "-"))
	sessDir := filepath.Join(home, ".pi", "agent", "sessions", piEncodeCWD(worktree))
	transcript := filepath.Join(sessDir, "2026-01-02T03-04-05-000Z_"+sid+".jsonl")

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Pass empty worktreePath to skip the git-worktree-remove branch; the
	// pi-resume severance happens later in the same flow.
	if err := headlessCleanup(session, "feature", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Errorf("pi resume transcript %s still exists after cleanup (err=%v); want removed (issue #2035)",
			transcript, err)
	}
}

// TestHeadlessCleanup_ClearsHarnessSessionIDForReviewChildren verifies the
// LIKE-cascade parity: after cleanup of a parent session, any review-agent
// child rows (session_name LIKE "<parent>~review-%") must also have their
// harness_session_id nulled. This mirrors SetEnded's behaviour so that the
// resume linkage is severed for review children too.
//
// The sever runs only when the archive step actually copied the
// parent's transcript, so seedRowWithLifecycleFields is used for the parent.
// The review children (which do not require their own archive gate to be
// unlocked — severPiResumeLinkage's LIKE cascade nulls them from the same
// UPDATE that clears the parent) get minimal seeds.
func TestHeadlessCleanup_ClearsHarnessSessionIDForReviewChildren(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	const parent = "myrepo@feature"
	child1 := parent + "~review-1-architect"
	child2 := parent + "~review-2-skeptic"

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	_ = seedRowWithLifecycleFields(t, dbFile, parent, "", "019e0000-0000-0000-0000-000000000001")

	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, name := range []string{child1, child2} {
		if err := d.UpsertStatus(name, "myrepo", "", "running", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %q: %v", name, err)
		}
	}
	if err := d.UpdateHarnessSessionID(child1, "019e0000-0000-0000-0000-000000000002"); err != nil {
		t.Fatalf("UpdateHarnessSessionID child1: %v", err)
	}
	if err := d.UpdateHarnessSessionID(child2, "019e0000-0000-0000-0000-000000000003"); err != nil {
		t.Fatalf("UpdateHarnessSessionID child2: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	if err := headlessCleanup(parent, "feature", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	for _, name := range []string{parent, child1, child2} {
		st, err := d2.CurrentStatus(name)
		if err != nil {
			t.Fatalf("CurrentStatus %q: %v", name, err)
		}
		if st == nil {
			t.Fatalf("row %q missing after cleanup", name)
		}
		if st.HarnessSessionID != nil {
			t.Errorf("session %q: HarnessSessionID = %q after cleanup; want nil (LIKE-cascade)",
				name, *st.HarnessSessionID)
		}
	}
}

// TestHeadlessCleanup_PiResumeAbsentSucceeds verifies the edge case where
// cleanup is invoked on a session whose pi resume JSONL never existed
// (fresh session, no transcript yet). Cleanup must still succeed.
//
// Semantics: when the transcript is missing, sever is SKIPPED —
// harness_session_id is retained so a subsequent cleanup can archive-then-
// sever if the transcript reappears (and the pi sessions dir is not
// modified).
func TestHeadlessCleanup_PiResumeAbsentSucceeds(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)
	clearPICodingAgentDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	const session = "myrepo@brand-new"
	worktree := filepath.Join(home, "code", "myrepo", "brand-new")
	const sid = "019e0000-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus(session, "myrepo", worktree, "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	instanceID := "22222222-3333-4444-5555-666666666666"
	if err := d.SetInstanceID(session, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.SetIsolationMode(session, "host"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: session,
		Repo:        "myrepo",
		Worktree:    worktree,
		Harness:     "pi",
		StartedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := d.UpdateHarnessSessionID(session, sid); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	if err := headlessCleanup(session, "brand-new", "", ""); err != nil {
		t.Errorf("headlessCleanup: %v, want nil (must succeed even with no on-disk transcript)", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	st, _ := d2.CurrentStatus(session)
	if st == nil {
		t.Fatal("row missing")
	}
	// Transcript missing → sever skipped → harness_session_id
	// retained so a follow-up cleanup can archive-then-sever if the file
	// reappears.
	if st.HarnessSessionID == nil || *st.HarnessSessionID != sid {
		t.Errorf("HarnessSessionID = %v after cleanup; want retained (%q) — sever must be skipped when transcript missing (issue #2336)",
			st.HarnessSessionID, sid)
	}
	// Sanity: ended_at was still stamped — cleanup succeeded from the
	// user's perspective; only the pi-resume linkage stayed intact.
	if st.EndedAt == nil {
		t.Errorf("EndedAt = nil after cleanup; want stamped (cleanup must still complete)")
	}
}

// TestHeadlessCleanup_LeavesOtherSessionsResumePointersAlone verifies that
// cleaning up one session does not collaterally clear another session's
// harness_session_id — the LIKE pattern is anchored to the cleaned session's
// name and the `~review-%` suffix, not a wildcard.
func TestHeadlessCleanup_LeavesOtherSessionsResumePointersAlone(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)
	clearPICodingAgentDir(t)
	t.Setenv("HOME", t.TempDir())

	const cleaned = "myrepo@going"
	const survivor = "myrepo@staying"
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, name := range []string{cleaned, survivor} {
		if err := d.UpsertStatus(name, "myrepo", "", "running", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %q: %v", name, err)
		}
	}
	const survivorSID = "019e0000-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	if err := d.UpdateHarnessSessionID(cleaned, "019e0000-cccc-cccc-cccc-cccccccccccc"); err != nil {
		t.Fatalf("UpdateHarnessSessionID cleaned: %v", err)
	}
	if err := d.UpdateHarnessSessionID(survivor, survivorSID); err != nil {
		t.Fatalf("UpdateHarnessSessionID survivor: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	if err := headlessCleanup(cleaned, "going", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	stSurvivor, _ := d2.CurrentStatus(survivor)
	if stSurvivor == nil {
		t.Fatal("survivor row missing")
	}
	if stSurvivor.HarnessSessionID == nil || *stSurvivor.HarnessSessionID != survivorSID {
		t.Errorf("survivor HarnessSessionID = %v, want %q (cleanup must not touch sibling rows)",
			stSurvivor.HarnessSessionID, survivorSID)
	}
}
