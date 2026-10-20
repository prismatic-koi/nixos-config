package session

// non_worktree_root_role_test.go — the start role of the root session of a
// non-worktree project (issue #3179).
//
// The root-session start path gives that session no role. A legacy
// root_agent_name ("worker" from before #954, or "review-goal" as in #2658)
// must not become the start role on restore, and the start path clears it so
// that agent_status agrees with the start config of the sidecar.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

func openNWRTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// nwrBareWorktree returns a worktree inside a fake bare+worktree layout.
func nwrBareWorktree(t *testing.T, branch string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".bare"), 0o755); err != nil {
		t.Fatalf("mkdir .bare: %v", err)
	}
	wt := filepath.Join(root, branch)
	if err := os.Mkdir(wt, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	return wt
}

func TestDefaultAgentForSession_NonWorktreeRootIgnoresStaleRole(t *testing.T) {
	d := openNWRTestDB(t)
	plain := t.TempDir()
	wt := nwrBareWorktree(t, "feature")

	seed := func(session, dir, role string) {
		t.Helper()
		if err := d.UpsertStatusSeedRootAgentName(session, "x", dir, "idle", nil, nil, role, "", ""); err != nil {
			t.Fatalf("seed %q: %v", session, err)
		}
	}
	seed("prism-test-root", plain, "worker")
	seed("prism-test-review", plain, "review-goal")
	seed("prism-test-root~investigate-v2", plain, "investigate")
	seed("prism-test-wt", wt, "worker")

	cases := []struct {
		session, dir, want, why string
	}{
		{"prism-test-root", plain, "", "a stale worker role must not become the start role of a non-worktree root"},
		{"prism-test-review", plain, "", "a stale review-goal role must not become the start role of a non-worktree root"},
		{"prism-test-root~investigate-v2", plain, "investigate", "a descendant keeps its recorded role"},
		{"prism-test-wt", wt, "worker", "a session in a worktree keeps its recorded role"},
	}
	for _, tc := range cases {
		if got := DefaultAgentForSession(tc.session, tc.dir, "", d); got != tc.want {
			t.Errorf("DefaultAgentForSession(%q) = %q, want %q — %s", tc.session, got, tc.want, tc.why)
		}
	}

	if got := DefaultAgentForSession("prism-test-root", plain, "worker", d); got != "worker" {
		t.Errorf("explicit role = %q, want %q (an explicit role still wins)", got, "worker")
	}
}

// seedStaleIncarnation seeds an agent_status row with a stale role, and an
// open sessions row for instanceID that carries the same stale role. This is
// the shape that `prism restore` leaves before session.Create runs.
func seedStaleIncarnation(t *testing.T, d *db.DB, session, dir, instanceID string) {
	t.Helper()
	if err := d.UpsertStatusSeedRootAgentName(session, "x", dir, "idle", nil, nil, "worker", "", ""); err != nil {
		t.Fatalf("seed agent_status: %v", err)
	}
	if err := d.SetInstanceID(session, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	stale := "worker"
	if err := d.InsertSession(db.Session{
		InstanceID:    instanceID,
		SessionName:   session,
		AgentRole:     &stale,
		RootAgentName: &stale,
		Worktree:      dir,
		Harness:       "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
}

// sessionRoles returns agent_role and root_agent_name of the sessions row for
// instanceID, with NULL as "".
func sessionRoles(t *testing.T, d *db.DB, instanceID string) (agentRole, rootAgentName string) {
	t.Helper()
	s, err := d.SessionByInstanceID(instanceID)
	if err != nil || s == nil {
		t.Fatalf("SessionByInstanceID(%q): s=%v err=%v", instanceID, s, err)
	}
	if s.AgentRole != nil {
		agentRole = *s.AgentRole
	}
	if s.RootAgentName != nil {
		rootAgentName = *s.RootAgentName
	}
	return agentRole, rootAgentName
}

func TestClearStaleNonWorktreeRootRole(t *testing.T) {
	plain := t.TempDir()
	wt := nwrBareWorktree(t, "feature")
	projectRoot := filepath.Dir(wt)

	cases := []struct {
		name      string
		session   string
		dir       string
		agent     string
		wantClear bool
	}{
		{"role-less non-worktree root: cleared", "prism-test-root", plain, "", true},
		{"explicit role: kept", "prism-test-root", plain, "worker", false},
		{"descendant: kept", "prism-test-root~investigate-v2", plain, "", false},
		{"worktree session: kept", "prism-test-wt", wt, "", false},
		{"project root of a bare+worktree layout: kept", "prism-test-proj", projectRoot, "", false},
		{"worktree-shaped name: kept", "prism-test@feature", plain, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := openNWRTestDB(t)
			const inst = "inst-current"
			seedStaleIncarnation(t, d, tc.session, tc.dir, inst)

			if err := ClearStaleNonWorktreeRootRole(d, tc.session, tc.dir, tc.agent, inst); err != nil {
				t.Fatalf("ClearStaleNonWorktreeRootRole: %v", err)
			}

			want := "worker"
			if tc.wantClear {
				want = ""
			}
			name, _, err := d.RootAgentName(tc.session)
			if err != nil {
				t.Fatalf("RootAgentName: %v", err)
			}
			if name != want {
				t.Errorf("agent_status.root_agent_name = %q, want %q", name, want)
			}
			role, root := sessionRoles(t, d, inst)
			if role != want || root != want {
				t.Errorf("sessions row: agent_role = %q, root_agent_name = %q, want %q for both", role, root, want)
			}
		})
	}
}

// TestClearStaleNonWorktreeRootRole_IncarnationScope pins which sessions row
// the clear touches. With no instance ID from the caller (the `prism switch`
// path), it uses agent_status.instance_id. An ended row is history: that
// incarnation ran with the stored role, so the clear keeps it.
func TestClearStaleNonWorktreeRootRole_IncarnationScope(t *testing.T) {
	const session = "prism-test-root"
	plain := t.TempDir()

	t.Run("no instance ID: agent_status.instance_id is used", func(t *testing.T) {
		d := openNWRTestDB(t)
		seedStaleIncarnation(t, d, session, plain, "inst-open")
		if err := ClearStaleNonWorktreeRootRole(d, session, plain, "", ""); err != nil {
			t.Fatalf("ClearStaleNonWorktreeRootRole: %v", err)
		}
		if role, root := sessionRoles(t, d, "inst-open"); role != "" || root != "" {
			t.Errorf("open sessions row: agent_role = %q, root_agent_name = %q, want NULL for both", role, root)
		}
	})

	t.Run("ended incarnation keeps its history", func(t *testing.T) {
		d := openNWRTestDB(t)
		seedStaleIncarnation(t, d, session, plain, "inst-ended")
		if err := d.UpdateSessionEnded("inst-ended", "finished"); err != nil {
			t.Fatalf("UpdateSessionEnded: %v", err)
		}
		if err := ClearStaleNonWorktreeRootRole(d, session, plain, "", ""); err != nil {
			t.Fatalf("ClearStaleNonWorktreeRootRole: %v", err)
		}
		if role, root := sessionRoles(t, d, "inst-ended"); role != "worker" || root != "worker" {
			t.Errorf("ended sessions row: agent_role = %q, root_agent_name = %q, want \"worker\" for both", role, root)
		}
		if name, _, _ := d.RootAgentName(session); name != "" {
			t.Errorf("agent_status.root_agent_name = %q, want NULL", name)
		}
	})
}
