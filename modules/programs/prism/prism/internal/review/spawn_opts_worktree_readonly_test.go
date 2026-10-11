package review_test

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/review"
)

// TestNewReviewerSpawnOpts_WorktreeReadOnly: every review agent is spawned
// with a read-only worktree (#3079), so it cannot change the branch under
// review.
func TestNewReviewerSpawnOpts_WorktreeReadOnly(t *testing.T) {
	for _, mode := range []string{"bwrap", "sandbox-exec", "host"} {
		t.Run(mode, func(t *testing.T) {
			opts := review.NewReviewerSpawnOptsForTest(newReviewerSpawnInputForTest(t, "repo@branch~review-1-review-code", mode, "pi"))
			if !opts.WorktreeReadOnly {
				t.Error("SpawnOpts.WorktreeReadOnly = false, want true for a review agent")
			}
		})
	}
}
