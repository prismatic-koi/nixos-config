package iris

// supervisor_state_change_test.go — tests for the supervisor's
// handleStateChange callback wired in issue #1701. Validates:
//
//   - handleStateChange("waiting") drives setState(StateWaiting) and
//     publishes a PublishState("waiting") event.
//   - handleStateChange("active") drives setState(StateActive) (idempotent
//     when already active).
//   - Transitions across waiting↔active fire PublishState each time.
//   - Unknown wire states are ignored (forward-compatible).
//   - handleStateChange is a no-op when the supervisor is in a terminal
//     state (the extension should never drive us out of finished/error).
//
// The full end-to-end (extension → harness socket → supervisor → DB +
// PublishState) is covered by harness_state_change_test.go and the higher-
// level integration tests; these unit tests pin the supervisor-side
// semantics so future refactors of setState do not silently drop the
// waiting transition.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

func newStateChangeSupervisor(t *testing.T) (*Supervisor, *statePublisherSpy, *db.DB) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	spy := &statePublisherSpy{}
	runDir, err := os.MkdirTemp("", "iris-sc-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	cfg := SupervisorConfig{
		SessionName: "iris-test@state-change",
		Worktree:    tmp,
		Role:        "worker",
		RunDir:      runDir,
		LogDir:      filepath.Join(tmp, "logs"),
		Database:    database,
		Publisher:   spy,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(sup.closeSessionLogFile)
	return sup, spy, database
}

// waitForPublications polls the spy until it has at least n events or the
// deadline expires. Returns the snapshot.
func waitForPublications(t *testing.T, spy *statePublisherSpy, n int, timeout time.Duration) []stateEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := spy.snapshot()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return spy.snapshot()
}

// TestHandleStateChange_Waiting verifies the canonical issue #1701 path: an
// extension state_change="waiting" frame results in StateWaiting being set
// and PublishState being called.
func TestHandleStateChange_Waiting(t *testing.T) {
	sup, spy, database := newStateChangeSupervisor(t)

	// The supervisor starts in StateSpawning. A state_change before active
	// is unusual but should still produce the transition — sandbox time so
	// far as the wire spec is concerned (no ordering assumed).
	sup.handleStateChange("waiting")

	if got := sup.State(); got != StateWaiting {
		t.Fatalf("State() = %q, want %q", got, StateWaiting)
	}

	got := waitForPublications(t, spy, 1, time.Second)
	if len(got) != 1 || got[0].State != string(StateWaiting) {
		t.Fatalf("expected PublishState(\"waiting\") once, got %v", got)
	}

	// sessions.iris_state in the DB should also be "waiting".
	sessions, err := database.IrisSessionsToRestore()
	if err != nil {
		t.Fatalf("IrisSessionsToRestore: %v", err)
	}
	var gotState string
	for _, s := range sessions {
		if s.InstanceID == sup.InstanceID() {
			gotState = s.IrisState
			break
		}
	}
	if gotState != string(StateWaiting) {
		t.Errorf("DB iris_state = %q, want %q", gotState, StateWaiting)
	}
}

// TestHandleStateChange_WaitingThenActive verifies the round-trip: paused →
// next prompt arrives → active. Each transition must call PublishState.
func TestHandleStateChange_WaitingThenActive(t *testing.T) {
	sup, spy, _ := newStateChangeSupervisor(t)

	sup.handleStateChange("waiting")
	sup.handleStateChange("active")

	if got := sup.State(); got != StateActive {
		t.Fatalf("State() = %q, want %q", got, StateActive)
	}

	got := waitForPublications(t, spy, 2, time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 publications, got %d: %v", len(got), got)
	}
	if got[0].State != string(StateWaiting) {
		t.Errorf("first publication = %q, want %q", got[0].State, StateWaiting)
	}
	if got[1].State != string(StateActive) {
		t.Errorf("second publication = %q, want %q", got[1].State, StateActive)
	}
}

// TestHandleStateChange_IdempotentWaiting verifies that a repeated
// state_change="waiting" frame does not produce a duplicate PublishState
// (avoids spamming subscribers when pi re-emits at every paused turn).
func TestHandleStateChange_IdempotentWaiting(t *testing.T) {
	sup, spy, _ := newStateChangeSupervisor(t)

	sup.handleStateChange("waiting")
	sup.handleStateChange("waiting") // repeat
	sup.handleStateChange("waiting") // repeat

	got := waitForPublications(t, spy, 1, time.Second)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 publication for repeated waiting, got %d: %v", len(got), got)
	}
}

// TestHandleStateChange_UnknownStateIgnored verifies forward compatibility:
// unknown wire-state strings (future extensions) are logged and ignored
// rather than crashing or mis-typing the in-memory state.
func TestHandleStateChange_UnknownStateIgnored(t *testing.T) {
	sup, spy, _ := newStateChangeSupervisor(t)
	pre := sup.State()

	sup.handleStateChange("compacting") // not driven through this path
	sup.handleStateChange("totally_made_up_state")

	if got := sup.State(); got != pre {
		t.Errorf("State() changed to %q after unknown wire states, want %q (unchanged)", got, pre)
	}
	got := spy.snapshot()
	if len(got) != 0 {
		t.Errorf("expected zero publications for unknown states, got %v", got)
	}
}

// TestHandleStateChange_FinishedIsNotDriven verifies that the terminal
// "finished" wire state from the extension is NOT propagated to setState
// via handleStateChange — terminal transitions are owned by the supervisor
// loop (clean exit, restart-exhaustion, kill). This guards against the
// extension racing the supervisor and double-firing session_end events.
func TestHandleStateChange_FinishedIsNotDriven(t *testing.T) {
	sup, spy, _ := newStateChangeSupervisor(t)
	pre := sup.State()

	sup.handleStateChange("finished")
	sup.handleStateChange("interrupted")

	if got := sup.State(); got != pre {
		t.Errorf("State() = %q, want %q (terminal wire states must not change state)", got, pre)
	}
	if got := spy.snapshot(); len(got) != 0 {
		t.Errorf("expected zero publications, got %v", got)
	}
}

// TestHandleStateChange_TerminalSupervisorIgnores verifies that once the
// supervisor has entered a terminal state, the harness handler cannot drive
// it back to active/waiting. The pi child should be gone, but a late frame
// in flight must not corrupt the terminal record.
func TestHandleStateChange_TerminalSupervisorIgnores(t *testing.T) {
	sup, spy, _ := newStateChangeSupervisor(t)

	// Drive to terminal.
	sup.setState(StateFinished)
	// Reset the spy so we only count post-terminal events.
	spy.mu.Lock()
	spy.states = nil
	spy.mu.Unlock()

	// Late state_change frames must be ignored.
	sup.handleStateChange("waiting")
	sup.handleStateChange("active")

	if got := sup.State(); got != StateFinished {
		t.Errorf("State() = %q, want %q (terminal must be sticky)", got, StateFinished)
	}
	if got := spy.snapshot(); len(got) != 0 {
		t.Errorf("expected zero post-terminal publications, got %v", got)
	}
}

