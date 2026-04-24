package agent

import (
	"strings"
	"testing"
)

func TestTransition_ValidPairs(t *testing.T) {
	valid := []struct {
		from, to AgentState
		label    string
	}{
		// Normal session lifecycle
		{StateIdle, StateActive, "session.created after tmux-session-start"},
		{StateActive, StateWaiting, "permission.asked / question.asked"},
		{StateWaiting, StateActive, "permission.replied / question.replied"},
		{StateActive, StateFinished, "idle debounce fires"},
		{StateActive, StateCompacting, "compaction started"},
		{StateCompacting, StateActive, "session.compacted — session resumes after compaction"},

		// Error paths
		{StateActive, StateError, "session.error"},
		{StateError, StateActive, "retry / next turn after error"},
		{StateWaiting, StateError, "session.error while waiting"},
		{StateError, StateFinished, "idle debounce after error"},

		// Interruption paths
		{StateActive, StateInterrupted, "pane-died / SIGINT / MessageAbortedError"},
		{StateWaiting, StateInterrupted, "pane-died while waiting for permission"},
		{StateIdle, StateInterrupted, "pane-died before session.created"},
		{StateCompacting, StateInterrupted, "pane-died during compaction"},
		{StateError, StateInterrupted, "pane-died after error"},

		// Container startup failure path (issue #994)
		{StateIdle, StateError, "container startup failure before session.created (WaitHealthy/CreateSession)"},

		// Edge cases from acceptance criteria
		{StateFinished, StateInterrupted, "non-zero exit overrides finished (pane-died)"},
		{StateFinished, StateActive, "session resumed after prior close"},
		{StateFinished, StateIdle, "tmux-session-start resets finished session on recreate"},
		{StateInterrupted, StateActive, "session resumed after interruption"},
		{StateInterrupted, StateIdle, "tmux-session-start resets interrupted session on recreate"},

		// Deleted from any state
		{StateActive, StateDeleted, "session.deleted while active"},
		{StateWaiting, StateDeleted, "session.deleted while waiting"},
		{StateFinished, StateDeleted, "session.deleted while finished"},
		{StateInterrupted, StateDeleted, "session.deleted while interrupted"},
		{StateError, StateDeleted, "session.deleted while error"},
		{StateCompacting, StateDeleted, "session.deleted while compacting"},
		{StateIdle, StateDeleted, "session.deleted while idle (process killed before session.created)"},
	}

	for _, tc := range valid {
		t.Run(tc.label, func(t *testing.T) {
			if err := Transition(tc.from, tc.to); err != nil {
				t.Errorf("Transition(%q, %q) = %v; want nil", tc.from, tc.to, err)
			}
		})
	}
}

func TestTransition_InvalidPairs(t *testing.T) {
	invalid := []struct {
		from, to AgentState
		wantErr  string
	}{
		// Terminal states may not restart unexpectedly
		{StateDeleted, StateActive, "deleted → active"},
		{StateDeleted, StateIdle, "deleted → idle"},
		{StateInterrupted, StateFinished, "interrupted → finished (must go through active)"},

		// Skip states (e.g. idle → waiting, idle → finished)
		{StateIdle, StateWaiting, "idle → waiting (no permission before active)"},
		{StateIdle, StateFinished, "idle → finished (nothing happened)"},
		{StateIdle, StateCompacting, "idle → compacting (nothing happened)"},

		// Compacting resumes to active; finished is no longer a valid direct transition
		{StateCompacting, StateFinished, "compacting → finished (compaction ≠ task completion)"},
		{StateCompacting, StateWaiting, "compacting → waiting"},
		{StateCompacting, StateError, "compacting → error"},
	}

	for _, tc := range invalid {
		t.Run(tc.wantErr, func(t *testing.T) {
			err := Transition(tc.from, tc.to)
			if err == nil {
				t.Errorf("Transition(%q, %q) = nil; want error", tc.from, tc.to)
				return
			}
			// Error message should mention both states.
			if !strings.Contains(err.Error(), string(tc.from)) {
				t.Errorf("Transition(%q, %q) error %q missing from-state", tc.from, tc.to, err)
			}
			if !strings.Contains(err.Error(), string(tc.to)) {
				t.Errorf("Transition(%q, %q) error %q missing to-state", tc.from, tc.to, err)
			}
		})
	}
}

func TestTransition_DeletedIsTerminal(t *testing.T) {
	// deleted is a terminal state — every to-state transition must fail.
	// Iterate a fixed slice (not the map) so we test all known states even if
	// a state were somehow not a key in ValidTransitions.
	allStates := []AgentState{
		StateActive, StateWaiting, StateFinished, StateCompacting,
		StateError, StateIdle, StateInterrupted, StateDeleted,
	}
	for _, to := range allStates {
		if to == StateDeleted {
			continue // skip self-loop
		}
		err := Transition(StateDeleted, to)
		if err == nil {
			t.Errorf("Transition(%q, %q) = nil; want error (deleted is terminal)", StateDeleted, to)
		}
	}
}

func TestTransition_UnknownFromState(t *testing.T) {
	err := Transition("bogus", StateActive)
	if err == nil {
		t.Error("Transition(\"bogus\", StateActive) = nil; want error for unknown from-state")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q does not mention unknown state name", err)
	}
}

// TestValidTransitionsCompleteness ensures every AgentState constant appears
// as a key in ValidTransitions. This catches the case where a new state constant
// is added to agent.go without updating machine.go.
func TestValidTransitionsCompleteness(t *testing.T) {
	allStates := []AgentState{
		StateActive, StateWaiting, StateFinished, StateCompacting,
		StateError, StateIdle, StateInterrupted, StateDeleted,
	}
	for _, s := range allStates {
		if _, ok := ValidTransitions[s]; !ok {
			t.Errorf("state %q is defined in agent.go but missing from ValidTransitions in machine.go", s)
		}
	}
}
