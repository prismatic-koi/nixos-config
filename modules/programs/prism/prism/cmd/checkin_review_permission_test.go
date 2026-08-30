package cmd

// checkin_review_permission_test.go — direct-route gate on the review aggregate.
//
// `prism checkin <parent>~review` WITHOUT --verbose reads d.QueryEvents
// inline for every review-group member, so it does not pass through the
// direct-route permission gate on runCheckinSession. These tests pin
// authorizeDirectCheckinReviewAggregate (the direct-route half) against the
// same tier table as the individual-session gate, using the same fixture
// helpers in checkin_permission_test.go.

import (
	"strings"
	"testing"
)

// TestDirectCheckinReviewAggregate_Worker_AllowsOwnParent is the aggregate
// counterpart of TestDirectCheckin_Worker_AllowsOwnReviewAgent: a worker
// reading the summary of its OWN review group must be admitted, even though
// authorizeDirectCheckin (the per-session gate) refuses a self-target.
func TestDirectCheckinReviewAggregate_Worker_AllowsOwnParent(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliWorker, cliRepo, "worker")
	f.seedReviewGroup(cliWorker, cliRepo, 1, cliWorkerReview)

	if err := authorizeDirectCheckinReviewAggregateFor(cliWorker, cliWorker); err != nil {
		t.Fatalf("worker reading the summary of its own review group was refused: %v", err)
	}
}

// TestDirectCheckinReviewAggregate_Worker_RefusesOtherWorkersGroup: a worker
// may not read another worker's review-agent summary.
func TestDirectCheckinReviewAggregate_Worker_RefusesOtherWorkersGroup(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliWorker, cliRepo, "worker")
	f.seedSession(cliOtherWorker, cliRepo, "worker")
	f.seedReviewGroup(cliOtherWorker, cliRepo, 1, "prism-test-checkin-cli@other-feature~review-1-review-goal")

	err := authorizeDirectCheckinReviewAggregateFor(cliWorker, cliOtherWorker)
	if err == nil {
		t.Fatal("worker reading another worker's review-agent summary was permitted — want a refusal")
	}
}

// TestDirectCheckinReviewAggregate_Coordinator_AllowsOwnRepo mirrors tier 2:
// a coordinator may read the aggregate summary of any own-repo session.
func TestDirectCheckinReviewAggregate_Coordinator_AllowsOwnRepo(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliCoordinator, cliRepo, "coordinator")
	f.seedSession(cliWorker, cliRepo, "worker")
	f.seedReviewGroup(cliWorker, cliRepo, 1, cliWorkerReview)

	if err := authorizeDirectCheckinReviewAggregateFor(cliCoordinator, cliWorker); err != nil {
		t.Fatalf("coordinator reading own-repo worker's review-agent summary was refused: %v", err)
	}
}

// TestDirectCheckinReviewAggregate_Coordinator_RefusesCrossRepoWorker mirrors
// tier 2's cross-repo refusal: a coordinator may not read another repo's
// worker's aggregate summary.
func TestDirectCheckinReviewAggregate_Coordinator_RefusesCrossRepoWorker(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliCoordinator, cliRepo, "coordinator")
	f.seedSession(cliAltWorker, cliAltRepo, "worker")
	f.seedReviewGroup(cliAltWorker, cliAltRepo, 1, "prism-test-checkin-cli-other@feature~review-1-review-goal")

	err := authorizeDirectCheckinReviewAggregateFor(cliCoordinator, cliAltWorker)
	if err == nil {
		t.Fatal("coordinator reading a cross-repo worker's review-agent summary was permitted — want a refusal")
	}
}

// TestDirectCheckinReviewAggregate_UnresolvableCaller_FailsClosed mirrors
// TestDirectCheckin_UnresolvableCaller_FailsClosed for the aggregate entry
// point.
func TestDirectCheckinReviewAggregate_UnresolvableCaller_FailsClosed(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.seedSession(cliWorker, cliRepo, "worker")

	err := authorizeDirectCheckinReviewAggregateFor("", cliWorker)
	if err == nil {
		t.Fatal("aggregate checkin with an unresolvable caller was permitted — want a refusal")
	}
	if !strings.Contains(err.Error(), "PRISM_SESSION_NAME") {
		t.Errorf("error does not name PRISM_SESSION_NAME, so the caller is not told the remedy: %v", err)
	}
}

// TestDirectCheckinReviewAggregate_TierThree_AuditsAccess pins that a
// privileged-coordinator read of another repo's aggregate summary writes the
// shared audit event, exactly as the per-session gate does.
func TestDirectCheckinReviewAggregate_TierThree_AuditsAccess(t *testing.T) {
	f := newDirectCheckinFixture(t)
	f.privilege(cliRepo)
	f.seedSession(cliCoordinator, cliRepo, "coordinator")
	f.seedSession(cliAltWorker, cliAltRepo, "worker")
	f.seedReviewGroup(cliAltWorker, cliAltRepo, 1, "prism-test-checkin-cli-other@feature~review-1-review-goal")

	if err := authorizeDirectCheckinReviewAggregateFor(cliCoordinator, cliAltWorker); err != nil {
		t.Fatalf("privileged coordinator reading a cross-repo review-agent summary was refused: %v", err)
	}

	events := f.auditEvents(cliCoordinator)
	if len(events) != 1 {
		t.Fatalf("expected exactly one audit event, got %d", len(events))
	}
	if events[0].Target != cliAltWorker {
		t.Errorf("audit event target = %q, want %q", events[0].Target, cliAltWorker)
	}
}
