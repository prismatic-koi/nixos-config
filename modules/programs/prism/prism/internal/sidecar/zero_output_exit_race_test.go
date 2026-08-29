package sidecar

// Tests for the fast-agent race in the finished-debounce
// zero-output-exit branch. A fast agent that produces real assistant output
// and then signals `finished` before its `turn_start` (idle -> active) upsert
// has been persisted must not be misclassified as an errored zero-output exit.
//
// See internal/sidecar/events.go (finished-debounce handler) and the
// assistantOutputSeen flag on Sidecar for the fix. The paired negative case
// (state_change{finished} from idle with NO assistant output) must still land
// in error to preserve the phantom-PR guard.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// TestSocketPipe_FastAgentRace_MsgAssistantThenFinishedFromIdle reproduces
// the race: a worker seeds persisted state idle, sends a msg_assistant
// (which the sidecar buffers in pipeAccum and latches assistantOutputSeen
// for), then sends state_change{finished} WITHOUT a persisted turn_start
// before the finished-debounce fires. Because assistant output was produced
// this session, the debounce must resolve to StateFinished (via a synthesised
// idle -> active -> finished path) and notify the coordinator with the
// "has finished" wording.
//
// This is the positive case: a fast agent that DID produce output must
// reach finished, not error. Without the fix, the debounce sees
// persisted state = idle and routes to notifyCoordinatorError.
func TestSocketPipe_FastAgentRace_MsgAssistantThenFinishedFromIdle(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newZeroOutputWorkerSidecar(t, sockPath)

	// Seed persisted idle: this reproduces the production scenario where
	// tmux-session-start has written idle, and the fast turn_start upsert
	// has not been persisted before the finished-debounce fires.
	seedWorkerIdle(t, sc)

	var (
		deliverMu    sync.Mutex
		capturedText string
		capturedCnt  int
	)
	sc.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		deliverMu.Lock()
		defer deliverMu.Unlock()
		capturedCnt++
		capturedText = text
		return nil
	}

	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sockPath)

	// Reproduce the race: msg_assistant (real output) then
	// state_change{finished} — NO turn_start persisted.
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "hello from the fast worker"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer was created after state_change{finished}")
	}

	// Pre-fire state must still be idle: no turn_start was sent, so
	// nothing has upserted the row past the seed.
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateIdle) {
		t.Fatalf("pre-fire state = %q, want %q (no turn_start sent — seed must persist)", s, agent.StateIdle)
	}

	timer.Fire()

	got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateFinished), 2*time.Second)
	if got != string(agent.StateFinished) {
		t.Errorf("state after fast-agent-race debounce = %q, want %q", got, agent.StateFinished)
	}
	if got == string(agent.StateError) {
		t.Error("fast-agent race with real output was misclassified as error — issue #2409 regression")
	}

	sc.WaitNotifies()

	deliverMu.Lock()
	defer deliverMu.Unlock()
	if capturedCnt != 1 {
		t.Fatalf("deliverFn invocations = %d, want 1", capturedCnt)
	}
	wantText := "Agent testrepo@feature has finished its current task"
	if capturedText != wantText {
		t.Errorf("coordinator notification text = %q, want %q", capturedText, wantText)
	}
	if strings.Contains(capturedText, "has errored") {
		t.Errorf("fast-agent race with real output must not use 'has errored' wording: %q", capturedText)
	}

	conn.Close()
	_ = wait()
}

// TestSocketPipe_TrueZeroOutputExit_NoMsgAssistantStillErrors is the paired
// negative case: same sequence (state_change{finished} from
// persisted idle) but WITHOUT any msg_assistant frame. This is a genuine
// zero-output exit (the phantom-PR case) and must still resolve to
// StateError with the "has errored" wording.
//
// It complements TestSocketPipe_ZeroOutputExit_ClassifiesAsError (which
// includes a turn_end): here we send only state_change{finished}, exercising
// the assistantOutputSeen=false discriminator directly.
func TestSocketPipe_TrueZeroOutputExit_NoMsgAssistantStillErrors(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newZeroOutputWorkerSidecar(t, sockPath)

	seedWorkerIdle(t, sc)

	var (
		deliverMu    sync.Mutex
		capturedText string
		capturedCnt  int
	)
	sc.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		deliverMu.Lock()
		defer deliverMu.Unlock()
		capturedCnt++
		capturedText = text
		return nil
	}

	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sockPath)

	// True zero-output exit: state_change{finished} arrives with NO
	// preceding msg_assistant and NO turn_start.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer was created after state_change{finished}")
	}

	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateIdle) {
		t.Fatalf("pre-fire state = %q, want %q", s, agent.StateIdle)
	}

	timer.Fire()

	got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateError), 2*time.Second)
	if got != string(agent.StateError) {
		t.Errorf("state after true zero-output debounce = %q, want %q", got, agent.StateError)
	}
	if got == string(agent.StateFinished) {
		t.Error("true zero-output exit was misclassified as finished — issue #2081 regression from #2409 fix")
	}

	sc.WaitNotifies()

	deliverMu.Lock()
	defer deliverMu.Unlock()
	if capturedCnt != 1 {
		t.Fatalf("deliverFn invocations = %d, want 1", capturedCnt)
	}
	wantText := "Agent testrepo@feature has errored its current task"
	if capturedText != wantText {
		t.Errorf("coordinator notification text = %q, want %q", capturedText, wantText)
	}
	if strings.Contains(capturedText, "has finished") {
		t.Errorf("true zero-output exit must not use 'has finished' wording: %q", capturedText)
	}

	conn.Close()
	_ = wait()
}
