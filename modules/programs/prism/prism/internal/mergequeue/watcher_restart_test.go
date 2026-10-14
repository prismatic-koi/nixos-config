package mergequeue

// Issue #3166: a prism restart keeps the merge queue.
//
// prism restore reads the watching rows (db.WatchingMerges) before it stops
// the old coordinator sidecar. The Shutdown of that sidecar abandons them
// (db.AbandonWatchingMerges). After restore picks the instance ID of the
// restored sidecar, it gives the rows back (db.ResumeMerges). Each test does
// these three steps in that order and then runs the production tick of the
// restored watcher.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

const restartSession = "myrepo@main"

// restartFixture is a coordinator with an enqueued PR for each number in
// prs, in that queue order.
type restartFixture struct {
	db   *db.DB
	srv  *capturingServer
	inst string
}

func newRestartFixture(t *testing.T, inst string, prs ...int) *restartFixture {
	t.Helper()
	d := openTestDB(t)
	srv := newCapturingServer(t)
	seedCoordinator(t, d, restartSession, inst, parsePort(t, srv.URL), "sid-"+inst)
	for _, pr := range prs {
		if _, err := d.EnqueueMerge(pr, "myrepo", restartSession, inst, nil); err != nil {
			t.Fatalf("EnqueueMerge(%d): %v", pr, err)
		}
		time.Sleep(time.Millisecond)
	}
	return &restartFixture{db: d, srv: srv, inst: inst}
}

// watcher returns a production watcher for the coordinator that runs with
// instanceID.
func (f *restartFixture) watcher(instanceID string, gh *ghFake) *Watcher {
	return newTickWatcher(f.db, instanceID, restartSession, f.srv.Client(), gh)
}

// queued reads the queue as restore does before it stops the old sidecar.
func (f *restartFixture) queued(t *testing.T) []db.PendingMerge {
	t.Helper()
	q, err := f.db.WatchingMerges(restartSession)
	if err != nil {
		t.Fatalf("WatchingMerges: %v", err)
	}
	return q
}

// stopOldSidecar does the merge-queue write of the old sidecar Shutdown.
func (f *restartFixture) stopOldSidecar(t *testing.T) {
	t.Helper()
	if err := f.db.AbandonWatchingMerges(f.inst); err != nil {
		t.Fatalf("AbandonWatchingMerges: %v", err)
	}
}

func (f *restartFixture) resume(t *testing.T, instanceID string, queued []db.PendingMerge) []int {
	t.Helper()
	resumed, err := f.db.ResumeMerges(restartSession, instanceID, queued)
	if err != nil {
		t.Fatalf("ResumeMerges: %v", err)
	}
	return resumed
}

func (f *restartFixture) listed(t *testing.T, instanceID, filter string) []int {
	t.Helper()
	rows, err := f.db.MergeQueueForInstance(instanceID, restartSession, filter)
	if err != nil {
		t.Fatalf("MergeQueueForInstance(%q): %v", filter, err)
	}
	prs := make([]int, 0, len(rows))
	for _, r := range rows {
		prs = append(prs, r.PR)
	}
	return prs
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRestart_ResumesQueueInOrder covers the restart that continues the
// instance. Without the resume, the rows are abandoned and the watcher
// polls nothing: that is the lost entry of #3166. `prism merges list
// --abandoned` must list them, although they carry the current instance
// ID. With the resume, the restored watcher merges each PR one time, in the
// queue order of before the restart.
func TestRestart_ResumesQueueInOrder(t *testing.T) {
	f := newRestartFixture(t, "inst-restart", 31, 32, 33)
	queued := f.queued(t)
	f.stopOldSidecar(t)

	if got := f.listed(t, f.inst, ""); len(got) != 0 {
		t.Fatalf("watching rows after the stop = %v, want none", got)
	}
	if got := f.listed(t, f.inst, "abandoned"); !equalInts(got, []int{31, 32, 33}) {
		t.Errorf("--abandoned rows for the same instance = %v, want [31 32 33]", got)
	}
	gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
	w := f.watcher(f.inst, gh)
	w.tick(context.Background())
	if len(gh.viewed) != 0 {
		t.Fatalf("watcher polled %v before the resume, want nothing", gh.viewed)
	}

	if resumed := f.resume(t, f.inst, queued); !equalInts(resumed, []int{31, 32, 33}) {
		t.Errorf("resumed = %v, want [31 32 33]", resumed)
	}
	if got := f.listed(t, f.inst, ""); !equalInts(got, []int{31, 32, 33}) {
		t.Errorf("watching rows after the resume = %v, want [31 32 33]", got)
	}
	if got := f.listed(t, f.inst, "abandoned"); len(got) != 0 {
		t.Errorf("--abandoned rows after the resume = %v, want none", got)
	}
	for _, pr := range []int{31, 32, 33} {
		row, err := f.db.PendingMergeByPR(pr, "myrepo")
		if err != nil || row == nil {
			t.Fatalf("PendingMergeByPR(%d): %v", pr, err)
		}
		if row.Error != nil || row.EndedAt != nil {
			t.Errorf("PR %d: error = %v, ended_at = %v, want both cleared", pr, row.Error, row.EndedAt)
		}
	}

	for i := 0; i < 4; i++ {
		w.tick(context.Background())
	}
	if !equalInts(gh.merged, []int{31, 32, 33}) {
		t.Errorf("gh pr merge calls = %v, want [31 32 33]", gh.merged)
	}
	if f.srv.called != 3 {
		t.Errorf("notifications = %d, want 3 (one per PR)", f.srv.called)
	}
}

// TestRestart_RowEndedBeforeStopStaysEnded covers a row that the old
// watcher ends while the old sidecar stops. The resume must not give it
// back, or the restored watcher would notify its outcome a second time.
func TestRestart_RowEndedBeforeStopStaysEnded(t *testing.T) {
	f := newRestartFixture(t, "inst-ended", 41, 42)
	queued := f.queued(t)

	infos := map[int]prInfo{
		41: {State: "OPEN", MergeStateStatus: "CLEAN"},
		42: blockedInfo("APPROVED", []checkEntry{{Name: "pr-gate", Status: "IN_PROGRESS"}}),
	}
	oldGH := newGHFake(t, prInfo{})
	oldGH.view = func(pr int) (prInfo, error) { return infos[pr], nil }
	f.watcher(f.inst, oldGH).tick(context.Background())
	f.stopOldSidecar(t)
	if f.srv.called != 1 {
		t.Fatalf("notifications from the old watcher = %d, want 1", f.srv.called)
	}

	if resumed := f.resume(t, f.inst, queued); !equalInts(resumed, []int{42}) {
		t.Errorf("resumed = %v, want [42]", resumed)
	}
	if row, _ := f.db.PendingMergeByPR(41, "myrepo"); row == nil || row.Status != "merged" {
		t.Errorf("PR 41 row = %+v, want merged", row)
	}

	newGH := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
	w := f.watcher(f.inst, newGH)
	w.tick(context.Background())
	w.tick(context.Background())
	if !equalInts(newGH.viewed, []int{42}) || !equalInts(newGH.merged, []int{42}) {
		t.Errorf("restored watcher viewed %v and merged %v, want [42] and [42]", newGH.viewed, newGH.merged)
	}
	if f.srv.called != 2 {
		t.Errorf("notifications = %d, want 2 (one per PR)", f.srv.called)
	}
}

// TestRestart_MergedButNotRecordedNotifiesOnce covers a merge that the old
// watcher sent to GitHub but did not record before the old sidecar
// stopped. The restored watcher must see MERGED, notify one time, and not
// merge again.
func TestRestart_MergedButNotRecordedNotifiesOnce(t *testing.T) {
	f := newRestartFixture(t, "inst-unrecorded", 51)
	queued := f.queued(t)
	f.stopOldSidecar(t)
	f.resume(t, f.inst, queued)

	mergedAt := "2026-10-13T08:11:04Z"
	gh := newGHFake(t, prInfo{State: "MERGED", MergedAt: &mergedAt})
	w := f.watcher(f.inst, gh)
	w.tick(context.Background())
	w.tick(context.Background())

	if len(gh.merged) != 0 {
		t.Errorf("gh pr merge calls = %v, want none", gh.merged)
	}
	if f.srv.called != 1 {
		t.Errorf("notifications = %d, want 1", f.srv.called)
	}
	if row, _ := f.db.PendingMergeByPR(51, "myrepo"); row == nil || row.Status != "merged" {
		t.Errorf("PR 51 row = %+v, want merged", row)
	}
}

// TestRestart_NewIncarnationOwnsQueue covers a restore that starts a new
// instance because the old sidecar did not stop in time. The rows move to
// the new instance, so `prism merges` lists them and the next stop of the
// new sidecar abandons them.
func TestRestart_NewIncarnationOwnsQueue(t *testing.T) {
	f := newRestartFixture(t, "inst-old", 61, 62)
	queued := f.queued(t)
	f.stopOldSidecar(t)

	const newInst = "inst-new"
	f.resume(t, newInst, queued)

	if got := f.listed(t, newInst, ""); !equalInts(got, []int{61, 62}) {
		t.Errorf("watching rows of the new instance = %v, want [61 62]", got)
	}
	gh := newGHFake(t, blockedInfo("APPROVED", []checkEntry{{Name: "pr-gate", Status: "IN_PROGRESS"}}))
	newTickWatcher(f.db, newInst, restartSession, http.DefaultClient, gh).tick(context.Background())
	if !equalInts(gh.viewed, []int{61}) {
		t.Errorf("new watcher viewed %v, want [61]", gh.viewed)
	}
	if err := f.db.AbandonWatchingMerges(newInst); err != nil {
		t.Fatalf("AbandonWatchingMerges(new): %v", err)
	}
	if got := f.listed(t, newInst, ""); len(got) != 0 {
		t.Errorf("watching rows after the new sidecar stops = %v, want none", got)
	}
}

// TestRestart_StillWatchingRowChangesOwner covers an old sidecar that got
// SIGKILL before its Shutdown abandoned the rows. Restore starts a new
// instance, and the rows that are still watching move to it.
func TestRestart_StillWatchingRowChangesOwner(t *testing.T) {
	f := newRestartFixture(t, "inst-killed", 81, 82)
	queued := f.queued(t)

	const newInst = "inst-after-kill"
	if resumed := f.resume(t, newInst, queued); !equalInts(resumed, []int{81, 82}) {
		t.Errorf("resumed = %v, want [81 82]", resumed)
	}
	if got := f.listed(t, newInst, ""); !equalInts(got, []int{81, 82}) {
		t.Errorf("watching rows of the new instance = %v, want [81 82]", got)
	}
	if got := f.listed(t, f.inst, ""); len(got) != 0 {
		t.Errorf("watching rows of the old instance = %v, want none", got)
	}
}

// TestRestart_ResumeLeavesOtherRowsAlone checks that ResumeMerges does not
// give back a row that the coordinator cancelled, or a row of another
// coordinator session.
func TestRestart_ResumeLeavesOtherRowsAlone(t *testing.T) {
	f := newRestartFixture(t, "inst-scope", 71, 72)
	other, err := f.db.EnqueueMerge(73, "other", "other@main", "inst-other", nil)
	if err != nil {
		t.Fatalf("EnqueueMerge(other): %v", err)
	}
	queued := append(f.queued(t), *other)
	if _, err := f.db.CancelMerge(72, "myrepo", f.inst); err != nil {
		t.Fatalf("CancelMerge: %v", err)
	}
	f.stopOldSidecar(t)
	if err := f.db.AbandonWatchingMerges("inst-other"); err != nil {
		t.Fatalf("AbandonWatchingMerges(other): %v", err)
	}

	if resumed := f.resume(t, f.inst, queued); !equalInts(resumed, []int{71}) {
		t.Errorf("resumed = %v, want [71]", resumed)
	}
	if row, _ := f.db.PendingMergeByPR(72, "myrepo"); row == nil || row.Status != "cancelled" {
		t.Errorf("PR 72 row = %+v, want cancelled", row)
	}
	if row, _ := f.db.PendingMergeByPR(73, "other"); row == nil || row.Status != "abandoned" {
		t.Errorf("other session row = %+v, want abandoned", row)
	}
}
