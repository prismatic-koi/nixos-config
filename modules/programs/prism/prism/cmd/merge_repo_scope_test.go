package cmd

// Tests for the cmd-layer repo scoping added in issue #2354.
//
// These tests exercise the runMerge command path: the incident of
// 2026-07-06 was caused by observeExistingMergeRow short-circuiting on a
// terminal row from another repo. The regression test seeds the incident
// shape at the DB level, calls runMerge from the "victim" repo, and
// asserts:
//
//   - fresh "PR #N enqueued ..." line on stdout (not a terminal
//     short-circuit),
//   - a new `watching` row for the caller's repo,
//   - the foreign repo's terminal row is untouched.
//
// AC #6 (already-merged output includes PR title) is tested via
// emitMergeWaitTerminal directly \u2014 that function is shared between the
// #1875 re-entry short-circuit and the --wait terminal path.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestRunMerge_CrossRepoCollision_EnqueuesFreshRow reproduces the incident
// of 2026-07-06: a terminal `merged` row for PR N exists under repo A,
// and repo B calls `prism merge N`. The short-circuit must NOT fire on
// repo A's row; instead, repo B's coordinator must enqueue its own
// watching row and print the fresh-enqueue line.
func TestRunMerge_CrossRepoCollision_EnqueuesFreshRow(t *testing.T) {
	openMergeTestDB(t)

	// Repo A: seed a terminal `merged` row. This is the foreign row
	// that used to trip the observeExistingMergeRow short-circuit
	// (issue #2354).
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	titleA := "chore: bump deps"
	if _, err := d.EnqueueMerge(47, "repo-a", "repo-a@main", "inst-a", &titleA); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	if err := d.TerminateMerge(47, "repo-a", "merged", ""); err != nil {
		t.Fatalf("TerminateMerge repo-a: %v", err)
	}
	d.Close()

	// Repo B: seed a coordinator session and call runMerge. We use the
	// raw UpsertStatusSeedRootAgentName path (not seedCoordinatorWithInstanceID,
	// which hard-codes repo="nixos-config") so the coordinator's
	// agent_status.repo is repo-b — the value that runMerge will use to
	// scope the enqueue.
	const coordSession = "repo-b@main"
	d2, err := openDB()
	if err != nil {
		t.Fatalf("openDB seed coord: %v", err)
	}
	if err := d2.UpsertStatusSeedRootAgentName(coordSession, "repo-b", "/worktree/repo-b", "idle", nil, nil, "coordinator", "", ""); err != nil {
		d2.Close()
		t.Fatalf("UpsertStatusSeedRootAgentName repo-b: %v", err)
	}
	if err := d2.SetInstanceID(coordSession, "inst-b"); err != nil {
		d2.Close()
		t.Fatalf("SetInstanceID repo-b: %v", err)
	}
	d2.Close()
	t.Setenv("PRISM_SESSION_NAME", coordSession)
	t.Setenv("TMUX", "")

	// The gh stub returns OPEN so preflight succeeds. If the incident
	// regresses, the short-circuit fires BEFORE preflight and gh is
	// never invoked, which the assertions below detect.
	counterPath := stubGhBinCounting(t, 47, "OPEN", "feat: real work in repo-b")

	out := captureStdout(t, func() {
		if err := runMerge(mergeCmd, []string{"47"}); err != nil {
			t.Fatalf("runMerge in repo-b: %v", err)
		}
	})

	// gh MUST have run: preflight fires exactly once. If a foreign-repo
	// terminal row short-circuited the enqueue, gh would not run.
	if n := countGhCalls(t, counterPath); n != 1 {
		t.Errorf("gh call count = %d, want 1 (foreign-repo terminal row must not short-circuit preflight \u2014 the incident of 2026-07-06 has regressed)", n)
	}

	// stdout must contain the fresh-enqueue banner and the queue
	// position. Terminal short-circuit output would be
	// "PR #47 merged." which is what the incident produced.
	if !strings.Contains(out, "PR #47 enqueued") {
		t.Errorf("stdout does not contain \"PR #47 enqueued\": %q\n(incident: instead printed a false-terminal short-circuit)", out)
	}
	if strings.Contains(out, "PR #47 merged") {
		t.Errorf("stdout contains \"PR #47 merged\" \u2014 the exact incident output. Foreign-repo row is being short-circuited on.\nstdout: %q", out)
	}

	// The DB must now hold TWO rows: repo-a's untouched merged row, and
	// repo-b's fresh watching row.
	d, err = openDB()
	if err != nil {
		t.Fatalf("openDB verify: %v", err)
	}
	defer d.Close()

	rowA, err := d.PendingMergeByPR(47, "repo-a")
	if err != nil {
		t.Fatalf("PendingMergeByPR repo-a: %v", err)
	}
	if rowA == nil {
		t.Fatal("repo-a row disappeared \u2014 runMerge must not touch foreign-repo rows")
	}
	if rowA.Status != "merged" {
		t.Errorf("repo-a row: status = %q, want merged (foreign row must be preserved)", rowA.Status)
	}

	rowB, err := d.PendingMergeByPR(47, "repo-b")
	if err != nil {
		t.Fatalf("PendingMergeByPR repo-b: %v", err)
	}
	if rowB == nil {
		t.Fatal("repo-b row missing \u2014 runMerge did not enqueue")
	}
	if rowB.Status != "watching" {
		t.Errorf("repo-b row: status = %q, want watching", rowB.Status)
	}
	if rowB.SessionName != coordSession {
		t.Errorf("repo-b row: session_name = %q, want %q", rowB.SessionName, coordSession)
	}
	if rowB.InstanceID != "inst-b" {
		t.Errorf("repo-b row: instance_id = %q, want inst-b", rowB.InstanceID)
	}
}

// TestWaitForMergeTerminal_ObservesOnlyOwnRepoRow verifies AC:
//
//	"prism merge <pr> --wait observes only the terminal state of the row
//	 belonging to the current repo."
//
// Seed a merged row for repo A's PR N and a watching row for repo B's PR N,
// then call waitForMergeTerminal from repo B's perspective. The wait must
// keep polling until repo B's row terminates — the merged repo-A row must
// be invisible.
func TestWaitForMergeTerminal_ObservesOnlyOwnRepoRow(t *testing.T) {
	openMergeTestDB(t)

	const pr = 47
	d, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	// repo-a already has PR 47 merged.
	if _, err := d.EnqueueMerge(pr, "repo-a", "repo-a@main", "inst-a", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-a: %v", err)
	}
	if err := d.TerminateMerge(pr, "repo-a", "merged", ""); err != nil {
		t.Fatalf("TerminateMerge repo-a: %v", err)
	}
	// repo-b's PR 47 is still watching.
	if _, err := d.EnqueueMerge(pr, "repo-b", "repo-b@main", "inst-b", nil); err != nil {
		t.Fatalf("EnqueueMerge repo-b: %v", err)
	}
	d.Close()

	// Flip repo-b's row to `failed` in a goroutine so waitForMergeTerminal
	// can observe a terminal state for repo-b specifically. If the wait
	// were still repo-blind, it would observe repo-a's `merged` immediately
	// on the first poll and return exit code 0 rather than the
	// terminal-fail code we expect from repo-b's `failed`.
	go func() {
		time.Sleep(150 * time.Millisecond)
		d2, dErr := openDB()
		if dErr != nil {
			t.Errorf("openDB in goroutine: %v", dErr)
			return
		}
		defer d2.Close()
		if err := d2.TerminateMerge(pr, "repo-b", "failed", "CI failed on repo-b"); err != nil {
			t.Errorf("TerminateMerge repo-b: %v", err)
		}
	}()

	out := captureStdout(t, func() {
		err := waitForMergeTerminal(pr, "repo-b", false, 5*time.Second)
		if err == nil {
			t.Fatal("waitForMergeTerminal: expected non-nil error for `failed` terminal from repo-b (a nil error would mean it observed repo-a's `merged` — the cross-repo bleed the fix is closing)")
		}
		if exitCodeOf(err) != waitExitTerminalFail {
			t.Errorf("waitForMergeTerminal: exit code = %d, want %d (terminal-fail); err=%v", exitCodeOf(err), waitExitTerminalFail, err)
		}
	})

	// The output must reference the CI-failed message from repo-b, not
	// the empty merged message from repo-a.
	if !strings.Contains(out, "CI failed on repo-b") {
		t.Errorf("stdout does not contain repo-b's error message — wait observed the wrong repo's row.\nout: %q", out)
	}
	if strings.Contains(out, "PR #47 merged") {
		t.Errorf("stdout contains \"PR #47 merged\" — wait observed repo-a's terminal row (the cross-repo bleed).\nout: %q", out)
	}
}

// TestEmitMergeWaitTerminal_IncludesPRTitleOnMerged verifies AC #6:
//
//	"The already-merged short-circuit output includes the stored PR title."
//
// The rationale is defence-in-depth: if a cross-repo mismatch does somehow
// re-appear (a new bypass path we haven't foreseen), including the PR
// title in the "PR #N merged" line makes the mismatch visually
// detectable by the caller \u2014 the incident of 2026-07-06 printed a bare
// "PR #47 merged." that gave the coordinator no signal that the merged
// row belonged to a different repo's PR.
func TestEmitMergeWaitTerminal_IncludesPRTitleOnMerged(t *testing.T) {
	title := "feat: something specific"
	row := &db.PendingMerge{
		PR:     47,
		Status: "merged",
		Title:  &title,
	}
	out := captureStdout(t, func() {
		if err := emitMergeWaitTerminal(row, false); err != nil {
			t.Fatalf("emitMergeWaitTerminal: %v", err)
		}
	})
	// The output must contain both the PR number AND the title.
	if !strings.Contains(out, "PR #47 merged") {
		t.Errorf("stdout missing merged banner: %q", out)
	}
	if !strings.Contains(out, title) {
		t.Errorf("stdout missing PR title %q \u2014 AC #6 requires the already-merged output to include the stored title so cross-repo mismatches are visually detectable: %q", title, out)
	}
	// Format must be "PR #N merged: <title>" (one line, colon-separated).
	// Cross-repo mismatches are only easily detectable when title is on
	// the same line as the PR number.
	expected := fmt.Sprintf("PR #%d merged: %s", row.PR, title)
	if !strings.Contains(out, expected) {
		t.Errorf("stdout does not match expected format %q; got %q", expected, out)
	}
}

// TestEmitMergeWaitTerminal_FallsBackToTerseFormWhenNoTitle preserves the
// existing terse output when the stored row has no title \u2014 older rows
// created before we captured titles must still render sensibly.
func TestEmitMergeWaitTerminal_FallsBackToTerseFormWhenNoTitle(t *testing.T) {
	row := &db.PendingMerge{
		PR:     47,
		Status: "merged",
		Title:  nil,
	}
	out := captureStdout(t, func() {
		if err := emitMergeWaitTerminal(row, false); err != nil {
			t.Fatalf("emitMergeWaitTerminal: %v", err)
		}
	})
	if strings.TrimSpace(out) != "PR #47 merged." {
		t.Errorf("stdout with nil title: got %q, want %q", out, "PR #47 merged.\n")
	}
}

// TestEmitMergeWaitTerminal_JSONShapeUnchanged verifies that the JSON path
// keeps its existing schema \u2014 the title-in-output change only affects
// the human-readable branch. JSON consumers pull the title from the
// separate `title` field and should not see any change.
func TestEmitMergeWaitTerminal_JSONShapeUnchanged(t *testing.T) {
	title := "feat: something specific"
	row := &db.PendingMerge{
		PR:     47,
		Status: "merged",
		Title:  &title,
	}
	out := captureStdout(t, func() {
		if err := emitMergeWaitTerminal(row, true /* jsonMode */); err != nil {
			t.Fatalf("emitMergeWaitTerminal --json: %v", err)
		}
	})
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &payload); err != nil {
		t.Fatalf("stdout not JSON: %v; got %q", err, out)
	}
	// The banner change must NOT appear on the JSON path.
	if _, ok := payload["title"]; !ok {
		t.Errorf("JSON output missing title key: %v", payload)
	}
	if payload["status"] != "merged" {
		t.Errorf("JSON status = %v, want merged", payload["status"])
	}
	if payload["title"] != title {
		t.Errorf("JSON title = %v, want %q", payload["title"], title)
	}
}
