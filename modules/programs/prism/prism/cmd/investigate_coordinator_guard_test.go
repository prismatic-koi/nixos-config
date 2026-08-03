package cmd

// Tests for the direct-CLI coordinator guard on `prism investigate`
// (issue #2597).
//
// PR #2596 gated the host-API /investigate endpoint, which covers every
// sandboxed caller (bwrap, sandbox-exec) because those sessions always carry
// PRISM_HOST_API. A session in `host` isolation mode has no host-API socket,
// so runInvestigate skips proxyInvestigate and reaches
// spawnInvestigateSessionWithDB directly. These tests pin the guard on that
// path: refusal for a non-coordinator invoker, admission for a coordinator
// invoker, and admission for the host-side child that the /investigate
// handler itself spawns.
//
// The guard sits at the top of spawnInvestigateSessionWithDB, which is the
// sole chokepoint between runInvestigate's invoker resolution and
// session.SpawnSession. Every test drives that function rather than the
// helper alone, so a guard that is present but unwired fails here.
//
// Test-suite isolation contract (AGENTS.md, issue #1608):
//   - sidecartest.NewIsolated redirects $XDG_STATE_HOME to a t.TempDir() and
//     sets the PRISM_TEST_MODE_RESTRICT_HOSTAPI guard.
//   - Session names use the "prism-test" prefix.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// seedInvestigateInvokerWithRole inserts an active invoker row for the given
// session with an explicit root_agent_name and isolation mode. A nil rootAgent
// leaves root_agent_name NULL — the pre-migration shape that makes
// IsCoordinatorSession fall back to the name heuristic alone.
func seedInvestigateInvokerWithRole(t *testing.T, d *db.DB, sessionName string, rootAgent *string, isolationMode string) {
	t.Helper()
	if err := d.UpsertStatusWithRootAgent(
		sessionName,
		"prism-test",
		"/tmp/test-worktree",
		"active",
		nil, nil,
		rootAgent, nil,
	); err != nil {
		t.Fatalf("seedInvestigateInvokerWithRole(%q): UpsertStatusWithRootAgent: %v", sessionName, err)
	}
	if err := d.SetIsolationMode(sessionName, isolationMode); err != nil {
		t.Fatalf("seedInvestigateInvokerWithRole(%q): SetIsolationMode(%q): %v", sessionName, isolationMode, err)
	}
}

// captureInvestigateSpawn swaps investigateSpawnSessionFn for a spy that
// records whether SpawnSession would have been called, without performing the
// tmux + sidecar side-effects. The returned pointer is non-nil only after the
// spy runs.
func captureInvestigateSpawn(t *testing.T) **session.SpawnOpts {
	t.Helper()
	var captured *session.SpawnOpts
	prev := investigateSpawnSessionFn
	investigateSpawnSessionFn = func(_ *db.DB, opts session.SpawnOpts) error {
		clone := opts
		captured = &clone
		return nil
	}
	t.Cleanup(func() { investigateSpawnSessionFn = prev })
	return &captured
}

// TestSpawnInvestigateSessionWithDB_HostModeWorker_Refused is the headline
// security assertion of #2597: a worker in host isolation mode — the exact
// session shape that carries no PRISM_HOST_API and therefore never meets the
// host-API gate — is refused on the direct path, and no session is spawned.
//
// Before the fix this call succeeded and spawned an investigator.
func TestSpawnInvestigateSessionWithDB_HostModeWorker_Refused(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")

	worker := "prism-test@worker-investigate-guard"
	role := "worker"
	seedInvestigateInvokerWithRole(t, bus.DB, worker, &role, "host")

	captured := captureInvestigateSpawn(t)

	err := spawnInvestigateSessionWithDB(bus.DB, worker, "look into the auth path", "auth-path")
	if err == nil {
		t.Fatal("spawnInvestigateSessionWithDB returned nil for a host-mode worker, want a refusal (issue #2597)")
	}
	if !strings.Contains(err.Error(), "coordinator sessions only") {
		t.Errorf("error = %q, want it to say the command is for coordinator sessions only", err.Error())
	}
	if !strings.Contains(err.Error(), worker) {
		t.Errorf("error = %q, want it to name the refused invoker %q", err.Error(), worker)
	}
	if *captured != nil {
		t.Errorf("investigateSpawnSessionFn was invoked for a refused invoker (SessionName=%q) — the guard must run before SpawnSession", (*captured).SessionName)
	}
}

// TestSpawnInvestigateSessionWithDB_NonCoordinatorRoles_Refused widens the
// refusal to the other non-coordinator roles the issue names: a review agent
// and an investigator. Both are reachable in host mode and both must be
// refused, so an investigator cannot recursively spawn investigators.
func TestSpawnInvestigateSessionWithDB_NonCoordinatorRoles_Refused(t *testing.T) {
	cases := []struct {
		name      string
		session   string
		rootAgent string
	}{
		{
			name:      "review agent",
			session:   "prism-test@main~review-1-review-code",
			rootAgent: "review-code",
		},
		{
			name:      "investigator",
			session:   "prism-test@main~investigate-auth-path",
			rootAgent: "investigate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus := sidecartest.NewIsolated(t, "")
			seedInvestigateInvokerWithRole(t, bus.DB, tc.session, &tc.rootAgent, "host")

			captured := captureInvestigateSpawn(t)

			err := spawnInvestigateSessionWithDB(bus.DB, tc.session, "look into the auth path", "auth-path")
			if err == nil {
				t.Fatalf("spawnInvestigateSessionWithDB(%q) returned nil, want a refusal (issue #2597)", tc.session)
			}
			if !strings.Contains(err.Error(), "coordinator sessions only") {
				t.Errorf("error = %q, want it to say the command is for coordinator sessions only", err.Error())
			}
			if *captured != nil {
				t.Errorf("investigateSpawnSessionFn was invoked for refused invoker %q", tc.session)
			}
		})
	}
}

// TestSpawnInvestigateSessionWithDB_HostModeCoordinator_Admitted is the other
// half of the gate: a coordinator in host isolation mode still spawns an
// investigator. This is what stops the fix from being "deny everyone".
//
// The session name deliberately does NOT end in "@main", so the admission is
// carried by the DB-backed root_agent_name check rather than the name
// heuristic.
func TestSpawnInvestigateSessionWithDB_HostModeCoordinator_Admitted(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")

	coordinator := "prism-test@coordinator-host-mode"
	role := "coordinator"
	seedInvestigateInvokerWithRole(t, bus.DB, coordinator, &role, "host")

	captured := captureInvestigateSpawn(t)

	if err := spawnInvestigateSessionWithDB(bus.DB, coordinator, "look into the auth path", "auth-path"); err != nil {
		t.Fatalf("spawnInvestigateSessionWithDB for a host-mode coordinator: %v", err)
	}
	if *captured == nil {
		t.Fatal("investigateSpawnSessionFn was never invoked — the guard refused a coordinator")
	}
	if want := coordinator + "~investigate-auth-path"; (*captured).SessionName != want {
		t.Errorf("SpawnOpts.SessionName = %q, want %q", (*captured).SessionName, want)
	}
}

// TestSpawnInvestigateSessionWithDB_HostAPIChild_Admitted covers the edge case
// the issue calls out: the host-side child that the /investigate handler
// spawns.
//
// The handler runs `prism investigate` with PRISM_SESSION_NAME set to the
// invoking session and PRISM_HOST_API cleared, so the child takes the direct
// path and meets this guard. Its invoker is the coordinator that
// requireCoordinator already admitted, so the guard must admit it too —
// otherwise the coordinator's route through the host API breaks end to end.
//
// The row here has a NULL root_agent_name on an "@main" session name: the
// pre-migration shape, where IsCoordinatorSession falls through to the name
// heuristic. Admitting it proves the guard does not depend on the DB role
// column being populated for the invoker.
func TestSpawnInvestigateSessionWithDB_HostAPIChild_Admitted(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")

	// The invoking coordinator, exactly as the handler passes it through
	// PRISM_SESSION_NAME.
	coordinator := "prism-test@main"
	seedInvestigateInvokerWithRole(t, bus.DB, coordinator, nil, "bwrap")

	captured := captureInvestigateSpawn(t)

	if err := spawnInvestigateSessionWithDB(bus.DB, coordinator, "look into the auth path", "auth-path"); err != nil {
		t.Fatalf("spawnInvestigateSessionWithDB for the host-API child of a coordinator: %v", err)
	}
	if *captured == nil {
		t.Fatal("investigateSpawnSessionFn was never invoked — the guard refused the host-side child of /investigate, breaking the coordinator's proxy route")
	}
	if want := coordinator + "~investigate-auth-path"; (*captured).SessionName != want {
		t.Errorf("SpawnOpts.SessionName = %q, want %q", (*captured).SessionName, want)
	}
}

// TestRequireInvestigateCoordinator_KeysOnInvokerSession pins the unit-level
// contract of the guard helper: the verdict is a function of the invoker
// session name and the DB, matching cmd/merge.go. A nil DB exercises the
// name-heuristic fallback in isolation.
func TestRequireInvestigateCoordinator_KeysOnInvokerSession(t *testing.T) {
	cases := []struct {
		invoker   string
		wantAdmit bool
	}{
		{"prism-test@main", true},
		{"prism-test@worker-branch", false},
		{"prism-test@main~investigate-auth-path", false},
		{"prism-test@main~review-1-review-code", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.invoker, func(t *testing.T) {
			err := requireInvestigateCoordinator(tc.invoker, nil)
			if tc.wantAdmit && err != nil {
				t.Errorf("requireInvestigateCoordinator(%q, nil) = %v, want nil (admit)", tc.invoker, err)
			}
			if !tc.wantAdmit && err == nil {
				t.Errorf("requireInvestigateCoordinator(%q, nil) = nil, want a refusal", tc.invoker)
			}
		})
	}
}
