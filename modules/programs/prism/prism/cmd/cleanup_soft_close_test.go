package cmd

// cleanup_soft_close_test.go — regression tests for soft-close transcript
// preservation.
//
// Soft-close paths (closeSession, headlessCloseSessionWithJSONTo — i.e.
// coordinator/@main sessions, non-@ sessions, open-PR workers, and
// --keep-worktree, which routes through headlessCloseSessionWithJSONTo) must
// PRESERVE the pi transcript JSONL in the pi sessions root: that file is
// what pi's interactive /resume reads, and deleting it on a soft close would
// permanently destroy conversation history. The auto-resume defence (a
// re-spawn on the same session name must start a fresh conversation) is
// carried entirely by the DB clear — spawn appends `--session <id>` only when
// agent_status.harness_session_id is non-empty AND a matching JSONL exists
// on disk — so the soft paths clear the DB pointer and purge
// pending_replay_deliveries WITHOUT the FS delete. The DB-only sever runs
// UNCONDITIONALLY on soft paths — not gated on the archive outcome —
// because a soft-closed session name is re-spawned routinely (every reopen
// of the coordinator @main) and a retained pointer next to a preserved
// transcript is exactly the auto-resume combination.
//
// Hard-cleanup paths (doCleanup, headlessCleanupWithJSONTo) retain the FS
// delete plus the copied-gate and archive-failure skip, byte-for-byte
// — covered in cleanup_archive_order_test.go and cleanup_sever_gate_test.go.
//
// Test discipline: the skip logic ("soft mode skips the FS
// delete") is a mode parameter, not a deep-frame gate, so the
// revert-and-watch-fail pair is expressed as two direct calls to
// archiveThenSeverPiResume on identical fixtures differing ONLY in the mode
// argument: soft preserves the transcript, hard deletes it. If the hard call
// failed to delete (wrong fixture paths, resolver drift), the guard test
// fails — proving the soft test's preservation assertion is load-bearing
// rather than vacuously true.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
)

// runArchiveThenSeverDirect opens the fixture DB and invokes
// archiveThenSeverPiResume directly with the given mode, returning the sever
// outcome. Used by the positive/revert-guard pair so the ONLY difference
// between the two tests is the mode argument.
func runArchiveThenSeverDirect(t *testing.T, f archiveOrderFixture, mode severMode) any {
	t.Helper()
	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	archiveErr, severOutcome := archiveThenSeverPiResume(d, f.session, archiveOrderIID, "host", mode)
	if archiveErr != nil {
		t.Fatalf("archiveThenSeverPiResume(mode=%v): archive error %v, want nil", mode, archiveErr)
	}
	return severOutcome
}

// TestArchiveThenSever_SoftMode_PreservesTranscript is the positive half of
// the revert-and-watch-fail pair: with severModeSoft, the transcript
// survives in the pi sessions root, the DB pointer is cleared, and the
// outcome reports true (the sever completed).
func TestArchiveThenSever_SoftMode_PreservesTranscript(t *testing.T) {
	f := setupArchiveOrderFixture(t, "soft-mode-direct", "")

	outcome := runArchiveThenSeverDirect(t, f, severModeSoft)

	if ok, isBool := outcome.(bool); !isBool || !ok {
		t.Errorf("sever outcome = %v (%T), want true (soft sever must complete)", outcome, outcome)
	}
	assertTranscriptArchived(t, f)
	assertSoftSeveredAfterArchive(t, f)
}

// TestArchiveThenSever_HardMode_RemovesTranscript_RevertGuard is the guard
// half: the SAME fixture shape processed with severModeHard must DELETE the
// transcript. If this fails,
// the preservation assertion in the soft-mode test above is not load-bearing
// — some other layer would be preserving the file regardless of the mode.
func TestArchiveThenSever_HardMode_RemovesTranscript_RevertGuard(t *testing.T) {
	f := setupArchiveOrderFixture(t, "hard-mode-revert", "")

	outcome := runArchiveThenSeverDirect(t, f, severModeHard)

	if ok, isBool := outcome.(bool); !isBool || !ok {
		t.Errorf("sever outcome = %v (%T), want true (hard sever must complete)", outcome, outcome)
	}
	if _, err := os.Stat(f.transcript); !os.IsNotExist(err) {
		t.Errorf("transcript %s survived severModeHard (err=%v); want removed — the soft-mode preservation assertion is not load-bearing",
			f.transcript, err)
	}
	assertHarnessSessionIDCleared(t, f)
}

// TestSoftClose_PurgesPendingReplayDeliveries verifies the soft sever still
// wipes the durable pending-replay buffer (the twin of the DB pointer):
// a re-spawn on the same session name must not pick up the
// previous incarnation's undelivered coordinator directives, even though the
// transcript itself is preserved.
func TestSoftClose_PurgesPendingReplayDeliveries(t *testing.T) {
	f := setupArchiveOrderFixture(t, "soft-close-replay", "")

	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := d.InsertPendingReplayDelivery(db.PendingReplayRow{
		SessionName: f.session,
		DeliveryID:  "2371-pending-directive",
		Text:        "coordinator directive that must not survive the close",
		DeliverAs:   "followUp",
		Source:      "coordinator",
	}); err != nil {
		t.Fatalf("InsertPendingReplayDelivery: %v", err)
	}
	d.Close()

	if err := headlessCloseSession(f.session); err != nil {
		t.Fatalf("headlessCloseSession: %v", err)
	}

	d2, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	count, err := d2.CountPendingReplayDeliveries(f.session)
	if err != nil {
		t.Fatalf("CountPendingReplayDeliveries: %v", err)
	}
	if count != 0 {
		t.Errorf("pending_replay_deliveries count = %d after soft close; want 0 (purge must run in soft mode too)", count)
	}
	// The purge must not have been bought at the cost of the transcript.
	if _, err := os.Stat(f.transcript); err != nil {
		t.Errorf("transcript %s missing after soft close (err=%v); want preserved (issue #2371)", f.transcript, err)
	}
}

// TestSoftClose_RespawnLaunchesPiWithoutSessionFlag verifies the auto-resume
// defence end-to-end from the spawn side: after a soft close, the DB
// harness_session_id is NULL, so the next spawn builds a pi invocation with
// NO `--session <id>` argument — a fresh conversation — even though the
// transcript for the old conversation is still on disk.
//
// The precondition assertion makes this test load-bearing: with the
// PRE-close harness_session_id, PIInvocation DOES find the preserved
// transcript and appends `--session`. So if the soft close ever stopped
// clearing the DB pointer, the post-close invocation would resume the
// defunct conversation and this test would fail.
func TestSoftClose_RespawnLaunchesPiWithoutSessionFlag(t *testing.T) {
	f := setupArchiveOrderFixture(t, "soft-close-respawn", "")

	if err := headlessCloseSession(f.session); err != nil {
		t.Fatalf("headlessCloseSession: %v", err)
	}

	// Transcript preserved — this is what makes the precondition below
	// meaningful (the resolver only appends --session when the file exists).
	if _, err := os.Stat(f.transcript); err != nil {
		t.Fatalf("transcript %s missing after soft close (err=%v); want preserved (issue #2371)", f.transcript, err)
	}

	// Precondition: were the DB pointer still populated with the old id,
	// spawn WOULD resume — the preserved transcript is resolvable.
	preArgs := container.PIInvocation(container.Config{
		SessionName:      f.session,
		Worktree:         f.worktree,
		HarnessSessionID: archiveOrderSID,
	})
	if !slices.Contains(preArgs, "--session") {
		t.Fatalf("precondition failed: PIInvocation with the pre-close id omitted --session (args=%v) — the post-close assertion below would be vacuous", preArgs)
	}

	// Mirror cmd/agent_run.go: read harness_session_id back from the DB and
	// thread it into container.Config.
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
		t.Fatal("agent_status row missing after soft close")
	}
	hsid := ""
	if st.HarnessSessionID != nil {
		hsid = *st.HarnessSessionID
	}
	if hsid != "" {
		t.Errorf("harness_session_id = %q after soft close; want cleared (#2035 defence)", hsid)
	}
	args := container.PIInvocation(container.Config{
		SessionName:      f.session,
		Worktree:         f.worktree,
		HarnessSessionID: hsid,
	})
	if slices.Contains(args, "--session") {
		t.Errorf("PIInvocation after soft close = %v; must NOT contain --session (re-spawn must start a fresh conversation — #2035)", args)
	}
}

// TestSoftClose_JSONEnvelope_ReportsCleared verifies that the --json
// envelope's harness_session_id_cleared field reports true after a
// successful soft close (the DB clear ran), while the transcript is
// preserved on disk.
func TestSoftClose_JSONEnvelope_ReportsCleared(t *testing.T) {
	f := setupArchiveOrderFixture(t, "soft-close-json", "")

	out := captureStdoutDuringFn(t, func() {
		if err := headlessCloseSessionWithJSON(f.session, true); err != nil {
			t.Errorf("headlessCloseSessionWithJSON: %v, want nil", err)
		}
	})

	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(out)), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	v, ok := env["harness_session_id_cleared"]
	if !ok {
		t.Fatalf("envelope missing harness_session_id_cleared; got: %v", env)
	}
	if got, isBool := v.(bool); !isBool || !got {
		t.Errorf("harness_session_id_cleared = %v (%T), want true (DB clear ran on soft close)", v, v)
	}
	assertSoftSeveredAfterArchive(t, f)
}

// TestSoftClose_NoTranscript_SucceedsAndReportsTruthfully covers the
// no-transcript edge case: a soft close of a session whose pi transcript
// never existed completes without error, and the envelope reports the sever
// outcome truthfully.
//
// The soft sever is DB-only and unconditional (the copied-gate protects the
// FS delete, which soft mode never performs), so the DB clear runs and the
// field reports `true` — which is truthful: harness_session_id_cleared speaks
// only to the DB clear, and makes no claim that a transcript was removed or
// preserved. The unconditional clear is load-bearing for the auto-resume
// defence: were the pointer retained on the archive skip
// paths, a transcript later appearing (or one the archive failed to locate)
// plus the live pointer would resume a defunct conversation on re-spawn.
func TestSoftClose_NoTranscript_SucceedsAndReportsTruthfully(t *testing.T) {
	f := setupArchiveOrderFixture(t, "soft-close-empty", "")
	// Remove the seeded transcript — the "pi never wrote anything" shape —
	// and plant a differently-named sibling to prove the soft path touches
	// nothing in the encoded-cwd dir.
	if err := os.Remove(f.transcript); err != nil {
		t.Fatalf("remove seeded transcript: %v", err)
	}
	sibling := filepath.Join(filepath.Dir(f.transcript), "2026-01-02T03-04-05-000Z_other-id.jsonl")
	if err := os.WriteFile(sibling, []byte(`{"type":"session"}`+"\n"), 0o600); err != nil {
		t.Fatalf("plant sibling transcript: %v", err)
	}

	out := captureStdoutDuringFn(t, func() {
		if err := headlessCloseSessionWithJSON(f.session, true); err != nil {
			t.Errorf("headlessCloseSessionWithJSON: %v, want nil (no-transcript soft close must succeed)", err)
		}
	})

	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(out)), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	v, ok := env["harness_session_id_cleared"]
	if !ok {
		t.Fatalf("envelope missing harness_session_id_cleared; got: %v", env)
	}
	if got, isBool := v.(bool); !isBool || !got {
		t.Errorf("harness_session_id_cleared = %v (%T), want true (soft mode clears the DB pointer even when no transcript was copied — issue #2371)", v, v)
	}

	// The sibling file must be untouched — no FS activity in the pi
	// sessions dir on a soft close, transcript or not.
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling transcript %s missing after soft close (err=%v); want untouched", sibling, err)
	}
	// DB pointer cleared; manifest-only archive still recorded.
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
		t.Fatal("agent_status row missing after soft close")
	}
	if st.HarnessSessionID != nil {
		t.Errorf("HarnessSessionID = %q after no-transcript soft close; want nil", *st.HarnessSessionID)
	}
	sess, err := d.SessionByInstanceID(archiveOrderIID)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess == nil || sess.ArchivePath == nil || *sess.ArchivePath == "" {
		t.Fatal("archive_path not recorded — the manifest-only archive must still be written on soft close")
	}
	if _, err := os.Stat(filepath.Join(*sess.ArchivePath, "session.jsonl")); !os.IsNotExist(err) {
		t.Errorf("session.jsonl unexpectedly present in manifest-only archive (err=%v)", err)
	}
}

// TestSoftClose_ArchiveFailure_TranscriptUntouchedPointerCleared covers the
// archive-failure edge case on a soft close: the transcript JSONL in the
// sessions root is untouched, the
// command still exits nil (generic archive failures are non-fatal), AND —
// unlike the hard path, where the sever is skipped so a re-run can
// archive-then-sever — the DB clear still runs. The soft sever is
// unconditional because a soft-closed session name is re-spawned routinely
// (every coordinator @main reopen); skipping the clear on archive failure
// would leave the pointer set next to the deliberately preserved transcript
// and the next re-spawn would silently auto-resume the closed conversation.
//
// The hard-mode counterpart — TestHeadlessCleanup_ArchiveFailureLeavesTranscript
// in cleanup_archive_order_test.go — asserts the pointer is RETAINED under
// the same forced archive failure; together the pair proves the
// archive-failure behaviour is mode-dependent and neither assertion is
// vacuous.
func TestSoftClose_ArchiveFailure_TranscriptUntouchedPointerCleared(t *testing.T) {
	f := setupArchiveOrderFixture(t, "soft-close-archive-fail", "")

	// Plant a regular file where the archive root directory should be, so
	// archive.Run's MkdirAll fails with ENOTDIR regardless of uid.
	dataHome := os.Getenv("XDG_DATA_HOME")
	if err := os.MkdirAll(filepath.Join(dataHome, "prism"), 0o700); err != nil {
		t.Fatalf("mkdir prism data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataHome, "prism", "archive"), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("plant archive-root blocker file: %v", err)
	}

	out := captureStdoutDuringFn(t, func() {
		if err := headlessCloseSessionWithJSON(f.session, true); err != nil {
			t.Errorf("headlessCloseSessionWithJSON: %v, want nil (archive failure is non-fatal)", err)
		}
	})

	if _, err := os.Stat(f.transcript); err != nil {
		t.Errorf("transcript %s missing after failed archive on soft close (err=%v); want untouched", f.transcript, err)
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
		t.Fatal("agent_status row missing after soft close")
	}
	if st.HarnessSessionID != nil {
		t.Errorf("HarnessSessionID = %q after failed archive on soft close; want nil — the soft sever is unconditional (#2371) so a re-spawn cannot auto-resume (#2035)",
			*st.HarnessSessionID)
	}

	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(out)), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	v, ok := env["harness_session_id_cleared"]
	if !ok {
		t.Fatalf("envelope missing harness_session_id_cleared; got: %v", env)
	}
	if got, isBool := v.(bool); !isBool || !got {
		t.Errorf("harness_session_id_cleared = %v (%T), want true (the DB clear ran despite the archive failure — #2371)", v, v)
	}
}

// TestSoftClose_Rerun_Idempotent exercises the re-close cycle: a second soft
// close of the same session hits archive.ErrAlreadyExists (the first run
// wrote the archive directory for this instance), and the unconditional soft
// sever still runs — an idempotent no-op DB clear — so the command exits
// nil, the envelope reports true, and the transcript is still preserved.
// This covers the archiveErr-propagation path: soft mode returns the archive
// error alongside the sever outcome so the callers' ErrAlreadyExists
// collision-warning handling stays mode-independent.
func TestSoftClose_Rerun_Idempotent(t *testing.T) {
	f := setupArchiveOrderFixture(t, "soft-close-rerun", "")

	if err := headlessCloseSession(f.session); err != nil {
		t.Fatalf("first headlessCloseSession: %v", err)
	}

	out := captureStdoutDuringFn(t, func() {
		if err := headlessCloseSessionWithJSON(f.session, true); err != nil {
			t.Errorf("second headlessCloseSessionWithJSON: %v, want nil (re-close must be idempotent)", err)
		}
	})

	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(out)), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	v, ok := env["harness_session_id_cleared"]
	if !ok {
		t.Fatalf("envelope missing harness_session_id_cleared; got: %v", env)
	}
	if got, isBool := v.(bool); !isBool || !got {
		t.Errorf("harness_session_id_cleared = %v (%T) on re-close, want true (unconditional soft sever, idempotent no-op)", v, v)
	}
	assertSoftSeveredAfterArchive(t, f)
}
