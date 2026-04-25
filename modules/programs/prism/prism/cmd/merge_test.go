package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

// openMergeTestDB opens an isolated test DB and registers cleanup.
// Sets testDBPath so that openDB() in cmd package uses the test DB.
func openMergeTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	SetTestDBPath(filepath.Join(dir, "merge_test.db"))
	t.Cleanup(func() { SetTestDBPath("") })
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
	if err := d.UpsertStatusSeedRootAgentName(workerSession, "nixos-config", "/worktree/feature", "idle", nil, nil, "worker"); err != nil {
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
	if err := d.UpsertStatusSeedRootAgentName(coordSession, "nixos-config", "/worktree/main", "idle", nil, nil, "coordinator"); err != nil {
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

// TestRunMerge_MintsInstanceIDWhenMissing verifies that when a coordinator
// session has no pre-existing instance_id in the DB, runMerge mints one and
// writes it to both agent_status and the sessions table. The call proceeds
// past the instance_id check and fails at the gh preflight (expected in a
// test environment with no real GitHub API), not at the instance_id guard.
//
// This is the fix for issue #1031: prism merge failed with
// "cannot determine instance_id" for @main coordinator sessions opened
// without going through prism switch.
func TestRunMerge_MintsInstanceIDWhenMissing(t *testing.T) {
	openMergeTestDB(t)

	// Seed a coordinator session WITHOUT an instance_id (simulates a
	// @main session opened outside of prism switch → ensureAndSwitch).
	const coordSession = "nixos-config@main"
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(coordSession, "nixos-config", "/worktree/main", "idle", nil, nil, "coordinator"); err != nil {
		d.Close()
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	// Confirm no instance_id is set.
	status, err := d.CurrentStatus(coordSession)
	if err != nil {
		d.Close()
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		d.Close()
		t.Fatal("CurrentStatus: got nil, want status row")
	}
	if status.InstanceID != nil {
		d.Close()
		t.Fatalf("precondition: expected nil instance_id, got %q", *status.InstanceID)
	}
	d.Close()

	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	// runMerge should fail at the gh preflight, not at the instance_id guard.
	err = runMerge(mergeCmd, []string{"999"})
	if err != nil {
		// Acceptable: gh preflight will fail in CI/test environments.
		if strings.Contains(err.Error(), "cannot determine instance_id") {
			t.Fatalf("runMerge still fails with old 'cannot determine instance_id' error — fix did not take effect: %v", err)
		}
		if strings.Contains(err.Error(), "cannot determine calling session") {
			t.Fatalf("runMerge returned 'cannot determine calling session' — should only happen when callerSession is empty, not here: %v", err)
		}
		if strings.Contains(err.Error(), "register session") || strings.Contains(err.Error(), "set instance_id") {
			t.Fatalf("runMerge failed at DB registration step: %v", err)
		}
		// Any other error (e.g. gh not found, PR 999 not open) is expected.
		t.Logf("runMerge failed at preflight (expected in test env): %v", err)
	}

	// Verify the instance_id was minted and persisted to agent_status.
	d2, err := openDB()
	if err != nil {
		t.Fatalf("openDB for verify: %v", err)
	}
	defer d2.Close()

	status2, err := d2.CurrentStatus(coordSession)
	if err != nil {
		t.Fatalf("CurrentStatus after runMerge: %v", err)
	}
	if status2 == nil {
		t.Fatal("CurrentStatus after runMerge: got nil, want status row")
	}
	if status2.InstanceID == nil || *status2.InstanceID == "" {
		t.Fatal("instance_id was not minted and written to agent_status")
	}

	// Verify a sessions row was inserted for the minted instance_id.
	sess, err := d2.SessionByInstanceID(*status2.InstanceID)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if sess == nil {
		t.Errorf("no sessions row found for minted instance_id %q", *status2.InstanceID)
	}
}
