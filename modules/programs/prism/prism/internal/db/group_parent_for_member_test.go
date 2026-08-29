package db_test

// Tests for GroupParentForMember.
//
// GroupParentForMember is the scope check behind tier 1 of the /checkin
// permission model: a worker may read the review agents of its own session and
// nothing else. The property that matters is what the helper does NOT do — it
// has no name heuristic. ParentSessionFor, its sibling, falls back to stripping
// the "~…" suffix off the session name, which would admit any session whose
// name merely looks like a review agent of the caller.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedGroupMember registers a group owned by parent and attaches member to it,
// mirroring what `prism review` does at spawn time.
func seedGroupMember(t *testing.T, d *db.DB, parent, member, repo string) string {
	t.Helper()
	groupID, err := d.RegisterGroup(parent)
	if err != nil {
		t.Fatalf("RegisterGroup(%q): %v", parent, err)
	}
	if err := d.UpsertStatus(member, repo, "/tmp/"+repo, "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%q): %v", member, err)
	}
	if err := d.SetGroupID(member, groupID); err != nil {
		t.Fatalf("SetGroupID(%q): %v", member, err)
	}
	return groupID
}

// TestGroupParentForMember_ResolvesThroughTheJoin covers the happy path: a
// review agent resolves to the session that spawned its round.
func TestGroupParentForMember_ResolvesThroughTheJoin(t *testing.T) {
	d := openTestDB(t)
	const (
		parent = "repo@feature"
		member = "repo@feature~review-1-review-code"
	)
	seedGroupMember(t, d, parent, member, "repo")

	got, found, err := d.GroupParentForMember(member)
	if err != nil {
		t.Fatalf("GroupParentForMember: %v", err)
	}
	if !found {
		t.Fatalf("found = false, want true")
	}
	if got != parent {
		t.Errorf("parent = %q, want %q", got, parent)
	}
}

// TestGroupParentForMember_EachRoundResolvesToTheSameParent covers the
// edge-case AC "a worker checking in on a review agent from an earlier round
// of its own session is permitted". Each round registers its own
// session_groups row; every one of those rows carries the same parent_session.
func TestGroupParentForMember_EachRoundResolvesToTheSameParent(t *testing.T) {
	d := openTestDB(t)
	const parent = "repo@feature"
	round1 := "repo@feature~review-1-review-goal"
	round2 := "repo@feature~review-2-review-goal"
	seedGroupMember(t, d, parent, round1, "repo")
	seedGroupMember(t, d, parent, round2, "repo")

	for _, member := range []string{round1, round2} {
		got, found, err := d.GroupParentForMember(member)
		if err != nil {
			t.Fatalf("GroupParentForMember(%q): %v", member, err)
		}
		if !found || got != parent {
			t.Errorf("GroupParentForMember(%q) = (%q, %v), want (%q, true)", member, got, found, parent)
		}
	}
}

// TestGroupParentForMember_NoNameHeuristic is the security-relevant case. The
// name of each session below reads as a review agent of "repo@feature", and
// ParentSessionFor returns that parent for every one of them. The DB-backed
// helper must report "not found" instead, because there is no session_groups
// row to back the claim.
func TestGroupParentForMember_NoNameHeuristic(t *testing.T) {
	cases := []struct {
		name string
		// seed prepares the DB and returns the session to look up.
		seed func(t *testing.T, d *db.DB) string
	}{
		{
			name: "no agent_status row at all",
			seed: func(t *testing.T, d *db.DB) string {
				return "repo@feature~review-1-review-code"
			},
		},
		{
			name: "row exists but group_id is NULL",
			seed: func(t *testing.T, d *db.DB) string {
				const member = "repo@feature~review-1-review-code"
				if err := d.UpsertStatus(member, "repo", "/tmp/repo", "active", nil, nil); err != nil {
					t.Fatalf("UpsertStatus: %v", err)
				}
				return member
			},
		},
		{
			name: "session_groups row deleted after the member joined",
			seed: func(t *testing.T, d *db.DB) string {
				const member = "repo@feature~review-1-review-code"
				groupID := seedGroupMember(t, d, "repo@feature", member, "repo")
				// agent_status.group_id is ON DELETE SET NULL, so the member
				// row survives as an orphan with a review-agent name.
				if err := d.QueryRow(
					"DELETE FROM session_groups WHERE group_id = ? RETURNING group_id", groupID,
				).Scan(new(string)); err != nil {
					t.Fatalf("delete session_groups row: %v", err)
				}
				return member
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			member := tc.seed(t, d)

			got, found, err := d.GroupParentForMember(member)
			if err != nil {
				t.Fatalf("GroupParentForMember: %v", err)
			}
			if found {
				t.Errorf("found = true (parent %q), want false — the name must not stand in for a session_groups row", got)
			}

			// Contrast: the name-heuristic sibling DOES answer here. If this
			// stops holding, the two helpers have converged and the scope
			// check needs re-reading.
			if heuristic := d.ParentSessionFor(member); heuristic != "repo@feature" {
				t.Errorf("ParentSessionFor(%q) = %q, want \"repo@feature\" — this test's contrast case no longer holds", member, heuristic)
			}
		})
	}
}

// TestGroupParentForMember_DoesNotMatchTheParentItself pins the self-checkin
// boundary at the DB layer: a worker is not a member of the groups it owns, so
// looking itself up finds nothing.
func TestGroupParentForMember_DoesNotMatchTheParentItself(t *testing.T) {
	d := openTestDB(t)
	const parent = "repo@feature"
	if err := d.UpsertStatus(parent, "repo", "/tmp/repo", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	seedGroupMember(t, d, parent, "repo@feature~review-1-review-code", "repo")

	got, found, err := d.GroupParentForMember(parent)
	if err != nil {
		t.Fatalf("GroupParentForMember: %v", err)
	}
	if found {
		t.Errorf("found = true (parent %q), want false — the owner of a group is not a member of it", got)
	}
}
