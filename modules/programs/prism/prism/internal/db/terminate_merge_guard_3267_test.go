package db_test

// Issue #3267: TerminateMerge changes only a watching row. The first
// terminal write decides the outcome. If a later write can change it, the
// late write of an old watcher sets a row that a stop abandoned to failed,
// and restore then does not resume the row.

import (
	"reflect"
	"testing"
)

func TestTerminateMerge_TerminalRowDoesNotChange(t *testing.T) {
	terminal := []string{"merged", "failed", "cancelled", "abandoned"}

	for _, first := range terminal {
		for _, second := range terminal {
			t.Run(first+"_then_"+second, func(t *testing.T) {
				d := openTestDB(t)
				const pr, repo, inst = 3267, "repo-a", "inst-3267"
				if _, err := d.EnqueueMerge(pr, repo, "repo-a@main", inst, nil); err != nil {
					t.Fatalf("EnqueueMerge: %v", err)
				}
				changed, err := d.TerminateMerge(pr, repo, first, "first outcome")
				if err != nil || !changed {
					t.Fatalf("TerminateMerge(%s) on a watching row = %v, %v, want true, nil", first, changed, err)
				}
				before, err := d.PendingMergeByPR(pr, repo)
				if err != nil || before == nil {
					t.Fatalf("PendingMergeByPR: %v", err)
				}

				changed, err = d.TerminateMerge(pr, repo, second, "second outcome")
				if err != nil {
					t.Fatalf("TerminateMerge(%s): %v", second, err)
				}
				if changed {
					t.Errorf("TerminateMerge(%s) on a %s row reported a change", second, first)
				}
				after, err := d.PendingMergeByPR(pr, repo)
				if err != nil || after == nil {
					t.Fatalf("PendingMergeByPR: %v", err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Errorf("row changed:\n before %+v\n after  %+v", before, after)
				}
			})
		}
	}
}

func TestTerminateMerge_MissingRowReportsNoChange(t *testing.T) {
	d := openTestDB(t)
	changed, err := d.TerminateMerge(404, "repo-a", "failed", "no row")
	if err != nil || changed {
		t.Errorf("TerminateMerge on a missing row = %v, %v, want false, nil", changed, err)
	}
}
