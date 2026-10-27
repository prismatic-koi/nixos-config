package mergequeue

import (
	"context"
	"testing"
	"time"
)

// Issue #3131: a queued PR that becomes conflicting ends its row and notifies once.
func TestWatcher_Conflict_TerminatesAndNotifiesOnce(t *testing.T) {
	for _, tc := range []struct{ state, mergeable string }{
		{"DIRTY", "CONFLICTING"},
		{"DIRTY", ""},
		{"BLOCKED", "CONFLICTING"},
	} {
		f := newBlockedTickFixture(t, 3131, []string{"pr-gate"}, prInfo{
			State: "OPEN", MergeStateStatus: tc.state, Mergeable: tc.mergeable,
		})
		f.watcher.tick(context.Background())
		f.watcher.tick(context.Background())
		if got := f.status(t); got != "failed" {
			t.Errorf("%v: status %q, want failed", tc, got)
		}
		if f.srv.called != 1 {
			t.Fatalf("%v: notifications %d, want 1", tc, f.srv.called)
		}
		if text := extractNotifyText(t, f.srv.lastBody); text != "PR #3131 has conflicts. Worker needs to rebase." {
			t.Errorf("%v: text %q", tc, text)
		}
		if f.gh.mergeAttempted() {
			t.Errorf("%v: merge attempted", tc)
		}
	}
}

func TestWatcher_Conflict_UnknownMergeableNotConflicting(t *testing.T) {
	f := newBlockedTickFixture(t, 3132, []string{"pr-gate"}, prInfo{
		State: "OPEN", MergeStateStatus: "UNKNOWN", Mergeable: "UNKNOWN",
	})
	f.watcher.tick(context.Background())
	if got := f.status(t); got != "watching" {
		t.Errorf("status %q, want watching", got)
	}
	if f.srv.called != 0 {
		t.Errorf("notifications %d, want 0", f.srv.called)
	}
}

// After the head conflicts, tick 1 ends its row and promotes the next PR;
// tick 2 polls the next PR.
func TestWatcher_Tick_ConflictMovesOnToNextPR(t *testing.T) {
	f := newBlockedTickFixture(t, 3150, []string{"pr-gate"}, prInfo{})
	time.Sleep(time.Millisecond)
	if _, err := f.db.EnqueueMerge(3151, f.repo, f.session, f.inst, nil); err != nil {
		t.Fatalf("EnqueueMerge: %v", err)
	}
	infos := map[int]prInfo{
		3150: {State: "OPEN", MergeStateStatus: "DIRTY", Mergeable: "CONFLICTING"},
		3151: {State: "OPEN", MergeStateStatus: "BLOCKED", Mergeable: "MERGEABLE"},
	}
	g := newGHFake(t, prInfo{})
	g.view = func(pr int) (prInfo, error) { return infos[pr], nil }
	f.watcher.runGHFunc = g.run

	f.watcher.tick(context.Background())

	if got := f.status(t); got != "failed" {
		t.Errorf("PR 3150 status = %q, want failed", got)
	}
	head, err := f.db.MergeQueueHead(f.session)
	if err != nil || head == nil || head.PR != 3151 {
		t.Fatalf("head after tick 1 = %+v (err %v), want PR 3151", head, err)
	}

	f.watcher.tick(context.Background())

	if len(g.viewed) != 2 || g.viewed[0] != 3150 || g.viewed[1] != 3151 {
		t.Errorf("viewed PRs = %v, want [3150 3151]", g.viewed)
	}
	if f.srv.called != 1 {
		t.Errorf("notifications = %d, want 1 (conflict only)", f.srv.called)
	}
	if len(g.merged) != 0 || len(g.updated) != 0 {
		t.Errorf("merge calls %v, update-branch calls %v, want none", g.merged, g.updated)
	}
}

// TestNotify_NonSocketPipeHarnessSendsNothing: a coordinator whose harness is
// not socket-pipe gets no request, and notify returns without a panic.
func TestNotify_NonSocketPipeHarnessSendsNothing(t *testing.T) {
	d := openTestDB(t)
	srv := newCapturingServer(t)
	seedCoordinator(t, d, "myrepo@main", "inst-nonpipe", srv, "sid")
	if err := d.SetHarnessRaw("myrepo@main", "legacy-http"); err != nil {
		t.Fatalf("SetHarnessRaw: %v", err)
	}
	w := &Watcher{db: d, instanceID: "inst-nonpipe", sessionName: "myrepo@main"}
	w.notify(context.Background(), "myrepo@main", "inst-nonpipe", "hello")
	if srv.called != 0 {
		t.Errorf("requests = %d, want 0", srv.called)
	}
}
