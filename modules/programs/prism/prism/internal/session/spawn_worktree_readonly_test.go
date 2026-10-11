package session

import "testing"

// TestSpawnSession_WritesWorktreeReadOnly: SpawnSession records
// opts.WorktreeReadOnly in agent_status before the agent starts, and a
// re-spawn of the same session name overwrites it (#3079).
func TestSpawnSession_WritesWorktreeReadOnly(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch~investigate-readonly"
	spawn := func(readOnly bool) {
		t.Helper()
		if err := SpawnSession(d, SpawnOpts{
			SessionName:      sessionName,
			Repo:             "myrepo",
			Worktree:         "/worktrees/myrepo-branch",
			AgentRole:        "investigate",
			Prompt:           "research this",
			Layout:           LayoutAgentOnly,
			PIExtensionDir:   testPIExtensionDir,
			WorktreeReadOnly: readOnly,
		}); err != nil {
			t.Fatalf("SpawnSession(WorktreeReadOnly=%v): %v", readOnly, err)
		}
		got, err := d.WorktreeReadOnly(sessionName)
		if err != nil {
			t.Fatalf("WorktreeReadOnly: %v", err)
		}
		if got != readOnly {
			t.Errorf("agent_status.worktree_readonly after a spawn with WorktreeReadOnly=%v = %v", readOnly, got)
		}
	}

	spawn(true)
	if err := d.SetEnded(sessionName); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	spawn(false)
}
