package db_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/prismatic-koi/prism/internal/db"
)

// openMaintenanceTestDB is a local helper that mirrors openTestDB in db_test.go
// but is named distinctly to avoid duplicate-symbol collisions if the helper in
// db_test.go is ever renamed or refactored by a parallel branch. It opens a
// fresh DB in a per-test tempdir and registers Close on cleanup.
func openMaintenanceTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// rawExec opens a second connection to the test DB and executes a statement.
// Used to backdate `ended_at` / `created_at` columns in test setup (there is
// no public API for this), and to install BEFORE-DELETE triggers for the
// rollback test.
func rawExec(t *testing.T, d *db.DB, query string, args ...any) {
	t.Helper()
	rawConn, err := sql.Open("sqlite", d.Path())
	if err != nil {
		t.Fatalf("rawExec open: %v", err)
	}
	defer rawConn.Close()
	if _, err := rawConn.Exec(query, args...); err != nil {
		t.Fatalf("rawExec %q: %v", query, err)
	}
}

// countRows returns the row count of the given table.
func countRows(t *testing.T, d *db.DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// rowExists reports whether a row matching the WHERE clause exists.
func rowExists(t *testing.T, d *db.DB, query string, args ...any) bool {
	t.Helper()
	var n int
	if err := d.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("rowExists %q: %v", query, err)
	}
	return n > 0
}

// TestPrune_AllAffectedTables exercises every table that Prune now touches:
// agent_events, bus_messages (delivered + failed), sessions (with cascade
// to spawn_outcome and spawn_inputs), agent_status (with the live-counterpart
// guard), and session_groups (orphan removal).
//
// Setup: populate the DB with one old row and one recent row in every
// affected table, plus one agent_status row whose ended_at is old but whose
// instance_id still has a live sessions counterpart (must be preserved by
// the guard).
//
// Assertions after a Prune with a 24h threshold:
//   - old rows in each pruned table are gone.
//   - recent rows are preserved.
//   - spawn_outcome / spawn_inputs cascade-deletes left no orphans.
//   - the protected agent_status row (live counterpart) was preserved.
//   - the empty session_group was deleted, the still-referenced group survived.
//   - existing agent_events / bus_messages pruning behaviour is unchanged.
func TestPrune_AllAffectedTables(t *testing.T) {
	d := openMaintenanceTestDB(t)

	old := time.Now().Add(-48 * time.Hour).UnixMilli() // > 24h ago
	recent := time.Now().UnixMilli()

	// ── agent_events ────────────────────────────────────────────────
	oldEventID := uuid.New().String()
	newEventID := uuid.New().String()
	if err := d.WriteEvent(db.Event{
		ID: oldEventID, SessionName: "repo@main", Repo: "repo",
		Worktree: "/wt", Type: "state_change", Payload: "{}",
		CreatedAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("WriteEvent old: %v", err)
	}
	if err := d.WriteEvent(db.Event{
		ID: newEventID, SessionName: "repo@main", Repo: "repo",
		Worktree: "/wt", Type: "state_change", Payload: "{}",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("WriteEvent new: %v", err)
	}

	// ── bus_messages ────────────────────────────────────────────────
	// Insert four messages: old-delivered, recent-delivered, old-failed,
	// recent-failed. The two "old" rows should be deleted; the two "recent"
	// rows should be preserved. Also insert one undelivered (NULL) message
	// to verify the existing "never delete undelivered" contract.
	oldDeliveredID := uuid.New().String()
	recentDeliveredID := uuid.New().String()
	oldFailedID := uuid.New().String()
	recentFailedID := uuid.New().String()
	pendingID := uuid.New().String()
	rawExec(t, d, `
INSERT INTO bus_messages (id, from_session, to_session, to_instance_id, repo, text, urgency, sent_at, delivered_at, failed_at)
VALUES
  (?, 'a', 'b', NULL, 'r', 't', 'normal', ?, ?, NULL),
  (?, 'a', 'b', NULL, 'r', 't', 'normal', ?, ?, NULL),
  (?, 'a', 'b', NULL, 'r', 't', 'normal', ?, NULL, ?),
  (?, 'a', 'b', NULL, 'r', 't', 'normal', ?, NULL, ?),
  (?, 'a', 'b', NULL, 'r', 't', 'normal', ?, NULL, NULL)`,
		oldDeliveredID, old, old,
		recentDeliveredID, recent, recent,
		oldFailedID, old, old,
		recentFailedID, recent, recent,
		pendingID, old, // pending: sent ages ago but never delivered/failed
	)

	// ── session_groups + sessions + agent_status + spawn_inputs + spawn_outcome
	//
	// Group A: contains an OLD ended session. After Prune, both the session and
	//          the only agent_status row that references the group are gone, so
	//          the group itself should be removed.
	// Group B: contains a RECENT ended session whose status row also references
	//          the group. The group must be preserved.
	groupA, err := d.RegisterGroup("parentA")
	if err != nil {
		t.Fatalf("RegisterGroup A: %v", err)
	}
	groupB, err := d.RegisterGroup("parentB")
	if err != nil {
		t.Fatalf("RegisterGroup B: %v", err)
	}

	// ----- old session in group A -----
	iidOld := uuid.New().String()
	if err := d.UpsertStatus("oldsess", "repo", "/wt/old", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus oldsess: %v", err)
	}
	if err := d.SetInstanceID("oldsess", iidOld); err != nil {
		t.Fatalf("SetInstanceID oldsess: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID: iidOld, SessionName: "oldsess", Repo: "repo",
		Worktree: "/wt/old", Harness: "pi", GroupID: &groupA,
		StartedAt: time.Now().Add(-72 * time.Hour),
	}); err != nil {
		t.Fatalf("InsertSession oldsess: %v", err)
	}
	if err := d.UpdateSessionEnded(iidOld, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded oldsess: %v", err)
	}
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID: iidOld, CreatedAt: time.Now().Add(-72 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs oldsess: %v", err)
	}
	if err := d.WriteSpawnOutcome(iidOld); err != nil {
		t.Fatalf("WriteSpawnOutcome oldsess: %v", err)
	}
	// Backdate sessions.ended_at and agent_status.ended_at + tie to groupA.
	rawExec(t, d, "UPDATE sessions SET ended_at = ? WHERE instance_id = ?", old, iidOld)
	rawExec(t, d, "UPDATE agent_status SET ended_at = ?, group_id = ? WHERE session_name = ?", old, groupA, "oldsess")

	// ----- recent session in group B -----
	iidRecent := uuid.New().String()
	if err := d.UpsertStatus("recentsess", "repo", "/wt/recent", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus recentsess: %v", err)
	}
	if err := d.SetInstanceID("recentsess", iidRecent); err != nil {
		t.Fatalf("SetInstanceID recentsess: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID: iidRecent, SessionName: "recentsess", Repo: "repo",
		Worktree: "/wt/recent", Harness: "pi", GroupID: &groupB,
		StartedAt: time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("InsertSession recentsess: %v", err)
	}
	if err := d.UpdateSessionEnded(iidRecent, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded recentsess: %v", err)
	}
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID: iidRecent, CreatedAt: time.Now().Add(-2 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs recentsess: %v", err)
	}
	if err := d.WriteSpawnOutcome(iidRecent); err != nil {
		t.Fatalf("WriteSpawnOutcome recentsess: %v", err)
	}
	rawExec(t, d, "UPDATE agent_status SET group_id = ? WHERE session_name = ?", groupB, "recentsess")

	// ----- a live session (never ended) -----
	_ = insertActiveSession(t, d, "livesess", "repo", "/wt/live")

	// ----- agent_status with the live-counterpart guard -----
	// "protsess" was ended ages ago, but a live sessions row exists for the
	// same instance_id (simulating a restart). The protected agent_status row
	// must NOT be deleted.
	iidShared := uuid.New().String()
	if err := d.UpsertStatus("protsess", "repo", "/wt/prot", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus protsess: %v", err)
	}
	if err := d.SetInstanceID("protsess", iidShared); err != nil {
		t.Fatalf("SetInstanceID protsess: %v", err)
	}
	// Live sessions row sharing the same instance_id.
	if err := d.InsertSession(db.Session{
		InstanceID: iidShared, SessionName: "protsess", Repo: "repo",
		Worktree: "/wt/prot", Harness: "pi",
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertSession protsess (live): %v", err)
	}
	// Backdate the agent_status.ended_at to before the threshold.
	rawExec(t, d, "UPDATE agent_status SET ended_at = ? WHERE session_name = ?", old, "protsess")

	// ----- a completely orphaned session_group (no members anywhere) -----
	orphanGroup, err := d.RegisterGroup("parentOrphan")
	if err != nil {
		t.Fatalf("RegisterGroup orphan: %v", err)
	}

	// ── pre-Prune sanity counts ─────────────────────────────────────
	if got, want := countRows(t, d, "sessions"), 4; got != want {
		t.Fatalf("pre-prune sessions count: got %d, want %d", got, want)
	}
	if got, want := countRows(t, d, "spawn_outcome"), 2; got != want {
		t.Fatalf("pre-prune spawn_outcome count: got %d, want %d", got, want)
	}
	if got, want := countRows(t, d, "spawn_inputs"), 2; got != want {
		t.Fatalf("pre-prune spawn_inputs count: got %d, want %d", got, want)
	}
	if got, want := countRows(t, d, "session_groups"), 3; got != want {
		t.Fatalf("pre-prune session_groups count: got %d, want %d", got, want)
	}

	// ── run Prune ───────────────────────────────────────────────────
	if err := d.Prune(24 * time.Hour); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// ── post-Prune assertions ───────────────────────────────────────

	// agent_events: old gone, new preserved.
	if rowExists(t, d, "SELECT COUNT(*) FROM agent_events WHERE id = ?", oldEventID) {
		t.Error("agent_events: old row still present after prune")
	}
	if !rowExists(t, d, "SELECT COUNT(*) FROM agent_events WHERE id = ?", newEventID) {
		t.Error("agent_events: recent row missing after prune")
	}

	// bus_messages: old-delivered + old-failed gone; recent + pending preserved.
	for _, id := range []string{oldDeliveredID, oldFailedID} {
		if rowExists(t, d, "SELECT COUNT(*) FROM bus_messages WHERE id = ?", id) {
			t.Errorf("bus_messages: old row %q still present", id)
		}
	}
	for _, id := range []string{recentDeliveredID, recentFailedID, pendingID} {
		if !rowExists(t, d, "SELECT COUNT(*) FROM bus_messages WHERE id = ?", id) {
			t.Errorf("bus_messages: row %q was pruned (should be preserved)", id)
		}
	}

	// sessions: old gone, recent + live + protsess preserved.
	if rowExists(t, d, "SELECT COUNT(*) FROM sessions WHERE instance_id = ?", iidOld) {
		t.Error("sessions: old row still present after prune")
	}
	for _, iid := range []string{iidRecent, iidShared} {
		if !rowExists(t, d, "SELECT COUNT(*) FROM sessions WHERE instance_id = ?", iid) {
			t.Errorf("sessions: row %q missing after prune", iid)
		}
	}

	// Cascade: spawn_outcome and spawn_inputs for the old session must be gone.
	if rowExists(t, d, "SELECT COUNT(*) FROM spawn_outcome WHERE instance_id = ?", iidOld) {
		t.Error("spawn_outcome: row for old session still present (cascade did not fire)")
	}
	if rowExists(t, d, "SELECT COUNT(*) FROM spawn_inputs WHERE instance_id = ?", iidOld) {
		t.Error("spawn_inputs: row for old session still present (cascade did not fire)")
	}
	// Recent session's spawn_outcome / spawn_inputs survive.
	if !rowExists(t, d, "SELECT COUNT(*) FROM spawn_outcome WHERE instance_id = ?", iidRecent) {
		t.Error("spawn_outcome: row for recent session was pruned")
	}
	if !rowExists(t, d, "SELECT COUNT(*) FROM spawn_inputs WHERE instance_id = ?", iidRecent) {
		t.Error("spawn_inputs: row for recent session was pruned")
	}

	// No orphans: every spawn_outcome/spawn_inputs row must point to a live
	// sessions row.
	if rowExists(t, d, `
SELECT COUNT(*) FROM spawn_outcome so
LEFT JOIN sessions s ON so.instance_id = s.instance_id
WHERE s.instance_id IS NULL`) {
		t.Error("spawn_outcome has rows without a matching sessions row (orphans)")
	}
	if rowExists(t, d, `
SELECT COUNT(*) FROM spawn_inputs si
LEFT JOIN sessions s ON si.instance_id = s.instance_id
WHERE s.instance_id IS NULL`) {
		t.Error("spawn_inputs has rows without a matching sessions row (orphans)")
	}

	// agent_status: old "oldsess" row gone, protected "protsess" row
	// preserved (because a live sessions row shares its instance_id), recent
	// + live still present.
	if rowExists(t, d, "SELECT COUNT(*) FROM agent_status WHERE session_name = 'oldsess'") {
		t.Error("agent_status: oldsess row still present after prune")
	}
	if !rowExists(t, d, "SELECT COUNT(*) FROM agent_status WHERE session_name = 'protsess'") {
		t.Error("agent_status: protsess row deleted despite live sessions counterpart")
	}
	for _, name := range []string{"recentsess", "livesess"} {
		if !rowExists(t, d, "SELECT COUNT(*) FROM agent_status WHERE session_name = ?", name) {
			t.Errorf("agent_status: %q row missing after prune", name)
		}
	}

	// session_groups: groupA (all members gone) and orphanGroup gone, groupB
	// (recentsess still references it) preserved.
	for _, gid := range []string{groupA, orphanGroup} {
		if rowExists(t, d, "SELECT COUNT(*) FROM session_groups WHERE group_id = ?", gid) {
			t.Errorf("session_groups: orphan group %q still present", gid)
		}
	}
	if !rowExists(t, d, "SELECT COUNT(*) FROM session_groups WHERE group_id = ?", groupB) {
		t.Error("session_groups: groupB removed despite live member references")
	}

	// No orphan session_groups: every surviving group must still have at least
	// one referencing row in sessions or agent_status.
	if rowExists(t, d, `
SELECT COUNT(*) FROM session_groups sg
WHERE NOT EXISTS (SELECT 1 FROM sessions     WHERE group_id = sg.group_id)
  AND NOT EXISTS (SELECT 1 FROM agent_status WHERE group_id = sg.group_id)`) {
		t.Error("session_groups has orphan rows after prune")
	}
}

// TestPrune_AtomicRollback verifies that a failure on one DELETE inside Prune
// rolls back every other DELETE — partial pruning is never observable.
//
// Failure is injected by installing a BEFORE-DELETE trigger on agent_status
// that always raises an error. agent_status is pruned after sessions, so a
// successful rollback must restore the sessions, agent_events and
// bus_messages rows that would otherwise have been deleted before the
// agent_status DELETE fired.
func TestPrune_AtomicRollback(t *testing.T) {
	d := openMaintenanceTestDB(t)

	old := time.Now().Add(-48 * time.Hour).UnixMilli()

	// agent_events that Prune would otherwise delete.
	oldEventID := uuid.New().String()
	if err := d.WriteEvent(db.Event{
		ID: oldEventID, SessionName: "repo@main", Repo: "repo",
		Worktree: "/wt", Type: "state_change", Payload: "{}",
		CreatedAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	// bus_messages (delivered) that Prune would otherwise delete.
	oldBusID := uuid.New().String()
	rawExec(t, d, `
INSERT INTO bus_messages (id, from_session, to_session, repo, text, urgency, sent_at, delivered_at)
VALUES (?, 'a', 'b', 'r', 't', 'normal', ?, ?)`, oldBusID, old, old)

	// sessions + agent_status that Prune would otherwise delete.
	iid := uuid.New().String()
	if err := d.UpsertStatus("oldsess", "repo", "/wt/old", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetInstanceID("oldsess", iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID: iid, SessionName: "oldsess", Repo: "repo",
		Worktree: "/wt/old", Harness: "pi",
		StartedAt: time.Now().Add(-72 * time.Hour),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := d.UpdateSessionEnded(iid, "finished"); err != nil {
		t.Fatalf("UpdateSessionEnded: %v", err)
	}
	rawExec(t, d, "UPDATE sessions SET ended_at = ? WHERE instance_id = ?", old, iid)
	rawExec(t, d, "UPDATE agent_status SET ended_at = ? WHERE session_name = ?", old, "oldsess")

	// Install a BEFORE-DELETE trigger on agent_status that always raises.
	rawConn, err := sql.Open("sqlite", d.Path())
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { rawConn.Close() })
	if _, err := rawConn.Exec(`
		CREATE TRIGGER force_agent_status_delete_error
		BEFORE DELETE ON agent_status
		BEGIN
			SELECT RAISE(ABORT, 'injected agent_status delete failure');
		END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Prune must return an error.
	if err := d.Prune(24 * time.Hour); err == nil {
		t.Fatal("Prune: expected error from injected trigger, got nil")
	}

	// Drop the trigger so post-rollback assertions are not affected.
	if _, err := rawConn.Exec("DROP TRIGGER force_agent_status_delete_error"); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	// Every pre-Prune row must still exist — the transaction rolled back.
	if !rowExists(t, d, "SELECT COUNT(*) FROM agent_events WHERE id = ?", oldEventID) {
		t.Error("agent_events: old row missing after rollback — atomicity broken")
	}
	if !rowExists(t, d, "SELECT COUNT(*) FROM bus_messages WHERE id = ?", oldBusID) {
		t.Error("bus_messages: old row missing after rollback — atomicity broken")
	}
	if !rowExists(t, d, "SELECT COUNT(*) FROM sessions WHERE instance_id = ?", iid) {
		t.Error("sessions: old row missing after rollback — atomicity broken")
	}
	if !rowExists(t, d, "SELECT COUNT(*) FROM agent_status WHERE session_name = 'oldsess'") {
		t.Error("agent_status: old row missing after rollback — atomicity broken")
	}
}
