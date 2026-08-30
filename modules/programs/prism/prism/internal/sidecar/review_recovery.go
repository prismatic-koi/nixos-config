package sidecar

// review_recovery.go — worker-sidecar watcher that rescues review groups
// orphaned by a dead monitor subprocess.
//
// Background. `prism review` spawns a detached `prism monitor-review`
// subprocess that owns the all-terminal → deliver transition. There is no
// other watcher anywhere in the system. If the monitor dies (OOM, kernel
// kill, PRISM upgrade mid-review, host reboot, silent StartMonitorProcess
// failure), the agent rows reach `finished` in the DB but no process
// delivers the review-complete prompt to the worker — the worker stays in
// `reviewing` indefinitely and the only escape is manual `prism cleanup`.
//
// This watcher lives in the worker session's own sidecar (the parent of
// any review group it might spawn), polls db.GroupCompleted on a coarse
// interval, and after a grace window dispatches the delivery via
// review.DeliverGroupResults. The delivery uses a deterministic
// delivery_id derived from the group_id so that if the original monitor
// is somehow still alive and delivers at the same moment, the receiving
// host-API /prompt dedup drops the second hit.
//
// The watcher is deliberately additive: in the happy path (monitor alive,
// delivers normally) the watcher's GroupCompleted check transitions from
// false to true exactly once, the grace window prevents firing, the
// monitor delivers and clears reviewingInFlight, and the watcher returns
// to no-op steady state. It cannot generate spurious prompts because:
//
//   (a) it only fires when reviewingInFlight == true (set by the /review
//       handler when this very sidecar accepted a review request), AND
//   (b) it only fires after grace window past first-observed-complete, AND
//   (c) it only fires when GroupCompleted is still true, AND
//   (d) the delivery_id dedup catches any double-fire across watcher+monitor.

import (
	"context"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// reviewRecoveryQuerier is the narrow interface used by reviewRecoveryTick
// for its two DB calls. The concrete implementation is *db.DB; tests may
// substitute a fake that simulates SQLITE_BUSY sequences.
type reviewRecoveryQuerier interface {
	LatestGroupForParent(parentSession string) (*db.GroupInfo, error)
	GroupCompleted(groupID string) (bool, error)
}

// startReviewRecoveryWatcher launches the recovery goroutine for this
// sidecar. It is a no-op (returns nil cancel) when the watcher is disabled
// for this session — review-agent roles and any session whose
// ReviewRecoveryInterval is negative.
//
// The watcher is a closer fit to the merge-queue watcher in
// internal/mergequeue/ than to the per-session inactivity watchdog: it
// observes group-level DB state on a ticker, not per-frame sidecar events.
// It uses the wall-clock time package directly (not s.cfg.Clock) for the
// first-seen-complete bookkeeping because the only thing the fake clock
// would help with is the polling cadence, and we already drive that via
// the ticker constructor below.
func (s *Sidecar) startReviewRecoveryWatcher(parentCtx context.Context) context.CancelFunc {
	if s.cfg.ReviewRecoveryInterval < 0 {
		s.logger().Printf("sidecar: review-recovery watcher disabled by ReviewRecoveryInterval=%v", s.cfg.ReviewRecoveryInterval)
		return func() {}
	}
	// Skip review-agent sessions — they cannot own a review group, so the
	// watcher would never have anything to do.
	if reviewAgentAllowlist[s.cfg.AgentRole] {
		return func() {}
	}
	// Skip when the DB is not configured — defensive; production sidecars
	// always have a DB but some unit tests construct minimal Sidecars.
	if s.cfg.DB == nil {
		return func() {}
	}

	interval := s.cfg.ReviewRecoveryInterval
	if interval == 0 {
		interval = defaultReviewRecoveryInterval
	}
	grace := s.cfg.ReviewRecoveryGrace
	if grace == 0 {
		grace = defaultReviewRecoveryGrace
	}

	ctx, cancel := context.WithCancel(parentCtx)
	s.mu.Lock()
	s.reviewRecoveryFirstSeenComplete = make(map[string]time.Time)
	s.mu.Unlock()

	go s.runReviewRecoveryWatcher(ctx, interval, grace)
	s.logger().Printf("sidecar: review-recovery watcher started (interval=%s, grace=%s)", interval, grace)
	return cancel
}

// runReviewRecoveryWatcher is the watcher loop body. Runs until ctx is
// cancelled.
func (s *Sidecar) runReviewRecoveryWatcher(ctx context.Context, interval, grace time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reviewRecoveryTick(grace, time.Now())
		}
	}
}

// reviewRecoveryTick runs one pass of the recovery loop. Extracted so tests
// can drive it directly without a real ticker.
//
// The tick is a sequence of cheap pre-checks (in-memory flag, DB lookup,
// timestamp comparison) and at most one delivery attempt. Each tick is
// idempotent: calling it twice in succession with the same DB state will
// either both observe "not yet past grace" or one will observe past-grace
// and dispatch, the other will observe the dedup result.
func (s *Sidecar) reviewRecoveryTick(grace time.Duration, now time.Time) {
	// Fast path: no review in flight from this session's perspective.
	s.mu.Lock()
	inFlight := s.reviewingInFlight
	qdb := s.reviewRecoveryQuerierOverride
	s.mu.Unlock()
	if !inFlight {
		s.clearReviewRecoveryFirstSeen()
		return
	}
	// Use the test override if present; fall back to the real DB.
	if qdb == nil {
		qdb = s.cfg.DB
	}

	// LatestGroupForParent is the right query: while
	// reviewingInFlight is true, the worker is awaiting a single in-flight
	// review round, and that round corresponds to the most recently created
	// session_groups row for this parent. ActiveReviewGroupForParent uses a
	// "has any non-terminal member" criterion which is exactly the case the
	// recovery watcher is designed to handle the OPPOSITE of: the stuck-but-
	// complete group has ZERO non-terminal members by definition.
	//
	// Retry on SQLITE_BUSY: the socket-pipe reader's turn_start upsert may
	// hold the write lock at the same instant the tick fires. Three attempts
	// × 10 ms matches the backoff used by the /review pre-emptive write
	// (host_api.go).
	const (
		recoveryDBAttempts = 3
		recoveryDBBackoff  = 10 * time.Millisecond
	)
	var groupInfo *db.GroupInfo
	if err := db.WithBusyRetry(recoveryDBAttempts, recoveryDBBackoff, func() error {
		var err error
		groupInfo, err = qdb.LatestGroupForParent(s.cfg.SessionName)
		return err
	}); err != nil {
		s.logger().Printf("sidecar: review-recovery: LatestGroupForParent: %v", err)
		return
	}
	if groupInfo == nil {
		// reviewingInFlight is true but no group exists: the monitor has
		// not yet registered the group, or the row was cleaned up. Drop
		// our first-seen bookkeeping so a fresh group later does not
		// inherit an old timestamp.
		s.clearReviewRecoveryFirstSeen()
		return
	}
	groupID := groupInfo.GroupID

	var done bool
	if err := db.WithBusyRetry(recoveryDBAttempts, recoveryDBBackoff, func() error {
		var err error
		done, err = qdb.GroupCompleted(groupID)
		return err
	}); err != nil {
		s.logger().Printf("sidecar: review-recovery: GroupCompleted(%s): %v", groupID, err)
		return
	}
	if !done {
		// Group still running — drop the first-seen timestamp for this
		// group_id if we had one, so a regression to non-terminal resets
		// the clock.
		s.dropReviewRecoveryFirstSeen(groupID)
		return
	}

	// Group is complete. Record first-observed time and check grace.
	s.mu.Lock()
	firstSeen, hadEntry := s.reviewRecoveryFirstSeenComplete[groupID]
	if !hadEntry {
		s.reviewRecoveryFirstSeenComplete[groupID] = now
		firstSeen = now
	}
	s.mu.Unlock()
	if !hadEntry {
		s.logger().Printf("sidecar: review-recovery: group %s observed complete; recovery dispatch in %s if monitor does not deliver", groupID, grace)
		return
	}
	if now.Sub(firstSeen) < grace {
		return
	}

	// Past grace and still complete. Dispatch the recovery delivery.
	s.dispatchReviewRecovery(groupID)
}

// dispatchReviewRecovery invokes review.DeliverGroupResults for the named
// group_id and logs the outcome. The result is interpreted as follows:
//
//   - Delivered (or buffered-for-replay) → log success, clear in-memory
//     bookkeeping; reviewingInFlight will be cleared by the receiving
//     sidecar's /prompt handler when the delivery is processed.
//   - Error returning a fallback path → log warning; leave bookkeeping in
//     place so the next tick re-attempts.
//   - Error with no fallback → log warning; next tick will try again.
//
// All paths are non-fatal — a sidecar that crashes here would also kill
// the host-API listener the in-sandbox CLI depends on.
func (s *Sidecar) dispatchReviewRecovery(groupID string) {
	deliveryID := review.RecoveryDeliveryID(groupID)
	s.logger().Printf("sidecar: review-recovery: monitor appears AWOL for group %s, delivering on its behalf (delivery_id=%s)", groupID, deliveryID)
	res, err := review.DeliverGroupResults(s.cfg.DB, groupID, deliveryID)
	if err != nil {
		// res may be non-nil with FallbackTo set.
		if res != nil && res.FallbackTo != "" {
			s.logger().Printf("sidecar: review-recovery: delivery failed for group %s: %v (fallback written to %s)", groupID, err, res.FallbackTo)
		} else {
			s.logger().Printf("sidecar: review-recovery: delivery failed for group %s: %v", groupID, err)
		}
		return
	}
	if res != nil && res.Delivered {
		s.logger().Printf("sidecar: review-recovery: delivered review-complete prompt for group %s (allPassed=%v)", groupID, res.AllPassed)
		s.dropReviewRecoveryFirstSeen(groupID)
		return
	}
	// No error and not delivered (unusual — DeliverGroupResults only returns
	// this when the parent session has ended) — log and clear bookkeeping.
	s.logger().Printf("sidecar: review-recovery: DeliverGroupResults returned no-error/no-delivery for group %s", groupID)
	s.dropReviewRecoveryFirstSeen(groupID)
}

// clearReviewRecoveryFirstSeen drops all first-seen-complete entries. Called
// when reviewingInFlight transitions to false or when no active group exists.
func (s *Sidecar) clearReviewRecoveryFirstSeen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reviewRecoveryFirstSeenComplete) == 0 {
		return
	}
	s.reviewRecoveryFirstSeenComplete = make(map[string]time.Time)
}

// dropReviewRecoveryFirstSeen drops the first-seen-complete entry for one
// group_id. Called when the group is no longer complete (regressed to
// non-terminal — extremely unlikely in practice but defensively handled) or
// after a successful recovery dispatch.
func (s *Sidecar) dropReviewRecoveryFirstSeen(groupID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reviewRecoveryFirstSeenComplete, groupID)
}

// ReviewRecoveryFirstSeenForTest exposes the first-seen-complete bookkeeping
// for integration tests. The returned map is a copy; modifications do not
// affect sidecar state.
func (s *Sidecar) ReviewRecoveryFirstSeenForTest() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reviewRecoveryFirstSeenComplete == nil {
		return nil
	}
	out := make(map[string]time.Time, len(s.reviewRecoveryFirstSeenComplete))
	for k, v := range s.reviewRecoveryFirstSeenComplete {
		out[k] = v
	}
	return out
}

// ReviewRecoveryTickForTest invokes one tick of the recovery loop with the
// given wall-clock time. Exported so integration tests can drive the loop
// deterministically without spinning a goroutine.
func (s *Sidecar) ReviewRecoveryTickForTest(grace time.Duration, now time.Time) {
	s.reviewRecoveryTick(grace, now)
}

// SetRecoveryQuerierForTest replaces the DB querier used by
// reviewRecoveryTick with the given fake. Call before the first tick.
// Only for use in tests.
func (s *Sidecar) SetRecoveryQuerierForTest(q reviewRecoveryQuerier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reviewRecoveryQuerierOverride = q
}
