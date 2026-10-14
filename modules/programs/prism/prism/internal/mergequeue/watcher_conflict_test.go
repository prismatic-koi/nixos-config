package mergequeue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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

// prTickGH answers gh calls per PR number for the production tick. It records
// `pr view` and `pr update-branch` calls by PR and fails on any merge.
type prTickGH struct {
	t        *testing.T
	infos    map[int]prInfo
	viewed   []int
	updated  []int
	required []string
}

func (g *prTickGH) run(_ context.Context, args ...string) ([]byte, error) {
	g.t.Helper()
	if len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "branches/main/protection") {
		return []byte(`{"required_status_checks":{"contexts":["pr-gate"]}}`), nil
	}
	if len(args) >= 5 && args[2] == "pr" {
		pr, err := strconv.Atoi(args[4])
		if err != nil {
			g.t.Fatalf("pr number not at argv[4]: %v", args)
		}
		switch args[3] {
		case "view":
			g.viewed = append(g.viewed, pr)
			body, _ := json.Marshal(g.infos[pr])
			return body, nil
		case "update-branch":
			g.updated = append(g.updated, pr)
			return nil, nil
		case "merge":
			g.t.Errorf("unexpected merge: %v", args)
			return nil, fmt.Errorf("no merge")
		}
	}
	g.t.Fatalf("unexpected gh call: %v", args)
	return nil, nil
}

// BEHIND keeps its behaviour through the production tick: update-branch, still watching.
func TestWatcher_Tick_BEHIND_UpdatesBranchAndStaysWatching(t *testing.T) {
	f := newBlockedTickFixture(t, 3140, []string{"pr-gate"}, prInfo{})
	g := &prTickGH{t: t, infos: map[int]prInfo{
		3140: {State: "OPEN", MergeStateStatus: "BEHIND", Mergeable: "MERGEABLE"},
	}}
	f.watcher.runGHFunc = g.run

	f.watcher.tick(context.Background())

	if len(g.updated) != 1 || g.updated[0] != 3140 {
		t.Errorf("update-branch calls = %v, want [3140]", g.updated)
	}
	if got := f.status(t); got != "watching" {
		t.Errorf("status = %q, want watching", got)
	}
	if f.srv.called != 0 {
		t.Errorf("notifications = %d, want 0", f.srv.called)
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
	g := &prTickGH{t: t, infos: map[int]prInfo{
		3150: {State: "OPEN", MergeStateStatus: "DIRTY", Mergeable: "CONFLICTING"},
		3151: {State: "OPEN", MergeStateStatus: "BLOCKED", Mergeable: "MERGEABLE"},
	}}
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
}
