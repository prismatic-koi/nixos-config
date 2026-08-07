package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// openMergeTestDB opens an isolated test DB and registers cleanup.
// Sets testDBPath so that openDB() in cmd package uses the test DB.
//
// It also unsets PRISM_HOST_API for the duration of the test so that
// runMerge / runMergesList / runMergesCancel exercise the host-side DB path
// rather than attempting to proxy through a host-API socket that does not
// exist in the test environment (#1043).
func openMergeTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	SetTestDBPath(filepath.Join(dir, "merge_test.db"))
	t.Cleanup(func() { SetTestDBPath("") })
	t.Setenv("PRISM_HOST_API", "")
}

// ── runMerge coordinator-only guard ───────────────────────────────────────────

// TestRunMerge_WorkerSessionIsRejected verifies that a worker session calling
// prism merge receives an error and no row is inserted. This is the security
// AC: "Worker agents are not permitted to invoke prism merge."
func TestRunMerge_WorkerSessionIsRejected(t *testing.T) {
	openMergeTestDB(t)

	// Seed a worker session.
	const workerSession = "nixos-config@feature"
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d.Close()
	if err := d.UpsertStatusSeedRootAgentName(workerSession, "nixos-config", "/worktree/feature", "idle", nil, nil, "worker", "", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	d.Close()

	t.Setenv("PRISM_SESSION_NAME", workerSession)
	t.Setenv("TMUX", "")

	// Call runMerge directly. It should return an error before inserting any row.
	err = runMerge(mergeCmd, []string{"42"})
	if err == nil {
		t.Fatal("runMerge: expected error for worker session, got nil")
	}
	if !strings.Contains(err.Error(), "coordinator sessions only") {
		t.Errorf("runMerge error %q does not mention 'coordinator sessions only'", err.Error())
	}

	// Confirm no row was inserted.
	d2, err2 := openDB()
	if err2 != nil {
		t.Fatalf("openDB for verify: %v", err2)
	}
	defer d2.Close()
	row, rowErr := d2.PendingMergeByPR(42, "nixos-config")
	if rowErr != nil {
		t.Fatalf("PendingMergeByPR: %v", rowErr)
	}
	if row != nil {
		t.Errorf("PendingMergeByPR(42): got row with status=%q, want nil (no row should be inserted for worker)", row.Status)
	}
}

// TestRunMerge_CoordinatorSessionNotRejectedByWorkerGuard verifies that a
// @main session (coordinator heuristic) is allowed past the worker-rejection
// gate. With the mint-on-the-fly fix, the call no longer fails at the
// instance_id check — it mints one and proceeds to the gh preflight, which
// fails in a test environment (no real GitHub API). The only assertion here is
// that the error is NOT about "coordinator sessions only".
func TestRunMerge_CoordinatorSessionNotRejectedByWorkerGuard(t *testing.T) {
	openMergeTestDB(t)

	const coordSession = "nixos-config@main"
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(coordSession, "nixos-config", "/worktree/main", "idle", nil, nil, "coordinator", "", ""); err != nil {
		d.Close()
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	d.Close()

	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	// The call will fail at the gh preflight (no real GitHub API in tests),
	// but NOT with "coordinator sessions only" or "cannot determine instance_id".
	err = runMerge(mergeCmd, []string{"999"})
	if err == nil {
		// Surprising but not impossible in a very unusual test environment.
		t.Log("runMerge returned nil — may have succeeded via gh if PR 999 is open")
		return
	}
	if strings.Contains(err.Error(), "coordinator sessions only") {
		t.Errorf("runMerge error %q mentions 'coordinator sessions only' — coordinator should not be blocked", err.Error())
	}
	if strings.Contains(err.Error(), "cannot determine instance_id") {
		t.Errorf("runMerge error %q mentions 'cannot determine instance_id' — should have been minted on the fly", err.Error())
	}
}

// ── mint-on-the-fly instance_id ───────────────────────────────────────────────

// TestRunMerge_FailsWhenInstanceIDMissing verifies that when a coordinator
// session has no instance_id in the DB, runMerge returns a clear error
// indicating the sidecar did not start correctly. The sidecar is now the
// sole owner of instance_id minting (issue #1252); on-the-fly recovery in
// runMerge has been removed.
func TestRunMerge_FailsWhenInstanceIDMissing(t *testing.T) {
	openMergeTestDB(t)

	// Seed a coordinator session WITHOUT an instance_id (simulates a
	// @main session whose sidecar did not run correctly).
	const coordSession = "nixos-config@main"
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(coordSession, "nixos-config", "/worktree/main", "idle", nil, nil, "coordinator", "", ""); err != nil {
		d.Close()
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	d.Close()

	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	// runMerge should fail with a clear error about missing instance_id,
	// not attempt on-the-fly minting.
	err = runMerge(mergeCmd, []string{"999"})
	if err == nil {
		t.Fatal("expected runMerge to return an error when instance_id is missing, got nil")
	}
	if !strings.Contains(err.Error(), "no instance_id") && !strings.Contains(err.Error(), "sidecar did not start") {
		t.Fatalf("expected error about missing instance_id / sidecar not starting, got: %v", err)
	}
}

// ── GitLab guardrail (#2669) ──────────────────────────────────────────────────

// TestRunMerge_RefusesOnGitLabRemote verifies that `prism merge` refuses
// outright when the current directory's origin remote is gitlab.com, before
// any coordinator/instance_id/gh preflight checks run, and names the `glab`
// workaround.
func TestRunMerge_RefusesOnGitLabRemote(t *testing.T) {
	openMergeTestDB(t)

	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init")
	runGit("remote", "add", "origin", "git@gitlab.com:owner/repo.git")
	t.Chdir(dir)

	// No session env is set — if the gitlab guard did not run first, this
	// would instead fail on "cannot determine calling session", which would
	// mask the intended message and prove the check ran too late (or not at
	// all).
	err := runMerge(mergeCmd, []string{"7"})
	if err == nil {
		t.Fatal("runMerge: expected error for a gitlab.com remote, got nil")
	}
	if !strings.Contains(err.Error(), "glab mr merge 7") {
		t.Errorf("runMerge error %q does not name the glab workaround", err.Error())
	}
	if !strings.Contains(err.Error(), "not supported by the prism merge queue") {
		t.Errorf("runMerge error %q does not explain why GitLab is unsupported", err.Error())
	}
}

// TestRunMerge_GitHubRemoteUnaffected verifies the gitlab guard does not
// fire for a github.com remote — it must fall through to the normal
// coordinator-only guard (proving the check is additive, not a regression
// for the primary forge).
func TestRunMerge_GitHubRemoteUnaffected(t *testing.T) {
	openMergeTestDB(t)

	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init")
	runGit("remote", "add", "origin", "git@github.com:owner/repo.git")
	t.Chdir(dir)

	const workerSession = "nixos-config@feature"
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := d.UpsertStatusSeedRootAgentName(workerSession, "nixos-config", "/worktree/feature", "idle", nil, nil, "worker", "", ""); err != nil {
		d.Close()
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	d.Close()

	t.Setenv("PRISM_SESSION_NAME", workerSession)
	t.Setenv("TMUX", "")

	err = runMerge(mergeCmd, []string{"42"})
	if err == nil {
		t.Fatal("runMerge: expected error for worker session, got nil")
	}
	if strings.Contains(err.Error(), "glab mr merge") {
		t.Errorf("runMerge error %q wrongly fired the gitlab guard for a github.com remote", err.Error())
	}
	if !strings.Contains(err.Error(), "coordinator sessions only") {
		t.Errorf("runMerge error %q does not mention 'coordinator sessions only'", err.Error())
	}
}

// ── re-entry / idempotence (#1875) ────────────────────────────────────────────
//
// These tests cover the cmd/-layer side of merge-queue idempotence. The DB
// layer's EnqueueMerge is already idempotent via ON CONFLICT(pr) DO UPDATE,
// but the cmd/-layer behaviour around re-entry was previously unverified:
//
//   - duplicate `gh pr view` round-trip on every re-entry,
//   - user-facing message reported "enqueued" as if the call were fresh,
//   - in-sandbox proxy path always ran preflight unconditionally even when
//     the PR was already terminal on the host.
//
// The fix is observeExistingMergeRow() at the top of runMerge: a single
// read-only probe of pending_merges short-circuits both the gh round-trip
// and the duplicate enqueue when a row already exists.

// seedCoordinatorWithInstanceID seeds a coordinator session with an
// instance_id so runMerge can reach the preflight / enqueue path without
// failing the worker-guard or the instance_id sidecar-startup check.
func seedCoordinatorWithInstanceID(t *testing.T, sessionName, instanceID string) {
	t.Helper()
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer d.Close()
	if err := d.UpsertStatusSeedRootAgentName(sessionName, "nixos-config", "/worktree/main", "idle", nil, nil, "coordinator", "", ""); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if err := d.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
}

// stubGhBinCounting installs a `gh` shim on PATH that increments a counter
// file on every invocation, then emits a valid PR-view JSON payload. Tests
// can read the counter file to assert how many times preflight() called
// out to gh. The counter file path is returned so the test owns its
// lifetime via t.TempDir().
func stubGhBinCounting(t *testing.T, pr int, state, title string) string {
	t.Helper()
	dir := t.TempDir()
	counterPath := filepath.Join(dir, "gh.calls")
	ghPath := filepath.Join(dir, "gh")
	// The script appends a marker per call, then emits a fixed JSON body.
	// flock-free append-by-line is enough for our purposes: tests are
	// single-threaded against this binary and a few extra bytes per call
	// is harmless. Counting newlines in the file gives the call count.
	script := fmt.Sprintf(`#!/bin/sh
echo call >> %q
cat <<EOF
{"state":"%s","number":%d,"title":"%s"}
EOF
`, counterPath, state, pr, title)
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub gh: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return counterPath
}

// countGhCalls returns the number of times the counting gh stub was
// invoked. Returns 0 if the counter file does not exist yet (no calls).
func countGhCalls(t *testing.T, counterPath string) int {
	t.Helper()
	data, err := os.ReadFile(counterPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read gh counter: %v", err)
	}
	return strings.Count(string(data), "\n")
}

// TestRunMerge_ReentrySameRow verifies AC (a): two sequential `runMerge`
// calls for the same PR converge to a single DB row (no duplicate). This
// is a regression test for the cmd/-layer side of the EnqueueMerge
// idempotence contract — a paper-cut bug here would either explode into
// duplicate rows or rewrite the original row's queue_position on every
// re-entry, both of which break the merge-queue watcher's FIFO ordering.
func TestRunMerge_ReentrySameRow(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-reentry-1")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")
	stubGhBinCounting(t, 1234, "OPEN", "feat: thing")

	// First call: row is created. Capture stdout to keep test output clean.
	_ = captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"1234"}); err != nil {
			t.Fatalf("first runMerge: %v", err)
		}
	})

	// Snapshot the row state after the first call.
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	first, err := d.PendingMergeByPR(1234, "nixos-config")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if first == nil {
		t.Fatal("expected a pending_merges row after first runMerge, got nil")
	}
	firstQueuePos := first.QueuePosition
	firstQueuedAt := first.QueuedAt
	d.Close()

	// Second call: must converge to the same row. observeExistingMergeRow
	// is expected to short-circuit, so neither preflight nor EnqueueMerge
	// runs — queue_position and queued_at stay identical.
	_ = captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"1234"}); err != nil {
			t.Fatalf("second runMerge: %v", err)
		}
	})

	d2, err := openDB()
	if err != nil {
		t.Fatalf("openDB for verify: %v", err)
	}
	defer d2.Close()
	second, err := d2.PendingMergeByPR(1234, "nixos-config")
	if err != nil {
		t.Fatalf("PendingMergeByPR (after 2nd): %v", err)
	}
	if second == nil {
		t.Fatal("row disappeared after second runMerge")
	}
	if second.QueuePosition != firstQueuePos {
		t.Errorf("queue_position changed across re-entry: first=%d second=%d (FIFO ordering must be stable)", firstQueuePos, second.QueuePosition)
	}
	if !second.QueuedAt.Equal(firstQueuedAt) {
		t.Errorf("queued_at changed across re-entry: first=%s second=%s", firstQueuedAt, second.QueuedAt)
	}
}

// TestRunMerge_ReentryAlreadyMergedSkipsGh verifies AC (b): re-enqueueing
// a PR that is already merged on host does not pay a `gh pr view`
// round-trip and does not re-insert a fresh watching row over the
// terminal one. The counting gh stub asserts call count = 0; the DB row
// must remain in status="merged" with its original merged_at.
//
// Note: the short-circuit is intentionally scoped to status=="merged"
// only. The failed / cancelled / abandoned counterparts in
// TestRunMerge_ReentryAfter{Failed,Cancelled,Abandoned}ReEnqueues assert
// the opposite contract for those statuses (they must re-enqueue, per the
// documented coordinator retry flow).
func TestRunMerge_ReentryAlreadyMergedSkipsGh(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-reentry-merged")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	// Seed an already-merged row directly via the DB (no runMerge call —
	// we want zero gh invocations before the test point).
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	title := "feat: already done"
	if _, err := d.EnqueueMerge(2222, "nixos-config", coordSession, "inst-reentry-merged", &title); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(2222, "nixos-config", "merged", ""); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}
	pre, err := d.PendingMergeByPR(2222, "nixos-config")
	if err != nil {
		t.Fatalf("PendingMergeByPR (pre): %v", err)
	}
	if pre == nil || pre.Status != "merged" {
		t.Fatalf("expected merged row pre-test, got %+v", pre)
	}
	preMergedAt := pre.MergedAt
	d.Close()

	// Install the counting gh stub AFTER seeding so we observe zero calls.
	counterPath := stubGhBinCounting(t, 2222, "OPEN", "feat: already done")

	out := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"2222"}); err != nil {
			t.Fatalf("runMerge on already-merged PR: %v", err)
		}
	})

	// AC (b) headline: zero gh round-trips on the re-entry path.
	if n := countGhCalls(t, counterPath); n != 0 {
		t.Errorf("gh was called %d times on re-entry of already-merged PR — expected 0 (preflight must be skipped)", n)
	}

	// The merged row must remain merged with its original merged_at — the
	// short-circuit must not have called EnqueueMerge, which would have
	// reset the row to status="watching" via the ON CONFLICT branch.
	d2, err := openDB()
	if err != nil {
		t.Fatalf("openDB for verify: %v", err)
	}
	defer d2.Close()
	post, err := d2.PendingMergeByPR(2222, "nixos-config")
	if err != nil {
		t.Fatalf("PendingMergeByPR (post): %v", err)
	}
	if post == nil {
		t.Fatal("row disappeared after re-entry on merged PR")
	}
	if post.Status != "merged" {
		t.Errorf("status changed across re-entry on merged PR: pre=merged post=%q (terminal row must be preserved)", post.Status)
	}
	if preMergedAt != nil && post.MergedAt != nil && !preMergedAt.Equal(*post.MergedAt) {
		t.Errorf("merged_at changed across re-entry: pre=%s post=%s", *preMergedAt, *post.MergedAt)
	}

	// stdout should describe the terminal status, not a fresh enqueue.
	if strings.Contains(out, "enqueued") {
		t.Errorf("stdout falsely reports \"enqueued\" on re-entry of merged PR: %q", out)
	}
	if !strings.Contains(out, "PR #2222 merged") {
		t.Errorf("stdout %q does not report the merged terminal status", out)
	}
}

// TestRunMerge_ReentryDistinguishableMessage verifies AC (c): the
// user-facing message on re-entry of an already-non-terminal row is
// distinguishable from the fresh-enqueue line. This is the user-visible
// half of the fix — a coordinator who runs `prism merge N` twice should
// not be told "enqueued" on the second call as if it were the first.
//
// As a side-assertion, the counting gh stub confirms the optional AC
// "skips the gh pr view round-trip" for a non-terminal re-entry.
func TestRunMerge_ReentryDistinguishableMessage(t *testing.T) {
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-reentry-msg")
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")
	counterPath := stubGhBinCounting(t, 3333, "OPEN", "feat: distinguishable")

	// First call: should print the standard "PR #N enqueued ..." line.
	firstOut := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"3333"}); err != nil {
			t.Fatalf("first runMerge: %v", err)
		}
	})
	if !strings.Contains(firstOut, "PR #3333 enqueued") {
		t.Errorf("first call stdout does not report fresh enqueue: %q", firstOut)
	}
	callsAfterFirst := countGhCalls(t, counterPath)
	// The #2420 initial-state probe makes two gh invocations per fresh
	// call: `gh pr view` (state, mergeable, reviewDecision, checks) and
	// `gh api ...branches/:branch/protection` (protected vs 404). Together
	// they are the invocation-time state-table probe.
	if callsAfterFirst != 2 {
		t.Errorf("first call should invoke gh exactly twice (pr view + branch protection per #2420 initial-state probe), got %d", callsAfterFirst)
	}

	// Second call: must NOT print the fresh-enqueue line, must print an
	// "already in queue" indication, and must NOT call gh.
	secondOut := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"3333"}); err != nil {
			t.Fatalf("second runMerge: %v", err)
		}
	})
	if strings.Contains(secondOut, "PR #3333 enqueued") {
		t.Errorf("second call falsely reports \"enqueued\" on re-entry: %q", secondOut)
	}
	if !strings.Contains(secondOut, "already in queue") {
		t.Errorf("second call stdout %q does not say \"already in queue\" (re-entry message must be distinguishable)", secondOut)
	}
	if firstOut == secondOut {
		t.Errorf("first and second runMerge produced identical stdout — messages must differ")
	}
	callsAfterSecond := countGhCalls(t, counterPath)
	if callsAfterSecond != callsAfterFirst {
		t.Errorf("second call invoked gh %d additional time(s) — expected 0 (preflight must be skipped on re-entry)", callsAfterSecond-callsAfterFirst)
	}
}

// TestRunMerge_ReentryAfterFailedReEnqueues verifies that a `failed`
// terminal row is NOT short-circuited — the documented coordinator flow
// is: on CI failure / merge conflicts, prompt the worker to fix and then
// re-run `prism merge <pr>` to re-enqueue. The new
// observeExistingMergeRow must fall through for status=="failed" so the
// normal preflight + EnqueueMerge path runs, which resets the row to
// `watching` via the ON CONFLICT branch in internal/db.EnqueueMerge.
//
// Assertions:
//   - gh IS invoked (preflight ran) — the counting stub records exactly 1
//     call;
//   - the row's status flips from `failed` back to `watching` (DB-layer
//     ON CONFLICT branch fired);
//   - stdout reports a fresh "enqueued" line, not "already in queue".
func TestRunMerge_ReentryAfterFailedReEnqueues(t *testing.T) {
	testRunMergeReentryAfterTerminalReEnqueues(t, 5551, "failed", "CI failed")
}

// TestRunMerge_ReentryAfterCancelledReEnqueues exercises the same
// re-enqueue contract for the `cancelled` terminal state. `prism merges
// cancel <pr>` produces this state; the coordinator is then free to
// re-enqueue with `prism merge <pr>` if cancellation was a mistake or a
// transient condition has cleared.
func TestRunMerge_ReentryAfterCancelledReEnqueues(t *testing.T) {
	testRunMergeReentryAfterTerminalReEnqueues(t, 5552, "cancelled", "")
}

// TestRunMerge_ReentryAfterAbandonedReEnqueues exercises the same
// re-enqueue contract for the `abandoned` terminal state. A row is
// abandoned when its watching coordinator session ends without reaching a
// terminal merge state; a new coordinator picking up the work may
// re-enqueue via `prism merge <pr>`.
func TestRunMerge_ReentryAfterAbandonedReEnqueues(t *testing.T) {
	testRunMergeReentryAfterTerminalReEnqueues(t, 5553, "abandoned", "")
}

// testRunMergeReentryAfterTerminalReEnqueues is the shared body for the
// three failed/cancelled/abandoned re-enqueue tests. The PR number is
// per-test so the tests can run in parallel without colliding on the
// pending_merges row.
func testRunMergeReentryAfterTerminalReEnqueues(t *testing.T, pr int, terminalStatus, errMsg string) {
	t.Helper()
	openMergeTestDB(t)
	const coordSession = "nixos-config@main"
	seedCoordinatorWithInstanceID(t, coordSession, "inst-reentry-"+terminalStatus)
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	// Seed a row in the requested terminal state directly via the DB.
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	title := "feat: needs retry"
	if _, err := d.EnqueueMerge(pr, "nixos-config", coordSession, "inst-reentry-"+terminalStatus, &title); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(pr, "nixos-config", terminalStatus, errMsg); err != nil {
		t.Fatalf("TerminateMerge(%s): %v", terminalStatus, err)
	}
	pre, err := d.PendingMergeByPR(pr, "nixos-config")
	if err != nil {
		t.Fatalf("PendingMergeByPR (pre): %v", err)
	}
	if pre == nil || pre.Status != terminalStatus {
		t.Fatalf("expected %s row pre-test, got %+v", terminalStatus, pre)
	}
	d.Close()

	// Install the counting gh stub AFTER seeding so the call count is
	// attributable to runMerge alone. The stub returns OPEN so preflight
	// accepts the PR — the documented retry-after-fix flow.
	counterPath := stubGhBinCounting(t, pr, "OPEN", title)

	out := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{fmt.Sprintf("%d", pr)}); err != nil {
			t.Fatalf("runMerge after %s: %v", terminalStatus, err)
		}
	})

	// gh MUST have been called — the short-circuit must not fire on
	// retry-eligible terminal statuses.
	if n := countGhCalls(t, counterPath); n != 2 {
		// #2420 initial-state probe = pr view + branch protection.
		t.Errorf("gh was called %d times on re-entry of %s PR — expected exactly 2 (#2420 probe = pr view + branch protection; must run on retry)", n, terminalStatus)
	}

	// The row must have flipped back to `watching` (EnqueueMerge's
	// ON CONFLICT branch resets status, clears error, refreshes
	// queue_position) — NOT remained in the terminal state.
	d2, err := openDB()
	if err != nil {
		t.Fatalf("openDB for verify: %v", err)
	}
	defer d2.Close()
	post, err := d2.PendingMergeByPR(pr, "nixos-config")
	if err != nil {
		t.Fatalf("PendingMergeByPR (post): %v", err)
	}
	if post == nil {
		t.Fatal("row disappeared after re-entry")
	}
	if post.Status != "watching" {
		t.Errorf("status: pre=%s post=%s — expected re-enqueue to reset row to watching", terminalStatus, post.Status)
	}
	if post.Error != nil && *post.Error != "" {
		t.Errorf("error field still set after re-enqueue: %q (EnqueueMerge ON CONFLICT should clear it)", *post.Error)
	}

	// User-facing message must be the fresh-enqueue line, not
	// "already in queue".
	if strings.Contains(out, "already in queue") {
		t.Errorf("stdout falsely reports \"already in queue\" on re-entry of %s PR: %q", terminalStatus, out)
	}
	wantEnqueued := fmt.Sprintf("PR #%d enqueued", pr)
	if !strings.Contains(out, wantEnqueued) {
		t.Errorf("stdout %q does not contain %q (the retry should print a fresh enqueue line)", out, wantEnqueued)
	}
}

// TestRunMerge_ProxyReentrySkipsGh covers the in-sandbox proxy path. When
// PRISM_HOST_API is set and a non-terminal row already exists on the host,
// runMerge must skip both the `gh pr view` preflight and the duplicate
// /merge POST — the proxyWaitProbe → /merges/by-pr lookup is the only
// host round-trip on re-entry.
//
// This is the proxy-path counterpart to TestRunMerge_ReentryDistinguishableMessage
// and exercises the same observeExistingMergeRow short-circuit through the
// proxyWaitProbe → /merges/by-pr lookup.
func TestRunMerge_ProxyReentrySkipsGh(t *testing.T) {
	openMergeTestDB(t) // sets PRISM_HOST_API="" — we override below.

	// Stage a non-terminal row in the fake host-API. The proxy probe's
	// /merges/by-pr GET will return this row, which trips the
	// observeExistingMergeRow short-circuit BEFORE the /merge POST.
	server, apiURL := startFakeHostAPIServer(t)
	server.mu.Lock()
	server.byPRRow = map[string]any{
		"PR":            4444,
		"SessionName":   "nixos-config@main",
		"InstanceID":    "inst-proxy",
		"QueuePosition": 1,
		"Status":        "watching",
		"Title":         "feat: proxy re-entry",
		"QueuedAt":      "2026-01-01T00:00:00Z",
	}
	server.mu.Unlock()
	t.Setenv("PRISM_HOST_API", apiURL)
	// Provide a callable session so resolveCallerRepo returns
	// "nixos-config" — without it the repo-gated short-circuit in
	// observeExistingMergeRow falls through and the assertions on
	// zero gh calls / zero /merge POSTs fail (issue #2354).
	t.Setenv("PRISM_SESSION_NAME", "nixos-config@main")
	t.Setenv("TMUX", "")

	// Install a counting gh stub. A regression that runs preflight on
	// the re-entry path would be caught here — zero gh calls expected.
	counterPath := stubGhBinCounting(t, 4444, "OPEN", "feat: proxy re-entry")

	out := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"4444"}); err != nil {
			t.Fatalf("runMerge (proxy re-entry): %v", err)
		}
	})

	// AC-aligned assertions: gh is not called, no /merge POST fires.
	if n := countGhCalls(t, counterPath); n != 0 {
		t.Errorf("gh was called %d times on proxy re-entry — expected 0 (preflight must be skipped)", n)
	}
	server.mu.Lock()
	var mergePostCount, byPRCount int
	for _, req := range server.requests {
		switch req.Path {
		case "/merge":
			mergePostCount++
		case "/merges/by-pr":
			byPRCount++
		}
	}
	server.mu.Unlock()
	if mergePostCount != 0 {
		t.Errorf("proxy fired %d /merge POSTs on re-entry — expected 0 (duplicate enqueue must be skipped)", mergePostCount)
	}
	if byPRCount == 0 {
		t.Errorf("proxy did not hit /merges/by-pr — the re-entry probe did not run via the host-API")
	}

	// stdout must say "already in queue", not "enqueued".
	if strings.Contains(out, "enqueued") {
		t.Errorf("stdout falsely reports \"enqueued\" on proxy re-entry: %q", out)
	}
	if !strings.Contains(out, "already in queue") {
		t.Errorf("stdout %q does not say \"already in queue\"", out)
	}
}
