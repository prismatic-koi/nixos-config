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
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestNonWorktreeRoot_StartRecordsNoWorkerRole pins AC3 for `prism switch`.
// The root-session start path gives the session the role that
// session.DefaultAgent returns for its directory, and seeds agent_status
// through tmux-session-start with that role. Neither
// agent_status.root_agent_name nor sessions.agent_role may say "worker".
// prism_spawns_total and prism_sessions_ended_total both take agent_role from
// that sessions row (exporter.LifecycleEventsTailSQL).
func TestNonWorktreeRoot_StartRecordsNoWorkerRole(t *testing.T) {
	// Cobra keeps a flag value between Execute calls. Without the reset, a
	// test that ran earlier with --agent-role worker seeds that role here.
	resetRootCmdFlags(t)
	t.Setenv("PRISM_HOST_API", "")
	worktree := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	if role := session.DefaultAgent(worktree, ""); role != "" {
		t.Fatalf("DefaultAgent(non-worktree dir) = %q, want \"\" — the root-session start path gives no role", role)
	}
	rootCmd.SetArgs([]string{"event", "tmux-session-start", "--session", nwrCLIRoot, "--worktree", worktree})
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
	assertNoSessionRole(t, d, *st.InstanceID)

	if !authz.IsNonWorktreeRootFromDB(nwrCLIRoot, d) {
		t.Error("IsNonWorktreeRootFromDB = false after the root start — the direct route does not see the coordinator role")
	}
}

// assertNoSessionRole fails unless the sessions row for instanceID has NULL
// agent_role and NULL root_agent_name.
func assertNoSessionRole(t *testing.T, d *db.DB, instanceID string) {
	t.Helper()
	sess, err := d.SessionByInstanceID(instanceID)
	if err != nil || sess == nil {
		t.Fatalf("SessionByInstanceID(%q): sess=%v err=%v", instanceID, sess, err)
	}
	if sess.AgentRole != nil {
		t.Errorf("sessions.agent_role = %q, want NULL", *sess.AgentRole)
	}
	if sess.RootAgentName != nil {
		t.Errorf("sessions.root_agent_name = %q, want NULL", *sess.RootAgentName)
	}
}

// TestNonWorktreeRoot_RestoreRecordsNoStaleRole pins AC3 for `prism restore`.
//
// A legacy root row holds root_agent_name = "worker" (written before #954),
// and its sessions row holds the same value. restoreProjectSession first
// takes the role from session.DefaultAgentForSession, then writes the
// sessions row with restoredInstanceID, then calls session.Create. Create
// runs session.ClearStaleNonWorktreeRootRole with opts.InstanceID. This test
// runs the same steps in the same order, on both restore paths:
//
//   - reopen: the old sidecar stopped, so restore reopens the sessions row
//     that agent_status names. ReopenSession keeps agent_role and
//     root_agent_name.
//   - new incarnation: the old sidecar did not stop, so restore inserts a new
//     row with restoredSessionRow, which copies prev.AgentRole and
//     s.RootAgentName.
//
// If the clear stops reaching the sessions row, prism_sessions_ended_total
// labels the restored incarnation agent_role="worker".
func TestNonWorktreeRoot_RestoreRecordsNoStaleRole(t *testing.T) {
	for _, tc := range []struct {
		name              string
		oldSidecarStopped bool
	}{
		{"reopen path", true},
		{"new incarnation path", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(filepath.Join(t.TempDir(), "prism.db"))
			if err != nil {
				t.Fatalf("db.Open: %v", err)
			}
			defer d.Close()
			worktree := t.TempDir()
			const prevInstance = "inst-legacy"

			if err := d.UpsertStatusSeedRootAgentName(nwrCLIRoot, nwrCLIRoot, worktree, "idle", nil, nil, "worker", "", "host"); err != nil {
				t.Fatalf("seed agent_status: %v", err)
			}
			if err := d.SetInstanceID(nwrCLIRoot, prevInstance); err != nil {
				t.Fatalf("SetInstanceID: %v", err)
			}
			stale := "worker"
			if err := d.InsertSession(db.Session{
				InstanceID: prevInstance, SessionName: nwrCLIRoot, AgentRole: &stale, RootAgentName: &stale,
				Repo: nwrCLIRoot, Worktree: worktree, Harness: "pi",
			}); err != nil {
				t.Fatalf("InsertSession: %v", err)
			}
			if err := d.UpdateSessionEnded(prevInstance, "finished"); err != nil {
				t.Fatalf("UpdateSessionEnded: %v", err)
			}

			st, err := d.CurrentStatus(nwrCLIRoot)
			if err != nil || st == nil {
				t.Fatalf("CurrentStatus: st=%v err=%v", st, err)
			}
			agent := session.DefaultAgentForSession(nwrCLIRoot, worktree, "", d)
			if agent != "" {
				t.Fatalf("restore role = %q, want \"\" — a stale role must not become the start role", agent)
			}
			instanceID := restoredInstanceID(d, *st, agent, tc.oldSidecarStopped)
			if instanceID == "" {
				t.Fatal("restoredInstanceID returned no instance ID")
			}
			if err := session.ClearStaleNonWorktreeRootRole(d, nwrCLIRoot, worktree, agent, instanceID); err != nil {
				t.Fatalf("ClearStaleNonWorktreeRootRole: %v", err)
			}

			assertNoSessionRole(t, d, instanceID)
			if name, _, _ := d.RootAgentName(nwrCLIRoot); name != "" {
				t.Errorf("agent_status.root_agent_name = %q, want NULL", name)
			}
			if !authz.IsNonWorktreeRootFromDB(nwrCLIRoot, d) {
				t.Error("IsNonWorktreeRootFromDB = false after restore — the direct route does not see the coordinator role")
			}
		})
	}
}

// nwrCLICallerShapes are the two callers that the inline guards in
// cmd/merge.go and cmd/profile.go must tell apart. Both have the same bare
// name and a non-worktree worktree. Only the recorded role differs.
var nwrCLICallerShapes = []struct {
	name      string
	role      string // root_agent_name; "" = NULL
	wantGuard bool   // true = the coordinator-only refusal fires
}{
	{"root session of a non-worktree project", "", false},
	{"bare-named worker", "worker", true},
}

// TestRunMerge_NonWorktreeRootPassesCoordinatorGuard drives the inline guard in
// runMerge. The row has no instance_id, so a caller that passes the guard
// stops at the instance_id check, before any gh call. If runMerge calls
// session.IsCoordinatorSession alone, the root session gets the
// coordinator-only refusal.
func TestRunMerge_NonWorktreeRootPassesCoordinatorGuard(t *testing.T) {
	for _, tc := range nwrCLICallerShapes {
		t.Run(tc.name, func(t *testing.T) {
			openMergeTestDB(t)
			d, err := openDB()
			if err != nil {
				t.Fatalf("openDB: %v", err)
			}
			seedErr := d.UpsertStatusSeedRootAgentName(nwrCLIRoot, nwrCLIRoot, t.TempDir(), "idle", nil, nil, tc.role, "", "host")
			d.Close()
			if seedErr != nil {
				t.Fatalf("seed: %v", seedErr)
			}
			t.Setenv("PRISM_SESSION_NAME", nwrCLIRoot)
			t.Setenv("TMUX", "")

			err = runMerge(mergeCmd, []string{"999"})
			if err == nil {
				t.Fatal("runMerge returned nil, want an error (the guard, or the missing instance_id)")
			}
			if got := strings.Contains(err.Error(), "coordinator sessions only"); got != tc.wantGuard {
				t.Fatalf("coordinator-only refusal = %v, want %v (error: %v)", got, tc.wantGuard, err)
			}
			if !tc.wantGuard && !strings.Contains(err.Error(), "no instance_id") {
				t.Errorf("error %q, want the missing instance_id error after the guard", err)
			}
		})
	}
}

// TestProfileUse_NonWorktreeRootPassesCoordinatorScopeGuard drives the inline
// guard in runProfileUse for --scope coordinator. A caller that passes the
// guard fails later, when it dials a sidecar socket that does not exist in the
// temp state dir. Only the error text of the guard matters here.
func TestProfileUse_NonWorktreeRootPassesCoordinatorScopeGuard(t *testing.T) {
	for _, tc := range nwrCLICallerShapes {
		t.Run(tc.name, func(t *testing.T) {
			withProfileFixture(t, sampleProfilesJSON)
			d := openProfileTestDB(t)
			if err := d.UpsertStatusSeedRootAgentName(nwrCLIRoot, nwrCLIRoot, t.TempDir(), "active", nil, nil, tc.role, "", "host"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			t.Setenv("PRISM_SESSION_NAME", nwrCLIRoot)

			_, err := runProfileUseWithScope(t, "coordinator", true, []string{"anthropic"})
			got := err != nil && strings.Contains(err.Error(), "coordinator sessions only")
			if got != tc.wantGuard {
				t.Fatalf("coordinator-only refusal = %v, want %v (error: %v)", got, tc.wantGuard, err)
			}
		})
	}
}
