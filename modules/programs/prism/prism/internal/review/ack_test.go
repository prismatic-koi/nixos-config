package review_test

// Tests for the AsyncResult.Ack format (#1051 AC-5).
//
// The Ack is the worker's first piece of evidence about which agents came up
// successfully and which did not. The headline scan target is the
// "Spawned: N, Failed: M (...)" line — operators eyeballing prism review
// output need to see partial-success outcomes immediately, not 20 minutes
// later when the monitor times out.

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/review"
)

// TestBuildAsyncAck_AllReady_NoFailures verifies the Ack format when every
// agent comes up healthy. The "Failed: 0" line is always emitted (stable
// format across runs).
func TestBuildAsyncAck_AllReady_NoFailures(t *testing.T) {
	sessions := []string{
		"test@feat~review-1-review-goal",
		"test@feat~review-1-review-code",
		"test@feat~review-1-review-security",
		"test@feat~review-1-review-qa",
		"test@feat~review-1-review-context",
	}
	got := review.BuildAsyncAckForTest("1234", 1, "group-abc", sessions, nil, "test@feat")

	// Headline summary line.
	if !strings.Contains(got, "Spawned: 5, Failed: 0") {
		t.Errorf("Ack missing 'Spawned: 5, Failed: 0' headline: %s", got)
	}
	// All sessions should appear in the bullet list.
	for _, s := range sessions {
		if !strings.Contains(got, s) {
			t.Errorf("Ack missing session %q: %s", s, got)
		}
	}
	// No failure block.
	if strings.Contains(got, "agent(s) failed to start") {
		t.Errorf("Ack contains 'failed to start' block but no failures occurred: %s", got)
	}
	// Worker session reference for monitoring.
	if !strings.Contains(got, "test@feat") {
		t.Errorf("Ack missing worker session reference: %s", got)
	}
}

// TestBuildAsyncAck_PartialSuccess_SurfacesFailures verifies the AC-5
// example text exactly: "Spawned: 3, Failed: 2 (review-goal: not ready
// within 30s, review-qa: not ready within 30s)".
func TestBuildAsyncAck_PartialSuccess_SurfacesFailures(t *testing.T) {
	sessions := []string{
		"test@feat~review-1-review-code",
		"test@feat~review-1-review-security",
		"test@feat~review-1-review-context",
	}
	failures := [][2]string{
		{"review-goal", "not ready within 30s"},
		{"review-qa", "not ready within 30s"},
	}
	got := review.BuildAsyncAckForTest("1234", 1, "group-abc", sessions, failures, "test@feat")

	// AC-5: the exact headline format.
	want := "Spawned: 3, Failed: 2 (review-goal: not ready within 30s, review-qa: not ready within 30s)"
	if !strings.Contains(got, want) {
		t.Errorf("Ack missing expected headline %q\nfull Ack:\n%s", want, got)
	}

	// The failure block should mention the per-session startup log.
	if !strings.Contains(got, "prism logs") || !strings.Contains(got, "--startup") {
		t.Errorf("Ack failure block must mention `prism logs <session> --startup`: %s", got)
	}

	// Successful sessions still appear in the bullet list.
	for _, s := range sessions {
		if !strings.Contains(got, s) {
			t.Errorf("Ack missing successful session %q: %s", s, got)
		}
	}

	// Failed agents must appear in the failure list.
	for _, f := range failures {
		if !strings.Contains(got, f[0]) {
			t.Errorf("Ack missing failed agent %q: %s", f[0], got)
		}
	}
}

// TestBuildAsyncAck_AllFailed_NoSessions verifies the edge case where every
// agent failed to come up. The Ack should still render coherently — no
// stray "monitor progress with" line for an empty session list.
func TestBuildAsyncAck_AllFailed_NoSessions(t *testing.T) {
	failures := [][2]string{
		{"review-goal", "not ready within 30s"},
		{"review-code", "config error: stale profiles.json"},
	}
	got := review.BuildAsyncAckForTest("1234", 1, "group-x", nil, failures, "test@feat")

	if !strings.Contains(got, "Spawned: 0, Failed: 2") {
		t.Errorf("Ack missing 'Spawned: 0, Failed: 2': %s", got)
	}
	// No "prism checkin <…>~review-1-review-goal" line — it would be
	// pointing at a session that never came up.
	if strings.Contains(got, "prism checkin") {
		t.Errorf("Ack has 'prism checkin' suggestion but no live sessions: %s", got)
	}
	// Different failure reasons should both appear.
	if !strings.Contains(got, "review-goal") || !strings.Contains(got, "not ready within 30s") {
		t.Errorf("Ack missing review-goal failure: %s", got)
	}
	if !strings.Contains(got, "review-code") || !strings.Contains(got, "stale profiles.json") {
		t.Errorf("Ack missing review-code failure with config error reason: %s", got)
	}
}

// TestBuildAsyncAck_PreservesOrder verifies that failures appear in the
// order they are passed in — matching the spawn loop's original agent order
// rather than alphabetical or insertion-sorted order. This makes the
// headline string deterministic for grep / scanning.
func TestBuildAsyncAck_PreservesOrder(t *testing.T) {
	failures := [][2]string{
		{"review-context", "not ready within 30s"},
		{"review-goal", "not ready within 30s"},
		{"review-qa", "not ready within 30s"},
	}
	got := review.BuildAsyncAckForTest("1234", 1, "g", nil, failures, "test@feat")

	// Find the headline; verify ordering of agent names within it.
	idxContext := strings.Index(got, "review-context")
	idxGoal := strings.Index(got, "review-goal")
	idxQa := strings.Index(got, "review-qa")
	if idxContext < 0 || idxGoal < 0 || idxQa < 0 {
		t.Fatalf("Ack missing expected agent names: %s", got)
	}
	if !(idxContext < idxGoal && idxGoal < idxQa) {
		t.Errorf("Ack reordered failures (context=%d goal=%d qa=%d): %s",
			idxContext, idxGoal, idxQa, got)
	}
}
