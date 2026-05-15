package parity_test

// mergequeue_test.go — §10.3 checklist item: "Merge queue".
//
// D-10 AC (functional, merge queue):
//
//   A test enqueues a fake PR record via iris and asserts the daemon's
//   merge-queue watcher transitions the row through `watching` to a
//   terminal state. The GitHub API call to actually merge may be stubbed;
//   the test exercises the queue's state machine, not GitHub.
//
// We use the iris.EnqueueMerge + iris.WatchMergeQueue surface: enqueue a
// fake PR, run the watcher with a stub Decide that returns "merge" on the
// first tick, and assert the row reaches `merged` with merged_at and
// ended_at set. We also exercise the "fail" decision in a second sub-test
// to prove the state machine handles non-merge terminal transitions.

import (
	"context"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

func TestParityMergeQueue_WatchingToMerged(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName := iristest.SessionName("coord")
	instanceID := "iris-test-instance-merge-001"

	row, err := iris.EnqueueMerge(iso.DB, 4242, sessionName, instanceID, strPtr("Iris parity gate"))
	if err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("initial row status = %q, want %q", row.Status, "watching")
	}

	// Spin up the watcher; Decide returns "merge" the first time it sees the row.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var decideCalls int
	doneCh := make(chan struct{})
	go func() {
		_ = iris.WatchMergeQueue(ctx, iris.WatchMergeQueueConfig{
			Database:     iso.DB,
			SessionName:  sessionName,
			PollInterval: 25 * time.Millisecond,
			Decide: func(_ context.Context, head db.PendingMerge) (string, string) {
				decideCalls++
				return "merge", ""
			},
		})
		close(doneCh)
	}()

	// Poll for the row to reach 'merged'.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := iso.DB.PendingMergeByPR(4242)
		if got != nil && got.Status == "merged" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	got, err := iso.DB.PendingMergeByPR(4242)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if got == nil {
		t.Fatalf("row 4242 missing after merge")
	}
	if got.Status != "merged" {
		t.Errorf("final status = %q, want %q", got.Status, "merged")
	}
	if got.MergedAt == nil {
		t.Errorf("merged_at is nil, want set on a merged row")
	}
	if got.EndedAt == nil {
		t.Errorf("ended_at is nil, want set on a terminal row")
	}
	if decideCalls == 0 {
		t.Errorf("Decide was never called")
	}

	cancel()
	<-doneCh
}

func TestParityMergeQueue_WatchingToFailed(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName := iristest.SessionName("coord-fail")
	if _, err := iris.EnqueueMerge(iso.DB, 4243, sessionName, "iris-fail-001", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	doneCh := make(chan struct{})
	go func() {
		_ = iris.WatchMergeQueue(ctx, iris.WatchMergeQueueConfig{
			Database:     iso.DB,
			SessionName:  sessionName,
			PollInterval: 25 * time.Millisecond,
			Decide: func(_ context.Context, head db.PendingMerge) (string, string) {
				return "fail", "stubbed CI failure"
			},
		})
		close(doneCh)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := iso.DB.PendingMergeByPR(4243)
		if got != nil && got.Status == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	got, err := iso.DB.PendingMergeByPR(4243)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if got == nil || got.Status != "failed" {
		t.Errorf("status = %v, want failed", got)
	}
	if got != nil && (got.Error == nil || *got.Error != "stubbed CI failure") {
		t.Errorf("error field = %v, want %q", got.Error, "stubbed CI failure")
	}
	if got != nil && got.MergedAt != nil {
		t.Errorf("merged_at = %v, want nil on a failed row", got.MergedAt)
	}
	if got != nil && got.EndedAt == nil {
		t.Errorf("ended_at = nil, want set on a terminal row")
	}

	cancel()
	<-doneCh
}

func TestParityMergeQueue_EnqueueIdempotent(t *testing.T) {
	iso := iristest.NewIsolated(t)
	sessionName := iristest.SessionName("coord-idemp")
	r1, err := iris.EnqueueMerge(iso.DB, 4244, sessionName, "iris-idemp-001", nil)
	if err != nil {
		t.Fatalf("EnqueueMerge first: %v", err)
	}
	r2, err := iris.EnqueueMerge(iso.DB, 4244, sessionName, "iris-idemp-001", nil)
	if err != nil {
		t.Fatalf("EnqueueMerge second: %v", err)
	}
	if r1.PR != r2.PR {
		t.Errorf("idempotent re-enqueue: PR changed (%d → %d)", r1.PR, r2.PR)
	}
	if r2.Status != "watching" {
		t.Errorf("re-enqueue status = %q, want watching", r2.Status)
	}
}

func strPtr(s string) *string { return &s }
