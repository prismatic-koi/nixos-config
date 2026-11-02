package session

import (
	"errors"
	"os"
	"strconv"
	"strings"
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
		stubPID, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")
		if pid, exited, gone := StopSidecarReport(name, 2*time.Second); pid != stubPID || !exited || !gone {
			t.Errorf("StopSidecarReport = (%d, %v, %v), want (%d, true, true)", pid, exited, gone, stubPID)
		}
		assertPIDFileGone(t, name)
	})

	t.Run("no PID file", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		if pid, exited, gone := StopSidecarReport("prism-test@report-none", 200*time.Millisecond); pid != 0 || !exited || !gone {
			t.Errorf("StopSidecarReport = (%d, %v, %v), want (0, true, true)", pid, exited, gone)
		}
	})

	// prism restore does not restore the session when gone is false. If the
	// PID file goes, the next restore finds no PID file, reports the old
	// sidecar as gone, and starts a second sidecar next to it (#3303).
	t.Run("unreadable command line", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		const name = "prism-test@report-unreadable"
		stubPID, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")
		withFakePIDArgv(t, stubPID, nil, errors.New("fake read failure"))
		var pid int
		var exited, gone bool
		stderr := captureStderr(t, func() { pid, exited, gone = StopSidecarReport(name, 2*time.Second) })
		if pid != stubPID || exited || gone {
			t.Errorf("StopSidecarReport = (%d, %v, %v), want (%d, false, false): the PID can still be the sidecar", pid, exited, gone, stubPID)
		}
		if !sidecarProcessExists(stubPID) {
			t.Error("StopSidecarReport signalled a PID whose command line it cannot read")
		}
		if !strings.Contains(stderr, "pid "+strconv.Itoa(stubPID)+" ") {
			t.Errorf("stderr = %q, want a warning that names pid %d", stderr, stubPID)
		}
		pidPath, err := SidecarPIDPath(name)
		if err != nil {
			t.Fatalf("SidecarPIDPath: %v", err)
		}
		if data, err := os.ReadFile(pidPath); err != nil || strings.TrimSpace(string(data)) != strconv.Itoa(stubPID) {
			t.Errorf("PID file = %q, %v, want it to stay with pid %d", data, err, stubPID)
		}
		_ = captureStderr(t, func() { pid, _, gone = StopSidecarReport(name, 200*time.Millisecond) })
		if pid != stubPID || gone {
			t.Errorf("second StopSidecarReport = (%d, gone %v), want (%d, gone false): the next stop must find the same process", pid, gone, stubPID)
		}
	})
}

// EnsureSidecarStoppedErr marks the error of an unreadable command line with
// ErrSidecarOwnerUnknown. A recycled PID is not the sidecar, so the stop
// reports no error for it: only the first can still be the sidecar.
func TestEnsureSidecarStoppedErr_OwnerUnknown_3175(t *testing.T) {
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
			const name = "prism-test@stop-err-owner"
			argv := tc.argv
			if argv == nil {
				argv = []string{"sidecar", "--session", name}
			}
			pid, _ := startSidecarStub(t, name, argv, "PRISM_TEST_STUB_LONG=1")
			if tc.unreadable {
				withFakePIDArgv(t, pid, nil, errors.New("fake read failure"))
			}
			var err error
			_ = captureStderr(t, func() { err = EnsureSidecarStoppedErr(name, time.Second) })
			if got := errors.Is(err, ErrSidecarOwnerUnknown); got != tc.wantUnknown {
				t.Errorf("errors.Is(err, ErrSidecarOwnerUnknown) = %v, want %v (err = %v)", got, tc.wantUnknown, err)
			}
			if !sidecarProcessExists(pid) {
				t.Error("EnsureSidecarStoppedErr signalled a PID that the owner check did not confirm")
			}
		})
	}
}
