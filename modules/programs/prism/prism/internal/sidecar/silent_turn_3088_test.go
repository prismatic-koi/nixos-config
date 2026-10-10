package sidecar

// silent_turn_3088_test.go — issue #3088. pi can end its run after a turn
// with no text and no tool call. The extension then sends run_error, and the
// sidecar must end the session at once with the reason recorded, instead of
// waiting for the inactivity watchdog.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
)

func TestRunError_EndsRunInErrorStateWithReason(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newReviewAgentSidecarWithActivityTimeout(t, sockPath, 30*time.Second)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	const reason = `the model ended its turn with no text and no tool call (stop reason "error", provider stop reason "refusal"): The model refused to complete the request`
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{
		"type":            "turn_end",
		"stop_reason":     "error",
		"raw_stop_reason": "refusal",
		"usage":           map[string]any{"output": 7733},
	})
	sendJSON(t, conn, map[string]any{
		"type":            "run_error",
		"stop_reason":     "error",
		"raw_stop_reason": "refusal",
		"reason":          reason,
	})

	if got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateError), 2*time.Second); got != string(agent.StateError) {
		t.Fatalf("state after run_error: got %q, want %q", got, agent.StateError)
	}

	var runErr, turnEnd string
	for _, ev := range getEvents(t, sc.cfg.DB, sc.cfg.SessionName) {
		switch ev.Type {
		case "run_error":
			runErr = ev.Payload
		case "turn_end":
			turnEnd = ev.Payload
		case "stall_error":
			t.Errorf("run_error path wrote a stall_error event: %s", ev.Payload)
		}
	}
	var p struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(runErr), &p); err != nil || p.Reason != reason {
		t.Errorf("run_error event payload: got %q, want reason %q", runErr, reason)
	}
	if !strings.Contains(turnEnd, `"stop_reason":"error"`) {
		t.Errorf("turn_end event does not hold the stop reason: %s", turnEnd)
	}

	// Timers: #1 handshake, #2 turn_start, #3 turn_end, #4 run_error. The
	// watchdog must find the session terminal and write nothing.
	timer := clk.WaitForTimerCount(4, 5*time.Second)
	if timer == nil {
		t.Fatal("no activity watchdog timer registered after run_error")
	}
	timer.Fire()
	for _, ev := range getEvents(t, sc.cfg.DB, sc.cfg.SessionName) {
		if ev.Type == "stall_error" {
			t.Errorf("watchdog wrote stall_error after run_error ended the run: %s", ev.Payload)
		}
	}

	conn.Close()
	_ = wait()
}

// If the watchdog fires in a turn that has no turn_end yet, the turn's text
// is held only in pipeAccum. The watchdog must write it, not discard it.
func TestInactivityWatchdog_FlushesBufferedAssistantText(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newReviewAgentSidecarWithActivityTimeout(t, sockPath, 30*time.Second)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "<verdict>PASS</verdict>"})

	// Timers: #1 handshake, #2 turn_start, #3 msg_assistant.
	timer := clk.WaitForTimerCount(3, 5*time.Second)
	if timer == nil {
		t.Fatal("no activity watchdog timer registered after msg_assistant")
	}
	timer.Fire()

	if got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateError), 2*time.Second); got != string(agent.StateError) {
		t.Fatalf("state after watchdog fire: got %q, want %q", got, agent.StateError)
	}
	sc.WaitNotifies()

	var found bool
	for _, ev := range getEvents(t, sc.cfg.DB, sc.cfg.SessionName) {
		if ev.Type == "msg_assistant" && strings.Contains(ev.Payload, "PASS") {
			found = true
		}
	}
	if !found {
		t.Error("watchdog discarded the buffered assistant text: no msg_assistant row holds it")
	}

	conn.Close()
	_ = wait()
}

// An empty accumulator must not produce a msg_assistant row: the review
// report reads the latest such row as the agent's last message.
func TestInactivityWatchdog_EmptyAccumulatorWritesNoRow(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newReviewAgentSidecarWithActivityTimeout(t, sockPath, 30*time.Second)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	timer := clk.WaitForTimerCount(2, 5*time.Second)
	if timer == nil {
		t.Fatal("no activity watchdog timer registered after turn_start")
	}
	timer.Fire()

	if got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateError), 2*time.Second); got != string(agent.StateError) {
		t.Fatalf("state after watchdog fire: got %q, want %q", got, agent.StateError)
	}
	sc.WaitNotifies()

	for _, ev := range getEvents(t, sc.cfg.DB, sc.cfg.SessionName) {
		if ev.Type == "msg_assistant" {
			t.Errorf("watchdog wrote a msg_assistant row for an empty accumulator: %s", ev.Payload)
		}
	}

	conn.Close()
	_ = wait()
}
