package review_test

// AC-6 regression: the review monitor must NOT wait the full per-agent
// timeout for an agent the spawn loop already declared failed (readiness
// timeout, config error, SpawnSession error, etc.). The mechanism that
// guarantees this is two-pronged:
//
//   1. RunAsync's monitor receives only liveSessions — agents that became
//      ready — so the failed-to-start agents are not in the monitor's
//      AgentSessions list at all.
//   2. The cleanup path for failed agents (cleanupAgentSession) transitions
//      the agent_status row to state="error", which db.GroupCompleted
//      treats as terminal. So even if a stale row remains in the group,
//      GroupCompleted will not block on it.
//
// This test validates (2) directly: it sets up a group with one "ready"
// agent (state=finished) and one "failed-readiness" agent (state=idle on
// gate trip → cleaned up to state=error), and asserts that GroupCompleted
// returns true. Without the state=error transition, GroupCompleted would
// return false and the monitor would spin until its timeout.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
)

func openMonitorTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestMonitor_FailedReadyAgent_DoesNotBlockGroupCompleted verifies AC-6:
// after the gate cleans up a failed-to-ready agent, the agent's row state
// is "error" (not "idle"), and db.GroupCompleted returns true once all the
// alive members have reached their terminal state. Without this, the monitor
// would block on the half-alive row forever.
func TestMonitor_FailedReadyAgent_DoesNotBlockGroupCompleted(t *testing.T) {
	d := openMonitorTestDB(t)

	// Register a group and seed two agents.
	groupID, err := d.RegisterGroup("test@parent")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	const sessReady = "test@parent~review-1-review-code"
	const sessFailed = "test@parent~review-1-review-goal"

	// Both agents start at state=idle (the seed state UpsertStatusSeedRootAgentName writes).
	for _, sess := range []string{sessReady, sessFailed} {
		if err := d.UpsertStatusSeedRootAgentName(sess, "test", "/tmp", "idle", nil, nil, "review-x"); err != nil {
			t.Fatalf("UpsertStatusSeedRootAgentName(%q): %v", sess, err)
		}
		if err := d.SetGroupID(sess, groupID); err != nil {
			t.Fatalf("SetGroupID(%q): %v", sess, err)
		}
	}

	// Pre-condition: with both at "idle", GroupCompleted must be false.
	if done, _ := d.GroupCompleted(groupID); done {
		t.Fatal("pre-condition: GroupCompleted = true with two idle members; want false")
	}

	// Simulate the gate tripping for sessFailed: the gate calls
	// gateReviewAgents → cleanupAgentSession, which transitions the row to
	// "error". Drive the same code path here by running the gate with a
	// short timeout and no readiness signal for sessFailed.
	agents := []review.Agent{
		{Name: "review-goal", OpencodeName: "review-goal"},
	}
	sessions := []string{sessFailed}
	spawnErr := make([]error, 1)
	spawnTimes := make([]time.Time, 1)
	review.GateReviewAgentsForTest(d, agents, sessions, spawnErr, spawnTimes,
		200*time.Millisecond, nil)
	if spawnErr[0] == nil {
		t.Fatal("gate did not record an error for the never-ready agent")
	}
	if !session.IsReadinessTimeout(spawnErr[0]) {
		t.Fatalf("gate spawnErr = %v, want *ReadinessTimeoutError", spawnErr[0])
	}

	// Verify post-cleanup state of the failed agent.
	stFailed, err := d.CurrentStatus(sessFailed)
	if err != nil {
		t.Fatalf("CurrentStatus(%q): %v", sessFailed, err)
	}
	if stFailed == nil {
		t.Fatalf("CurrentStatus(%q): row is gone — cleanup should leave the row but mark it error", sessFailed)
	}
	if stFailed.State != "error" {
		t.Errorf("failed-ready agent state = %q, want %q (AC-6: must be terminal so GroupCompleted treats it as done)", stFailed.State, "error")
	}

	// Now drive sessReady to a clean terminal state: finished.
	if err := d.UpsertStatus(sessReady, "test", "/tmp", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%q, finished): %v", sessReady, err)
	}

	// AC-6: GroupCompleted must now return true. Without the state=error
	// transition for sessFailed, this would still return false and the
	// monitor would block.
	done, gErr := d.GroupCompleted(groupID)
	if gErr != nil {
		t.Fatalf("GroupCompleted: %v", gErr)
	}
	if !done {
		t.Errorf("GroupCompleted = false; expected true now that all members are terminal (review-code=finished, review-goal=error)")
	}
}
