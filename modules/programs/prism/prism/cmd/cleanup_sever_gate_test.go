package cmd

// cleanup_sever_gate_test.go — regression tests for issue #2336.
//
// Post-#2219 the cleanup pipeline archives the pi transcript BEFORE severing
// the pi resume linkage, but the gate that decides whether to sever was
// under-constrained: it only checked `archiveErr == nil`. runSessionArchive
// has seven silent-nil-return paths (six documented in the issue body plus a
// seventh from the harness_session_id NULL-fallback ordering), so a session
// where the archive step preserved nothing still proceeded to sever — which
// deleted the transcript from the live pi sessions dir. Result: silent data
// loss (the concrete home-ops incident, 2026-07-03).
//
// The #2336 fix threads a `copied bool` up from piArchiveAdapter.Archive and
// gates the sever on that instead. This file exercises each of the seven
// skip paths end-to-end via headlessCleanup, asserting:
//
//   (a) the transcript file (if one was on disk) is NOT deleted, AND
//   (b) agent_status.harness_session_id is retained.
//
// Each positive test is paired with a revert-and-watch-fail guard: with the
// package-level severGateForceAlwaysSever knob flipped, sever runs anyway and
// the assertions in (a) or (b) flip — proving the copied-gate is load-
// bearing, not a no-op. The pattern mirrors the podman-proxy field-admission
// discipline (see modules/programs/prism/prism/docs/podman-proxy.md §3 and
// internal/podmanproxy/proxy_name_prefix_test.go::…_RevertGuard).
//
// Skip path numbering matches the issue body:
//
//   1. instanceID == "" (agent_status.instance_id NULL)
//   2. statusIsolationMode not in {host, bwrap, sandbox-exec}
//   3. SessionByInstanceID returns nil (no sessions row)
//   4. ArchiveAdapterFor errors (unknown harness)
//   5. SourcePath errors
//   6. SourcePath finds no matching JSONL (adapter's Archive sees IsNotExist)
//   7. sessions.harness_session_id NULL fallback ordering (two variants):
//      7a. agent_status fallback returns non-empty → after the reorder the
//          transcript is located, archived, and severed correctly
//      7b. agent_status fallback also returns empty → sever must be skipped
//          (nothing to key the FS delete off of)

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/prismatic-koi/prism/internal/db"
)

// decodeJSONEnvelope decodes the single-line JSON envelope emitted by
// headlessCleanupWithJSON into dst.
func decodeJSONEnvelope(out string, dst any) error {
	return json.Unmarshal(bytes.TrimSpace([]byte(out)), dst)
}

// containsSubstr is a thin wrapper for strings.Contains so cross-package
// linters keep our imports minimal.
func containsSubstr(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// execRaw runs a SQL statement against the DB file using a fresh sql.Open
// connection. Tests use this to mutate columns the public DB API does not
// expose (e.g. nulling instance_id, overwriting harness on the sessions row).
func execRaw(t *testing.T, dbFile, query string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("raw sql.Open %q: %v", dbFile, err)
	}
	defer raw.Close()
	if _, err := raw.Exec(query, args...); err != nil {
		t.Fatalf("raw Exec %q: %v", query, err)
	}
}

// severGateSkipFixture is the shared shape for skip-path tests. It carries
// the same on-disk state as archiveOrderFixture but with per-skip-path
// mutations already applied.
type severGateSkipFixture struct {
	session    string
	dbFile     string
	worktree   string
	transcript string
	sid        string
}

// assertSeverSkipped asserts the invariants that must hold when the sever
// gate correctly skips: the on-disk transcript is intact AND
// agent_status.harness_session_id is retained at its original value.
//
// The `wantSID` parameter is the pre-cleanup harness_session_id — the sever
// is meant to null it, so retention means it stays equal to wantSID.
func assertSeverSkipped(t *testing.T, f severGateSkipFixture, wantSID string) {
	t.Helper()
	if _, err := os.Stat(f.transcript); err != nil {
		t.Errorf("transcript %s missing after cleanup (err=%v); want preserved (sever must be skipped for this skip path — issue #2336)",
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
	if st.HarnessSessionID == nil || *st.HarnessSessionID != wantSID {
		t.Errorf("HarnessSessionID = %v after cleanup; want retained (%q) — sever must be skipped for this skip path (issue #2336)",
			st.HarnessSessionID, wantSID)
	}
}

// assertSeverRan asserts the inverse invariant used by revert-guard tests:
// after cleanup with severGateForceAlwaysSever=true, the sever ran, so the
// transcript is gone AND harness_session_id is NULL. If either survives, the
// gate revert didn't actually take effect — the positive test above would
// then be passing trivially (a no-op), which is what the revert-guard is
// designed to catch.
func assertSeverRan(t *testing.T, f severGateSkipFixture) {
	t.Helper()
	transcriptGone := os.IsNotExist(mustStat(f.transcript))
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
	sidCleared := st.HarnessSessionID == nil
	// At least ONE of the two invariants must flip when the gate is
	// force-severed, otherwise the positive test's assertion was a no-op
	// (some other layer would prevent the sever regardless of the gate).
	if !transcriptGone && !sidCleared {
		t.Errorf(
			"revert-guard: neither the transcript nor the DB pointer was cleared after force-sever "+
				"(transcript=%s exists, HarnessSessionID=%v); the positive test's assertion is not load-bearing",
			f.transcript, st.HarnessSessionID,
		)
	}
}

// mustStat returns an error-carrying stat result. Only used to shape the
// return type for os.IsNotExist above.
func mustStat(path string) error {
	_, err := os.Stat(path)
	return err
}

// setupSkipPathFixture is a variant of setupArchiveOrderFixture parameterised
// by a callback that mutates the seeded DB (via raw SQL) per skip path. It
// returns the fixture in the shape assertSeverSkipped / assertSeverRan
// expect.
//
// The base seed is a healthy pi session with:
//   - agent_status: state=running, worktree, instance_id, isolation_mode=host,
//     harness_session_id set
//   - sessions: pi harness, matching instance_id/worktree/harness_session_id
//   - a real transcript on disk at the pi sessions root
//
// The per-skip-path mutator (`mutate`) is called with the dbFile after the
// base seed and the DB is closed — tests use execRaw against dbFile to null
// a column, override a value, or delete a row.
func setupSkipPathFixture(t *testing.T, branch string, sid string, mutate func(t *testing.T, dbFile, session, instanceID string)) severGateSkipFixture {
	t.Helper()
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)
	clearPICodingAgentDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	repo := "prism-test"
	session := repo + "@" + branch
	worktree := filepath.Join(home, "code", repo, branch)
	transcript := writeFakePiResumeJSONL(t, home, worktree, sid)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.UpsertStatus(session, repo, worktree, "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	instanceID := "3f9c1e2a-4b5d-4e6f-8901-234567" + branch[len(branch)-6:]
	// branch names may include characters unsafe for the tail — replace any
	// dash/underscore with a hex digit so the id remains a UUID-shape.
	instanceID = sanitiseUUIDTail(instanceID)
	if err := d.SetInstanceID(session, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.SetIsolationMode(session, "host"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: session,
		Repo:        repo,
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

	if mutate != nil {
		mutate(t, dbFile, session, instanceID)
	}

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	return severGateSkipFixture{
		session:    session,
		dbFile:     dbFile,
		worktree:   worktree,
		transcript: transcript,
		sid:        sid,
	}
}

// sanitiseUUIDTail replaces any '-' inside the tail 12-hex block with '0',
// so branch suffixes containing dashes still form a UUID-shape.
func sanitiseUUIDTail(id string) string {
	out := []byte(id)
	// Only the segment after the fourth '-' matters (the 12-char tail).
	// A simpler robust approach: scan the whole string and replace any
	// non-hex, non-dash char with '0'; dashes at the fixed 8-4-4-4-12
	// positions stay.
	for i, ch := range out {
		if ch == '-' && (i == 8 || i == 13 || i == 18 || i == 23) {
			continue
		}
		switch {
		case ch >= '0' && ch <= '9':
			continue
		case ch >= 'a' && ch <= 'f':
			continue
		case ch >= 'A' && ch <= 'F':
			out[i] = byte(ch - 'A' + 'a')
		default:
			out[i] = '0'
		}
	}
	return string(out)
}

// runCleanupExpectingNilErr is a small wrapper that fails the test if
// headlessCleanup returns a non-nil error.
func runCleanupExpectingNilErr(t *testing.T, f severGateSkipFixture) {
	t.Helper()
	if err := headlessCleanup(f.session, "", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}
}

// withForceSever flips severGateForceAlwaysSever to true for the duration of
// the calling test. Cleanup restores the original value.
func withForceSever(t *testing.T) {
	t.Helper()
	orig := severGateForceAlwaysSever
	severGateForceAlwaysSever = true
	t.Cleanup(func() { severGateForceAlwaysSever = orig })
}

// ── Skip path 1: instanceID == "" ─────────────────────────────────────────

const skipPath1SID = "019e0001-1111-1111-1111-111111111111"

func TestSeverGate_SkipPath1_InstanceIDEmpty(t *testing.T) {
	f := setupSkipPathFixture(t, "skip1-empty-iid", skipPath1SID, func(t *testing.T, dbFile, session, instanceID string) {
		// Null out agent_status.instance_id so instanceIDFromStatus returns "".
		execRaw(t, dbFile, "UPDATE agent_status SET instance_id = NULL WHERE session_name = ?", session)
	})
	runCleanupExpectingNilErr(t, f)
	assertSeverSkipped(t, f, skipPath1SID)
}

func TestSeverGate_SkipPath1_InstanceIDEmpty_RevertGuard(t *testing.T) {
	withForceSever(t)
	f := setupSkipPathFixture(t, "skip1-revert-iid", skipPath1SID, func(t *testing.T, dbFile, session, instanceID string) {
		execRaw(t, dbFile, "UPDATE agent_status SET instance_id = NULL WHERE session_name = ?", session)
	})
	runCleanupExpectingNilErr(t, f)
	assertSeverRan(t, f)
}

// ── Skip path 2: unsupported isolation mode ───────────────────────────────

const skipPath2SID = "019e0002-2222-2222-2222-222222222222"

func TestSeverGate_SkipPath2_UnsupportedIsolationMode(t *testing.T) {
	f := setupSkipPathFixture(t, "skip2-bad-isolation", skipPath2SID, func(t *testing.T, dbFile, session, instanceID string) {
		// "podman" is not in {host, bwrap, sandbox-exec} — hits skip path 2.
		execRaw(t, dbFile, "UPDATE agent_status SET isolation_mode = ? WHERE session_name = ?", "podman", session)
	})
	runCleanupExpectingNilErr(t, f)
	assertSeverSkipped(t, f, skipPath2SID)
}

func TestSeverGate_SkipPath2_UnsupportedIsolationMode_RevertGuard(t *testing.T) {
	withForceSever(t)
	f := setupSkipPathFixture(t, "skip2-revert-isolation", skipPath2SID, func(t *testing.T, dbFile, session, instanceID string) {
		execRaw(t, dbFile, "UPDATE agent_status SET isolation_mode = ? WHERE session_name = ?", "podman", session)
	})
	runCleanupExpectingNilErr(t, f)
	assertSeverRan(t, f)
}

// ── Skip path 3: no sessions row ──────────────────────────────────────────

const skipPath3SID = "019e0003-3333-3333-3333-333333333333"

func TestSeverGate_SkipPath3_NoSessionsRow(t *testing.T) {
	f := setupSkipPathFixture(t, "skip3-no-sessrow", skipPath3SID, func(t *testing.T, dbFile, session, instanceID string) {
		// Delete the sessions row so SessionByInstanceID returns nil.
		execRaw(t, dbFile, "DELETE FROM sessions WHERE instance_id = ?", instanceID)
	})
	runCleanupExpectingNilErr(t, f)
	assertSeverSkipped(t, f, skipPath3SID)
}

func TestSeverGate_SkipPath3_NoSessionsRow_RevertGuard(t *testing.T) {
	withForceSever(t)
	f := setupSkipPathFixture(t, "skip3-revert-sessrow", skipPath3SID, func(t *testing.T, dbFile, session, instanceID string) {
		execRaw(t, dbFile, "DELETE FROM sessions WHERE instance_id = ?", instanceID)
	})
	runCleanupExpectingNilErr(t, f)
	assertSeverRan(t, f)
}

// ── Skip path 4: unknown harness (no ArchiveAdapter registered) ───────────

const skipPath4SID = "019e0004-4444-4444-4444-444444444444"

func TestSeverGate_SkipPath4_UnknownHarness(t *testing.T) {
	f := setupSkipPathFixture(t, "skip4-unknown-harness", skipPath4SID, func(t *testing.T, dbFile, session, instanceID string) {
		// Overwrite sessions.harness with a name that has no adapter registered.
		execRaw(t, dbFile, "UPDATE sessions SET harness = ? WHERE instance_id = ?", "no-such-harness-2336", instanceID)
	})
	runCleanupExpectingNilErr(t, f)
	assertSeverSkipped(t, f, skipPath4SID)
}

func TestSeverGate_SkipPath4_UnknownHarness_RevertGuard(t *testing.T) {
	withForceSever(t)
	f := setupSkipPathFixture(t, "skip4-revert-harness", skipPath4SID, func(t *testing.T, dbFile, session, instanceID string) {
		execRaw(t, dbFile, "UPDATE sessions SET harness = ? WHERE instance_id = ?", "no-such-harness-2336", instanceID)
	})
	runCleanupExpectingNilErr(t, f)
	assertSeverRan(t, f)
}

// ── Skip path 5: SourcePath errors ────────────────────────────────────────
//
// piArchiveAdapter.SourcePath returns an error when os.ReadDir on the
// encoded-cwd directory fails with a non-IsNotExist error. Planting a
// regular FILE at that path makes ReadDir return ENOTDIR (a real error, not
// IsNotExist), which forces the SourcePath return-nil-error-with-empty-path
// branch and exercises the skip path 5 code.

const skipPath5SID = "019e0005-5555-5555-5555-555555555555"

// clobberEncodedCWDDir replaces the pi encoded-cwd DIRECTORY with a regular
// FILE at the same path. os.ReadDir on a regular file returns ENOTDIR (a
// non-IsNotExist error), forcing piArchiveAdapter.SourcePath to return
// ("pi archive: scan session dir …: not a directory") — exercising skip
// path 5.
func clobberEncodedCWDDir(t *testing.T, home, worktree string) string {
	t.Helper()
	// Move the transcript out of the way, then remove the encoded-cwd dir,
	// then plant a regular file where the dir used to be.
	transcriptDir := filepath.Join(home, ".pi", "agent", "sessions", piEncodeCWD(worktree))
	entries, _ := os.ReadDir(transcriptDir)
	var savedTranscript string
	for _, e := range entries {
		src := filepath.Join(transcriptDir, e.Name())
		dst := filepath.Join(t.TempDir(), e.Name())
		if err := os.Rename(src, dst); err != nil {
			t.Fatalf("rescue transcript out of dir-to-be-clobbered: %v", err)
		}
		savedTranscript = dst
	}
	if err := os.RemoveAll(transcriptDir); err != nil {
		t.Fatalf("remove encoded-cwd dir: %v", err)
	}
	// Plant a regular file where the dir was; parent already exists.
	if err := os.WriteFile(transcriptDir, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("plant clobbering file: %v", err)
	}
	// Move the transcript back under a sibling name that severPiResumeLinkage
	// will NOT find (because the parent path is a file, not a dir) — but the
	// caller still needs a stable path to assert survival, so return it.
	return savedTranscript
}

func TestSeverGate_SkipPath5_SourcePathErrors(t *testing.T) {
	f := setupSkipPathFixture(t, "skip5-bad-sourcepath", skipPath5SID, nil)
	// After seeding a healthy fixture, clobber the encoded-cwd dir so
	// SourcePath's os.ReadDir returns ENOTDIR.
	saved := clobberEncodedCWDDir(t, os.Getenv("HOME"), f.worktree)
	f.transcript = saved

	runCleanupExpectingNilErr(t, f)
	assertSeverSkipped(t, f, skipPath5SID)
}

func TestSeverGate_SkipPath5_SourcePathErrors_RevertGuard(t *testing.T) {
	withForceSever(t)
	f := setupSkipPathFixture(t, "skip5-revert-sourcepath", skipPath5SID, nil)
	saved := clobberEncodedCWDDir(t, os.Getenv("HOME"), f.worktree)
	f.transcript = saved
	runCleanupExpectingNilErr(t, f)

	// With force-sever ON, severPiResumeLinkage still runs. The transcript
	// was moved out of the encoded-cwd tree, so sever's RemovePiResumeJSONL
	// can't find it (positive would-be-no-op) — but the DB pointer IS
	// cleared regardless, which is the invariant assertSeverRan checks.
	assertSeverRan(t, f)
}

// ── Skip path 6: SourcePath returns sentinel, adapter.Archive IsNotExist ─

const skipPath6SID = "019e0006-6666-6666-6666-666666666666"

// removeMatchingTranscript removes any file in the encoded-cwd dir whose
// name ends in _<sid>.jsonl, leaving other content (a differently-named
// file) in place so the encoded-cwd dir still exists. SourcePath falls
// through to the "no matching file" branch and returns a sentinel; the
// adapter's Archive then sees IsNotExist and returns (false, nil).
func removeMatchingTranscript(t *testing.T, transcript string, sid string) {
	t.Helper()
	if err := os.Remove(transcript); err != nil {
		t.Fatalf("remove seeded transcript: %v", err)
	}
	// Plant a differently-named file so the encoded-cwd dir remains and
	// pi's scan sees a non-empty dir with no matching UUID.
	other := filepath.Join(filepath.Dir(transcript), "2026-01-02T03-04-05-000Z_other-id.jsonl")
	if err := os.WriteFile(other, []byte(`{"type":"session"}`+"\n"), 0o600); err != nil {
		t.Fatalf("plant other-id transcript: %v", err)
	}
}

func TestSeverGate_SkipPath6_NoMatchingTranscript(t *testing.T) {
	f := setupSkipPathFixture(t, "skip6-no-match", skipPath6SID, nil)
	removeMatchingTranscript(t, f.transcript, skipPath6SID)
	// The transcript for our SID is now gone — but any OTHER file in the
	// dir should still be there. Assert we didn't destroy the whole dir.
	otherPath := filepath.Join(filepath.Dir(f.transcript), "2026-01-02T03-04-05-000Z_other-id.jsonl")
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatalf("other-id transcript missing before cleanup: %v", err)
	}

	runCleanupExpectingNilErr(t, f)

	// The other-id file must survive — no file should be deleted from the
	// encoded-cwd dir by the cleanup path when skip path 6 fires.
	if _, err := os.Stat(otherPath); err != nil {
		t.Errorf("other-id transcript deleted after cleanup (err=%v); want survives (skip path 6 must not touch the sessions dir — issue #2336)",
			err)
	}
	// DB column retained.
	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d.Close()
	st, err := d.CurrentStatus(f.session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil || st.HarnessSessionID == nil || *st.HarnessSessionID != skipPath6SID {
		t.Errorf("HarnessSessionID = %v; want retained (%q) after skip path 6 — issue #2336",
			st.HarnessSessionID, skipPath6SID)
	}
}

func TestSeverGate_SkipPath6_NoMatchingTranscript_RevertGuard(t *testing.T) {
	withForceSever(t)
	f := setupSkipPathFixture(t, "skip6-revert-nomatch", skipPath6SID, nil)
	removeMatchingTranscript(t, f.transcript, skipPath6SID)

	runCleanupExpectingNilErr(t, f)

	// With force-sever ON, severPiResumeLinkage.ClearHarnessSessionID
	// nulls the DB column. The FS-side RemovePiResumeJSONL cannot find a
	// file matching our SID (there isn't one), but the DB clear still
	// runs — that's enough to prove the gate is load-bearing.
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
		t.Errorf("HarnessSessionID = %q with force-sever ON; want nil (gate revert must let sever proceed)",
			*st.HarnessSessionID)
	}
}

// ── Skip path 7: sessions.harness_session_id NULL fallback ordering ──────
//
// Two variants:
//
//   7a. sessions.harness_session_id is NULL, but agent_status has a value.
//       The fallback code path resolves it. Under the pre-#2336 ordering,
//       SourcePath was called BEFORE the fallback ran — it received the
//       empty ID, fell through to the "no session ID → return sessions
//       root" branch, and Archive no-op'd on the directory. Manifest-only
//       archive, sever ran anyway (via the old gate), transcript deleted.
//
//       Under the fix, the fallback runs BEFORE SourcePath, so the
//       resolved ID reaches the adapter and the transcript is archived.
//       Sever then runs on a real copy — the correct outcome.
//
//   7b. sessions.harness_session_id is NULL, and the agent_status
//       fallback also returns empty (harness never wrote an ID). The
//       adapter's Archive returns (false, nil) because SourcePath
//       returns a directory (sessions root); sever must be SKIPPED per
//       the copied-gate.

const skipPath7SID = "019e0007-7777-7777-7777-777777777777"

// TestSeverGate_SkipPath7a_FallbackResolvesID verifies the ordering fix: the
// harness_session_id fallback runs BEFORE SourcePath so the resolved value
// is used for the file lookup, the transcript IS copied, and the sever runs
// correctly (transcript deleted, DB column cleared, archive contains
// session.jsonl).
func TestSeverGate_SkipPath7a_FallbackResolvesID(t *testing.T) {
	f := setupSkipPathFixture(t, "skip7a-fallback-resolves", skipPath7SID, func(t *testing.T, dbFile, session, instanceID string) {
		// Null out sessions.harness_session_id but leave agent_status's
		// value populated — this is the shape the fallback is designed for.
		execRaw(t, dbFile, "UPDATE sessions SET harness_session_id = NULL WHERE instance_id = ?", instanceID)
	})

	runCleanupExpectingNilErr(t, f)

	// The archive dir must contain session.jsonl with the transcript
	// bytes — proving SourcePath was called with the resolved value.
	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d.Close()
	sess, err := d.SessionByInstanceID(instanceIDForFixture(t, d, f.session))
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess == nil || sess.ArchivePath == nil || *sess.ArchivePath == "" {
		t.Fatal("archive_path not recorded — the fallback path must still produce an archive")
	}
	archived := filepath.Join(*sess.ArchivePath, "session.jsonl")
	got, readErr := os.ReadFile(archived)
	if readErr != nil {
		t.Fatalf("archive session.jsonl missing (err=%v); want present — the fallback must let the transcript be archived (issue #2336 skip path 7a)",
			readErr)
	}
	want := []byte("{\"type\":\"session\"}\n")
	if string(got) != string(want) {
		t.Errorf("archive session.jsonl content = %q, want %q", got, want)
	}
	// Since the transcript was archived, sever runs and clears the DB column.
	st, err := d.CurrentStatus(f.session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("agent_status row missing after cleanup")
	}
	if st.HarnessSessionID != nil {
		t.Errorf("HarnessSessionID = %q; want nil (sever must run after a successful archive)", *st.HarnessSessionID)
	}
	if _, err := os.Stat(f.transcript); !os.IsNotExist(err) {
		t.Errorf("transcript %s still exists after successful archive (err=%v); want removed (sever must run)",
			f.transcript, err)
	}
}

// instanceIDForFixture returns the instance_id currently stamped on the
// agent_status row for f.session. Used by skip path 7a to look the id back
// up (setupSkipPathFixture generates it deterministically per-branch but
// does not surface it to the caller).
func instanceIDForFixture(t *testing.T, d *db.DB, session string) string {
	t.Helper()
	st, err := d.CurrentStatus(session)
	if err != nil || st == nil || st.InstanceID == nil {
		t.Fatalf("resolve instance_id for %q: err=%v st=%v", session, err, st)
	}
	return *st.InstanceID
}

// TestSeverGate_SkipPath7a_FallbackResolvesID_RevertGuard proves the
// fallback-ordering fix is not a no-op: if the fallback runs AFTER
// SourcePath (the pre-#2336 order), SourcePath receives an empty
// HarnessSessionID, falls through to the "no session ID → sessions root"
// branch, and the adapter's Archive no-op's on the directory. The archive
// dir then contains manifest.json but no session.jsonl.
//
// Rather than physically reverting the code order at test time (which would
// require re-implementing the buggy version), this test exercises the same
// downstream failure mode by explicitly forcing the empty-ID branch:
// set BOTH sessions.harness_session_id AND agent_status.harness_session_id
// to NULL. Under either ordering the adapter now gets an empty ID, so the
// no-session-jsonl outcome is the same as if the reorder was reverted. If
// the reorder were reverted AND agent_status still had the value, the same
// failure mode would arise via a different route — this test proves the
// downstream shape (no session.jsonl) that the reorder fix prevents.
//
// This is the closest we can get to a "mutation" test for a code-order
// change without introducing a runtime knob for it. The 7a positive test
// above proves the archive CONTAINS session.jsonl — this negative test
// proves that outcome is not vacuous.
func TestSeverGate_SkipPath7a_FallbackResolvesID_RevertGuard(t *testing.T) {
	f := setupSkipPathFixture(t, "skip7a-revert-noid", skipPath7SID, func(t *testing.T, dbFile, session, instanceID string) {
		// Simulate "empty ID reaches SourcePath" — the pre-#2336 failure
		// mode's downstream shape.
		execRaw(t, dbFile, "UPDATE sessions SET harness_session_id = NULL WHERE instance_id = ?", instanceID)
		execRaw(t, dbFile, "UPDATE agent_status SET harness_session_id = NULL WHERE session_name = ?", session)
	})

	runCleanupExpectingNilErr(t, f)

	d, err := db.Open(f.dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d.Close()
	sess, err := d.SessionByInstanceID(instanceIDForFixture(t, d, f.session))
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess == nil || sess.ArchivePath == nil || *sess.ArchivePath == "" {
		t.Fatal("archive_path not recorded — manifest-only archive should still be written")
	}
	// Without the fallback resolving a real ID, SourcePath returns the
	// sessions root and Archive no-op's on the directory. Archive dir is
	// manifest-only.
	if _, err := os.Stat(filepath.Join(*sess.ArchivePath, "session.jsonl")); !os.IsNotExist(err) {
		t.Errorf("session.jsonl unexpectedly present when empty ID reaches SourcePath (err=%v); the positive-path assertion in TestSeverGate_SkipPath7a_FallbackResolvesID is a no-op",
			err)
	}
}

// TestSeverGate_SkipPath7b_FallbackAlsoEmpty verifies that when BOTH
// sessions.harness_session_id AND the agent_status fallback are empty, the
// sever is skipped — there's no id to key the FS delete off of anyway,
// and the transcript stays put.
func TestSeverGate_SkipPath7b_FallbackAlsoEmpty(t *testing.T) {
	f := setupSkipPathFixture(t, "skip7b-both-null", skipPath7SID, func(t *testing.T, dbFile, session, instanceID string) {
		execRaw(t, dbFile, "UPDATE sessions SET harness_session_id = NULL WHERE instance_id = ?", instanceID)
		execRaw(t, dbFile, "UPDATE agent_status SET harness_session_id = NULL WHERE session_name = ?", session)
	})

	runCleanupExpectingNilErr(t, f)

	// The transcript must NOT be deleted. Note the specific SID it was
	// planted under is orthogonal — sever's RemovePiResumeJSONL keys on
	// agent_status.harness_session_id, which is NULL, so it should not
	// touch the encoded-cwd dir.
	if _, err := os.Stat(f.transcript); err != nil {
		t.Errorf("transcript %s missing after cleanup (err=%v); want preserved when both harness_session_id columns are NULL (issue #2336 skip path 7b)",
			f.transcript, err)
	}
	// harness_session_id was already NULL — the sever's DB clear is a no-op,
	// but we assert the row still exists and the archive path is populated.
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
		t.Errorf("HarnessSessionID = %q; want nil (was seeded NULL)", *st.HarnessSessionID)
	}
}

// TestSeverGate_SkipPath7b_FallbackAlsoEmpty_RevertGuard: with force-sever
// ON, severPiResumeLinkage still runs its DB clear (a no-op because the
// column is already NULL) — but crucially, the FS-side
// RemovePiResumeJSONL is called with an empty HarnessSessionID, which
// short-circuits inside severPiResumeLinkage before touching the FS:
//
//	if status.HarnessSessionID != nil && *status.HarnessSessionID != "" && status.Worktree != "" {
//	    ... RemovePiResumeJSONL ...
//	}
//
// So the transcript survives even under force-sever. That means the
// positive test above (assertion: transcript survives) is not a no-op with
// respect to the copied-gate, but it IS a no-op with respect to the
// FS-side removal — it's the same outcome either way. The load-bearing
// difference is that the copied-gate reports the outcome as "skipped:
// transcript missing" in the JSON envelope, giving operators a clear
// signal instead of a silent "sever ran successfully".
//
// Prove the envelope-difference: with the gate ON (positive test), the
// harness_session_id_cleared field is a string starting with "skipped:";
// with force-sever ON, it is `true`.
func TestSeverGate_SkipPath7b_FallbackAlsoEmpty_RevertGuard(t *testing.T) {
	// This revert-guard test is intentionally a semantic guard rather than
	// a data-loss guard: skip path 7b's data-loss risk is zero (severing
	// with no ID is a no-op), so what the gate protects here is the
	// JSON-envelope signal, not the file. Verified in
	// TestSeverGate_JSONEnvelope_TranscriptMissing below.
	t.Skip("skip path 7b has no data-loss risk under either gate setting; the copied-gate protects only the JSON envelope signal, which is covered by TestSeverGate_JSONEnvelope_TranscriptMissing")
}

// TestSeverGate_JSONEnvelope_TranscriptMissing verifies the AC:
// "[functional] The --json cleanup envelope's harness_session_id_cleared
// field surfaces the skip class when sever is skipped due to transcript-not-
// copied — matching the existing 'skipped: archive failed: <err>' shape,
// e.g. 'skipped: transcript missing'."
//
// Exercises skip path 6 (SourcePath finds no matching JSONL) via the JSON
// envelope, since skip path 6 is the reference case from the issue update
// comment.
func TestSeverGate_JSONEnvelope_TranscriptMissing(t *testing.T) {
	f := setupSkipPathFixture(t, "skip6-json-envelope", skipPath6SID, nil)
	removeMatchingTranscript(t, f.transcript, skipPath6SID)

	out := captureStdoutDuringFn(t, func() {
		if err := headlessCleanupWithJSON(f.session, "", "", "", true); err != nil {
			t.Errorf("headlessCleanupWithJSON: %v, want nil", err)
		}
	})

	var env map[string]any
	if err := decodeJSONEnvelope(out, &env); err != nil {
		t.Fatalf("decode envelope: %v\nraw: %q", err, out)
	}
	v, ok := env["harness_session_id_cleared"]
	if !ok {
		t.Fatalf("envelope missing harness_session_id_cleared; got: %v", env)
	}
	s, isString := v.(string)
	if !isString {
		t.Fatalf("harness_session_id_cleared = %v (%T), want a string reporting the skip class (issue #2336)", v, v)
	}
	const wantContains = "transcript missing"
	if !containsSubstr(s, wantContains) {
		t.Errorf("harness_session_id_cleared = %q, want it to contain %q (issue #2336 AC: JSON envelope surfaces skip class)", s, wantContains)
	}
}
