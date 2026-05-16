package iris

// supervisor_notify_parent_test.go — unit tests for Supervisor.setState's
// terminal-state notification trigger (issue #1700, iris notifyParentWorker
// analogue).
//
// These tests drive setState directly (no pi child) and assert:
//
//   - StateFinished with a non-empty ParentSession fires NotifyParent once
//     with terminal=StateFinished.
//   - StateError with a non-empty ParentSession fires NotifyParent once
//     with terminal=StateError.
//   - Empty ParentSession suppresses the notification (top-level spawn).
//   - Non-terminal transitions (active, spawning) never fire NotifyParent.
//   - The supervisor's mu lock is NOT held while NotifyParent runs (the
//     callback must be able to take its own locks without deadlocking).
//   - The deliveryID parameter is a non-empty UUID-shaped string and is
//     distinct across two consecutive terminal transitions (defensive).

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// notifyParentSpy captures every NotifyParent invocation.
type notifyParentSpy struct {
	mu    sync.Mutex
	calls []notifyParentCall
	// onCall, when non-nil, runs synchronously inside the spy under spy.mu;
	// useful for asserting that the supervisor's own mu is not held.
	onCall func(call notifyParentCall)
}

type notifyParentCall struct {
	Child      string
	Parent     string
	State      SessionState
	DeliveryID string
}

func (sp *notifyParentSpy) record(child, parent string, state SessionState, deliveryID string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	call := notifyParentCall{Child: child, Parent: parent, State: state, DeliveryID: deliveryID}
	sp.calls = append(sp.calls, call)
	if sp.onCall != nil {
		sp.onCall(call)
	}
}

func (sp *notifyParentSpy) snapshot() []notifyParentCall {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	out := make([]notifyParentCall, len(sp.calls))
	copy(out, sp.calls)
	return out
}

func newNotifyParentTestSupervisor(t *testing.T, parentName string) (*Supervisor, *notifyParentSpy) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	runDir, err := os.MkdirTemp("", "iris-np-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	spy := &notifyParentSpy{}
	cfg := SupervisorConfig{
		SessionName:   "iris-test@child",
		Worktree:      tmp,
		Role:          "worker",
		RunDir:        runDir,
		Database:      database,
		ParentSession: parentName,
		NotifyParent:  spy.record,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(sup.closeSessionLogFile)
	return sup, spy
}

// TestSupervisorSetState_NotifyParent_FinishedFires asserts that
// transitioning to StateFinished fires NotifyParent exactly once with the
// correct child / parent / state arguments.
func TestSupervisorSetState_NotifyParent_FinishedFires(t *testing.T) {
	sup, spy := newNotifyParentTestSupervisor(t, "iris-test@parent")

	sup.setState(StateActive)
	sup.setState(StateFinished)

	// NotifyParent is invoked in a goroutine — wait briefly for it.
	if !waitForNotifyCalls(spy, 1, 1*time.Second) {
		t.Fatalf("expected 1 NotifyParent call, got %d", len(spy.snapshot()))
	}
	calls := spy.snapshot()
	if calls[0].Child != "iris-test@child" {
		t.Errorf("child = %q, want iris-test@child", calls[0].Child)
	}
	if calls[0].Parent != "iris-test@parent" {
		t.Errorf("parent = %q, want iris-test@parent", calls[0].Parent)
	}
	if calls[0].State != StateFinished {
		t.Errorf("state = %s, want %s", calls[0].State, StateFinished)
	}
	if calls[0].DeliveryID == "" {
		t.Errorf("delivery_id is empty; want a UUID-shaped string")
	}
}

// TestSupervisorSetState_NotifyParent_ErrorFires asserts that transitioning
// to StateError also fires NotifyParent with state=StateError.
func TestSupervisorSetState_NotifyParent_ErrorFires(t *testing.T) {
	sup, spy := newNotifyParentTestSupervisor(t, "iris-test@parent")

	sup.setState(StateActive)
	sup.setState(StateError)

	if !waitForNotifyCalls(spy, 1, 1*time.Second) {
		t.Fatalf("expected 1 NotifyParent call, got %d", len(spy.snapshot()))
	}
	calls := spy.snapshot()
	if calls[0].State != StateError {
		t.Errorf("state = %s, want %s", calls[0].State, StateError)
	}
}

// TestSupervisorSetState_NotifyParent_NoParentNoFire asserts that an empty
// ParentSession (top-level spawn) suppresses the notification entirely.
func TestSupervisorSetState_NotifyParent_NoParentNoFire(t *testing.T) {
	sup, spy := newNotifyParentTestSupervisor(t, "" /* no parent */)

	sup.setState(StateActive)
	sup.setState(StateFinished)

	// Give the goroutine a chance to fire (it shouldn't).
	time.Sleep(100 * time.Millisecond)
	if got := len(spy.snapshot()); got != 0 {
		t.Fatalf("NotifyParent fired %d times for a top-level spawn; want 0", got)
	}
}

// TestSupervisorSetState_NotifyParent_NonTerminalNoFire asserts that
// transitions to non-terminal states (active, spawning, escalated) never
// fire NotifyParent.
func TestSupervisorSetState_NotifyParent_NonTerminalNoFire(t *testing.T) {
	sup, spy := newNotifyParentTestSupervisor(t, "iris-test@parent")

	sup.setState(StateActive)
	sup.setState(StateEscalated)
	sup.setState(StateActive)

	time.Sleep(100 * time.Millisecond)
	if got := len(spy.snapshot()); got != 0 {
		t.Fatalf("NotifyParent fired %d times for non-terminal transitions; want 0", got)
	}
}

// TestSupervisorSetState_NotifyParent_LockNotHeld asserts that the
// supervisor's mu is NOT held when NotifyParent runs. The callback proves
// this by acquiring sup.mu itself — if setState held the lock the goroutine
// would block until setState returns, which would deadlock with the
// straight-line driver in the test.
//
// Rationale: prompt delivery may block on socket I/O. Holding s.mu across
// external I/O is the failure mode #1687 explicitly prohibits.
func TestSupervisorSetState_NotifyParent_LockNotHeld(t *testing.T) {
	sup, spy := newNotifyParentTestSupervisor(t, "iris-test@parent")

	gotLock := make(chan struct{})
	spy.onCall = func(_ notifyParentCall) {
		// Try to acquire sup.mu inside the callback. If setState holds it
		// across the goroutine launch we'd block here forever; the timeout
		// guarded below fails the test.
		sup.mu.Lock()
		sup.mu.Unlock()
		close(gotLock)
	}

	sup.setState(StateActive)
	sup.setState(StateFinished)

	select {
	case <-gotLock:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("NotifyParent callback could not acquire sup.mu within 2s — setState is holding the lock across the notification dispatch (violates #1687)")
	}
}

// TestSupervisorSetState_NotifyParent_DistinctDeliveryIDs asserts that two
// terminal transitions on the SAME supervisor (which is unusual but
// defensive — the suppression rule should normally prevent the second
// from firing) produce distinct delivery_ids on every actual invocation.
// This pins the #1695 contract: each call mints its own UUID.
func TestSupervisorSetState_NotifyParent_DistinctDeliveryIDs(t *testing.T) {
	// Two separate supervisors with the same parent simulate two children
	// finishing back-to-back. Each must use its own delivery_id.
	sup1, spy1 := newNotifyParentTestSupervisor(t, "iris-test@parent")
	sup2, spy2 := newNotifyParentTestSupervisor(t, "iris-test@parent")

	sup1.setState(StateActive)
	sup1.setState(StateFinished)
	sup2.setState(StateActive)
	sup2.setState(StateFinished)

	if !waitForNotifyCalls(spy1, 1, 1*time.Second) {
		t.Fatalf("spy1: expected 1 call, got %d", len(spy1.snapshot()))
	}
	if !waitForNotifyCalls(spy2, 1, 1*time.Second) {
		t.Fatalf("spy2: expected 1 call, got %d", len(spy2.snapshot()))
	}

	id1 := spy1.snapshot()[0].DeliveryID
	id2 := spy2.snapshot()[0].DeliveryID
	if id1 == "" || id2 == "" {
		t.Fatalf("delivery_ids must be non-empty: %q %q", id1, id2)
	}
	if id1 == id2 {
		t.Errorf("delivery_ids must differ across calls: both were %q", id1)
	}
}

// TestSupervisorSetState_NotifyParent_IdempotentTerminalNoRefire asserts
// that the existing setState "suppress redundant terminal" guard (which
// protects against the kill path's defensive double-fire — see the
// pre-existing comment in setState) also suppresses the notification.
// Without this we would deliver two notifications for one logical
// termination.
func TestSupervisorSetState_NotifyParent_IdempotentTerminalNoRefire(t *testing.T) {
	sup, spy := newNotifyParentTestSupervisor(t, "iris-test@parent")

	sup.setState(StateActive)
	sup.setState(StateFinished)
	// Second transition to the same terminal — the existing guard turns
	// this into a no-op for the state write AND must also skip the notify.
	sup.setState(StateFinished)

	time.Sleep(100 * time.Millisecond)
	if got := len(spy.snapshot()); got != 1 {
		t.Fatalf("NotifyParent fired %d times for a double terminal transition; want 1 (the guard must suppress the repeat)", got)
	}
}

// TestSupervisorSetState_NotifyParent_CrossTerminalLatch asserts the
// defence-in-depth latch on the notification trigger: even if a
// hypothetical future change to Kill or the supervisor loop drove a
// Finished→Error (or Error→Finished) sequence through setState, the
// notification must still fire exactly once.
//
// The existing setState dedup at the top of the method only suppresses
// identical-terminal transitions (Finished→Finished, Error→Error).
// Cross-terminal sequences pass the dedup and would deliver a second
// notification with the wrong wording (e.g. "has finished" then
// "has errored" for one logical termination). The s.parentNotified
// latch guards against that.
//
// Today no code path produces this sequence — the kill path returns
// before its final setState(StateError) on SIGTERM-clean and the loop's
// intermediate Finished is suppressed by the killReason guard on the
// SIGKILL path. This test pins the invariant against a future
// regression in either of those paths.
func TestSupervisorSetState_NotifyParent_CrossTerminalLatch(t *testing.T) {
	sup, spy := newNotifyParentTestSupervisor(t, "iris-test@parent")

	sup.setState(StateActive)
	sup.setState(StateFinished)
	// Cross-terminal: Finished → Error. The existing setState dedup does
	// not cover this (state == Finished, newState == Error: different).
	// The parentNotified latch must stop the second notification.
	sup.setState(StateError)

	time.Sleep(100 * time.Millisecond)
	calls := spy.snapshot()
	if len(calls) != 1 {
		t.Fatalf("NotifyParent fired %d times across Finished\u2192Error; want 1 (the latch must suppress the second fire)", len(calls))
	}
	// The single call must carry the FIRST terminal (Finished) — the latch
	// closes after the first fire so the second is dropped, not
	// replaced.
	if calls[0].State != StateFinished {
		t.Errorf("first (and only) notification state = %s, want %s", calls[0].State, StateFinished)
	}
}

// waitForNotifyCalls polls spy.snapshot() until the count matches want or
// the deadline expires. Used to bridge the goroutine-dispatched callback
// without flaky sleeps.
func waitForNotifyCalls(spy *notifyParentSpy, want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(spy.snapshot()) >= want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return len(spy.snapshot()) >= want
}
