package mergequeue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// openTestDB creates a temporary test database and returns it.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// seedCoordinator inserts an agent_status row for a coordinator session so
// notify() can look up its harness port and harness_session_id.
func seedCoordinator(t *testing.T, d *db.DB, sessionName, instanceID string, port int, harnessSessionID string) {
	t.Helper()
	if err := d.UpsertStatus(sessionName, "myrepo", "/tmp/wt", "active", nil, &harnessSessionID); err != nil {
		t.Fatalf("seed coordinator status: %v", err)
	}
	if err := d.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("seed coordinator instance_id: %v", err)
	}
	// Force the port to the value we want via execSQL.
	_ = execSQL(d, "UPDATE agent_status SET harness_port = ?, harness_session_id = ? WHERE session_name = ?",
		port, harnessSessionID, sessionName)
}

// execSQL runs a raw SQL statement against the test DB via QueryRow.
// DML statements (UPDATE/INSERT) return sql.ErrNoRows from Scan, which we ignore.
func execSQL(d *db.DB, q string, args ...any) error {
	var dummy int
	err := d.QueryRow(q, args...).Scan(&dummy)
	if err != nil && strings.Contains(err.Error(), "no rows") {
		return nil
	}
	return err
}

// ── DB-layer tests ─────────────────────────────────────────────────────────────

// TestEnqueueMerge_FIFO verifies that queue_position increases monotonically
// and that MergeQueueHead returns the row with the lowest queue_position.
func TestEnqueueMerge_FIFO(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-fifo"
	session := "myrepo@main"

	// Enqueue PRs in reverse order to ensure FIFO is by queue_position, not PR number.
	m3, err := d.EnqueueMerge(300, session, instanceID, nil)
	if err != nil {
		t.Fatalf("EnqueueMerge(300): %v", err)
	}
	time.Sleep(time.Millisecond) // ensure distinct timestamps
	m2, err := d.EnqueueMerge(200, session, instanceID, nil)
	if err != nil {
		t.Fatalf("EnqueueMerge(200): %v", err)
	}
	time.Sleep(time.Millisecond)
	m1, err := d.EnqueueMerge(100, session, instanceID, nil)
	if err != nil {
		t.Fatalf("EnqueueMerge(100): %v", err)
	}

	// queue_position must be strictly increasing in insertion order.
	if !(m3.QueuePosition < m2.QueuePosition && m2.QueuePosition < m1.QueuePosition) {
		t.Errorf("queue_positions not strictly increasing: m3=%d m2=%d m1=%d",
			m3.QueuePosition, m2.QueuePosition, m1.QueuePosition)
	}

	// Head must be the first-inserted row (PR 300).
	head, err := d.MergeQueueHead(session)
	if err != nil {
		t.Fatalf("MergeQueueHead: %v", err)
	}
	if head == nil {
		t.Fatal("MergeQueueHead: got nil, want PR 300")
	}
	if head.PR != 300 {
		t.Errorf("MergeQueueHead.PR: got %d, want 300", head.PR)
	}
}

// TestEnqueueMerge_Idempotent verifies that enqueueing the same PR twice returns
// the existing row without inserting a duplicate.
func TestEnqueueMerge_Idempotent(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-idem"
	session := "myrepo@main"

	m1, err := d.EnqueueMerge(42, session, instanceID, nil)
	if err != nil {
		t.Fatalf("first EnqueueMerge: %v", err)
	}

	m2, err := d.EnqueueMerge(42, session, instanceID, nil)
	if err != nil {
		t.Fatalf("second EnqueueMerge: %v", err)
	}

	// Must be the same row — identical PR, status, and queue_position.
	if m1.QueuePosition != m2.QueuePosition {
		t.Errorf("second enqueue returned different queue_position: got %d, want %d", m2.QueuePosition, m1.QueuePosition)
	}
	if m2.Status != "watching" {
		t.Errorf("second enqueue status: got %q, want watching", m2.Status)
	}

	// Only one row should exist.
	rows, err := d.MergeQueueForInstance(instanceID, session, "")
	if err != nil {
		t.Fatalf("MergeQueueForInstance: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(rows))
	}
}

// TestEnqueueMerge_TerminalRowReplaceable verifies that a terminal row can be
// overwritten by a fresh EnqueueMerge.
func TestEnqueueMerge_TerminalRowReplaceable(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-term"
	session := "myrepo@main"

	// Enqueue and then fail.
	if _, err := d.EnqueueMerge(99, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(99, "failed", "CI failed"); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}

	// Re-enqueue — should produce a fresh watching row.
	m, err := d.EnqueueMerge(99, session, instanceID, nil)
	if err != nil {
		t.Fatalf("second EnqueueMerge: %v", err)
	}
	if m.Status != "watching" {
		t.Errorf("re-enqueued status: got %q, want watching", m.Status)
	}
	if m.Error != nil {
		t.Errorf("re-enqueued error: got %v, want nil", m.Error)
	}
}

// TestAbandonWatchingMerges verifies that AbandonWatchingMerges transitions all
// watching rows for the given instanceID to abandoned.
func TestAbandonWatchingMerges(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-abandon"
	session := "myrepo@main"

	// Enqueue two PRs.
	if _, err := d.EnqueueMerge(1, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(1): %v", err)
	}
	if _, err := d.EnqueueMerge(2, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(2): %v", err)
	}

	// Abandon all watching rows.
	if err := d.AbandonWatchingMerges(instanceID); err != nil {
		t.Fatalf("AbandonWatchingMerges: %v", err)
	}

	// Both rows should now be abandoned.
	for _, pr := range []int{1, 2} {
		row, err := d.PendingMergeByPR(pr)
		if err != nil {
			t.Fatalf("PendingMergeByPR(%d): %v", pr, err)
		}
		if row == nil {
			t.Fatalf("PendingMergeByPR(%d): got nil", pr)
		}
		if row.Status != "abandoned" {
			t.Errorf("PR %d status: got %q, want abandoned", pr, row.Status)
		}
		if row.Error == nil || *row.Error != "coordinator session ended" {
			t.Errorf("PR %d error: got %v, want 'coordinator session ended'", pr, row.Error)
		}
		if row.EndedAt == nil {
			t.Errorf("PR %d ended_at: got nil, want set", pr)
		}
	}

	// Head must now be nil.
	head, err := d.MergeQueueHead(session)
	if err != nil {
		t.Fatalf("MergeQueueHead after abandon: %v", err)
	}
	if head != nil {
		t.Errorf("MergeQueueHead after abandon: got PR %d, want nil", head.PR)
	}
}

// TestCancelMerge verifies that CancelMerge transitions a watching row to
// cancelled and returns false for non-matching rows.
func TestCancelMerge(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-cancel"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(55, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	// Cancel it.
	cancelled, err := d.CancelMerge(55, instanceID)
	if err != nil {
		t.Fatalf("CancelMerge: %v", err)
	}
	if !cancelled {
		t.Error("CancelMerge: returned false, want true")
	}

	row, err := d.PendingMergeByPR(55)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "cancelled" {
		t.Errorf("status after cancel: got %q, want cancelled", row.Status)
	}

	// Cancelling again returns false (already terminal).
	cancelled2, err := d.CancelMerge(55, instanceID)
	if err != nil {
		t.Fatalf("second CancelMerge: %v", err)
	}
	if cancelled2 {
		t.Error("second CancelMerge: returned true, want false")
	}

	// Wrong instanceID returns false.
	if _, err := d.EnqueueMerge(66, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(66): %v", err)
	}
	cancelled3, err := d.CancelMerge(66, "other-instance")
	if err != nil {
		t.Fatalf("CancelMerge wrong instance: %v", err)
	}
	if cancelled3 {
		t.Error("CancelMerge with wrong instanceID: returned true, want false")
	}
}

// TestMergeQueueHead_ScopedBySessionName verifies that MergeQueueHead only
// returns rows for the given session_name, not for a different session even
// if that session has its own watching rows. This ensures coordinators don't
// see each other's queue entries.
func TestMergeQueueHead_ScopedBySessionName(t *testing.T) {
	d := openTestDB(t)
	sessionA := "repo-a@main"
	sessionB := "repo-b@main"

	// Enqueue a PR under session A (any instance_id).
	if _, err := d.EnqueueMerge(10, sessionA, "inst-A", nil); err != nil {
		t.Fatalf("EnqueueMerge sessionA: %v", err)
	}

	// Head for session B must be nil (different coordinator).
	head, err := d.MergeQueueHead(sessionB)
	if err != nil {
		t.Fatalf("MergeQueueHead sessionB: %v", err)
	}
	if head != nil {
		t.Errorf("MergeQueueHead sessionB: got PR %d, want nil", head.PR)
	}

	// Head for session A must return PR 10.
	headA, err := d.MergeQueueHead(sessionA)
	if err != nil {
		t.Fatalf("MergeQueueHead sessionA: %v", err)
	}
	if headA == nil {
		t.Fatal("MergeQueueHead sessionA: got nil, want PR 10")
	}
	if headA.PR != 10 {
		t.Errorf("MergeQueueHead sessionA: got PR %d, want 10", headA.PR)
	}
}

// TestMergeQueueHead_AcrossInstanceIDs verifies the core fix: MergeQueueHead
// returns a row even when the enqueued instance_id differs from the watcher's
// instance_id. This is the scenario triggered when prism merge mints a fresh
// UUID that doesn't match the sidecar's startup instance_id.
func TestMergeQueueHead_AcrossInstanceIDs(t *testing.T) {
	d := openTestDB(t)
	session := "myrepo@main"
	mintedInstanceID := "freshly-minted-uuid"
	watcherInstanceID := "sidecar-startup-uuid"

	// prism merge enqueues with a freshly-minted instance_id.
	if _, err := d.EnqueueMerge(42, session, mintedInstanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge with minted ID: %v", err)
	}

	// The watcher queries by session_name, not instance_id — must find the row.
	head, err := d.MergeQueueHead(session)
	if err != nil {
		t.Fatalf("MergeQueueHead: %v", err)
	}
	if head == nil {
		t.Fatal("MergeQueueHead: got nil — watcher cannot see row enqueued with different instance_id")
	}
	if head.PR != 42 {
		t.Errorf("MergeQueueHead.PR: got %d, want 42", head.PR)
	}
	// The row still carries the minted instance_id (no mutation needed).
	if head.InstanceID != mintedInstanceID {
		t.Errorf("head.InstanceID: got %q, want %q", head.InstanceID, mintedInstanceID)
	}
	// The watcher's own instance_id is irrelevant for queue lookup.
	_ = watcherInstanceID
}

// TestMergeQueueForInstance_Filters verifies the filter modes.
func TestMergeQueueForInstance_Filters(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-filter"
	session := "myrepo@main"

	// Enqueue 4 PRs. Fail one, leave one watching, cancel one, abandon one.
	if _, err := d.EnqueueMerge(1, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(1): %v", err)
	}
	if _, err := d.EnqueueMerge(2, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(2): %v", err)
	}
	if _, err := d.EnqueueMerge(3, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(3): %v", err)
	}
	if _, err := d.EnqueueMerge(4, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(4): %v", err)
	}

	// Fail PR 2.
	if err := d.TerminateMerge(2, "failed", "CI failed"); err != nil {
		t.Fatalf("TerminateMerge(2): %v", err)
	}
	// Cancel PR 3.
	if _, err := d.CancelMerge(3, instanceID); err != nil {
		t.Fatalf("CancelMerge(3): %v", err)
	}
	// Abandon PR 4 via AbandonWatchingMerges (simulates coordinator shutdown).
	// First, mark PR 1 as merged so only PR 4 is still watching.
	if err := d.TerminateMerge(1, "merged", ""); err != nil {
		t.Fatalf("TerminateMerge(1,merged): %v", err)
	}
	if err := d.AbandonWatchingMerges(instanceID); err != nil {
		t.Fatalf("AbandonWatchingMerges: %v", err)
	}

	// Default filter: only watching — should be empty (PR 4 was abandoned).
	watching, err := d.MergeQueueForInstance(instanceID, session, "")
	if err != nil {
		t.Fatalf("MergeQueueForInstance(watching): %v", err)
	}
	if len(watching) != 0 {
		t.Errorf("watching filter: got %d rows, want 0 (all terminated)", len(watching))
	}

	// Failed filter.
	failed, err := d.MergeQueueForInstance(instanceID, session, "failed")
	if err != nil {
		t.Fatalf("MergeQueueForInstance(failed): %v", err)
	}
	if len(failed) != 1 || failed[0].PR != 2 {
		t.Errorf("failed filter: got %d rows, want 1 (PR 2)", len(failed))
	}

	// All filter includes all terminal states including abandoned (per AC).
	// merged(1) + failed(2) + cancelled(3) + abandoned(4) = 4 rows.
	all, err := d.MergeQueueForInstance(instanceID, session, "all")
	if err != nil {
		t.Fatalf("MergeQueueForInstance(all): %v", err)
	}
	if len(all) != 4 {
		t.Errorf("all filter: got %d rows, want 4 (includes abandoned per AC)", len(all))
	}
	// Verify abandoned row is included in --all.
	foundAbandoned := false
	for _, m := range all {
		if m.PR == 4 && m.Status == "abandoned" {
			foundAbandoned = true
		}
	}
	if !foundAbandoned {
		t.Error("all filter: abandoned PR 4 not found — --all must include abandoned rows")
	}
}

// TestMergeQueueForInstance_AbandonedFilter verifies that the abandoned filter
// returns rows from a different instanceID for the same session_name.
func TestMergeQueueForInstance_AbandonedFilter(t *testing.T) {
	d := openTestDB(t)
	session := "myrepo@main"
	oldInstance := "inst-old"
	newInstance := "inst-new"

	// Enqueue under old instance and abandon.
	if _, err := d.EnqueueMerge(77, session, oldInstance, nil); err != nil {
		t.Fatalf("EnqueueMerge(old): %v", err)
	}
	if err := d.AbandonWatchingMerges(oldInstance); err != nil {
		t.Fatalf("AbandonWatchingMerges(old): %v", err)
	}

	// Enqueue under new instance.
	if _, err := d.EnqueueMerge(88, session, newInstance, nil); err != nil {
		t.Fatalf("EnqueueMerge(new): %v", err)
	}

	// Abandoned filter from new instance perspective.
	abandoned, err := d.MergeQueueForInstance(newInstance, session, "abandoned")
	if err != nil {
		t.Fatalf("MergeQueueForInstance(abandoned): %v", err)
	}
	if len(abandoned) != 1 || abandoned[0].PR != 77 {
		t.Errorf("abandoned filter: got %d rows, want 1 (PR 77)", len(abandoned))
	}

	// Watching filter from new instance sees only PR 88.
	watching, err := d.MergeQueueForInstance(newInstance, session, "")
	if err != nil {
		t.Fatalf("MergeQueueForInstance(watching): %v", err)
	}
	if len(watching) != 1 || watching[0].PR != 88 {
		t.Errorf("watching after old abandon: got %d rows, want PR 88", len(watching))
	}
}

// ── Watcher unit tests (using an in-process HTTP server to mock GitHub API) ────

// mockGHServer mocks the `gh` binary by returning predetermined responses.
// The watcher calls `gh pr view` and `gh pr merge` — we mock the CLI by
// setting PRISM_TEST_GH_MOCK_RESPONSES (key=args, value=output+exitcode).
// Because mocking actual OS executables in Go is complex, these tests use a
// helper approach: we test the tick() logic by directly calling the DB state
// machine through the watcher's exported-for-test tick path.

// To test the watcher without mocking the OS, we build a fakeWatcher that
// replaces the gh CLI calls with an injectable function.

type fakeWatcher struct {
	watcher               *Watcher
	fetchFn               func(ctx context.Context, pr int) (*prInfo, error)
	mergeFn               func(ctx context.Context, pr int) ([]byte, error)
	updateFn              func(ctx context.Context, pr int) error
	fetchRequiredChecksFn func(ctx context.Context) ([]string, error)
	// checkMergedStateFn is called on the error path of tryMerge to reconcile
	// PR state. When nil, returns "" (unknown / fall-through to keyword check).
	checkMergedStateFn func(ctx context.Context, pr int) string
}

// processHead is a test-friendly version of tick() that uses injected functions
// instead of the real gh CLI.
func (fw *fakeWatcher) processHead(ctx context.Context) {
	head, err := fw.watcher.db.MergeQueueHead(fw.watcher.sessionName)
	if err != nil || head == nil {
		return
	}
	_ = fw.watcher.db.UpdateMergeLastChecked(head.PR)

	prInfoVal, err := fw.fetchFn(ctx, head.PR)
	if err != nil {
		return
	}

	if prInfoVal.State == "CLOSED" && !prInfoVal.isMerged() {
		fw.watcher.failAndNotify(head, "PR was closed without merging")
		return
	}
	if prInfoVal.State == "MERGED" || prInfoVal.isMerged() {
		fw.watcher.succeedAndNotify(ctx, head)
		return
	}

	switch prInfoVal.MergeStateStatus {
	case "CLEAN":
		out, mergeErr := fw.mergeFn(ctx, head.PR)
		if mergeErr == nil {
			fw.watcher.succeedAndNotify(ctx, head)
			return
		}
		// First: reconcile by checking the PR's actual state.
		var reconciledState string
		if fw.checkMergedStateFn != nil {
			reconciledState = fw.checkMergedStateFn(ctx, head.PR)
		}
		if reconciledState == "MERGED" {
			fw.watcher.succeedAndNotify(ctx, head)
			return
		}
		// Second: keyword-based branch-moved-race check.
		combined := strings.ToLower(string(out) + mergeErr.Error())
		if isBranchMovedRace(combined) {
			return // transient
		}
		errMsg := fmt.Sprintf("gh pr merge failed: %s", strings.TrimSpace(string(out)))
		fw.watcher.failAndNotify(head, errMsg)

	case "BEHIND":
		_ = fw.updateFn(ctx, head.PR)

	case "DIRTY":
		fw.watcher.failAndNotify(head, "merge conflicts")

	case "BLOCKED":
		if hasCIFailure(prInfoVal.StatusCheckRollup) {
			fw.watcher.failAndNotify(head, "CI failed")
		} else if prInfoVal.ReviewDecision == "REVIEW_REQUIRED" {
			fw.watcher.failAndNotify(head, "human reviewer approval required before merge")
		} else if prInfoVal.ReviewDecision == "CHANGES_REQUESTED" {
			fw.watcher.failAndNotify(head, "reviewer requested changes — fix and re-request review")
		} else {
			log.Printf("[mergequeue] PR #%d BLOCKED but no CI failure or known review requirement (reviewDecision=%q) — staying watching", head.PR, prInfoVal.ReviewDecision)
		}

	case "UNSTABLE":
		var required []string
		var fetchErr error
		if fw.fetchRequiredChecksFn != nil {
			required, fetchErr = fw.fetchRequiredChecksFn(ctx)
		}
		if fetchErr != nil {
			// stay watching
			return
		}
		if requiredChecksAllPassed(prInfoVal.StatusCheckRollup, required) {
			out, mergeErr := fw.mergeFn(ctx, head.PR)
			if mergeErr == nil {
				fw.watcher.succeedAndNotify(ctx, head)
				return
			}
			// First: reconcile state.
			var reconciledState string
			if fw.checkMergedStateFn != nil {
				reconciledState = fw.checkMergedStateFn(ctx, head.PR)
			}
			if reconciledState == "MERGED" {
				fw.watcher.succeedAndNotify(ctx, head)
				return
			}
			// Second: keyword check.
			combined := strings.ToLower(string(out) + mergeErr.Error())
			if isBranchMovedRace(combined) {
				return
			}
			errMsg := fmt.Sprintf("gh pr merge failed: %s", strings.TrimSpace(string(out)))
			fw.watcher.failAndNotify(head, errMsg)
		}
		// stay watching if required checks not all passed

	case "UNKNOWN", "HAS_HOOKS", "DRAFT":
		// stay watching

	}
}

// newFakeWatcher builds a fakeWatcher for tests.
func newFakeWatcher(
	d *db.DB,
	instanceID, sessionName string,
	httpClient *http.Client,
	fetchFn func(context.Context, int) (*prInfo, error),
	mergeFn func(context.Context, int) ([]byte, error),
	updateFn func(context.Context, int) error,
) *fakeWatcher {
	w := New(d, instanceID, sessionName, httpClient)
	return &fakeWatcher{
		watcher:  w,
		fetchFn:  fetchFn,
		mergeFn:  mergeFn,
		updateFn: updateFn,
		// fetchRequiredChecksFn is nil by default; UNSTABLE tests set it explicitly.
	}
}

// capturingServer returns an httptest.Server that captures the last received
// request body (for notification verification).
type capturingServer struct {
	*httptest.Server
	lastBody []byte
	called   int
}

func newCapturingServer(t *testing.T) *capturingServer {
	t.Helper()
	cs := &capturingServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r.Body)
		cs.lastBody = body
		cs.called++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(cs.Server.Close)
	return cs
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 512)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

// TestWatcher_CLEANTransitionToMerged verifies that a CLEAN PR gets merged and
// transitions to 'merged' with a notification.
func TestWatcher_CLEANTransitionToMerged(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-clean"
	session := "myrepo@main"

	// Seed coordinator with a harness server.
	srv := newCapturingServer(t)
	// Parse port from srv.URL.
	port := parsePort(t, srv.URL)
	sid := "pi-sid-clean"
	seedCoordinator(t, d, session, instanceID, port, sid)

	// Enqueue PR 100.
	if _, err := d.EnqueueMerge(100, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "CLEAN"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			return nil, nil // merge succeeds
		},
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	// Row must be merged.
	row, err := d.PendingMergeByPR(100)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "merged" {
		t.Errorf("status: got %q, want merged", row.Status)
	}
	if row.MergedAt == nil {
		t.Error("merged_at: got nil, want set")
	}
	if row.EndedAt == nil {
		t.Error("ended_at: got nil, want set")
	}
}

// TestWatcher_BEHINDLeaveWatching verifies that a BEHIND PR triggers
// update-branch and stays watching.
func TestWatcher_BEHINDLeaveWatching(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-behind"
	session := "myrepo@main"
	updateCalled := false

	if _, err := d.EnqueueMerge(200, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, http.DefaultClient,
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "BEHIND"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error {
			updateCalled = true
			return nil
		},
	)

	fw.processHead(context.Background())

	if !updateCalled {
		t.Error("BEHIND: update-branch was not called")
	}

	// Row must still be watching.
	row, err := d.PendingMergeByPR(200)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status after BEHIND: got %q, want watching", row.Status)
	}
}

// TestWatcher_DIRTYTransitionToFailed verifies conflict detection.
func TestWatcher_DIRTYTransitionToFailed(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-dirty"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	sid := "pi-sid-dirty"
	seedCoordinator(t, d, session, instanceID, port, sid)

	if _, err := d.EnqueueMerge(300, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "DIRTY"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(300)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed", row.Status)
	}
	if row.Error == nil || *row.Error != "merge conflicts" {
		t.Errorf("error: got %v, want 'merge conflicts'", row.Error)
	}
}

// TestWatcher_BLOCKEDWithCIFailure transitions to failed.
func TestWatcher_BLOCKEDWithCIFailure(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-blocked-ci"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-ci")

	if _, err := d.EnqueueMerge(400, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:             "OPEN",
				MergeStateStatus:  "BLOCKED",
				StatusCheckRollup: []checkEntry{{Conclusion: "FAILURE"}},
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(400)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed", row.Status)
	}
	if row.Error == nil || *row.Error != "CI failed" {
		t.Errorf("error: got %v, want 'CI failed'", row.Error)
	}
}

// TestWatcher_BLOCKEDWithoutCIFailureStaysWatching verifies that BLOCKED with
// no failed checks does not terminate the row (CI still running).
func TestWatcher_BLOCKEDWithoutCIFailureStaysWatching(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-blocked-noci"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(500, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, http.DefaultClient,
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:             "OPEN",
				MergeStateStatus:  "BLOCKED",
				StatusCheckRollup: []checkEntry{{Conclusion: "IN_PROGRESS"}},
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(500)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching (no CI failure)", row.Status)
	}
}

// TestWatcher_BLOCKEDWithReviewRequired transitions to failed with the review
// notification message when reviewDecision is "REVIEW_REQUIRED" and CI is passing.
func TestWatcher_BLOCKEDWithReviewRequired(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-blocked-review"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-review")

	if _, err := d.EnqueueMerge(555, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:             "OPEN",
				MergeStateStatus:  "BLOCKED",
				StatusCheckRollup: []checkEntry{{Conclusion: "SUCCESS"}},
				ReviewDecision:    "REVIEW_REQUIRED",
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	// Row must have transitioned to failed.
	row, err := d.PendingMergeByPR(555)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed", row.Status)
	}
	if row.Error == nil || *row.Error != "human reviewer approval required before merge" {
		t.Errorf("error: got %v, want 'human reviewer approval required before merge'", row.Error)
	}

	// Notification must have been sent and contain the expected text.
	if srv.called == 0 {
		t.Fatal("no notification sent to coordinator")
	}
	var body map[string]any
	if err := json.Unmarshal(srv.lastBody, &body); err != nil {
		t.Fatalf("unmarshal notification body: %v", err)
	}
	parts, _ := body["parts"].([]interface{})
	if len(parts) == 0 {
		t.Fatal("notification body has no parts")
	}
	part, _ := parts[0].(map[string]interface{})
	text, _ := part["text"].(string)
	if !strings.Contains(text, "PR #555") {
		t.Errorf("notification text %q does not contain 'PR #555'", text)
	}
	if !strings.Contains(text, "human reviewer approval required before merge") {
		t.Errorf("notification text %q does not contain review-required message", text)
	}
}

// TestWatcher_BLOCKEDCIFailureNotMisdiagnosedAsReviewRequired verifies that
// a BLOCKED PR with a CI failure and REVIEW_REQUIRED reviewDecision is
// diagnosed as CI failure (CI check takes precedence) — or more precisely,
// that CI failure is checked first and is not suppressed by reviewDecision.
func TestWatcher_BLOCKEDCIFailureNotMisdiagnosedAsReviewRequired(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-blocked-cifirst"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-cifirst")

	if _, err := d.EnqueueMerge(444, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:             "OPEN",
				MergeStateStatus:  "BLOCKED",
				StatusCheckRollup: []checkEntry{{Conclusion: "FAILURE"}},
				ReviewDecision:    "REVIEW_REQUIRED",
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	// Row must be failed with "CI failed" (CI check takes precedence).
	row, err := d.PendingMergeByPR(444)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed", row.Status)
	}
	if row.Error == nil || *row.Error != "CI failed" {
		t.Errorf("error: got %v, want 'CI failed'", row.Error)
	}
}

// TestWatcher_BLOCKEDReviewRequiredIsReEnqueueable verifies the edge-case AC:
// after a terminal "review required" failure, the coordinator can re-enqueue
// the same PR with prism merge and the new row starts as watching.
func TestWatcher_BLOCKEDReviewRequiredIsReEnqueueable(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-reenqueue"
	session := "myrepo@main"

	// First enqueue and fail with review-required.
	if _, err := d.EnqueueMerge(666, session, instanceID, nil); err != nil {
		t.Fatalf("first EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(666, "failed", "human reviewer approval required before merge"); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}

	// Re-enqueue — simulates coordinator running `prism merge <pr>` after approval.
	m, err := d.EnqueueMerge(666, session, instanceID, nil)
	if err != nil {
		t.Fatalf("second EnqueueMerge: %v", err)
	}
	if m.Status != "watching" {
		t.Errorf("re-enqueued status: got %q, want watching", m.Status)
	}
	if m.Error != nil {
		t.Errorf("re-enqueued error: got %v, want nil", m.Error)
	}
}

// TestWatcher_BLOCKEDNoReviewDecisionNoCI stays watching (CI still running case).
func TestWatcher_BLOCKEDNoReviewDecisionNoCIStaysWatching(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-blocked-nostatus"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(777, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, http.DefaultClient,
		func(_ context.Context, pr int) (*prInfo, error) {
			// No CI failure, no review requirement — e.g. CI still running.
			return &prInfo{
				State:             "OPEN",
				MergeStateStatus:  "BLOCKED",
				StatusCheckRollup: []checkEntry{{Conclusion: "PENDING"}},
				ReviewDecision:    "",
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(777)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching", row.Status)
	}
}

// TestWatcher_BLOCKEDWithChangesRequested transitions to failed with the
// changes-requested notification message when reviewDecision is
// "CHANGES_REQUESTED" and CI is passing. Regression test for issue #1884:
// previously this case fell through to the "staying watching" log branch
// and the PR sat in the queue forever.
func TestWatcher_BLOCKEDWithChangesRequested(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-blocked-changes"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-changes")

	if _, err := d.EnqueueMerge(1884, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:             "OPEN",
				MergeStateStatus:  "BLOCKED",
				StatusCheckRollup: []checkEntry{{Conclusion: "SUCCESS"}},
				ReviewDecision:    "CHANGES_REQUESTED",
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	// Row must have transitioned to failed with the expected reason.
	const wantMsg = "reviewer requested changes — fix and re-request review"
	row, err := d.PendingMergeByPR(1884)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed", row.Status)
	}
	if row.Error == nil || *row.Error != wantMsg {
		t.Errorf("error: got %v, want %q", row.Error, wantMsg)
	}

	// Notification must have been sent and contain the expected text.
	if srv.called == 0 {
		t.Fatal("no notification sent to coordinator")
	}
	var body map[string]any
	if err := json.Unmarshal(srv.lastBody, &body); err != nil {
		t.Fatalf("unmarshal notification body: %v", err)
	}
	parts, _ := body["parts"].([]interface{})
	if len(parts) == 0 {
		t.Fatal("notification body has no parts")
	}
	part, _ := parts[0].(map[string]interface{})
	text, _ := part["text"].(string)
	if !strings.Contains(text, "PR #1884") {
		t.Errorf("notification text %q does not contain 'PR #1884'", text)
	}
	if !strings.Contains(text, wantMsg) {
		t.Errorf("notification text %q does not contain changes-requested message", text)
	}
}

// TestWatcher_BLOCKEDUnrecognisedReviewDecisionLogsValue verifies the
// defensive log branch: an unrecognised ReviewDecision value (e.g. a future
// GitHub enum addition) keeps the row in watching state AND emits a log line
// containing the literal decision string for forensic visibility.
func TestWatcher_BLOCKEDUnrecognisedReviewDecisionLogsValue(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-blocked-foobar"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(1885, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, http.DefaultClient,
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:             "OPEN",
				MergeStateStatus:  "BLOCKED",
				StatusCheckRollup: []checkEntry{{Conclusion: "SUCCESS"}},
				ReviewDecision:    "FOOBAR",
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	// Capture log output only around processHead so we don't pick up the
	// unrelated repo-resolution log lines emitted by New().
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})

	fw.processHead(context.Background())

	// Row stays in watching state (stay-watching path).
	row, err := d.PendingMergeByPR(1885)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching (unrecognised decision must not terminate)", row.Status)
	}

	// Log line must include the literal value "FOOBAR" for forensic visibility.
	logged := buf.String()
	if !strings.Contains(logged, "FOOBAR") {
		t.Errorf("log output %q does not contain literal \"FOOBAR\"", logged)
	}
	if !strings.Contains(logged, "PR #1885") {
		t.Errorf("log output %q does not reference PR #1885", logged)
	}
}

// TestWatcher_UNSTABLEStaysWatchingWhenRequiredPending verifies that when
// UNSTABLE and a required check is still in progress, the row stays watching.
func TestWatcher_UNSTABLEStaysWatchingWhenRequiredPending(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-unstable"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(600, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, http.DefaultClient,
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:            "OPEN",
				MergeStateStatus: "UNSTABLE",
				StatusCheckRollup: []checkEntry{
					{Name: "pr-gate", Status: "IN_PROGRESS", Conclusion: ""},
					{Name: "validate-flakes", Status: "COMPLETED", Conclusion: "SUCCESS"},
				},
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)
	fw.fetchRequiredChecksFn = func(_ context.Context) ([]string, error) {
		return []string{"pr-gate"}, nil
	}

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(600)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching", row.Status)
	}
}

// TestWatcher_UNKNOWNStaysWatching verifies UNKNOWN leaves the row watching.
func TestWatcher_UNKNOWNStaysWatching(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-unknown"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(700, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, http.DefaultClient,
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "UNKNOWN"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(700)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching", row.Status)
	}
}

// TestWatcher_BranchMovedRaceRetries verifies that when gh pr merge returns a
// branch-moved error, the row stays watching (no notification).
func TestWatcher_BranchMovedRaceRetries(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-race"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(800, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	notified := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		notified = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "CLEAN"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			return []byte("already merged"), fmt.Errorf("exit status 1")
		},
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	if notified {
		t.Error("branch-moved race: notification was sent, want no notification")
	}

	row, err := d.PendingMergeByPR(800)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status after race: got %q, want watching", row.Status)
	}
}

// TestWatcher_ClosedExternallyTransitionsToFailed verifies that a PR closed
// without merging becomes failed.
func TestWatcher_ClosedExternallyTransitionsToFailed(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-closed"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-closed")

	if _, err := d.EnqueueMerge(900, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "CLOSED", MergedAt: nil}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(900)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed", row.Status)
	}
}

// TestWatcher_Cancellation verifies CancelMerge terminates a watching row.
func TestWatcher_Cancellation(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-canc"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(111, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	cancelled, err := d.CancelMerge(111, instanceID)
	if err != nil {
		t.Fatalf("CancelMerge: %v", err)
	}
	if !cancelled {
		t.Error("CancelMerge: returned false, want true")
	}

	// Head must now be nil (no more watching rows).
	head, err := d.MergeQueueHead(session)
	if err != nil {
		t.Fatalf("MergeQueueHead after cancel: %v", err)
	}
	if head != nil {
		t.Errorf("MergeQueueHead after cancel: got PR %d, want nil", head.PR)
	}
}

// TestWatcher_NextPRPromotedAfterTerminal verifies that after the head
// terminates, the next PR becomes head on the next tick.
func TestWatcher_NextPRPromotedAfterTerminal(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-promote"
	session := "myrepo@main"

	// Enqueue two PRs.
	if _, err := d.EnqueueMerge(10, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(10): %v", err)
	}
	time.Sleep(time.Millisecond)
	if _, err := d.EnqueueMerge(20, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(20): %v", err)
	}

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-promote")

	// First tick: head is PR 10 — mark as DIRTY so it fails.
	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "DIRTY"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)
	fw.processHead(context.Background())

	// Head should now be PR 20.
	head, err := d.MergeQueueHead(session)
	if err != nil {
		t.Fatalf("MergeQueueHead after first tick: %v", err)
	}
	if head == nil {
		t.Fatal("MergeQueueHead: got nil, want PR 20")
	}
	if head.PR != 20 {
		t.Errorf("MergeQueueHead after first tick: got PR %d, want PR 20", head.PR)
	}
}

// TestWatcher_SecurityDenies verifies the isBranchMovedRace helper identifies
// known race messages.
func TestWatcher_SecurityDenies(t *testing.T) {
	cases := []struct {
		output string
		isRace bool
	}{
		{"already merged\n", true},
		{"pull request is not mergeable", true},
		{"base branch was modified", true},
		{"some other error", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isBranchMovedRace(tc.output)
		if got != tc.isRace {
			t.Errorf("isBranchMovedRace(%q) = %v, want %v", tc.output, got, tc.isRace)
		}
	}
}

// TestMigration_V18ToV19_CreatesTable verifies that the v18→v19 migration
// creates the pending_merges table with the expected schema.
func TestMigration_V18ToV19_CreatesTable(t *testing.T) {
	d := openTestDB(t)

	// pending_merges table must exist.
	var tname string
	if err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='pending_merges'").Scan(&tname); err != nil {
		t.Fatalf("pending_merges table not found after v18→v19 migration: %v", err)
	}

	// idx_pending_merges_status_instance must exist.
	var iname string
	if err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='index' AND name='idx_pending_merges_status_instance'").Scan(&iname); err != nil {
		t.Fatalf("idx_pending_merges_status_instance not found after v18→v19 migration: %v", err)
	}
}

// TestMigration_V19ToV20_CreatesSessionIndex verifies that the v19→v20
// migration creates idx_pending_merges_status_session on
// (session_name, status, queue_position) to cover MergeQueueHead.
func TestMigration_V19ToV20_CreatesSessionIndex(t *testing.T) {
	d := openTestDB(t)

	// idx_pending_merges_status_session must exist after v19→v20.
	var iname string
	if err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='index' AND name='idx_pending_merges_status_session'").Scan(&iname); err != nil {
		t.Fatalf("idx_pending_merges_status_session not found after v19→v20 migration: %v", err)
	}

	// The old instance-keyed index must still exist (used by AbandonWatchingMerges / CancelMerge).
	var iname2 string
	if err := d.QueryRow("SELECT name FROM sqlite_master WHERE type='index' AND name='idx_pending_merges_status_instance'").Scan(&iname2); err != nil {
		t.Fatalf("idx_pending_merges_status_instance missing after v19→v20 — should be preserved: %v", err)
	}
}

// TestMigration_V18ToV19_Idempotent verifies opening the same DB twice doesn't error.
func TestMigration_V18ToV19_Idempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "idem.db")

	d1, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	d1.Close()

	d2, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer d2.Close()

	var version int
	if err := d2.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != 36 {
		t.Errorf("schema_version after second open: got %d, want 36", version)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// parsePort extracts the TCP port from an httptest.Server URL.
func parsePort(t *testing.T, rawURL string) int {
	t.Helper()
	// URL format: http://127.0.0.1:<port>
	parts := strings.Split(rawURL, ":")
	if len(parts) < 3 {
		t.Fatalf("cannot parse port from URL %q", rawURL)
	}
	var port int
	if _, err := fmt.Sscanf(parts[2], "%d", &port); err != nil {
		t.Fatalf("cannot parse port from %q: %v", parts[2], err)
	}
	return port
}

// TestWatcher_NotificationBodyContainsPR verifies the notification body
// sent to the coordinator contains the PR number.
func TestWatcher_NotificationBodyContainsPR(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-notif"
	session := "myrepo@main"

	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = readAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-notif")

	if _, err := d.EnqueueMerge(123, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "CLEAN"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	if len(received) == 0 {
		t.Fatal("no notification received by coordinator")
	}

	var body map[string]any
	if err := json.Unmarshal(received, &body); err != nil {
		t.Fatalf("unmarshal notification body: %v", err)
	}

	parts, ok := body["parts"].([]interface{})
	if !ok || len(parts) == 0 {
		t.Fatalf("notification body has no parts: %v", body)
	}
	part, _ := parts[0].(map[string]interface{})
	text, _ := part["text"].(string)
	if !strings.Contains(text, "PR #123") {
		t.Errorf("notification text %q does not mention PR #123", text)
	}
	if !strings.Contains(text, "git pull") {
		t.Errorf("notification text %q does not mention 'git pull'", text)
	}

	_ = os.Getenv // avoid unused import
}

// TestPRInfoJSONFields_NoMergedField guards against regressing to the
// (invalid) "merged" field. `gh pr view` rejects unknown JSON fields with a
// non-zero exit, which silently broke the entire merge-queue watcher between
// #1014 and the fix for this regression. The canonical merge-detection field
// is `mergedAt` (a nullable timestamp string).
func TestPRInfoJSONFields_NoMergedField(t *testing.T) {
	fields := strings.Split(prInfoJSONFields, ",")
	for _, f := range fields {
		if f == "merged" {
			t.Errorf("prInfoJSONFields contains invalid 'merged' field; gh pr view will reject it. Got: %q", prInfoJSONFields)
		}
	}
	hasMergedAt := false
	hasReviewDecision := false
	for _, f := range fields {
		if f == "mergedAt" {
			hasMergedAt = true
		}
		if f == "reviewDecision" {
			hasReviewDecision = true
		}
	}
	if !hasMergedAt {
		t.Errorf("prInfoJSONFields must include 'mergedAt' to detect merged state. Got: %q", prInfoJSONFields)
	}
	if !hasReviewDecision {
		t.Errorf("prInfoJSONFields must include 'reviewDecision' to detect review-required blocks (#1357). Got: %q", prInfoJSONFields)
	}
}

// TestPRInfo_IsMerged verifies the merged-detection logic against the three
// states the watcher cares about: a non-empty mergedAt timestamp (merged), a
// nil mergedAt pointer (not merged), and an empty-string mergedAt (also not
// merged — a defensive case in case `gh` ever emits "" instead of null).
func TestPRInfo_IsMerged(t *testing.T) {
	mergedTime := "2026-04-26T10:09:40Z"
	emptyStr := ""

	cases := []struct {
		name       string
		info       prInfo
		wantMerged bool
	}{
		{
			name:       "MergedAt set to ISO timestamp",
			info:       prInfo{State: "MERGED", MergedAt: &mergedTime},
			wantMerged: true,
		},
		{
			name:       "MergedAt is nil pointer (PR open or closed without merge)",
			info:       prInfo{State: "OPEN", MergedAt: nil},
			wantMerged: false,
		},
		{
			name:       "MergedAt points to empty string (defensive)",
			info:       prInfo{State: "OPEN", MergedAt: &emptyStr},
			wantMerged: false,
		},
		{
			name:       "CLOSED + nil MergedAt = closed without merging",
			info:       prInfo{State: "CLOSED", MergedAt: nil},
			wantMerged: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.isMerged(); got != tc.wantMerged {
				t.Errorf("isMerged() = %v, want %v", got, tc.wantMerged)
			}
		})
	}
}

// TestWatcher_RunGHPassesRepoFlag verifies the #1055 fix: every gh invocation
// from the watcher includes "--repo <owner/name>" as the first two argv tokens
// so calls succeed regardless of the sidecar's CWD. Exercises the real
// fetchPRInfo / tryMerge / updateBranch methods with an injected runGHFunc
// that captures argv.
func TestWatcher_RunGHPassesRepoFlag(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-repo-flag"
	session := "myrepo@main"
	const repoSlug = "owner/myrepo"

	// Seed the coordinator so notify() can find the harness port — needed
	// because the CLEAN→merged path fires a notification.
	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-repo-flag")

	// Build a Watcher directly (bypass New so we don't shell out to gh) with
	// the repo cached and a runGHFunc that records every invocation.
	var calls [][]string
	w := &Watcher{
		db:          d,
		instanceID:  instanceID,
		sessionName: session,
		httpClient:  srv.Client(),
		repo:        repoSlug,
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			// Copy args defensively so later calls don't alias the slice.
			snap := make([]string, len(args))
			copy(snap, args)
			calls = append(calls, snap)
			// Tailor the response so each method completes its happy path.
			if len(args) >= 4 && args[2] == "pr" && args[3] == "view" {
				return []byte(`{"state":"OPEN","mergedAt":null,"mergeStateStatus":"CLEAN","statusCheckRollup":[],"reviewDecision":""}`), nil
			}
			// merge / update-branch return empty success.
			return nil, nil
		},
	}

	ctx := context.Background()

	// 1. fetchPRInfo (calls gh pr view).
	if _, err := w.fetchPRInfo(ctx, 101); err != nil {
		t.Fatalf("fetchPRInfo: %v", err)
	}
	// 2. tryMerge (calls gh pr merge --squash on the CLEAN path).
	if _, err := d.EnqueueMerge(202, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	head, err := d.MergeQueueHead(session)
	if err != nil || head == nil {
		t.Fatalf("MergeQueueHead: %v / nil=%v", err, head == nil)
	}
	w.tryMerge(ctx, head)
	// 3. updateBranch (calls gh pr update-branch).
	if err := w.updateBranch(ctx, 303); err != nil {
		t.Fatalf("updateBranch: %v", err)
	}

	if len(calls) != 3 {
		t.Fatalf("expected 3 gh invocations (view, merge, update-branch); got %d: %v", len(calls), calls)
	}

	for i, argv := range calls {
		if len(argv) < 2 {
			t.Errorf("call %d argv too short: %v", i, argv)
			continue
		}
		if argv[0] != "--repo" {
			t.Errorf("call %d argv[0]: got %q, want %q (full argv: %v)", i, argv[0], "--repo", argv)
		}
		if argv[1] != repoSlug {
			t.Errorf("call %d argv[1]: got %q, want %q (full argv: %v)", i, argv[1], repoSlug, argv)
		}
	}

	// Spot-check that the trailing subcommand is preserved (defence against a
	// future refactor that accidentally drops the original args).
	wantSubs := []string{"view", "merge", "update-branch"}
	for i, sub := range wantSubs {
		argv := calls[i]
		found := false
		for _, a := range argv {
			if a == sub {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("call %d argv missing %q subcommand: %v", i, sub, argv)
		}
	}
}

// TestWatcher_RunExitsWhenRepoUnresolved verifies AC #5: when owner/name
// resolution fails at startup (Watcher.repo == ""), Run() returns immediately
// without entering the poll loop.
func TestWatcher_RunExitsWhenRepoUnresolved(t *testing.T) {
	d := openTestDB(t)
	w := &Watcher{
		db:          d,
		instanceID:  "inst-no-repo",
		sessionName: "myrepo@main",
		httpClient:  http.DefaultClient,
		repo:        "", // resolution failure simulated
	}

	// A non-cancelled context: if Run enters the poll loop it would block
	// for PollInterval. We assert it returns within a small budget instead.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// PASS — Run returned without polling.
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when repo was unresolved — would block the goroutine forever")
	}
}

// TestNew_NoAgentStatusRow_LeavesRepoEmpty verifies that when CurrentStatus
// returns no row for the session, New() leaves Watcher.repo == "" rather than
// shelling out to gh from a possibly-bogus directory. Together with
// TestWatcher_RunExitsWhenRepoUnresolved this gives end-to-end coverage of the
// AC #5 startup-failure path.
func TestNew_NoAgentStatusRow_LeavesRepoEmpty(t *testing.T) {
	d := openTestDB(t)
	// No seedCoordinator call → CurrentStatus("ghost-session@main") returns nil.
	w := New(d, "inst-ghost", "ghost-session@main", nil)
	if w == nil {
		t.Fatal("New returned nil")
	}
	if w.repo != "" {
		t.Errorf("Watcher.repo: got %q, want empty (no agent_status row)", w.repo)
	}
}

// ── requiredChecksAllPassed unit tests ────────────────────────────────────────

// TestRequiredChecksAllPassed_AllSuccess verifies the happy path: every
// required name appears in the rollup with a SUCCESS conclusion.
func TestRequiredChecksAllPassed_AllSuccess(t *testing.T) {
	rollup := []checkEntry{
		{Name: "pr-gate", Conclusion: "SUCCESS", Status: "COMPLETED"},
		{Name: "validate-flakes", Conclusion: "SUCCESS", Status: "COMPLETED"},
	}
	if !requiredChecksAllPassed(rollup, []string{"pr-gate"}) {
		t.Error("requiredChecksAllPassed: want true when required check is SUCCESS, got false")
	}
}

// TestRequiredChecksAllPassed_MissingRequired verifies that a required name
// absent from the rollup causes the helper to return false.
func TestRequiredChecksAllPassed_MissingRequired(t *testing.T) {
	rollup := []checkEntry{
		{Name: "validate-flakes", Conclusion: "SUCCESS", Status: "COMPLETED"},
	}
	if requiredChecksAllPassed(rollup, []string{"pr-gate"}) {
		t.Error("requiredChecksAllPassed: want false when required check is missing from rollup, got true")
	}
}

// TestRequiredChecksAllPassed_InProgress verifies that a required check that
// is still running (no conclusion yet) returns false.
func TestRequiredChecksAllPassed_InProgress(t *testing.T) {
	rollup := []checkEntry{
		{Name: "pr-gate", Status: "IN_PROGRESS", Conclusion: ""},
	}
	if requiredChecksAllPassed(rollup, []string{"pr-gate"}) {
		t.Error("requiredChecksAllPassed: want false when required check is IN_PROGRESS, got true")
	}
}

// TestRequiredChecksAllPassed_Failure verifies that a required check with
// conclusion FAILURE returns false.
func TestRequiredChecksAllPassed_Failure(t *testing.T) {
	rollup := []checkEntry{
		{Name: "pr-gate", Conclusion: "FAILURE", Status: "COMPLETED"},
	}
	if requiredChecksAllPassed(rollup, []string{"pr-gate"}) {
		t.Error("requiredChecksAllPassed: want false when required check is FAILURE, got true")
	}
}

// TestRequiredChecksAllPassed_EmptyConclusion verifies that a required check
// with an empty conclusion (e.g. queued/pending) returns false.
func TestRequiredChecksAllPassed_EmptyConclusion(t *testing.T) {
	rollup := []checkEntry{
		{Name: "pr-gate", Conclusion: "", Status: "QUEUED"},
	}
	if requiredChecksAllPassed(rollup, []string{"pr-gate"}) {
		t.Error("requiredChecksAllPassed: want false when required check has empty conclusion, got true")
	}
}

// TestRequiredChecksAllPassed_LegacyCommitStatus verifies that legacy
// commit-status entries (State field instead of Conclusion, Context instead
// of Name) are handled correctly.
func TestRequiredChecksAllPassed_LegacyCommitStatus(t *testing.T) {
	// Legacy entries use Context (not Name) and State (not Conclusion).
	rollup := []checkEntry{
		{Context: "pr-gate", State: "SUCCESS"},
		{Context: "validate-flakes", State: "PENDING"},
	}
	// pr-gate passes via the legacy State field.
	if !requiredChecksAllPassed(rollup, []string{"pr-gate"}) {
		t.Error("requiredChecksAllPassed: want true for legacy SUCCESS state, got false")
	}
	// validate-flakes is still PENDING.
	if requiredChecksAllPassed(rollup, []string{"validate-flakes"}) {
		t.Error("requiredChecksAllPassed: want false for legacy PENDING state, got true")
	}
}

// TestRequiredChecksAllPassed_MixedModernAndLegacy verifies a rollup that
// contains both modern check-run entries and legacy commit-status entries.
func TestRequiredChecksAllPassed_MixedModernAndLegacy(t *testing.T) {
	rollup := []checkEntry{
		{Name: "pr-gate", Conclusion: "SUCCESS", Status: "COMPLETED"},
		{Context: "legacy-status", State: "SUCCESS"},
		{Name: "optional-slow", Status: "IN_PROGRESS", Conclusion: ""},
	}
	// Both required checks are SUCCESS (one modern, one legacy).
	if !requiredChecksAllPassed(rollup, []string{"pr-gate", "legacy-status"}) {
		t.Error("requiredChecksAllPassed: want true when all required (mixed) checks are SUCCESS, got false")
	}
	// optional-slow is not required — its IN_PROGRESS state must not block.
	if !requiredChecksAllPassed(rollup, []string{"pr-gate"}) {
		t.Error("requiredChecksAllPassed: want true when only required check is SUCCESS (optional ignored), got false")
	}
}

// TestRequiredChecksAllPassed_EmptyRequired verifies that an empty required
// list returns false (conservative: don't merge when no constraints configured).
func TestRequiredChecksAllPassed_EmptyRequired(t *testing.T) {
	rollup := []checkEntry{
		{Name: "pr-gate", Conclusion: "SUCCESS"},
	}
	if requiredChecksAllPassed(rollup, nil) {
		t.Error("requiredChecksAllPassed: want false for empty required list, got true")
	}
	if requiredChecksAllPassed(rollup, []string{}) {
		t.Error("requiredChecksAllPassed: want false for empty (non-nil) required list, got true")
	}
}

// ── handleHead / UNSTABLE path integration tests ──────────────────────────────

// TestWatcher_UNSTABLEAllRequiredPass verifies that when mergeStateStatus is
// UNSTABLE and all required checks are SUCCESS, the watcher proceeds to merge.
func TestWatcher_UNSTABLEAllRequiredPass(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-unstable-merge"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-unstable-merge")

	if _, err := d.EnqueueMerge(1001, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	mergeCalled := false
	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:            "OPEN",
				MergeStateStatus: "UNSTABLE",
				StatusCheckRollup: []checkEntry{
					// Required check: SUCCESS
					{Name: "pr-gate", Conclusion: "SUCCESS", Status: "COMPLETED"},
					// Optional check: still running — must not block merge
					{Name: "validate-flakes", Status: "IN_PROGRESS", Conclusion: ""},
				},
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			mergeCalled = true
			return nil, nil // merge succeeds
		},
		func(_ context.Context, pr int) error { return nil },
	)
	fw.fetchRequiredChecksFn = func(_ context.Context) ([]string, error) {
		return []string{"pr-gate"}, nil
	}

	fw.processHead(context.Background())

	if !mergeCalled {
		t.Error("UNSTABLE+all-required-pass: merge was not attempted")
	}
	row, err := d.PendingMergeByPR(1001)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "merged" {
		t.Errorf("status: got %q, want merged", row.Status)
	}
}

// TestWatcher_UNSTABLERequiredStillRunning verifies that when UNSTABLE and
// the required check is IN_PROGRESS, the row stays watching.
func TestWatcher_UNSTABLERequiredStillRunning(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-unstable-running"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(1002, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	mergeCalled := false
	fw := newFakeWatcher(d, instanceID, session, http.DefaultClient,
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:            "OPEN",
				MergeStateStatus: "UNSTABLE",
				StatusCheckRollup: []checkEntry{
					{Name: "pr-gate", Status: "IN_PROGRESS", Conclusion: ""},
				},
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			mergeCalled = true
			return nil, nil
		},
		func(_ context.Context, pr int) error { return nil },
	)
	fw.fetchRequiredChecksFn = func(_ context.Context) ([]string, error) {
		return []string{"pr-gate"}, nil
	}

	fw.processHead(context.Background())

	if mergeCalled {
		t.Error("UNSTABLE+required-in-progress: merge was called, want staying watching")
	}
	row, err := d.PendingMergeByPR(1002)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching", row.Status)
	}
}

// TestWatcher_UNSTABLEBranchProtectionFetchError verifies that when the
// branch-protection API call fails, the watcher logs and stays watching.
func TestWatcher_UNSTABLEBranchProtectionFetchError(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-unstable-fetcherr"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(1003, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	mergeCalled := false
	fw := newFakeWatcher(d, instanceID, session, http.DefaultClient,
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:            "OPEN",
				MergeStateStatus: "UNSTABLE",
				StatusCheckRollup: []checkEntry{
					{Name: "pr-gate", Conclusion: "SUCCESS", Status: "COMPLETED"},
				},
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			mergeCalled = true
			return nil, nil
		},
		func(_ context.Context, pr int) error { return nil },
	)
	// Simulate a branch-protection API failure.
	fw.fetchRequiredChecksFn = func(_ context.Context) ([]string, error) {
		return nil, fmt.Errorf("gh api: HTTP 403 forbidden")
	}

	fw.processHead(context.Background())

	if mergeCalled {
		t.Error("UNSTABLE+fetch-error: merge was called, want staying watching")
	}
	row, err := d.PendingMergeByPR(1003)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching", row.Status)
	}
}

// TestWatcher_UNSTABLERequiredAbsentFromRollup verifies that when a required
// check name is not present in the rollup at all, the watcher stays watching.
func TestWatcher_UNSTABLERequiredAbsentFromRollup(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-unstable-absent"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(1004, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	mergeCalled := false
	fw := newFakeWatcher(d, instanceID, session, http.DefaultClient,
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:            "OPEN",
				MergeStateStatus: "UNSTABLE",
				// Rollup does NOT include pr-gate at all.
				StatusCheckRollup: []checkEntry{
					{Name: "validate-flakes", Conclusion: "SUCCESS", Status: "COMPLETED"},
				},
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			mergeCalled = true
			return nil, nil
		},
		func(_ context.Context, pr int) error { return nil },
	)
	fw.fetchRequiredChecksFn = func(_ context.Context) ([]string, error) {
		return []string{"pr-gate"}, nil
	}

	fw.processHead(context.Background())

	if mergeCalled {
		t.Error("UNSTABLE+required-absent: merge was called, want staying watching")
	}
	row, err := d.PendingMergeByPR(1004)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching", row.Status)
	}
}

// ── fetchRequiredChecks cache tests ───────────────────────────────────────────

// TestFetchRequiredChecks_ParsesModernChecks verifies that the branch-protection
// API response is correctly parsed when it contains modern check-run entries
// (the checks[] array).
func TestFetchRequiredChecks_ParsesModernChecks(t *testing.T) {
	d := openTestDB(t)

	const repoSlug = "owner/testrepo"
	var callCount int
	w := &Watcher{
		db:          d,
		instanceID:  "inst-bprot",
		sessionName: "myrepo@main",
		httpClient:  http.DefaultClient,
		repo:        repoSlug,
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			callCount++
			// Return a modern branch-protection response.
			body := `{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"},{"context":"lint"}]}}`
			return []byte(body), nil
		},
	}

	names, err := w.fetchRequiredChecks(context.Background())
	if err != nil {
		t.Fatalf("fetchRequiredChecks: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("want 2 names, got %d: %v", len(names), names)
	}
	// Order should be stable (dedup preserves insertion order).
	if names[0] != "pr-gate" || names[1] != "lint" {
		t.Errorf("names: got %v, want [pr-gate lint]", names)
	}
}

// TestFetchRequiredChecks_ParsesLegacyContexts verifies that legacy
// commit-status contexts (the contexts[] array) are parsed correctly.
func TestFetchRequiredChecks_ParsesLegacyContexts(t *testing.T) {
	d := openTestDB(t)

	w := &Watcher{
		db:          d,
		instanceID:  "inst-bprot-legacy",
		sessionName: "myrepo@main",
		httpClient:  http.DefaultClient,
		repo:        "owner/testrepo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			body := `{"required_status_checks":{"contexts":["legacy-ci"],"checks":[]}}`
			return []byte(body), nil
		},
	}

	names, err := w.fetchRequiredChecks(context.Background())
	if err != nil {
		t.Fatalf("fetchRequiredChecks: %v", err)
	}
	if len(names) != 1 || names[0] != "legacy-ci" {
		t.Errorf("names: got %v, want [legacy-ci]", names)
	}
}

// TestFetchRequiredChecks_CacheTTL verifies that a cached response is reused
// within the TTL and re-fetched after expiry.
func TestFetchRequiredChecks_CacheTTL(t *testing.T) {
	d := openTestDB(t)

	var callCount int
	current := time.Now()
	w := &Watcher{
		db:          d,
		instanceID:  "inst-bprot-ttl",
		sessionName: "myrepo@main",
		httpClient:  http.DefaultClient,
		repo:        "owner/testrepo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			callCount++
			body := `{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]}}`
			return []byte(body), nil
		},
		nowFunc: func() time.Time { return current },
	}

	// First call: populates cache.
	names1, err := w.fetchRequiredChecks(context.Background())
	if err != nil {
		t.Fatalf("first fetchRequiredChecks: %v", err)
	}
	if callCount != 1 {
		t.Errorf("first call: callCount = %d, want 1", callCount)
	}

	// Second call within TTL: cache hit, no new gh invocation.
	names2, err := w.fetchRequiredChecks(context.Background())
	if err != nil {
		t.Fatalf("second fetchRequiredChecks: %v", err)
	}
	if callCount != 1 {
		t.Errorf("second call (within TTL): callCount = %d, want 1", callCount)
	}
	if len(names1) != len(names2) || names1[0] != names2[0] {
		t.Errorf("cache miss returned different names: %v vs %v", names1, names2)
	}

	// Advance time past TTL.
	current = current.Add(requiredChecksCacheTTL + time.Second)

	// Third call after TTL: cache miss, re-fetches.
	_, err = w.fetchRequiredChecks(context.Background())
	if err != nil {
		t.Fatalf("third fetchRequiredChecks: %v", err)
	}
	if callCount != 2 {
		t.Errorf("third call (after TTL): callCount = %d, want 2", callCount)
	}
}

// ── tryMerge reconciliation tests (issue #1645) ─────────────────────────────

// TestWatcher_MergeAlreadyInProgress_PRIsMerged verifies AC: when gh pr merge
// returns an error and checkPRMergedState returns "MERGED", the watcher
// transitions to merged and sends the success notification.
func TestWatcher_MergeAlreadyInProgress_PRIsMerged(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-reconcile-merged"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-reconcile-merged")

	if _, err := d.EnqueueMerge(2100, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "CLEAN"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			// Simulate "Merge already in progress" GraphQL error.
			return []byte("GraphQL: Merge already in progress (mergePullRequest)"),
				fmt.Errorf("exit status 1")
		},
		func(_ context.Context, pr int) error { return nil },
	)
	// checkMergedStateFn returns MERGED — PR did actually merge.
	fw.checkMergedStateFn = func(_ context.Context, pr int) string {
		return "MERGED"
	}

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(2100)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "merged" {
		t.Errorf("status: got %q, want merged", row.Status)
	}
	if row.MergedAt == nil {
		t.Error("merged_at: got nil, want set")
	}
	// Success notification must have been sent.
	if srv.called == 0 {
		t.Fatal("no success notification sent to coordinator")
	}
	var body map[string]any
	if err := json.Unmarshal(srv.lastBody, &body); err != nil {
		t.Fatalf("unmarshal notification body: %v", err)
	}
	parts, _ := body["parts"].([]interface{})
	if len(parts) == 0 {
		t.Fatal("notification body has no parts")
	}
	part, _ := parts[0].(map[string]interface{})
	text, _ := part["text"].(string)
	if !strings.Contains(text, "PR #2100") {
		t.Errorf("notification text %q does not mention PR #2100", text)
	}
	if !strings.Contains(text, "merged") {
		t.Errorf("notification text %q does not mention 'merged'", text)
	}
}

// TestWatcher_MergeAlreadyInProgress_PRIsOpen verifies AC: when gh pr merge
// returns an error and checkPRMergedState returns "OPEN" (not merged), the
// watcher falls through to the keyword check and then to failAndNotify.
func TestWatcher_MergeAlreadyInProgress_PRIsOpen(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-reconcile-open"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-reconcile-open")

	if _, err := d.EnqueueMerge(2101, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "CLEAN"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			// Error that doesn't match any keyword.
			return []byte("GraphQL: Merge already in progress (mergePullRequest)"),
				fmt.Errorf("exit status 1")
		},
		func(_ context.Context, pr int) error { return nil },
	)
	// checkMergedStateFn returns OPEN — PR did NOT merge.
	fw.checkMergedStateFn = func(_ context.Context, pr int) string {
		return "OPEN"
	}

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(2101)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed", row.Status)
	}
}

// TestWatcher_MergeError_CheckPRStateItself_Fails verifies AC (edge-case):
// when gh pr merge errors AND checkPRMergedState itself fails (returns ""),
// the watcher falls through to the keyword check. When neither the state
// reconciliation nor the keywords match, failAndNotify is called.
func TestWatcher_MergeError_CheckPRStateItself_Fails(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-reconcile-viewerr"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-reconcile-viewerr")

	if _, err := d.EnqueueMerge(2102, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "CLEAN"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			// Error that doesn't match any keyword.
			return []byte("GraphQL: Merge already in progress (mergePullRequest)"),
				fmt.Errorf("exit status 1")
		},
		func(_ context.Context, pr int) error { return nil },
	)
	// checkMergedStateFn returns "" — gh pr view failed (network error, etc.).
	fw.checkMergedStateFn = func(_ context.Context, pr int) string {
		return ""
	}

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(2102)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed (checkMergedState failure must fall through to failAndNotify)", row.Status)
	}
}

// TestWatcher_GenuineFailure_NoRegression verifies AC: a genuine gh pr merge
// error (not a race keyword, not reconciled as MERGED) still transitions to
// failed. This is a non-regression test for the existing failure path.
func TestWatcher_GenuineFailure_NoRegression(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-genuine-fail"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-genuine-fail")

	if _, err := d.EnqueueMerge(2103, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "CLEAN"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			// A genuine failure unrelated to any race.
			return []byte("GraphQL: repository has no pull request with the number 2103"),
				fmt.Errorf("exit status 1")
		},
		func(_ context.Context, pr int) error { return nil },
	)
	// checkMergedStateFn returns OPEN — PR was not merged.
	fw.checkMergedStateFn = func(_ context.Context, pr int) string {
		return "OPEN"
	}

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(2103)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed (genuine error must not reconcile as merged)", row.Status)
	}
}

// ── lookupWorkerArchivePath scoping tests ─────────────────────────────────────

// seedWorkerSession inserts a sessions row simulating a worker session for the
// given PR branch (session_name = "<repoShort>@<pr>", repo = repoFull). The
// optional archivePath is set on the row if non-empty.
func seedWorkerSession(t *testing.T, d *db.DB, instanceID, sessionName, repo, archivePath string) {
	t.Helper()
	sess := db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    "/tmp/wt-" + instanceID,
		Harness:     "pi",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("seedWorkerSession InsertSession(%s): %v", sessionName, err)
	}
	if archivePath != "" {
		if err := d.UpdateSessionArchivePath(instanceID, archivePath); err != nil {
			t.Fatalf("seedWorkerSession UpdateSessionArchivePath(%s): %v", instanceID, err)
		}
	}
}

// seedCoordinatorSession inserts both an agent_status row and a sessions row for
// a coordinator, so that SessionByInstanceID can resolve it.
func seedCoordinatorSession(t *testing.T, d *db.DB, sessionName, instanceID, repo string) {
	t.Helper()
	if err := d.UpsertStatus(sessionName, repo, "/tmp/coord-wt", "active", nil, nil); err != nil {
		t.Fatalf("seedCoordinatorSession UpsertStatus(%s): %v", sessionName, err)
	}
	if err := d.SetInstanceID(sessionName, instanceID); err != nil {
		t.Fatalf("seedCoordinatorSession SetInstanceID(%s): %v", sessionName, err)
	}
	sess := db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    "/tmp/coord-wt",
		Harness:     "pi",
	}
	if err := d.InsertSession(sess); err != nil {
		t.Fatalf("seedCoordinatorSession InsertSession(%s): %v", sessionName, err)
	}
}

// TestLookupWorkerArchivePath_InstanceIDScoping verifies that when two repos
// share a PR number (e.g. nixos-config@782 and myrepo@782), the primary lookup
// path (head.InstanceID non-empty) returns the correct archive for the repo
// whose coordinator enqueued the merge.
func TestLookupWorkerArchivePath_InstanceIDScoping(t *testing.T) {
	d := openTestDB(t)

	// Coordinator for nixos-config.
	coordInstanceID := "coord-inst-nixos-782"
	coordSession := "nixos-config@main"
	coordRepo := "owner/nixos-config"
	seedCoordinatorSession(t, d, coordSession, coordInstanceID, coordRepo)

	// Worker sessions: one per repo, both ending in @782.
	seedWorkerSession(t, d, "worker-inst-nixos-782", "nixos-config@782", "owner/nixos-config",
		"/archives/nixos-config-782.tar.gz")
	seedWorkerSession(t, d, "worker-inst-myrepo-782", "myrepo@782", "owner/myrepo",
		"/archives/myrepo-782.tar.gz")

	w := &Watcher{
		db:          d,
		instanceID:  coordInstanceID,
		sessionName: coordSession,
		httpClient:  http.DefaultClient,
		repo:        coordRepo,
	}

	head := &db.PendingMerge{
		PR:          782,
		SessionName: coordSession,
		InstanceID:  coordInstanceID,
	}

	got := w.lookupWorkerArchivePath(head)
	want := "/archives/nixos-config-782.tar.gz"
	if got != want {
		t.Errorf("lookupWorkerArchivePath: got %q, want %q", got, want)
	}
}

// TestLookupWorkerArchivePath_LegacyRepoFallback verifies that when
// head.InstanceID is empty (a legacy pending_merges row), lookupWorkerArchivePath
// falls back to scoping by w.repo and still returns the correct archive.
func TestLookupWorkerArchivePath_LegacyRepoFallback(t *testing.T) {
	d := openTestDB(t)

	// Worker sessions: one per repo, both ending in @782.
	seedWorkerSession(t, d, "worker-leg-nixos-782", "nixos-config@782", "owner/nixos-config",
		"/archives/nixos-config-legacy-782.tar.gz")
	seedWorkerSession(t, d, "worker-leg-myrepo-782", "myrepo@782", "owner/myrepo",
		"/archives/myrepo-legacy-782.tar.gz")

	w := &Watcher{
		db:          d,
		instanceID:  "",
		sessionName: "nixos-config@main",
		httpClient:  http.DefaultClient,
		repo:        "owner/nixos-config",
	}

	// Legacy row: InstanceID is empty.
	head := &db.PendingMerge{
		PR:          782,
		SessionName: "nixos-config@main",
		InstanceID:  "",
	}

	got := w.lookupWorkerArchivePath(head)
	want := "/archives/nixos-config-legacy-782.tar.gz"
	if got != want {
		t.Errorf("lookupWorkerArchivePath legacy fallback: got %q, want %q", got, want)
	}
}

// TestFetchRequiredChecks_ErrorStaysWatching verifies the Watcher.tick()
// integration: when fetchRequiredChecks returns an error on an UNSTABLE PR,
// the row stays in watching and no merge is attempted. This exercises the
// real tick() path (not fakeWatcher) using a stubbed runGHFunc.
func TestFetchRequiredChecks_ErrorStaysWatching(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-bprot-err-tick"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(2001, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	w := &Watcher{
		db:          d,
		instanceID:  instanceID,
		sessionName: session,
		httpClient:  http.DefaultClient,
		repo:        "owner/testrepo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			// gh pr view succeeds, gh api branch protection fails.
			if len(args) >= 3 && args[2] == "pr" && args[3] == "view" {
				return []byte(`{"state":"OPEN","mergedAt":null,"mergeStateStatus":"UNSTABLE","statusCheckRollup":[{"name":"pr-gate","conclusion":"SUCCESS","status":"COMPLETED"}],"reviewDecision":""}`), nil
			}
			// Branch protection API call → simulate failure.
			return []byte("forbidden"), fmt.Errorf("exit status 1")
		},
	}

	w.tick(context.Background())

	row, err := d.PendingMergeByPR(2001)
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching (branch-protection fetch error must not merge)", row.Status)
	}
}
