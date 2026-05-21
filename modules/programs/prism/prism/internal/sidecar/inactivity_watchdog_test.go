package sidecar

// inactivity_watchdog_test.go — coverage for the per-session inactivity
// watchdog added by #1709 to rescue review agents that complete substantive
// work but never emit state_change{finished}.
//
// The watchdog is a Sidecar-side timer that resets on every inbound frame
// (handlePipeFrame or HandleEvent). When it fires (after Config.ActivityTimeout
// of total silence), the session is force-transitioned to StateError with note
// "inactivity timeout" so the review-group monitor's GroupCompleted check
// returns true and the worker is freed.

import (
	"strings"
	"testing"
	"time"

	pih "github.com/prismatic-koi/prism/internal/harness/pi"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// newReviewAgentSidecarWithActivityTimeout builds a Sidecar with
// AgentRole="review-goal" and an explicit ActivityTimeout so the inactivity-
// watchdog path is exercisable in unit tests without real wall-clock waits.
// Returns the sidecar and the testClock so callers can drive the timer
// manually. Named to avoid colliding with newReviewAgentSidecar in
// sidecar_test.go (different signature, different purpose).
//
// Host-state isolation: redirects $XDG_STATE_HOME to a tempdir so any
// path lookup performed by the sidecar (e.g. notifyParentWorker on
// watchdog fire) cannot reach the real prism state directory (#1709,
// issue #1608 defence in depth).
func newReviewAgentSidecarWithActivityTimeout(t *testing.T, sockPath string, activityTimeout time.Duration) (*Sidecar, *testClock) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName:           "testrepo@main~review-1-review-goal",
		Repo:                  "testrepo",
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             "review-goal",
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		PipeReconnectTimeout:  200 * time.Millisecond,
		ActivityTimeout:       activityTimeout,
		IsolationMode:         config.IsolationMode("host"),
		Harness:               pih.New("", "", ""),
	}
	return New(cfg), clk
}

// TestReviewAgent_DefaultActivityTimeout verifies that New() defaults
// ActivityTimeout to DefaultReviewAgentInactivityTimeout for review-agent
// roles (AC: the rescue path is opt-in via role, not by explicit caller).
func TestReviewAgent_DefaultActivityTimeout(t *testing.T) {
	d := openTestDB(t)
	for _, role := range []string{
		"review-goal", "review-code", "review-context",
		"review-qa", "review-security",
	} {
		t.Run(role, func(t *testing.T) {
			sc := New(Config{
				SessionName: "testrepo@main~review-1-" + role,
				Repo:        "testrepo",
				DB:          d,
				AgentRole:   role,
				HarnessName: "pi",
				Harness:     pih.New("", "", ""),
			})
			if sc.cfg.ActivityTimeout != DefaultReviewAgentInactivityTimeout {
				t.Errorf("review agent role %q: ActivityTimeout = %v, want %v",
					role, sc.cfg.ActivityTimeout, DefaultReviewAgentInactivityTimeout)
			}
		})
	}
}

// TestNonReviewAgent_NoDefaultActivityTimeout verifies that workers and
// coordinators are NOT given a default inactivity timeout. These sessions
// may sit idle awaiting human or peer input for long periods; the watchdog
// is review-agent-only.
func TestNonReviewAgent_NoDefaultActivityTimeout(t *testing.T) {
	d := openTestDB(t)
	for _, role := range []string{"worker", "coordinator", "", "investigate"} {
		t.Run("role="+role, func(t *testing.T) {
			sc := New(Config{
				SessionName: "testrepo@main",
				Repo:        "testrepo",
				DB:          d,
				AgentRole:   role,
				HarnessName: "pi",
				Harness:     pih.New("", "", ""),
			})
			if sc.cfg.ActivityTimeout != 0 {
				t.Errorf("non-review role %q: ActivityTimeout = %v, want 0",
					role, sc.cfg.ActivityTimeout)
			}
		})
	}
}

// TestExplicitActivityTimeout_PreservedForAnyRole verifies that an explicit
// non-zero ActivityTimeout is preserved in New() for any role, so operators
// who want a workers-wide watchdog can opt into one without touching the
// review-agent default.
func TestExplicitActivityTimeout_PreservedForAnyRole(t *testing.T) {
	d := openTestDB(t)
	for _, role := range []string{"worker", "coordinator", "review-goal", ""} {
		t.Run("role="+role, func(t *testing.T) {
			explicit := 42 * time.Minute
			sc := New(Config{
				SessionName:     "testrepo@main",
				Repo:            "testrepo",
				DB:              d,
				AgentRole:       role,
				HarnessName:     "pi",
				Harness:         pih.New("", "", ""),
				ActivityTimeout: explicit,
			})
			if sc.cfg.ActivityTimeout != explicit {
				t.Errorf("explicit ActivityTimeout for role %q: got %v, want %v",
					role, sc.cfg.ActivityTimeout, explicit)
			}
		})
	}
}

// TestInactivityWatchdog_FiresAfterTurnEndWithNoStateChange reproduces the
// exact stall described in issue #1709: a review agent receives turn_start,
// streams an assistant message, sends turn_end, then never emits
// state_change{finished} (because the LLM stopReason was not "stop"). The
// session must be force-transitioned to StateError so GroupCompleted returns
// true and the review monitor can deliver.
func TestInactivityWatchdog_FiresAfterTurnEndWithNoStateChange(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newReviewAgentSidecarWithActivityTimeout(t, sockPath, 30*time.Second)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive the agent through a normal turn that ends without
	// state_change{finished} — the stall pattern.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "I checked the build and tests; all green."})
	sendJSON(t, conn, map[string]any{"type": "turn_end"})

	// The state should be active after turn_start. Wait for it to land in the
	// DB so the assertion is not racing the inbound-frame handler.
	if got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateActive), 2*time.Second); got != string(agent.StateActive) {
		t.Fatalf("state after turn_start: got %q, want %q", got, agent.StateActive)
	}

	// The watchdog is re-armed on every inbound frame, so the latest timer
	// registered is the live one (older timers have been Stop()ed). Wait
	// until the post-turn_end re-arm has happened, then fire that timer.
	// Timer registration order so far:
	//   #1 armed by the post-handshake touchActivity()
	//   #2 armed by turn_start's touchActivity()
	//   #3 armed by msg_assistant's touchActivity()
	//   #4 armed by turn_end's touchActivity()  ← the one we want
	timer := clk.WaitForTimerCount(4, 5*time.Second)
	if timer == nil {
		t.Fatal("no activity watchdog timer was registered after turn_end")
	}
	// Fire the watchdog — simulates the full ActivityTimeout of silence
	// post-turn_end.
	timer.Fire()

	// The session must reach StateError; GroupCompleted will then return true.
	if got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateError), 2*time.Second); got != string(agent.StateError) {
		t.Fatalf("state after inactivity timeout: got %q, want %q", got, agent.StateError)
	}

	conn.Close()
	_ = wait()
}

// TestInactivityWatchdog_ResetByInboundFrame verifies that any inbound frame
// resets the watchdog. Without this property the watchdog would falsely fire
// on agents doing legitimate long-running tool calls that punctuate the
// silence with tool_call/tool_result frames.
func TestInactivityWatchdog_ResetByInboundFrame(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newReviewAgentSidecarWithActivityTimeout(t, sockPath, 30*time.Second)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	// First watchdog timer (armed after handshake or first frame).
	t1 := clk.WaitForTimerCount(1, 5*time.Second)
	if t1 == nil {
		t.Fatal("no watchdog timer after first inbound frame")
	}

	// Send another frame. The handler must Stop() t1 and arm a new timer.
	sendJSON(t, conn, map[string]any{"type": "tool_call", "toolName": "bash"})
	if !t1.WaitStopped(5 * time.Second) {
		t.Fatal("watchdog timer was not stopped by a subsequent inbound frame")
	}
	// A new timer must now exist.
	t2 := clk.WaitForTimerCount(2, 5*time.Second)
	if t2 == nil {
		t.Fatal("no new watchdog timer was registered after the reset frame")
	}
	if t2 == t1 {
		t.Fatal("expected a fresh watchdog timer after reset, got the same instance")
	}

	conn.Close()
	_ = wait()
}

// TestInactivityWatchdog_DisabledForWorker verifies that a worker session
// (ActivityTimeout=0) never registers a watchdog timer. This is the
// safety property protecting non-review sessions from accidental
// force-termination when they sit waiting on a human prompt or a slow
// review-complete delivery.
func TestInactivityWatchdog_DisabledForWorker(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive frames; no watchdog timer should be registered (only finished-
	// debounce-style timers, which require state_change{finished}).
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "tool_call", "toolName": "bash"})
	sendJSON(t, conn, map[string]any{"type": "turn_end"})

	// Give the sidecar a moment to process the frames.
	if got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateActive), 2*time.Second); got != string(agent.StateActive) {
		t.Fatalf("worker state after turn_start: got %q, want active", got)
	}
	if n := clk.TimerCount(); n != 0 {
		// We expect ZERO timers because there is no state_change{finished}
		// (which would arm the 2s finished-debounce) AND ActivityTimeout=0
		// (so touchActivity is a no-op).
		t.Errorf("worker registered %d timer(s); want 0 with ActivityTimeout=0", n)
	}

	conn.Close()
	_ = wait()
}

// TestInactivityWatchdog_NoOpWhenAlreadyTerminal verifies the watchdog is
// state-idempotent: if the session has already reached a terminal state via
// a normal path (state_change{finished} → debounce → StateFinished), firing
// the watchdog must not overwrite that state.
func TestInactivityWatchdog_NoOpWhenAlreadyTerminal(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newReviewAgentSidecarWithActivityTimeout(t, sockPath, 30*time.Second)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Timer registration order:
	//   #1 activity watchdog (armed post-handshake by touchActivity)
	//   #2 activity watchdog (armed by turn_start's touchActivity, replacing #1)
	//   #3 activity watchdog (armed by state_change's touchActivity, replacing #2)
	//   #4 finished debounce (armed by handleSessionFinished after state_change)
	finishedDebounce := clk.WaitForTimerCount(4, 5*time.Second)
	if finishedDebounce == nil {
		t.Fatal("no finished-debounce timer registered")
	}
	finishedDebounce.Fire()
	if got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateFinished), 2*time.Second); got != string(agent.StateFinished) {
		t.Fatalf("state after finished debounce: got %q, want finished", got)
	}

	// Now fire the live activity watchdog (timer #3) — must be a no-op
	// because the session is already terminal. Timer #3 was Stop()ed but
	// testTimer.Fire still invokes the closure when called directly; we
	// instead drive the closure on a *non-stopped* clone path by re-arming
	// touchActivity. Simpler: trigger handleActivityTimeout directly with the
	// timeout argument and assert state is unchanged. The closure obeys the
	// same terminal-state guard regardless of which timer instance armed it.
	sc.handleActivityTimeout(sc.cfg.ActivityTimeout)

	// State must remain finished.
	if got := getState(t, sc.cfg.DB, sc.cfg.SessionName); got != string(agent.StateFinished) {
		t.Errorf("state after watchdog fire on terminal session: got %q, want finished (watchdog must be no-op when terminal)", got)
	}

	conn.Close()
	_ = wait()
}

// ── #1761: mid-tool heartbeat ───────────────────────────────────────────────
//
// The PI extension emits a `tool_progress` frame on a fixed cadence while a
// tool execution is in flight so that long-running bash invocations (e.g.
// `nix build`, `go test -count=20`) don't silence the wire long enough to
// trip the per-session inactivity watchdog.
//
// Sidecar-side contract:
//
//   - tool_progress is treated as an inbound frame: it resets the watchdog
//     via touchActivity (same path as every other frame).
//   - tool_progress is NOT written to agent_events: it must remain invisible
//     to downstream consumers (narrative renderer, checkin, TUI). The
//     renderer's default branch would otherwise print "tool_progress" lines
//     between every tool_call/tool_result pair.
//   - The genuine-stuck rescue path is preserved: if no tool_progress (or
//     other frame) arrives, the watchdog still fires within the configured
//     window. This is implicitly true given the watchdog has no per-frame
//     branch, but the test asserts it explicitly so future refactors can't
//     accidentally regress it.

// TestToolProgressHeartbeat_ResetsWatchdog drives the exact pathology from
// issue #1761: a review agent runs a long bash tool with no other frames
// emitted, but the extension sends a tool_progress heartbeat. The watchdog
// must be reset (not fire) for each heartbeat that arrives within the
// timeout window.
func TestToolProgressHeartbeat_ResetsWatchdog(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newReviewAgentSidecarWithActivityTimeout(t, sockPath, 30*time.Second)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Open a turn and start a tool call — the "before the long sleep"
	// situation the extension sees just before kicking off `nix build`.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{
		"type": "tool_call",
		"id":   "call-1",
		"name": "bash",
		"args": map[string]any{"command": "nix build .#prism"},
	})

	// Timer registration order so far:
	//   #1 armed by post-handshake touchActivity
	//   #2 armed by turn_start touchActivity (replaces #1)
	//   #3 armed by tool_call touchActivity  (replaces #2)
	t3 := clk.WaitForTimerCount(3, 5*time.Second)
	if t3 == nil {
		t.Fatal("no watchdog timer after tool_call")
	}

	// Heartbeat #1 — must Stop t3 and arm t4.
	sendJSON(t, conn, map[string]any{
		"type": "tool_progress",
		"id":   "call-1",
		"name": "bash",
	})
	if !t3.WaitStopped(5 * time.Second) {
		t.Fatal("tool_progress did not reset the watchdog (t3 not stopped)")
	}
	t4 := clk.WaitForTimerCount(4, 5*time.Second)
	if t4 == nil || t4 == t3 {
		t.Fatal("no fresh watchdog timer after tool_progress")
	}

	// Heartbeat #2 — must reset again.
	sendJSON(t, conn, map[string]any{
		"type": "tool_progress",
		"id":   "call-1",
		"name": "bash",
	})
	if !t4.WaitStopped(5 * time.Second) {
		t.Fatal("second tool_progress did not reset the watchdog (t4 not stopped)")
	}
	t5 := clk.WaitForTimerCount(5, 5*time.Second)
	if t5 == nil || t5 == t4 {
		t.Fatal("no fresh watchdog timer after second tool_progress")
	}

	conn.Close()
	_ = wait()
}

// TestToolProgressHeartbeat_NotWrittenToEvents verifies tool_progress is
// excluded from agent_events. The narrative renderer's default branch would
// otherwise print one "tool_progress" line per heartbeat, polluting checkin
// output and the TUI feed with internal liveness noise.
func TestToolProgressHeartbeat_NotWrittenToEvents(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, _ := newReviewAgentSidecarWithActivityTimeout(t, sockPath, 30*time.Second)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Send a tool_call (must persist) bracketed by several tool_progress
	// heartbeats (must NOT persist), then a tool_result (must persist).
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{
		"type": "tool_call",
		"id":   "call-1",
		"name": "bash",
		"args": map[string]any{"command": "nix build .#prism"},
	})
	for i := 0; i < 5; i++ {
		sendJSON(t, conn, map[string]any{
			"type": "tool_progress",
			"id":   "call-1",
			"name": "bash",
		})
	}
	sendJSON(t, conn, map[string]any{
		"type":    "tool_result",
		"id":      "call-1",
		"success": true,
		"output":  "ok",
	})
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	var (
		sawToolCall   bool
		sawToolResult bool
	)
	for _, ev := range events {
		if ev.Type == "tool_progress" {
			t.Errorf("tool_progress frame leaked into agent_events (event=%+v) — must be invisible to downstream consumers", ev)
		}
		if ev.Type == "tool_call" {
			sawToolCall = true
		}
		if ev.Type == "tool_result" {
			sawToolResult = true
		}
	}
	if !sawToolCall {
		t.Error("tool_call event missing from agent_events (control)")
	}
	if !sawToolResult {
		t.Error("tool_result event missing from agent_events (control)")
	}
}

// TestToolProgressHeartbeat_GenuineStuckStillFires verifies the rescue path
// is preserved: if the agent stops emitting heartbeats (PI hung, event loop
// blocked, etc.), the watchdog still force-transitions the session to error
// within the configured window. This is the property that makes #1728's
// rescue logic load-bearing — the heartbeat must not be load-bearing in the
// opposite direction (preventing fire when truly stuck).
func TestToolProgressHeartbeat_GenuineStuckStillFires(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newReviewAgentSidecarWithActivityTimeout(t, sockPath, 30*time.Second)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive into a long tool call with a couple of heartbeats — simulates
	// a tool running normally for a while.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{
		"type": "tool_call",
		"id":   "call-1",
		"name": "bash",
		"args": map[string]any{"command": "nix build .#prism"},
	})
	sendJSON(t, conn, map[string]any{"type": "tool_progress", "id": "call-1", "name": "bash"})
	sendJSON(t, conn, map[string]any{"type": "tool_progress", "id": "call-1", "name": "bash"})

	// State is now active. Timer registration:
	//   #1 post-handshake, #2 turn_start, #3 tool_call,
	//   #4 first tool_progress, #5 second tool_progress.
	if got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateActive), 2*time.Second); got != string(agent.StateActive) {
		t.Fatalf("state after tool_call: got %q, want active", got)
	}
	live := clk.WaitForTimerCount(5, 5*time.Second)
	if live == nil {
		t.Fatal("no watchdog timer after two heartbeats")
	}
	// Now the agent stops emitting heartbeats entirely (PI hung). Fire the
	// live watchdog — the rescue path must fire.
	live.Fire()

	if got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateError), 2*time.Second); got != string(agent.StateError) {
		t.Fatalf("state after watchdog fire on heartbeat silence: got %q, want %q — rescue path regressed", got, agent.StateError)
	}

	conn.Close()
	_ = wait()
}

// ── #1842: goNotify registration ─────────────────────────────────────────────
//
// handleActivityTimeout previously spawned the parent-worker startup-failure
// notification with a raw `go` instead of s.goNotify. The raw `go` bypassed
// notifyWG, so tests had to sleep or poll to observe the notification — the
// same race class that motivated goNotify in #1713/#1716.
//
// The fix wraps the call in s.goNotify so WaitNotifies() drains it
// deterministically.

// TestInactivityWatchdog_NotifyRegisteredWithWaitGroup verifies that when the
// inactivity watchdog fires on a review-agent session, the parent-worker
// startup-failure notification is tracked by notifyWG so WaitNotifies()
// returns only after the delivery has completed — no sleeping or polling
// required (#1842).
func TestInactivityWatchdog_NotifyRegisteredWithWaitGroup(t *testing.T) {
	// The parent worker session for "prism-test@worker-1842~review-1-review-goal"
	// is "prism-test@worker-1842".
	workerSession := "prism-test@worker-1842"
	reviewSession := workerSession + "~review-1-review-goal"

	// NewIsolated seeds the worker as an active session in the DB and starts
	// an httptest.Server to capture delivered prompt bodies. It also sets
	// XDG_STATE_HOME and PRISM_TEST_MODE_RESTRICT_HOSTAPI so no host-state
	// is touched (#1608).
	bus := sidecartest.NewIsolated(t, workerSession)

	sockPath := shortSockPath(t)
	clk := newTestClock()
	cfg := Config{
		SessionName:           reviewSession,
		Repo:                  "prism-test",
		Worktree:              t.TempDir(),
		DB:                    bus.DB,
		Clock:                 clk,
		AgentRole:             "review-goal",
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		PipeReconnectTimeout:  200 * time.Millisecond,
		ActivityTimeout:       30 * time.Second,
		IsolationMode:         config.IsolationMode("host"),
		Harness:               pih.New("", "", ""),
	}
	sc := New(cfg)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive to active so the watchdog timer is armed.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	if got := waitForState(t, bus.DB, sc.cfg.SessionName, string(agent.StateActive), 2*time.Second); got != string(agent.StateActive) {
		t.Fatalf("state after turn_start: got %q, want %q", got, agent.StateActive)
	}

	// Wait for the watchdog timer that turn_start armed (#2 overall — #1 from
	// the post-handshake touchActivity, #2 from turn_start's touchActivity).
	timer := clk.WaitForTimerCount(2, 5*time.Second)
	if timer == nil {
		t.Fatal("no activity watchdog timer registered after turn_start")
	}

	// Fire the watchdog — simulates ActivityTimeout of silence.
	timer.Fire()

	// Wait for the session to reach StateError (the DB write) before checking
	// the notification, so we know handleActivityTimeout ran to completion.
	if got := waitForState(t, bus.DB, sc.cfg.SessionName, string(agent.StateError), 2*time.Second); got != string(agent.StateError) {
		t.Fatalf("state after watchdog fire: got %q, want %q", got, agent.StateError)
	}

	// WaitNotifies() blocks until every goroutine spawned via goNotify has
	// returned — no sleep or poll required. If the notification was still
	// spawned with a raw `go` this call would return immediately and the
	// body assertion below would race.
	sc.WaitNotifies()

	// The parent worker must have received a notification containing the
	// inactivity-timeout reason.
	bodies := bus.CopyBodies()
	if len(bodies) == 0 {
		t.Fatal("no notification delivered to parent worker after inactivity watchdog fired")
	}
	found := false
	for _, b := range bodies {
		if strings.Contains(b, "inactivity timeout") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("notification body does not contain \"inactivity timeout\"; bodies: %v", bodies)
	}

	conn.Close()
	_ = wait()
}
