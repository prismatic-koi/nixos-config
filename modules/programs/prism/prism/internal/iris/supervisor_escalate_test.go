package iris

// supervisor_escalate_test.go — unit tests for Supervisor.Escalate /
// Supervisor.Resume (issue #1693). These exercise the state machine
// directly, without spawning pi, so the assertions are deterministic.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newEscalateTestSupervisor builds a Supervisor without invoking Start. The
// returned supervisor has the spy wired as Publisher so the test can assert
// the PublishState calls fired by Escalate/Resume.
func newEscalateTestSupervisor(t *testing.T) (*Supervisor, *statePublisherSpy) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	runDir, err := os.MkdirTemp("", "iris-esc-sup-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	spy := &statePublisherSpy{}
	cfg := SupervisorConfig{
		SessionName: "iris-test@escalate",
		Worktree:    tmp,
		Role:        "worker",
		RunDir:      runDir,
		Database:    database,
		Publisher:   spy,
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(sup.closeSessionLogFile)
	return sup, spy
}

// TestSupervisor_EscalateResumeRoundTrip drives the state machine through
// the documented happy path: active → escalated → active.
func TestSupervisor_EscalateResumeRoundTrip(t *testing.T) {
	sup, spy := newEscalateTestSupervisor(t)

	sup.setState(StateActive)
	if err := sup.Escalate(); err != nil {
		t.Fatalf("Escalate from active: %v", err)
	}
	if got := sup.State(); got != StateEscalated {
		t.Fatalf("after Escalate: state = %s, want %s", got, StateEscalated)
	}

	sup.Resume()
	if got := sup.State(); got != StateActive {
		t.Fatalf("after Resume: state = %s, want %s", got, StateActive)
	}

	// Allow the publisher to flush in case it ever becomes async.
	time.Sleep(20 * time.Millisecond)
	got := spy.snapshot()
	want := []stateEvent{
		{Name: "iris-test@escalate", State: string(StateActive)},
		{Name: "iris-test@escalate", State: string(StateEscalated)},
		{Name: "iris-test@escalate", State: string(StateActive)},
	}
	if len(got) != len(want) {
		t.Fatalf("publications = %d, want %d (got=%+v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("publication[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestSupervisor_EscalateIdempotent asserts that calling Escalate when the
// session is already escalated is a no-op (returns nil, no extra state
// publication).
func TestSupervisor_EscalateIdempotent(t *testing.T) {
	sup, spy := newEscalateTestSupervisor(t)
	sup.setState(StateActive)
	if err := sup.Escalate(); err != nil {
		t.Fatalf("Escalate (first): %v", err)
	}
	before := len(spy.snapshot())
	if err := sup.Escalate(); err != nil {
		t.Fatalf("Escalate (second): %v", err)
	}
	after := len(spy.snapshot())
	if after != before {
		t.Errorf("idempotent Escalate produced extra publication: before=%d after=%d", before, after)
	}
	if got := sup.State(); got != StateEscalated {
		t.Errorf("state = %s, want escalated", got)
	}
}

// TestSupervisor_EscalateRejectsNonActive asserts that Escalate from a
// non-active, non-escalated state (spawning, finished, error) returns an
// error and does NOT mutate state.
func TestSupervisor_EscalateRejectsNonActive(t *testing.T) {
	cases := []struct {
		name  string
		state SessionState
	}{
		{"from_spawning", StateSpawning},
		{"from_finished", StateFinished},
		{"from_error", StateError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sup, _ := newEscalateTestSupervisor(t)
			sup.setState(tc.state)
			err := sup.Escalate()
			if err == nil {
				t.Fatalf("Escalate from %s: want error, got nil", tc.state)
			}
			if got := sup.State(); got != tc.state {
				t.Errorf("state changed from %s to %s on rejected Escalate", tc.state, got)
			}
		})
	}
}

// TestSupervisor_ResumeNonEscalatedIsNoOp asserts that Resume from active,
// spawning, finished, or error states is a no-op (no state change, no extra
// publication). This matters because resumeSession is called from
// handlePromptDeliver on EVERY prompt delivery; non-escalated sessions must
// not see their state flapped.
func TestSupervisor_ResumeNonEscalatedIsNoOp(t *testing.T) {
	cases := []SessionState{StateActive, StateSpawning, StateFinished, StateError}
	for _, st := range cases {
		t.Run(string(st), func(t *testing.T) {
			sup, spy := newEscalateTestSupervisor(t)
			sup.setState(st)
			before := len(spy.snapshot())
			sup.Resume()
			after := len(spy.snapshot())
			if after != before {
				t.Errorf("Resume from %s produced extra publication: before=%d after=%d", st, before, after)
			}
			if got := sup.State(); got != st {
				t.Errorf("Resume from %s changed state to %s", st, got)
			}
		})
	}
}
