package session

// Tests for the ended-session sidecar sweep, EnsureSidecarStopped, and the
// tmux presence probe (#3160).

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

func openSweepTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// withSweepTmux makes the sweep see every tmux session as presence.
func withSweepTmux(t *testing.T, presence TmuxPresence) {
	t.Helper()
	t.Cleanup(SetSweepTmuxPresenceForTest(func(string) TmuxPresence { return presence }))
}

func seedRow(t *testing.T, d *db.DB, name string) {
	t.Helper()
	if err := d.UpsertStatus(name, "prism-test", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%q): %v", name, err)
	}
}

func endRow(t *testing.T, d *db.DB, name string) {
	t.Helper()
	// ended_at has millisecond resolution. The pause puts it after the
	// mtime of a PID file that the test wrote just before.
	time.Sleep(20 * time.Millisecond)
	if err := d.SetEnded(name); err != nil {
		t.Fatalf("SetEnded(%q): %v", name, err)
	}
}

func pidFileExists(t *testing.T, name string) bool {
	t.Helper()
	p, err := SidecarPIDPath(name)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	_, err = os.Stat(p)
	return err == nil
}

func TestSweepEndedSidecars_StopsSidecarOfEndedSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withSweepTmux(t, TmuxGone)
	d := openSweepTestDB(t)
	const name = "prism-test@sweep~review-1-review-goal"
	seedRow(t, d, name)
	_, wait := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")
	endRow(t, d, name)

	res, err := SweepEndedSidecars(d, SweepOpts{StopTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("SweepEndedSidecars: %v", err)
	}
	if ws := wait(); !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("stub wait status = %v, want killed by SIGTERM", ws)
	}
	if !slices.Equal(res.Stopped, []string{name}) {
		t.Errorf("Stopped = %v, want [%s]", res.Stopped, name)
	}
	if pidFileExists(t, name) {
		t.Error("PID file still present after the sweep stopped the sidecar")
	}
}

// The review agents of a round that still runs have open rows. The sweep
// must not signal them, and must not touch their PID files.
func TestSweepEndedSidecars_SparesLiveSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withSweepTmux(t, TmuxGone)
	d := openSweepTestDB(t)
	const name = "prism-test@sweep-live~review-1-review-code"
	seedRow(t, d, name)
	pid, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")

	res, err := SweepEndedSidecars(d, SweepOpts{StopTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("SweepEndedSidecars: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !sidecarProcessExists(pid) {
		t.Error("the sweep stopped the sidecar of a live session")
	}
	if !pidFileExists(t, name) {
		t.Error("the sweep removed the PID file of a live session")
	}
	if len(res.Stopped)+len(res.StaleRemoved)+len(res.Failed) != 0 {
		t.Errorf("result = %+v, want no action on a live session", res)
	}
}

// A PID file whose PID runs another program, or no program, is removed,
// and the PID gets no signal.
func TestSweepEndedSidecars_StalePIDFileRemovedWithoutSignal(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withSweepTmux(t, TmuxGone)
	d := openSweepTestDB(t)

	const other = "prism-test@sweep-other~review-1-review-qa"
	seedRow(t, d, other)
	otherPID, _ := startSidecarStub(t, other, []string{"not-a-sidecar"}, "PRISM_TEST_STUB_LONG=1")
	endRow(t, d, other)

	// The sidecar of another session is not the sidecar of this session.
	const wrong = "prism-test@sweep-wrong~review-1-review-qa"
	seedRow(t, d, wrong)
	wrongPID, _ := startSidecarStub(t, wrong, []string{"sidecar", "--session", "prism-test@somebody-else"}, "PRISM_TEST_STUB_LONG=1")
	endRow(t, d, wrong)

	const dead = "prism-test@sweep-dead~review-1-review-qa"
	seedRow(t, d, dead)
	_, wait := startSidecarStub(t, dead, []string{"sidecar", "--session", dead}, "PRISM_TEST_SUBPROCESS=1")
	wait()
	endRow(t, d, dead)

	res, err := SweepEndedSidecars(d, SweepOpts{StopTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("SweepEndedSidecars: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !sidecarProcessExists(otherPID) || !sidecarProcessExists(wrongPID) {
		t.Error("the sweep signalled a PID that does not run the sidecar of its session")
	}
	for _, name := range []string{other, wrong, dead} {
		if pidFileExists(t, name) {
			t.Errorf("stale PID file of %q still present", name)
		}
	}
	slices.Sort(res.StaleRemoved)
	want := []string{dead, other, wrong}
	slices.Sort(want)
	if !slices.Equal(res.StaleRemoved, want) {
		t.Errorf("StaleRemoved = %v, want %v", res.StaleRemoved, want)
	}
	if len(res.Stopped) != 0 {
		t.Errorf("Stopped = %v, want none", res.Stopped)
	}
}

// Fail closed: with no live set, the sweep sends no signal and removes no
// PID file, not even a stale one.
func TestSweepEndedSidecars_FailsClosedWhenLiveSetUnreadable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withSweepTmux(t, TmuxGone)
	d := openSweepTestDB(t)

	const ended = "prism-test@sweep-closed~review-1-review-goal"
	seedRow(t, d, ended)
	pid, _ := startSidecarStub(t, ended, []string{"sidecar", "--session", ended}, "PRISM_TEST_STUB_LONG=1")
	endRow(t, d, ended)

	const dead = "prism-test@sweep-closed-dead~review-1-review-goal"
	_, wait := startSidecarStub(t, dead, []string{"sidecar", "--session", dead}, "PRISM_TEST_SUBPROCESS=1")
	wait()

	d.Close()
	_, err := SweepEndedSidecars(d, SweepOpts{StopTimeout: 500 * time.Millisecond})
	if err == nil {
		t.Fatal("SweepEndedSidecars = nil error, want the live-set read error")
	}
	time.Sleep(100 * time.Millisecond)
	if !sidecarProcessExists(pid) {
		t.Error("the sweep signalled a sidecar without a live set")
	}
	if !pidFileExists(t, ended) || !pidFileExists(t, dead) {
		t.Error("the sweep removed a PID file without a live set")
	}
}

// A sidecar that started after the row ended belongs to a new incarnation
// whose row still carries the old ended_at.
func TestSweepEndedSidecars_SparesSidecarNewerThanEndedAt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withSweepTmux(t, TmuxGone)
	d := openSweepTestDB(t)
	const name = "prism-test@sweep-respawn~investigate-x"
	seedRow(t, d, name)
	endRow(t, d, name)
	time.Sleep(20 * time.Millisecond)
	pid, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")

	if _, err := SweepEndedSidecars(d, SweepOpts{StopTimeout: 500 * time.Millisecond}); err != nil {
		t.Fatalf("SweepEndedSidecars: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !sidecarProcessExists(pid) {
		t.Error("the sweep stopped a sidecar that started after its row ended")
	}
}

// No row: the sweep cannot prove that the session ended, so it sends no
// signal.
func TestSweepEndedSidecars_SparesSessionWithNoRow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withSweepTmux(t, TmuxGone)
	d := openSweepTestDB(t)
	const name = "prism-test@sweep-norow"
	pid, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")

	if _, err := SweepEndedSidecars(d, SweepOpts{StopTimeout: 500 * time.Millisecond}); err != nil {
		t.Fatalf("SweepEndedSidecars: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !sidecarProcessExists(pid) {
		t.Error("the sweep stopped the sidecar of a session with no row")
	}
}

func TestSweepEndedSidecars_TmuxPresence(t *testing.T) {
	cases := []struct {
		desc        string
		presence    TmuxPresence
		releaseLive bool
		wantStopped bool
	}{
		{"unknown tmux answer", TmuxPresenceUnknown, true, false},
		{"tmux session present, no release", TmuxPresent, false, false},
		{"tmux session present, released", TmuxPresent, true, true},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			withSweepTmux(t, c.presence)
			d := openSweepTestDB(t)
			const name = "prism-test@sweep-tmux~review-1-review-security"
			seedRow(t, d, name)
			pid, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")
			endRow(t, d, name)

			var killed []string
			res, err := SweepEndedSidecars(d, SweepOpts{
				StopTimeout: 2 * time.Second,
				ReleaseLive: func(st *db.Status) bool { return c.releaseLive && st.SessionName == name },
				KillTmux:    func(s string) { killed = append(killed, s) },
			})
			if err != nil {
				t.Fatalf("SweepEndedSidecars: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
			if got := !sidecarProcessExists(pid); got != c.wantStopped {
				t.Errorf("sidecar stopped = %v, want %v (result %+v)", got, c.wantStopped, res)
			}
			wantKilled := []string(nil)
			if c.wantStopped && c.presence == TmuxPresent {
				wantKilled = []string{name}
			}
			if !slices.Equal(killed, wantKilled) {
				t.Errorf("KillTmux calls = %v, want %v", killed, wantKilled)
			}
		})
	}
}

func TestSweepEndedSidecars_BusyWhenLockHeld(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	withSweepTmux(t, TmuxGone)
	d := openSweepTestDB(t)
	const name = "prism-test@sweep-busy~review-1-review-goal"
	seedRow(t, d, name)
	pid, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")
	endRow(t, d, name)

	unlock, locked, err := lockSidecarSweep(time.Second)
	if err != nil || !locked {
		t.Fatalf("lockSidecarSweep = %v, %v", locked, err)
	}
	defer unlock()

	res, err := SweepEndedSidecars(d, SweepOpts{LockWait: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("SweepEndedSidecars: %v", err)
	}
	if !res.Busy {
		t.Error("Busy = false, want true while another sweep holds the lock")
	}
	if !sidecarProcessExists(pid) {
		t.Error("a busy sweep stopped a sidecar")
	}
}

// A sidecar that exits only after SIGKILL is stopped. StopSidecar reports
// false for it (no exit after SIGTERM), EnsureSidecarStopped reports true.
func TestEnsureSidecarStopped_SIGKILLCountsAsStopped(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@ensure-stuck"
	_, wait := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_IGNORE_TERM=1")

	if !EnsureSidecarStopped(name, 200*time.Millisecond) {
		t.Fatal("EnsureSidecarStopped = false, want true after SIGKILL")
	}
	if ws := wait(); !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("stub wait status = %v, want killed by SIGKILL", ws)
	}
	assertPIDFileGone(t, name)
}

// An unreadable command line is a failed stop. The PID file stays, so that
// a later stop can find the process again.
func TestEnsureSidecarStopped_UnreadableCmdlineKeepsPIDFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@ensure-unreadable"
	pid, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")
	withFakePIDArgv(t, pid, nil, errors.New("fake read failure"))

	var stopped bool
	_ = captureStderr(t, func() { stopped = EnsureSidecarStopped(name, 200*time.Millisecond) })

	if stopped {
		t.Error("EnsureSidecarStopped = true, want false: the PID can still be the sidecar")
	}
	if !sidecarProcessExists(pid) {
		t.Error("EnsureSidecarStopped signalled a PID whose command line it cannot read")
	}
	if !pidFileExists(t, name) {
		t.Error("EnsureSidecarStopped removed the PID file of a PID that can still be the sidecar")
	}
}

// While a stop waits for the sidecar, the PID file stays. A second stop
// that runs at the same time must not read "no PID file" as "no sidecar".
func TestStopSidecar_PIDFileStaysDuringTheWait(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@stop-wait"
	_, wait := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_IGNORE_TERM=1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		StopSidecar(name, 600*time.Millisecond)
	}()
	time.Sleep(200 * time.Millisecond)
	if !pidFileExists(t, name) {
		t.Error("PID file removed while the sidecar still runs after SIGTERM")
	}
	<-done
	wait()
	assertPIDFileGone(t, name)
}

func TestRemoveOwnSidecarPIDFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	write := func(name string, pid int) {
		p, _ := SidecarPIDPath(name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("prism-test@own", os.Getpid())
	write("prism-test@newer", os.Getpid()+1)

	RemoveOwnSidecarPIDFile("prism-test@own")
	RemoveOwnSidecarPIDFile("prism-test@newer")

	if pidFileExists(t, "prism-test@own") {
		t.Error("PID file with the PID of this process still present")
	}
	if !pidFileExists(t, "prism-test@newer") {
		t.Error("PID file of a newer sidecar removed")
	}
}

func TestTmuxSessionPresence(t *testing.T) {
	cases := []struct {
		desc string
		out  string
		err  error
		want TmuxPresence
	}{
		{"listed", "a@b\nprism-test@x.y\nc", nil, TmuxPresent},
		{"prefix of a listed name is not a match", "prism-test@x.y~review-1-review-goal", nil, TmuxGone},
		{"not listed", "a@b", nil, TmuxGone},
		{"no server", "", errors.New("tmux list-sessions: exit status 1: no server running on /tmp/tmux-1000/default"), TmuxGone},
		{"no socket", "", errors.New("tmux list-sessions: exit status 1: error connecting to /tmp/tmux-1000/default (No such file or directory)"), TmuxGone},
		{"dead socket", "", errors.New("tmux list-sessions: exit status 1: error connecting to /tmp/tmux-1000/default (Connection refused)"), TmuxGone},
		{"socket permission", "", errors.New("tmux list-sessions: exit status 1: error connecting to /tmp/tmux-1000/default (Permission denied)"), TmuxPresenceUnknown},
		{"no tmux binary", "", errors.New(`tmux list-sessions: exec: "tmux": executable file not found in $PATH: `), TmuxPresenceUnknown},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			prev := listTmuxSessions
			listTmuxSessions = func() (string, error) { return c.out, c.err }
			t.Cleanup(func() { listTmuxSessions = prev })
			if got := TmuxSessionPresence("prism-test@x.y"); got != c.want {
				t.Errorf("TmuxSessionPresence = %v, want %v", got, c.want)
			}
		})
	}
}
