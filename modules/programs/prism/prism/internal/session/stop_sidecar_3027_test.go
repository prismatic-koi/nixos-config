package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// startSidecarStub starts this test binary with argv and the extra env, and
// writes its PID to the sidecar PID file of sessionName. The returned wait
// function reaps the process and returns its wait status.
func startSidecarStub(t *testing.T, sessionName string, argv []string, env ...string) (int, func() syscall.WaitStatus) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(self, argv...)
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub: %v", err)
	}
	pidPath, err := SidecarPIDPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}
	done := make(chan syscall.WaitStatus, 1)
	go func() {
		_ = cmd.Wait()
		ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
		done <- ws
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	// Give the stub time to install its signal disposition.
	time.Sleep(100 * time.Millisecond)
	return cmd.Process.Pid, func() syscall.WaitStatus {
		select {
		case ws := <-done:
			return ws
		case <-time.After(5 * time.Second):
			t.Fatal("stub did not exit")
			return 0
		}
	}
}

func TestStopSidecar_NoPIDFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if !StopSidecar("prism-test@stop-none", 200*time.Millisecond) {
		t.Error("StopSidecar with no PID file = false, want true")
	}
}

func TestStopSidecar_GracefulStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@stop-graceful"
	_, wait := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")

	if !StopSidecar(name, 2*time.Second) {
		t.Fatal("StopSidecar = false, want true for a sidecar that stops on SIGTERM")
	}
	if ws := wait(); !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("stub wait status = %v, want killed by SIGTERM", ws)
	}
}

// A sidecar that ignores SIGTERM must get SIGKILL, so that it cannot write
// to the database after restore decides what to do with its instance.
func TestStopSidecar_SIGKILLsSidecarThatIgnoresSIGTERM(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@stop-stuck"
	_, wait := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_IGNORE_TERM=1")

	if StopSidecar(name, 200*time.Millisecond) {
		t.Fatal("StopSidecar = true, want false for a sidecar that ignores SIGTERM")
	}
	if ws := wait(); !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("stub wait status = %v, want killed by SIGKILL", ws)
	}
}

// A PID file that names another process must not lead to any signal.
func TestStopSidecar_OtherProcessNotSignalled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@stop-other"
	pid, _ := startSidecarStub(t, name, []string{"not-a-sidecar"}, "PRISM_TEST_STUB_LONG=1")

	if !StopSidecar(name, 200*time.Millisecond) {
		t.Error("StopSidecar = false, want true when the PID belongs to another process")
	}
	if !sidecarProcessExists(pid) {
		t.Error("StopSidecar killed a process that is not the session's sidecar")
	}
	pidPath, _ := SidecarPIDPath(name)
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("PID file still present after StopSidecar: %v", err)
	}
}

// withFakeProcCmdline points procRoot at a fake tree where pid has the given
// /proc/<pid>/cmdline content.
func withFakeProcCmdline(t *testing.T, pid int, cmdline string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fake proc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatalf("write fake cmdline: %v", err)
	}
	prev := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = prev })
}

func TestSidecarPIDOwner(t *testing.T) {
	const name = "prism-test@owner"
	cases := []struct {
		desc    string
		cmdline string
		want    pidOwner
	}{
		{"sidecar of the session", "/bin/prism\x00sidecar\x00--session\x00" + name + "\x00", pidOwnerSidecar},
		{"sidecar of another session", "/bin/prism\x00sidecar\x00--session\x00other@x\x00", pidOwnerOther},
		{"another program", "/usr/bin/sleep\x0030\x00", pidOwnerOther},
		{"empty command line (kernel thread)", "", pidOwnerOther},
	}
	for i, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			pid := 4_000_000 + i
			withFakeProcCmdline(t, pid, c.cmdline)
			if got := sidecarPIDOwner(pid, name); got != c.want {
				t.Errorf("sidecarPIDOwner = %v, want %v", got, c.want)
			}
		})
	}
}

// After a reboot, the PID in a stale PID file can belong to a kernel thread,
// whose command line is empty. StopSidecar must treat that PID as another
// process: return true at once, send no signal, and remove the PID file.
// Restore then keeps the instance ID of the session.
func TestStopSidecar_EmptyCmdlineIsNotASidecar(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@stop-kthread"
	pid, _ := startSidecarStub(t, name, []string{"not-a-sidecar"}, "PRISM_TEST_STUB_LONG=1")
	withFakeProcCmdline(t, pid, "")

	const timeout = 2 * time.Second
	start := time.Now()
	stopped := StopSidecar(name, timeout)
	elapsed := time.Since(start)

	if !stopped {
		t.Error("StopSidecar = false, want true for a PID with an empty command line")
	}
	if elapsed >= timeout {
		t.Errorf("StopSidecar took %v, want it to return without the %v wait", elapsed, timeout)
	}
	if !sidecarProcessExists(pid) {
		t.Error("StopSidecar signalled a process that is not the session's sidecar")
	}
	pidPath, _ := SidecarPIDPath(name)
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("PID file still present after StopSidecar: %v", err)
	}
}
