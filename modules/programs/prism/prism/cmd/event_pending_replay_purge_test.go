package cmd

// event_pending_replay_purge_test.go — regression tests for the round-2
// review-context finding on PR #2365: respawning on the same branch name
// (a supported flow via #2094) must not resurrect coordinator directives
// buffered against a previous incarnation.
//
// The load-bearing hook lives in cmd/event.go's tmux-session-start
// handler, which calls DeletePendingReplayDeliveriesForSession after
// ClearEnded. This test exercises the handler end-to-end via cobra to
// catch any regression that removes the purge call.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestEventTmuxSessionStart_PurgesStalePendingReplay is the AC:
// after a previous incarnation buffered pending-replay rows, a
// tmux-session-start event on the same session name must purge them
// so restorePendingReplayFromDB on the fresh sidecar sees an empty
// buffer.
func TestEventTmuxSessionStart_PurgesStalePendingReplay(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	const session = "myrepo@respawn-test"
	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Seed a pending-replay row from a previous incarnation. Use a recent
	// timestamp so the 90-day time-based Prune sweep inside
	// tmux-session-start does NOT concurrently delete it — we want to
	// isolate the per-session purge as the load-bearing hook here.
	if _, err := d.InsertPendingReplayDelivery(db.PendingReplayRow{
		SessionName: session,
		DeliveryID:  "stale-directive",
		Text:        "coordinator's stale reply",
		DeliverAs:   "steer",
		QueuedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("seed stale delivery: %v", err)
	}
	// Pre-check: the row is present.
	if n, _ := d.CountPendingReplayDeliveries(session); n != 1 {
		t.Fatalf("pre-check: seed row count = %d, want 1", n)
	}
	d.Close()

	// Drive the tmux-session-start handler through cobra — same path a
	// fresh spawn on this session name would take.
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })
	rootCmd.SetArgs([]string{
		"event", "tmux-session-start",
		"--session", session,
		"--worktree", worktree,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("tmux-session-start Execute: %v", err)
	}

	// Post-check: the row is gone.
	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	if n, _ := d2.CountPendingReplayDeliveries(session); n != 0 {
		t.Errorf("post tmux-session-start: pending-replay row count = %d, want 0 (respawn safety net broken)", n)
	}
}

// TestEventTmuxSessionStart_PreservesOtherSessionsPendingReplay verifies
// the purge is per-session and does not touch other sessions' rows.
func TestEventTmuxSessionStart_PreservesOtherSessionsPendingReplay(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	const targetSession = "myrepo@respawn-target"
	const otherSession = "myrepo@other-worker"
	worktree := t.TempDir()

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Seed pending-replay rows for BOTH sessions with a recent timestamp
	// so the 90-day time-based Prune sweep inside tmux-session-start does
	// not concurrently delete the untargeted session's row — the
	// per-session purge is what this test is scoped to.
	for _, session := range []string{targetSession, otherSession} {
		if _, err := d.InsertPendingReplayDelivery(db.PendingReplayRow{
			SessionName: session,
			DeliveryID:  session + "-directive",
			Text:        "reply for " + session,
			DeliverAs:   "steer",
			QueuedAt:    time.Now(),
		}); err != nil {
			t.Fatalf("seed %s: %v", session, err)
		}
	}
	if n, _ := d.CountPendingReplayDeliveries(targetSession); n != 1 {
		t.Fatalf("pre-check: target row count = %d, want 1", n)
	}
	if n, _ := d.CountPendingReplayDeliveries(otherSession); n != 1 {
		t.Fatalf("pre-check: other row count = %d, want 1", n)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })
	rootCmd.SetArgs([]string{
		"event", "tmux-session-start",
		"--session", targetSession,
		"--worktree", worktree,
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("tmux-session-start Execute: %v", err)
	}

	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	if n, _ := d2.CountPendingReplayDeliveries(targetSession); n != 0 {
		t.Errorf("target session pending-replay count = %d, want 0 (purge did not run)", n)
	}
	if n, _ := d2.CountPendingReplayDeliveries(otherSession); n != 1 {
		t.Errorf("other session pending-replay count = %d, want 1 (purge was too broad)", n)
	}
}
