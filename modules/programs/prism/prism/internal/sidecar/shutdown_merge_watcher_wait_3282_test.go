package sidecar

import (
	"context"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/mergequeue"
)

// startFakeMergeWatcher installs a merge-queue watcher that runs body
// after the cancel, as a watcher that is in a tick at the stop does. It
// returns the channel that closes when body returns.
func startFakeMergeWatcher(sc *Sidecar, body func()) chan struct{} {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		body()
	}()
	sc.mu.Lock()
	sc.mergeWatcherCancel = cancel
	sc.mergeWatcherDone = done
	sc.mu.Unlock()
	return done
}

func coordinatorWithQueuedPR(t *testing.T, iid string, pr int) *Sidecar {
	t.Helper()
	sc, _ := newTestSidecar(t)
	d := sc.cfg.DB
	sc.setInstanceID(iid)
	if err := d.InsertSession(db.Session{
		InstanceID: iid, SessionName: sc.cfg.SessionName, Repo: sc.cfg.Repo, Worktree: sc.cfg.Worktree, Harness: "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if _, err := d.EnqueueMerge(pr, sc.cfg.Repo, sc.cfg.SessionName, iid, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	return sc
}

// TestShutdown_WaitsForTheMergeWatcher covers a stop that comes while the
// watcher ends a row (#3282). Shutdown waits for the watcher before it
// abandons the queue, so the terminal write and its notice stay, and the
// process does not exit while the watcher sends the notification.
func TestShutdown_WaitsForTheMergeWatcher(t *testing.T) {
	const pr = 3282
	sc := coordinatorWithQueuedPR(t, "inst-shutdown-3282", pr)
	d := sc.cfg.DB
	done := startFakeMergeWatcher(sc, func() {
		time.Sleep(50 * time.Millisecond)
		if _, err := d.TerminateMergeWithNotice(pr, sc.cfg.Repo, "merged", "", "PR #3282 merged."); err != nil {
			t.Errorf("TerminateMergeWithNotice: %v", err)
		}
	})

	sc.Shutdown()

	select {
	case <-done:
	default:
		t.Fatal("Shutdown returned before the merge-queue watcher returned")
	}
	row, err := d.PendingMergeByPR(pr, sc.cfg.Repo)
	if err != nil || row == nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "merged" {
		t.Errorf("status = %q, want merged: the abandon of the stop came before the write of the watcher", row.Status)
	}
	if notices, err := d.PendingMergeNotices(sc.cfg.SessionName); err != nil || len(notices) != 1 {
		t.Errorf("pending notices = %+v, %v, want the notice of the watcher", notices, err)
	}
}

// TestShutdown_MergeWatcherWaitIsBounded covers a watcher that does not
// return. Shutdown stops waiting after mergequeue.StopWait and abandons
// the queue.
func TestShutdown_MergeWatcherWaitIsBounded(t *testing.T) {
	const pr = 3283
	sc := coordinatorWithQueuedPR(t, "inst-shutdown-3282-hung", pr)
	release := make(chan struct{})
	defer close(release)
	startFakeMergeWatcher(sc, func() { <-release })

	start := time.Now()
	sc.Shutdown()
	if elapsed := time.Since(start); elapsed < mergequeue.StopWait || elapsed > 5*mergequeue.StopWait {
		t.Errorf("Shutdown took %v, want about mergequeue.StopWait (%v)", elapsed, mergequeue.StopWait)
	}
	row, err := sc.cfg.DB.PendingMergeByPR(pr, sc.cfg.Repo)
	if err != nil || row == nil || row.Status != "abandoned" {
		t.Errorf("row after Shutdown = %+v, %v, want abandoned", row, err)
	}
}
