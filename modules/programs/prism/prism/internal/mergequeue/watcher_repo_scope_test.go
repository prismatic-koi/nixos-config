package mergequeue

// Watcher-level regression tests for the repo scoping.
//
// The DB-layer tests in internal/db/mergequeue_repo_scope_test.go cover
// the direct SQL surface. These tests pin down the watcher-facing
// invariants: PendingMerge rows returned by MergeQueueHead carry a Repo
// field that the watcher then threads into TerminateMerge and
// UpdateMergeLastChecked, so the watcher can never touch a same-numbered
// row belonging to another repo.

import (
	"context"
	"net/http"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestMergeQueueHead_CarriesRepoOnRow verifies that MergeQueueHead
// populates the Repo field on the returned row. The watcher relies on
// this to scope its terminal writes; if the field were empty, the
// watcher's TerminateMerge call would try to touch a row keyed on
// (repo="", pr=N), which never matches production rows.
func TestMergeQueueHead_CarriesRepoOnRow(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.EnqueueMerge(500, "aws-databases", "aws-databases@main", "inst-x", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	head, err := d.MergeQueueHead("aws-databases@main")
	if err != nil {
		t.Fatalf("MergeQueueHead: %v", err)
	}
	if head == nil {
		t.Fatal("MergeQueueHead: got nil, want the enqueued row")
	}
	if head.Repo != "aws-databases" {
		t.Errorf("head.Repo = %q, want aws-databases (the watcher needs this to scope its terminal writes)", head.Repo)
	}
}

// TestWatcher_TerminateOnlyAffectsOwnRepo is the end-to-end regression:
// two coordinators (one per repo) both enqueue their own PR N. When
// watcher A merges its row through the production tick, watcher B's row
// must remain watching.
func TestWatcher_TerminateOnlyAffectsOwnRepo(t *testing.T) {
	d := openTestDB(t)

	const pr = 47
	// Repo A enqueues.
	if _, err := d.EnqueueMerge(pr, "repo-a", "repo-a@main", "inst-a", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	// Repo B enqueues the same PR number.
	if _, err := d.EnqueueMerge(pr, "repo-b", "repo-b@main", "inst-b", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-b: %v", err)
	}

	gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
	newTickWatcher(d, "inst-a", "repo-a@main", http.DefaultClient, gh).tick(context.Background())

	rowA, err := d.PendingMergeByPR(pr, "repo-a")
	if err != nil || rowA == nil || rowA.Status != "merged" {
		t.Fatalf("repo-a row = %+v (err %v), want merged", rowA, err)
	}

	// Repo B's row must remain untouched.
	rowB, err := d.PendingMergeByPR(pr, "repo-b")
	if err != nil {
		t.Fatalf("PendingMergeByPR repo-b: %v", err)
	}
	if rowB == nil {
		t.Fatal("repo-b row disappeared \u2014 watcher A's TerminateMerge crossed repos")
	}
	if rowB.Status != "watching" {
		t.Errorf("repo-b row: status = %q, want watching (watcher A's terminal write crossed repos \u2014 the incident of 2026-07-06 has regressed)", rowB.Status)
	}
}

// TestWatcher_UpdateLastCheckedOnlyAffectsOwnRepo covers the same
// invariant for the per-tick heartbeat: a tick of watcher A must not bump
// the timestamp of a same-numbered row on another repo.
func TestWatcher_UpdateLastCheckedOnlyAffectsOwnRepo(t *testing.T) {
	d := openTestDB(t)

	const pr = 55
	if _, err := d.EnqueueMerge(pr, "repo-a", "repo-a@main", "inst-a", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	if _, err := d.EnqueueMerge(pr, "repo-b", "repo-b@main", "inst-b", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-b: %v", err)
	}

	gh := newGHFake(t, blockedInfo("APPROVED", []checkEntry{{Name: "pr-gate", Status: "IN_PROGRESS"}}))
	newTickWatcher(d, "inst-a", "repo-a@main", http.DefaultClient, gh).tick(context.Background())

	rowA, _ := d.PendingMergeByPR(pr, "repo-a")
	rowB, _ := d.PendingMergeByPR(pr, "repo-b")
	if rowA == nil || rowB == nil {
		t.Fatal("both rows must be present")
	}
	if rowA.LastCheckedAt == nil {
		t.Error("repo-a: LastCheckedAt is nil after heartbeat")
	}
	if rowB.LastCheckedAt != nil {
		t.Errorf("repo-b: LastCheckedAt = %v, want nil (heartbeat on repo-a must not touch repo-b)", *rowB.LastCheckedAt)
	}
}

// Guard against an accidental change to db.PendingMerge that drops the
// Repo field. This is a compile-time reminder rather than a runtime
// assertion: if someone removes the field, the file will fail to build
// here first.
var _ = db.PendingMerge{Repo: ""}
