package iris

// mergequeue.go — iris-side merge queue surface (D-10 parity).
//
// Prism's `prism merge <pr>` enqueues a PR row into pending_merges and
// (separately) the coordinator's sidecar runs a mergequeue.Watcher that
// drives the row through 'watching' → terminal state. The schema is shared
// (§10.4 of daemon-mode-design.md), so iris reuses the same pending_merges
// table.
//
// This file exposes a thin iris-flavoured surface:
//
//   EnqueueMerge(db, pr, parent) — wraps db.EnqueueMerge so callers don't
//                                  have to import internal/db directly.
//   WatchMergeQueue              — a minimal poll loop that exercises the
//                                  same state-machine contract the prism
//                                  watcher does, but with an injectable
//                                  decision function so tests can drive
//                                  transitions without a real `gh` binary.
//
// The decision function (MergeDecisionFunc) is the integration seam used
// by the parity test: the test passes a function that returns "merge" on
// the first call, the watcher writes the terminal row via
// db.TerminateMerge, and the test asserts the row reached `merged`. The
// production daemon will eventually wire this to the existing
// internal/mergequeue.Watcher (which already has runGHFunc injection for
// the same reason).

import (
	"context"
	"fmt"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// MergeDecisionFunc inspects the head of the merge queue and returns a
// directive for the watcher. The decision is one of:
//
//   "merge"   → write status="merged", merged_at=now, ended_at=now.
//   "fail"    → write status="failed" with the supplied error string.
//   "cancel"  → write status="cancelled" with the supplied error string.
//   "wait"    → no state change; the watcher ticks again on the next poll.
//
// The errMsg return is recorded on the pending_merges.error column when
// decision is "fail" or "cancel"; ignored otherwise.
//
// In production, MergeDecisionFunc would consult `gh pr view`'s
// mergeStateStatus and required-check status. In the parity test, it
// returns a fixed sequence to drive a deterministic transition.
type MergeDecisionFunc func(ctx context.Context, head db.PendingMerge) (decision, errMsg string)

// EnqueueMerge enqueues a PR for the merge queue, returning the resulting
// pending_merges row. It is a thin wrapper around db.EnqueueMerge that
// callers in cmd/iris can use without importing internal/db.
func EnqueueMerge(database *db.DB, pr int, sessionName, instanceID string, title *string) (*db.PendingMerge, error) {
	if database == nil {
		return nil, fmt.Errorf("iris merge: Database is required")
	}
	if pr <= 0 {
		return nil, fmt.Errorf("iris merge: PR number must be positive, got %d", pr)
	}
	if sessionName == "" {
		return nil, fmt.Errorf("iris merge: sessionName is required")
	}
	return database.EnqueueMerge(pr, sessionName, instanceID, title)
}

// WatchMergeQueueConfig holds the parameters for a single watcher loop.
type WatchMergeQueueConfig struct {
	// Database is the open iris DB.
	Database *db.DB
	// SessionName scopes the watcher to one coordinator session's queue.
	SessionName string
	// PollInterval is how often the watcher reads MergeQueueHead. 0 means
	// 100ms (tests) — production callers should set this to ~45s.
	PollInterval time.Duration
	// Decide is called every tick with the queue head (if any) and returns
	// the action to take. Must not be nil.
	Decide MergeDecisionFunc
}

// WatchMergeQueue runs the watcher loop until ctx is cancelled. On every
// tick it reads the head of the queue for SessionName and, when a row is
// present, calls Decide and dispatches the directive to db.TerminateMerge
// (or no-op for "wait").
//
// WatchMergeQueue is the iris-side analogue of internal/mergequeue.Watcher
// minus the GitHub-specific logic. The parity test exercises this loop
// directly to verify the queue's state-machine contract.
func WatchMergeQueue(ctx context.Context, cfg WatchMergeQueueConfig) error {
	if cfg.Database == nil {
		return fmt.Errorf("iris merge: Database is required")
	}
	if cfg.SessionName == "" {
		return fmt.Errorf("iris merge: SessionName is required")
	}
	if cfg.Decide == nil {
		return fmt.Errorf("iris merge: Decide is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}

	tick := time.NewTicker(cfg.PollInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			head, err := cfg.Database.MergeQueueHead(cfg.SessionName)
			if err != nil {
				return fmt.Errorf("iris merge: queue head: %w", err)
			}
			if head == nil {
				continue
			}
			// Record the last-checked timestamp so observers see liveness.
			if err := cfg.Database.UpdateMergeLastChecked(head.PR); err != nil {
				// Non-fatal — continue.
				_ = err
			}

			decision, errMsg := cfg.Decide(ctx, *head)
			switch decision {
			case "merge":
				if err := cfg.Database.TerminateMerge(head.PR, "merged", ""); err != nil {
					return fmt.Errorf("iris merge: terminate merged: %w", err)
				}
			case "fail":
				if err := cfg.Database.TerminateMerge(head.PR, "failed", errMsg); err != nil {
					return fmt.Errorf("iris merge: terminate failed: %w", err)
				}
			case "cancel":
				if err := cfg.Database.TerminateMerge(head.PR, "cancelled", errMsg); err != nil {
					return fmt.Errorf("iris merge: terminate cancelled: %w", err)
				}
			case "wait":
				// no-op — tick again next poll.
			default:
				return fmt.Errorf("iris merge: unknown decision %q", decision)
			}
		}
	}
}
