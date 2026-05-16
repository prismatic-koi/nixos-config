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
	"testing"
	"time"

	pih "github.com/prismatic-koi/prism/internal/harness/pi"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/config"
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
