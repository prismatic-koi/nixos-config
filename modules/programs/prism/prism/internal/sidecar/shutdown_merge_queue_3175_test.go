package sidecar

import (
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestShutdown_RestoreFindsTheAbandonedQueue checks the link between the
// Shutdown of a coordinator sidecar and the restore at login after a host
// shutdown (#3175). Shutdown abandons the watching rows and ends the
// sessions row. db.MergesAbandonedByStop must then find each abandoned row
// of that stop, in queue order. If Shutdown gives the two writes different
// time stamps, restore cannot find the rows.
func TestShutdown_RestoreFindsTheAbandonedQueue(t *testing.T) {
	sc, _ := newTestSidecar(t)
	d := sc.cfg.DB
	const iid = "inst-shutdown-3175"
	sc.setInstanceID(iid)
	if err := d.InsertSession(db.Session{
		InstanceID: iid, SessionName: sc.cfg.SessionName, Repo: sc.cfg.Repo, Worktree: sc.cfg.Worktree, Harness: "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	for _, pr := range []int{7, 5, 6} {
		if _, err := d.EnqueueMerge(pr, sc.cfg.Repo, sc.cfg.SessionName, iid, nil); err != nil {
			t.Fatalf("EnqueueMerge(%d): %v", pr, err)
		}
		time.Sleep(time.Millisecond)
	}

	sc.Shutdown()

	rows, err := d.MergesAbandonedByStop(sc.cfg.SessionName, iid)
	if err != nil {
		t.Fatalf("MergesAbandonedByStop: %v", err)
	}
	var got []int
	for _, r := range rows {
		got = append(got, r.PR)
	}
	if len(got) != 3 || got[0] != 7 || got[1] != 5 || got[2] != 6 {
		t.Errorf("rows of the stop = %v, want [7 5 6]", got)
	}
	if s, err := d.SessionByInstanceID(iid); err != nil || s == nil || s.EndedAt == nil {
		t.Errorf("sessions row after Shutdown = %+v (%v), want ended", s, err)
	}
}
