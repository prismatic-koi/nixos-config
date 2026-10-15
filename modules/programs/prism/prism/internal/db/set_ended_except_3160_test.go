package db_test

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// SetEndedExcept ends the session and its review children, except the
// children in keep. Both tables follow the same rule: a kept child keeps
// agent_status.ended_at and sessions.ended_at NULL (#3160).
func TestSetEndedExcept_KeepsListedChildrenOpen(t *testing.T) {
	d := openTestDB(t)
	const parent = "repo@keep_100%"
	kept := parent + "~review-1-review-goal"
	ended := parent + "~review-1-review-code"
	unrelated := "repo@keep_1000~review-1-review-goal"

	for i, name := range []string{parent, kept, ended, unrelated} {
		if err := d.UpsertStatus(name, "repo", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", name, err)
		}
		iid := []string{"iid-parent", "iid-kept", "iid-ended", "iid-unrelated"}[i]
		if err := d.SetInstanceID(name, iid); err != nil {
			t.Fatalf("SetInstanceID(%q): %v", name, err)
		}
		if err := d.InsertSession(db.Session{InstanceID: iid, SessionName: name, Repo: "repo", Worktree: "/wt"}); err != nil {
			t.Fatalf("InsertSession(%q): %v", name, err)
		}
	}

	if err := d.SetEndedExcept(parent, []string{kept}); err != nil {
		t.Fatalf("SetEndedExcept: %v", err)
	}

	for name, wantEnded := range map[string]bool{parent: true, ended: true, kept: false, unrelated: false} {
		st, err := d.CurrentStatus(name)
		if err != nil || st == nil {
			t.Fatalf("CurrentStatus(%q) = %v, %v", name, st, err)
		}
		if got := st.EndedAt != nil; got != wantEnded {
			t.Errorf("agent_status %q ended = %v, want %v", name, got, wantEnded)
		}
		sess, err := d.SessionByInstanceID(*st.InstanceID)
		if err != nil || sess == nil {
			t.Fatalf("SessionByInstanceID(%q) = %v, %v", *st.InstanceID, sess, err)
		}
		if got := sess.EndedAt != nil; got != wantEnded {
			t.Errorf("sessions %q ended = %v, want %v", name, got, wantEnded)
		}
	}
}
