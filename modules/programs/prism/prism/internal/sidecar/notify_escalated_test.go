package sidecar

import (
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
)

// TestNotifyCoordinator_EscalatedSuppressed verifies that while a
// session is in the escalated state, the sidecar must NOT call
// notifyCoordinator with a "has finished" message. The session.escalated bus
// event has already informed the coordinator and a finished notification
// would be a duplicate, false signal.
func TestNotifyCoordinator_EscalatedSuppressed(t *testing.T) {
	// Redirect XDG_STATE_HOME so any sidecar writes (touchDashboardSentinel,
	// socket paths, etc.) stay within a temp directory and never touch the
	// developer's real prism state. Also activate the host-API socket guard
	// so promptdelivery refuses to dial any real host socket.
	useShortStateHome(t)

	d := openTestDB(t)

	coordSID := "coord-sid-escalated-suppressed"
	seedCoordinator(t, d, "test-repo", coordSID)
	coord := listenHostAPI(t, "test-repo@main")

	clk := newTestClock()
	workerSession := "test-repo@feature"
	cfg := Config{
		SessionName: workerSession,
		Repo:        "test-repo",
		Worktree:    "/tmp/test-worktree-escalated",
		HarnessURL:  "http://localhost:14002",
		DB:          d,
		Clock:       clk,
		Harness:     newSSEHarness(),
		AgentRole:   "worker",
	}
	worker := New(cfg)

	// Seed worker as escalated — this is the state `prism escalate` left it
	// in just before the harness started winding down.
	_ = d.UpsertStatus(workerSession, "test-repo", "/tmp/test-worktree-escalated",
		string(agent.StateEscalated), nil, nil)

	// Directly invoke notifyCoordinator (the call site we want to verify
	// suppresses). We do not need to drive the full debounce machinery; the
	// suppression guard inside notifyCoordinator must short-circuit before
	// any HTTP call is attempted.
	worker.notifyCoordinator("")

	// Allow any in-flight goroutine work to settle, though notifyCoordinator
	// is synchronous from the goroutine that calls it.
	time.Sleep(50 * time.Millisecond)

	// No bus messages must have been written.
	var totalMsgs int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ?", "test-repo@main").Scan(&totalMsgs); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	if totalMsgs != 0 {
		t.Errorf("escalated session must NOT send coordinator notification, but got %d bus message(s)", totalMsgs)
	}

	count := coord.count()
	if count != 0 {
		t.Errorf("prompts on the host-API socket of the coordinator = %d for escalated session, want 0", count)
	}
}
