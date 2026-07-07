package db

// pending_replay_lifecycle_test.go — regression tests for the lifecycle
// hooks added in response to the round-2 review-context finding on
// PR #2365.
//
// The durable pending-replay buffer landed with no lifecycle management:
// respawning on the same branch name (a supported, tested flow via #2094)
// would let restorePendingReplayFromDB drain a previous incarnation's
// coordinator directives into the fresh agent, and abandoned rows would
// accumulate indefinitely.
//
// Two hooks now cover it:
//
//   - DeletePendingReplayDeliveriesForSession — called from
//     severPiResumeLinkage (prism cleanup) and tmux-session-start.
//   - PrunePendingReplayDeliveriesOlderThan — swept by the periodic Prune
//     job alongside the other time-based tables.

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDeletePendingReplayDeliveriesForSession_ScopesToTargetSession
// verifies the per-session delete removes every entry for the target
// session and leaves other sessions untouched.
func TestDeletePendingReplayDeliveriesForSession_ScopesToTargetSession(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// Seed two sessions with two entries each.
	for _, session := range []string{
		"prism-test@invoker-lifecycle-a",
		"prism-test@invoker-lifecycle-b",
	} {
		for i, body := range []string{"first", "second"} {
			if _, insertErr := d.InsertPendingReplayDelivery(PendingReplayRow{
				SessionName: session,
				DeliveryID:  session + "-" + body,
				Text:        body,
				DeliverAs:   "steer",
				QueuedAt:    time.UnixMilli(int64(1000 + i*1000)),
			}); insertErr != nil {
				t.Fatalf("seed %s/%s: %v", session, body, insertErr)
			}
		}
	}

	// Delete only session A's entries.
	if err := d.DeletePendingReplayDeliveriesForSession("prism-test@invoker-lifecycle-a"); err != nil {
		t.Fatalf("DeletePendingReplayDeliveriesForSession: %v", err)
	}

	if n, _ := d.CountPendingReplayDeliveries("prism-test@invoker-lifecycle-a"); n != 0 {
		t.Errorf("session-A count after delete = %d, want 0", n)
	}
	if n, _ := d.CountPendingReplayDeliveries("prism-test@invoker-lifecycle-b"); n != 2 {
		t.Errorf("session-B count after session-A delete = %d, want 2 (must not be affected)", n)
	}
}

// TestDeletePendingReplayDeliveriesForSession_Idempotent verifies calling
// the per-session delete on a session with no entries is a no-op that
// returns nil.
func TestDeletePendingReplayDeliveriesForSession_Idempotent(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// No rows exist for the target session; delete must not error.
	if err := d.DeletePendingReplayDeliveriesForSession("prism-test@invoker-empty"); err != nil {
		t.Errorf("DeletePendingReplayDeliveriesForSession on empty session: %v", err)
	}
}

// TestPrunePendingReplayDeliveriesOlderThan_TimeBoundary verifies the
// time-based prune deletes only entries older than the cutoff.
func TestPrunePendingReplayDeliveriesOlderThan_TimeBoundary(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const session = "prism-test@invoker-prune-time"
	// Seed three rows across the boundary.
	if _, err := d.InsertPendingReplayDelivery(PendingReplayRow{
		SessionName: session, DeliveryID: "old-1", Text: "old", DeliverAs: "steer",
		QueuedAt: time.UnixMilli(1000),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertPendingReplayDelivery(PendingReplayRow{
		SessionName: session, DeliveryID: "old-2", Text: "old", DeliverAs: "steer",
		QueuedAt: time.UnixMilli(2000),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertPendingReplayDelivery(PendingReplayRow{
		SessionName: session, DeliveryID: "new-1", Text: "new", DeliverAs: "steer",
		QueuedAt: time.UnixMilli(5000),
	}); err != nil {
		t.Fatal(err)
	}
	// Prune everything older than 3000 — the two "old-*" rows must go, "new-1" must survive.
	n, err := d.PrunePendingReplayDeliveriesOlderThan(3000)
	if err != nil {
		t.Fatalf("PrunePendingReplayDeliveriesOlderThan: %v", err)
	}
	if n != 2 {
		t.Errorf("prune returned %d, want 2", n)
	}
	remaining, err := d.LoadPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("Load after prune: %v", err)
	}
	if len(remaining) != 1 || remaining[0].DeliveryID != "new-1" {
		t.Errorf("post-prune rows: %+v, want just new-1", remaining)
	}
}

// TestPrune_SweepsPendingReplayDeliveries confirms the periodic Prune()
// entry point sweeps pending_replay_deliveries with the same threshold
// used for agent_events. This is the "abandoned session never cleaned up"
// safety net.
func TestPrune_SweepsPendingReplayDeliveries(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const session = "prism-test@invoker-prune-sweep"
	// A very old row (queued 30 days ago) — well outside any sensible
	// retention window.
	oldQueuedAt := time.Now().Add(-30 * 24 * time.Hour)
	if _, err := d.InsertPendingReplayDelivery(PendingReplayRow{
		SessionName: session, DeliveryID: "very-old", Text: "abandoned",
		DeliverAs: "steer", QueuedAt: oldQueuedAt,
	}); err != nil {
		t.Fatal(err)
	}
	// A fresh row (queued now) that must survive.
	if _, err := d.InsertPendingReplayDelivery(PendingReplayRow{
		SessionName: session, DeliveryID: "fresh", Text: "alive",
		DeliverAs: "steer", QueuedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Prune with a 7-day cutoff — the very-old row is 30 days old and
	// must be swept; the fresh row must remain.
	if err := d.Prune(7 * 24 * time.Hour); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	after, err := d.LoadPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("Load after Prune: %v", err)
	}
	if len(after) != 1 || after[0].DeliveryID != "fresh" {
		t.Errorf("post-Prune rows: %+v, want just [fresh]", after)
	}
}
