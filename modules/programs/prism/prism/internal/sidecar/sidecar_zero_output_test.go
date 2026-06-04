// Tests for the zero-output-exit classification fix (issue #2081).
//
// A worker session that connects, produces no assistant output, and
// disconnects within seconds of the handshake was previously classified as
// `finished` — the coordinator then chased a PR that never existed. The fix in
// handleSessionFinished combines two signals from the issue:
//
//	(3) the persisted-state machine rejects the transition to finished (the
//	    session never reached `active`, so it is still `idle`); and
//	(1) the session produced no assistant output (sawAssistantOutput == false).
//
// When both hold, the session is routed to StateError and the coordinator
// receives the "has errored" notification instead of "has finished".
//
// # Isolation contract
//
// newZeroOutputWorkerSidecar redirects XDG_STATE_HOME to a t.TempDir(), sets
// PRISM_TEST_MODE_RESTRICT_HOSTAPI=1, uses an openTestDB temp database, and
// installs a capturing notifyCoordinatorDeliverFn seam so no notification ever
// reaches a live host. Session names carry the prism-test@ prefix per AGENTS.md.
package sidecar

import (
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// coordNotifyTracker records every coordinator notification delivered through
// the notifyCoordinatorDeliverFn seam. It is safe for concurrent use because
// the delivery runs on a goNotify goroutine; readers must drain via
// sc.WaitNotifies() before calling snapshot().
type coordNotifyTracker struct {
	mu    sync.Mutex
	calls []coordNotifyCall
}

type coordNotifyCall struct {
	to   string
	text string
}

func (c *coordNotifyTracker) record(to, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, coordNotifyCall{to: to, text: text})
}

func (c *coordNotifyTracker) snapshot() []coordNotifyCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]coordNotifyCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// newZeroOutputWorkerSidecar builds a socket-pipe worker sidecar against an
// isolated temp DB, seeds a discoverable coordinator row and the worker's
// initial `idle` agent_status row (as tmux-session-start does before the
// sidecar attaches), and installs a capturing coordinator-delivery seam.
func newZeroOutputWorkerSidecar(t *testing.T, sockPath, worker, coord, repo string) (*Sidecar, *testClock, *coordNotifyTracker) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName:           worker,
		Repo:                  repo,
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             "worker",
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		PipeReconnectTimeout:  200 * time.Millisecond,
		Harness:               pih.New("", "", ""),
	}
	sc := New(cfg)

	// Seed a discoverable coordinator for this repo so notifyCoordinator can
	// resolve a delivery target via CoordinatorForRepo.
	coordAgent := "coordinator"
	if err := d.UpsertStatusWithRootAgent(coord, repo, "/tmp/coord-"+coord, string(agent.StateActive), nil, nil, &coordAgent, nil); err != nil {
		t.Fatalf("seed coordinator %q: %v", coord, err)
	}
	// Seed the worker's initial idle state — this is the persisted state the
	// finished debounce sees when the zero-output worker never went active.
	if err := d.UpsertStatus(worker, repo, "/tmp/worker-"+worker, string(agent.StateIdle), nil, nil); err != nil {
		t.Fatalf("seed worker %q idle: %v", worker, err)
	}

	tr := &coordNotifyTracker{}
	sc.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		tr.record(sessionName, text)
		return nil
	}
	return sc, clk, tr
}

// TestSocketPipe_ZeroOutputExit_ClassifiedAsError drives the exact reproducer
// from issue #2081: a turn_end with NO preceding turn_start and NO assistant
// text (an empty turn), immediately followed by state_change{finished}. The
// worker never reached `active` and produced no output, so the finished
// debounce must route it to StateError and deliver the "has errored"
// notification, NOT "has finished".
func TestSocketPipe_ZeroOutputExit_ClassifiedAsError(t *testing.T) {
	sockPath := shortSockPath(t)
	worker := "prism-test@zero-output-worker"
	coord := "prism-test@main"
	repo := "prism-test"
	sc, clk, tr := newZeroOutputWorkerSidecar(t, sockPath, worker, coord, repo)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Empty turn: turn_end with no turn_start and no msg_assistant fragments,
	// then a finish signal. This mirrors the captured production frames where
	// turn_end was the first inbound frame.
	sendJSON(t, conn, map[string]any{"type": "turn_end", "agent": "worker"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer was created after state_change{finished}")
	}
	timer.Fire()
	sc.WaitNotifies()

	// The session must land in error, not finished.
	st := waitForState(t, sc.cfg.DB, worker, string(agent.StateError), 2*time.Second)
	if st != string(agent.StateError) {
		t.Fatalf("zero-output worker state = %q, want %q", st, agent.StateError)
	}

	// The coordinator must receive exactly one notification with the "has
	// errored" wording.
	calls := tr.snapshot()
	if len(calls) != 1 {
		t.Fatalf("coordinator notifications = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].to != coord {
		t.Errorf("notification target = %q, want %q", calls[0].to, coord)
	}
	wantText := "Agent " + worker + " has errored its current task"
	if calls[0].text != wantText {
		t.Errorf("notification body = %q, want %q", calls[0].text, wantText)
	}

	conn.Close()
	_ = wait()
}

// TestSocketPipe_NormalExit_StillClassifiedAsFinished is the negative case:
// a worker that runs a normal turn (turn_start → assistant text → turn_end)
// and produces output must still classify as StateFinished and deliver the
// "has finished" notification. This proves the zero-output guard is not
// over-broad.
//
// Note: in the PI socket-pipe path lastAssistantAgent is cleared on the
// root-agent turn_end, so it is "" at finish time in BOTH the normal and the
// zero-output case. The load-bearing "did this session produce output" signal
// is therefore sawAssistantOutput (set by the non-empty turn_end here), not
// lastAssistantAgent.
func TestSocketPipe_NormalExit_StillClassifiedAsFinished(t *testing.T) {
	sockPath := shortSockPath(t)
	worker := "prism-test@normal-output-worker"
	coord := "prism-test@main"
	repo := "prism-test"
	sc, clk, tr := newZeroOutputWorkerSidecar(t, sockPath, worker, coord, repo)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// A normal turn: go active, stream assistant text, end the turn with that
	// text, then finish.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "Opened PR #123 and pushed the branch."})
	sendJSON(t, conn, map[string]any{
		"type":  "turn_end",
		"agent": "worker",
		"usage": map[string]any{"input": 10, "output": 5},
	})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer was created after state_change{finished}")
	}
	timer.Fire()
	sc.WaitNotifies()

	st := waitForState(t, sc.cfg.DB, worker, string(agent.StateFinished), 2*time.Second)
	if st != string(agent.StateFinished) {
		t.Fatalf("normal worker state = %q, want %q", st, agent.StateFinished)
	}

	calls := tr.snapshot()
	if len(calls) != 1 {
		t.Fatalf("coordinator notifications = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].to != coord {
		t.Errorf("notification target = %q, want %q", calls[0].to, coord)
	}
	wantText := "Agent " + worker + " has finished its current task"
	if calls[0].text != wantText {
		t.Errorf("notification body = %q, want %q", calls[0].text, wantText)
	}

	conn.Close()
	_ = wait()
}

// TestSocketPipe_OutputWithoutActive_StillFinishes proves the guard requires
// BOTH signals: a session that produced assistant output but whose persisted
// state never advanced past idle (so the state machine would reject
// idle → finished, signal 3) is NOT routed to error, because it produced
// output (signal 1 is false). This is the combination boundary that keeps the
// fix narrow — only genuine zero-output exits are reclassified.
func TestSocketPipe_OutputWithoutActive_StillFinishes(t *testing.T) {
	sockPath := shortSockPath(t)
	worker := "prism-test@output-no-active-worker"
	coord := "prism-test@main"
	repo := "prism-test"
	sc, clk, tr := newZeroOutputWorkerSidecar(t, sockPath, worker, coord, repo)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Produce assistant output WITHOUT a turn_start, so agent_status stays at
	// the seeded `idle` while sawAssistantOutput becomes true.
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "Some real work happened here."})
	sendJSON(t, conn, map[string]any{"type": "turn_end", "agent": "worker"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer was created after state_change{finished}")
	}
	timer.Fire()
	sc.WaitNotifies()

	// Signal (1) is false (output was produced), so the guard must NOT fire:
	// the session finishes despite the idle→finished state-machine rejection.
	st := waitForState(t, sc.cfg.DB, worker, string(agent.StateFinished), 2*time.Second)
	if st != string(agent.StateFinished) {
		t.Fatalf("worker-with-output state = %q, want %q", st, agent.StateFinished)
	}

	calls := tr.snapshot()
	if len(calls) != 1 {
		t.Fatalf("coordinator notifications = %d, want 1: %+v", len(calls), calls)
	}
	wantText := "Agent " + worker + " has finished its current task"
	if calls[0].text != wantText {
		t.Errorf("notification body = %q, want %q", calls[0].text, wantText)
	}

	conn.Close()
	_ = wait()
}
