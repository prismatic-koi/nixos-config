package session

// Tests for the keybind carve-out of the empty-prompt guard. The guard at
// the top of SpawnSession refuses LayoutFull / LayoutAgentOnly when
// opts.Prompt is empty. The tmux Prefix+a keybind needs to spawn a
// full-layout session with no initial prompt because the operator types it
// to the live agent after the popup attaches. The opts.AllowEmptyPrompt field
// opts the caller out of the guard; cmd/spawn.go sets it when
// PRISM_KEYBIND_SPAWN is present (the dedicated keybind sentinel).
//
// These tests are package-internal (`package session`, not session_test)
// so they can use spyTmuxBin / openSpawnTestDB from spawn_test.go — the
// same pattern as TestSpawnSession_NoPrompt_LayoutAgentOnly_Rejected.

import (
	"strings"
	"testing"
)

// TestSpawnSession_AllowEmptyPrompt_LayoutFull_Accepted verifies that
// SpawnSession with Layout=LayoutFull, empty Prompt, AND AllowEmptyPrompt=true
// passes the layer-4 guard. We do not assert the full spawn succeeds — that
// would require a real tmux server and sidecar binary — only that the
// rejection at the layer-4 guard does NOT fire.
//
// A successful pass means the error (if any) does not contain the
// "Prompt is required" message, OR the call returns nil. Both outcomes
// confirm the relaxation worked; either way, the layer-4 reject does not
// match what we observe.
func TestSpawnSession_AllowEmptyPrompt_LayoutFull_Accepted(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@allow-empty-full"
	opts := SpawnOpts{
		SessionName: sessionName,
		Repo:        "myrepo",
		Worktree:    "/worktrees/myrepo-allow-empty-full",
		AgentRole:   "worker",
		// Prompt deliberately empty.
		Layout:           LayoutFull,
		IsolationMode:    "host",
		HarnessName:      "pi",
		AllowEmptyPrompt: true,
		PIExtensionDir:   testPIExtensionDir,
	}

	err := SpawnSession(d, opts)
	if err != nil && strings.Contains(err.Error(), "Prompt is required") {
		t.Fatalf("SpawnSession with AllowEmptyPrompt=true still returned the layer-4 'Prompt is required' rejection: %v", err)
	}
}

// TestSpawnSession_AllowEmptyPrompt_LayoutFull_OptInRequired is the
// matching negative test: the same call without AllowEmptyPrompt set is
// still rejected by the layer-4 guard. Together with the positive test
// above this proves the carve-out is gated on the opt-in and does not
// silently weaken the guard for any caller that forgot to set it.
func TestSpawnSession_AllowEmptyPrompt_LayoutFull_OptInRequired(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@allow-empty-full-default"
	opts := SpawnOpts{
		SessionName:   sessionName,
		Repo:          "myrepo",
		Worktree:      "/worktrees/myrepo-allow-empty-full-default",
		AgentRole:     "worker",
		Layout:        LayoutFull,
		IsolationMode: "host",
		HarnessName:   "pi",
		// AllowEmptyPrompt deliberately omitted (defaults to false).,
		PIExtensionDir: testPIExtensionDir,
	}

	err := SpawnSession(d, opts)
	if err == nil {
		t.Fatal("SpawnSession (empty Prompt, LayoutFull, AllowEmptyPrompt=false): got nil, want layer-4 rejection per issue #1891")
	}
	if !strings.Contains(err.Error(), "Prompt is required") {
		t.Errorf("error %q does not mention 'Prompt is required'", err.Error())
	}
	// Nothing should have been written to the DB: refusal must happen
	// before any side-effects.
	if st, _ := d.CurrentStatus(sessionName); st != nil {
		t.Errorf("agent_status row created despite empty-prompt rejection: %+v", st)
	}
}
