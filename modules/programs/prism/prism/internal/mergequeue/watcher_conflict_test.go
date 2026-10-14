package mergequeue

import (
	"context"
	"testing"
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
