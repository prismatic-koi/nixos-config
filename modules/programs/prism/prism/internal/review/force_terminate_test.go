package review_test

// force_terminate_test.go — coverage for forceTerminateStuckMembers (#1709).
//
// When the monitor's outer safety timeout fires, any review-agent row still
// in a non-terminal state must be force-transitioned to "error" so that
// GroupCompleted returns true on subsequent calls and a future
// `prism review` for the same parent can run instead of being refused with
// "round N already in progress".

import (
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/review"
)

// TestForceTerminateStuckMembers_TransitionsActiveRows verifies the core
// invariant: an active row is rewritten to "error" by the sweep.
func TestForceTerminateStuckMembers_TransitionsActiveRows(t *testing.T) {
	d := openTestDB(t)
	stuck := "prism-test@invoker-force-terminate~review-1-review-goal"
	if err := d.UpsertStatus(stuck, "testrepo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	review.ForceTerminateStuckMembersForTest(d, []string{stuck}, 10*time.Minute)

	status, err := d.CurrentStatus(stuck)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("session row disappeared after sweep")
	}
	if status.State != "error" {
		t.Errorf("state after sweep: got %q, want %q", status.State, "error")
	}
}

// TestForceTerminateStuckMembers_LeavesTerminalRowsAlone verifies the sweep
// is state-idempotent: rows already in a terminal state must not be
// overwritten. Without this guard the sweep could clobber a real `finished`
// verdict if it raced a slow GroupCompleted reload.
func TestForceTerminateStuckMembers_LeavesTerminalRowsAlone(t *testing.T) {
	d := openTestDB(t)
	terminalStates := []string{"finished", "error", "interrupted", "deleted"}
	sessions := make([]string, 0, len(terminalStates))
	for _, state := range terminalStates {
		sess := "prism-test@invoker-force-terminate-terminal~review-1-review-" + state
		if err := d.UpsertStatus(sess, "testrepo", "/wt", state, nil, nil); err != nil {
			t.Fatalf("seed %q: %v", sess, err)
		}
		sessions = append(sessions, sess)
	}

	review.ForceTerminateStuckMembersForTest(d, sessions, 10*time.Minute)

	for i, sess := range sessions {
		status, err := d.CurrentStatus(sess)
		if err != nil {
			t.Fatalf("CurrentStatus(%q): %v", sess, err)
		}
		if status == nil {
			t.Fatalf("session %q disappeared", sess)
		}
		if status.State != terminalStates[i] {
			t.Errorf("session %q state: got %q, want %q (sweep clobbered terminal state)",
				sess, status.State, terminalStates[i])
		}
	}
}

// TestForceTerminateStuckMembers_SkipsMissingRows verifies that a non-existent
// session name in the input list is silently skipped rather than causing the
// sweep to fail. Missing rows correspond to agents whose `prism cleanup` ran
// concurrently with the monitor's safety timeout.
func TestForceTerminateStuckMembers_SkipsMissingRows(t *testing.T) {
	d := openTestDB(t)
	live := "prism-test@invoker-force-terminate-mixed~review-1-review-goal"
	if err := d.UpsertStatus(live, "testrepo", "/wt", "active", nil, nil); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	// Mix of missing (never seeded), empty, and live sessions.
	review.ForceTerminateStuckMembersForTest(d,
		[]string{"", "prism-test@invoker-does-not-exist", live},
		10*time.Minute)

	status, err := d.CurrentStatus(live)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil || status.State != "error" {
		gotState := "<nil>"
		if status != nil {
			gotState = status.State
		}
		t.Errorf("live session state after mixed-input sweep: got %q, want error", gotState)
	}
}

// TestForceTerminateStuckMembers_LeavesEndedRowsAlone verifies that rows with
// ended_at set (closed by `prism cleanup` or `prism reset`) are skipped. Such
// rows already count as terminal for GroupCompleted's ended_at arm, so a
// state rewrite would be wasted work and could mask the true terminal cause
// from forensic inspection.
func TestForceTerminateStuckMembers_LeavesEndedRowsAlone(t *testing.T) {
	d := openTestDB(t)
	ended := "prism-test@invoker-force-terminate-ended~review-1-review-goal"
	// Seed in a non-terminal state then SetEnded to simulate `prism cleanup`'s
	// state="interrupted" + ended_at write.
	if err := d.UpsertStatus(ended, "testrepo", "/wt", "interrupted", nil, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := d.SetEnded(ended); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	review.ForceTerminateStuckMembersForTest(d, []string{ended}, 10*time.Minute)

	status, err := d.CurrentStatus(ended)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("session row disappeared")
	}
	if status.State != "interrupted" {
		t.Errorf("state after sweep on ended row: got %q, want %q (sweep clobbered an ended-row's state)",
			status.State, "interrupted")
	}
	if status.EndedAt == nil {
		t.Error("ended_at was cleared by sweep — must be preserved")
	}
}
