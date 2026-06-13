package session

// Tests for F5 (#1880): when spawnFullLayout or spawnAgentOnlyLayout returns
// an error, SpawnSession must run the same cleanup primitives as the
// readiness-timeout path rather than returning a hint string and leaving
// residue in the DB.
//
// Before the fix the layout-failure path returned:
//   fmt.Errorf("%w — to remove side effects run: prism cleanup ...", layoutErr)
// without calling KillSidecar / cleanupHalfAliveSession / tmux.KillSession /
// removeInitialPrompt. The readiness-timeout path (which can fire for the same
// partially-spawned session) DID auto-clean. The asymmetry left DB residue
// (active agent_status row, allocated port) on every layout failure.
//
// The fix calls the same four cleanup primitives before returning the error,
// leaving the DB in the same clean state as a readiness-timeout failure.

import (
	"strings"
	"testing"
)

// TestSpawnSession_LayoutFailure_CleanesUpDB verifies that when the tmux
// step fails (forced by failTmuxBin), SpawnSession auto-cleans the DB so
// no residue is left for a retry:
//
//   - no agent_status row with ended_at IS NULL (or no row at all).
//   - no sessions row with ended_at IS NULL.
//   - port released (harness_port IS NULL).
func TestSpawnSession_LayoutFailure_CleansUpDB(t *testing.T) {
	d, _ := openSpawnTestDB(t)
	// Force the tmux step to fail — spawnAgentOnlyLayout calls
	// tmux.NewSessionDetached which will exit non-zero.
	failTmuxBin(t, "duplicate session (injected by test)")
	t.Setenv("PRISM_TEST_SUBPROCESS", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const sessionName = "myrepo@branch~layout-fail-cleanup"
	opts := SpawnOpts{
		SessionName:    sessionName,
		Repo:           "myrepo",
		Worktree:       "/worktrees/myrepo-branch",
		AgentRole:      "review-code",
		Prompt:         "review this PR",
		Layout:         LayoutAgentOnly,
		IsolationMode:  "host",
		HarnessName:    "pi",
		PIExtensionDir: testPIExtensionDir,
	}

	err := SpawnSession(d, opts)
	if err == nil {
		t.Fatal("SpawnSession with failing tmux: got nil error, want error")
	}
	// The error must still contain the operator hint for diagnosability.
	if !strings.Contains(err.Error(), "prism cleanup") {
		t.Errorf("error %q does not contain 'prism cleanup' hint", err.Error())
	}

	// DB cleanup check (a): agent_status must be ended (ended_at IS NOT NULL)
	// or absent entirely.
	st, statusErr := d.CurrentStatus(sessionName)
	if statusErr != nil {
		t.Fatalf("CurrentStatus: %v", statusErr)
	}
	if st != nil && st.EndedAt == nil {
		t.Errorf("agent_status row %q is still alive (ended_at IS NULL) after layout-failure cleanup — F5 cleanup did not run (#1880)", sessionName)
	}

	// DB cleanup check (b): sessions row must be ended or absent.
	// The sessions row is pre-inserted by SpawnSession before the layout
	// step, so after cleanup it must have ended_at IS NOT NULL.
	if st != nil && st.InstanceID != nil && *st.InstanceID != "" {
		sess, sessErr := d.SessionByInstanceID(*st.InstanceID)
		if sessErr != nil {
			t.Fatalf("SessionByInstanceID: %v", sessErr)
		}
		if sess != nil && sess.EndedAt == nil {
			t.Errorf("sessions row for instance_id %s has ended_at IS NULL after layout-failure cleanup — zombie row will block a retry (#1880 F5)",
				*st.InstanceID)
		}
	}

	// DB cleanup check (c): port must be released (harness_port IS NULL).
	if st != nil && st.HarnessPort != nil {
		t.Errorf("harness_port = %d after layout-failure cleanup — port was not released (#1880 F5)", *st.HarnessPort)
	}
}
