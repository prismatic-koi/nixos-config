package iris

// supervisor_kill_notify_test.go — kill-path notification integration
// tests for issue #1700. The unit tests in
// supervisor_notify_parent_test.go drive setState directly; here we drive
// Supervisor.Kill against a real child process and assert that exactly
// one parent-notification fires across the SIGTERM-clean kill flow.
//
// This is the regression guard against the kill-path double-fire concern
// raised in review-context's blocking review (round 1): without the
// s.parentNotified latch and the existing Kill-returns-early-on-<-s.done
// behaviour, a single Kill could conceivably drive Finished→Error through
// setState and produce two notifications with mismatched wording.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// killNotifyTestSupervisor is a copy of killTestSupervisor parameterised
// to wire a parent + NotifyParent recorder. We do not refactor
// killTestSupervisor itself because other kill-path tests depend on its
// exact shape.
func killNotifyTestSupervisor(t *testing.T, piBin, parent string) (*Supervisor, *killNotifyRecorder) {
	t.Helper()
	shortPrefix, err := os.MkdirTemp("", "iris-kill-notify-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })

	dbPath := filepath.Join(shortPrefix, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	rec := &killNotifyRecorder{}

	cfg := SupervisorConfig{
		SessionName:     "iris-kill-notify@test",
		Worktree:        shortPrefix,
		Role:            "worker",
		ParentSession:   parent,
		PIBinaryPath:    piBin,
		RunDir:          shortPrefix,
		LogDir:          filepath.Join(shortPrefix, "logs"),
		Database:        database,
		ShutdownTimeout: 100 * time.Millisecond,
		NotifyParent:    rec.record,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sup.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sup.State() == StateActive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sup.State() != StateActive {
		t.Fatalf("supervisor never reached StateActive (got %s)", sup.State())
	}
	return sup, rec
}

// killNotifyRecorder is a thread-safe recorder for NotifyParent calls
// scoped to a single kill-path integration test. Mirrors notifyParentSpy
// in supervisor_notify_parent_test.go but kept separate so each test
// file owns its own helper and naming.
type killNotifyRecorder struct {
	mu    sync.Mutex
	calls []killNotifyCall
}

type killNotifyCall struct {
	Child  string
	Parent string
	State  SessionState
}

func (r *killNotifyRecorder) record(child, parent string, state SessionState, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, killNotifyCall{Child: child, Parent: parent, State: state})
}

func (r *killNotifyRecorder) snapshot() []killNotifyCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]killNotifyCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestSupervisorKill_CleanSIGTERM_NotifiesParentOnce asserts that a
// SIGTERM-clean Kill (the happy path: /bin/sleep responds to SIGTERM and
// exits 0) fires the parent-notification exactly ONCE, with state =
// StateFinished and wording matching "has finished".
//
// This is the precise behaviour AC #1700 calls "exactly-once delivery …
// even if the terminal-state transition fires twice". Without correct
// dedup in setState, the loop's ctx-cancel branch and Kill's final
// setState(StateError) could both fire and produce two notifications
// with mismatched bodies ("has finished" then "has errored"). By
// inspection of Kill, the SIGTERM-clean path returns at the
// case <-s.done branch BEFORE reaching the final setState(StateError),
// so today only Finished fires — and the parentNotified latch is the
// defence-in-depth against any future regression.
func TestSupervisorKill_CleanSIGTERM_NotifiesParentOnce(t *testing.T) {
	script := writeShellScript(t, "exec sleep 60\n", "")
	sup, rec := killNotifyTestSupervisor(t, script, "iris-test@parent")

	state, err := sup.Kill(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if state != StateFinished {
		t.Fatalf("terminal state = %s, want %s (SIGTERM-clean)", state, StateFinished)
	}

	// Wait briefly for the NotifyParent goroutine to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.snapshot()) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// And then a little more, so a SECOND erroneous fire would also be
	// observed by the time we check.
	time.Sleep(200 * time.Millisecond)

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("NotifyParent fired %d times on SIGTERM-clean kill; want exactly 1 (states observed: %+v)", len(calls), calls)
	}
	if calls[0].State != StateFinished {
		t.Errorf("single notification state = %s, want %s (clean SIGTERM must produce 'has finished', not 'has errored')", calls[0].State, StateFinished)
	}
	if calls[0].Parent != "iris-test@parent" {
		t.Errorf("notification parent = %q, want iris-test@parent", calls[0].Parent)
	}
}

// TestSupervisorKill_SIGKILLEscalation_NotifiesParentOnce asserts that
// the SIGKILL escalation path (pi ignores SIGTERM, Kill escalates after
// the grace period) fires exactly one notification with state = StateError.
// This is the symmetric case to the clean-SIGTERM test above: the loop's
// intermediate setState(StateFinished) must be suppressed by the
// killReason guard so the only terminal that reaches the trigger is
// Kill's final setState(StateError).
func TestSupervisorKill_SIGKILLEscalation_NotifiesParentOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("SIGKILL escalation sleeps ~1s; skipping in short mode")
	}
	// trap '' TERM ignores SIGTERM; SIGKILL still reaps the shell.
	//
	// The @READY@ marker + waitForReady handshake guarantees the trap is
	// armed before Kill fires SIGTERM. Without it the test was ~10% flaky
	// under -race (#1739): Supervisor.setState(StateActive) fires the
	// moment cmd.Start returns, i.e. while /bin/sh is still parsing the
	// script, so the test could send SIGTERM before the trap was
	// installed and bash would exit 143 cleanly within the grace.
	readyPath := newReadyPath(t)
	script := writeShellScript(t, "trap '' TERM\n@READY@\nsleep 60\n", readyPath)
	sup, rec := killNotifyTestSupervisor(t, script, "iris-test@parent")
	waitForReady(t, readyPath, 5*time.Second)

	state, err := sup.Kill(context.Background(), 1*time.Second)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if state != StateError {
		t.Fatalf("terminal state = %s, want %s (SIGKILL escalation)", state, StateError)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.snapshot()) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("NotifyParent fired %d times on SIGKILL kill; want exactly 1 (states observed: %+v)", len(calls), calls)
	}
	if calls[0].State != StateError {
		t.Errorf("single notification state = %s, want %s", calls[0].State, StateError)
	}
}
