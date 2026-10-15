package mergequeue

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const workflowScopeErr = "GraphQL: refusing to allow a Personal Access Token to create or update workflow `.github/workflows/pr-gate.yml` without `workflow` scope (updatePullRequestBranch)"

func behindFixture(t *testing.T, pr int, update func(int) error) (*blockedTickFixture, *ghFake) {
	f := newBlockedTickFixture(t, pr, []string{"pr-gate"}, prInfo{})
	g := newGHFake(t, prInfo{State: "OPEN", MergeStateStatus: "BEHIND", Mergeable: "MERGEABLE"})
	g.update = update
	f.watcher.runGHFunc = g.run
	return f, g
}

// Issue #3180: a permanent update-branch refusal ends the row, notifies once.
func TestWatcher_UpdateBranch_PermanentTerminatesAndNotifiesOnce(t *testing.T) {
	f, g := behindFixture(t, 3165, func(int) error { return errors.New(workflowScopeErr) })
	f.watcher.tick(context.Background())
	f.watcher.tick(context.Background())
	if got := f.status(t); got != "failed" {
		t.Errorf("status %q, want failed", got)
	}
	if f.srv.called != 1 || len(g.updated) != 1 {
		t.Fatalf("notifications %d, update calls %d, want 1 and 1", f.srv.called, len(g.updated))
	}
	text := extractNotifyText(t, f.srv.lastBody)
	for _, want := range []string{"PR #3165", "without `workflow` scope", "rebase", "push"} {
		if !strings.Contains(text, want) {
			t.Errorf("text %q lacks %q", text, want)
		}
	}
}

func TestWatcher_UpdateBranch_PermanentMovesOnToNextPR(t *testing.T) {
	f, g := behindFixture(t, 3165, func(int) error { return errors.New(workflowScopeErr) })
	time.Sleep(time.Millisecond)
	if _, err := f.db.EnqueueMerge(3166, f.repo, f.session, f.inst, nil); err != nil {
		t.Fatal(err)
	}
	f.watcher.tick(context.Background())
	f.watcher.tick(context.Background())
	if len(g.viewed) != 2 || g.viewed[1] != 3166 {
		t.Errorf("viewed %v, want second poll on 3166", g.viewed)
	}
}

func TestWatcher_UpdateBranch_TransientThenSuccessIsSilent(t *testing.T) {
	n := 0
	f, _ := behindFixture(t, 3167, func(int) error {
		n++
		if n < 3 {
			return errors.New("dial tcp: i/o timeout")
		}
		return nil
	})
	for i := 0; i < 4; i++ {
		f.watcher.tick(context.Background())
	}
	if got := f.status(t); got != "watching" || f.srv.called != 0 {
		t.Errorf("status %q, notifications %d, want watching and 0", got, f.srv.called)
	}
	if f.watcher.updateFailCount != 0 {
		t.Errorf("counter %d, want 0 after success", f.watcher.updateFailCount)
	}
}

func TestWatcher_UpdateBranch_UnknownRepeatedNotifiesOnce(t *testing.T) {
	f, _ := behindFixture(t, 3168, func(int) error { return errors.New("dial tcp: i/o timeout") })
	for i := 0; i < updateFailThreshold-1; i++ {
		f.watcher.tick(context.Background())
	}
	if f.srv.called != 0 {
		t.Fatalf("notified before threshold")
	}
	for i := 0; i < 3; i++ {
		f.watcher.tick(context.Background())
	}
	if f.srv.called != 1 {
		t.Errorf("notifications %d, want 1", f.srv.called)
	}
	if got := f.status(t); got != "watching" {
		t.Errorf("status %q, want watching", got)
	}
}

func TestWatcher_UpdateBranch_CounterResetsOnSuccess(t *testing.T) {
	fail := true
	f, _ := behindFixture(t, 3169, func(int) error {
		if fail {
			return errors.New("boom")
		}
		return nil
	})
	for i := 0; i < updateFailThreshold-1; i++ {
		f.watcher.tick(context.Background())
	}
	fail = false
	f.watcher.tick(context.Background())
	fail = true
	for i := 0; i < updateFailThreshold-1; i++ {
		f.watcher.tick(context.Background())
	}
	if f.srv.called != 0 {
		t.Errorf("notifications %d, want 0 (counter reset)", f.srv.called)
	}
}
