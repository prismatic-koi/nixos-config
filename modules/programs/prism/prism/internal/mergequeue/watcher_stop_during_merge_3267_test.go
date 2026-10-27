package mergequeue

// Issue #3267: a stop of the coordinator sidecar while `gh pr merge` runs.
//
// Shutdown cancels the context of the watcher and then abandons the
// watching rows (db.EndIncarnation). It does not wait for the watcher. The
// cancel kills the gh call, so tryMerge gets an error that does not tell
// whether the merge occurred. The old watcher must leave the row for
// restore to resume, and it must send no notification.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// cancelAwareGH answers as execGH does: a call on a cancelled context
// fails with the context error, and gh does not run.
func cancelAwareGH(gh *ghFake) func(context.Context, ...string) ([]byte, error) {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return gh.run(ctx, args...)
	}
}

// errKilled is the error of a gh call that the cancel killed.
var errKilled = errors.New("signal: killed")

func (f *restartFixture) assertRow(t *testing.T, pr int, status string) {
	t.Helper()
	row, err := f.db.PendingMergeByPR(pr, "myrepo")
	if err != nil || row == nil {
		t.Fatalf("PendingMergeByPR(%d): %v", pr, err)
	}
	if row.Status != status {
		var msg string
		if row.Error != nil {
			msg = *row.Error
		}
		t.Errorf("PR %d: status %q (error %q), want %q", pr, row.Status, msg, status)
	}
}

// restoreAndTick does the restore steps after the stop, as in #3175, and
// runs two ticks of the restored watcher with gh.
func (f *restartFixture) restoreAndTick(t *testing.T, pr int, gh *ghFake) {
	t.Helper()
	stopped := f.abandonedByStop(t)
	if got := mergePRs(stopped); !equalInts(got, []int{pr}) {
		t.Fatalf("rows of the stop = %v, want [%d]", got, pr)
	}
	if resumed := f.resume(t, f.inst, stopped); !equalInts(resumed, []int{pr}) {
		t.Fatalf("resumed = %v, want [%d]", resumed, pr)
	}
	w := f.watcher(f.inst, gh)
	w.tick(context.Background())
	w.tick(context.Background())
}

// TestStopDuringMerge_RestoreResumesRow covers the two orders of the stop.
// The abandon write of Shutdown can come before or after the old watcher
// reads the error of the killed call. In both orders the row stays
// abandoned, the old watcher sends no notification, and restore resumes
// the row.
func TestStopDuringMerge_RestoreResumesRow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		abandonFirst bool
	}{
		{"abandon before the watcher reads the error", true},
		{"abandon after the watcher returns", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const pr = 101
			f := newShutdownFixture(t, "inst-stop-3267", pr)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			oldGH := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
			oldGH.merge = func(int) ([]byte, error) {
				cancel()
				if tc.abandonFirst {
					f.shutDown(t)
				}
				return nil, errKilled
			}
			w := f.watcher(f.inst, oldGH)
			w.runGHFunc = cancelAwareGH(oldGH)
			w.tick(ctx)
			if !tc.abandonFirst {
				f.shutDown(t)
			}

			if !equalInts(oldGH.merged, []int{pr}) {
				t.Fatalf("gh pr merge calls = %v, want [%d]", oldGH.merged, pr)
			}
			f.assertRow(t, pr, "abandoned")
			if f.srv.called != 0 {
				t.Fatalf("notifications from the old watcher = %d, want 0", f.srv.called)
			}

			newGH := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
			f.restoreAndTick(t, pr, newGH)
			if !equalInts(newGH.merged, []int{pr}) {
				t.Errorf("restored watcher merged %v, want [%d]", newGH.merged, pr)
			}
			f.assertRow(t, pr, "merged")
			if f.srv.called != 1 {
				t.Errorf("notifications = %d, want 1", f.srv.called)
			}
		})
	}
}

// TestStopDuringMerge_MergedOnGitHub_NextWatcherNotifiesOnce covers a merge
// that GitHub did before the cancel killed the gh call. The old watcher
// cannot know it. The restored watcher sees MERGED, does not merge again,
// and notifies one time.
func TestStopDuringMerge_MergedOnGitHub_NextWatcherNotifiesOnce(t *testing.T) {
	const pr = 102
	f := newShutdownFixture(t, "inst-merged-3267", pr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oldGH := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
	oldGH.merge = func(int) ([]byte, error) {
		cancel()
		return nil, errKilled
	}
	oldGH.state = func(int) (string, error) { return "MERGED", nil }
	w := f.watcher(f.inst, oldGH)
	w.runGHFunc = cancelAwareGH(oldGH)
	w.tick(ctx)
	f.shutDown(t)

	f.assertRow(t, pr, "abandoned")
	if f.srv.called != 0 {
		t.Fatalf("notifications from the old watcher = %d, want 0", f.srv.called)
	}

	mergedAt := "2026-10-20T09:00:00Z"
	newGH := newGHFake(t, prInfo{State: "MERGED", MergedAt: &mergedAt})
	f.restoreAndTick(t, pr, newGH)
	if len(newGH.merged) != 0 {
		t.Errorf("restored watcher merged %v, want no merge", newGH.merged)
	}
	f.assertRow(t, pr, "merged")
	if f.srv.called != 1 {
		t.Fatalf("notifications = %d, want 1", f.srv.called)
	}
	if text := extractNotifyText(t, f.srv.lastBody); !strings.Contains(text, "PR #102 merged") {
		t.Errorf("notification = %q, want it to report PR #102 as merged", text)
	}
}

// TestMergeFailure_LiveContext_FailsAndNotifiesOnce covers a real gh pr
// merge failure with no stop. The row ends as failed, the coordinator gets
// one notification, and a later stop does not give the row back.
func TestMergeFailure_LiveContext_FailsAndNotifiesOnce(t *testing.T) {
	const pr = 103
	f := newShutdownFixture(t, "inst-fail-3267", pr)

	gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
	gh.merge = func(int) ([]byte, error) {
		return []byte("GraphQL: Required status check \"pr-gate\" is expected."), errors.New("exit status 1")
	}
	w := f.watcher(f.inst, gh)
	w.runGHFunc = cancelAwareGH(gh)
	w.tick(context.Background())
	w.tick(context.Background())

	f.assertRow(t, pr, "failed")
	if f.srv.called != 1 {
		t.Fatalf("notifications = %d, want 1", f.srv.called)
	}
	if text := extractNotifyText(t, f.srv.lastBody); !strings.Contains(text, "PR #103 merge failed: gh pr merge failed: GraphQL") {
		t.Errorf("notification = %q, want the merge failure of PR #103", text)
	}

	f.shutDown(t)
	if got := f.abandonedByStop(t); len(got) != 0 {
		t.Errorf("rows of the stop = %v, want none", mergePRs(got))
	}
}

// TestWatcher_RowEndedDuringMerge_NoNotification covers a row that another
// path ends while gh pr merge runs. The write of the watcher does not
// change the row, and the watcher sends no notification. The context stays
// live, so a notification, if the watcher sends one, gets to the test
// server.
func TestWatcher_RowEndedDuringMerge_NoNotification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		end   func(f *restartFixture, t *testing.T, pr int)
		merge func() ([]byte, error)
		want  string
	}{
		{
			name: "cancel during a merge that succeeds",
			end: func(f *restartFixture, t *testing.T, pr int) {
				if ok, err := f.db.CancelMerge(pr, "myrepo", f.inst); err != nil || !ok {
					t.Fatalf("CancelMerge = %v, %v", ok, err)
				}
			},
			merge: func() ([]byte, error) { return nil, nil },
			want:  "cancelled",
		},
		{
			name:  "stop during a merge that fails",
			end:   func(f *restartFixture, t *testing.T, _ int) { f.shutDown(t) },
			merge: func() ([]byte, error) { return []byte("GraphQL: not allowed"), errors.New("exit status 1") },
			want:  "abandoned",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const pr = 104
			f := newShutdownFixture(t, "inst-ended-3267", pr)
			gh := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "CLEAN"})
			gh.merge = func(int) ([]byte, error) {
				tc.end(f, t, pr)
				return tc.merge()
			}
			f.watcher(f.inst, gh).tick(context.Background())

			f.assertRow(t, pr, tc.want)
			if f.srv.called != 0 {
				t.Errorf("notifications = %d, want 0", f.srv.called)
			}
		})
	}
}
