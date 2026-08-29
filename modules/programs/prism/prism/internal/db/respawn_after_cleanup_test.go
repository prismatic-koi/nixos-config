package db_test

// Regression tests for the respawn-after-cleanup transition.
//
// `prism cleanup --yes --session <name>` does not touch agent_status.state;
// it sets ended_at. Re-spawning on the same branch name calls
// UpsertStatusSeedRootAgentName with state="idle", which routes through
// the state-machine advisory checkTransition. If the
// ValidTransitions[StateError] map does not include StateIdle, the
// re-spawn-after-error path logs
//
//   [prism] UpsertStatusSeedRootAgentName: invalid transition for session
//   "...": agent state machine: invalid transition "error" → "idle"
//
// even though the structurally analogous finished→idle and interrupted→idle
// transitions were already allowed. These tests assert the observable
// invariant from the issue's AC #1: after the post-cleanup row is left
// with a prior terminal state (error / finished / interrupted), a re-seed
// to idle produces no advisory warning on stderr.
//
// Why a stderr-capture test, given that checkTransition is currently
// advisory-only? Because that is the user-visible regression surface:
// operators report the warning ("warning fires on every re-spawn"), and
// any future tightening of checkTransition that converted advisory logs
// into errors would silently regress this path without a test that
// asserts the no-warning invariant. The capture pins the observable.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// captureStderr runs fn with os.Stderr redirected to a pipe, drains the pipe
// concurrently to avoid the pipe-buffer deadlock documented in
// docs/stdout-capture-testing.md, and returns the captured
// bytes. Stderr is always restored even if fn panics.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	// Drain the pipe concurrently so a write larger than the kernel buffer
	// cannot block fn (see docs/stdout-capture-testing.md).
	doneCh := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		doneCh <- buf.Bytes()
	}()

	defer func() {
		os.Stderr = orig
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	captured := <-doneCh
	if err := r.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	return string(captured)
}

// simulateCleanup mirrors what cmd.headlessCleanupWithJSON does to the
// agent_status row for the purpose of this unit test: it releases the port,
// nulls harness_session_id, and sets ended_at. The state column is
// intentionally left at its prior terminal value — that is the residue the
// re-spawn must tolerate.
func simulateCleanup(t *testing.T, d *db.DB, session string) {
	t.Helper()
	// ReleasePort errors when the session does not exist; tolerate that for
	// tests that never allocated a port.
	_ = d.ReleasePort(session)
	if err := d.ClearHarnessSessionID(session); err != nil {
		t.Fatalf("ClearHarnessSessionID: %v", err)
	}
	if err := d.SetEnded(session); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
}

// TestRespawnAfterCleanup_NoTransitionWarning is AC #1:
// after cleanup leaves the row at a terminal state (error / finished /
// interrupted), re-seeding via UpsertStatusSeedRootAgentName with state=idle
// must not log "invalid transition" for any of those prior states.
//
// This is the user-visible regression test: it captures stderr and asserts
// the warning is absent.
func TestRespawnAfterCleanup_NoTransitionWarning(t *testing.T) {
	// All four non-deleted terminal states must be tolerated; the parametric
	// loop also guards against a regression where only one is fixed in
	// isolation (e.g. a future change that removes error→idle but keeps
	// finished→idle).
	priorStates := []string{"error", "finished", "interrupted"}

	for _, priorState := range priorStates {
		t.Run(priorState, func(t *testing.T) {
			d := openTestDB(t)
			const session = "myrepo@feature"
			const repo = "myrepo"
			const worktree = "/code/myrepo/feature"

			// Seed the row at the prior terminal state. We must go through
			// active because idle→finished is not a valid direct transition
			// in the state machine (idle→error and idle→interrupted ARE
			// allowed directly, but routing through active uniformly keeps
			// the setup faithful to the real lifecycle).
			if err := d.UpsertStatusSeedRootAgentName(session, repo, worktree, "idle", nil, nil, "worker", "pi", "bwrap"); err != nil {
				t.Fatalf("seed idle: %v", err)
			}
			if err := d.UpsertStatusWithRootAgent(session, repo, worktree, "active", nil, nil, nil, nil); err != nil {
				t.Fatalf("transition to active: %v", err)
			}
			if err := d.UpsertStatusWithRootAgent(session, repo, worktree, priorState, nil, nil, nil, nil); err != nil {
				t.Fatalf("transition to %s: %v", priorState, err)
			}

			// Apply the agent_status mutations that prism cleanup performs.
			simulateCleanup(t, d, session)

			// Re-spawn on the same branch name: this is exactly what
			// cmd/event.go::tmux-session-start invokes when the operator
			// re-runs `prism spawn --branch <name>` after cleanup.
			stderr := captureStderr(t, func() {
				if err := d.UpsertStatusSeedRootAgentName(session, repo, worktree, "idle", nil, nil, "worker", "pi", "bwrap"); err != nil {
					t.Fatalf("re-spawn UpsertStatusSeedRootAgentName: %v", err)
				}
			})

			if strings.Contains(stderr, "invalid transition") {
				t.Errorf("re-spawn after cleanup with prior state %q produced an 'invalid transition' warning:\n%s",
					priorState, stderr)
			}

			// Sanity: the row's state column must reflect the re-seed.
			s, err := d.CurrentStatus(session)
			if err != nil {
				t.Fatalf("CurrentStatus: %v", err)
			}
			if s == nil {
				t.Fatal("CurrentStatus: nil after re-spawn")
			}
			if s.State != "idle" {
				t.Errorf("State after re-spawn: got %q, want \"idle\"", s.State)
			}
		})
	}
}

// TestRespawnAfterCleanup_DifferentBranchName is AC negative test #1:
// over-broad-fix guard. Re-spawning on a different branch name must still
// work — and must not be regressed by any state-machine widening. The
// "different branch" path goes through a fresh INSERT (no prior row, so
// checkTransition's fromState is empty and validation is skipped).
func TestRespawnAfterCleanup_DifferentBranchName(t *testing.T) {
	d := openTestDB(t)
	const oldSession = "myrepo@feature-old"
	const newSession = "myrepo@feature-new"
	const repo = "myrepo"

	// Seed and bring oldSession to error, then simulate cleanup.
	if err := d.UpsertStatusSeedRootAgentName(oldSession, repo, "/code/myrepo/feature-old", "idle", nil, nil, "worker", "pi", "bwrap"); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	// idle→error is permitted directly (container startup failure).
	if err := d.UpsertStatusWithRootAgent(oldSession, repo, "/code/myrepo/feature-old", "error", nil, nil, nil, nil); err != nil {
		t.Fatalf("transition old to error: %v", err)
	}
	simulateCleanup(t, d, oldSession)

	// Re-spawn on a DIFFERENT branch name — no prior row for this session,
	// so the fresh-insert path is exercised. Must succeed with no warning.
	stderr := captureStderr(t, func() {
		if err := d.UpsertStatusSeedRootAgentName(newSession, repo, "/code/myrepo/feature-new", "idle", nil, nil, "worker", "pi", "bwrap"); err != nil {
			t.Fatalf("re-spawn on different branch: %v", err)
		}
	})
	if strings.Contains(stderr, "invalid transition") {
		t.Errorf("re-spawn on different branch produced unexpected warning:\n%s", stderr)
	}

	// Sanity: the new row exists and is at idle.
	s, err := d.CurrentStatus(newSession)
	if err != nil {
		t.Fatalf("CurrentStatus(new): %v", err)
	}
	if s == nil || s.State != "idle" {
		t.Errorf("new session state: got %+v, want state=idle", s)
	}

	// Sanity: the old row is still present with ended_at set (cleanup did
	// not delete it under Option C).
	old, err := d.CurrentStatus(oldSession)
	if err != nil {
		t.Fatalf("CurrentStatus(old): %v", err)
	}
	if old == nil {
		t.Fatal("old session row missing")
	}
	if old.EndedAt == nil {
		t.Errorf("old session EndedAt: got nil, want non-nil (cleanup must have stamped it)")
	}
}

// TestRespawnAfterCleanup_ReviewChildrenStillEndedByCleanup is AC negative
// test #2: review-child cleanup is not regressed. cleanup uses SetEnded with
// a LIKE-cascade for "<parent>~review-%" rows, and the fix here does NOT
// touch the cleanup path — but adding a guard here makes the invariant
// explicit, so any future reordering of headlessCleanupWithJSON cannot
// silently break review-child end-cascade.
func TestRespawnAfterCleanup_ReviewChildrenStillEndedByCleanup(t *testing.T) {
	d := openTestDB(t)
	const parent = "myrepo@feature"
	child1 := parent + "~review-1-review-code"
	child2 := parent + "~review-1-review-goal"

	// Seed parent and two review children, all in active state.
	for _, name := range []string{parent, child1, child2} {
		if err := d.UpsertStatusSeedRootAgentName(name, "myrepo", "/code/myrepo/feature", "idle", nil, nil, "worker", "pi", "bwrap"); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
		if err := d.UpsertStatusWithRootAgent(name, "myrepo", "/code/myrepo/feature", "active", nil, nil, nil, nil); err != nil {
			t.Fatalf("transition %q to active: %v", name, err)
		}
	}

	// SetEnded on the parent must cascade to the review children via the
	// existing LIKE pattern. This is the invariant cmd.headlessCleanup
	// relies on; we assert it here so a future refactor of cleanup that
	// hides this behind a different helper still exercises the cascade.
	if err := d.SetEnded(parent); err != nil {
		t.Fatalf("SetEnded(parent): %v", err)
	}

	for _, name := range []string{parent, child1, child2} {
		s, err := d.CurrentStatus(name)
		if err != nil {
			t.Fatalf("CurrentStatus(%q): %v", name, err)
		}
		if s == nil {
			t.Fatalf("row %q missing after SetEnded", name)
		}
		if s.EndedAt == nil {
			t.Errorf("EndedAt for %q: got nil, want non-nil (cleanup cascade broken)", name)
		}
	}
}

// TestRespawnAfterCleanup_ArchiveLookupOrdering is AC "no regression in
// archive lookup": cmd.runSessionArchive reads harness_session_id from
// agent_status before SetEnded is called by cleanup. Under Option C the
// row is not deleted, so the read continues to work. This test pins the
// invariant: a row written with harness_session_id is readable both
// before and after SetEnded, so any future re-ordering of cleanup that
// moves the archive read past SetEnded does not silently break.
func TestRespawnAfterCleanup_ArchiveLookupOrdering(t *testing.T) {
	d := openTestDB(t)
	const session = "myrepo@feature"
	const sid = "019e0000-0000-0000-0000-000000000042"

	if err := d.UpsertStatusSeedRootAgentName(session, "myrepo", "/code/myrepo/feature", "idle", nil, nil, "worker", "pi", "bwrap"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := d.UpdateHarnessSessionID(session, sid); err != nil {
		t.Fatalf("UpdateHarnessSessionID: %v", err)
	}

	// Pre-SetEnded read (the path cleanup exercises today).
	pre, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus pre-SetEnded: %v", err)
	}
	if pre == nil || pre.HarnessSessionID == nil || *pre.HarnessSessionID != sid {
		t.Fatalf("pre-SetEnded HarnessSessionID: got %+v, want %q", pre, sid)
	}

	if err := d.SetEnded(session); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	// Post-SetEnded read: the row must still be present (Option C does not
	// delete the row). This is the invariant the archive path needs if it
	// ever moves the read past SetEnded.
	post, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus post-SetEnded: %v", err)
	}
	if post == nil {
		t.Fatal("row removed by SetEnded; Option C contract violated (the row must remain so cleanup's other reads succeed)")
	}
	if post.EndedAt == nil {
		t.Errorf("EndedAt post-SetEnded: got nil, want non-nil")
	}
}

// TestRespawnAfterCleanup_WithRealDBFile pins the same invariant against
// a real on-disk SQLite file (not just the openTestDB temp DB). This guards
// against an unlikely-but-real regression where the in-memory schema and
// the on-disk schema diverge on the agent_status row's ended_at semantics.
func TestRespawnAfterCleanup_WithRealDBFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	const session = "myrepo@feature"
	if err := d.UpsertStatusSeedRootAgentName(session, "myrepo", "/code/myrepo/feature", "idle", nil, nil, "worker", "pi", "bwrap"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// idle→error is permitted directly (container startup failure path).
	if err := d.UpsertStatusWithRootAgent(session, "myrepo", "/code/myrepo/feature", "error", nil, nil, nil, nil); err != nil {
		t.Fatalf("transition to error: %v", err)
	}
	simulateCleanup(t, d, session)

	stderr := captureStderr(t, func() {
		if err := d.UpsertStatusSeedRootAgentName(session, "myrepo", "/code/myrepo/feature", "idle", nil, nil, "worker", "pi", "bwrap"); err != nil {
			t.Fatalf("re-spawn UpsertStatusSeedRootAgentName: %v", err)
		}
	})
	if strings.Contains(stderr, "invalid transition") {
		t.Errorf("real-DB re-spawn after cleanup produced unexpected warning:\n%s", stderr)
	}
}
