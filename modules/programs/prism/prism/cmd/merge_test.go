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

// TestRunMerge_MainSessionHeuristicRejected verifies that a @main session
// name (coordinator heuristic) that calls prism merge IS allowed (not
// rejected). This tests the inverse: coordinators can enqueue.
// We can't test a full enqueue without a real GitHub API, so we just verify
// that the worker-rejection gate is NOT hit for a @main session, i.e. the
// error (if any) is about missing instance_id, not about being a worker.
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

	// The call will fail — but NOT with "coordinator sessions only".
	// It should fail at the instance_id check (no instance_id set).
	err = runMerge(mergeCmd, []string{"999"})
	if err == nil {
		// Surprising but not impossible in a very unusual test environment.
		t.Log("runMerge returned nil — may have succeeded via gh if PR 999 is open")
		return
	}
	if strings.Contains(err.Error(), "coordinator sessions only") {
		t.Errorf("runMerge error %q mentions 'coordinator sessions only' — coordinator should not be blocked", err.Error())
	}
}
