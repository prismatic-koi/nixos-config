package cmd

// Tests for issue #3166: prism restore (and so prism restart) keeps the
// merge queue of a coordinator session. The Shutdown of the old sidecar
// abandons the watching rows, and restore gives them back to the restored
// sidecar in the same order.

import (
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// enqueueCoordinatorPRs enqueues prs, in that queue order, for the
// coordinator sessionName with instance ID iid.
func enqueueCoordinatorPRs(t *testing.T, d *db.DB, sessionName, iid string, prs ...int) {
	t.Helper()
	for _, pr := range prs {
		if _, err := d.EnqueueMerge(pr, "testrepo", sessionName, iid, nil); err != nil {
			t.Fatalf("EnqueueMerge(%d): %v", pr, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// assertQueue checks that the watching rows of sessionName are prs, in that
// order, and that iid owns each of them.
func assertQueue(t *testing.T, d *db.DB, sessionName, iid string, prs ...int) {
	t.Helper()
	rows, err := d.WatchingMerges(sessionName)
	if err != nil {
		t.Fatalf("WatchingMerges: %v", err)
	}
	got := make([]int, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.PR)
		if r.InstanceID != iid {
			t.Errorf("PR %d instance_id = %q, want %q", r.PR, r.InstanceID, iid)
		}
	}
	if len(got) != len(prs) {
		t.Fatalf("watching PRs = %v, want %v", got, prs)
	}
	for i := range prs {
		if got[i] != prs[i] {
			t.Fatalf("watching PRs = %v, want %v", got, prs)
		}
	}
}

// TestResumeMergeQueue runs the restore steps around the stop of the old
// sidecar without tmux: read the queue, do the Shutdown writes of the old
// sidecar, pick the instance ID, and resume.
func TestResumeMergeQueue(t *testing.T) {
	const sessionName = "testrepo@main"

	for _, tc := range []struct {
		name    string
		stopped bool
	}{
		{"old sidecar stopped", true},
		{"old sidecar did not stop", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openRestoreTestDB(t)
			status := seedStatus(t, d, sessionName, "/wt", nil)
			oldIID := uuid.New().String()
			seedIncarnation(t, d, sessionName, oldIID, "tier-heavy", time.Time{})
			status.InstanceID = &oldIID
			enqueueCoordinatorPRs(t, d, sessionName, oldIID, 3, 1, 2)

			queued, err := d.WatchingMerges(sessionName)
			if err != nil {
				t.Fatalf("WatchingMerges: %v", err)
			}
			if err := d.AbandonWatchingMerges(oldIID); err != nil {
				t.Fatalf("AbandonWatchingMerges: %v", err)
			}
			if err := d.UpdateSessionEnded(oldIID, "finished"); err != nil {
				t.Fatalf("UpdateSessionEnded: %v", err)
			}

			iid := restoredInstanceID(d, status, "coordinator", tc.stopped)
			if tc.stopped != (iid == oldIID) {
				t.Fatalf("restoredInstanceID = %q (old %q), stopped = %v", iid, oldIID, tc.stopped)
			}
			resumeMergeQueue(d, "restore", sessionName, iid, queued, true)

			assertQueue(t, d, sessionName, iid, 3, 1, 2)
		})
	}
}

// TestRestoreSession_ResumesMergeQueue restores a coordinator whose old
// sidecar abandons the queue when it stops, as a real Shutdown does. The
// restored session keeps its instance, and the queue is watching again in
// the same order.
func TestRestoreSession_ResumesMergeQueue(t *testing.T) {
	d := startRestoreTest(t)
	const sessionName = "testrepo@restart-queue-3166"
	status := seedStatus(t, d, sessionName, t.TempDir(), nil)
	iid := uuid.New().String()
	seedIncarnation(t, d, sessionName, iid, "tier-heavy", time.Time{})
	status.InstanceID = &iid
	enqueueCoordinatorPRs(t, d, sessionName, iid, 20, 10)

	stubDone := startSlowStopStub(t, d, sessionName, iid, "0s")

	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	select {
	case ws := <-stubDone:
		if ws.Signaled() {
			t.Errorf("old sidecar wait status = %v, want a clean exit after SIGTERM", ws)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old sidecar is still running after restore")
	}
	if got := currentInstanceID(t, d, sessionName); got != iid {
		t.Fatalf("agent_status.instance_id = %q, want the same instance %q", got, iid)
	}
	assertQueue(t, d, sessionName, iid, 20, 10)
}

// TestRestoreSession_OldSidecarDoesNotStop_ResumesMergeQueue restores a
// coordinator whose old sidecar abandons the queue and then does not stop
// inside the wait. Restore starts a new instance, and the new instance owns
// the queue.
func TestRestoreSession_OldSidecarDoesNotStop_ResumesMergeQueue(t *testing.T) {
	d := startRestoreTest(t)
	prevTimeout := restoreSidecarStopTimeout
	restoreSidecarStopTimeout = 300 * time.Millisecond
	t.Cleanup(func() { restoreSidecarStopTimeout = prevTimeout })

	const sessionName = "testrepo@restart-queue-slow-3166"
	status := seedStatus(t, d, sessionName, t.TempDir(), nil)
	oldIID := uuid.New().String()
	seedIncarnation(t, d, sessionName, oldIID, "tier-heavy", time.Time{})
	status.InstanceID = &oldIID
	enqueueCoordinatorPRs(t, d, sessionName, oldIID, 7, 5)

	stubDone := startSlowStopStub(t, d, sessionName, oldIID, "1s")

	t.Setenv("PRISM_CMD_TEST_STUB", "1")
	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}
	t.Cleanup(func() { session.KillSidecar(sessionName) })

	select {
	case ws := <-stubDone:
		if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Errorf("old sidecar wait status = %v, want killed by SIGKILL", ws)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old sidecar is still running after restore")
	}
	newIID := currentInstanceID(t, d, sessionName)
	if newIID == "" || newIID == oldIID {
		t.Fatalf("agent_status.instance_id = %q, want a new instance (old %q)", newIID, oldIID)
	}
	assertQueue(t, d, sessionName, newIID, 7, 5)
}
