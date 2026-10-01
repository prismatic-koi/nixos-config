package db_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// foreignKeyViolations runs PRAGMA foreign_key_check on a second connection
// and returns one line per violation.
func foreignKeyViolations(t *testing.T, d *db.DB) []string {
	t.Helper()
	rawConn, err := sql.Open("sqlite", d.Path())
	if err != nil {
		t.Fatalf("foreign_key_check open: %v", err)
	}
	defer rawConn.Close()
	rows, err := rawConn.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			t.Fatalf("scan foreign_key_check: %v", err)
		}
		out = append(out, table+" -> "+parent)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_check: %v", err)
	}
	return out
}

// insertSessionRow inserts a sessions row with the given start time and,
// when endedAt is non-zero, stamps ended_at at that time.
func insertSessionRow(t *testing.T, d *db.DB, instanceID, sessionName string, startedAt, endedAt time.Time) {
	t.Helper()
	if err := d.InsertSession(db.Session{
		InstanceID: instanceID, SessionName: sessionName, Repo: "hass-config",
		Worktree: "/wt/hass-config/main", Harness: "pi", StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("InsertSession %s: %v", instanceID, err)
	}
	if !endedAt.IsZero() {
		rawExec(t, d, "UPDATE sessions SET ended_at = ?, end_state = 'finished' WHERE instance_id = ?",
			endedAt.UnixMilli(), instanceID)
	}
}

// writeInstanceEvent writes one agent_events row tagged with instanceID.
func writeInstanceEvent(t *testing.T, d *db.DB, sessionName, instanceID string, createdAt time.Time) string {
	t.Helper()
	id := uuid.New().String()
	iid := instanceID
	if err := d.WriteEvent(db.Event{
		ID: id, SessionName: sessionName, Repo: "hass-config",
		Worktree: "/wt/hass-config/main", Type: "state_change", Payload: "{}",
		InstanceID: &iid, CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("WriteEvent %s: %v", instanceID, err)
	}
	return id
}

// TestPrune_EndedSessionWithEventsAfterThreshold reproduces the data shape of
// issue #3027: an ended session whose ended_at is older than the threshold
// still has agent_events rows that are newer than the threshold. The
// agent_events foreign key has no ON DELETE action, so the sessions DELETE
// fails with FOREIGN KEY constraint failed (787) unless Prune first clears
// the reference on those rows.
func TestPrune_EndedSessionWithEventsAfterThreshold(t *testing.T) {
	d := openMaintenanceTestDB(t)

	const sessionName = "hass-config@main"
	now := time.Now()
	day := 24 * time.Hour

	// The ended incarnation. It ended before the threshold, but a sidecar
	// kept writing events with its instance ID after the threshold.
	endedIID := uuid.New().String()
	insertSessionRow(t, d, endedIID, sessionName, now.Add(-120*day), now.Add(-100*day))
	oldEventID := writeInstanceEvent(t, d, sessionName, endedIID, now.Add(-110*day))
	var staleEventIDs []string
	for i := 0; i < 3; i++ {
		staleEventIDs = append(staleEventIDs,
			writeInstanceEvent(t, d, sessionName, endedIID, now.Add(-time.Duration(i+1)*time.Hour)))
	}

	// The live incarnation that `prism switch` started.
	liveIID := uuid.New().String()
	insertSessionRow(t, d, liveIID, sessionName, now.Add(-2*day), time.Time{})
	liveEventID := writeInstanceEvent(t, d, sessionName, liveIID, now.Add(-time.Hour))

	// A row with ended_at IS NULL that started long before the threshold,
	// the shape of the orphan row in the issue. Prune must keep it.
	openIID := uuid.New().String()
	insertSessionRow(t, d, openIID, sessionName, now.Add(-140*day), time.Time{})
	openOldEventID := writeInstanceEvent(t, d, sessionName, openIID, now.Add(-139*day))

	if err := d.Prune(90 * day); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if rowExists(t, d, "SELECT COUNT(*) FROM sessions WHERE instance_id = ?", endedIID) {
		t.Error("sessions: the ended session is still present after Prune")
	}
	if !rowExists(t, d, "SELECT COUNT(*) FROM sessions WHERE instance_id = ?", liveIID) {
		t.Error("sessions: the live session was deleted")
	}
	if !rowExists(t, d, "SELECT COUNT(*) FROM sessions WHERE instance_id = ?", openIID) {
		t.Error("sessions: a row with ended_at IS NULL was deleted")
	}

	if rowExists(t, d, "SELECT COUNT(*) FROM agent_events WHERE id = ?", oldEventID) {
		t.Error("agent_events: an event older than the threshold is still present")
	}
	if rowExists(t, d, "SELECT COUNT(*) FROM agent_events WHERE id = ?", openOldEventID) {
		t.Error("agent_events: an event older than the threshold is still present")
	}
	for _, id := range staleEventIDs {
		if !rowExists(t, d, "SELECT COUNT(*) FROM agent_events WHERE id = ? AND instance_id IS NULL", id) {
			t.Errorf("agent_events %s: want the row kept with instance_id NULL", id)
		}
	}
	if !rowExists(t, d, "SELECT COUNT(*) FROM agent_events WHERE id = ? AND instance_id = ?", liveEventID, liveIID) {
		t.Error("agent_events: the live session's event lost its instance_id")
	}

	if rowExists(t, d, `
SELECT COUNT(*) FROM agent_events
 WHERE instance_id IS NOT NULL
   AND instance_id NOT IN (SELECT instance_id FROM sessions)`) {
		t.Error("agent_events: a row references a sessions.instance_id that does not exist")
	}
	if v := foreignKeyViolations(t, d); len(v) != 0 {
		t.Errorf("PRAGMA foreign_key_check: want no rows, got %v", v)
	}
}
