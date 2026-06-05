// Tests for issue #2134 — agent_status lifecycle bookkeeping during cleanup.
//
// `prism cleanup --yes --session <name>` must, after the worktree/branch/tmux
// teardown, leave the agent_status row in a fully cleaned-up state:
//
//   - ended_at stamped to a time at or after the cleanup invocation
//   - harness_port set to NULL ("released" sentinel)
//   - harness_session_id set to NULL
//
// And the --json envelope must report these three outcomes via the new fields:
// ended_at_stamped, harness_port_released, harness_session_id_cleared. Each
// field is `true` on success (including idempotent no-ops) or a string
// describing the failure.
//
// These tests exercise the worktree-less paths (review subsessions) and the
// already-dead tmux path that the issue calls out as the leaky surface, plus
// the idempotent re-cleanup AC and the unknown-session security AC.

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// captureStdoutDuringFn redirects os.Stdout for the duration of fn and
// returns whatever was written. Output is drained concurrently to defend
// against the kernel-pipe-buffer deadlock documented in
// docs/stdout-capture-testing.md — although cleanup JSON envelopes are well
// under 64 KiB, following the convention keeps this test resilient if the
// envelope ever grows.
func captureStdoutDuringFn(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	// Drain concurrently so a write larger than the pipe buffer cannot
	// deadlock the writer.
	done := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(r)
		done <- buf
	}()

	defer func() {
		os.Stdout = orig
	}()
	fn()
	_ = w.Close()
	return string(<-done)
}

// seedRowWithLifecycleFields creates an agent_status row with harness_port and
// harness_session_id populated, mirroring the live-session state before
// cleanup. Returns the seeded port for later assertions.
func seedRowWithLifecycleFields(t *testing.T, dbFile, session, worktree, harnessSessionID string) int {
	t.Helper()
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := d.UpsertStatus(session, "prism-test", worktree, "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	port, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}
	if err := d.UpdateHarnessSessionID(session, harnessSessionID); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}
	return port
}

// assertLifecycleColumnsCleared asserts that the row for session has ended_at
// stamped at-or-after notBefore, harness_port == NULL, and harness_session_id
// == NULL. Fails the test with a clear per-column message on each violation.
func assertLifecycleColumnsCleared(t *testing.T, dbFile, session string, notBefore time.Time) {
	t.Helper()
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d.Close()
	status, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus(%q): %v", session, err)
	}
	if status == nil {
		t.Fatalf("CurrentStatus(%q) returned nil — row missing", session)
	}
	if status.EndedAt == nil {
		t.Errorf("ended_at: got nil, want stamped at-or-after %v", notBefore)
	} else if status.EndedAt.Before(notBefore) {
		t.Errorf("ended_at: got %v, want at-or-after %v", *status.EndedAt, notBefore)
	}
	if status.HarnessPort != nil {
		t.Errorf("harness_port: got %d, want nil (released)", *status.HarnessPort)
	}
	if status.HarnessSessionID != nil {
		t.Errorf("harness_session_id: got %q, want nil (cleared)", *status.HarnessSessionID)
	}
}

// TestHeadlessCleanup_StampsLifecycleColumns is the core AC: after cleanup of
// a worker session without a worktree (the issue's primary reproduction
// shape), all three lifecycle columns are cleared.
func TestHeadlessCleanup_StampsLifecycleColumns(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	session := "prism-test@2134-worker"
	port := seedRowWithLifecycleFields(t, dbFile, session, "", "abcd-1234")
	t.Logf("seeded session=%q harness_port=%d harness_session_id=%q", session, port, "abcd-1234")

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	before := time.Now().Add(-1 * time.Second) // tolerate millisecond truncation in DB
	if err := headlessCleanup(session, "2134-worker", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	assertLifecycleColumnsCleared(t, dbFile, session, before)
}

// TestHeadlessCleanup_ReviewSubsession verifies the AC for review subsessions
// — sessions without a worktree, named <parent>~review-<N>-<agent>. The
// cleanup command goes through the same `headlessCleanupWithJSON` path with
// worktreePath="" because the review subsession reuses the parent's worktree
// and tmux is already dead by cleanup time.
func TestHeadlessCleanup_ReviewSubsession(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	parent := "prism-test@2134-worker"
	// Review subsession name shape: <parent>~review-<N>-<agent>.
	child := parent + "~review-1-review-code"
	// Seed parent (so cascade reads see a coherent group) and the child.
	_ = seedRowWithLifecycleFields(t, dbFile, parent, "", "parent-hsid")
	port := seedRowWithLifecycleFields(t, dbFile, child, "", "child-hsid")
	t.Logf("seeded child=%q harness_port=%d", child, port)

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	before := time.Now().Add(-1 * time.Second)
	// Cleanup the child directly — mimics `prism cleanup --yes --session <child>`.
	if err := headlessCleanup(child, "2134-worker~review-1-review-code", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	assertLifecycleColumnsCleared(t, dbFile, child, before)
}

// TestHeadlessCleanup_DeadTmuxStillClearsDB verifies the AC: "All three
// updates happen regardless of whether the tmux session was alive at cleanup
// time (already-dead tmux sessions still get the DB cleanup)."
//
// withNoopTmux replaces tmux with a stub that exits 0 for every command — so
// from the cleanup function's point of view, the session is "already dead"
// (KillSession is a no-op, ListClients returns nothing). The DB updates must
// still happen.
func TestHeadlessCleanup_DeadTmuxStillClearsDB(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	session := "prism-test@2134-dead-tmux"
	_ = seedRowWithLifecycleFields(t, dbFile, session, "", "dead-tmux-hsid")

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	before := time.Now().Add(-1 * time.Second)
	if err := headlessCleanup(session, "2134-dead-tmux", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	assertLifecycleColumnsCleared(t, dbFile, session, before)
}

// TestHeadlessCleanup_JSONEnvelopeReportsAllOutcomes verifies the AC:
// "prism cleanup --yes --json envelope includes per-update fields (e.g.
// ended_at_stamped, harness_port_released, harness_session_id_cleared)
// reporting true on success or an error description on failure."
//
// Captures stdout from headlessCleanupWithJSON in json mode, unmarshals the
// envelope, and asserts the three new fields each carry `true`.
func TestHeadlessCleanup_JSONEnvelopeReportsAllOutcomes(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	session := "prism-test@2134-json"
	_ = seedRowWithLifecycleFields(t, dbFile, session, "", "json-hsid")

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	out := captureStdoutDuringFn(t, func() {
		if err := headlessCleanupWithJSON(session, "2134-json", "", "", true); err != nil {
			t.Fatalf("headlessCleanupWithJSON: %v", err)
		}
	})

	// The envelope is a single JSON object on stdout. Parse it permissively
	// (map[string]any) so we can assert the heterogeneous `true | string`
	// value shape without committing the test to a Go struct definition.
	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(out)), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}

	for _, field := range []string{"ended_at_stamped", "harness_port_released", "harness_session_id_cleared"} {
		v, ok := env[field]
		if !ok {
			t.Errorf("envelope missing field %q; got: %v", field, env)
			continue
		}
		// Success path: each value must be the boolean `true`. (Failure
		// paths exercise the string branch — see other tests.)
		if got, isBool := v.(bool); !isBool || !got {
			t.Errorf("field %q: got %v (%T), want true", field, v, v)
		}
	}
}

// TestHeadlessCleanup_Idempotent verifies the AC: "Re-running prism cleanup
// --yes --session <name> on an already-ended session is idempotent — does not
// error, and produces a JSON envelope indicating each resource was already in
// the cleaned-up state."
//
// Runs cleanup once to put the row into the cleaned-up state, then runs it
// again and checks the second invocation does not error and reports the
// three lifecycle fields as `true` (i.e. "in the cleaned-up state").
func TestHeadlessCleanup_Idempotent(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	session := "prism-test@2134-idempotent"
	_ = seedRowWithLifecycleFields(t, dbFile, session, "", "idempotent-hsid")

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// First cleanup — the real one.
	if err := headlessCleanup(session, "2134-idempotent", "", ""); err != nil {
		t.Fatalf("first headlessCleanup: %v", err)
	}

	// Second cleanup — should be idempotent.
	out := captureStdoutDuringFn(t, func() {
		if err := headlessCleanupWithJSON(session, "2134-idempotent", "", "", true); err != nil {
			t.Errorf("second headlessCleanup: got error %v, want nil (idempotent)", err)
		}
	})

	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(out)), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}

	for _, field := range []string{"ended_at_stamped", "harness_port_released", "harness_session_id_cleared"} {
		v, ok := env[field]
		if !ok {
			t.Errorf("envelope missing field %q on idempotent re-run; got: %v", field, env)
			continue
		}
		if got, isBool := v.(bool); !isBool || !got {
			t.Errorf("idempotent re-run: field %q got %v (%T), want true", field, v, v)
		}
	}

	// Sanity: the row is still in the cleaned-up state after two runs.
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d.Close()
	status, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("CurrentStatus returned nil — row missing after idempotent re-run")
	}
	if status.EndedAt == nil {
		t.Errorf("ended_at: got nil, want stamped (idempotent re-run should not unstamp)")
	}
	if status.HarnessPort != nil {
		t.Errorf("harness_port: got %d, want nil after idempotent re-run", *status.HarnessPort)
	}
	if status.HarnessSessionID != nil {
		t.Errorf("harness_session_id: got %q, want nil after idempotent re-run", *status.HarnessSessionID)
	}
}

// TestHeadlessCleanup_JSONEnvelopeOnDBOpenFailure verifies that when the DB
// cannot be opened, the JSON envelope surfaces the failure in all three
// lifecycle fields rather than emitting `null`. This is the exact silent
// failure mode the issue calls out: prior to the fix, openDB errors led to a
// successful-looking envelope with no signal that bookkeeping never ran.
//
// Exercises this by pointing SetTestDBPath at a path that is a directory
// (not a regular file), which causes db.Open to fail.
func TestHeadlessCleanup_JSONEnvelopeOnDBOpenFailure(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	// Make dbPath() resolve to a directory, which db.Open cannot open as
	// a SQLite file. Any subsequent CurrentStatus / SetEnded calls in the
	// `else` branch are skipped.
	badDBPath := filepath.Join(t.TempDir(), "not-a-file")
	if err := os.MkdirAll(badDBPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	SetTestDBPath(badDBPath)
	t.Cleanup(func() { SetTestDBPath("") })

	session := "prism-test@2134-dbfail"
	out := captureStdoutDuringFn(t, func() {
		// Note: we expect cleanup to "succeed" (return nil) even when the
		// DB is unavailable — the worktree/tmux teardown still ran. The
		// JSON envelope is what surfaces the DB skip.
		_ = headlessCleanupWithJSON(session, "2134-dbfail", "", "", true)
	})

	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(out)), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}

	for _, field := range []string{"ended_at_stamped", "harness_port_released", "harness_session_id_cleared"} {
		v, ok := env[field]
		if !ok {
			t.Errorf("envelope missing field %q; got: %v", field, env)
			continue
		}
		// On DB-open failure each field must carry a non-empty error
		// description string — never `true` (which would falsely imply
		// the bookkeeping succeeded) and never `null` (which would be the
		// pre-fix silent-failure mode).
		s, isStr := v.(string)
		if !isStr {
			t.Errorf("field %q: got %v (%T), want a non-empty error string", field, v, v)
			continue
		}
		if s == "" {
			t.Errorf("field %q: got empty string, want a non-empty error description", field)
		}
	}
}

// TestCleanupCmd_UnknownSessionReturnsError verifies the security AC:
// "Cleanup does not stamp ended_at on a row whose <name> doesn't match an
// existing row (no implicit row creation). Unknown sessions return a clear
// error."
//
// The validation lives at the top of cleanupCmd.RunE, but we test it via the
// underlying check: cleanupCmd uses CurrentStatus to detect unknown sessions
// and constructs an enumerated error. Here we assert that with an empty DB
// (no rows), the row for a never-seen session remains absent after a cleanup
// attempt — no implicit creation of a phantom row.
func TestCleanupCmd_UnknownSessionDoesNotCreateRow(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	// Open and close to create the schema, but seed no rows.
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	unknown := "prism-test@never-existed"

	// Run headlessCleanup directly — it does not validate session existence
	// (that's cleanupCmd.RunE's job), so it MUST NOT create a row.
	if err := headlessCleanup(unknown, "never-existed", "", ""); err != nil {
		// headlessCleanup is best-effort — returning nil here is the
		// current contract. The security property is that no row gets
		// created, asserted below.
		t.Logf("headlessCleanup returned %v (informational only)", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	got, err := d2.CurrentStatus(unknown)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if got != nil {
		t.Errorf("CurrentStatus(%q) returned non-nil row %+v — cleanup must NOT create a phantom agent_status row", unknown, got)
	}
}

// TestSessionsList_ExcludesEndedRows verifies the AC: "prism sessions list
// (no flags) does not include rows where ended_at IS NOT NULL — verified by
// spawning, cleaning up, and confirming the cleaned-up row drops out of the
// list output."
//
// Sessions list is sourced from d.AllActiveStatus which filters
// `WHERE ended_at IS NULL`. This test confirms a cleaned-up row is excluded
// from that query result.
func TestSessionsList_ExcludesEndedRows(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	session := "prism-test@2134-list-filter"
	_ = seedRowWithLifecycleFields(t, dbFile, session, "", "list-filter-hsid")

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Sanity: before cleanup the row is in AllActiveStatus.
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	pre, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus (pre): %v", err)
	}
	d.Close()
	if !containsSession(pre, session) {
		t.Fatal("seeded session not in AllActiveStatus pre-cleanup — test setup invalid")
	}

	if err := headlessCleanup(session, "2134-list-filter", "", ""); err != nil {
		t.Fatalf("headlessCleanup: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	post, err := d2.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus (post): %v", err)
	}
	if containsSession(post, session) {
		names := make([]string, 0, len(post))
		for _, s := range post {
			names = append(names, s.SessionName)
		}
		t.Errorf("cleaned-up session %q is still in AllActiveStatus: %v", session, names)
	}
}

// containsSession reports whether any Status in ss has the given session
// name. Helper kept local to this test file to avoid leaking into the wider
// cmd package surface.
func containsSession(ss []db.Status, name string) bool {
	for _, s := range ss {
		if s.SessionName == name {
			return true
		}
	}
	return false
}

// TestHeadlessCleanup_ReleasePortError verifies the per-update reporting on
// failure: when ReleasePort errors (the row doesn't exist), the JSON
// envelope surfaces the error string rather than reporting success.
//
// This is defence-in-depth — cleanupCmd.RunE's pre-flight validation should
// catch the unknown-session case before headlessCleanup runs. But if
// validation is bypassed (e.g. a future refactor), the JSON envelope must
// still report the failure.
func TestHeadlessCleanup_ReleasePortErrorSurfacedInJSON(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	// Open just to materialise the schema; seed no row for the test session.
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	missing := "prism-test@2134-no-row"
	out := captureStdoutDuringFn(t, func() {
		if err := headlessCleanupWithJSON(missing, "2134-no-row", "", "", true); err != nil {
			t.Fatalf("headlessCleanupWithJSON: %v", err)
		}
	})

	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(out)), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}

	// ReleasePort errors when n == 0 (no row matched). The envelope's
	// harness_port_released field must therefore be a string (the error
	// description) rather than `true`.
	v, ok := env["harness_port_released"]
	if !ok {
		t.Fatalf("envelope missing harness_port_released; got: %v", env)
	}
	if _, isStr := v.(string); !isStr {
		t.Errorf("harness_port_released: got %v (%T) for missing row, want non-empty error string", v, v)
	}
}

// debugDumpEnvelope marshals env for inclusion in test failure messages.
// Helper kept here so a test failure shows a stable, deterministic JSON
// rendering rather than Go's map-order-randomised %v.
func debugDumpEnvelope(t *testing.T, env map[string]any) string {
	t.Helper()
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v (marshal err: %v)", env, err)
	}
	return string(b)
}

var _ = debugDumpEnvelope // keep helper available without forcing every test to use it
