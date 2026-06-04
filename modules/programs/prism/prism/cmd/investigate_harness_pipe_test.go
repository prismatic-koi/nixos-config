package cmd

// Regression tests for issue #2111 — `prism investigate` must pre-compute
// SpawnOpts.HarnessPipeSockPath for host-mode pi invokers, mirroring the
// same gate that lives in cmd/spawn.go, cmd/switch.go, and cmd/restore.go.
//
// For host-mode invokers, the PI extension only learns where to reach the
// sidecar via PRISM_HARNESS_PIPE in the tmux pane env. That env var is only
// emitted by agentPaneEnvVars when opts.HarnessPipeSockPath != "". Before
// the fix, cmd/investigate.go::spawnInvestigateSession built SpawnOpts with
// HarnessPipeSockPath left empty unconditionally, which made the PI extension
// no-op out, the `--agent` flag never register, and pi reject
// `--agent investigate --prompt "..."` as `Unknown options`.
//
// The two tests below intercept the SpawnSession call via the
// investigateSpawnSessionFn package var and assert on the captured SpawnOpts:
//
//   - TestSpawnInvestigateSession_HostMode_PopulatesHarnessPipeSockPath
//     proves the gate fires for host-mode invokers.
//   - TestSpawnInvestigateSession_BwrapMode_LeavesHarnessPipeSockPathEmpty
//     proves the gate does real work — for container-mode invokers
//     HarnessPipeSockPath stays empty, so the existing container-mode
//     injection paths (bwrap --setenv, sandbox-exec profile, podman --env)
//     remain responsible for PRISM_HARNESS_PIPE.
//
// Test-suite isolation contract (AGENTS.md, issue #1608):
//   - sidecartest.NewIsolated redirects $XDG_STATE_HOME to a t.TempDir() and
//     sets the PRISM_TEST_MODE_RESTRICT_HOSTAPI guard.
//   - Session names use the "prism-test@" prefix.

import (
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// seedInvestigateInvoker inserts an active invoker row for the given session
// and explicitly stamps the requested isolation_mode. The invoker row must
// exist before spawnInvestigateSessionWithDB can call CurrentStatus.
func seedInvestigateInvoker(t *testing.T, d *db.DB, sessionName, isolationMode string) {
	t.Helper()
	agentName := "coordinator"
	if err := d.UpsertStatusWithRootAgent(
		sessionName,
		"prism-test",
		"/tmp/test-worktree",
		"active",
		nil, nil,
		&agentName, nil,
	); err != nil {
		t.Fatalf("seedInvestigateInvoker: UpsertStatusWithRootAgent: %v", err)
	}
	if err := d.SetIsolationMode(sessionName, isolationMode); err != nil {
		t.Fatalf("seedInvestigateInvoker: SetIsolationMode(%q): %v", isolationMode, err)
	}
}

// TestSpawnInvestigateSession_HostMode_PopulatesHarnessPipeSockPath verifies
// AC: when the invoker's resolved isolation mode is "host" and the harness
// is "pi" (socket-pipe transport), spawnOpts.HarnessPipeSockPath equals
// session.SidecarHarnessPipePath(<new-session-name>).
//
// This is the failure case in #2111: before the fix HarnessPipeSockPath was
// left empty for host-mode invokers, the PI extension no-op'd, and pi
// rejected --agent/--prompt as unknown flags.
func TestSpawnInvestigateSession_HostMode_PopulatesHarnessPipeSockPath(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")

	invoker := "prism-test@coord-" + t.Name()
	seedInvestigateInvoker(t, bus.DB, invoker, "host")

	var captured *session.SpawnOpts
	prev := investigateSpawnSessionFn
	investigateSpawnSessionFn = func(_ *db.DB, opts session.SpawnOpts) error {
		clone := opts
		captured = &clone
		return nil
	}
	t.Cleanup(func() { investigateSpawnSessionFn = prev })

	const promptText = "trace the call chain for SSH auth"
	const suppliedName = "ssh-auth-trace"
	if err := spawnInvestigateSessionWithDB(bus.DB, invoker, promptText, suppliedName); err != nil {
		t.Fatalf("spawnInvestigateSessionWithDB: %v", err)
	}
	if captured == nil {
		t.Fatal("investigateSpawnSessionFn was never invoked — SpawnOpts not captured")
	}

	wantSessionName := invoker + "~investigate-" + suppliedName
	if captured.SessionName != wantSessionName {
		t.Fatalf("SpawnOpts.SessionName = %q, want %q", captured.SessionName, wantSessionName)
	}

	// Sanity: investigate hard-codes the pi harness.
	if captured.HarnessName != "pi" {
		t.Errorf("SpawnOpts.HarnessName = %q, want %q", captured.HarnessName, "pi")
	}
	// Sanity: the invoker's host isolation mode propagated into spawnOpts.
	if captured.IsolationMode != "host" {
		t.Errorf("SpawnOpts.IsolationMode = %q, want %q", captured.IsolationMode, "host")
	}

	wantPath, err := session.SidecarHarnessPipePath(wantSessionName)
	if err != nil {
		t.Fatalf("session.SidecarHarnessPipePath(%q): %v", wantSessionName, err)
	}
	if captured.HarnessPipeSockPath != wantPath {
		t.Errorf("SpawnOpts.HarnessPipeSockPath = %q, want %q (issue #2111: host-mode pi invoker must populate the harness pipe sock path so agentPaneEnvVars emits PRISM_HARNESS_PIPE)",
			captured.HarnessPipeSockPath, wantPath)
	}
}

// TestSpawnInvestigateSession_BwrapMode_LeavesHarnessPipeSockPathEmpty
// verifies the gate does real work: when the invoker's resolved isolation
// mode is not "host" (here bwrap), SpawnOpts.HarnessPipeSockPath stays empty
// so the existing container-mode injection paths (bwrap --setenv, sandbox-exec
// profile, podman --env) remain responsible for PRISM_HARNESS_PIPE.
//
// Without this assertion a buggy fix that unconditionally set
// HarnessPipeSockPath for all isolation modes would pass the host-mode test
// — this is the negative half of the gate-correctness proof.
func TestSpawnInvestigateSession_BwrapMode_LeavesHarnessPipeSockPathEmpty(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")

	invoker := "prism-test@coord-" + t.Name()
	seedInvestigateInvoker(t, bus.DB, invoker, "bwrap")

	var captured *session.SpawnOpts
	prev := investigateSpawnSessionFn
	investigateSpawnSessionFn = func(_ *db.DB, opts session.SpawnOpts) error {
		clone := opts
		captured = &clone
		return nil
	}
	t.Cleanup(func() { investigateSpawnSessionFn = prev })

	const promptText = "trace the call chain for SSH auth"
	const suppliedName = "ssh-auth-trace"
	if err := spawnInvestigateSessionWithDB(bus.DB, invoker, promptText, suppliedName); err != nil {
		t.Fatalf("spawnInvestigateSessionWithDB: %v", err)
	}
	if captured == nil {
		t.Fatal("investigateSpawnSessionFn was never invoked — SpawnOpts not captured")
	}

	if captured.IsolationMode != "bwrap" {
		t.Errorf("SpawnOpts.IsolationMode = %q, want %q", captured.IsolationMode, "bwrap")
	}

	if captured.HarnessPipeSockPath != "" {
		t.Errorf("SpawnOpts.HarnessPipeSockPath = %q, want \"\" for bwrap invokers — container-mode injection paths (bwrap --setenv, sandbox-exec profile, podman --env) own PRISM_HARNESS_PIPE for container sessions; pre-computing the sock path here would double-inject (issue #2111 AC).",
			captured.HarnessPipeSockPath)
	}
}
