package session

import (
	"errors"
	"testing"
	"time"
)

// prism restore and prism switch resume the merge queue of a coordinator
// only when the stop proves that no process that can be the old sidecar
// runs (#3175). These tests pin the stop results that they read.

func TestStopSidecarReport_3175(t *testing.T) {
	t.Run("sidecar exits after SIGTERM", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		const name = "prism-test@report-exits"
		startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")
		if exited, gone := StopSidecarReport(name, 2*time.Second); !exited || !gone {
			t.Errorf("StopSidecarReport = (%v, %v), want (true, true)", exited, gone)
		}
	})

	t.Run("no PID file", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		if exited, gone := StopSidecarReport("prism-test@report-none", 200*time.Millisecond); !exited || !gone {
			t.Errorf("StopSidecarReport = (%v, %v), want (true, true)", exited, gone)
		}
	})

	t.Run("unreadable command line", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		const name = "prism-test@report-unreadable"
		pid, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")
		withFakePIDArgv(t, pid, nil, errors.New("fake read failure"))
		var exited, gone bool
		_ = captureStderr(t, func() { exited, gone = StopSidecarReport(name, 2*time.Second) })
		if exited || gone {
			t.Errorf("StopSidecarReport = (%v, %v), want (false, false): the PID can still be the sidecar", exited, gone)
		}
		if !sidecarProcessExists(pid) {
			t.Error("StopSidecarReport signalled a PID whose command line it cannot read")
		}
	})
}

// KillSidecarAndWait marks the error of an unreadable command line with
// ErrSidecarOwnerUnknown, and not the error of a recycled PID: only the
// first can still be the sidecar.
func TestKillSidecarAndWait_OwnerUnknown_3175(t *testing.T) {
	for _, tc := range []struct {
		desc        string
		argv        []string
		unreadable  bool
		wantUnknown bool
	}{
		{"recycled PID", []string{"not-a-sidecar"}, false, false},
		{"unreadable command line", nil, true, true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const name = "prism-test@kill-wait-owner"
			argv := tc.argv
			if argv == nil {
				argv = []string{"sidecar", "--session", name}
			}
			pid, _ := startSidecarStub(t, name, argv, "PRISM_TEST_STUB_LONG=1")
			if tc.unreadable {
				withFakePIDArgv(t, pid, nil, errors.New("fake read failure"))
			}
			var err error
			_ = captureStderr(t, func() { err = KillSidecarAndWait(name, time.Second) })
			if !errors.Is(err, ErrSidecarNotSignalled) {
				t.Fatalf("err = %v, want ErrSidecarNotSignalled", err)
			}
			if got := errors.Is(err, ErrSidecarOwnerUnknown); got != tc.wantUnknown {
				t.Errorf("errors.Is(err, ErrSidecarOwnerUnknown) = %v, want %v (err = %v)", got, tc.wantUnknown, err)
			}
		})
	}
}
