package cmd

// sidecar_port_test.go — regression tests for the harness_port
// double-allocation race (issue #2357).
//
// Pre-#2357, the sidecar's useTCP startup branch (sandbox-exec pi sessions)
// called db.AllocatePort a second time — after the spawn/restore path had
// already allocated a port and written it to agent_status.harness_port. The
// used-port query did not exclude the session's own row, so the second
// allocation always picked a different port and overwrote the column. `prism
// agent-run` does a one-shot read of harness_port and bakes
// PRISM_HARNESS_PIPE into PI's immutable env; when that read landed before
// the sidecar's overwrite, PI was left dialling a port nobody binds — the
// session ran headless (aws-kubernetes@main incident).
//
// These tests drive the same DB-level sequence the production paths execute
// and assert the invariant: the port agent-run injects equals the port the
// sidecar resolves to bind, regardless of read/allocate ordering.

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

func openSidecarPortTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// readHarnessPort mimics prism agent-run's one-shot read of
// agent_status.harness_port (cmd/agent_run_sandbox_exec_darwin.go): the value
// read here is what gets baked into PI's immutable env as
// PRISM_HARNESS_PIPE=tcp://127.0.0.1:<port>.
func readHarnessPort(t *testing.T, d *db.DB, session string) int {
	t.Helper()
	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil || st.HarnessPort == nil {
		t.Fatalf("no harness_port recorded for %s", session)
	}
	return *st.HarnessPort
}

// TestResolveHarnessPipeTCPPort_SpawnPath_AgentRunReadsBeforeSidecar is the
// core #2357 regression: the spawn path allocates a port, agent-run's
// one-shot read executes BEFORE the sidecar's startup port resolution would
// have run, and the sidecar must still end up binding the exact port
// agent-run read. Pre-fix, resolveHarnessPipeTCPPort's predecessor
// (an unconditional d.AllocatePort) returned a different port and overwrote
// the column, breaking the invariant.
func TestResolveHarnessPipeTCPPort_SpawnPath_AgentRunReadsBeforeSidecar(t *testing.T) {
	d := openSidecarPortTestDB(t)
	const session = "prism-test@2357-spawn"

	// Spawn step 1: seed the agent_status row (SpawnSession step 1).
	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/spawn", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Spawn step 3: allocate the port and write harness_port — this happens
	// synchronously BEFORE the tmux layout / agent pane is created.
	spawnPort, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort (spawn): %v", err)
	}

	// Agent pane starts: agent-run performs its one-shot read and bakes the
	// port into PI's env. This is the racing read — it executes before the
	// sidecar's startup resolution below.
	envPort := readHarnessPort(t, d, session)
	if envPort != spawnPort {
		t.Fatalf("agent-run read %d, want spawn-allocated %d", envPort, spawnPort)
	}

	// Sidecar startup (cmd/sidecar.go useTCP branch): must resolve to the
	// same port agent-run read.
	bindPort, err := resolveHarnessPipeTCPPort(d, session, spawnPort)
	if err != nil {
		t.Fatalf("resolveHarnessPipeTCPPort: %v", err)
	}
	if bindPort != envPort {
		t.Errorf("sidecar would bind %d but PI's env has %d — session would run headless (#2357)", bindPort, envPort)
	}

	// The column must not have been overwritten with a different value after
	// the pane launched (issue #2357 AC).
	if after := readHarnessPort(t, d, session); after != envPort {
		t.Errorf("harness_port overwritten after pane launch: got %d, want %d", after, envPort)
	}
}

// TestResolveHarnessPipeTCPPort_RestorePath_ReacquiresPreviousPort covers the
// restore flow (cmd/restore.go): the session row survives from the previous
// lifecycle with a recorded port, restore re-runs AllocatePort, the pane's
// agent-run reads, then the sidecar resolves. All three must agree — and the
// port must be the previous lifecycle's port (stable across restore).
func TestResolveHarnessPipeTCPPort_RestorePath_ReacquiresPreviousPort(t *testing.T) {
	d := openSidecarPortTestDB(t)
	const session = "prism-test@2357-restore"

	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/restore", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	// Previous lifecycle allocated a port; the process tree then died
	// (reboot / prism restart) without clearing the row.
	prevPort, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort (previous lifecycle): %v", err)
	}

	// Restore path: RefreshWorktree + AllocatePort (cmd/restore.go).
	if err := d.RefreshWorktree(session, "prism-test", "/code/prism-test/restore"); err != nil {
		t.Fatalf("RefreshWorktree: %v", err)
	}
	restorePort, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort (restore): %v", err)
	}
	if restorePort != prevPort {
		t.Errorf("restore drifted the port: got %d, want previous %d", restorePort, prevPort)
	}

	// Agent pane's one-shot read, then the sidecar's resolution.
	envPort := readHarnessPort(t, d, session)
	bindPort, err := resolveHarnessPipeTCPPort(d, session, restorePort)
	if err != nil {
		t.Fatalf("resolveHarnessPipeTCPPort: %v", err)
	}
	if bindPort != envPort {
		t.Errorf("sidecar would bind %d but PI's env has %d (#2357)", bindPort, envPort)
	}
}

// TestResolveHarnessPipeTCPPort_DBValueWinsOverFlag verifies that when the
// --port flag diverges from agent_status.harness_port, the DB value wins:
// agent-run reads the DB, not the flag, so binding the flag value would
// break the env-equals-bind invariant.
func TestResolveHarnessPipeTCPPort_DBValueWinsOverFlag(t *testing.T) {
	d := openSidecarPortTestDB(t)
	const session = "prism-test@2357-diverge"

	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/diverge", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	dbPort, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}

	got, err := resolveHarnessPipeTCPPort(d, session, dbPort+7)
	if err != nil {
		t.Fatalf("resolveHarnessPipeTCPPort: %v", err)
	}
	if got != dbPort {
		t.Errorf("resolve returned %d, want DB value %d (flag was %d)", got, dbPort, dbPort+7)
	}
}

// TestResolveHarnessPipeTCPPort_NoRecordedPort_Allocates verifies the
// fallback: when no port is recorded (cleared row, direct sidecar
// invocation), the sidecar allocates one and persists it so a later
// agent-run read agrees with the listener.
func TestResolveHarnessPipeTCPPort_NoRecordedPort_Allocates(t *testing.T) {
	d := openSidecarPortTestDB(t)
	const session = "prism-test@2357-norecord"

	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/norecord", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	got, err := resolveHarnessPipeTCPPort(d, session, 0)
	if err != nil {
		t.Fatalf("resolveHarnessPipeTCPPort: %v", err)
	}
	if got == 0 {
		t.Fatal("resolve returned port 0")
	}
	if after := readHarnessPort(t, d, session); after != got {
		t.Errorf("harness_port %d does not match resolved port %d", after, got)
	}
}
