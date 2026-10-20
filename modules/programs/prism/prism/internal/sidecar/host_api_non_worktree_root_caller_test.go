package sidecar

// host_api_non_worktree_root_caller_test.go — the caller role of the root
// session of a non-worktree project (issue #3179).
//
// The root-session start path (`prism switch`, `prism restore`) starts a
// non-worktree session such as `obsidian` with no AgentRole, in a directory
// outside every bare+worktree layout. The host API gives that caller the
// coordinator role from its start config (startedAsNonWorktreeRoot). This
// file pins the grant and the refusals that must survive it:
//
//   - The root session gets the coordinator role on every caller-role gate:
//     /prompt, requireCoordinator, /list-sessions?all=true, and /checkin.
//   - A worker with a bare name stays a worker. The role comes from the start
//     config, not from the shape of the name.
//   - A descendant ("~") of the root session stays a worker.
//   - A cross-repo prompt to a session that is not a root session is still
//     refused, and so is a tier-3 checkin without a privileged repo.
//
// # Isolation contract
//
// Every sidecar is built on sidecartest.NewIsolated(t, ""), so
// $XDG_STATE_HOME points at a t.TempDir(), PRISM_TEST_MODE_RESTRICT_HOSTAPI
// is set, and the DB is a private SQLite file. Every session name has the
// "prism-test" prefix. The prism binary is a shell stub, so no handler can
// start a real session.
//
// # Negative-mutation guarantee
//
// Remove `|| s.startedAsNonWorktreeRoot()` from the /prompt gate and
// TestHostAPI_NonWorktreeRoot_PromptsOtherRepoCoordinator fails with 403.
// Make startedAsNonWorktreeRoot ignore Config.AgentRole or Config.Worktree,
// and the bare-named worker cases fail.

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/sessionname"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

const (
	nwrRoot        = "prism-test-nwroot"
	nwrDescendant  = "prism-test-nwroot~investigate-v2"
	nwrOtherRepo   = "prism-test-nwother"
	nwrOtherCoord  = "prism-test-nwother@main"
	nwrOtherWorker = "prism-test-nwother@feature"
)

// nwrWorktreeDir returns a worktree inside a fake bare+worktree layout: a
// project root that holds ".bare", with the worktree as its child.
func nwrWorktreeDir(t *testing.T) string {
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

// nwrSidecar builds an isolated sidecar whose start config is (agentRole,
// worktree). callerRootAgent seeds root_agent_name for the caller row: ""
// leaves the column NULL. The DB also holds a coordinator and a worker in
// another repo.
func nwrSidecar(t *testing.T, sessionName, agentRole, worktree, callerRootAgent, stubBody string, privilegedRepos []string) (*Sidecar, *db.DB) {
	t.Helper()
	bus := sidecartest.NewIsolated(t, "")
	d := bus.DB

	repo := sessionname.Repo(sessionName)
	if err := d.UpsertStatusSeedRootAgentName(sessionName, repo, worktree, "active", nil, nil, callerRootAgent, "", ""); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(nwrOtherCoord, nwrOtherRepo, "/tmp/"+nwrOtherCoord, "active", nil, nil, "coordinator", "", ""); err != nil {
		t.Fatalf("seed other-repo coordinator: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(nwrOtherWorker, nwrOtherRepo, "/tmp/"+nwrOtherWorker, "active", nil, nil, "worker", "", ""); err != nil {
		t.Fatalf("seed other-repo worker: %v", err)
	}

	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	if err := os.WriteFile(stubPath, []byte(stubBody), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return New(Config{
		SessionName:            sessionName,
		Repo:                   repo,
		Worktree:               worktree,
		HarnessURL:             "http://localhost:14000",
		DB:                     d,
		Clock:                  newTestClock(),
		AgentRole:              agentRole,
		PrismBinaryPath:        stubPath,
		CheckinPrivilegedRepos: privilegedRepos,
		Harness:                newSSEHarness(),
		Logger:                 log.New(os.Stderr, "", 0),
	}), d
}

const nwrSuccessStub = "#!/bin/sh\nexit 0\n"

func nwrPrompt(t *testing.T, sc *Sidecar, target string) (int, string) {
	t.Helper()
	rr := doHostAPI(t, sc, http.MethodPost, "/prompt", `{"session":"`+target+`","prompt":"kia ora"}`)
	return rr.Code, rr.Body.String()
}

// TestHostAPI_NonWorktreeRoot_PromptsOtherRepoCoordinator is the reported
// defect: `prism prompt nixos-config@main` from `obsidian` must be delivered.
func TestHostAPI_NonWorktreeRoot_PromptsOtherRepoCoordinator(t *testing.T) {
	sc, _ := nwrSidecar(t, nwrRoot, "", t.TempDir(), "", nwrSuccessStub, nil)

	if code, body := nwrPrompt(t, sc, nwrOtherCoord); code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 — the root session of a non-worktree project must reach another repo's coordinator", code, body)
	}
}

// TestHostAPI_NonWorktreeRoot_StaleDBRoleIgnored pins that the /prompt gate
// reads the start config and not root_agent_name. A legacy row can carry
// "worker" (before #954) or "review-goal" (#2658). The start config of the
// sidecar still says "no role", so the caller is the root session.
func TestHostAPI_NonWorktreeRoot_StaleDBRoleIgnored(t *testing.T) {
	for _, stale := range []string{"worker", "review-goal"} {
		t.Run(stale, func(t *testing.T) {
			sc, _ := nwrSidecar(t, nwrRoot, "", t.TempDir(), stale, nwrSuccessStub, nil)
			if code, body := nwrPrompt(t, sc, nwrOtherCoord); code != http.StatusOK {
				t.Fatalf("status = %d, body = %s, want 200 — a stale root_agent_name must not demote the root session", code, body)
			}
		})
	}
}

// TestHostAPI_NonWorktreeRoot_CrossRepoNonRootRefused pins that the
// coordinator role does not open every cross-repo target. A prompt from the
// root session to another repo's worker is refused, exactly as it is for a
// "<repo>@main" coordinator.
func TestHostAPI_NonWorktreeRoot_CrossRepoNonRootRefused(t *testing.T) {
	sc, _ := nwrSidecar(t, nwrRoot, "", t.TempDir(), "", nwrSuccessStub, nil)

	code, body := nwrPrompt(t, sc, nwrOtherWorker)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403 for a cross-repo worker target", code, body)
	}
	if msg := errorField(t, body); !strings.Contains(msg, "cross-repo") {
		t.Errorf("error %q should be the cross-repo refusal", msg)
	}
}

// nwrWorkerShapes are callers that must keep the worker role. Each one has a
// name that looks like a root session or derives from one.
func nwrWorkerShapes(t *testing.T) []struct {
	name, session, agentRole, worktree string
} {
	return []struct {
		name, session, agentRole, worktree string
	}{
		{"bare-named worker started with the worker role", nwrRoot, "worker", t.TempDir()},
		{"bare-named session started in a worktree", nwrRoot, "", nwrWorktreeDir(t)},
		{"descendant of the root session", nwrDescendant, "", t.TempDir()},
	}
}

// TestHostAPI_BareNamedWorker_PromptStaysWorker is the security half of the
// /prompt gate. The name alone never gives the coordinator branch.
func TestHostAPI_BareNamedWorker_PromptStaysWorker(t *testing.T) {
	for _, tc := range nwrWorkerShapes(t) {
		t.Run(tc.name, func(t *testing.T) {
			sc, _ := nwrSidecar(t, tc.session, tc.agentRole, tc.worktree, tc.agentRole, nwrSuccessStub, nil)

			code, body := nwrPrompt(t, sc, nwrOtherCoord)
			if code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %s, want 403 — this caller must keep the worker role", code, body)
			}
			if msg := errorField(t, body); !strings.Contains(msg, "workers can only prompt their own coordinator") {
				t.Errorf("error %q should be the worker refusal", msg)
			}
		})
	}
}

// nwrCoordinatorOnly is every host-API gate that needs the coordinator role:
// the requireCoordinator surface (as listed in
// TestHostAPI_CoordinatorOnly_DeniesWorker), /list-sessions?all=true, and
// the gates that let a coordinator act on another session. Each request
// below targets another session, so a caller without the role gets 403.
var nwrCoordinatorOnly = []struct {
	method, path, body string
}{
	{http.MethodPost, "/escalate", `{"prompt":"kia ora","from":"prism-test-nwother@feature"}`},
	{http.MethodPost, "/set-model", `{"session":"prism-test-nwother@main","model":"m"}`},
	{http.MethodPost, "/apply-profile", `{"profile":"p","scope":"session","session":"prism-test-nwother@main"}`},
	{http.MethodPost, "/apply-profile", `{"profile":"p","scope":"coordinator"}`},
	{http.MethodPost, "/register-provider", `{"name":"n","scope":"session","session":"prism-test-nwother@main"}`},
	{http.MethodPost, "/register-provider", `{"name":"n","scope":"coordinator"}`},
	{http.MethodPost, "/set-active-tools", `{"session":"prism-test-nwother@main","tools":[]}`},
	{http.MethodPost, "/abort", `{"session":"prism-test-nwother@main"}`},
	{http.MethodPost, "/spawn", `{"branch":"feature-x","prompt":"do the thing"}`},
	{http.MethodPost, "/cleanup", `{"session":"prism-test-nwroot","yes":true}`},
	{http.MethodPost, "/close", `{"session":"prism-test-nwroot"}`},
	{http.MethodPost, "/merge", `{"pr":1}`},
	{http.MethodGet, "/merges", ""},
	{http.MethodPost, "/merges/cancel", `{"pr":1}`},
	{http.MethodPost, "/investigate", `{"prompt":"look into this"}`},
	{http.MethodGet, "/logs?session=prism-test-nwroot", ""},
	{http.MethodGet, "/db/query?sql=SELECT+1", ""},
	{http.MethodGet, "/db/schema", ""},
	{http.MethodGet, "/db/tables", ""},
	{http.MethodGet, "/audit", ""},
	{http.MethodGet, "/list-sessions?all=true", ""},
}

// TestHostAPI_NonWorktreeRoot_CoordinatorOnlyAdmitted pins AC1: the root
// session of a non-worktree project has the same role as a "<repo>@main"
// coordinator on the whole coordinator-only surface. The stub exits 1, so an
// endpoint that shells out returns 500 after the gate. Any status other than
// 403 shows that the gate admitted the caller.
func TestHostAPI_NonWorktreeRoot_CoordinatorOnlyAdmitted(t *testing.T) {
	for _, tc := range nwrCoordinatorOnly {
		t.Run(tc.method+" "+tc.path+" "+tc.body, func(t *testing.T) {
			sc, _ := nwrSidecar(t, nwrRoot, "", t.TempDir(), "", failingStub, nil)
			rr := doHostAPI(t, sc, tc.method, tc.path, tc.body)
			if rr.Code == http.StatusForbidden {
				t.Fatalf("%s %s: status = 403, body = %q — the root session must pass the coordinator gate",
					tc.method, tc.path, truncateForLog(rr.Body.String(), 200))
			}
		})
	}
}

// TestHostAPI_BareNamedWorker_CoordinatorOnlyRefused is the mirror of the test
// above for the callers that must keep the worker role.
func TestHostAPI_BareNamedWorker_CoordinatorOnlyRefused(t *testing.T) {
	for _, shape := range nwrWorkerShapes(t) {
		for _, tc := range nwrCoordinatorOnly {
			t.Run(shape.name+"/"+tc.method+" "+tc.path+" "+tc.body, func(t *testing.T) {
				sc, _ := nwrSidecar(t, shape.session, shape.agentRole, shape.worktree, shape.agentRole, failingStub, nil)
				rr := doHostAPI(t, sc, tc.method, tc.path, tc.body)
				if rr.Code != http.StatusForbidden {
					t.Fatalf("%s %s: status = %d, body = %q, want 403",
						tc.method, tc.path, rr.Code, truncateForLog(rr.Body.String(), 200))
				}
			})
		}
	}
}

// TestHostAPI_NonWorktreeRoot_CheckinTiers pins AC8. The root session reads at
// tier 2: another repo's coordinator is in reach, another repo's worker is
// not. Tier 3 (any session in any repo) needs its repo in the privileged
// list.
func TestHostAPI_NonWorktreeRoot_CheckinTiers(t *testing.T) {
	t.Run("tier 2: cross-repo coordinator is in reach", func(t *testing.T) {
		sc, _ := nwrSidecar(t, nwrRoot, "", t.TempDir(), "", failingStub, nil)
		if code, body := checkin(t, sc, nwrOtherCoord); code != http.StatusOK {
			t.Fatalf("status = %d, body = %s, want 200", code, body)
		}
	})
	t.Run("no privilege: cross-repo worker is refused", func(t *testing.T) {
		sc, _ := nwrSidecar(t, nwrRoot, "", t.TempDir(), "", failingStub, []string{"prism-test-unrelated"})
		code, body := checkin(t, sc, nwrOtherWorker)
		if code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want 403 — tier 3 needs the caller's repo in the privileged list", code, body)
		}
	})
	t.Run("privileged repo: cross-repo worker is in reach", func(t *testing.T) {
		sc, _ := nwrSidecar(t, nwrRoot, "", t.TempDir(), "", failingStub, []string{nwrRoot})
		if code, body := checkin(t, sc, nwrOtherWorker); code != http.StatusOK {
			t.Fatalf("status = %d, body = %s, want 200 at tier 3", code, body)
		}
	})
	t.Run("bare-named worker: cross-repo coordinator is refused", func(t *testing.T) {
		sc, _ := nwrSidecar(t, nwrRoot, "worker", t.TempDir(), "worker", failingStub, []string{nwrRoot})
		code, body := checkin(t, sc, nwrOtherCoord)
		if code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want 403 — a worker reads only its own review agents", code, body)
		}
	})
}

// TestHostAPI_WorktreeWorker_PromptNonCoordinatorRefused pins AC6: a worker in
// a worktree repo still gets 403 when it prompts a session other than its own
// coordinator.
func TestHostAPI_WorktreeWorker_PromptNonCoordinatorRefused(t *testing.T) {
	const (
		worker  = "prism-test-nwother@feature"
		sibling = "prism-test-nwother@sibling"
	)
	sc, d := nwrSidecar(t, worker, "worker", nwrWorktreeDir(t), "worker", nwrSuccessStub, nil)
	if err := d.UpsertStatusSeedRootAgentName(sibling, nwrOtherRepo, "/tmp/"+sibling, "active", nil, nil, "worker", "", ""); err != nil {
		t.Fatalf("seed sibling: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(nwrRoot, nwrRoot, t.TempDir(), "active", nil, nil, "", "", ""); err != nil {
		t.Fatalf("seed non-worktree root: %v", err)
	}

	for _, target := range []string{sibling, nwrRoot} {
		if code, body := nwrPrompt(t, sc, target); code != http.StatusForbidden {
			t.Errorf("prompt %q: status = %d, body = %s, want 403", target, code, body)
		}
	}
	if code, body := nwrPrompt(t, sc, nwrOtherCoord); code != http.StatusOK {
		t.Errorf("prompt own coordinator: status = %d, body = %s, want 200", code, body)
	}
}

// nwrWatcherStarted runs a socket-pipe sidecar with the given start config
// and reports whether Run started the merge-queue watcher. The watcher starts
// before the pipe socket binds, so the socket file is the signal to read.
func nwrWatcherStarted(t *testing.T, sessionName, agentRole, worktree string) bool {
	t.Helper()
	sockPath := shortSockPath(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	sc := New(Config{
		SessionName:           sessionName,
		Repo:                  sessionname.Repo(sessionName),
		Worktree:              worktree,
		DB:                    openTestDB(t),
		Clock:                 newTestClock(),
		AgentRole:             agentRole,
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		Harness:               pih.New("", agentRole, ""),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wait := runSidecarRun(ctx, sc)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pipe socket never appeared — Run did not reach runStartupSocketPipe")
		}
		time.Sleep(20 * time.Millisecond)
	}
	sc.mu.Lock()
	started := sc.mergeWatcherCancel != nil
	sc.mu.Unlock()

	conn, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("Run() returned error: %v", err)
	}
	return started
}

// TestSidecar_NonWorktreeRoot_StartsMergeWatcher pins the watcher arm.
// requireCoordinator admits the root session to `prism merge`, so the root
// session must drive its own queue. A bare-named worker and a descendant must
// not start a watcher.
func TestSidecar_NonWorktreeRoot_StartsMergeWatcher(t *testing.T) {
	cases := []struct {
		name, session, agentRole string
		want                     bool
	}{
		{"root session of a non-worktree project", nwrRoot, "", true},
		{"bare-named worker", nwrRoot, "worker", false},
		{"descendant of the root session", nwrDescendant, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nwrWatcherStarted(t, tc.session, tc.agentRole, t.TempDir()); got != tc.want {
				t.Errorf("merge-queue watcher started = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolveRoleForSession_NonWorktreeRootTakesCoordinatorSlot pins the
// /apply-profile fan-out. A target with no recorded role gets the worker
// slot, except the root session of a non-worktree project, which gets the
// coordinator slot that `prism agent-run` gives it at start. A recorded role
// always wins.
func TestResolveRoleForSession_NonWorktreeRootTakesCoordinatorSlot(t *testing.T) {
	sc, d := nwrSidecar(t, nwrOtherCoord, "coordinator", nwrWorktreeDir(t), "coordinator", nwrSuccessStub, nil)

	cases := []struct {
		session, role, worktree, want string
	}{
		{nwrRoot, "", t.TempDir(), "coordinator"},
		{nwrDescendant, "", t.TempDir(), "worker"},
		{"prism-test-nwother@nullrole", "", nwrWorktreeDir(t), "worker"},
		{"prism-test-nwother@reviewer", "review-goal", nwrWorktreeDir(t), "review-goal"},
	}
	for _, tc := range cases {
		if err := d.UpsertStatusSeedRootAgentName(tc.session, sessionname.Repo(tc.session), tc.worktree, "active", nil, nil, tc.role, "", ""); err != nil {
			t.Fatalf("seed %q: %v", tc.session, err)
		}
		got, skip := resolveRoleForSession(sc, tc.session)
		if skip != "" {
			t.Fatalf("resolveRoleForSession(%q) skip = %q", tc.session, skip)
		}
		if got != tc.want {
			t.Errorf("resolveRoleForSession(%q) = %q, want %q", tc.session, got, tc.want)
		}
	}
}
