package db_test

// Tests for ReapableReviewAgents.
//
// ReapableReviewAgents is the candidate query behind the automatic release of
// finished review-agent sessions. It is the layer that makes the release safe,
// so these tests pin the SQL contract directly rather than through the caller:
// every arm of the predicate, the group scoping, and the projection.

import (
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedReapGroup registers a group for parent and attaches one member per entry
// in states, named `<parent>~review-1-agent-<i>`. It returns the group id and
// the member names.
func seedReapGroup(t *testing.T, d *db.DB, parent string, states []string, delivered bool) (string, []string) {
	t.Helper()
	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup(%q): %v", parent, err)
	}
	names := make([]string, 0, len(states))
	for i, state := range states {
		name := parent + "~review-1-agent-" + string(rune('a'+i))
		if err := d.UpsertStatus(name, "nixos-config", "/wt", state, nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", name, err)
		}
		if err := d.SetGroupID(name, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", name, err)
		}
		names = append(names, name)
	}
	if delivered {
		if err := d.SetGroupDeliveredAt(groupID); err != nil {
			t.Fatalf("SetGroupDeliveredAt: %v", err)
		}
	}
	return groupID, names
}

// futureCutoff is a cut-off far enough ahead that any group delivered during
// the test is inside it.
func futureCutoff() int64 {
	return time.Now().Add(24 * time.Hour).UnixMilli()
}

func candidateNames(cs []db.ReapCandidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.SessionName)
	}
	return out
}

// TestReapableReviewAgents_RequiresDeliveredGroup is the load-bearing safety
// property. A group whose review-complete prompt has NOT been delivered yields
// no candidates, however terminal its members are. This is what makes it
// impossible to release an agent while its round is still running.
func TestReapableReviewAgents_RequiresDeliveredGroup(t *testing.T) {
	d := openTestDB(t)

	_, _ = seedReapGroup(t, d, "nixos-config@undelivered",
		[]string{"finished", "finished", "finished"}, false)

	got, err := d.ReapableReviewAgents("", futureCutoff())
	if err != nil {
		t.Fatalf("ReapableReviewAgents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got candidates %v from an undelivered group, want none", candidateNames(got))
	}
}

// TestReapableReviewAgents_TerminalStatesOnly pins the per-session arm: only
// finished / error / deleted are returned. `interrupted` is excluded on
// purpose — an interrupted agent can still be redirected with `prism prompt`
// so it is no more terminal here than it is for GroupCompleted.
func TestReapableReviewAgents_TerminalStatesOnly(t *testing.T) {
	reapable := map[string]bool{
		"finished": true, "error": true, "deleted": true,
		"active": false, "idle": false, "waiting": false,
		"compacting": false, "reviewing": false, "escalated": false,
		"interrupted": false,
	}
	for state, want := range reapable {
		t.Run(state, func(t *testing.T) {
			d := openTestDB(t)
			_, names := seedReapGroup(t, d, "nixos-config@state-"+state, []string{state}, true)

			got, err := d.ReapableReviewAgents("", futureCutoff())
			if err != nil {
				t.Fatalf("ReapableReviewAgents: %v", err)
			}
			if want && len(got) != 1 {
				t.Fatalf("state %q: got %v, want the single candidate %v", state, candidateNames(got), names)
			}
			if !want && len(got) != 0 {
				t.Fatalf("state %q is not terminal, but it was returned as a candidate: %v", state, candidateNames(got))
			}
			if want && got[0].State != state {
				t.Errorf("candidate State = %q, want %q", got[0].State, state)
			}
		})
	}
}

// TestReapableReviewAgents_ExcludesAlreadyEndedRows pins the idempotence arm.
// A row that a previous sweep closed must not come back.
func TestReapableReviewAgents_ExcludesAlreadyEndedRows(t *testing.T) {
	d := openTestDB(t)

	_, names := seedReapGroup(t, d, "nixos-config@already-ended", []string{"finished"}, true)
	if err := d.SetEnded(names[0]); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	got, err := d.ReapableReviewAgents("", futureCutoff())
	if err != nil {
		t.Fatalf("ReapableReviewAgents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none — the row is already ended", candidateNames(got))
	}
}

// TestReapableReviewAgents_HonoursCutoff pins the grace-period arm: the
// cut-off is compared against session_groups.delivered_at.
func TestReapableReviewAgents_HonoursCutoff(t *testing.T) {
	d := openTestDB(t)

	_, names := seedReapGroup(t, d, "nixos-config@cutoff", []string{"finished"}, true)

	// A cut-off in the past excludes a group delivered just now.
	past := time.Now().Add(-time.Hour).UnixMilli()
	got, err := d.ReapableReviewAgents("", past)
	if err != nil {
		t.Fatalf("ReapableReviewAgents (past cut-off): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v with a cut-off one hour in the past, want none", candidateNames(got))
	}

	// A cut-off in the future includes it.
	got, err = d.ReapableReviewAgents("", futureCutoff())
	if err != nil {
		t.Fatalf("ReapableReviewAgents (future cut-off): %v", err)
	}
	if len(got) != 1 || got[0].SessionName != names[0] {
		t.Errorf("got %v with a future cut-off, want %v", candidateNames(got), names)
	}
}

// TestReapableReviewAgents_GroupScope pins the optional group filter: the
// monitor sweeps only the round it delivered.
func TestReapableReviewAgents_GroupScope(t *testing.T) {
	d := openTestDB(t)

	mineID, mine := seedReapGroup(t, d, "nixos-config@scope-mine", []string{"finished"}, true)
	_, theirs := seedReapGroup(t, d, "nixos-config@scope-theirs", []string{"finished"}, true)

	scoped, err := d.ReapableReviewAgents(mineID, futureCutoff())
	if err != nil {
		t.Fatalf("ReapableReviewAgents (scoped): %v", err)
	}
	if len(scoped) != 1 || scoped[0].SessionName != mine[0] {
		t.Fatalf("scoped query returned %v, want exactly %v", candidateNames(scoped), mine)
	}

	all, err := d.ReapableReviewAgents("", futureCutoff())
	if err != nil {
		t.Fatalf("ReapableReviewAgents (unscoped): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("unscoped query returned %v, want both %v and %v", candidateNames(all), mine, theirs)
	}
}

// TestReapableReviewAgents_ExcludesNonGroupSessions pins the join: a session
// that belongs to no review group is never a candidate, whatever its state.
// Workers, coordinators, and investigators all fall into this class.
func TestReapableReviewAgents_ExcludesNonGroupSessions(t *testing.T) {
	d := openTestDB(t)

	for _, name := range []string{
		"nixos-config@main",
		"nixos-config@some-worker",
		"nixos-config@some-worker~investigate-a-slug",
	} {
		if err := d.UpsertStatus(name, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", name, err)
		}
	}
	// One genuine, eligible review agent, so a zero result cannot come from
	// the query matching nothing at all.
	_, mine := seedReapGroup(t, d, "nixos-config@with-group", []string{"finished"}, true)

	got, err := d.ReapableReviewAgents("", futureCutoff())
	if err != nil {
		t.Fatalf("ReapableReviewAgents: %v", err)
	}
	if len(got) != 1 || got[0].SessionName != mine[0] {
		t.Errorf("got %v, want only the group member %v", candidateNames(got), mine)
	}
}

// TestReapableReviewAgents_Projection pins the returned fields, including the
// COALESCE that turns a NULL isolation_mode into "" for the reaper's
// fall-back-to-bwrap dispatch.
func TestReapableReviewAgents_Projection(t *testing.T) {
	d := openTestDB(t)

	parent := "nixos-config@projection"
	groupID, names := seedReapGroup(t, d, parent, []string{"finished", "error"}, true)
	// Give the first member a mode and leave the second's column NULL.
	if err := d.SetIsolationMode(names[0], "bwrap"); err != nil {
		t.Fatalf("SetIsolationMode: %v", err)
	}

	got, err := d.ReapableReviewAgents("", futureCutoff())
	if err != nil {
		t.Fatalf("ReapableReviewAgents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2: %v", len(got), candidateNames(got))
	}
	// Results are ordered by session_name, so index 0 is names[0].
	if got[0].SessionName != names[0] || got[1].SessionName != names[1] {
		t.Fatalf("results are not ordered by session name: %v", candidateNames(got))
	}
	for i, c := range got {
		if c.GroupID != groupID {
			t.Errorf("candidate %d: GroupID = %q, want %q", i, c.GroupID, groupID)
		}
		if c.ParentSession != parent {
			t.Errorf("candidate %d: ParentSession = %q, want %q", i, c.ParentSession, parent)
		}
		if c.DeliveredAt <= 0 {
			t.Errorf("candidate %d: DeliveredAt = %d, want the group's delivered_at", i, c.DeliveredAt)
		}
	}
	if got[0].IsolationMode != "bwrap" {
		t.Errorf("IsolationMode = %q, want %q", got[0].IsolationMode, "bwrap")
	}
	if got[1].IsolationMode != "" {
		t.Errorf("IsolationMode for a NULL column = %q, want \"\" (the reaper falls back to bwrap)", got[1].IsolationMode)
	}
}

// TestReapableReviewAgents_EmptyDatabase pins the no-rows path: an empty
// result, not an error and not a nil-deref.
func TestReapableReviewAgents_EmptyDatabase(t *testing.T) {
	d := openTestDB(t)

	got, err := d.ReapableReviewAgents("", futureCutoff())
	if err != nil {
		t.Fatalf("ReapableReviewAgents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v from an empty database, want none", candidateNames(got))
	}
}
