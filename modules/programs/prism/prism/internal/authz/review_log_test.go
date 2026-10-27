package authz

// review_log_test.go — the properties of AuthorizeReviewLog that neither
// route proves by itself. The rule table is pinned per route, in
// internal/sidecar/host_api_review_log_test.go and cmd/reviews_log_test.go.

import (
	"net/http"
	"testing"
)

func TestAuthorizeReviewLog_FailsClosed(t *testing.T) {
	cases := []struct {
		name string
		req  ReviewLogRequest
		want int
	}{
		{"empty caller with an empty target", ReviewLogRequest{Target: ""}, http.StatusForbidden},
		{"empty caller", ReviewLogRequest{Target: "demo@feature"}, http.StatusForbidden},
		{"coordinator with no DB, other session", ReviewLogRequest{Caller: "demo@main", CallerIsCoordinator: true, Target: "demo@feature"}, http.StatusForbidden},
		{"worker, other session of its repo", ReviewLogRequest{Caller: "demo@feature", Target: "demo@other"}, http.StatusForbidden},
		{"worker names a coordinator", ReviewLogRequest{Caller: "demo@feature", Target: "demo@main"}, http.StatusForbidden},
		{"target with ..", ReviewLogRequest{Caller: "demo@feature", Target: "demo@feature/.."}, http.StatusBadRequest},
		{"no target", ReviewLogRequest{Caller: "demo@feature"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		tc.req.Logger = quietLogger()
		got := AuthorizeReviewLog(tc.req)
		if got.Allow || got.Status != tc.want {
			t.Errorf("%s: decision = %+v, want a refusal with status %d", tc.name, got, tc.want)
		}
	}
}

// The same target and two callers give two verdicts. If the rule reads the
// caller from anywhere other than ReviewLogRequest.Caller, they converge.
func TestAuthorizeReviewLog_CallerIsAParameter(t *testing.T) {
	const target = "demo@feature"
	own := AuthorizeReviewLog(ReviewLogRequest{Caller: target, Target: target, Logger: quietLogger()})
	other := AuthorizeReviewLog(ReviewLogRequest{Caller: "demo@sibling", Target: target, Logger: quietLogger()})
	if !own.Allow {
		t.Errorf("own session refused: %s", own.Message)
	}
	if other.Allow {
		t.Error("another worker was admitted to the review logs of demo@feature")
	}
}
