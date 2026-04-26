package mergequeue

import (
	"context"
	"encoding/json"
	"fmt"
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
	watcher  *Watcher
	fetchFn  func(ctx context.Context, pr int) (*prInfo, error)
	mergeFn  func(ctx context.Context, pr int) ([]byte, error)
	updateFn func(ctx context.Context, pr int) error
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
		}

	case "UNSTABLE", "UNKNOWN", "HAS_HOOKS", "DRAFT":
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
	sid := "opencode-sid-clean"
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
	sid := "opencode-sid-dirty"
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

// TestWatcher_UNSTABLEStaysWatching verifies UNSTABLE leaves the row watching.
func TestWatcher_UNSTABLEStaysWatching(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-unstable"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(600, session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, http.DefaultClient,
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "UNSTABLE"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

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
		output  string
		isRace  bool
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
	if version != 22 {
		t.Errorf("schema_version after second open: got %d, want 22", version)
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
	for _, f := range fields {
		if f == "mergedAt" {
			hasMergedAt = true
		}
	}
	if !hasMergedAt {
		t.Errorf("prInfoJSONFields must include 'mergedAt' to detect merged state. Got: %q", prInfoJSONFields)
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
		name     string
		info     prInfo
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
				return []byte(`{"state":"OPEN","mergedAt":null,"mergeStateStatus":"CLEAN","statusCheckRollup":[]}`), nil
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
