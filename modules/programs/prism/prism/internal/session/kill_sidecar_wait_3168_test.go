package session

import (
	"testing"
)

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
