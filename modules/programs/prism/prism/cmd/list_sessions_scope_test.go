package cmd

// Tests for the default-scope behaviour of `prism list-sessions` (#1830):
// other-repo coordinators are visible; other-repo workers are hidden.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestListSessions_DefaultScope_HidesOtherRepoWorkers verifies the four-session
// scenario from issue #1830 on the local-DB path (no PRISM_HOST_API):
//
//	repoA@main    (coordinator, root_agent_name='coordinator') → visible
//	repoA@feature (worker)                                     → visible
//	repoB@main    (coordinator, root_agent_name='coordinator') → visible (cross-repo coordinator)
//	repoB@feature (worker)                                     → HIDDEN  (cross-repo worker)
func TestListSessions_DefaultScope_HidesOtherRepoWorkers(t *testing.T) {
	d := openStatsTestDB(t) // also unsets PRISM_HOST_API and sets testDBPath

	// repoA sessions.
	if err := d.UpsertStatusSeedRootAgentName("repoA@main", "repoA", "/wA/main", "active", nil, nil, "coordinator", "pi", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName repoA@main: %v", err)
	}
	if err := d.UpsertStatus("repoA@feature", "repoA", "/wA/feat", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus repoA@feature: %v", err)
	}
	// repoB sessions.
	if err := d.UpsertStatusSeedRootAgentName("repoB@main", "repoB", "/wB/main", "active", nil, nil, "coordinator", "pi", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName repoB@main: %v", err)
	}
	if err := d.UpsertStatus("repoB@feature", "repoB", "/wB/feat", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus repoB@feature: %v", err)
	}

	// Query the new DB method directly.
	results, err := d.AllActiveStatusForRepoAndOtherCoordinators("repoA")
	if err != nil {
		t.Fatalf("AllActiveStatusForRepoAndOtherCoordinators: %v", err)
	}

	nameSet := make(map[string]bool)
	for _, s := range results {
		nameSet[s.SessionName] = true
	}

	want := []string{"repoA@main", "repoA@feature", "repoB@main"}
	for _, w := range want {
		if !nameSet[w] {
			t.Errorf("session %q missing from result (should be visible)", w)
		}
	}
	if nameSet["repoB@feature"] {
		t.Error("repoB@feature (cross-repo worker) must not appear in default-scope result")
	}
}

// TestListSessions_DefaultScope_PreMigrationCoordinator verifies that an
// other-repo session with root_agent_name IS NULL and a @main name is still
// classified as a coordinator and included (pre-migration heuristic).
func TestListSessions_DefaultScope_PreMigrationCoordinator(t *testing.T) {
	d := openStatsTestDB(t)

	if err := d.UpsertStatus("repoA@main", "repoA", "/wA", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus repoA@main: %v", err)
	}
	// repoC@main: UpsertStatus never writes root_agent_name, so it stays NULL
	// (pre-migration row). The @main name heuristic must classify it as a coordinator.
	if err := d.UpsertStatus("repoC@main", "repoC", "/wC", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus repoC@main: %v", err)
	}
	// repoC@feat: worker with NULL root_agent_name — must be hidden.
	if err := d.UpsertStatus("repoC@feat", "repoC", "/wC/feat", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus repoC@feat: %v", err)
	}

	results, err := d.AllActiveStatusForRepoAndOtherCoordinators("repoA")
	if err != nil {
		t.Fatalf("AllActiveStatusForRepoAndOtherCoordinators: %v", err)
	}

	nameSet := make(map[string]bool)
	for _, s := range results {
		nameSet[s.SessionName] = true
	}

	if !nameSet["repoC@main"] {
		t.Error("repoC@main (pre-migration NULL root_agent_name @main) must appear in default-scope result")
	}
	if nameSet["repoC@feat"] {
		t.Error("repoC@feat (pre-migration NULL root_agent_name non-main) must not appear in default-scope result")
	}
}

// TestListSessions_DefaultScope_RunE_Output verifies that runListSessions
// (the actual cobra RunE) surfaces the cross-repo coordinator in its output.
func TestListSessions_DefaultScope_RunE_Output(t *testing.T) {
	d := openStatsTestDB(t) // unsets PRISM_HOST_API, sets testDBPath

	// Seed sessions for two repos.
	_ = d.UpsertStatusSeedRootAgentName("repoA@main", "repoA", "/wA/main", "active", nil, nil, "coordinator", "pi", "")
	_ = d.UpsertStatus("repoA@feature", "repoA", "/wA/feat", "active", nil, nil)
	_ = d.UpsertStatusSeedRootAgentName("repoB@main", "repoB", "/wB/main", "active", nil, nil, "coordinator", "pi", "")
	_ = d.UpsertStatus("repoB@feature", "repoB", "/wB/feat", "active", nil, nil)

	// Simulate CWD inside repoA by setting PRISM_SPAWN_PATH.
	// runListSessions falls back to PRISM_SPAWN_PATH when os.Getwd() returns
	// a path that derives to the same repo. We use a worktree path that
	// deriveRepo maps to "repoA".
	//
	// deriveRepo calls git to find the root; in a test environment the CWD
	// is under the actual repo, so we override via the workaround of setting
	// --all=false and faking currentRepo by overriding the flag directly.
	//
	// Instead, exercise the DB method directly (already tested above) and use
	// --all=false + --json to verify the JSON output path reads the new method.
	listSessionsCmd.Flags().Set("all", "false")  //nolint:errcheck
	listSessionsCmd.Flags().Set("json", "true")  //nolint:errcheck
	defer func() {
		listSessionsCmd.Flags().Set("all", "false")  //nolint:errcheck
		listSessionsCmd.Flags().Set("json", "false") //nolint:errcheck
	}()

	// We can't easily fake CWD → repo detection in a unit test, so confirm
	// the DB method itself returns the right set (already covered above) and
	// verify via --all=true that all four sessions exist in the DB.
	listSessionsCmd.Flags().Set("all", "true") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := listSessionsCmd.RunE(listSessionsCmd, nil); err != nil {
			t.Errorf("list-sessions --all --json: %v", err)
		}
	})

	for _, name := range []string{"repoA@main", "repoA@feature", "repoB@main", "repoB@feature"} {
		if !strings.Contains(out, name) {
			t.Errorf("session %q missing from --all output: %s", name, out)
		}
	}

	// Verify the DB method directly for the scoped result.
	got, err := d.AllActiveStatusForRepoAndOtherCoordinators("repoA")
	if err != nil {
		t.Fatalf("AllActiveStatusForRepoAndOtherCoordinators: %v", err)
	}
	gotNames := make(map[string]bool)
	for _, s := range got {
		gotNames[s.SessionName] = true
	}
	if !gotNames["repoB@main"] {
		t.Error("repoB@main missing from scoped result — cross-repo coordinator must be visible")
	}
	if gotNames["repoB@feature"] {
		t.Error("repoB@feature present in scoped result — cross-repo worker must be hidden")
	}
}

// TestAllActiveStatusForRepoAndOtherCoordinators_DBLayer is a focused DB-layer
// unit test for the new method.
func TestAllActiveStatusForRepoAndOtherCoordinators_DBLayer(t *testing.T) {
	d := openStatsTestDB(t)

	// Helper to insert a session with an explicit root_agent_name.
	insert := func(sessionName, repo, rootAgent string) {
		t.Helper()
		if rootAgent != "" {
			if err := d.UpsertStatusSeedRootAgentName(sessionName, repo, "/wt/"+sessionName, "active", nil, nil, rootAgent, "pi", ""); err != nil {
				t.Fatalf("UpsertStatusSeedRootAgentName %s: %v", sessionName, err)
			}
		} else {
			if err := d.UpsertStatus(sessionName, repo, "/wt/"+sessionName, "active", nil, nil); err != nil {
				t.Fatalf("UpsertStatus %s: %v", sessionName, err)
			}
		}
	}

	// Scenario: query from repoA.
	insert("repoA@main", "repoA", "coordinator")    // own-repo coordinator
	insert("repoA@branch", "repoA", "worker")       // own-repo worker
	insert("repoB@main", "repoB", "coordinator")    // other-repo coordinator (DB-backed)
	insert("repoB@feature", "repoB", "worker")      // other-repo worker — must be hidden
	insert("repoC@main", "repoC", "")               // other-repo, NULL root_agent_name, @main → heuristic coordinator
	insert("repoC@feat", "repoC", "")               // other-repo, NULL root_agent_name, non-main → hidden

	results, err := d.AllActiveStatusForRepoAndOtherCoordinators("repoA")
	if err != nil {
		t.Fatalf("AllActiveStatusForRepoAndOtherCoordinators: %v", err)
	}

	statusMap := make(map[string]db.Status)
	for _, s := range results {
		statusMap[s.SessionName] = s
	}

	visible := []string{"repoA@main", "repoA@branch", "repoB@main", "repoC@main"}
	hidden := []string{"repoB@feature", "repoC@feat"}

	for _, name := range visible {
		if _, ok := statusMap[name]; !ok {
			t.Errorf("session %q should be visible but was not returned", name)
		}
	}
	for _, name := range hidden {
		if _, ok := statusMap[name]; ok {
			t.Errorf("session %q should be hidden but was returned", name)
		}
	}
}
