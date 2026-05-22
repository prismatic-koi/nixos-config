package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

// openMergeTestDB opens an isolated test DB and registers cleanup.
// Sets testDBPath so that openDB() in cmd package uses the test DB.
//
// It also unsets PRISM_HOST_API for the duration of the test so that
// runMerge / runMergesList / runMergesCancel exercise the host-side DB path
// rather than attempting to proxy through a host-API socket that does not
// exist in the test environment (#1043).
func openMergeTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	SetTestDBPath(filepath.Join(dir, "merge_test.db"))
	t.Cleanup(func() { SetTestDBPath("") })
	t.Setenv("PRISM_HOST_API", "")
}

// ── runMerge coordinator-only guard ───────────────────────────────────────────

// TestRunMerge_WorkerSessionIsRejected verifies that a worker session calling
// prism merge receives an error and no row is inserted. This is the security
// AC: "Worker agents are not permitted to invoke prism merge."
func TestRunMerge_WorkerSessionIsRejected(t *testing.T) {
	openMergeTestDB(t)

	// Seed a worker session.
	const workerSession = "nixos-config@feature"
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d.Close()
	if err := d.UpsertStatusSeedRootAgentName(workerSession, "nixos-config", "/worktree/feature", "idle", nil, nil, "worker", "", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	d.Close()

	t.Setenv("PRISM_SESSION_NAME", workerSession)
	t.Setenv("TMUX", "")

	// Call runMerge directly. It should return an error before inserting any row.
	err = runMerge(mergeCmd, []string{"42"})
	if err == nil {
		t.Fatal("runMerge: expected error for worker session, got nil")
	}
	if !strings.Contains(err.Error(), "coordinator sessions only") {
		t.Errorf("runMerge error %q does not mention 'coordinator sessions only'", err.Error())
	}

	// Confirm no row was inserted.
	d2, err2 := openDB()
	if err2 != nil {
		t.Fatalf("openDB for verify: %v", err2)
	}
	defer d2.Close()
	row, rowErr := d2.PendingMergeByPR(42)
	if rowErr != nil {
		t.Fatalf("PendingMergeByPR: %v", rowErr)
	}
	if row != nil {
		t.Errorf("PendingMergeByPR(42): got row with status=%q, want nil (no row should be inserted for worker)", row.Status)
	}
}

// TestRunMerge_CoordinatorSessionNotRejectedByWorkerGuard verifies that a
// @main session (coordinator heuristic) is allowed past the worker-rejection
// gate. With the mint-on-the-fly fix, the call no longer fails at the
// instance_id check — it mints one and proceeds to the gh preflight, which
// fails in a test environment (no real GitHub API). The only assertion here is
// that the error is NOT about "coordinator sessions only".
func TestRunMerge_CoordinatorSessionNotRejectedByWorkerGuard(t *testing.T) {
	openMergeTestDB(t)

	const coordSession = "nixos-config@main"
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(coordSession, "nixos-config", "/worktree/main", "idle", nil, nil, "coordinator", "", ""); err != nil {
		d.Close()
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	d.Close()

	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	// The call will fail at the gh preflight (no real GitHub API in tests),
	// but NOT with "coordinator sessions only" or "cannot determine instance_id".
	err = runMerge(mergeCmd, []string{"999"})
	if err == nil {
		// Surprising but not impossible in a very unusual test environment.
		t.Log("runMerge returned nil — may have succeeded via gh if PR 999 is open")
		return
	}
	if strings.Contains(err.Error(), "coordinator sessions only") {
		t.Errorf("runMerge error %q mentions 'coordinator sessions only' — coordinator should not be blocked", err.Error())
	}
	if strings.Contains(err.Error(), "cannot determine instance_id") {
		t.Errorf("runMerge error %q mentions 'cannot determine instance_id' — should have been minted on the fly", err.Error())
	}
}

// ── mint-on-the-fly instance_id ───────────────────────────────────────────────

// TestRunMerge_FailsWhenInstanceIDMissing verifies that when a coordinator
// session has no instance_id in the DB, runMerge returns a clear error
// indicating the sidecar did not start correctly. The sidecar is now the
// sole owner of instance_id minting (issue #1252); on-the-fly recovery in
// runMerge has been removed.
func TestRunMerge_FailsWhenInstanceIDMissing(t *testing.T) {
	openMergeTestDB(t)

	// Seed a coordinator session WITHOUT an instance_id (simulates a
	// @main session whose sidecar did not run correctly).
	const coordSession = "nixos-config@main"
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(coordSession, "nixos-config", "/worktree/main", "idle", nil, nil, "coordinator", "", ""); err != nil {
		d.Close()
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	d.Close()

	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	// runMerge should fail with a clear error about missing instance_id,
	// not attempt on-the-fly minting.
	err = runMerge(mergeCmd, []string{"999"})
	if err == nil {
		t.Fatal("expected runMerge to return an error when instance_id is missing, got nil")
	}
	if !strings.Contains(err.Error(), "no instance_id") && !strings.Contains(err.Error(), "sidecar did not start") {
		t.Fatalf("expected error about missing instance_id / sidecar not starting, got: %v", err)
	}
}
