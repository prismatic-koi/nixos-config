package cmd

// non_worktree_root_role_test.go — the direct CLI route for the root session
// of a non-worktree project (issue #3179).
//
// A `host`-mode session has no PRISM_HOST_API, so its CLI reaches the direct
// guards in cmd/ and never the host-API handler. The two routes must give one
// answer for one session. The host API decides with
//
//	authz.IsCoordinatorSession(name) || authz.IsNonWorktreeRoot(name, Config.AgentRole, Config.Worktree)
//
// (requireCoordinator in internal/sidecar/host_api.go). The direct route reads
// the same start record from agent_status. Each fixture below is one start
// record, as the start path leaves it in the DB and in the sidecar start
// config. internal/sidecar/host_api_non_worktree_root_caller_test.go pins the
// handler itself.
//
// Isolation: every test points openDB() at a t.TempDir() database, clears
// PRISM_HOST_API, and uses "prism-test" session names.

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/authz"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

const (
	nwrCLIRoot        = "prism-test-nwcli"
	nwrCLIOtherRepo   = "prism-test-nwcli-other"
	nwrCLIOtherCoord  = "prism-test-nwcli-other@main"
	nwrCLIOtherWorker = "prism-test-nwcli-other@feature"
)

// nwrCLIWorktree returns a worktree inside a fake bare+worktree layout.
func nwrCLIWorktree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".bare"), 0o755); err != nil {
		t.Fatalf("mkdir .bare: %v", err)
	}
	wt := filepath.Join(root, "feature")
	if err := os.Mkdir(wt, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	return wt
}

// TestNonWorktreeRoot_DirectGuardsMatchHostAPI pins AC9. For each start record,
// every direct-CLI coordinator guard gives the answer that requireCoordinator
// gives on the host API.
func TestNonWorktreeRoot_DirectGuardsMatchHostAPI(t *testing.T) {
	cases := []struct {
		name      string
		session   string
		startRole string // Config.AgentRole, and root_agent_name ("" = NULL)
		worktree  string // Config.Worktree, and agent_status.worktree
		want      bool
	}{
		{"root session of a non-worktree project", nwrCLIRoot, "", t.TempDir(), true},
		{"bare-named worker", nwrCLIRoot, "worker", t.TempDir(), false},
		{"bare-named session in a worktree", nwrCLIRoot, "", nwrCLIWorktree(t), false},
		{"descendant of the root session", nwrCLIRoot + "~investigate-v2", "", t.TempDir(), false},
		{"worktree coordinator", nwrCLIOtherCoord, "coordinator", nwrCLIWorktree(t), true},
		{"worktree worker", nwrCLIOtherWorker, "worker", nwrCLIWorktree(t), false},
	}
	quiet := log.New(io.Discard, "", 0)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(filepath.Join(t.TempDir(), "prism.db"))
			if err != nil {
				t.Fatalf("db.Open: %v", err)
			}
			defer d.Close()
			if err := d.UpsertStatusSeedRootAgentName(tc.session, "x", tc.worktree, "idle", nil, nil, tc.startRole, "", "host"); err != nil {
				t.Fatalf("seed: %v", err)
			}

			hostAPI := authz.IsCoordinatorSession(tc.session, d, quiet) ||
				authz.IsNonWorktreeRoot(tc.session, tc.startRole, tc.worktree)
			if hostAPI != tc.want {
				t.Fatalf("host-API verdict = %v, want %v", hostAPI, tc.want)
			}

			direct := map[string]bool{
				"requireInvestigateCoordinator": requireInvestigateCoordinator(tc.session, d) == nil,
				"requireMergesCoordinator":      requireMergesCoordinator("prism merges list", "prism merges list", tc.session, d) == nil,
				"requireAuditCoordinator":       requireAuditCoordinator(tc.session, d) == nil,
				// cmd/merge.go and cmd/profile.go call this helper inline.
				"callerHasCoordinatorRole": callerHasCoordinatorRole(tc.session, d),
			}
			for guard, got := range direct {
				if got != hostAPI {
					t.Errorf("%s admits = %v, host API admits = %v — the two routes disagree", guard, got, hostAPI)
				}
			}
		})
	}
}

// seedNWRCheckin seeds the root session of a non-worktree project, plus a
// coordinator and a worker in another repo, into the direct-checkin fixture.
func seedNWRCheckin(t *testing.T, f *directCheckinFixture) {
	t.Helper()
	if err := f.DB.UpsertStatusSeedRootAgentName(nwrCLIRoot, nwrCLIRoot, t.TempDir(), "active", nil, nil, "", "", "host"); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	f.seedSession(nwrCLIOtherCoord, nwrCLIOtherRepo, "coordinator")
	f.seedSession(nwrCLIOtherWorker, nwrCLIOtherRepo, "worker")
}

// TestNonWorktreeRoot_DirectCheckinTiers pins AC8 on the direct route. The
// root session reads at tier 2. Tier 3 needs its repo in the privileged list.
func TestNonWorktreeRoot_DirectCheckinTiers(t *testing.T) {
	t.Run("tier 2: cross-repo coordinator is in reach", func(t *testing.T) {
		f := newDirectCheckinFixture(t)
		seedNWRCheckin(t, f)
		if err := authorizeDirectCheckinFor(nwrCLIRoot, nwrCLIOtherCoord); err != nil {
			t.Fatalf("refused: %v", err)
		}
	})
	t.Run("no privilege: cross-repo worker is refused", func(t *testing.T) {
		f := newDirectCheckinFixture(t)
		seedNWRCheckin(t, f)
		f.privilege("prism-test-unrelated")
		if err := authorizeDirectCheckinFor(nwrCLIRoot, nwrCLIOtherWorker); err == nil {
			t.Fatal("admitted — tier 3 needs the caller's repo in the privileged list")
		}
	})
	t.Run("privileged repo: cross-repo worker is in reach", func(t *testing.T) {
		f := newDirectCheckinFixture(t)
		seedNWRCheckin(t, f)
		f.privilege(nwrCLIRoot)
		if err := authorizeDirectCheckinFor(nwrCLIRoot, nwrCLIOtherWorker); err != nil {
			t.Fatalf("refused at tier 3: %v", err)
		}
	})
}

// TestNonWorktreeRoot_StartRecordsNoWorkerRole pins AC3. The root-session
// start path gives the session the role session.DefaultAgent returns for its
// directory, and seeds agent_status through tmux-session-start with that
// role. Neither agent_status.root_agent_name nor sessions.agent_role may say
// "worker", and the worker spawn count (the same sessions join that labels
// prism_spawns_total) must not include the session.
func TestNonWorktreeRoot_StartRecordsNoWorkerRole(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	worktree := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })
	since := time.Now().Add(-time.Minute)

	role := session.DefaultAgent(worktree, "")
	if role != "" {
		t.Fatalf("DefaultAgent(non-worktree dir) = %q, want \"\" — the root-session start path gives no role", role)
	}
	args := []string{"event", "tmux-session-start", "--session", nwrCLIRoot, "--worktree", worktree}
	if role != "" {
		args = append(args, "--agent-role", role)
	}
	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("tmux-session-start: %v", err)
	}

	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	st, err := d.CurrentStatus(nwrCLIRoot)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus: st=%v err=%v", st, err)
	}
	if st.RootAgentName != nil {
		t.Errorf("agent_status.root_agent_name = %q, want NULL", *st.RootAgentName)
	}
	if st.InstanceID == nil {
		t.Fatal("agent_status.instance_id is NULL after tmux-session-start")
	}
	sess, err := d.SessionByInstanceID(*st.InstanceID)
	if err != nil || sess == nil {
		t.Fatalf("SessionByInstanceID: sess=%v err=%v", sess, err)
	}
	if sess.AgentRole != nil && *sess.AgentRole == "worker" {
		t.Errorf("sessions.agent_role = %q, want not \"worker\"", *sess.AgentRole)
	}

	n, err := d.CountRoleEventsSince(context.Background(), session.EventSpawnIntent, "worker", since)
	if err != nil {
		t.Fatalf("CountRoleEventsSince: %v", err)
	}
	if n != 0 {
		t.Errorf("worker spawn count = %d, want 0 — the root session is not a worker spawn", n)
	}

	if !authz.IsNonWorktreeRootFromDB(nwrCLIRoot, d) {
		t.Error("IsNonWorktreeRootFromDB = false after the root start — the direct route does not see the coordinator role")
	}
}
