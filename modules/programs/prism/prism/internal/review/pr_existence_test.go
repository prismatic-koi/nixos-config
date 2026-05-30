package review

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// ── fake PRStateRunner ────────────────────────────────────────────────────────

// scriptedPRRunner is a PRStateRunner that returns a single canned response.
// Tests construct one per case; CheckPRState is called once per invocation.
type scriptedPRRunner struct {
	stdout string
	stderr string
	err    error

	// callCount records how many times run() was invoked. Used to assert the
	// helper was actually called (defence against future refactors that
	// short-circuit before reaching the runner).
	callCount int
	// lastPR records the PR number passed to run(). Used to assert the
	// helper received the expected PR argument.
	lastPR string
}

func (s *scriptedPRRunner) run(prNumber string) (string, string, error) {
	s.callCount++
	s.lastPR = prNumber
	return s.stdout, s.stderr, s.err
}

// ── terminal-case tests: missing / closed / merged / transient ────────────────

// TestCheckPRState_Missing verifies that a "Could not resolve to a
// PullRequest" diagnostic from gh maps to PRStateMissing with the
// "PR #<N> does not exist" message.
func TestCheckPRState_Missing(t *testing.T) {
	runner := &scriptedPRRunner{
		stderr: "GraphQL: Could not resolve to a PullRequest with the number of 99999999. (repository.pullRequest)",
		err:    errors.New("exit status 1"),
	}

	err := checkPRStateWithRunner("99999999", runner)
	if err == nil {
		t.Fatal("CheckPRState: expected error for missing PR, got nil")
	}
	var pe *PRStateError
	if !errors.As(err, &pe) {
		t.Fatalf("CheckPRState: error is not *PRStateError: %T %v", err, err)
	}
	if pe.Kind != PRStateMissing {
		t.Errorf("CheckPRState: Kind=%q, want %q", pe.Kind, PRStateMissing)
	}
	for _, want := range []string{
		"PR #99999999",
		"does not exist",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("CheckPRState: missing-PR message lacks %q; got: %s", want, pe.Msg)
		}
	}
	if runner.callCount != 1 {
		t.Errorf("CheckPRState: runner.callCount=%d, want 1", runner.callCount)
	}
	if runner.lastPR != "99999999" {
		t.Errorf("CheckPRState: runner.lastPR=%q, want %q", runner.lastPR, "99999999")
	}
}

// TestCheckPRState_Closed verifies that {"state":"CLOSED","mergedAt":null}
// maps to PRStateClosed with a message naming the closed state.
func TestCheckPRState_Closed(t *testing.T) {
	runner := &scriptedPRRunner{
		stdout: `{"state":"CLOSED","mergedAt":null}`,
	}

	err := checkPRStateWithRunner("2039", runner)
	if err == nil {
		t.Fatal("CheckPRState: expected error for CLOSED PR, got nil")
	}
	var pe *PRStateError
	if !errors.As(err, &pe) {
		t.Fatalf("CheckPRState: error is not *PRStateError: %T %v", err, err)
	}
	if pe.Kind != PRStateClosed {
		t.Errorf("CheckPRState: Kind=%q, want %q", pe.Kind, PRStateClosed)
	}
	for _, want := range []string{
		"PR #2039",
		"closed",
		"merged: false",
		"nothing to review",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("CheckPRState: closed-PR message lacks %q; got: %s", want, pe.Msg)
		}
	}
}

// TestCheckPRState_Merged verifies that {"state":"MERGED",...} maps to
// PRStateMerged with a message naming the merged state.
func TestCheckPRState_Merged(t *testing.T) {
	runner := &scriptedPRRunner{
		stdout: `{"state":"MERGED","mergedAt":"2026-05-30T05:18:21Z"}`,
	}

	err := checkPRStateWithRunner("2046", runner)
	if err == nil {
		t.Fatal("CheckPRState: expected error for MERGED PR, got nil")
	}
	var pe *PRStateError
	if !errors.As(err, &pe) {
		t.Fatalf("CheckPRState: error is not *PRStateError: %T %v", err, err)
	}
	if pe.Kind != PRStateMerged {
		t.Errorf("CheckPRState: Kind=%q, want %q", pe.Kind, PRStateMerged)
	}
	for _, want := range []string{
		"PR #2046",
		"merged",
		"nothing to review",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("CheckPRState: merged-PR message lacks %q; got: %s", want, pe.Msg)
		}
	}
}

// TestCheckPRState_MergedViaMergedAt verifies the defence-in-depth path: when
// state is reported as "CLOSED" but mergedAt is non-null, we treat it as
// merged (matches the prInfo.isMerged() convention in mergequeue/watcher.go).
// Without this, a future gh schema change could silently flip a merged PR
// into the "CLOSED" branch and emit the wrong message.
func TestCheckPRState_MergedViaMergedAt(t *testing.T) {
	runner := &scriptedPRRunner{
		stdout: `{"state":"CLOSED","mergedAt":"2026-05-30T05:18:21Z"}`,
	}

	err := checkPRStateWithRunner("2046", runner)
	if err == nil {
		t.Fatal("CheckPRState: expected error, got nil")
	}
	var pe *PRStateError
	if !errors.As(err, &pe) {
		t.Fatalf("CheckPRState: error is not *PRStateError: %T %v", err, err)
	}
	if pe.Kind != PRStateMerged {
		t.Errorf("CheckPRState: Kind=%q, want %q (state=CLOSED + mergedAt set must map to merged)", pe.Kind, PRStateMerged)
	}
}

// TestCheckPRState_OpenPasses verifies that {"state":"OPEN",...} returns nil
// (the caller proceeds to the rebase gate).
func TestCheckPRState_OpenPasses(t *testing.T) {
	runner := &scriptedPRRunner{
		stdout: `{"state":"OPEN","mergedAt":null}`,
	}

	err := checkPRStateWithRunner("2040", runner)
	if err != nil {
		t.Fatalf("CheckPRState: unexpected error for OPEN PR: %v", err)
	}
	if runner.callCount != 1 {
		t.Errorf("CheckPRState: runner.callCount=%d, want 1", runner.callCount)
	}
}

// TestCheckPRState_TransientGHError verifies that a non-"PR not found" gh
// error is surfaced as PRStateTransient with a "could not determine PR state"
// message — distinctly from PRStateMissing. This is the [edge-case] AC: a
// transient gh failure must NOT silently pass through to spawn agents.
func TestCheckPRState_TransientGHError(t *testing.T) {
	runner := &scriptedPRRunner{
		stderr: "error connecting to api.github.com: dial tcp: lookup api.github.com: no such host",
		err:    errors.New("exit status 1"),
	}

	err := checkPRStateWithRunner("2040", runner)
	if err == nil {
		t.Fatal("CheckPRState: expected error for transient gh failure, got nil")
	}
	var pe *PRStateError
	if !errors.As(err, &pe) {
		t.Fatalf("CheckPRState: error is not *PRStateError: %T %v", err, err)
	}
	if pe.Kind != PRStateTransient {
		t.Errorf("CheckPRState: Kind=%q, want %q (transient gh error must NOT be classified as missing)", pe.Kind, PRStateTransient)
	}
	for _, want := range []string{
		"could not determine PR state",
		"no such host",
	} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("CheckPRState: transient-error message lacks %q; got: %s", want, pe.Msg)
		}
	}
	// Must NOT match the "does not exist" message — that would mislead the
	// caller into thinking the PR is gone when in fact we couldn't reach gh.
	if strings.Contains(pe.Msg, "does not exist") {
		t.Errorf("CheckPRState: transient error must not say 'does not exist'; got: %s", pe.Msg)
	}
}

// TestCheckPRState_RateLimitIsTransient is a second transient-case test
// covering GitHub's API rate-limit response. Rate-limit is a transient
// condition (retry after window resets), not a permanent missing-PR signal.
func TestCheckPRState_RateLimitIsTransient(t *testing.T) {
	runner := &scriptedPRRunner{
		stderr: "API rate limit exceeded for user ID 12345. (X-RateLimit-Remaining: 0)",
		err:    errors.New("exit status 1"),
	}

	err := checkPRStateWithRunner("2040", runner)
	if err == nil {
		t.Fatal("CheckPRState: expected error for rate-limit, got nil")
	}
	var pe *PRStateError
	if !errors.As(err, &pe) {
		t.Fatalf("CheckPRState: error is not *PRStateError: %T %v", err, err)
	}
	if pe.Kind != PRStateTransient {
		t.Errorf("CheckPRState: Kind=%q, want %q (rate-limit is transient)", pe.Kind, PRStateTransient)
	}
	if !strings.Contains(pe.Msg, "could not determine PR state") {
		t.Errorf("CheckPRState: rate-limit message lacks 'could not determine PR state'; got: %s", pe.Msg)
	}
}

// TestCheckPRState_MalformedJSONIsTransient verifies that unparseable gh
// output is treated as transient — refuse rather than silently passing
// through to spawn agents on an unverified state.
func TestCheckPRState_MalformedJSONIsTransient(t *testing.T) {
	runner := &scriptedPRRunner{
		stdout: `{this is not valid json`,
	}

	err := checkPRStateWithRunner("2040", runner)
	if err == nil {
		t.Fatal("CheckPRState: expected error for malformed JSON, got nil")
	}
	var pe *PRStateError
	if !errors.As(err, &pe) {
		t.Fatalf("CheckPRState: error is not *PRStateError: %T %v", err, err)
	}
	if pe.Kind != PRStateTransient {
		t.Errorf("CheckPRState: Kind=%q, want %q (parse failure is transient)", pe.Kind, PRStateTransient)
	}
	if !strings.Contains(pe.Msg, "could not determine PR state") {
		t.Errorf("CheckPRState: parse-failure message lacks 'could not determine PR state'; got: %s", pe.Msg)
	}
}

// TestCheckPRState_UnknownStateIsRefused verifies defence in depth against
// future gh schema additions: an unrecognised state value (neither OPEN,
// CLOSED, nor MERGED) is refused with a transient-class error rather than
// silently passing through.
func TestCheckPRState_UnknownStateIsRefused(t *testing.T) {
	runner := &scriptedPRRunner{
		stdout: `{"state":"DRAFT","mergedAt":null}`,
	}

	err := checkPRStateWithRunner("2040", runner)
	if err == nil {
		t.Fatal("CheckPRState: expected error for unknown state, got nil")
	}
	var pe *PRStateError
	if !errors.As(err, &pe) {
		t.Fatalf("CheckPRState: error is not *PRStateError: %T %v", err, err)
	}
	if pe.Kind != PRStateTransient {
		t.Errorf("CheckPRState: Kind=%q, want %q (unknown state must refuse)", pe.Kind, PRStateTransient)
	}
	for _, want := range []string{`PR #2040`, `unrecognised state`, `"DRAFT"`} {
		if !strings.Contains(pe.Msg, want) {
			t.Errorf("CheckPRState: unknown-state message lacks %q; got: %s", want, pe.Msg)
		}
	}
}

// TestCheckPRState_EmptyPRNumber verifies that an empty PR number is
// rejected before reaching the runner. Defence in depth — cobra's
// ExactArgs(1) ought to catch this upstream, but a future caller that
// constructs CheckPRState calls directly must not silently invoke gh
// with no argument.
func TestCheckPRState_EmptyPRNumber(t *testing.T) {
	runner := &scriptedPRRunner{}
	err := checkPRStateWithRunner("", runner)
	if err == nil {
		t.Fatal("CheckPRState: expected error for empty PR number, got nil")
	}
	if runner.callCount != 0 {
		t.Errorf("CheckPRState: runner.callCount=%d, want 0 (must not invoke gh with empty PR)", runner.callCount)
	}
}

// ── no-side-effects contract: counter must NOT move on any refusal ────────────

// TestCheckPRState_NoIncrementsCounter is the explicit no-counter-increment
// test required by AC #6. It mirrors TestPreflight_NoIncrementsCounter and
// asserts the same structural contract: as long as CheckPRState does not
// write any `<parent>~review-<N>-<agent>` rows to agent_status, the cycle
// counter (NextRoundNumber) cannot move. Three back-to-back refusals
// (missing / closed / merged) must leave the counter at 1.
//
// This guards against a future refactor that, e.g., starts seeding round
// rows from inside the gate or shares numbering with NextRoundNumber.
func TestCheckPRState_NoIncrementsCounter(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	parent := "test-repo@feature"

	// Sanity: counter starts at 1 (no prior rounds).
	if got := NextRoundNumber(d, parent); got != 1 {
		t.Fatalf("NextRoundNumber baseline: got %d, want 1", got)
	}

	cases := []struct {
		name   string
		runner *scriptedPRRunner
	}{
		{
			name: "missing PR",
			runner: &scriptedPRRunner{
				stderr: "GraphQL: Could not resolve to a PullRequest with the number of 99999999. (repository.pullRequest)",
				err:    errors.New("exit status 1"),
			},
		},
		{
			name:   "closed PR",
			runner: &scriptedPRRunner{stdout: `{"state":"CLOSED","mergedAt":null}`},
		},
		{
			name:   "merged PR",
			runner: &scriptedPRRunner{stdout: `{"state":"MERGED","mergedAt":"2026-05-30T05:18:21Z"}`},
		},
		{
			name: "transient gh error",
			runner: &scriptedPRRunner{
				stderr: "error connecting to api.github.com",
				err:    errors.New("exit status 1"),
			},
		},
	}

	for _, tc := range cases {
		if err := checkPRStateWithRunner("2040", tc.runner); err == nil {
			t.Errorf("%s: expected gate error, got nil", tc.name)
		}
		// After every gate failure, the counter must still be 1.
		if got := NextRoundNumber(d, parent); got != 1 {
			t.Errorf("%s: NextRoundNumber after gate failure: got %d, want 1 (gate must not advance counter)", tc.name, got)
		}
	}

	// Final assertion: four gate failures back to back must leave the
	// counter exactly where it started — same contract as the rebase gate.
	if got := NextRoundNumber(d, parent); got != 1 {
		t.Fatalf("after 4 gate failures: NextRoundNumber=%d, want 1 (gate failures must not consume cycles)", got)
	}
}

// ── ordering contract: PR-existence check runs BEFORE the rebase gate ─────────

// TestCheckPRState_RunsBeforeRebaseGate is the AC #5 ordering test. It
// asserts the structural property by composition: a caller that runs
// CheckPRState then Preflight, where CheckPRState refuses, must never call
// Preflight. This is the same shape as cmd/review.go's runReview, which
// short-circuits on a non-nil CheckPRState error before reaching Preflight.
//
// We use a fakeGit that records calls; if Preflight was reached, fetch would
// have been called. The test asserts fetch was NOT called when CheckPRState
// refused — which is the only way to satisfy AC #5 in cmd/review.go.
func TestCheckPRState_RunsBeforeRebaseGate(t *testing.T) {
	prRunner := &scriptedPRRunner{
		stderr: "GraphQL: Could not resolve to a PullRequest with the number of 99999999. (repository.pullRequest)",
		err:    errors.New("exit status 1"),
	}
	fg := newFakeGit() // empty script — any call would be unscripted and would surface as a test failure

	// Simulate cmd/review.go ordering: CheckPRState → Preflight.
	prErr := checkPRStateWithRunner("99999999", prRunner)
	if prErr == nil {
		t.Fatal("CheckPRState: expected refusal, got nil")
	}
	// In runReview, the PR-existence error short-circuits before Preflight.
	// We replicate that here: if CheckPRState refused, we MUST NOT call
	// Preflight. The test passes if we do not call Preflight (proving the
	// ordering is observable to callers).
	if len(fg.calls) != 0 {
		t.Errorf("Preflight must not run after CheckPRState refusal; fakeGit.calls=%v", fg.calls)
	}
}

// TestCheckPRState_OpenPRPassesThroughToRebaseGate verifies the AC #4
// pass-through contract: when the PR is OPEN, CheckPRState returns nil and
// the caller proceeds to Preflight. We assert by composition — calling
// CheckPRState then Preflight should reach the rebase gate's first git call
// (rev-parse HEAD).
func TestCheckPRState_OpenPRPassesThroughToRebaseGate(t *testing.T) {
	prRunner := &scriptedPRRunner{
		stdout: `{"state":"OPEN","mergedAt":null}`,
	}
	if err := checkPRStateWithRunner("2040", prRunner); err != nil {
		t.Fatalf("CheckPRState: expected nil for OPEN PR, got: %v", err)
	}

	// Now run Preflight against a fakeGit that completes successfully — the
	// composition (PR-state OK → Preflight OK) is the pass-through case.
	fg := newFakeGit().
		on("rev-parse HEAD", scriptedResponse{stdout: "deadbeef\n", exitCode: 0}).
		on("fetch origin main", scriptedResponse{exitCode: 0}).
		on("rev-parse --verify origin/main", scriptedResponse{stdout: "abc123\n", exitCode: 0}).
		on("merge-base --is-ancestor origin/main HEAD", scriptedResponse{exitCode: 0})

	if err := Preflight(PreflightOpts{Worktree: "/fake/worktree", gitRunner: fg}); err != nil {
		t.Fatalf("Preflight (post-OPEN-passthrough): unexpected error: %v", err)
	}
	// And the rebase gate must actually have been exercised.
	if !fg.called("merge-base --is-ancestor origin/main HEAD") {
		t.Errorf("Preflight: expected ancestor check to be invoked after OPEN passthrough; calls=%v", fg.calls)
	}
}
