package cmd

// agent_run_review_diff_test.go — issue #3377. prism agent-run gives a review
// agent the review diff dir of the parent of its group, from the DB, and
// gives no other session a review diff dir.

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

func TestReviewDiffDirFor_GroupParentOnly_3377(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := sidecartest.OpenDB(t, filepath.Join(t.TempDir(), "prism.db"))
	const parent = "prism-test@3377-parent"
	const agent = parent + "~review-1-review-code"
	// The name of this session says that it is a review agent of parent,
	// but it is in no group. A name is not proof of the parent.
	const named = parent + "~review-1-review-qa"
	const worker = "prism-test@3377-worker"

	groupID, err := d.RegisterGroupWithPR(parent, "7", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}
	for _, s := range []string{agent, named, worker} {
		if err := d.UpsertStatus(s, "prism-test", "/wt", "running", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", s, err)
		}
	}
	if err := d.SetGroupID(agent, groupID); err != nil {
		t.Fatalf("SetGroupID: %v", err)
	}

	want, err := review.DiffDirForParent(parent)
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewDiffDirFor(d, agent); got != want {
		t.Errorf("reviewDiffDirFor(review agent) = %q, want %q", got, want)
	}
	for _, s := range []string{named, worker, "prism-test@3377-missing"} {
		if got := reviewDiffDirFor(d, s); got != "" {
			t.Errorf("reviewDiffDirFor(%q) = %q, want \"\"", s, got)
		}
	}
}
