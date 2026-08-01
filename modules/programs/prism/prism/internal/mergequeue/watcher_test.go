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
	m3, err := d.EnqueueMerge(300, "myrepo", session, instanceID, nil)
	if err != nil {
		t.Fatalf("EnqueueMerge(300): %v", err)
	}
	time.Sleep(time.Millisecond) // ensure distinct timestamps
	m2, err := d.EnqueueMerge(200, "myrepo", session, instanceID, nil)
	if err != nil {
		t.Fatalf("EnqueueMerge(200): %v", err)
	}
	time.Sleep(time.Millisecond)
	m1, err := d.EnqueueMerge(100, "myrepo", session, instanceID, nil)
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

	m1, err := d.EnqueueMerge(42, "myrepo", session, instanceID, nil)
	if err != nil {
		t.Fatalf("first EnqueueMerge: %v", err)
	}

	m2, err := d.EnqueueMerge(42, "myrepo", session, instanceID, nil)
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
	if _, err := d.EnqueueMerge(99, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	if err := d.TerminateMerge(99, "myrepo", "failed", "CI failed"); err != nil {
		t.Fatalf("TerminateMerge: %v", err)
	}

	// Re-enqueue — should produce a fresh watching row.
	m, err := d.EnqueueMerge(99, "myrepo", session, instanceID, nil)
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
	if _, err := d.EnqueueMerge(1, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(1): %v", err)
	}
	if _, err := d.EnqueueMerge(2, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(2): %v", err)
	}

	// Abandon all watching rows.
	if err := d.AbandonWatchingMerges(instanceID); err != nil {
		t.Fatalf("AbandonWatchingMerges: %v", err)
	}

	// Both rows should now be abandoned.
	for _, pr := range []int{1, 2} {
		row, err := d.PendingMergeByPR(pr, "myrepo")
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

	if _, err := d.EnqueueMerge(55, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	// Cancel it.
	cancelled, err := d.CancelMerge(55, "myrepo", instanceID)
	if err != nil {
		t.Fatalf("CancelMerge: %v", err)
	}
	if !cancelled {
		t.Error("CancelMerge: returned false, want true")
	}

	row, err := d.PendingMergeByPR(55, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "cancelled" {
		t.Errorf("status after cancel: got %q, want cancelled", row.Status)
	}

	// Cancelling again returns false (already terminal).
	cancelled2, err := d.CancelMerge(55, "myrepo", instanceID)
	if err != nil {
		t.Fatalf("second CancelMerge: %v", err)
	}
	if cancelled2 {
		t.Error("second CancelMerge: returned true, want false")
	}

	// Wrong instanceID returns false.
	if _, err := d.EnqueueMerge(66, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(66): %v", err)
	}
	cancelled3, err := d.CancelMerge(66, "myrepo", "other-instance")
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
	if _, err := d.EnqueueMerge(10, "repo-a", sessionA, "inst-A", nil); err != nil {
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
	if _, err := d.EnqueueMerge(42, "myrepo", session, mintedInstanceID, nil); err != nil {
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
	if _, err := d.EnqueueMerge(1, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(1): %v", err)
	}
	if _, err := d.EnqueueMerge(2, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(2): %v", err)
	}
	if _, err := d.EnqueueMerge(3, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(3): %v", err)
	}
	if _, err := d.EnqueueMerge(4, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(4): %v", err)
	}

	// Fail PR 2.
	if err := d.TerminateMerge(2, "myrepo", "failed", "CI failed"); err != nil {
		t.Fatalf("TerminateMerge(2): %v", err)
	}
	// Cancel PR 3.
	if _, err := d.CancelMerge(3, "myrepo", instanceID); err != nil {
		t.Fatalf("CancelMerge(3): %v", err)
	}
	// Abandon PR 4 via AbandonWatchingMerges (simulates coordinator shutdown).
	// First, mark PR 1 as merged so only PR 4 is still watching.
	if err := d.TerminateMerge(1, "myrepo", "merged", ""); err != nil {
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
	if _, err := d.EnqueueMerge(77, "myrepo", session, oldInstance, nil); err != nil {
		t.Fatalf("EnqueueMerge(old): %v", err)
	}
	if err := d.AbandonWatchingMerges(oldInstance); err != nil {
		t.Fatalf("AbandonWatchingMerges(old): %v", err)
	}

	// Enqueue under new instance.
	if _, err := d.EnqueueMerge(88, "myrepo", session, newInstance, nil); err != nil {
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
	// fetchProtectionFn injects a branch-protection snapshot for the #2420
	// tick path. When nil, protection defaults to configured=true with
	// requiredChecks derived from fetchRequiredChecksFn (or empty if that is
	// also nil) so existing tests that predate the branch-protection gate
	// keep exercising the merge path.
	fetchProtectionFn func(ctx context.Context) (protectionCache, error)
	// checkMergedStateFn is called on the error path of tryMerge to reconcile
	// PR state. When nil, returns "" (unknown / fall-through to keyword check).
	checkMergedStateFn func(ctx context.Context, pr int) string
}

// processHead is a test-friendly version of tick() that uses injected functions
// instead of the real gh CLI. Mirrors the #2420 async-poll state machine as
// amended by #2525: a coordinator notification is produced only by a
// prism-driven merge, an out-of-band merge, a close without merge, a genuine
// merge mutation failure, or a BLOCKED PR whose required checks have all
// concluded with at least one failure. All other states stay watching
// silently. An unprotected repo NEVER auto-merges.
//
// Prefer driving the real Watcher.tick through an injected runGHFunc for new
// tests — see watcher_blocked_ci_failure_test.go. This mirror must be kept in
// step with tick() by hand, so it is the weaker signal of the two.
func (fw *fakeWatcher) processHead(ctx context.Context) {
	head, err := fw.watcher.db.MergeQueueHead(fw.watcher.sessionName)
	if err != nil || head == nil {
		return
	}
	_ = fw.watcher.db.UpdateMergeLastChecked(head.PR, head.Repo)

	prInfoVal, err := fw.fetchFn(ctx, head.PR)
	if err != nil {
		return
	}

	// Terminal: out-of-band merge.
	if prInfoVal.State == "MERGED" || prInfoVal.isMerged() {
		fw.watcher.succeedAndNotify(ctx, head, mergeOutcomeExternal, prInfoVal)
		return
	}
	// Terminal: closed without merging.
	if prInfoVal.State == "CLOSED" && !prInfoVal.isMerged() {
		fw.watcher.notifyClosedNotMerged(ctx, head)
		return
	}

	// Branch-protection gate (#2420): NEVER auto-merge without protection.
	prot, protErr := fw.resolveProtection(ctx)
	if protErr != nil {
		return // stay watching
	}
	if !prot.configured {
		// Unprotected repo — stay watching for a human to merge/close.
		return
	}

	switch prInfoVal.MergeStateStatus {
	case "CLEAN":
		fw.attemptMerge(ctx, head)

	case "UNSTABLE":
		if requiredChecksAllPassed(prInfoVal.StatusCheckRollup, prot.requiredChecks) {
			fw.attemptMerge(ctx, head)
		}
		// else stay watching if required checks not all passed

	case "BEHIND":
		_ = fw.updateFn(ctx, head.PR)

	case "BLOCKED":
		// #2525: terminal only when the required set is fully accounted for
		// and a real failure conclusion is present; otherwise silent.
		if failed := failedRequiredChecks(prInfoVal.StatusCheckRollup, prot.requiredChecks); len(failed) > 0 {
			fw.watcher.notifyRequiredChecksFailed(ctx, head, failed)
			return
		}

	case "DIRTY", "UNKNOWN", "HAS_HOOKS", "DRAFT":
		// #2420: stay watching silently — no coordinator notification.

	}
}

// resolveProtection returns the injected protection snapshot or a synthetic
// default (configured=true, requiredChecks from fetchRequiredChecksFn if set)
// so tests that predate the branch-protection gate continue to exercise the
// merge path without additional wiring.
func (fw *fakeWatcher) resolveProtection(ctx context.Context) (protectionCache, error) {
	if fw.fetchProtectionFn != nil {
		return fw.fetchProtectionFn(ctx)
	}
	var required []string
	if fw.fetchRequiredChecksFn != nil {
		var err error
		required, err = fw.fetchRequiredChecksFn(ctx)
		if err != nil {
			return protectionCache{}, err
		}
	}
	return protectionCache{configured: true, requiredChecks: required}, nil
}

// attemptMerge invokes the injected merge function with the CLEAN/UNSTABLE
// reconciliation and branch-moved-race handling that production tick uses.
func (fw *fakeWatcher) attemptMerge(ctx context.Context, head *db.PendingMerge) {
	out, mergeErr := fw.mergeFn(ctx, head.PR)
	if mergeErr == nil {
		fw.watcher.succeedAndNotify(ctx, head, mergeOutcomePrismDriven, nil)
		return
	}
	// First: reconcile by checking the PR's actual state.
	var reconciledState string
	if fw.checkMergedStateFn != nil {
		reconciledState = fw.checkMergedStateFn(ctx, head.PR)
	}
	if reconciledState == "MERGED" {
		fw.watcher.succeedAndNotify(ctx, head, mergeOutcomeReconciled, nil)
		return
	}
	// Second: keyword-based branch-moved-race check.
	combined := strings.ToLower(string(out) + mergeErr.Error())
	if isBranchMovedRace(combined) {
		return // transient
	}
	errMsg := fmt.Sprintf("gh pr merge failed: %s", strings.TrimSpace(string(out)))
	fw.watcher.failAndNotify(head, errMsg)
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
	if _, err := d.EnqueueMerge(100, "myrepo", session, instanceID, nil); err != nil {
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
	row, err := d.PendingMergeByPR(100, "myrepo")
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

	if _, err := d.EnqueueMerge(200, "myrepo", session, instanceID, nil); err != nil {
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
	row, err := d.PendingMergeByPR(200, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status after BEHIND: got %q, want watching", row.Status)
	}
}

// TestWatcher_UNSTABLEStaysWatchingWhenRequiredPending verifies that when
// UNSTABLE and a required check is still in progress, the row stays watching.
func TestWatcher_UNSTABLEStaysWatchingWhenRequiredPending(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-unstable"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(600, "myrepo", session, instanceID, nil); err != nil {
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

	row, err := d.PendingMergeByPR(600, "myrepo")
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

	if _, err := d.EnqueueMerge(700, "myrepo", session, instanceID, nil); err != nil {
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

	row, err := d.PendingMergeByPR(700, "myrepo")
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

	if _, err := d.EnqueueMerge(800, "myrepo", session, instanceID, nil); err != nil {
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

	row, err := d.PendingMergeByPR(800, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status after race: got %q, want watching", row.Status)
	}
}

// TestWatcher_ClosedExternallyTransitionsToFailed verifies that during polling,
// a PR observed as CLOSED (without merge) terminates the row as failed and
// delivers the #2420 closed-without-merge notification: text contains
// "closed without merge" plus "Please clean up the branch and worktree" and
// does not imply prism performed the cleanup.
func TestWatcher_ClosedExternallyTransitionsToFailed(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-closed"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-closed")

	if _, err := d.EnqueueMerge(900, "myrepo", session, instanceID, nil); err != nil {
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

	row, err := d.PendingMergeByPR(900, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed", row.Status)
	}

	if srv.called == 0 {
		t.Fatal("no closed-without-merge notification sent to coordinator")
	}
	text := extractNotifyText(t, srv.lastBody)
	if !strings.Contains(text, "PR #900") {
		t.Errorf("notification text %q does not mention PR #900", text)
	}
	if !strings.Contains(text, "closed without merge") {
		t.Errorf("notification text %q does not contain 'closed without merge' (#2420 discipline)", text)
	}
	if !strings.Contains(text, "Please clean up the branch and worktree") {
		t.Errorf("notification text %q does not contain #2420 completion-discipline phrase 'Please clean up the branch and worktree'", text)
	}
	if strings.Contains(text, "prism cleanup") {
		t.Errorf("notification text %q must NOT imply prism performed the cleanup itself", text)
	}
}

// TestWatcher_Cancellation verifies CancelMerge terminates a watching row.
func TestWatcher_Cancellation(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-canc"
	session := "myrepo@main"

	if _, err := d.EnqueueMerge(111, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	cancelled, err := d.CancelMerge(111, "myrepo", instanceID)
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
	if _, err := d.EnqueueMerge(10, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(10): %v", err)
	}
	time.Sleep(time.Millisecond)
	if _, err := d.EnqueueMerge(20, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge(20): %v", err)
	}

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-promote")

	// First tick: head is PR 10 — report as CLOSED so it terminates via
	// notifyClosedNotMerged. Using CLOSED (a #2420-terminal state) rather
	// than DIRTY (which now stays watching silently mid-poll) is the way
	// to drive a promotion transition in the new state machine.
	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "CLOSED", MergedAt: nil}, nil
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
	if version < 38 {
		t.Errorf("schema_version after second open: got %d, want >= 38", version)
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

	if _, err := d.EnqueueMerge(123, "myrepo", session, instanceID, nil); err != nil {
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
	if !strings.Contains(text, "Please clean up the branch and worktree") {
		t.Errorf("notification text %q does not contain #2420 completion-discipline phrase 'Please clean up the branch and worktree'", text)
	}
	if strings.Contains(text, "prism cleanup") {
		t.Errorf("notification text %q must not instruct `prism cleanup` — the coordinator does the cleanup, prism does not", text)
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
	hasMergedBy := false
	for _, f := range fields {
		if f == "mergedAt" {
			hasMergedAt = true
		}
		if f == "reviewDecision" {
			hasReviewDecision = true
		}
		if f == "mergedBy" {
			hasMergedBy = true
		}
	}
	if !hasMergedAt {
		t.Errorf("prInfoJSONFields must include 'mergedAt' to detect merged state. Got: %q", prInfoJSONFields)
	}
	if !hasReviewDecision {
		t.Errorf("prInfoJSONFields must include 'reviewDecision' to detect review-required blocks (#1357). Got: %q", prInfoJSONFields)
	}
	if !hasMergedBy {
		t.Errorf("prInfoJSONFields must include 'mergedBy' to name the merger in the external-merge notification (#2298). Got: %q", prInfoJSONFields)
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
	if _, err := d.EnqueueMerge(202, "myrepo", session, instanceID, nil); err != nil {
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

	if _, err := d.EnqueueMerge(1001, "myrepo", session, instanceID, nil); err != nil {
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
	row, err := d.PendingMergeByPR(1001, "myrepo")
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

	if _, err := d.EnqueueMerge(1002, "myrepo", session, instanceID, nil); err != nil {
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
	row, err := d.PendingMergeByPR(1002, "myrepo")
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

	if _, err := d.EnqueueMerge(1003, "myrepo", session, instanceID, nil); err != nil {
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
	row, err := d.PendingMergeByPR(1003, "myrepo")
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

	if _, err := d.EnqueueMerge(1004, "myrepo", session, instanceID, nil); err != nil {
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
	row, err := d.PendingMergeByPR(1004, "myrepo")
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
	current = current.Add(protectionCacheTTL + time.Second)

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

	if _, err := d.EnqueueMerge(2100, "myrepo", session, instanceID, nil); err != nil {
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

	row, err := d.PendingMergeByPR(2100, "myrepo")
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

	if _, err := d.EnqueueMerge(2101, "myrepo", session, instanceID, nil); err != nil {
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

	row, err := d.PendingMergeByPR(2101, "myrepo")
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

	if _, err := d.EnqueueMerge(2102, "myrepo", session, instanceID, nil); err != nil {
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

	row, err := d.PendingMergeByPR(2102, "myrepo")
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

	if _, err := d.EnqueueMerge(2103, "myrepo", session, instanceID, nil); err != nil {
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

	row, err := d.PendingMergeByPR(2103, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q, want failed (genuine error must not reconcile as merged)", row.Status)
	}
}

// ── succeedAndNotify variant rendering tests (issue #2298) ──────────────────

// extractNotifyText pulls the parts[0].text string out of the most recent
// JSON body the capturing server received. Test-only helper.
func extractNotifyText(t *testing.T, body []byte) string {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("extractNotifyText: unmarshal: %v", err)
	}
	parts, _ := parsed["parts"].([]interface{})
	if len(parts) == 0 {
		t.Fatal("extractNotifyText: notification body has no parts")
	}
	part, _ := parts[0].(map[string]interface{})
	text, _ := part["text"].(string)
	return text
}

// TestWatcher_ExternalMerge_NotifyTextDistinguishedAndNamesByLogin verifies
// AC1 (#2298): when the pre-poll early-return fires (prInfo.State == "MERGED"
// or prInfo.isMerged()), the notification text identifies the PR as merged
// externally, names the merger by GitHub login, includes the merge
// timestamp, and does NOT instruct the coordinator to run `prism cleanup`.
func TestWatcher_ExternalMerge_NotifyTextDistinguishedAndNamesByLogin(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-ext-merge"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-ext-merge")

	if _, err := d.EnqueueMerge(2200, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	mergedAt := "2026-06-16T22:32:47Z"
	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{
				State:    "MERGED",
				MergedAt: &mergedAt,
				MergedBy: &userRef{Login: "b-h-mck"},
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			t.Fatal("external merge path must not invoke gh pr merge")
			return nil, nil
		},
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	if srv.called == 0 {
		t.Fatal("no notification sent to coordinator")
	}
	text := extractNotifyText(t, srv.lastBody)

	if !strings.Contains(text, "PR #2200") {
		t.Errorf("notification text %q does not mention 'PR #2200'", text)
	}
	if !strings.Contains(text, "merged out-of-band") {
		t.Errorf("notification text %q does not contain 'merged out-of-band' — external-merge case must be distinguished from prism-driven (#2420)", text)
	}
	if !strings.Contains(text, "@b-h-mck") {
		t.Errorf("notification text %q does not name merger @b-h-mck", text)
	}
	if !strings.Contains(text, mergedAt) {
		t.Errorf("notification text %q does not include merge timestamp %q", text, mergedAt)
	}
	if !strings.Contains(text, "Please clean up the branch and worktree") {
		t.Errorf("notification text %q does not contain #2420 completion-discipline phrase 'Please clean up the branch and worktree'", text)
	}
	if strings.Contains(text, "prism cleanup") {
		t.Errorf("notification text %q must NOT imply prism performed the cleanup itself", text)
	}

	// Row must still terminate as merged (DB state unchanged from current behaviour).
	row, err := d.PendingMergeByPR(2200, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "merged" {
		t.Errorf("status: got %q, want merged (DB transition unchanged for external case)", row.Status)
	}
}

// TestWatcher_ExternalMerge_MergedByNullDegradesGracefully verifies AC4
// (#2298): when gh returns mergedBy=null (or an empty login), the
// external-merge notification still emits successfully — the merger field
// degrades gracefully rather than blocking the notification.
func TestWatcher_ExternalMerge_MergedByNullDegradesGracefully(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-ext-merge-null"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-ext-merge-null")

	if _, err := d.EnqueueMerge(2201, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	mergedAt := "2026-06-17T09:00:00Z"
	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			// MergedBy is nil — gh returned "mergedBy": null.
			return &prInfo{
				State:    "MERGED",
				MergedAt: &mergedAt,
				MergedBy: nil,
			}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)

	fw.processHead(context.Background())

	if srv.called == 0 {
		t.Fatal("no notification sent to coordinator — mergedBy=nil must not block notification")
	}
	text := extractNotifyText(t, srv.lastBody)

	if !strings.Contains(text, "PR #2201") {
		t.Errorf("notification text %q does not mention 'PR #2201'", text)
	}
	if !strings.Contains(text, "merged out-of-band") {
		t.Errorf("notification text %q does not contain 'merged out-of-band' (#2420)", text)
	}
	if strings.Contains(text, "@") {
		t.Errorf("notification text %q contains '@' — with mergedBy=nil the merger should be omitted, not emitted as an empty handle", text)
	}
	if !strings.Contains(text, mergedAt) {
		t.Errorf("notification text %q does not include merge timestamp %q (graceful degrade must still surface mergedAt when present)", text, mergedAt)
	}
	if !strings.Contains(text, "Please clean up the branch and worktree") {
		t.Errorf("notification text %q does not contain #2420 completion-discipline phrase 'Please clean up the branch and worktree'", text)
	}
}

// TestWatcher_PrismDrivenMerge_NotifyTextUnchanged_NoArchive verifies AC2
// (#2298): when tryMerge succeeds via `gh pr merge --squash` and no worker
// archive_path is recorded, the notification text matches the existing
// `PR #N merged. Run \`git pull\` ...` form byte-for-byte. Non-regression
// for the canonical case.
func TestWatcher_PrismDrivenMerge_NotifyTextUnchanged_NoArchive(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-prism-merge-noarch"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-prism-merge-noarch")

	if _, err := d.EnqueueMerge(2210, "myrepo", session, instanceID, nil); err != nil {
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

	if srv.called == 0 {
		t.Fatal("no notification sent to coordinator")
	}
	text := extractNotifyText(t, srv.lastBody)

	want := "PR #2210 merged. Please clean up the branch and worktree."
	if text != want {
		t.Errorf("prism-driven notification (no archive) text mismatch —\n got:  %q\n want: %q", text, want)
	}
}

// TestWatcher_PrismDrivenMerge_NotifyTextUnchanged_WithArchive verifies AC2
// (#2298): when tryMerge succeeds and a worker archive_path is recorded, the
// notification text matches the existing
// `PR #N merged. Archive: <path>. Run \`git pull\` ...` form byte-for-byte.
func TestWatcher_PrismDrivenMerge_NotifyTextUnchanged_WithArchive(t *testing.T) {
	d := openTestDB(t)
	coordInstanceID := "inst-coord-arch"
	coordSession := "myrepo@main"
	coordRepo := "owner/myrepo"

	// Seed an agent_status row for notification routing.
	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, coordSession, coordInstanceID, port, "sid-coord-arch")

	// Seed both a coordinator sessions row (so lookupWorkerArchivePath can
	// resolve the coordinator's repo via instance_id) and a worker session
	// whose archive_path will be surfaced in the notification.
	coordSess := db.Session{
		InstanceID:  coordInstanceID,
		SessionName: coordSession,
		Repo:        coordRepo,
		Worktree:    "/tmp/coord-wt-arch",
		Harness:     "pi",
	}
	if err := d.InsertSession(coordSess); err != nil {
		t.Fatalf("insert coordinator session: %v", err)
	}
	seedWorkerSession(t, d, "worker-inst-arch-2211", "myrepo@2211", coordRepo,
		"/archives/myrepo-2211.tar.gz")

	if _, err := d.EnqueueMerge(2211, "myrepo", coordSession, coordInstanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, coordInstanceID, coordSession, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "CLEAN"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) { return nil, nil },
		func(_ context.Context, pr int) error { return nil },
	)
	// Pin the watcher's repo so lookupWorkerArchivePath has a non-empty scope.
	fw.watcher.repo = coordRepo

	fw.processHead(context.Background())

	if srv.called == 0 {
		t.Fatal("no notification sent to coordinator")
	}
	text := extractNotifyText(t, srv.lastBody)

	want := "PR #2211 merged. Archive: /archives/myrepo-2211.tar.gz. Please clean up the branch and worktree."
	if text != want {
		t.Errorf("prism-driven notification (with archive) text mismatch —\n got:  %q\n want: %q", text, want)
	}
}

// TestWatcher_Reconciled_NotifyTextIncludesReconciledNote verifies AC3
// (#2298): when tryMerge errors but checkPRMergedState returns MERGED, the
// notification text indicates the merge was reconciled from an errored
// mutation AND still instructs `git pull` + `prism cleanup` (this IS a
// prism-driven merge, just with a recovery breadcrumb). The text is distinct
// from both the canonical and the external cases.
func TestWatcher_Reconciled_NotifyTextIncludesReconciledNote(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-reconciled-text"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-reconciled-text")

	if _, err := d.EnqueueMerge(2220, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "CLEAN"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			return []byte("GraphQL: Merge already in progress (mergePullRequest)"),
				fmt.Errorf("exit status 1")
		},
		func(_ context.Context, pr int) error { return nil },
	)
	fw.checkMergedStateFn = func(_ context.Context, pr int) string { return "MERGED" }

	fw.processHead(context.Background())

	if srv.called == 0 {
		t.Fatal("no notification sent to coordinator")
	}
	text := extractNotifyText(t, srv.lastBody)

	if !strings.Contains(text, "PR #2220") {
		t.Errorf("notification text %q does not mention 'PR #2220'", text)
	}
	if !strings.Contains(text, "Reconciled") {
		t.Errorf("notification text %q does not contain reconciliation breadcrumb — reconciled case must be distinct from canonical case", text)
	}
	if !strings.Contains(text, "Please clean up the branch and worktree") {
		t.Errorf("notification text %q does not contain #2420 completion-discipline phrase 'Please clean up the branch and worktree' — reconciled case is still a prism-driven merge", text)
	}
	if strings.Contains(text, "prism cleanup") {
		t.Errorf("notification text %q must NOT imply prism performed the cleanup itself (#2420)", text)
	}
	if strings.Contains(text, "merged out-of-band") {
		t.Errorf("notification text %q contains 'merged out-of-band' — reconciled case must NOT be mistaken for external merge", text)
	}
}

// TestRenderExternalMergeNotifyText_NilPrInfo verifies the table of
// degraded-info renderings: with prInfo == nil the text emits with no
// merger detail and still avoids the `prism cleanup` instruction. Pure
// rendering test (no DB, no notify) so we can pin the exact strings.
func TestRenderExternalMergeNotifyText_NilPrInfo(t *testing.T) {
	got := renderExternalMergeNotifyText(42, nil)
	want := "PR #42 merged out-of-band. Please clean up the branch and worktree."
	if got != want {
		t.Errorf("renderExternalMergeNotifyText(42, nil) =\n got:  %q\n want: %q", got, want)
	}
}

// TestRenderExternalMergeNotifyText_LoginAndTimestamp pins the happy-path
// rendering used by AC1.
func TestRenderExternalMergeNotifyText_LoginAndTimestamp(t *testing.T) {
	ts := "2026-06-16T22:32:47Z"
	info := &prInfo{
		MergedAt: &ts,
		MergedBy: &userRef{Login: "b-h-mck"},
	}
	got := renderExternalMergeNotifyText(15, info)
	want := "PR #15 merged out-of-band (merged by @b-h-mck at 2026-06-16T22:32:47Z). Please clean up the branch and worktree."
	if got != want {
		t.Errorf("renderExternalMergeNotifyText —\n got:  %q\n want: %q", got, want)
	}
}

// TestRenderExternalMergeNotifyText_LoginOnly verifies the case where gh
// returned mergedBy but mergedAt is unset (defensive — should not normally
// happen on a MERGED PR but we still want to render gracefully).
func TestRenderExternalMergeNotifyText_LoginOnly(t *testing.T) {
	info := &prInfo{
		MergedAt: nil,
		MergedBy: &userRef{Login: "someone"},
	}
	got := renderExternalMergeNotifyText(99, info)
	want := "PR #99 merged out-of-band (merged by @someone). Please clean up the branch and worktree."
	if got != want {
		t.Errorf("renderExternalMergeNotifyText —\n got:  %q\n want: %q", got, want)
	}
}

// TestRenderExternalMergeNotifyText_TimestampOnly verifies the case where gh
// returned mergedAt but mergedBy=null — still emits a useful breadcrumb.
func TestRenderExternalMergeNotifyText_TimestampOnly(t *testing.T) {
	ts := "2026-06-17T09:00:00Z"
	info := &prInfo{
		MergedAt: &ts,
		MergedBy: nil,
	}
	got := renderExternalMergeNotifyText(123, info)
	want := "PR #123 merged out-of-band (merged at 2026-06-17T09:00:00Z). Please clean up the branch and worktree."
	if got != want {
		t.Errorf("renderExternalMergeNotifyText —\n got:  %q\n want: %q", got, want)
	}
}

// TestRenderExternalMergeNotifyText_EmptyLogin verifies that a mergedBy
// object with an empty login string (rare but possible — e.g. a deleted
// account) is treated identically to mergedBy=nil: no "@" is emitted.
func TestRenderExternalMergeNotifyText_EmptyLogin(t *testing.T) {
	ts := "2026-06-17T09:00:00Z"
	info := &prInfo{
		MergedAt: &ts,
		MergedBy: &userRef{Login: ""},
	}
	got := renderExternalMergeNotifyText(456, info)
	want := "PR #456 merged out-of-band (merged at 2026-06-17T09:00:00Z). Please clean up the branch and worktree."
	if got != want {
		t.Errorf("renderExternalMergeNotifyText —\n got:  %q\n want: %q", got, want)
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

	if _, err := d.EnqueueMerge(2001, "myrepo", session, instanceID, nil); err != nil {
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

	row, err := d.PendingMergeByPR(2001, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching (branch-protection fetch error must not merge)", row.Status)
	}
}

// ── #2420 async-poll state-machine tests ──────────────────────────────────────

// TestWatcher_Unprotected_NeverAutoMerges is the core #2420 rule: when the
// branch-protection API returns HTTP 404, the watcher does NOT auto-merge,
// even when GitHub reports the PR as CLEAN. The row stays watching so a
// human can either merge/close the PR or configure branch protection. No
// coordinator notification fires while polling — the initial invocation
// message already told them the repo is unprotected.
func TestWatcher_Unprotected_NeverAutoMerges(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-unprotected"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-unprotected")

	if _, err := d.EnqueueMerge(2420, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	mergeCalled := false
	w := &Watcher{
		db:          d,
		instanceID:  instanceID,
		sessionName: session,
		httpClient:  srv.Client(),
		repo:        "myrepo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			// gh pr ... calls go through runGH, so args[0..1] are "--repo
			// <slug>" (prepended there). gh api ... calls go through
			// runGHNoRepo (#2438: gh api rejects --repo), so those args
			// start directly with "api".
			switch {
			case len(args) >= 4 && args[2] == "pr" && args[3] == "view":
				// A repo without protection typically reports CLEAN
				// once the (nonexistent) required gates trivially pass.
				return []byte(`{"state":"OPEN","mergedAt":null,"mergeStateStatus":"CLEAN","statusCheckRollup":[],"reviewDecision":""}`), nil
			case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "branches/main/protection"):
				// gh api on an unprotected branch returns HTTP 404. gh
				// renders this as a non-zero exit with "HTTP 404" in
				// stdout+stderr — replicate that shape here.
				return []byte(`HTTP 404: Branch not protected`), fmt.Errorf("exit status 1")
			case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "rules/branches/main"):
				// #2436: the ruleset-fallback probe also 404s — genuinely
				// unprotected, neither classic protection nor any ruleset.
				return []byte(`HTTP 404: Not Found`), fmt.Errorf("exit status 1")
			case len(args) >= 4 && args[2] == "pr" && args[3] == "merge":
				mergeCalled = true
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected gh call: %v", args)
		},
	}

	w.tick(context.Background())

	if mergeCalled {
		t.Fatal("watcher invoked gh pr merge on an unprotected repo — #2420 rule violated: unprotected repos are NEVER auto-merged")
	}
	row, err := d.PendingMergeByPR(2420, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching (unprotected repo must stay watching for human action)", row.Status)
	}
	if srv.called != 0 {
		t.Errorf("notifications sent: %d — polling on an unprotected repo must be silent", srv.called)
	}
}

// TestWatcher_Unprotected_ExternalMergeStillNotifies verifies that the
// unprotected-repo silent-continuation rule does NOT suppress the terminal
// out-of-band-merge notification. The row remains in the queue while polling
// silently, then transitions to merged when the poll observes MERGED state.
func TestWatcher_Unprotected_ExternalMergeStillNotifies(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-unprot-extmerge"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-unprot-extmerge")

	if _, err := d.EnqueueMerge(2421, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	mergedAt := "2026-07-18T09:00:00Z"
	prView := fmt.Sprintf(`{"state":"MERGED","mergedAt":%q,"mergedBy":{"login":"prismatic-koi"},"mergeStateStatus":"","statusCheckRollup":[],"reviewDecision":""}`, mergedAt)

	w := &Watcher{
		db:          d,
		instanceID:  instanceID,
		sessionName: session,
		httpClient:  srv.Client(),
		repo:        "myrepo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			switch {
			case len(args) >= 4 && args[2] == "pr" && args[3] == "view":
				return []byte(prView), nil
			case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "branches/main/protection"):
				return []byte(`HTTP 404: Branch not protected`), fmt.Errorf("exit status 1")
			}
			return nil, fmt.Errorf("unexpected gh call: %v", args)
		},
	}

	w.tick(context.Background())

	row, err := d.PendingMergeByPR(2421, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "merged" {
		t.Errorf("status: got %q, want merged (out-of-band merge on an unprotected repo must still notify)", row.Status)
	}
	if srv.called == 0 {
		t.Fatal("no out-of-band-merge notification sent to coordinator")
	}
	text := extractNotifyText(t, srv.lastBody)
	if !strings.Contains(text, "PR #2421 merged out-of-band") {
		t.Errorf("notification text %q does not contain 'PR #2421 merged out-of-band'", text)
	}
	if !strings.Contains(text, "Please clean up the branch and worktree") {
		t.Errorf("notification text %q does not contain #2420 completion-discipline phrase", text)
	}
}

// TestWatcher_BLOCKED_StaysWatchingSilently verifies that during polling,
// a BLOCKED PR awaiting a human stays watching with no coordinator
// notification. This is the #2420 replacement for the pre-#2420 failAndNotify
// paths for BLOCKED-empty-decision, REVIEW_REQUIRED and CHANGES_REQUESTED —
// all of which now defer to a human via the initial-invocation message rather
// than firing terminal coordinator alerts mid-poll.
//
// #2525 carved one sub-state back out of this rule: a BLOCKED PR whose
// required checks have all concluded with at least one failure IS terminal,
// because nothing resolves it without a new push. That case moved to
// TestWatcher_BLOCKED_RequiredCheckFailed_TerminatesAndNotifies. The
// optional_check_failure case below pins the boundary — a failing check
// outside the required set is still silent.
func TestWatcher_BLOCKED_StaysWatchingSilently(t *testing.T) {
	cases := []struct {
		name string
		info *prInfo
	}{
		{
			name: "empty_review_decision_checks_passing",
			info: &prInfo{
				State:            "OPEN",
				MergeStateStatus: "BLOCKED",
				ReviewDecision:   "",
				StatusCheckRollup: []checkEntry{
					{Name: "pr-gate", Conclusion: "SUCCESS", Status: "COMPLETED"},
				},
			},
		},
		{
			name: "review_required",
			info: &prInfo{
				State:            "OPEN",
				MergeStateStatus: "BLOCKED",
				ReviewDecision:   "REVIEW_REQUIRED",
				StatusCheckRollup: []checkEntry{
					{Name: "pr-gate", Conclusion: "SUCCESS", Status: "COMPLETED"},
				},
			},
		},
		{
			name: "changes_requested",
			info: &prInfo{
				State:            "OPEN",
				MergeStateStatus: "BLOCKED",
				ReviewDecision:   "CHANGES_REQUESTED",
				StatusCheckRollup: []checkEntry{
					{Name: "pr-gate", Conclusion: "SUCCESS", Status: "COMPLETED"},
				},
			},
		},
		{
			// A failing check that branch protection does not require must
			// not trigger the #2525 transition.
			name: "optional_check_failure",
			info: &prInfo{
				State:            "OPEN",
				MergeStateStatus: "BLOCKED",
				ReviewDecision:   "REVIEW_REQUIRED",
				StatusCheckRollup: []checkEntry{
					{Name: "pr-gate", Conclusion: "SUCCESS", Status: "COMPLETED"},
					{Name: "validate-flakes", Conclusion: "FAILURE", Status: "COMPLETED"},
				},
			},
		},
		{
			name: "unrecognised_review_decision",
			info: &prInfo{
				State:            "OPEN",
				MergeStateStatus: "BLOCKED",
				ReviewDecision:   "FOOBAR",
				StatusCheckRollup: []checkEntry{
					{Name: "pr-gate", Conclusion: "SUCCESS", Status: "COMPLETED"},
				},
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			const session = "myrepo@main"
			instanceID := "inst-blocked-silent-" + tc.name

			srv := newCapturingServer(t)
			port := parsePort(t, srv.URL)
			seedCoordinator(t, d, session, instanceID, port, "sid-blocked-silent-"+tc.name)

			if _, err := d.EnqueueMerge(3400, "myrepo", session, instanceID, nil); err != nil {
				t.Fatalf("EnqueueMerge: %v", err)
			}

			fw := newFakeWatcher(d, instanceID, session, srv.Client(),
				func(_ context.Context, pr int) (*prInfo, error) { return tc.info, nil },
				func(_ context.Context, pr int) ([]byte, error) {
					t.Fatal("BLOCKED PR must not attempt to merge (#2420 silent-continuation)")
					return nil, nil
				},
				func(_ context.Context, pr int) error { return nil },
			)
			fw.fetchRequiredChecksFn = func(_ context.Context) ([]string, error) {
				return []string{"pr-gate"}, nil
			}

			fw.processHead(context.Background())

			row, err := d.PendingMergeByPR(3400, "myrepo")
			if err != nil {
				t.Fatalf("PendingMergeByPR: %v", err)
			}
			if row.Status != "watching" {
				t.Errorf("status: got %q, want watching (#2420 stays-silent-during-polling)", row.Status)
			}
			if srv.called != 0 {
				t.Errorf("notifications sent: %d — BLOCKED during polling must be silent (#2420)", srv.called)
			}
		})
	}
}

// TestWatcher_DIRTY_StaysWatchingSilently verifies that a DIRTY PR observed
// mid-poll (worker pushed a bad rebase) stays watching silently. At
// invocation time DIRTY exits with a "worker needs to rebase" message
// (handled in cmd/merge.go); mid-poll a DIRTY state is transient — the
// worker may fix it — and the poller keeps polling silently per #2420.
func TestWatcher_DIRTY_StaysWatchingSilently(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-dirty-silent"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-dirty-silent")

	if _, err := d.EnqueueMerge(3410, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
		func(_ context.Context, pr int) (*prInfo, error) {
			return &prInfo{State: "OPEN", MergeStateStatus: "DIRTY"}, nil
		},
		func(_ context.Context, pr int) ([]byte, error) {
			t.Fatal("DIRTY PR must not attempt to merge (#2420 silent-continuation)")
			return nil, nil
		},
		func(_ context.Context, pr int) error { return nil },
	)
	fw.fetchRequiredChecksFn = func(_ context.Context) ([]string, error) {
		return []string{"pr-gate"}, nil
	}

	fw.processHead(context.Background())

	row, err := d.PendingMergeByPR(3410, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status: got %q, want watching (DIRTY during polling is not terminal — #2420)", row.Status)
	}
	if srv.called != 0 {
		t.Errorf("notifications sent: %d — DIRTY during polling must be silent", srv.called)
	}
}

// TestWatcher_NewCommits_KeepsPollingSilently is the AC assertion for
// "new commits pushed → keep polling silently": we tick once against an
// UNSTABLE state with a required check in progress, then tick again against
// a CLEAN state with the check passed. Neither tick should fire an
// intermediate notification — only the final merge notification.
func TestWatcher_NewCommits_KeepsPollingSilently(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-newcommits"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-newcommits")

	if _, err := d.EnqueueMerge(3420, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	// First tick: worker just pushed, required check is in progress.
	fw := newFakeWatcher(d, instanceID, session, srv.Client(),
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
			t.Fatal("in-progress check must not trigger a merge")
			return nil, nil
		},
		func(_ context.Context, pr int) error { return nil },
	)
	fw.fetchRequiredChecksFn = func(_ context.Context) ([]string, error) {
		return []string{"pr-gate"}, nil
	}
	fw.processHead(context.Background())

	if srv.called != 0 {
		t.Errorf("intermediate notification fired during in-progress poll: %d (#2420 must be silent)", srv.called)
	}
	row, err := d.PendingMergeByPR(3420, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR after first tick: %v", err)
	}
	if row.Status != "watching" {
		t.Errorf("status after first tick: got %q, want watching", row.Status)
	}

	// Second tick: required check now green, mergeStateStatus flips to CLEAN.
	fw.fetchFn = func(_ context.Context, pr int) (*prInfo, error) {
		return &prInfo{
			State:            "OPEN",
			MergeStateStatus: "CLEAN",
			StatusCheckRollup: []checkEntry{
				{Name: "pr-gate", Status: "COMPLETED", Conclusion: "SUCCESS"},
			},
		}, nil
	}
	fw.mergeFn = func(_ context.Context, pr int) ([]byte, error) { return nil, nil }

	fw.processHead(context.Background())

	row, err = d.PendingMergeByPR(3420, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR after second tick: %v", err)
	}
	if row.Status != "merged" {
		t.Errorf("status after second tick: got %q, want merged", row.Status)
	}
	if srv.called != 1 {
		t.Errorf("total notifications: got %d, want 1 (only the merge notification, no intermediate)", srv.called)
	}
	text := extractNotifyText(t, srv.lastBody)
	if !strings.Contains(text, "Please clean up the branch and worktree") {
		t.Errorf("notification text %q does not contain #2420 completion-discipline phrase", text)
	}
}

// TestWatcher_ApprovalDetected_MergesAndNotifies is the "approval + green → merge"
// AC: a protected repo where reviewDecision transitions to APPROVED and all
// required checks are green causes the watcher to squash-merge and fire the
// prism-driven success notification (with the #2420 completion discipline).
func TestWatcher_ApprovalDetected_MergesAndNotifies(t *testing.T) {
	d := openTestDB(t)
	instanceID := "inst-approval"
	session := "myrepo@main"

	srv := newCapturingServer(t)
	port := parsePort(t, srv.URL)
	seedCoordinator(t, d, session, instanceID, port, "sid-approval")

	if _, err := d.EnqueueMerge(3430, "myrepo", session, instanceID, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}

	mergeCalled := false
	w := &Watcher{
		db:          d,
		instanceID:  instanceID,
		sessionName: session,
		httpClient:  srv.Client(),
		repo:        "myrepo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			switch {
			case len(args) >= 4 && args[2] == "pr" && args[3] == "view":
				// APPROVED + CLEAN — the positive signal for protected repos.
				return []byte(`{"state":"OPEN","mergedAt":null,"mergeStateStatus":"CLEAN","statusCheckRollup":[{"name":"pr-gate","conclusion":"SUCCESS","status":"COMPLETED"}],"reviewDecision":"APPROVED"}`), nil
			case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "branches/main/protection"):
				return []byte(`{"required_status_checks":{"contexts":[],"checks":[{"context":"pr-gate"}]}}`), nil
			case len(args) >= 4 && args[2] == "pr" && args[3] == "merge":
				mergeCalled = true
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected gh call: %v", args)
		},
	}

	w.tick(context.Background())

	if !mergeCalled {
		t.Fatal("watcher did not invoke gh pr merge on APPROVED + CLEAN protected repo")
	}
	row, err := d.PendingMergeByPR(3430, "myrepo")
	if err != nil {
		t.Fatalf("PendingMergeByPR: %v", err)
	}
	if row.Status != "merged" {
		t.Errorf("status: got %q, want merged", row.Status)
	}
	if srv.called == 0 {
		t.Fatal("no merge notification sent to coordinator")
	}
	text := extractNotifyText(t, srv.lastBody)
	if !strings.Contains(text, "PR #3430 merged") {
		t.Errorf("notification text %q does not mention PR #3430", text)
	}
	if !strings.Contains(text, "Please clean up the branch and worktree") {
		t.Errorf("notification text %q does not contain #2420 completion-discipline phrase", text)
	}
	if strings.Contains(text, "prism cleanup") {
		t.Errorf("notification text %q must NOT imply prism performed the cleanup", text)
	}
}

// TestFetchProtection_404TreatedAsUnconfigured verifies the branch-protection
// 404 handling: gh api returns HTTP 404 with "Branch not protected" when the
// endpoint has no protection configured. The fetchProtection helper treats
// this as configured=false (NOT an error), which the tick() path uses to
// route to the silent stay-watching branch instead of merging.
func TestFetchProtection_404TreatedAsUnconfigured(t *testing.T) {
	d := openTestDB(t)

	w := &Watcher{
		db:          d,
		instanceID:  "inst-404",
		sessionName: "myrepo@main",
		httpClient:  http.DefaultClient,
		repo:        "owner/bootstrap-repo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			return []byte(`HTTP 404: Branch not protected (https://api.github.com/repos/owner/bootstrap-repo/branches/main/protection)`),
				fmt.Errorf("exit status 1")
		},
	}

	state, err := w.fetchProtection(context.Background())
	if err != nil {
		t.Fatalf("fetchProtection: got error %v, want nil (404 is a state, not an error)", err)
	}
	if state.configured {
		t.Errorf("fetchProtection: configured=true, want false on 404 (#2420)")
	}
	if len(state.requiredChecks) != 0 {
		t.Errorf("fetchProtection: requiredChecks=%v, want empty on 404", state.requiredChecks)
	}
}

// TestFetchProtection_CachesUnconfigured verifies the unconfigured
// (configured=false) result is cached, so a bootstrap-repo poll does not
// hammer the API every tick.
func TestFetchProtection_CachesUnconfigured(t *testing.T) {
	d := openTestDB(t)

	var calls int
	w := &Watcher{
		db:          d,
		instanceID:  "inst-404-cache",
		sessionName: "myrepo@main",
		httpClient:  http.DefaultClient,
		repo:        "owner/bootstrap-repo",
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			calls++
			return []byte(`HTTP 404: Branch not protected`), fmt.Errorf("exit status 1")
		},
	}

	if _, err := w.fetchProtection(context.Background()); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := w.fetchProtection(context.Background()); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	// Since #2436, a classic 404 triggers one ruleset-fallback call before
	// concluding "unconfigured", so the first fetch costs 2 calls; the
	// second fetch within the TTL must still be a pure cache hit (0 more).
	if calls != 2 {
		t.Errorf("calls after two fetches within TTL: got %d, want 2 (classic+ruleset on first fetch, cached on second)", calls)
	}
}

// TestFetchProtection_ProbeCallsOmitRepoFlag is the #2438 regression test.
//
// #2437 wired the watcher's branch-protection probe through w.runGH, which
// unconditionally prepends "--repo <owner/name>" to every gh invocation
// (the #1055 CWD-independence fix for `gh pr ...`). But `gh api` REJECTS
// "--repo" outright ("unknown flag: --repo"), so every real-world probe on
// this ruleset-protected repo failed with a non-404 error, and the watcher
// stayed "watching" forever.
//
// This test captures the exact argv handed to the stubbed gh runner and
// asserts:
//   - the branch-protection probe's `gh api ...` calls carry NO "--repo"
//     flag and target the fully-qualified "repos/<owner>/<repo>/..." path
//     (both the classic and, on 404, the ruleset-fallback path); and
//   - a sibling `gh pr view` call (routed through w.runGH, not the probe)
//     still DOES carry "--repo" -- proving the fix is scoped to the `gh
//     api` call sites and does not regress #1055.
//
// Provenance: reverting the fetchProtection routing back to w.runGH makes
// this test fail because argv[0] is "--repo" instead of "api" --
// confirming the assertion is not a no-op.
func TestFetchProtection_ProbeCallsOmitRepoFlag(t *testing.T) {
	d := openTestDB(t)
	const repoSlug = "owner/nixos-config"

	var calls [][]string
	w := &Watcher{
		db:          d,
		instanceID:  "inst-2438-argv",
		sessionName: "myrepo@main",
		httpClient:  http.DefaultClient,
		repo:        repoSlug,
		runGHFunc: func(_ context.Context, args ...string) ([]byte, error) {
			snap := make([]string, len(args))
			copy(snap, args)
			calls = append(calls, snap)
			switch {
			case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "branches/main/protection"):
				// Classic endpoint 404s -- this repo is ruleset-protected,
				// not classic-protected (mirrors the live #2435 scenario).
				return []byte(`HTTP 404: Branch not protected`), fmt.Errorf("exit status 1")
			case len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "rules/branches/main"):
				return []byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-gate"}]}}]`), nil
			case len(args) >= 4 && args[2] == "pr" && args[3] == "view":
				return []byte(`{"state":"OPEN","mergedAt":null,"mergeStateStatus":"CLEAN","statusCheckRollup":[],"reviewDecision":""}`), nil
			}
			return nil, fmt.Errorf("unexpected gh call: %v", args)
		},
	}

	ctx := context.Background()

	state, err := w.fetchProtection(ctx)
	if err != nil {
		t.Fatalf("fetchProtection: %v", err)
	}
	if !state.configured {
		t.Fatal("fetchProtection: configured=false, want true (ruleset-fallback should have found the effective rule)")
	}
	if len(state.requiredChecks) != 1 || state.requiredChecks[0] != "pr-gate" {
		t.Errorf("requiredChecks: got %v, want [pr-gate]", state.requiredChecks)
	}

	if _, err := w.fetchPRInfo(ctx, 2435); err != nil {
		t.Fatalf("fetchPRInfo: %v", err)
	}

	if len(calls) != 3 {
		t.Fatalf("expected 3 gh invocations (classic api, ruleset api, pr view); got %d: %v", len(calls), calls)
	}

	// Calls 0 and 1 are the protection-probe `gh api` calls: no "--repo".
	for i, wantSubstr := range []string{"branches/main/protection", "rules/branches/main"} {
		argv := calls[i]
		if len(argv) < 2 {
			t.Fatalf("protection-probe call %d argv too short: %v", i, argv)
		}
		if argv[0] == "--repo" {
			t.Errorf("protection-probe call %d argv: got %q as argv[0], want no --repo flag (gh api rejects --repo) -- full argv: %v", i, argv[0], argv)
		}
		if argv[0] != "api" {
			t.Errorf("protection-probe call %d argv[0]: got %q, want %q", i, argv[0], "api")
		}
		if !strings.Contains(argv[1], wantSubstr) {
			t.Errorf("protection-probe call %d argv[1]: got %q, want substring %q", i, argv[1], wantSubstr)
		}
		if !strings.HasPrefix(argv[1], fmt.Sprintf("repos/%s/", repoSlug)) {
			t.Errorf("protection-probe call %d argv[1]: got %q, want fully-qualified repos/%s/... path", i, argv[1], repoSlug)
		}
	}

	// Call 2 is `gh pr view`, routed through w.runGH: --repo IS required
	// (#1055 CWD-independence).
	prViewArgv := calls[2]
	if len(prViewArgv) < 2 || prViewArgv[0] != "--repo" || prViewArgv[1] != repoSlug {
		t.Errorf("gh pr view call argv: got %v, want [--repo %q ...]", prViewArgv, repoSlug)
	}
}

// TestPollInterval_Is30Seconds pins the #2420 poll cadence: reducing from
// 45s to 30s so coordinators see merge outcomes more quickly after a human
// approves the PR.
func TestPollInterval_Is30Seconds(t *testing.T) {
	if PollInterval != 30*time.Second {
		t.Errorf("PollInterval: got %v, want 30s (#2420)", PollInterval)
	}
}
