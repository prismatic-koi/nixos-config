package sidecar

// checkin_review_summary_test.go
//
// GET /checkin/review-summary, the host-API half of the aggregate gate for
// the non-verbose `prism checkin <parent>~review` form. Without this route,
// the CLI renders that form entirely from a local direct-DB read with no
// host-API route at all, so a sandboxed caller cannot reach it, and a
// host-mode caller reaches it with no gate. These tests pin the endpoint
// from the outside — through the HTTP handler — mirroring
// checkin_permission_test.go's shape for GET /checkin.

import (
	"net/http"
	"testing"
)

// reviewSummary issues GET /checkin/review-summary?parent=<parent> against sc.
func reviewSummary(t *testing.T, sc *Sidecar, parent string) (int, string) {
	t.Helper()
	rr := doHostAPI(t, sc, http.MethodGet, "/checkin/review-summary?parent="+parent, "")
	return rr.Code, rr.Body.String()
}

// TestCheckinReviewSummary_Worker_AllowsOwnParent is the aggregate
// counterpart of TestCheckin_Worker_AllowsOwnReviewAgent: a worker reading
// the summary of its OWN review group is admitted, even though passing the
// parent straight into the per-session /checkin gate would hit the tier-1
// self-checkin denial.
func TestCheckinReviewSummary_Worker_AllowsOwnParent(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedReviewGroup(ckWorker, ckRepo, 1, ckWorkerOldRev)

	sc := f.sidecarFor(ckWorker, ckRepo, "worker", nil)
	code, body := reviewSummary(t, sc, ckWorker)
	if code != http.StatusOK {
		t.Fatalf("worker reading its own review-group summary got %d, want 200: %s", code, body)
	}
}

// TestCheckinReviewSummary_Worker_RefusesOtherWorkersGroup is the headline
// The defect: a worker must not be able to read another worker's
// review-agent summary through the aggregate form.
func TestCheckinReviewSummary_Worker_RefusesOtherWorkersGroup(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedSession(ckOtherWorker, ckRepo, "worker")
	f.seedReviewGroup(ckOtherWorker, ckRepo, 1, ckOtherWorkerReview)

	sc := f.sidecarFor(ckWorker, ckRepo, "worker", nil)
	code, body := reviewSummary(t, sc, ckOtherWorker)
	if code != http.StatusForbidden {
		t.Fatalf("worker reading another worker's review-group summary got %d, want 403: %s", code, body)
	}
}

// TestCheckinReviewSummary_Coordinator_AllowsOwnRepo mirrors tier 2.
func TestCheckinReviewSummary_Coordinator_AllowsOwnRepo(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckCoordinator, ckRepo, "coordinator")
	f.seedSession(ckWorker, ckRepo, "worker")
	f.seedReviewGroup(ckWorker, ckRepo, 1, ckWorkerOldRev)

	sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator", nil)
	code, body := reviewSummary(t, sc, ckWorker)
	if code != http.StatusOK {
		t.Fatalf("coordinator reading own-repo review-group summary got %d, want 200: %s", code, body)
	}
}

// TestCheckinReviewSummary_Coordinator_RefusesCrossRepo mirrors tier 2's
// cross-repo refusal.
func TestCheckinReviewSummary_Coordinator_RefusesCrossRepo(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckCoordinator, ckRepo, "coordinator")
	f.seedSession(ckAltWorker, ckAltRepo, "worker")
	f.seedReviewGroup(ckAltWorker, ckAltRepo, 1, ckAltWorkerReview)

	sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator", nil)
	code, body := reviewSummary(t, sc, ckAltWorker)
	if code != http.StatusForbidden {
		t.Fatalf("coordinator reading a cross-repo review-group summary got %d, want 403: %s", code, body)
	}
}

// TestCheckinReviewSummary_MissingParamIsBadRequest mirrors
// TestCheckin_MissingSessionParamIsBadRequest.
func TestCheckinReviewSummary_MissingParamIsBadRequest(t *testing.T) {
	f := newCheckinFixture(t)
	f.seedSession(ckCoordinator, ckRepo, "coordinator")

	sc := f.sidecarFor(ckCoordinator, ckRepo, "coordinator", nil)
	rr := doHostAPI(t, sc, http.MethodGet, "/checkin/review-summary", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing parent param got %d, want 400: %s", rr.Code, rr.Body.String())
	}
}
