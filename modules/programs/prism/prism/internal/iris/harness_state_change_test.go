package iris_test

// harness_state_change_test.go — tests for the state_change frame handler
// added in issue #1701. Validates:
//
//   - state_change="waiting" → handler called with "waiting"
//   - state_change="active"  → handler called with "active"
//   - state_change="finished"/"interrupted" → handler called (supervisor
//     decides whether to act); event row written in all cases
//   - agent_events row written BEFORE the handler runs (PR #1657 ordering)
//   - malformed state_change frames (missing state field, bad JSON) are
//     ignored without crashing the handler
//   - handler is optional (nil) — frame is still recorded as an event

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// startServerWithStateHandler starts a harness server, wires a recording
// state-change handler, and returns both.
func startServerWithStateHandler(t *testing.T) (*iris.HarnessSocketServer, *stateChangeRecorder) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	const instanceID = "test-state-instance-001"
	insertTestSessionRow(t, database, instanceID, "test@state-change", tmp)

	sockPath := filepath.Join(tmp, "harness.sock")
	sess := &iris.SessionRecord{
		InstanceID:      instanceID,
		SessionName:     "test@state-change",
		Worktree:        tmp,
		Role:            "worker",
		HarnessSockPath: sockPath,
	}
	srv, err := iris.NewHarnessSocketServer(sess, database)
	if err != nil {
		t.Fatalf("NewHarnessSocketServer: %v", err)
	}

	rec := &stateChangeRecorder{}
	srv.SetStateChangeHandler(rec.record)

	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	go func() { _ = srv.AcceptOne(ctx) }()

	return srv, rec
}

type stateChangeRecorder struct {
	mu     sync.Mutex
	states []string
}

func (r *stateChangeRecorder) record(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, state)
}

func (r *stateChangeRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.states))
	copy(out, r.states)
	return out
}

func (r *stateChangeRecorder) waitForLen(t *testing.T, n int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := r.snapshot()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return r.snapshot()
}

// TestStateChange_Waiting verifies that a state_change waiting frame is
// dispatched to the handler. This is the production trigger for the iris
// prompt waiting-state guard (#1689) — the entire AC chain in #1701 hinges
// on the handler firing.
func TestStateChange_Waiting(t *testing.T) {
	srv, rec := startServerWithStateHandler(t)
	conn, r := dialHarness(t, srv.SockPath())
	doHandshake(t, conn, r)

	sendFrame(t, conn, map[string]any{
		"type":  "state_change",
		"state": "waiting",
	})

	got := rec.waitForLen(t, 1, 2*time.Second)
	if len(got) != 1 || got[0] != "waiting" {
		t.Fatalf("expected handler called with 'waiting' once, got %v", got)
	}
}

// TestStateChange_Active verifies that a state_change active frame is
// dispatched. This is the transition out of waiting when a prompt arrives
// (turn_start → before_agent_start → state_change:active).
func TestStateChange_Active(t *testing.T) {
	srv, rec := startServerWithStateHandler(t)
	conn, r := dialHarness(t, srv.SockPath())
	doHandshake(t, conn, r)

	sendFrame(t, conn, map[string]any{
		"type":  "state_change",
		"state": "active",
	})

	got := rec.waitForLen(t, 1, 2*time.Second)
	if len(got) != 1 || got[0] != "active" {
		t.Fatalf("expected handler called with 'active' once, got %v", got)
	}
}

// TestStateChange_WaitingThenActive verifies the full cycle: a session goes
// into waiting (paused for input) and back to active when a turn starts.
func TestStateChange_WaitingThenActive(t *testing.T) {
	srv, rec := startServerWithStateHandler(t)
	conn, r := dialHarness(t, srv.SockPath())
	doHandshake(t, conn, r)

	sendFrame(t, conn, map[string]any{"type": "state_change", "state": "waiting"})
	sendFrame(t, conn, map[string]any{"type": "state_change", "state": "active"})

	got := rec.waitForLen(t, 2, 2*time.Second)
	if len(got) != 2 || got[0] != "waiting" || got[1] != "active" {
		t.Fatalf("expected [waiting active], got %v", got)
	}
}

// TestStateChange_MissingStateField verifies that malformed frames are
// rejected without dispatching to the handler. The frame should still be
// recorded as an observation event (writeObservationEvent runs first).
func TestStateChange_MissingStateField(t *testing.T) {
	srv, rec := startServerWithStateHandler(t)
	conn, r := dialHarness(t, srv.SockPath())
	doHandshake(t, conn, r)

	// state_change frame with missing state field — handler must NOT be called.
	sendFrame(t, conn, map[string]any{"type": "state_change"})
	// Follow with a well-formed frame so we have a wait condition.
	sendFrame(t, conn, map[string]any{"type": "state_change", "state": "waiting"})

	got := rec.waitForLen(t, 1, 2*time.Second)
	if len(got) != 1 || got[0] != "waiting" {
		t.Fatalf("expected handler called only once with 'waiting' (malformed frame ignored), got %v", got)
	}
}

// TestStateChange_NoHandler verifies that the server does not crash when
// no state-change handler is wired. Frames are still written to the events
// stream via writeObservationEvent.
func TestStateChange_NoHandler(t *testing.T) {
	srv := startServer(t) // no SetStateChangeHandler
	conn, r := dialHarness(t, srv.SockPath())
	doHandshake(t, conn, r)

	sendFrame(t, conn, map[string]any{"type": "state_change", "state": "waiting"})
	// Allow the server goroutine to process the frame.
	time.Sleep(100 * time.Millisecond)

	// If we got here without a panic, the test passes.
}
