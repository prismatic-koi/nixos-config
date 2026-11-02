package cmd

// Tests for #3392: the teardown of a failed --abtest leg ends the processes
// in the sandbox of the session after the tmux kill and before the row
// ends. When the cleanup of a parent keeps a review child because a process
// in its sandbox still runs, the child keeps its row and its port, and the
// error of the cleanup names the process and the child. The tests replace
// the stops. internal/review tests the stops of the review children, and
// internal/session and internal/integration test the stop itself.

import (
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// AC: the teardown of an --abtest leg ends the processes in the sandbox of
// the session after the tmux kill and before the row ends. When a process
// still runs, the leg keeps its row, its port, and its worktree, and the
// warning names the process and the session.
func TestTeardownAbtestLeg_StopsSandboxProcessesAfterTmuxKill_3392(t *testing.T) {
	for _, procRuns := range []bool{false, true} {
		label := "no process runs"
		if procRuns {
			label = "a process still runs"
		}
		t.Run(label, func(t *testing.T) {
			const name = "myrepo@abtest-sandbox"
			dbFile := seedHookRows(t, name)
			s := withAbtestSeams(t, true)
			parents := withChildStops(t, dbFile)
			var stopErr error
			if procRuns {
				stopErr = stillRuns(name)
			}
			calls := withSandboxStops(t, dbFile, "", stopErr)
			prev := stopSessionSandboxProcs
			stopSessionSandboxProcs = func(n string) error { s.order = append(s.order, "sandbox"); return prev(n) }

			d, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			stderr := captureStderrDuringFn(t, func() { teardownAbtestLeg(d, "/bare", name, "/wt/sandbox", true) })
			d.Close()

			if !slices.Equal(*calls, []string{name}) {
				t.Errorf("sandbox stops = %v, want [%s]", *calls, name)
			}
			if !slices.Equal(s.order, []string{"stop", "tmux", "sandbox"}) {
				t.Errorf("order = %v, want [stop tmux sandbox]", s.order)
			}
			st := readStatus(t, dbFile, name)
			if !procRuns {
				if st.EndedAt == nil {
					t.Error("row open, want ended")
				}
				if len(*parents) != 1 || !slices.Equal(s.removed, []string{"/wt/sandbox"}) {
					t.Errorf("review children stopped for %v, worktrees removed %v, want one call and [/wt/sandbox]", *parents, s.removed)
				}
				return
			}
			if st.EndedAt != nil || st.HarnessPort == nil {
				t.Errorf("ended_at=%v port=%v, want an open row with its port", st.EndedAt, st.HarnessPort)
			}
			if len(*parents) != 0 {
				t.Errorf("review children stopped for %v, want none: the row of the leg stays", *parents)
			}
			if len(s.removed) != 0 {
				t.Errorf("worktrees removed %v, want kept", s.removed)
			}
			if !strings.Contains(stderr, "4243") || !strings.Contains(stderr, strconv.Quote(name)) {
				t.Errorf("stderr does not name the process 4243 and the session %q:\n%s", name, stderr)
			}
		})
	}
}

// withSandboxKeptChild replaces both review-child stops of the cleanup
// paths with a fake that ends each child of the parent except stuck, and
// keeps stuck because a process in its sandbox still runs.
func withSandboxKeptChild(t *testing.T, stuck string) {
	t.Helper()
	fake := func(d *db.DB, parent string) review.KeptChildren {
		rows, err := d.AllStatusesWithPrefix(parent + "~review-")
		if err != nil {
			t.Errorf("AllStatusesWithPrefix: %v", err)
		}
		for _, r := range rows {
			if r.SessionName != stuck {
				_ = d.ReleasePort(r.SessionName)
				_ = d.SetEnded(r.SessionName)
			}
		}
		return review.KeptChildren{SandboxRuns: []string{stuck}, SandboxErr: stillRuns(stuck)}
	}
	prevCleanup, prevEnd, prevHook := cleanupReviewChildren, stopReviewChildrenForEnd, hookStopReviewChildren
	cleanupReviewChildren = func(d *db.DB, parent, _ string) review.KeptChildren { return fake(d, parent) }
	stopReviewChildrenForEnd = fake
	hookStopReviewChildren = fake
	t.Cleanup(func() {
		cleanupReviewChildren, stopReviewChildrenForEnd, hookStopReviewChildren = prevCleanup, prevEnd, prevHook
	})
}

// AC: when the cleanup of a parent keeps a review child because a process
// in its sandbox still runs, the parent ends, the child keeps its row and
// its port, and the command exits non-zero with an error that names the
// process and the child. The error does not say that a sidecar did not
// stop.
func TestCleanupParent_SandboxKeptChildKeepsRowAndNamesProcess_3392(t *testing.T) {
	paths := []struct {
		name string
		run  func(parent string) error
	}{
		{"headlessCleanupWithJSONTo", func(p string) error {
			return headlessCleanupWithJSONTo(p, "branch", "", "", false, io.Discard)
		}},
		{"headlessCloseSessionWithJSONTo", func(p string) error { return headlessCloseSessionWithJSONTo(p, false, io.Discard) }},
		{"closeSession", closeSession},
		{"doCleanup", func(p string) error {
			m := cleanupModel{session: p, worktreeName: "branch"}
			done, ok := m.doCleanup()().(cleanupDoneMsg)
			if !ok {
				return errors.New("doCleanup did not return a cleanupDoneMsg")
			}
			return done.err
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			// A descendant name makes doCleanup skip git, so that the path
			// needs no repository.
			const parent = "myrepo@sandbox-kept-child~investigate-x"
			stuck := parent + "~review-1-review-goal"
			ended := parent + "~review-1-review-code"
			dbFile := seedHookRows(t, parent, stuck, ended)
			tmuxLog := withSpyTmux(t)
			installFakeRunner(t)
			withCleanupStops(t, dbFile, tmuxLog, func(string) bool { return true })
			withSandboxStops(t, dbFile, tmuxLog, nil)
			withSandboxKeptChild(t, stuck)

			var err error
			captureStderrDuringFn(t, func() { err = p.run(parent) })

			if !errors.Is(err, errSandboxProcessRuns) || errors.Is(err, errSidecarNotStopped) {
				t.Errorf("error = %v, want errSandboxProcessRuns and not errSidecarNotStopped", err)
			}
			if err != nil && (!strings.Contains(err.Error(), "4243") || !strings.Contains(err.Error(), strconv.Quote(stuck))) {
				t.Errorf("error %q does not name the process 4243 and the child %q", err, stuck)
			}
			if readStatus(t, dbFile, parent).EndedAt == nil {
				t.Error("parent row open, want ended")
			}
			if readStatus(t, dbFile, ended).EndedAt == nil {
				t.Error("other child open, want ended")
			}
			if st := readStatus(t, dbFile, stuck); st.EndedAt != nil || st.HarnessPort == nil {
				t.Errorf("kept child: ended_at=%v port=%v, want an open row with its port", st.EndedAt, st.HarnessPort)
			}
		})
	}
}

// The session-closed hook of a parent keeps the row of a review child where
// a process in the sandbox still runs, out of the db.SetEnded cascade.
func TestEventTmuxSessionEnd_SandboxKeptChildKeepsRow_3392(t *testing.T) {
	const parent = "myrepo@hook-sandbox-child"
	stuck := parent + "~review-1-review-qa"
	dbFile := seedHookRows(t, parent, stuck)
	withHookStops(t, func(string) bool { return true }, func(*db.DB, string) []string { return nil })
	withSandboxStops(t, dbFile, "", nil)
	withSandboxKeptChild(t, stuck)

	runTmuxSessionEnd(t, parent)

	if readStatus(t, dbFile, parent).EndedAt == nil {
		t.Error("parent row open, want ended")
	}
	if st := readStatus(t, dbFile, stuck); st.EndedAt != nil || st.HarnessPort == nil {
		t.Errorf("kept child: ended_at=%v port=%v, want an open row with its port", st.EndedAt, st.HarnessPort)
	}
}

// The error of a cleanup that kept review children names each child with
// its reason, and wraps the sentinel of each reason, so that the TUI
// cleanup returns it and the command exits non-zero.
func TestKeptChildrenError_NamesEachReason_3392(t *testing.T) {
	if err := keptChildrenError(review.KeptChildren{}); err != nil {
		t.Errorf("keptChildrenError(none) = %v, want nil", err)
	}
	const sidecar, sandbox = "myrepo@p~review-1-review-qa", "myrepo@p~review-1-review-goal"
	err := keptChildrenError(review.KeptChildren{
		SidecarRuns: []string{sidecar},
		SandboxRuns: []string{sandbox},
		SandboxErr:  stillRuns(sandbox),
	})
	if !errors.Is(err, errSidecarNotStopped) || !errors.Is(err, errSandboxProcessRuns) {
		t.Errorf("error = %v, want both errSidecarNotStopped and errSandboxProcessRuns", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "sidecar did not stop for "+strconv.Quote(sidecar)) {
		t.Errorf("error %q does not name %q as a child whose sidecar did not stop", msg, sidecar)
	}
	if strings.Contains(msg, "sidecar did not stop for "+strconv.Quote(sandbox)) {
		t.Errorf("error %q says that the sidecar of %q did not stop", msg, sandbox)
	}
	if !strings.Contains(msg, "4243") || !strings.Contains(msg, strconv.Quote(sandbox)) {
		t.Errorf("error %q does not name the process 4243 and the child %q", msg, sandbox)
	}
	w, _ := cleanupWrapper{}.Update(cleanupDoneMsg{keptChildrenError(review.KeptChildren{SandboxRuns: []string{sandbox}, SandboxErr: stillRuns(sandbox)})})
	if got := w.(cleanupWrapper).doneErr; !errors.Is(got, errSandboxProcessRuns) {
		t.Errorf("doneErr = %v, want the sandbox error", got)
	}
}
