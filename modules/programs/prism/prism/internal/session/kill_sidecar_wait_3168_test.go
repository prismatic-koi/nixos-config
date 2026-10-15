package session

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// When KillSidecar sends no signal, KillSidecarAndWait returns at once with
// ErrSidecarNotSignalled. It does not wait the full timeout, and it does not
// report "did not exit … after SIGTERM" (#3168).
func TestKillSidecarAndWait_NoSignalReturnsAtOnce_3168(t *testing.T) {
	const timeout = 5 * time.Second
	cases := []struct {
		desc       string
		argv       []string
		unreadable bool
	}{
		{"recycled PID", []string{"not-a-sidecar"}, false},
		{"unreadable command line", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const name = "prism-test@kill-wait-nosignal"
			argv := tc.argv
			if argv == nil {
				argv = []string{"sidecar", "--session", name}
			}
			pid, _ := startSidecarStub(t, name, argv, "PRISM_TEST_STUB_LONG=1")
			if tc.unreadable {
				withFakePIDArgv(t, pid, nil, errors.New("fake read failure"))
			}

			start := time.Now()
			var err error
			_ = captureStderr(t, func() { err = KillSidecarAndWait(name, timeout) })
			elapsed := time.Since(start)

			if !errors.Is(err, ErrSidecarNotSignalled) {
				t.Fatalf("err = %v, want ErrSidecarNotSignalled", err)
			}
			if strings.Contains(err.Error(), "after SIGTERM") {
				t.Errorf("err = %q, names a SIGTERM that was not sent", err)
			}
			if elapsed > timeout/2 {
				t.Errorf("KillSidecarAndWait took %v, want an immediate return", elapsed)
			}
			if !sidecarProcessExists(pid) {
				t.Error("KillSidecarAndWait signalled a PID that the owner check did not confirm")
			}
			assertPIDFileGone(t, name)
		})
	}
}

func TestKillSidecar_ReportsSignal_3168(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const live = "prism-test@kill-report-live"
	_, wait := startSidecarStub(t, live, []string{"sidecar", "--session", live}, "PRISM_TEST_STUB_LONG=1")
	if !KillSidecar(live) {
		t.Error("KillSidecar = false for a live sidecar, want true")
	}
	_ = wait()

	const other = "prism-test@kill-report-other"
	startSidecarStub(t, other, []string{"not-a-sidecar"}, "PRISM_TEST_STUB_LONG=1")
	var sent bool
	_ = captureStderr(t, func() { sent = KillSidecar(other) })
	if sent {
		t.Error("KillSidecar = true for a recycled PID, want false")
	}
	if KillSidecar("prism-test@kill-report-none") {
		t.Error("KillSidecar = true with no PID file, want false")
	}
}
