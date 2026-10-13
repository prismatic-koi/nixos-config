package session

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// captureStderr returns what fn writes to os.Stderr. It drains the pipe
// while fn runs (docs/stdout-capture-testing.md).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&buf, r)
	}()
	prev := os.Stderr
	os.Stderr = w
	func() {
		defer func() { os.Stderr = prev }()
		fn()
	}()
	_ = w.Close()
	wg.Wait()
	_ = r.Close()
	return buf.String()
}

func assertPIDFileGone(t *testing.T, sessionName string) {
	t.Helper()
	pidPath, _ := SidecarPIDPath(sessionName)
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("PID file still present: %v", err)
	}
}

// startZombie starts a child that exits at once, and does not reap it until
// the test ends. It writes the PID of the zombie to the sidecar PID file of
// sessionName.
func startZombie(t *testing.T, sessionName string) int {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(self, "sidecar", "--session", sessionName)
	cmd.Env = append(os.Environ(), "PRISM_TEST_SUBPROCESS=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	pid := cmd.Process.Pid
	// The child closes the pipe when it exits. Do not wait with
	// sidecarProcessExists: it is the function under test.
	_, _ = io.Copy(io.Discard, out)
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("kill(%d, 0) = %v, want nil for an unreaped zombie", pid, err)
	}
	pidPath, err := SidecarPIDPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}
	return pid
}

// The real reader must work where the tests run, including inside the
// sandbox-exec profile of a prism session.
func TestReadPIDArgv_LiveProcess(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@argv"
	pid, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")

	argv, err := readPIDArgv(pid)
	if err != nil {
		t.Fatalf("readPIDArgv(%d): %v", pid, err)
	}
	want := []string{"sidecar", "--session", name}
	if len(argv) != len(want)+1 || argv[0] == "" || !slices.Equal(argv[1:], want) {
		t.Errorf("readPIDArgv(%d) = %q, want [<binary> %s]", pid, argv, strings.Join(want, " "))
	}
}

func TestKillSidecar_SignalsLiveSidecar(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@kill-live"
	_, wait := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")

	KillSidecar(name)

	if ws := wait(); !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Errorf("stub wait status = %v, want killed by SIGTERM", ws)
	}
	assertPIDFileGone(t, name)
}

// A PID file that names another process must not lead to any signal.
func TestKillSidecar_OtherProcessNotSignalled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@kill-other"
	pid, _ := startSidecarStub(t, name, []string{"not-a-sidecar"}, "PRISM_TEST_STUB_LONG=1")

	KillSidecar(name)

	if !sidecarProcessExists(pid) {
		t.Error("KillSidecar killed a process that is not a sidecar")
	}
	assertPIDFileGone(t, name)
}

// Fail closed: the stub has the argv of the sidecar, so only the owner
// check keeps it alive. If KillSidecar signals a PID whose command line it
// cannot read, the stub dies and this test fails.
func TestKillSidecar_UnreadableCmdlineNotSignalled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@kill-unreadable"
	pid, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")
	withFakePIDArgv(t, pid, nil, errors.New("fake read failure"))

	stderr := captureStderr(t, func() { KillSidecar(name) })

	time.Sleep(100 * time.Millisecond)
	if !sidecarProcessExists(pid) {
		t.Error("KillSidecar signalled a PID whose command line it cannot read")
	}
	if !strings.Contains(stderr, "pid "+strconv.Itoa(pid)+" ") {
		t.Errorf("stderr = %q, want a warning that names pid %d", stderr, pid)
	}
	assertPIDFileGone(t, name)
}

func TestStopSidecar_UnreadableCmdlineNotSignalled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@stop-unreadable"
	pid, _ := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_STUB_LONG=1")
	withFakePIDArgv(t, pid, nil, errors.New("fake read failure"))

	const timeout = 2 * time.Second
	var stopped bool
	start := time.Now()
	stderr := captureStderr(t, func() { stopped = StopSidecar(name, timeout) })
	elapsed := time.Since(start)

	if stopped {
		t.Error("StopSidecar = true, want false: the PID can still be the sidecar")
	}
	if elapsed >= timeout {
		t.Errorf("StopSidecar took %v, want it to return without the %v wait", elapsed, timeout)
	}
	time.Sleep(100 * time.Millisecond)
	if !sidecarProcessExists(pid) {
		t.Error("StopSidecar signalled a PID whose command line it cannot read")
	}
	if !strings.Contains(stderr, "pid "+strconv.Itoa(pid)+" ") {
		t.Errorf("stderr = %q, want a warning that names pid %d", stderr, pid)
	}
	assertPIDFileGone(t, name)
}

// A PID that no longer exists is not a failure to prove ownership: no
// warning, and StopSidecar reports that no sidecar runs.
func TestStopSidecar_GonePID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@stop-gone"
	_, wait := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_SUBPROCESS=1")
	wait()

	var stopped bool
	stderr := captureStderr(t, func() { stopped = StopSidecar(name, 2*time.Second) })

	if !stopped {
		t.Error("StopSidecar = false, want true for a PID that no longer exists")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want no warning for a PID that no longer exists", stderr)
	}
	assertPIDFileGone(t, name)
}

func TestKillSidecar_GonePID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@kill-gone"
	_, wait := startSidecarStub(t, name, []string{"sidecar", "--session", name}, "PRISM_TEST_SUBPROCESS=1")
	wait()

	stderr := captureStderr(t, func() { KillSidecar(name) })

	if stderr != "" {
		t.Errorf("stderr = %q, want no warning for a PID that no longer exists", stderr)
	}
	assertPIDFileGone(t, name)
}

// A zombie has exited. On Darwin, kill(pid, 0) succeeds for it and its
// command line cannot be read, so without the zombie check StopSidecar
// reports an unknown live PID and returns false.
func TestStopSidecar_ZombieIsGone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@stop-zombie"
	startZombie(t, name)

	var stopped bool
	stderr := captureStderr(t, func() { stopped = StopSidecar(name, 2*time.Second) })

	if !stopped {
		t.Error("StopSidecar = false, want true for a zombie")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want no warning for a zombie", stderr)
	}
	assertPIDFileGone(t, name)
}

func TestKillSidecar_ZombieIsGone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@kill-zombie"
	startZombie(t, name)

	stderr := captureStderr(t, func() { KillSidecar(name) })

	if strings.Contains(stderr, "cannot read") {
		t.Errorf("stderr = %q, want no unreadable-command-line warning for a zombie", stderr)
	}
	assertPIDFileGone(t, name)
}
