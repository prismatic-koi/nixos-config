package cmd

// Tests for issue #3273: the stale-sidecar path of prism switch does not
// start a second merge-queue watcher, and does not lose the queue when the
// old sidecar is slow to exit. The tmux-session-end hook keeps the ended_at
// of a sidecar stop, so MergesAbandonedByStop still finds its rows.

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// shortenSwitchStop sets the SIGTERM wait, and the SIGKILL wait, of the
// stale-sidecar path to d.
func shortenSwitchStop(t *testing.T, d time.Duration) {
	t.Helper()
	prev := switchSidecarStopTimeout
	switchSidecarStopTimeout = d
	t.Cleanup(func() { switchSidecarStopTimeout = prev })
}

// TestSwitchStaleSidecar_SlowExit_ResumesQueue stops an old coordinator
// sidecar that does not exit inside the SIGTERM wait. Its Shutdown abandons
// the queue at once and then takes longer than the wait. The stop must
// send SIGKILL, and the new instance must own the queue, in the same
// order. The output must not say that the old sidecar can still watch it.
func TestSwitchStaleSidecar_SlowExit_ResumesQueue(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	shortenSwitchStop(t, 300*time.Millisecond)
	const sessionName = "testrepo@switch-slow-3273"
	d := openRestoreTestDB(t)
	_, oldIID := seedCoordinatorQueue(t, d, sessionName, 42, 41)
	st := currentStatusRow(t, d, sessionName)
	stubDone := startSlowStopStub(t, d, sessionName, oldIID, "1s")

	var queued []db.PendingMerge
	var stopErr error
	_, stopStderr := captureStdoutStderr(t, func() { queued, stopErr = stopStaleSidecar(d, sessionName, st) })
	if stopErr != nil {
		t.Fatalf("stopStaleSidecar: %v, want no error after SIGKILL ended the old sidecar", stopErr)
	}
	select {
	case ws := <-stubDone:
		if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Errorf("old sidecar wait status = %v, want killed by SIGKILL", ws)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old sidecar is still running after the stop")
	}
	if rows, _ := d.WatchingMerges(sessionName); len(rows) != 0 {
		t.Fatalf("watching rows after the stop = %d, want none: the stub abandons them", len(rows))
	}

	newIID := startNewSwitchIncarnation(t, d, sessionName)
	stdout, resumeStderr := captureStdoutStderr(t, func() { resumeStaleMergeQueue(d, sessionName, queued) })

	assertQueue(t, d, sessionName, newIID, 42, 41)
	if want := "merge queue resumed: PR #42, PR #41"; !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	for _, unwanted := range []string{"can still run", "not resumed"} {
		if out := stopStderr + resumeStderr; strings.Contains(out, unwanted) {
			t.Errorf("stderr = %q, must not contain %q: the old sidecar exited", out, unwanted)
		}
	}
}

// TestSwitchStaleSidecar_RefusedThenOldSidecarExits covers an old sidecar
// that the stop cannot prove gone, and that exits after the switch. The
// switch refuses, so the rows stay with the old process. Its Shutdown then
// abandons them. The next switch must find them and resume them.
func TestSwitchStaleSidecar_RefusedThenOldSidecarExits(t *testing.T) {
	const sessionName = "testrepo@switch-refused-3273"
	d := openRestoreTestDB(t)
	_, oldIID := seedCoordinatorQueue(t, d, sessionName, 52, 51)

	stubSwitchStop(t, false)
	if _, err := stopStaleSidecar(d, sessionName, currentStatusRow(t, d, sessionName)); err == nil {
		t.Fatal("stopStaleSidecar: no error, want an error without proof that the old sidecar is gone")
	}
	assertQueue(t, d, sessionName, oldIID, 52, 51)

	shutDownSidecar(t, d, oldIID)

	stubSwitchStop(t, true)
	queued, err := stopStaleSidecar(d, sessionName, currentStatusRow(t, d, sessionName))
	if err != nil {
		t.Fatalf("stopStaleSidecar: %v", err)
	}
	newIID := startNewSwitchIncarnation(t, d, sessionName)
	_, _ = captureStdoutStderr(t, func() { resumeStaleMergeQueue(d, sessionName, queued) })
	assertQueue(t, d, sessionName, newIID, 52, 51)
}

// staleSwitchFixture is a stale coordinator session in a test tmux server:
// the tmux session exists, its agent_status row is quiet, no socket of the
// session answers, and its queue is watching.
type staleSwitchFixture struct {
	srv         *cmdTestServer
	d           *db.DB
	dir         string
	sessionName string
	iid         string
	tmuxID      string
}

func newStaleSwitchFixture(t *testing.T, prs ...int) staleSwitchFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stubNvimOnPath(t)
	srv := newCmdTestServer(t)
	withCmdServer(t, srv)
	d := openRestoreTestDB(t)
	SetTestDBPath(d.Path())
	t.Cleanup(func() { SetTestDBPath("") })

	dir := filepath.Join(t.TempDir(), "switch-stale-3273")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f := staleSwitchFixture{srv: srv, d: d, dir: dir, sessionName: mustNameFor(t, dir, "")}
	if err := tmux.NewSessionDetached(f.sessionName, dir); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	f.tmuxID = f.sessionID(t)
	seedStatus(t, d, f.sessionName, dir, nil)
	f.iid = uuid.New().String()
	seedIncarnation(t, d, f.sessionName, f.iid, "tier-heavy", time.Time{})
	enqueueCoordinatorPRs(t, d, f.sessionName, f.iid, prs...)
	var one int
	if err := d.QueryRow(`UPDATE agent_status SET last_seen = ? WHERE session_name = ? RETURNING 1`,
		time.Now().Add(-5*time.Minute).UnixMilli(), f.sessionName).Scan(&one); err != nil {
		t.Fatalf("make last_seen stale: %v", err)
	}
	return f
}

// sessionID returns the tmux session_id of the session. A recreated
// session gets a new ID.
func (f staleSwitchFixture) sessionID(t *testing.T) string {
	t.Helper()
	out, err := f.srv.output("list-sessions", "-F", "#{session_name} #{session_id}")
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if name, id, ok := strings.Cut(line, " "); ok && name == f.sessionName && id != "" {
			return id
		}
	}
	t.Fatalf("no tmux session %q in %q", f.sessionName, out)
	return ""
}

func switchTestOpts() session.Opts {
	return session.Opts{
		Headless:       true,
		IsolationMode:  "host",
		HarnessName:    "pi",
		PIExtensionDir: "/test/prism-pi-extension",
	}
}

// TestEnsureAndSwitch_StaleSidecarNotGone_KeepsSession drives the
// stale-sidecar path of ensureAndSwitch when the stop cannot prove that the
// old sidecar is gone. The switch must return an error before it starts a
// sidecar: the tmux session is not recreated, no sidecar PID file is
// written, and the queue stays with the old instance.
func TestEnsureAndSwitch_StaleSidecarNotGone_KeepsSession(t *testing.T) {
	f := newStaleSwitchFixture(t, 62, 61)
	stubSwitchStop(t, false)

	err := ensureAndSwitch(f.dir, "", switchTestOpts())
	if err == nil {
		t.Fatal("ensureAndSwitch: no error, want an error without proof that the old sidecar is gone")
	}
	if want := "merge queue is not resumed: PR #62, PR #61"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want %q", err, want)
	}
	if got := f.sessionID(t); got != f.tmuxID {
		t.Errorf("tmux session_id = %s, want %s: the session must not be recreated", got, f.tmuxID)
	}
	pidPath, pidErr := session.SidecarPIDPath(f.sessionName)
	if pidErr != nil {
		t.Fatalf("SidecarPIDPath: %v", pidErr)
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("sidecar PID file %s: %v, want absent: no sidecar may start", pidPath, statErr)
	}
	assertQueue(t, f.d, f.sessionName, f.iid, 62, 61)
}

// TestEnsureAndSwitch_StaleSidecarGone_ResumesQueue drives the stale-sidecar
// path of ensureAndSwitch when the old sidecar exits on the stop. The
// session is recreated, and the queue that the Shutdown abandoned is
// watching again.
func TestEnsureAndSwitch_StaleSidecarGone_ResumesQueue(t *testing.T) {
	f := newStaleSwitchFixture(t, 72, 71)
	prev := switchStopSidecar
	switchStopSidecar = func(string, time.Duration) error {
		shutDownSidecar(t, f.d, f.iid)
		return nil
	}
	t.Cleanup(func() { switchStopSidecar = prev })

	var err error
	stdout, stderr := captureStdoutStderr(t, func() { err = ensureAndSwitch(f.dir, "", switchTestOpts()) })
	if err != nil {
		t.Fatalf("ensureAndSwitch: %v (stderr %q)", err, stderr)
	}
	if got := f.sessionID(t); got == f.tmuxID {
		t.Errorf("tmux session_id = %s, want a new session", got)
	}
	if want := "merge queue resumed: PR #72, PR #71"; !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want %q (stderr %q)", stdout, want, stderr)
	}
	assertQueue(t, f.d, f.sessionName, currentInstanceID(t, f.d, f.sessionName), 72, 71)
}

// TestEventTmuxSessionEnd_KeepsEndedAtOfStop_3273 runs the tmux-session-end
// hook after a sidecar stop that kept the session ended its sessions row.
// The hook must keep that ended_at, so MergesAbandonedByStop still finds
// the rows of the stop. A row that no stop ended must still end, with
// end_state finished.
func TestEventTmuxSessionEnd_KeepsEndedAtOfStop_3273(t *testing.T) {
	for _, tc := range []struct {
		desc    string
		stopped bool
	}{
		{"sessions row ended by a stop", true},
		{"sessions row open", false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			const sessionName = "myrepo@main"
			dbFile := seedHookRows(t, sessionName)
			d, err := db.Open(dbFile)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			t.Cleanup(func() { d.Close() })
			iid := uuid.New().String()
			if err := d.SetInstanceID(sessionName, iid); err != nil {
				t.Fatalf("SetInstanceID: %v", err)
			}
			if err := d.InsertSession(db.Session{
				InstanceID: iid, SessionName: sessionName, Repo: "myrepo", Worktree: "/code/myrepo/main", Harness: "pi",
			}); err != nil {
				t.Fatalf("InsertSession: %v", err)
			}
			for _, pr := range []int{82, 81} {
				if _, err := d.EnqueueMerge(pr, "myrepo", sessionName, iid, nil); err != nil {
					t.Fatalf("EnqueueMerge(%d): %v", pr, err)
				}
			}
			if tc.stopped {
				if err := d.EndIncarnation(iid, "finished"); err != nil {
					t.Fatalf("EndIncarnation: %v", err)
				}
			}
			before := sessionRow(t, d, iid)
			// A later write must get a later millisecond.
			time.Sleep(5 * time.Millisecond)
			withHookStops(t, func(string) bool { return true }, func(*db.DB, string) []string { return nil })

			runTmuxSessionEnd(t, sessionName)

			after := sessionRow(t, d, iid)
			if after.EndedAt == nil || after.EndState == nil || *after.EndState != "finished" {
				t.Fatalf("sessions row after the hook: ended_at=%v end_state=%v, want ended, finished", after.EndedAt, after.EndState)
			}
			if !tc.stopped {
				return
			}
			if !after.EndedAt.Equal(*before.EndedAt) {
				t.Errorf("sessions.ended_at = %v, want the ended_at of the stop %v", after.EndedAt, before.EndedAt)
			}
			rows, err := d.MergesAbandonedByStop(sessionName, iid)
			if err != nil {
				t.Fatalf("MergesAbandonedByStop: %v", err)
			}
			if len(rows) != 2 || rows[0].PR != 82 || rows[1].PR != 81 {
				t.Errorf("MergesAbandonedByStop = %v, want PR #82, PR #81", rows)
			}
		})
	}
}
