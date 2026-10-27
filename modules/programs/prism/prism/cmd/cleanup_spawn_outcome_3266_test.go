package cmd

// Tests for #3266: each cleanup path writes the spawn_outcome aggregates of
// the parent incarnation from the instance ID of the snapshot. The
// session-closed hook can clear agent_status.instance_id before the cascade
// runs, and then the cascade query does not find the parent.
// TestCleanupPaths_SnapshotBeforeTmuxKill_3213 holds the case where the hook
// runs first.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// spawnOutcomeRows returns the number of spawn_outcome rows of instanceID
// that have aggregated_at, and the number of all spawn_outcome rows.
func spawnOutcomeRows(t *testing.T, dbFile, instanceID string) (aggregated, total int) {
	t.Helper()
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM spawn_outcome WHERE instance_id = ? AND aggregated_at IS NOT NULL", instanceID,
	).Scan(&aggregated); err != nil {
		t.Fatalf("count aggregated spawn_outcome rows: %v", err)
	}
	if err := d.QueryRow("SELECT COUNT(*) FROM spawn_outcome").Scan(&total); err != nil {
		t.Fatalf("count spawn_outcome rows: %v", err)
	}
	return aggregated, total
}

// AC (#3266): when the hook has not run, the incarnation gets one aggregate
// row only.
func TestCleanupPaths_ParentSpawnOutcomeNoHook_3266(t *testing.T) {
	for _, path := range snapshotCleanupPaths {
		t.Run(path.name, func(t *testing.T) {
			f, run := path.seed(t)
			withHookAfterTmuxKill(t, func(session string) {
				if st := readStatus(t, f.dbFile, session); st.InstanceID == nil || *st.InstanceID != archiveOrderIID {
					t.Fatalf("instance_id = %v after the tmux kill, want %s", st.InstanceID, archiveOrderIID)
				}
			})

			if err := run(); err != nil {
				t.Fatalf("cleanup: %v", err)
			}

			if aggregated, total := spawnOutcomeRows(t, f.dbFile, archiveOrderIID); aggregated != 1 || total != 1 {
				t.Errorf("spawn_outcome rows: %d aggregated for %s, %d in total, want 1 and 1", aggregated, archiveOrderIID, total)
			}
		})
	}
}

// AC (#3266): a session that has no instance ID before the tmux kill gets
// no spawn_outcome row, and the cleanup does not fail.
func TestCleanupPaths_NoInstanceIDNoSpawnOutcome_3266(t *testing.T) {
	for _, path := range snapshotCleanupPaths {
		t.Run(path.name, func(t *testing.T) {
			f, run := path.seed(t)
			d, err := db.Open(f.dbFile)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			if err := d.ClearInstanceID(f.session); err != nil {
				t.Fatalf("ClearInstanceID: %v", err)
			}
			d.Close()
			withHookStops(t, func(string) bool { return true }, func(*db.DB, string) []string { return nil })
			withHookAfterTmuxKill(t, func(session string) { runTmuxSessionEnd(t, session) })

			if err := run(); err != nil {
				t.Fatalf("cleanup: %v", err)
			}

			if _, total := spawnOutcomeRows(t, f.dbFile, archiveOrderIID); total != 0 {
				t.Errorf("spawn_outcome rows = %d, want 0 for a session with no instance ID", total)
			}
			if readStatus(t, f.dbFile, f.session).EndedAt == nil {
				t.Error("agent_status row open, want ended")
			}
		})
	}
}
