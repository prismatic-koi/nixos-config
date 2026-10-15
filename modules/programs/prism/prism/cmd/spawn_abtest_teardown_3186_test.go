package cmd

// Tests for #3186: the --abtest both-or-neither teardown stops the sidecar
// first. A leg whose sidecar does not stop keeps its row, its port, its
// tmux session, and its worktree.

import (
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

type abtestSeams struct {
	order     []string
	tmuxKills []string
	removed   []string
}

func withAbtestSeams(t *testing.T, stopOK bool) *abtestSeams {
	t.Helper()
	s := &abtestSeams{}
	pStop, pKill, pRm := abtestStopSidecar, abtestKillTmux, abtestRemoveWorktree
	abtestStopSidecar = func(name string, _ time.Duration) bool {
		s.order = append(s.order, "stop")
		return stopOK
	}
	abtestKillTmux = func(name string) error {
		s.order = append(s.order, "tmux")
		s.tmuxKills = append(s.tmuxKills, name)
		return nil
	}
	abtestRemoveWorktree = func(_, path string) error {
		s.removed = append(s.removed, path)
		return nil
	}
	t.Cleanup(func() { abtestStopSidecar, abtestKillTmux, abtestRemoveWorktree = pStop, pKill, pRm })
	return s
}

func TestTeardownAbtestLeg_StopFailureKeepsResources_3186(t *testing.T) {
	const name = "myrepo@abtest-a"
	dbFile := seedHookRows(t, name)
	s := withAbtestSeams(t, false)
	withChildStops(t, dbFile)

	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	teardownAbtestLeg(d, "/bare", name, "/wt/a", true)
	d.Close()

	st := readStatus(t, dbFile, name)
	if st.EndedAt != nil || st.HarnessPort == nil {
		t.Errorf("ended_at=%v port=%v, want open row with its port", st.EndedAt, st.HarnessPort)
	}
	if len(s.tmuxKills) != 0 {
		t.Errorf("tmux killed %v, want kept", s.tmuxKills)
	}
	if len(s.removed) != 0 {
		t.Errorf("worktrees removed %v, want kept", s.removed)
	}
}

func TestTeardownAbtestLeg_StopSuccessEndsLeg_3186(t *testing.T) {
	const name = "myrepo@abtest-b"
	dbFile := seedHookRows(t, name)
	s := withAbtestSeams(t, true)
	parents := withChildStops(t, dbFile)

	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	teardownAbtestLeg(d, "/bare", name, "/wt/b", true)
	d.Close()

	if got := s.order; len(got) != 2 || got[0] != "stop" || got[1] != "tmux" {
		t.Errorf("order %v, want [stop tmux]", got)
	}
	if readStatus(t, dbFile, name).EndedAt == nil {
		t.Error("row open, want ended")
	}
	if len(*parents) != 1 {
		t.Errorf("review children stopped for %v, want one call", *parents)
	}
	if len(s.removed) != 1 || s.removed[0] != "/wt/b" {
		t.Errorf("removed %v, want [/wt/b]", s.removed)
	}
}
