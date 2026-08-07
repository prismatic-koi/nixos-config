package db_test

// historical_group_reads_test.go — issue #2649.
//
// Two read sites answer a question about a review round that is already over.
// Both used db.GroupResults, which drops rows whose ended_at is set.
//
// That was already lossy before #2649 — a parent cleanup closes its review
// children, and these reads run at or after cleanup time. The automatic
// release makes it certain and much earlier: every member of a delivered round
// is closed 15 minutes later, so both reads see an empty map for every round
// older than that.
//
// These tests seed a complete round, close every member the way the release
// does, and assert each read still answers from the surviving history.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedClosedReviewRound seeds a review group for parent whose members all
// finished with the given verdict, then closes every member row — the state
// the automatic release leaves behind. It returns the group id.
func seedClosedReviewRound(t *testing.T, d *db.DB, parent, verdict string, n int) string {
	t.Helper()
	groupID, err := d.RegisterGroupWithPR(parent, "2649", 1)
	if err != nil {
		t.Fatalf("RegisterGroupWithPR: %v", err)
	}
	roles := []string{"review-goal", "review-code", "review-qa", "review-security", "review-context"}
	for i := 0; i < n; i++ {
		sess := parent + "~review-1-" + roles[i%len(roles)]
		if err := d.UpsertStatus(sess, "prism-test-repo", "/tmp/test-wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
		if err := d.WriteEvent(db.Event{
			ID:          uuid.New().String(),
			SessionName: sess,
			Repo:        "prism-test-repo",
			Worktree:    "/tmp/test-wt",
			Type:        "msg_assistant",
			Payload:     `{"text":"Reviewed.\n<verdict>` + verdict + `</verdict>"}`,
			CreatedAt:   time.Now(),
		}); err != nil {
			t.Fatalf("WriteEvent(%q): %v", sess, err)
		}
		// Close the row exactly as the automatic release does.
		if err := d.SetEnded(sess); err != nil {
			t.Fatalf("SetEnded(%q): %v", sess, err)
		}
	}
	return groupID
}

// TestComputeSpawnOutcome_ReviewRollupSurvivesTheRelease covers the fallback
// review roll-up in ComputeSpawnOutcome (internal/db/sessions.go).
//
// The roll-up is a fallback: #2110 added a dedicated write path that persists
// the verdict at review-complete time, and the merge prefers it. The fallback
// therefore serves exactly the sessions where that write never fired. Reading
// through GroupResults, those sessions lost their verdict entirely once the
// release closed the member rows.
func TestComputeSpawnOutcome_ReviewRollupSurvivesTheRelease(t *testing.T) {
	d := openTestDB(t)
	parent := "prism-test@rollup-survives"

	iid := uuid.New().String()
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: parent,
		Repo:        "prism-test-repo",
		Worktree:    "/tmp/test-wt",
		Harness:     "pi",
		StartedAt:   time.Now().Add(-5 * time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	seedClosedReviewRound(t, d, parent, "PASS", 5)

	// No UpdateSpawnOutcomeReviewResult call — this is the fallback path.
	out, err := d.ComputeSpawnOutcome(iid)
	if err != nil {
		t.Fatalf("ComputeSpawnOutcome: %v", err)
	}
	if out == nil {
		t.Fatal("ComputeSpawnOutcome returned nil")
	}
	if out.ReviewGroupID == nil {
		t.Fatal("ReviewGroupID is nil — the group lookup itself failed")
	}
	if out.ReviewVerdict == nil {
		t.Fatalf("ReviewVerdict is nil after the release closed the member rows — the roll-up read must survive it")
	}
	if *out.ReviewVerdict != "pass" {
		t.Errorf("ReviewVerdict = %q, want %q", *out.ReviewVerdict, "pass")
	}
	if out.ReviewPassCount == nil || *out.ReviewPassCount != 5 {
		t.Errorf("ReviewPassCount = %v, want 5", out.ReviewPassCount)
	}
	if out.ReviewFailCount == nil || *out.ReviewFailCount != 0 {
		t.Errorf("ReviewFailCount = %v, want 0", out.ReviewFailCount)
	}
}

// TestAbtestGroupSessions_SurvivesTheRelease covers the second historical read
// site: `prism stats abtest <group_id>` (cmd/stats_compare.go →
// db.AbtestGroupSessions).
//
// session_groups rows are written only by `prism review`
// (db.RegisterGroupWithPR is its sole caller), so the members this resolves
// ARE review agents. Reading through GroupResults, the call returned an empty
// map once the release closed them and the function hard-errored with
// "no members found for group" — a retrospective command failing on a round
// that completed normally.
func TestAbtestGroupSessions_SurvivesTheRelease(t *testing.T) {
	d := openTestDB(t)
	parent := "prism-test@abtest-survives"

	groupID := seedClosedReviewRound(t, d, parent, "PASS", 3)
	// Give each member a sessions row so the resolver has something to return.
	for _, role := range []string{"review-goal", "review-code", "review-qa"} {
		if err := d.InsertSession(db.Session{
			InstanceID:  uuid.New().String(),
			SessionName: parent + "~review-1-" + role,
			Repo:        "prism-test-repo",
			Worktree:    "/tmp/test-wt",
			Harness:     "pi",
			StartedAt:   time.Now().Add(-5 * time.Minute),
		}); err != nil {
			t.Fatalf("InsertSession(%s): %v", role, err)
		}
	}

	got, err := d.AbtestGroupSessions(groupID)
	if err != nil {
		t.Fatalf("AbtestGroupSessions after the release closed the members: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("resolved %d members, want 3 — the release must not empty a historical group read", len(got))
	}
}
