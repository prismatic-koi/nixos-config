package mergequeue

// Issue #3175: the merge queue survives a host shutdown.
//
// At a host shutdown, the Shutdown of the coordinator sidecar abandons the
// watching rows and ends the sessions row of its instance, with one time
// stamp (db.EndIncarnation). The restore at login then finds no watching
// rows. It finds the rows of that stop with db.MergesAbandonedByStop, and
// gives them back with db.ResumeMerges. Each test does these steps and then
// runs the production tick of the restored watcher.

import (
	"context"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// newShutdownFixture is a restartFixture whose instance has a sessions
// row, as the instance of a running coordinator sidecar has.
func newShutdownFixture(t *testing.T, inst string, prs ...int) *restartFixture {
	t.Helper()
	f := newRestartFixture(t, inst, prs...)
	if err := f.db.InsertSession(db.Session{
		InstanceID: inst, SessionName: restartSession, Repo: "myrepo", Worktree: "/tmp/wt", Harness: "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	return f
}

// shutDown does the DB writes of the Shutdown of the sidecar of inst.
func (f *restartFixture) shutDown(t *testing.T) {
	t.Helper()
	if err := f.db.EndIncarnation(f.inst, "finished"); err != nil {
		t.Fatalf("EndIncarnation: %v", err)
	}
}

// abandonedByStop reads the queue as the restore at login does.
func (f *restartFixture) abandonedByStop(t *testing.T) []db.PendingMerge {
	t.Helper()
	rows, err := f.db.MergesAbandonedByStop(restartSession, f.inst)
	if err != nil {
		t.Fatalf("MergesAbandonedByStop: %v", err)
	}
	return rows
}

func mergePRs(rows []db.PendingMerge) []int {
	prs := make([]int, 0, len(rows))
	for _, r := range rows {
		prs = append(prs, r.PR)
	}
	return prs
}

// TestHostShutdown_RestoreResumesQueueInOrder covers the lost entries of
// #3175. After the stop, no row is watching, so the read that restore does
// before its own stop finds nothing. The rows of the stop must come back
// in the same order, and the restored watcher must merge and notify each PR
// one time.
func TestHostShutdown_RestoreResumesQueueInOrder(t *testing.T) {
	f := newShutdownFixture(t, "inst-shutdown", 13, 11, 12)
	f.shutDown(t)

	if got := f.queued(t); len(got) != 0 {
		t.Fatalf("watching rows after the shutdown = %v, want none", mergePRs(got))
	}
	stopped := f.abandonedByStop(t)
	if got := mergePRs(stopped); !equalInts(got, []int{13, 11, 12}) {
		t.Fatalf("rows of the stop = %v, want [13 11 12]", got)
	}
	if resumed := f.resume(t, f.inst, stopped); !equalInts(resumed, []int{13, 11, 12}) {
		t.Errorf("resumed = %v, want [13 11 12]", resumed)
	}

	gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
	w := f.watcher(f.inst, gh)
	for i := 0; i < 4; i++ {
		w.tick(context.Background())
	}
	if !equalInts(gh.merged, []int{13, 11, 12}) {
		t.Errorf("gh pr merge calls = %v, want [13 11 12]", gh.merged)
	}
	if f.srv.called != 3 {
		t.Errorf("notifications = %d, want 3 (one per PR)", f.srv.called)
	}
}

// TestHostShutdown_EarlierStopStaysAbandoned covers a row that an earlier
// stop of the same instance abandoned. The coordinator did not enqueue it
// again, so it must not come back at a later restore.
func TestHostShutdown_EarlierStopStaysAbandoned(t *testing.T) {
	f := newShutdownFixture(t, "inst-two-stops", 21)
	f.shutDown(t)
	if err := f.db.ReopenSession(f.inst); err != nil {
		t.Fatalf("ReopenSession: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := f.db.EnqueueMerge(22, "myrepo", restartSession, f.inst, nil); err != nil {
		t.Fatalf("EnqueueMerge(22): %v", err)
	}
	f.shutDown(t)

	stopped := f.abandonedByStop(t)
	if got := mergePRs(stopped); !equalInts(got, []int{22}) {
		t.Fatalf("rows of the last stop = %v, want [22]", got)
	}
	f.resume(t, f.inst, stopped)
	if row, _ := f.db.PendingMergeByPR(21, "myrepo"); row == nil || row.Status != "abandoned" {
		t.Errorf("PR 21 row = %+v, want abandoned", row)
	}

	gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
	w := f.watcher(f.inst, gh)
	w.tick(context.Background())
	w.tick(context.Background())
	if !equalInts(gh.viewed, []int{22}) {
		t.Errorf("restored watcher viewed %v, want [22]", gh.viewed)
	}
}

// TestHostShutdown_EndedRowsStayEnded covers a row that the old watcher
// merged and a row that the coordinator cancelled before the shutdown.
// Neither may come back, and the merged row must not be notified again.
func TestHostShutdown_EndedRowsStayEnded(t *testing.T) {
	f := newShutdownFixture(t, "inst-ended-rows", 31, 32, 33)
	if _, err := f.db.CancelMerge(33, "myrepo", f.inst); err != nil {
		t.Fatalf("CancelMerge: %v", err)
	}
	infos := map[int]prInfo{
		31: {State: "OPEN", MergeStateStatus: "CLEAN"},
		32: blockedInfo("APPROVED", []checkEntry{{Name: "pr-gate", Status: "IN_PROGRESS"}}),
	}
	oldGH := newGHFake(t, prInfo{})
	oldGH.view = func(pr int) (prInfo, error) { return infos[pr], nil }
	f.watcher(f.inst, oldGH).tick(context.Background())
	f.shutDown(t)
	if f.srv.called != 1 {
		t.Fatalf("notifications from the old watcher = %d, want 1", f.srv.called)
	}

	stopped := f.abandonedByStop(t)
	if got := mergePRs(stopped); !equalInts(got, []int{32}) {
		t.Fatalf("rows of the stop = %v, want [32]", got)
	}
	f.resume(t, f.inst, stopped)
	for pr, want := range map[int]string{31: "merged", 32: "watching", 33: "cancelled"} {
		if row, _ := f.db.PendingMergeByPR(pr, "myrepo"); row == nil || row.Status != want {
			t.Errorf("PR %d row = %+v, want %s", pr, row, want)
		}
	}

	newGH := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
	w := f.watcher(f.inst, newGH)
	w.tick(context.Background())
	w.tick(context.Background())
	if !equalInts(newGH.merged, []int{32}) {
		t.Errorf("restored watcher merged %v, want [32]", newGH.merged)
	}
	if f.srv.called != 2 {
		t.Errorf("notifications = %d, want 2 (one per merged PR)", f.srv.called)
	}
}

// TestMergesAbandonedByStop_Scope checks that only the rows of the session
// and the instance, abandoned by the stop that ended the instance, qualify.
func TestMergesAbandonedByStop_Scope(t *testing.T) {
	f := newShutdownFixture(t, "inst-scope-3175", 41)
	if _, err := f.db.EnqueueMerge(42, "myrepo", restartSession, "inst-other-3175", nil); err != nil {
		t.Fatalf("EnqueueMerge(42): %v", err)
	}
	if _, err := f.db.EnqueueMerge(43, "other", "other@main", f.inst, nil); err != nil {
		t.Fatalf("EnqueueMerge(43): %v", err)
	}
	if err := f.db.InsertSession(db.Session{
		InstanceID: "inst-other-3175", SessionName: restartSession, Repo: "myrepo", Worktree: "/tmp/wt", Harness: "pi",
	}); err != nil {
		t.Fatalf("InsertSession(other): %v", err)
	}
	if err := f.db.EndIncarnation("inst-other-3175", "finished"); err != nil {
		t.Fatalf("EndIncarnation(other): %v", err)
	}
	f.shutDown(t)

	if got := mergePRs(f.abandonedByStop(t)); !equalInts(got, []int{41}) {
		t.Errorf("rows of the stop = %v, want [41]", got)
	}
}

// TestMergesAbandonedByStop_NoRowsWithoutTheStop covers two stops that do
// not qualify: an abandon that did not end the sessions row, and a later
// stop that abandoned nothing.
func TestMergesAbandonedByStop_NoRowsWithoutTheStop(t *testing.T) {
	f := newShutdownFixture(t, "inst-no-stop-3175", 51)
	if err := f.db.AbandonWatchingMerges(f.inst); err != nil {
		t.Fatalf("AbandonWatchingMerges: %v", err)
	}
	if got := f.abandonedByStop(t); len(got) != 0 {
		t.Errorf("rows with an open sessions row = %v, want none", mergePRs(got))
	}

	time.Sleep(2 * time.Millisecond)
	f.shutDown(t)
	if got := f.abandonedByStop(t); len(got) != 0 {
		t.Errorf("rows after a stop that abandoned nothing = %v, want none", mergePRs(got))
	}
}
