package sidecar

import (
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
)

// TestSocketPipe_TurnStartClearsEscalated verifies that an incoming
// turn_start frame transitions a session out of the "escalated" state back
// into "active". The state machine must permit escalated→active and the
// sidecar's PI turn_start handler must perform the upsert unconditionally
// (no reviewing-style guard for escalated).
func TestSocketPipe_TurnStartClearsEscalated(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive the session to active, then directly write escalated to the DB
	// (mirroring what `prism escalate` does on the host side). The sidecar's
	// next incoming turn_start must clear it.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})

	// Wait until the state is reflected in the DB.
	deadline := time.Now().Add(2 * time.Second)
	for getState(t, sc.cfg.DB, sc.cfg.SessionName) != string(agent.StateActive) {
		if time.Now().After(deadline) {
			t.Fatal("session never reached active state")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Inject the escalated state directly: this is what `prism escalate`
	// does (UpsertStatus to escalated) before the sidecar sees the next
	// frame.
	if err := sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo,
		sc.cfg.Worktree, string(agent.StateEscalated), nil, nil); err != nil {
		t.Fatalf("inject escalated: %v", err)
	}

	// turn_start must clear it back to active.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	deadline = time.Now().Add(2 * time.Second)
	for {
		st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if st == string(agent.StateActive) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("escalated did not clear after turn_start; state=%q", st)
		}
		time.Sleep(20 * time.Millisecond)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_FinishedDebounceSuppressedWhileEscalated verifies that
// while a session is in the escalated state, the sidecar must NOT transition
// it to finished even when the harness fires state_change{finished}. The
// session.escalated bus event already informed the coordinator; a finished
// transition would clobber the escalated state and emit a redundant
// notification.
func TestSocketPipe_FinishedDebounceSuppressedWhileEscalated(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive to active.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})

	deadline := time.Now().Add(2 * time.Second)
	for getState(t, sc.cfg.DB, sc.cfg.SessionName) != string(agent.StateActive) {
		if time.Now().After(deadline) {
			t.Fatal("never reached active")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Inject escalated.
	if err := sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo,
		sc.cfg.Worktree, string(agent.StateEscalated), nil, nil); err != nil {
		t.Fatalf("inject escalated: %v", err)
	}

	// Now fire state_change{finished} — sidecar will start the debounce timer.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Wait for the debounce timer to be created.
	deadline = time.Now().Add(2 * time.Second)
	var idleTimer *testTimer
	for {
		idleTimer = clk.LastTimer()
		if idleTimer != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no finished debounce timer created")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Fire the timer — the suppression guard inside must bail out without
	// writing finished.
	idleTimer.Fire()

	// Give the goroutine a moment to process and verify state remains
	// escalated, NOT finished.
	time.Sleep(50 * time.Millisecond)
	st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if st != string(agent.StateEscalated) {
		t.Errorf("after debounce fire, state = %q, want %q (suppression failed)",
			st, agent.StateEscalated)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}
