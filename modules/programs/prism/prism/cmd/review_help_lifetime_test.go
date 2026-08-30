package cmd

// Tests that `prism review --help` states the current review-agent session
// lifetime.
//
// `prism review --help` states how long review-agent sessions live. That text
// is the copy a user and an agent actually read. If the file header is
// corrected but the cobra Long string 43 lines below it is not, --help keeps
// promising a stale contract ("sessions persist until prism cleanup is
// invoked on the parent").
//
// A comment listing the prose sites cannot catch that. This test can.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/review"
)

// TestReviewHelp_StatesTheReleaseWindow asserts that the help text names the
// grace period, and names it with the value review.ReapGracePeriod actually
// carries. Changing the constant without changing the help text fails here.
func TestReviewHelp_StatesTheReleaseWindow(t *testing.T) {
	long := reviewCmd.Long

	// The constant rendered the way prose says it: "15 minutes".
	mins := int(review.ReapGracePeriod / time.Minute)
	want := strconv.Itoa(mins) + " minutes"
	if !strings.Contains(long, want) {
		t.Errorf("`prism review --help` does not state the release window %q.\n"+
			"review.ReapGracePeriod is %s — update the cobra Long string in cmd/review.go to match.\n"+
			"Long text was:\n%s", want, review.ReapGracePeriod, long)
	}
}

// TestReviewHelp_DoesNotClaimSessionsPersistUntilCleanup is the direct guard
// for the stale sentence. Review agents are not kept until the parent is
// cleaned up, so any wording that promises that is false.
func TestReviewHelp_DoesNotClaimSessionsPersistUntilCleanup(t *testing.T) {
	long := strings.ToLower(reviewCmd.Long)
	for _, banned := range []string{
		"persist until prism cleanup",
		"persist until cleanup",
	} {
		if strings.Contains(long, banned) {
			t.Errorf("`prism review --help` contains %q — review agents are released %s after the round is delivered, so that promise is false",
				banned, review.ReapGracePeriod)
		}
	}
}

// TestReviewHelp_PointsAtTheSurvivingReads makes sure the help text does not
// stop at "the session goes away". The reads that still work after a release
// are the actionable half: without them a reader concludes the agent's
// reasoning is gone, and it is not.
func TestReviewHelp_PointsAtTheSurvivingReads(t *testing.T) {
	long := reviewCmd.Long
	for _, want := range []string{"prism checkin", "prism reviews list"} {
		if !strings.Contains(long, want) {
			t.Errorf("`prism review --help` does not mention %q — a reader must be told which reads survive the release", want)
		}
	}
}
