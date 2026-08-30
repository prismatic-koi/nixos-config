package cmd

// escalate_dead_pipe_states_test.go — regression tests for the dead-pipe
// escalate-state transitions.
//
// `prism escalate` must transition the calling session to `escalated` from
// `idle` and `error`, not only from `active`. A session whose harness pipe
// never delivered a turn event sits at `idle` (no turn events flowing) or
// `error` (startup-handshake timeout stamped it) while pi is mid-turn. From
// those states `prism escalate` delivers the message to the coordinator, but a
// state write restricted to `active` logs "invalid transition" and (if
// checkTransition were tightened) is silently skipped.
//
// The sidecar's finish-notification suppression, the escalated-state indicator
// in `prism sessions list`, and the escalate-marker audit event all rely on
// the DB state being `escalated`. When the write is dropped, the coordinator
// sees a worker in `error` (or later stamped `finished`) with no signal that
// the worker is awaiting guidance.
//
// This file exercises the state-machine transitions: idle → escalated and
// error → escalated must both succeed and land the row in `escalated`.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/agent"
)

// TestEscalate_FromIdleState_TransitionsToEscalated is the primary case:
// "prism escalate invoked by a session whose DB state is `idle` transitions
// the session to `escalated`, delivers the escalation to the coordinator,
// and the session shows `escalated` in prism sessions list."
func TestEscalate_FromIdleState_TransitionsToEscalated(t *testing.T) {
	d := openPromptTestDB(t)
	_ = seedEscalatePair(t, d, "repo@feature", "repo@main")

	// Force the worker into the idle state — this is the dead-pipe symptom
	// the fix addresses (no turn events flowing from a headless pi).
	// UpsertStatus bypasses UpsertStatusIfNotTerminal's active-only guard.
	if err := d.UpsertStatus("repo@feature", "repo", "/tmp/wt", string(agent.StateIdle), nil, nil); err != nil {
		t.Fatalf("force worker to idle: %v", err)
	}
	pre, err := d.CurrentStatus("repo@feature")
	if err != nil {
		t.Fatalf("read pre-state: %v", err)
	}
	if pre.State != string(agent.StateIdle) {
		t.Fatalf("pre-state = %q, want %q", pre.State, agent.StateIdle)
	}

	_, stderr := captureStdoutStderr(t, func() {
		if err := runEscalateForSessionOpts(d, "repo@feature", "", "please advise",
			escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
			t.Fatalf("runEscalateForSessionOpts: %v", err)
		}
	})

	post, err := d.CurrentStatus("repo@feature")
	if err != nil {
		t.Fatalf("read post-state: %v", err)
	}
	if post.State != string(agent.StateEscalated) {
		t.Fatalf("post-state = %q, want %q (Gap A: idle\u2192escalated must succeed)", post.State, agent.StateEscalated)
	}

	// The transition-table entry for idle→escalated must not fire the
	// advisory "invalid transition" warning. If checkTransition is ever
	// tightened to hard-reject invalid transitions, this assertion doubles
	// as the write-not-skipped signal.
	if strings.Contains(stderr, "invalid transition") && strings.Contains(stderr, `"idle"`) {
		t.Errorf("stderr carried an invalid-transition warning for idle→escalated (Gap A regression): %q", stderr)
	}

	// The escalated-state contract also produces a bus event and a bus
	// message. Both must be present so the coordinator's dashboard /
	// notification path fires.
	if n := eventCount(t, d, "repo@feature", "session.escalated"); n != 1 {
		t.Errorf("session.escalated event count = %d, want 1", n)
	}
	if n := busRowCount(t, d, "repo@feature", "repo@main"); n != 1 {
		t.Errorf("bus_messages count = %d, want 1", n)
	}
}

// TestEscalate_FromErrorState_TransitionsToEscalated is the edge case:
// "prism escalate invoked by a session whose DB state is `error` also
// transitions the session to `escalated`."
func TestEscalate_FromErrorState_TransitionsToEscalated(t *testing.T) {
	d := openPromptTestDB(t)
	_ = seedEscalatePair(t, d, "repo@feature", "repo@main")

	// Force the worker into error — this is the writeStartupError symptom.
	if err := d.UpsertStatus("repo@feature", "repo", "/tmp/wt", string(agent.StateError), nil, nil); err != nil {
		t.Fatalf("force worker to error: %v", err)
	}
	pre, err := d.CurrentStatus("repo@feature")
	if err != nil {
		t.Fatalf("read pre-state: %v", err)
	}
	if pre.State != string(agent.StateError) {
		t.Fatalf("pre-state = %q, want %q", pre.State, agent.StateError)
	}

	_, stderr := captureStdoutStderr(t, func() {
		if err := runEscalateForSessionOpts(d, "repo@feature", "", "please advise",
			escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
			t.Fatalf("runEscalateForSessionOpts: %v", err)
		}
	})

	post, err := d.CurrentStatus("repo@feature")
	if err != nil {
		t.Fatalf("read post-state: %v", err)
	}
	if post.State != string(agent.StateEscalated) {
		t.Fatalf("post-state = %q, want %q (Gap A: error\u2192escalated must succeed)", post.State, agent.StateEscalated)
	}
	if strings.Contains(stderr, "invalid transition") && strings.Contains(stderr, `"error"`) {
		t.Errorf("stderr carried an invalid-transition warning for error→escalated (Gap A regression): %q", stderr)
	}
}

// TestEscalate_FromActive_UnchangedBehaviour verifies the active-state
// path: a session already in `active` transitions to `escalated`.
func TestEscalate_FromActive_UnchangedBehaviour(t *testing.T) {
	d := openPromptTestDB(t)
	_ = seedEscalatePair(t, d, "repo@feature", "repo@main")

	pre, err := d.CurrentStatus("repo@feature")
	if err != nil {
		t.Fatalf("read pre-state: %v", err)
	}
	if pre.State != string(agent.StateActive) {
		t.Fatalf("seedEscalatePair should leave from-session at active; got %q", pre.State)
	}
	if err := runEscalateForSessionOpts(d, "repo@feature", "", "advise pls",
		escalateOptions{dedupWindow: escalateDefaultDedupWindow}); err != nil {
		t.Fatalf("runEscalateForSessionOpts: %v", err)
	}
	post, err := d.CurrentStatus("repo@feature")
	if err != nil {
		t.Fatalf("read post-state: %v", err)
	}
	if post.State != string(agent.StateEscalated) {
		t.Errorf("post-state = %q, want %q", post.State, agent.StateEscalated)
	}
}
