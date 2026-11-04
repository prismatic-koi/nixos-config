package cmd

// Tests for #3400: prism restore ends the row of a session whose worktree
// is missing or not recorded. On Darwin, a process that the session
// detached can outlive its tmux session (#3375). So restore ends each
// process in the sandbox of the session before the row ends, and keeps the
// row when such a process still runs. The tests replace the sandbox stop,
// except the test of the production stop off a sandbox-exec session.

import (
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// noWorktreeCase is a row that restore cannot start. reason is in the
// stderr line of restore.
type noWorktreeCase struct{ desc, worktree, reason string }

func noWorktreeCases(t *testing.T) []noWorktreeCase {
	return []noWorktreeCase{
		{"no worktree recorded", "", "no worktree recorded"},
		{"worktree missing", filepath.Join(t.TempDir(), "gone"), "not accessible"},
	}
}

// runRestoreProjectSession runs restoreProjectSession for the row of
// sessionName in the DB at dbFile, with worktree in place of its worktree.
// It returns the result and the stderr of the call.
func runRestoreProjectSession(t *testing.T, dbFile, sessionName, worktree string) (restoreOutcome, error, string) {
	t.Helper()
	st := readStatus(t, dbFile, sessionName)
	st.Worktree = worktree
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	var outcome restoreOutcome
	var restoreErr error
	stderr := captureStderrDuringFn(t, func() {
		pending := false
		outcome, restoreErr = restoreProjectSession(d, *st, &pending, 0)
	})
	return outcome, restoreErr, stderr
}

// AC: for a session whose worktree is missing or not recorded, restore ends
// the processes in the sandbox of the session before the row ends, and
// before the stop of the review children. The fake stop checks that the row
// is still open.
func TestRestoreNoWorktree_StopsSandboxProcessesBeforeRowEnds_3400(t *testing.T) {
	for _, tc := range noWorktreeCases(t) {
		t.Run(tc.desc, func(t *testing.T) {
			const session = "myrepo@restore-detached"
			dbFile := seedHookRows(t, session)
			var order []string
			calls := withSandboxStops(t, dbFile, "", nil)
			prevStop := stopSessionSandboxProcs
			stopSessionSandboxProcs = func(s string) error { order = append(order, "sandbox"); return prevStop(s) }
			withChildStops(t, dbFile)
			prevChildren := stopReviewChildrenForEnd
			stopReviewChildrenForEnd = func(d *db.DB, parent string) review.KeptChildren {
				order = append(order, "children")
				return prevChildren(d, parent)
			}

			outcome, err, stderr := runRestoreProjectSession(t, dbFile, session, tc.worktree)

			if err != nil || outcome != restoreOutcomeSkipped {
				t.Fatalf("restoreProjectSession = %v, %v, want skipped, nil", outcome, err)
			}
			if !slices.Equal(*calls, []string{session}) {
				t.Errorf("sandbox stops = %v, want [%s]", *calls, session)
			}
			if !slices.Equal(order, []string{"sandbox", "children"}) {
				t.Errorf("order = %v, want the sandbox stop, then the review children", order)
			}
			if readStatus(t, dbFile, session).EndedAt == nil {
				t.Error("row open, want ended")
			}
			if !strings.Contains(stderr, tc.reason) || !strings.Contains(stderr, "marking ended") {
				t.Errorf("stderr does not give the reason %q and say that restore ends the row:\n%s", tc.reason, stderr)
			}
		})
	}
}

// AC: when a process in the sandbox still runs, restore keeps the row, the
// port, and the review children. The error names the process and the
// session.
func TestRestoreNoWorktree_SandboxProcessStillRunsKeepsRow_3400(t *testing.T) {
	for _, tc := range noWorktreeCases(t) {
		t.Run(tc.desc, func(t *testing.T) {
			const session = "myrepo@restore-detached-runs"
			child := session + "~review-1-review-goal"
			dbFile := seedHookRows(t, session, child)
			calls := withSandboxStops(t, dbFile, "", stillRuns(session))
			parents := withChildStops(t, dbFile)

			outcome, err, stderr := runRestoreProjectSession(t, dbFile, session, tc.worktree)

			if outcome != restoreOutcomeSkipped {
				t.Errorf("outcome = %v, want skipped", outcome)
			}
			var se *prismSession.SandboxProcessError
			if !errors.As(err, &se) {
				t.Errorf("error = %v, want the *SandboxProcessError of the stop", err)
			}
			if !errors.Is(err, errSandboxProcessRuns) {
				t.Errorf("error = %v, want errSandboxProcessRuns", err)
			}
			if err != nil && (!strings.Contains(err.Error(), "4243") || !strings.Contains(err.Error(), strconv.Quote(session))) {
				t.Errorf("error %q does not name the process 4243 and the session %q", err, session)
			}
			if !slices.Equal(*calls, []string{session}) {
				t.Errorf("sandbox stops = %v, want [%s]", *calls, session)
			}
			if len(*parents) != 0 {
				t.Errorf("review children stopped for %v, want none: the row of the parent stays open", *parents)
			}
			for _, name := range []string{session, child} {
				if st := readStatus(t, dbFile, name); st.EndedAt != nil || st.HarnessPort == nil {
					t.Errorf("%s: ended_at=%v port=%v, want an open row with its port", name, st.EndedAt, st.HarnessPort)
				}
			}
			if strings.Contains(stderr, "marking ended") {
				t.Errorf("stderr says that restore ends the row, but the row stays open:\n%s", stderr)
			}
		})
	}
}

// AC: prism restore writes the error of a kept row on stderr, with the
// session name, the process, and the session in the sandbox. It counts the
// row as failed and continues, as for a session that it refuses for its
// old sidecar (#3303).
func TestRestore_SandboxProcessStillRunsIsReportedAndCountedFailed_3400(t *testing.T) {
	d := startRestoreServer(t)
	SetTestDBPath(d.Path())
	t.Cleanup(func() { SetTestDBPath("") })
	withRestoreConfig(t, config.Config{PIExtensionDir: "/test/prism-pi-extension", RestoreStaggerDelayMs: -1})

	const session = "testrepo@restore-detached-3400"
	seedStatus(t, d, session, filepath.Join(t.TempDir(), "gone"), nil)
	calls := withSandboxStops(t, d.Path(), "", stillRuns(session))

	var restoreErr error
	stdout, stderr := captureStdoutStderr(t, func() { restoreErr = Restore(false) })
	if restoreErr != nil {
		t.Fatalf("Restore: %v, want no error: a kept row must not fail the restore of the other sessions", restoreErr)
	}

	if !slices.Equal(*calls, []string{session}) {
		t.Errorf("sandbox stops = %v, want [%s]", *calls, session)
	}
	if want := "restore complete: 0 restored, 0 skipped, 1 failed"; !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	for _, want := range []string{
		"restore " + strconv.Quote(session) + ": ",
		"process 4243 in the sandbox of session " + strconv.Quote(session),
		"prism restore kept the row of " + strconv.Quote(session),
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	}
	if readStatus(t, d.Path(), session).EndedAt != nil {
		t.Error("row ended, want open")
	}
}

// AC: restore of a session with a recorded worktree continues the session,
// as before. It does not end the processes in the sandbox of the session.
func TestRestoreRecordedWorktree_DoesNotStopSandboxProcesses_3400(t *testing.T) {
	d := startRestoreServer(t)
	const session = "testrepo@restore-continues-3400"
	status := seedStatus(t, d, session, t.TempDir(), nil)
	calls := withSandboxStops(t, d.Path(), "", stillRuns(session))
	t.Cleanup(func() { prismSession.KillSidecar(session) })

	if err := callRestoreSession(d, status); err != nil {
		t.Fatalf("restoreSession: %v", err)
	}

	if len(*calls) != 0 {
		t.Errorf("sandbox stops = %v, want none: restore continues the session", *calls)
	}
	if !prismSession.HasExactSession(session) {
		t.Errorf("tmux session %q is missing, want it restored", session)
	}
	if isEnded(t, d, session) {
		t.Error("row ended, want open")
	}
}

// AC: for a bwrap or host-mode row, restore ends the row as before. The
// test runs the production stop. Off Darwin it does nothing. On Darwin, no
// process carries the sandbox of the session, so the stop finds none.
func TestRestoreNoWorktree_ProductionStopEndsBwrapAndHostRows_3400(t *testing.T) {
	for _, mode := range []config.IsolationMode{config.IsolationBwrap, config.IsolationHost} {
		for _, tc := range noWorktreeCases(t) {
			t.Run(string(mode)+"/"+tc.desc, func(t *testing.T) {
				session := "myrepo@restore-" + string(mode)
				dbFile := seedHookRows(t, session)
				d, err := db.Open(dbFile)
				if err != nil {
					t.Fatalf("open db: %v", err)
				}
				if err := d.SetIsolationMode(session, string(mode)); err != nil {
					t.Fatalf("SetIsolationMode: %v", err)
				}
				d.Close()

				outcome, err, _ := runRestoreProjectSession(t, dbFile, session, tc.worktree)

				if err != nil || outcome != restoreOutcomeSkipped {
					t.Fatalf("restoreProjectSession = %v, %v, want skipped, nil", outcome, err)
				}
				if readStatus(t, dbFile, session).EndedAt == nil {
					t.Error("row open, want ended")
				}
			})
		}
	}
}
