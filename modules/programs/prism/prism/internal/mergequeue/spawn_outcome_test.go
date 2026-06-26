package mergequeue

// Issue #2110: merge-queue watcher persists pr_merged_at on the worker's
// spawn_outcome row inside succeedAndNotify.
//
// The watcher already has the timestamp at hand (it is the same wall-clock
// it passes to TerminateMerge to set pending_merges.merged_at), and it has
// the PR number from head.PR. The worker is located via
// spawn_outcome.pr_number, which the worker-side sidecar wrote when it
// observed the `gh pr create` invocation. Together that gives a precise,
// stale-data-free link between the merge event and the worker row whose
// pr_merged_at column the `prism stats compare` renderer reads.
//
// Tests here exercise the integration end-to-end against an isolated DB:
//
//   - happy path: worker has pr_number set → succeedAndNotify writes
//     pr_merged_at on the worker's spawn_outcome row.
//   - negative-mutation guard: no worker row matches pr_number → write is
//     a silent no-op, no orphan spawn_outcome row gets created.
//
// The notification firing is exercised by the parallel TestWatcher_CLEAN…
// suite; we focus here strictly on the new DB column behaviour.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedWorkerWithPR inserts a worker sessions row (FK target) and seeds a
// spawn_outcome row with pr_number set via the worker-side write helper.
// Mirrors what the sidecar's `gh pr create` capture would do in production.
func seedWorkerWithPR(t *testing.T, d *db.DB, instanceID, sessionName string, prNumber int) {
	t.Helper()
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/tmp/worker",
		Harness:     "pi",
		StartedAt:   time.Now().Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession(worker %s): %v", sessionName, err)
	}
	if err := d.UpdateSpawnOutcomePR(instanceID, prNumber); err != nil {
		t.Fatalf("UpdateSpawnOutcomePR: %v", err)
	}
}

// TestSucceedAndNotify_PersistsPRMergedAt verifies the headline write
// trigger: a successful merge writes pr_merged_at on the worker's
// spawn_outcome row, located via the pr_number → instance_id join.
func TestSucceedAndNotify_PersistsPRMergedAt(t *testing.T) {
	d := openTestDB(t)
	const (
		coordSession = "myrepo@main"
		coordIID     = "inst-coord-2110"
		workerSess   = "myrepo@2110-feature"
		workerIID    = "inst-worker-2110"
		pr           = 2110
	)

	// Seed the coordinator so notify() has a row to look up (it logs a
	// warning rather than panicking if the row is missing, but seeding it
	// avoids noise and mirrors production).
	seedCoordinator(t, d, coordSession, coordIID, 0, "pi-sid-2110")

	// Seed the worker session + pr_number on its spawn_outcome row.
	seedWorkerWithPR(t, d, workerIID, workerSess, pr)

	// Enqueue the merge row that succeedAndNotify will terminate.
	if _, err := d.EnqueueMerge(pr, coordSession, coordIID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	head, err := d.PendingMergeByPR(pr)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if head == nil {
		t.Fatal("PendingMergeByPR: nil after enqueue")
	}

	// Build a watcher just enough to call succeedAndNotify. We do NOT call
	// .Run() — only the success handler is under test. The repo field is
	// set via New (looks up coordinator status); a missing port is fine
	// because the failing notify path just logs.
	w := New(d, coordIID, coordSession, http.DefaultClient)
	// Capture the wall-clock right before invoking succeedAndNotify so the
	// assertion below has a tight upper bound on the persisted value.
	wantAtLeast := time.Now().UnixMilli()
	w.succeedAndNotify(context.Background(), head, mergeOutcomePrismDriven, nil)
	wantAtMost := time.Now().UnixMilli()

	// The worker's spawn_outcome row must now carry a pr_merged_at value.
	out, err := d.SpawnOutcomeByInstanceID(workerIID)
	if err != nil {
		t.Fatalf("SpawnOutcomeByInstanceID(worker): %v", err)
	}
	if out == nil {
		t.Fatal("SpawnOutcomeByInstanceID(worker): nil row — pr_merged_at write did not fire")
	}
	if out.PRNumber == nil || *out.PRNumber != pr {
		t.Fatalf("PRNumber: got %v, want %d (must survive merge-queue write)", out.PRNumber, pr)
	}
	if out.PRMergedAt == nil {
		t.Fatal("PRMergedAt: nil — write trigger did not fire on succeedAndNotify")
	}
	got := *out.PRMergedAt
	if got < wantAtLeast || got > wantAtMost {
		t.Errorf("PRMergedAt = %d, want in [%d, %d] (within the succeedAndNotify call window)", got, wantAtLeast, wantAtMost)
	}

	// Pending_merges must also have been transitioned to 'merged' with
	// merged_at populated — the watcher's existing contract must not be
	// regressed by the new DB write.
	pm, err := d.PendingMergeByPR(pr)
	if err != nil {
		t.Fatalf("PendingMergeByPR(after): %v", err)
	}
	if pm == nil || pm.Status != "merged" || pm.MergedAt == nil {
		t.Fatalf("pending_merges after succeedAndNotify: status=%v merged_at=%v, want status=merged and merged_at set", pm, pm)
	}
}

// TestSucceedAndNotify_NoWorkerRow_SkipsPRMergedAt verifies the
// negative-mutation guard: when no worker row carries the PR number (e.g.
// the worker died before `gh pr create` capture ran), the write is silently
// skipped — no orphan spawn_outcome row appears, and the notification still
// fires (verified by the absence of a panic / error on the call site).
func TestSucceedAndNotify_NoWorkerRow_SkipsPRMergedAt(t *testing.T) {
	d := openTestDB(t)
	const (
		coordSession = "myrepo@main"
		coordIID     = "inst-coord-orphan"
		pr           = 9999
	)

	seedCoordinator(t, d, coordSession, coordIID, 0, "pi-sid-orphan")

	// No worker session is seeded — InstanceIDForPRNumber will return "".
	if _, err := d.EnqueueMerge(pr, coordSession, coordIID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	head, err := d.PendingMergeByPR(pr)
	if err != nil || head == nil {
		t.Fatalf("PendingMergeByPR: err=%v head=%v", err, head)
	}

	w := New(d, coordIID, coordSession, http.DefaultClient)
	w.succeedAndNotify(context.Background(), head, mergeOutcomePrismDriven, nil)

	// pending_merges must still have transitioned to merged.
	pm, err := d.PendingMergeByPR(pr)
	if err != nil || pm == nil {
		t.Fatalf("PendingMergeByPR(after): err=%v pm=%v", err, pm)
	}
	if pm.Status != "merged" {
		t.Errorf("pending_merges.status = %q, want merged", pm.Status)
	}

	// InstanceIDForPRNumber must NOT find a row — we never seeded one. The
	// write path therefore must not have created an orphan row.
	gotIID, err := d.InstanceIDForPRNumber(pr)
	if err != nil {
		t.Fatalf("InstanceIDForPRNumber: %v", err)
	}
	if gotIID != "" {
		t.Errorf("InstanceIDForPRNumber(%d): got %q, want empty (no orphan spawn_outcome row should exist)", pr, gotIID)
	}
}
