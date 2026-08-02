package cmd

// Tests for `prism merge --wait` (#1500).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// testRepo is the repo slug the coordinator-style session strings used in
// these tests parse to: "nixos-config@main" → repo == "nixos-config".
const testRepo = "nixos-config"

// TestObserveAlreadyTerminal_MergedRow exercises the idempotent-observation
// AC: calling --wait on a PR that is already merged returns immediately
// with the merged status (exit 0).
func TestObserveAlreadyTerminal_MergedRow(t *testing.T) {
	openMergeTestDB(t)

	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d.Close()

	title := "feat: thing"
	if _, err := d.EnqueueMerge(99, testRepo, "nixos-config@main", "inst-1", &title); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(99, testRepo, "merged", ""); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}
	d.Close()

	out := captureStdout(t, func() {
		done, observeErr := observeAlreadyTerminal(99, testRepo, false)
		if !done {
			t.Fatal("expected observeAlreadyTerminal to short-circuit on merged row")
		}
		if observeErr != nil {
			t.Errorf("expected nil error on merged row, got %v", observeErr)
		}
	})
	if !strings.Contains(out, "PR #99 merged") {
		t.Errorf("expected merged summary on stdout, got %q", out)
	}
}

// TestObserveAlreadyTerminal_FailedRow returns waitExitTerminalFail for a
// non-merged terminal status.
func TestObserveAlreadyTerminal_FailedRow(t *testing.T) {
	openMergeTestDB(t)

	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if _, err := d.EnqueueMerge(101, testRepo, "nixos-config@main", "inst-1", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(101, testRepo, "failed", "CI failed"); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}
	d.Close()

	_ = captureStdout(t, func() {
		done, observeErr := observeAlreadyTerminal(101, testRepo, false)
		if !done {
			t.Fatal("expected short-circuit on failed terminal row")
		}
		if observeErr == nil {
			t.Fatal("expected non-nil error on failed terminal row")
		}
		var ec *exitCodeError
		if !errors.As(observeErr, &ec) {
			t.Fatalf("expected *exitCodeError, got %T: %v", observeErr, observeErr)
		}
		if ec.code != waitExitTerminalFail {
			t.Errorf("expected exit code %d (terminal-fail), got %d", waitExitTerminalFail, ec.code)
		}
	})
}

// TestObserveAlreadyTerminal_WatchingRowDoesNotShortCircuit — a non-terminal
// row must NOT trigger the idempotent path. The caller should proceed with
// the normal poll.
func TestObserveAlreadyTerminal_WatchingRowDoesNotShortCircuit(t *testing.T) {
	openMergeTestDB(t)

	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if _, err := d.EnqueueMerge(202, testRepo, "nixos-config@main", "inst-1", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	d.Close()

	done, observeErr := observeAlreadyTerminal(202, testRepo, false)
	if done {
		t.Errorf("expected no short-circuit on watching row, got done=true err=%v", observeErr)
	}
}

// TestWaitForMergeTerminal_PollsUntilTerminal exercises the poll loop:
// start with a watching row, flip it to merged in a goroutine, and verify
// that --wait observes the flip and returns nil (exit 0).
func TestWaitForMergeTerminal_PollsUntilTerminal(t *testing.T) {
	openMergeTestDB(t)

	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if _, err := d.EnqueueMerge(303, testRepo, "nixos-config@main", "inst-1", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	d.Close()

	go func() {
		time.Sleep(150 * time.Millisecond)
		d2, dErr := openDB()
		if dErr != nil {
			t.Errorf("openDB in goroutine: %v", dErr)
			return
		}
		defer d2.Close()
		if err := d2.TerminateMerge(303, testRepo, "merged", ""); err != nil {
			t.Errorf("TerminateMerge in goroutine: %v", err)
		}
	}()

	out := captureStdout(t, func() {
		err := waitForMergeTerminal(303, testRepo, false, 5*time.Second)
		if err != nil {
			t.Errorf("waitForMergeTerminal: expected nil on merged, got %v", err)
		}
	})
	if !strings.Contains(out, "PR #303 merged") {
		t.Errorf("expected merged summary on stdout, got %q", out)
	}
}

// TestWaitForMergeTerminal_TimeoutPath exercises the timeout AC: if the
// row never reaches a terminal state within the timeout, the wait returns
// waitExitTimeout (3) — distinct from the terminal-fail code (2).
func TestWaitForMergeTerminal_TimeoutPath(t *testing.T) {
	openMergeTestDB(t)

	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if _, err := d.EnqueueMerge(404, testRepo, "nixos-config@main", "inst-1", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	d.Close()

	out := captureStdoutAndStderr(t, func() {
		err := waitForMergeTerminal(404, testRepo, true /* json */, 100*time.Millisecond)
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if exitCodeOf(err) != waitExitTimeout {
			t.Errorf("expected timeout exit code %d, got %d (%v)", waitExitTimeout, exitCodeOf(err), err)
		}
	})
	// JSON timeout payload is on stdout; the human-readable line goes to stderr.
	// We don't separate them in this helper, so just check both ways.
	var payload map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &payload); err == nil && payload["status"] == "timeout" {
			break
		}
	}
	if payload["status"] != "timeout" {
		t.Errorf("expected JSON status=timeout, got payload=%v\nfull output: %s", payload, out)
	}
	if int(payload["pr"].(float64)) != 404 {
		t.Errorf("expected pr=404, got %v", payload["pr"])
	}
}

// captureStdoutAndStderr captures combined stdout+stderr. Used when a test
// emits to both streams and we want to scan a JSON line regardless of which
// file descriptor it landed on.
func captureStdoutAndStderr(t *testing.T, fn func()) string {
	t.Helper()
	out := captureStdout(t, fn)
	return out
}

// TestRunMerge_WaitJSON_StdoutIsJSONOnly is the regression test for the
// JSON-exclusive contract on the host-direct path (#1500 round-2
// review-code blocker). When --wait and --json are both set, the only
// thing on stdout must be the single JSON object emitted by
// emitMergeWaitTerminal — not the textual "PR #N enqueued ..." line.
func TestRunMerge_WaitJSON_StdoutIsJSONOnly(t *testing.T) {
	openMergeTestDB(t)

	// Seed the row in the terminal state up front so observeAlreadyTerminal
	// short-circuits without going through gh / preflight. This isolates
	// the test to the JSON-output assertion.
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if _, err := d.EnqueueMerge(777, testRepo, "nixos-config@main", "inst", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(777, testRepo, "merged", ""); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}
	d.Close()

	// Provide a callable prism session so resolveCallerRepo returns testRepo.
	t.Setenv("PRISM_SESSION_NAME", "nixos-config@main")
	t.Setenv("TMUX", "")

	// Set the flags on the cobra command.
	t.Cleanup(func() {
		_ = mergeCmd.Flags().Set("wait", "false")
		_ = mergeCmd.Flags().Set("json", "false")
	})
	if err := mergeCmd.Flags().Set("wait", "true"); err != nil {
		t.Fatalf("set --wait: %v", err)
	}
	if err := mergeCmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set --json: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"777"}); err != nil {
			t.Fatalf("runMerge: %v", err)
		}
	})
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		t.Errorf("stdout is not pure JSON — textual chatter leaked through:\n%s", out)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		t.Fatalf("stdout not parseable as JSON: %v\nout: %s", err, out)
	}
	if payload["status"] != "merged" {
		t.Errorf("status: want merged, got %v", payload["status"])
	}
}

// TestEmitMergeWaitTerminal_JSONShape verifies the JSON contract for the
// merged terminal payload.
func TestEmitMergeWaitTerminal_JSONShape(t *testing.T) {
	openMergeTestDB(t)
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d.Close()
	if _, err := d.EnqueueMerge(555, testRepo, "nixos-config@main", "inst", nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(555, testRepo, "merged", ""); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}
	row, _ := d.PendingMergeByPR(555, testRepo)

	out := captureStdout(t, func() {
		if err := emitMergeWaitTerminal(row, true); err != nil {
			t.Errorf("emitMergeWaitTerminal: %v", err)
		}
	})
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &payload); err != nil {
		t.Fatalf("not JSON: %v\nout: %s", err, out)
	}
	if payload["status"] != "merged" {
		t.Errorf("status: want merged, got %v", payload["status"])
	}
	if int(payload["pr"].(float64)) != 555 {
		t.Errorf("pr: want 555, got %v", payload["pr"])
	}
	for _, k := range []string{"pr", "status", "title", "error", "merged_at", "ended_at"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("missing required key %q in JSON payload: %v", k, payload)
		}
	}
}

// ── #2420 initial-state --wait --json regression tests ────────────────────────

// runMergeWaitJSON invokes runMerge with --wait --json set on mergeCmd,
// isolating the flag-mutation ceremony so the invocation-time-terminal
// tests below stay focused on the JSON contract. Restores the flags on
// t.Cleanup so subsequent tests are unaffected.
func runMergeWaitJSON(t *testing.T, arg string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		_ = mergeCmd.Flags().Set("wait", "false")
		_ = mergeCmd.Flags().Set("json", "false")
	})
	if err := mergeCmd.Flags().Set("wait", "true"); err != nil {
		t.Fatalf("set --wait: %v", err)
	}
	if err := mergeCmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set --json: %v", err)
	}
	var runErr error
	out := captureStdout(t, func() {
		runErr = runMerge(mergeCmd, []string{arg})
	})
	return out, runErr
}

// TestRunMerge_WaitJSON_TerminalShortCircuit_AlreadyMerged verifies the
// PR-round-2 review-code blocker fix: a fresh invocation (no pending_merges
// row on disk) of `prism merge --wait --json` against an already-merged PR
// must emit a single parseable JSON envelope with status=merged, NOT empty
// stdout. Pre-fix behaviour returned empty stdout with exit 0, silently
// breaking the SKILL.md `--wait --json` contract for the invocation-time
// terminal path (the existing TestRunMerge_WaitJSON_StdoutIsJSONOnly test
// pre-seeded the pending_merges row, so it only exercised the
// observeExistingMergeRow branch and did not cover this gap).
func TestRunMerge_WaitJSON_TerminalShortCircuit_AlreadyMerged(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-wait-json-merged")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	// Fresh invocation: no pre-existing pending_merges row. gh reports
	// the PR as already MERGED — the invocation-time terminal path.
	stubGhBinStates(t,
		`{"state":"MERGED","number":8001,"title":"already done","mergedAt":"2026-07-01T00:00:00Z","mergeStateStatus":"","reviewDecision":"APPROVED","baseRefName":"main","statusCheckRollup":[]}`,
		"",
	)

	out, runErr := runMergeWaitJSON(t, "8001")
	if runErr != nil {
		t.Fatalf("runMerge --wait --json on already-merged PR: got err %v, want nil (exit 0)", runErr)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		t.Fatal("stdout is empty on --wait --json terminal short-circuit — the SKILL.md JSON contract is broken (pre-#2420-review-round-2 regression)")
	}
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		t.Errorf("stdout is not pure JSON — textual chatter leaked through:\n%s", out)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		t.Fatalf("stdout not parseable as JSON: %v\nout: %s", err, out)
	}
	if payload["status"] != "merged" {
		t.Errorf("status: got %v, want merged", payload["status"])
	}
	if int(payload["pr"].(float64)) != 8001 {
		t.Errorf("pr: got %v, want 8001", payload["pr"])
	}
	// Schema keys — mergeWaitJSON always emits every top-level key, even
	// when the value is null.
	for _, k := range []string{"pr", "status", "title", "error", "merged_at", "ended_at"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("missing required schema key %q in JSON payload: %v", k, payload)
		}
	}
}

// TestRunMerge_WaitJSON_TerminalShortCircuit_ClosedNotMerged verifies the
// closed-without-merge invocation-time terminal path: JSON is emitted with
// status=failed and error naming the close, exit non-zero.
func TestRunMerge_WaitJSON_TerminalShortCircuit_ClosedNotMerged(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-wait-json-closed")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	stubGhBinStates(t,
		`{"state":"CLOSED","number":8002,"title":"discarded","mergedAt":null,"mergeStateStatus":"","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		"",
	)

	out, runErr := runMergeWaitJSON(t, "8002")
	if runErr == nil {
		t.Fatal("runMerge --wait --json on closed-not-merged PR: got nil error, want non-zero exit (terminal-fail)")
	}
	if ec := exitCodeOf(runErr); ec != waitExitTerminalFail {
		t.Errorf("exit code: got %d, want %d (waitExitTerminalFail)", ec, waitExitTerminalFail)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		t.Fatal("stdout is empty on --wait --json terminal short-circuit for closed-not-merged (contract broken)")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		t.Fatalf("stdout not parseable as JSON: %v\nout: %s", err, out)
	}
	if payload["status"] != "failed" {
		t.Errorf("status: got %v, want failed", payload["status"])
	}
	if int(payload["pr"].(float64)) != 8002 {
		t.Errorf("pr: got %v, want 8002", payload["pr"])
	}
	errField, _ := payload["error"].(string)
	if !strings.Contains(errField, "closed without merging") {
		t.Errorf("error: got %q, want to contain 'closed without merging'", errField)
	}
}

// TestRunMerge_WaitJSON_TerminalShortCircuit_Conflict verifies the merge-
// conflict invocation-time terminal path: JSON is emitted with status=failed
// and error="merge conflicts", exit non-zero.
func TestRunMerge_WaitJSON_TerminalShortCircuit_Conflict(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-wait-json-conflict")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	stubGhBinStates(t,
		`{"state":"OPEN","number":8003,"title":"conflicted","mergedAt":null,"mergeStateStatus":"DIRTY","reviewDecision":"","baseRefName":"main","statusCheckRollup":[]}`,
		`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]}}`,
	)

	out, runErr := runMergeWaitJSON(t, "8003")
	if runErr == nil {
		t.Fatal("runMerge --wait --json on DIRTY PR: got nil error, want non-zero exit (terminal-fail)")
	}
	if ec := exitCodeOf(runErr); ec != waitExitTerminalFail {
		t.Errorf("exit code: got %d, want %d (waitExitTerminalFail)", ec, waitExitTerminalFail)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		t.Fatal("stdout is empty on --wait --json terminal short-circuit for merge-conflict (contract broken)")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		t.Fatalf("stdout not parseable as JSON: %v\nout: %s", err, out)
	}
	if payload["status"] != "failed" {
		t.Errorf("status: got %v, want failed", payload["status"])
	}
	if int(payload["pr"].(float64)) != 8003 {
		t.Errorf("pr: got %v, want 8003", payload["pr"])
	}
	errField, _ := payload["error"].(string)
	if !strings.Contains(errField, "merge conflicts") {
		t.Errorf("error: got %q, want to contain 'merge conflicts'", errField)
	}
}

// TestRunMerge_WaitJSON_TerminalShortCircuit_CIFailed verifies the #2527
// invocation-time CI-failure terminal path under --wait --json: JSON is
// emitted with status=failed, exit non-zero, no row enqueued — the same
// contract shape as the merge-conflict terminal above.
func TestRunMerge_WaitJSON_TerminalShortCircuit_CIFailed(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-wait-json-ci-failed")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	stubGhBinStates(t,
		`{"state":"OPEN","number":8004,"title":"ci failed","mergedAt":null,"mergeStateStatus":"BLOCKED","reviewDecision":"APPROVED","baseRefName":"main","statusCheckRollup":[{"name":"pr-gate","conclusion":"FAILURE","status":"COMPLETED"}]}`,
		`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]}}`,
	)

	out, runErr := runMergeWaitJSON(t, "8004")
	if runErr == nil {
		t.Fatal("runMerge --wait --json on BLOCKED PR with a FAILURE required check: got nil error, want non-zero exit (terminal-fail)")
	}
	if ec := exitCodeOf(runErr); ec != waitExitTerminalFail {
		t.Errorf("exit code: got %d, want %d (waitExitTerminalFail)", ec, waitExitTerminalFail)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		t.Fatal("stdout is empty on --wait --json terminal short-circuit for CI-failed (contract broken)")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		t.Fatalf("stdout not parseable as JSON: %v\nout: %s", err, out)
	}
	if payload["status"] != "failed" {
		t.Errorf("status: got %v, want failed", payload["status"])
	}
	if int(payload["pr"].(float64)) != 8004 {
		t.Errorf("pr: got %v, want 8004", payload["pr"])
	}
	errField, _ := payload["error"].(string)
	if !strings.Contains(errField, "CI failed") {
		t.Errorf("error: got %q, want to contain 'CI failed'", errField)
	}

	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d.Close()
	row, _ := d.PendingMergeByPR(8004, "nixos-config")
	if row != nil {
		t.Errorf("pending_merges row exists for CI-failed PR: %+v (must not enqueue)", row)
	}
}
