//go:build !darwin

package session

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Off Darwin the sandbox stop does nothing, so each path that calls it
// behaves as before on Linux, in bwrap and in host mode (#3392). It sends
// no signal, makes no run dir, and returns at once.
func TestSandboxStopOffDarwin_IsANoOp_3392(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	prev := signalSandboxProcess
	signalSandboxProcess = func(pid int, sig syscall.Signal) error {
		t.Errorf("signal %v sent to process %d off Darwin", sig, pid)
		return nil
	}
	t.Cleanup(func() { signalSandboxProcess = prev })
	const name = "prism-test@3392-off-darwin"

	start := time.Now()
	if err := StopSandboxProcesses(name, SandboxProcessStopTimeout); err != nil {
		t.Fatalf("StopSandboxProcesses = %v, want nil", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the stop took %v, want no wait", took)
	}
	sock, err := SidecarHostAPIPath(name)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(sock)); !os.IsNotExist(err) {
		t.Errorf("run dir of %q: stat err = %v, want not exist", name, err)
	}
}
