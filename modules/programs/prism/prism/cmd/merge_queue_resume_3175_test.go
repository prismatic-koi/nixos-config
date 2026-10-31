package cmd

// Tests for issue #3175: the merge queue of a coordinator session survives
// a host shutdown (and the restore at login) and the stale-sidecar path of
// prism switch.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// seedCoordinatorQueue seeds an active coordinator sessionName with an open
// incarnation and the merge queue prs, and returns its status and
// instance ID.
func seedCoordinatorQueue(t *testing.T, d *db.DB, sessionName string, prs ...int) (db.Status, string) {
	t.Helper()
	status := seedStatus(t, d, sessionName, "/wt", nil)
	iid := uuid.New().String()
	seedIncarnation(t, d, sessionName, iid, "tier-heavy", time.Time{})
	status.InstanceID = &iid
	enqueueCoordinatorPRs(t, d, sessionName, iid, prs...)
	return status, iid
}

// shutDownSidecar does the DB writes of the Shutdown of the coordinator
// sidecar of iid, as at a host shutdown.
func shutDownSidecar(t *testing.T, d *db.DB, iid string) {
	t.Helper()
	if err := d.EndIncarnation(iid, "finished"); err != nil {
		t.Fatalf("EndIncarnation: %v", err)
	}
}

func assertMergeRow(t *testing.T, d *db.DB, pr int, status, iid string) {
	t.Helper()
	row, err := d.PendingMergeByPR(pr, "testrepo")
	if err != nil || row == nil {
		t.Fatalf("PendingMergeByPR(%d): %v", pr, err)
	}
	if row.Status != status || row.InstanceID != iid {
		t.Errorf("PR %d: status %q, instance %q, want %q, %q", pr, row.Status, row.InstanceID, status, iid)
	}
}

// TestResumeMergeQueue_AfterHostShutdown runs the restore steps at login
// without tmux. The old sidecar stopped at the host shutdown, so no row is
// watching when restore starts, and no sidecar runs.
func TestResumeMergeQueue_AfterHostShutdown(t *testing.T) {
	const sessionName = "testrepo@main"
	d := openRestoreTestDB(t)
	status, iid := seedCoordinatorQueue(t, d, sessionName, 3, 1, 2)
	shutDownSidecar(t, d, iid)

	queued, err := d.WatchingMerges(sessionName)
	if err != nil || len(queued) != 0 {
		t.Fatalf("WatchingMerges = %v, %v, want no rows", queued, err)
	}
	if queued, err = withMergesAbandonedByStop(d, sessionName, iid, queued); err != nil {
		t.Fatalf("withMergesAbandonedByStop: %v", err)
	}
	got := restoredInstanceID(d, status, "coordinator", true)
	if got != iid {
		t.Fatalf("restoredInstanceID = %q, want the same instance %q", got, iid)
	}
	stdout, _ := captureStdoutStderr(t, func() {
		resumeMergeQueue(d, "restore", sessionName, got, queued)
	})

	assertQueue(t, d, sessionName, iid, 3, 1, 2)
	if want := "merge queue resumed: PR #3, PR #1, PR #2"; !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestRestoreSession_AfterHostShutdown_ResumesMergeQueue restores a
// coordinator whose sidecar stopped at a host shutdown, before restore. The
// restored session keeps its instance, and its queue is watching again in
// the same order.
func TestRestoreSession_AfterHostShutdown_ResumesMergeQueue(t *testing.T) {
	d := startRestoreTest(t)
	const sessionName = "testrepo@host-shutdown-3175"
	status := seedStatus(t, d, sessionName, t.TempDir(), nil)
	iid := uuid.New().String()
	seedIncarnation(t, d, sessionName, iid, "tier-heavy", time.Time{})
	status.InstanceID = &iid
	enqueueCoordinatorPRs(t, d, sessionName, iid, 30, 10, 20)
	shutDownSidecar(t, d, iid)

	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	if got := currentInstanceID(t, d, sessionName); got != iid {
		t.Fatalf("agent_status.instance_id = %q, want the same instance %q", got, iid)
	}
	assertQueue(t, d, sessionName, iid, 30, 10, 20)
}

// stubSwitchStop replaces the sidecar stop of the stale-sidecar path of
// prism switch with one that reports gone.
func stubSwitchStop(t *testing.T, gone bool) {
	t.Helper()
	prev := switchStopSidecar
	switchStopSidecar = func(string, time.Duration) error {
		if gone {
			return nil
		}
		return errors.New("fake stop: the old sidecar still runs")
	}
	t.Cleanup(func() { switchStopSidecar = prev })
}

// startNewSwitchIncarnation does what the tmux-session-start seed does
// inside session.Create when the old sidecar ended its sessions row: it
// mints a new instance ID.
func startNewSwitchIncarnation(t *testing.T, d *db.DB, sessionName string) string {
	t.Helper()
	iid := uuid.New().String()
	if err := d.SetInstanceID(sessionName, iid); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID: iid, SessionName: sessionName, Repo: "testrepo", Worktree: "/wt", Harness: "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	return iid
}

func currentStatusRow(t *testing.T, d *db.DB, sessionName string) *db.Status {
	t.Helper()
	st, err := d.CurrentStatus(sessionName)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus %q: %v", sessionName, err)
	}
	return st
}

// TestSwitchStaleSidecar_ResumesMergeQueue runs the stale-sidecar path of
// prism switch against a stub sidecar that ends its incarnation on SIGTERM,
// as a real Shutdown does. The new instance that the seed mints must own
// the queue, in the same order. A row that the coordinator cancelled must
// not come back.
func TestSwitchStaleSidecar_ResumesMergeQueue(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const sessionName = "testrepo@switch-stale-3175"
	d := openRestoreTestDB(t)
	_, oldIID := seedCoordinatorQueue(t, d, sessionName, 12, 11, 13)
	if _, err := d.CancelMerge(13, "testrepo", oldIID); err != nil {
		t.Fatalf("CancelMerge: %v", err)
	}
	st := currentStatusRow(t, d, sessionName)
	stubDone := startSlowStopStub(t, d, sessionName, oldIID, "0s")

	queued, err := stopStaleSidecar(d, sessionName, st)
	select {
	case ws := <-stubDone:
		if ws.Signaled() {
			t.Errorf("old sidecar wait status = %v, want a clean exit after SIGTERM", ws)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old sidecar is still running after the stop")
	}
	if err != nil {
		t.Fatalf("stopStaleSidecar: %v, want no error after the old sidecar exited", err)
	}
	if rows, _ := d.WatchingMerges(sessionName); len(rows) != 0 {
		t.Fatalf("watching rows after the stop = %d, want none: the stub abandons them", len(rows))
	}

	newIID := startNewSwitchIncarnation(t, d, sessionName)
	stdout, _ := captureStdoutStderr(t, func() { resumeStaleMergeQueue(d, sessionName, queued) })

	assertQueue(t, d, sessionName, newIID, 12, 11)
	assertMergeRow(t, d, 13, "cancelled", oldIID)
	if want := "merge queue resumed: PR #12, PR #11"; !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestSwitchStaleSidecar_StoppedBeforeSwitch covers an old sidecar that a
// stop before the switch ended, while agent_status stayed active. Its rows
// are abandoned before the switch reads the queue, so the switch must find
// them through the stop that ended the incarnation.
func TestSwitchStaleSidecar_StoppedBeforeSwitch(t *testing.T) {
	const sessionName = "testrepo@switch-stopped-3175"
	d := openRestoreTestDB(t)
	_, oldIID := seedCoordinatorQueue(t, d, sessionName, 22, 21)
	shutDownSidecar(t, d, oldIID)
	stubSwitchStop(t, true)

	queued, err := stopStaleSidecar(d, sessionName, currentStatusRow(t, d, sessionName))
	if err != nil {
		t.Fatalf("stopStaleSidecar: %v", err)
	}
	newIID := startNewSwitchIncarnation(t, d, sessionName)
	_, _ = captureStdoutStderr(t, func() { resumeStaleMergeQueue(d, sessionName, queued) })

	assertQueue(t, d, sessionName, newIID, 22, 21)
}

// TestSwitchStaleSidecar_StopResults checks the stop results of the
// stale-sidecar path. Only a stop that proves that no process that can be
// the old sidecar runs may resume the queue. Without that proof, the switch
// returns an error, and the queue does not change (#3273).
func TestSwitchStaleSidecar_StopResults(t *testing.T) {
	for _, tc := range []struct {
		desc string
		gone bool
	}{
		{"old sidecar gone", true},
		{"no proof that the old sidecar is gone", false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			const sessionName = "testrepo@switch-result-3175"
			d := openRestoreTestDB(t)
			_, oldIID := seedCoordinatorQueue(t, d, sessionName, 31, 32)
			stubSwitchStop(t, tc.gone)

			queued, err := stopStaleSidecar(d, sessionName, currentStatusRow(t, d, sessionName))
			if !tc.gone {
				if err == nil {
					t.Fatal("stopStaleSidecar: no error, want an error without proof that the old sidecar is gone")
				}
				for _, want := range []string{"not restarted", "merge queue is not resumed: PR #31, PR #32"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want %q", err, want)
					}
				}
				assertQueue(t, d, sessionName, oldIID, 31, 32)
				return
			}
			if err != nil {
				t.Fatalf("stopStaleSidecar: %v", err)
			}
			newIID := startNewSwitchIncarnation(t, d, sessionName)
			_, _ = captureStdoutStderr(t, func() { resumeStaleMergeQueue(d, sessionName, queued) })
			assertQueue(t, d, sessionName, newIID, 31, 32)
		})
	}
}
