package db_test

// Tests for the shared comparison-data helpers added in #2098:
// ResolveSessionArg, SessionIsTerminal, CompareRunOutcome, AssembleCompareRun,
// and AbtestGroupSessions. These back both the CLI direct path and the
// host-API proxy path, so their behaviour is the single source of truth for
// byte-identical `prism stats compare` / `abtest` output.

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// seedSessionForCompare inserts agent_status + sessions rows for a session in
// the given agent_status state, mirroring the cleanup-path end_state write for
// terminal states.
func seedSessionForCompare(t *testing.T, d *db.DB, sessionName, state string) (instanceID string) {
	t.Helper()
	instanceID = uuid.New().String()
	if err := d.UpsertStatus(sessionName, "repo", "/wt/"+sessionName, state, nil, nil); err != nil {
		t.Fatalf("UpsertStatus %q: %v", sessionName, err)
	}
	if err := d.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID %q: %v", sessionName, err)
	}
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "repo",
		Worktree:    "/wt/" + sessionName,
		Harness:     "pi",
		StartedAt:   time.Now().Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("InsertSession %q: %v", sessionName, err)
	}
	switch state {
	case "finished", "error", "interrupted", "deleted":
		if err := d.UpdateSessionEnded(instanceID, state); err != nil {
			t.Fatalf("UpdateSessionEnded %q: %v", sessionName, err)
		}
	}
	return instanceID
}

func TestResolveSessionArg(t *testing.T) {
	d := openTestDB(t)
	iid := seedSessionForCompare(t, d, "repo@feature", "finished")

	t.Run("by full uuid", func(t *testing.T) {
		sess, err := d.ResolveSessionArg(iid, false)
		if err != nil || sess == nil {
			t.Fatalf("ResolveSessionArg(uuid) = (%v, %v)", sess, err)
		}
		if sess.InstanceID != iid {
			t.Errorf("instance = %q, want %q", sess.InstanceID, iid)
		}
	})

	t.Run("by session name", func(t *testing.T) {
		sess, err := d.ResolveSessionArg("repo@feature", false)
		if err != nil || sess == nil {
			t.Fatalf("ResolveSessionArg(name) = (%v, %v)", sess, err)
		}
		if sess.InstanceID != iid {
			t.Errorf("instance = %q, want %q", sess.InstanceID, iid)
		}
	})

	t.Run("by unambiguous prefix", func(t *testing.T) {
		sess, err := d.ResolveSessionArg(iid[:8], false)
		if err != nil || sess == nil {
			t.Fatalf("ResolveSessionArg(prefix) = (%v, %v)", sess, err)
		}
		if sess.InstanceID != iid {
			t.Errorf("instance = %q, want %q", sess.InstanceID, iid)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := d.ResolveSessionArg("definitely-not-a-real-arg", false)
		if err == nil {
			t.Fatal("expected error for unknown arg")
		}
		if !strings.Contains(err.Error(), "definitely-not-a-real-arg") {
			t.Errorf("error should name the arg; got %v", err)
		}
	})

	t.Run("missing full uuid", func(t *testing.T) {
		ghost := "cccccccc-2222-3333-4444-555555555555"
		_, err := d.ResolveSessionArg(ghost, false)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("expected 'not found' error for missing uuid; got %v", err)
		}
	})
}

func TestSessionIsTerminal(t *testing.T) {
	d := openTestDB(t)

	t.Run("terminal via agent_status", func(t *testing.T) {
		iid := seedSessionForCompare(t, d, "repo@done", "finished")
		sess, _ := d.SessionByInstanceID(iid)
		if !d.SessionIsTerminal(sess) {
			t.Error("finished session should be terminal")
		}
	})

	t.Run("live session is not terminal", func(t *testing.T) {
		iid := seedSessionForCompare(t, d, "repo@live", "active")
		sess, _ := d.SessionByInstanceID(iid)
		if d.SessionIsTerminal(sess) {
			t.Error("active session must not be terminal")
		}
	})

	t.Run("nil session", func(t *testing.T) {
		if d.SessionIsTerminal(nil) {
			t.Error("nil session must not be terminal")
		}
	})
}

func TestAssembleCompareRun_Live(t *testing.T) {
	d := openTestDB(t)
	iid := seedSessionForCompare(t, d, "repo@live", "active")
	sess, _ := d.SessionByInstanceID(iid)

	cr := d.AssembleCompareRun(sess)
	if cr.Session == nil || cr.Session.InstanceID != iid {
		t.Fatalf("AssembleCompareRun session = %v", cr.Session)
	}
	// Live (non-terminal, no persisted outcome) → Outcome stays nil so the
	// renderer shows "—".
	if cr.Outcome != nil {
		t.Errorf("live session Outcome = %+v, want nil", cr.Outcome)
	}
}

func TestAbtestGroupSessions(t *testing.T) {
	d := openTestDB(t)
	seedSessionForCompare(t, d, "repo@zeta", "finished")
	seedSessionForCompare(t, d, "repo@alpha", "finished")

	groupID, err := d.RegisterGroup("repo@main")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	if err := d.SetGroupID("repo@zeta", groupID); err != nil {
		t.Fatalf("SetGroupID zeta: %v", err)
	}
	if err := d.SetGroupID("repo@alpha", groupID); err != nil {
		t.Fatalf("SetGroupID alpha: %v", err)
	}

	t.Run("sorted by session name", func(t *testing.T) {
		sessions, err := d.AbtestGroupSessions(groupID)
		if err != nil {
			t.Fatalf("AbtestGroupSessions: %v", err)
		}
		if len(sessions) != 2 {
			t.Fatalf("got %d sessions, want 2", len(sessions))
		}
		if sessions[0].SessionName != "repo@alpha" || sessions[1].SessionName != "repo@zeta" {
			t.Errorf("not sorted by session_name: %q, %q", sessions[0].SessionName, sessions[1].SessionName)
		}
	})

	t.Run("unknown group errors", func(t *testing.T) {
		_, err := d.AbtestGroupSessions("no-such-group")
		if err == nil || !strings.Contains(err.Error(), "no members") {
			t.Errorf("expected 'no members' error; got %v", err)
		}
	})
}
