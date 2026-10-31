package session

// Tests for #3380: a stop of a sidecar also stops each process that the
// sidecar started in its process group, for example a podman client of
// `prism container`. Such a process can otherwise create a container after
// the container sweep of the stop.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// clientScript is a fake podman client. It ignores SIGTERM and SIGHUP, and
// runs until SIGKILL.
const clientScript = `sh -c 'trap "" TERM HUP; echo $$ > "$STUB_DIR/client.tmp"; mv "$STUB_DIR/client.tmp" "$STUB_DIR/client.pid"; while :; do sleep 0.05; done' &
while [ ! -s "$STUB_DIR/client.pid" ]; do sleep 0.01; done
`

// sidecarWithClient is a fake sidecar that starts the fake client in its
// process group and exits on SIGTERM.
const sidecarWithClient = clientScript + `trap 'exit 0' TERM
: > "$STUB_DIR/ready"
while :; do sleep 0.05; done
`

// sidecarRemovingPIDFile is sidecarWithClient, but at SIGTERM it removes
// its own PID file ($STUB_PIDFILE) before it exits, as the real sidecar
// does.
const sidecarRemovingPIDFile = clientScript + `trap 'rm -f "$STUB_PIDFILE"; exit 0' TERM
: > "$STUB_DIR/ready"
while :; do sleep 0.05; done
`

// startGroupStub starts script under /bin/sh with the command line
// `sh sidecar --session name`, so that the owner check accepts it as the
// sidecar of name, and writes its PID to the PID file of name. attr sets
// the process group: Setsid as StartSidecarWithOpts does, or another one.
// $STUB_DIR of the script is dir. When reap is false, the test does not
// reap the stub, and an exited stub stays a zombie.
func startGroupStub(t *testing.T, name, dir, script string, attr *syscall.SysProcAttr, reap bool) (pid int, exited <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script, "sh", "sidecar", "--session", name)
	cmd.Env = append(os.Environ(), "STUB_DIR="+dir)
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub: %v", err)
	}
	done := make(chan struct{})
	if reap {
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if !reap {
			_ = cmd.Wait()
		}
	})
	pidPath, err := SidecarPIDPath(name)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}
	return cmd.Process.Pid, done
}

// waitClientPID returns the PID of the fake client, and kills the client at
// the end of the test.
func waitClientPID(t *testing.T, dir string) int {
	t.Helper()
	deadline := time.Now().Add(stubReadyTimeout)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "client.pid"))
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && convErr == nil && pid > 0 {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake client did not write its PID within %v", stubReadyTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitStubFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(stubReadyTimeout)
	for !fileExists(path) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %v", path, stubReadyTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitExited waits until the stub exits.
func waitExited(t *testing.T, exited <-chan struct{}) {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(stubReadyTimeout):
		t.Fatal("the stub did not exit")
	}
}

func pidFilePresent(t *testing.T, name string) bool {
	t.Helper()
	p, err := SidecarPIDPath(name)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	return fileExists(p)
}

// AC: a process that the sidecar started, and that still runs when the
// sidecar exits, is gone when the stop returns.
func TestEnsureSidecarStoppedErr_KillsProcessThatTheSidecarStarted_3380(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@group-kill"
	dir := t.TempDir()
	_, exited := startGroupStub(t, name, dir, sidecarWithClient, &syscall.SysProcAttr{Setsid: true}, true)
	waitStubFile(t, filepath.Join(dir, "ready"))
	client := waitClientPID(t, dir)

	if err := EnsureSidecarStoppedErr(name, 2*time.Second); err != nil {
		t.Fatalf("EnsureSidecarStoppedErr = %v, want nil", err)
	}
	waitExited(t, exited)
	if sidecarProcessExists(client) {
		t.Errorf("the client (pid %d) that the sidecar started still runs after the stop", client)
	}
	if pidFilePresent(t, name) {
		t.Error("PID file still present after the stop")
	}
}

// AC: the stop is refused when a process that the sidecar started still
// runs after the wait. The error names the PID and the session. The PID
// file stays, also when the sidecar removed it at SIGTERM, so that the next
// stop waits for the group again.
func TestEnsureSidecarStoppedErr_RefusesWhenAProcessOfTheGroupStillRuns_3380(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@group-survivor"
	var kills []int
	prev := signalGroup
	signalGroup = func(pgid int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			// The process survives SIGKILL, as a process in an
			// uninterruptible wait does.
			kills = append(kills, pgid)
			return nil
		}
		return prev(pgid, sig)
	}
	t.Cleanup(func() { signalGroup = prev })
	pidPath, err := SidecarPIDPath(name)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	t.Setenv("STUB_PIDFILE", pidPath)
	dir := t.TempDir()
	pid, exited := startGroupStub(t, name, dir, sidecarRemovingPIDFile, &syscall.SysProcAttr{Setsid: true}, true)
	waitStubFile(t, filepath.Join(dir, "ready"))
	client := waitClientPID(t, dir)

	err = EnsureSidecarStoppedErr(name, 300*time.Millisecond)
	var groupErr *SidecarGroupError
	if !errors.As(err, &groupErr) {
		t.Fatalf("EnsureSidecarStoppedErr = %v, want a *SidecarGroupError", err)
	}
	if !groupErr.Signalled || groupErr.PGID != pid || !slices.Contains(groupErr.PIDs, client) {
		t.Errorf("error = %+v, want a signalled group %d that holds the client %d", groupErr, pid, client)
	}
	if msg := err.Error(); !strings.Contains(msg, strconv.Itoa(client)) || !strings.Contains(msg, strconv.Quote(name)) {
		t.Errorf("error %q does not name the client pid %d and the session %q", msg, client, name)
	}
	if len(kills) == 0 || kills[0] != pid {
		t.Errorf("SIGKILL went to groups %v, want the group %d of the sidecar", kills, pid)
	}
	waitExited(t, exited)
	if got := readSidecarPID(name); got != pid {
		t.Errorf("PID file holds %d, want %d: the sidecar removed its PID file, and the next stop must still find the group", got, pid)
	}
	if EnsureSidecarStopped(name, 100*time.Millisecond) {
		t.Error("EnsureSidecarStopped = true, want false while the client runs")
	}
}

// AC: when the sidecar has already exited and its process group is empty,
// the stop returns at once. A zombie has exited, so a sidecar that its
// parent did not reap yet does not count.
func TestEnsureSidecarStoppedErr_ExitedSidecarEmptyGroupReturnsAtOnce_3380(t *testing.T) {
	for _, reap := range []bool{true, false} {
		t.Run("reaped="+strconv.FormatBool(reap), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const name = "prism-test@group-empty"
			pid, exited := startGroupStub(t, name, t.TempDir(), "exit 0", &syscall.SysProcAttr{Setsid: true}, reap)
			if reap {
				waitExited(t, exited)
			} else {
				deadline := time.Now().Add(stubReadyTimeout)
				for sidecarProcessExists(pid) {
					if time.Now().After(deadline) {
						t.Fatal("the stub did not exit")
					}
					time.Sleep(5 * time.Millisecond)
				}
			}

			const timeout = 5 * time.Second
			start := time.Now()
			if err := EnsureSidecarStoppedErr(name, timeout); err != nil {
				t.Fatalf("EnsureSidecarStoppedErr = %v, want nil", err)
			}
			if elapsed := time.Since(start); elapsed > timeout/2 {
				t.Errorf("the stop took %v, want much less than the timeout %v", elapsed, timeout)
			}
			if pidFilePresent(t, name) {
				t.Error("PID file still present after the stop")
			}
		})
	}
}

// When the sidecar exited before the stop, prism cannot prove that the
// group is the group of the sidecar. A process of the group in the session
// of the sidecar gets no signal, and the stop is refused.
func TestEnsureSidecarStoppedErr_ExitedSidecarLiveGroupRefusedWithoutSignal_3380(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@group-orphan"
	dir := t.TempDir()
	pid, exited := startGroupStub(t, name, dir, clientScript+"exit 0\n", &syscall.SysProcAttr{Setsid: true}, true)
	client := waitClientPID(t, dir)
	waitExited(t, exited)

	err := EnsureSidecarStoppedErr(name, 300*time.Millisecond)
	var groupErr *SidecarGroupError
	if !errors.As(err, &groupErr) {
		t.Fatalf("EnsureSidecarStoppedErr = %v, want a *SidecarGroupError", err)
	}
	if groupErr.Signalled || groupErr.PGID != pid || !slices.Contains(groupErr.PIDs, client) {
		t.Errorf("error = %+v, want an unsignalled group %d that holds the client %d", groupErr, pid, client)
	}
	if msg := err.Error(); !strings.Contains(msg, strconv.Itoa(client)) || !strings.Contains(msg, strconv.Quote(name)) {
		t.Errorf("error %q does not name the client pid %d and the session %q", msg, client, name)
	}
	if !sidecarProcessExists(client) {
		t.Error("the stop signalled a process of a group that it cannot prove to be the group of the sidecar")
	}
	if !pidFilePresent(t, name) {
		t.Error("PID file removed, want kept for the next stop")
	}

	if err := syscall.Kill(client, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the client: %v", err)
	}
	if err := EnsureSidecarStoppedErr(name, 2*time.Second); err != nil {
		t.Errorf("EnsureSidecarStoppedErr after the client exited = %v, want nil", err)
	}
	if pidFilePresent(t, name) {
		t.Error("PID file still present after the stop")
	}
}

// A group in another session is not the group of the sidecar: the sidecar
// starts in a session of its own, and setpgid cannot move a process to
// another session. A stale PID file that names the ID of such a group does
// not stop the session, and its processes get no signal.
func TestEnsureSidecarStoppedErr_ExitedSidecarGroupOfAnotherSessionIgnored_3380(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const name = "prism-test@group-foreign"
	dir := t.TempDir()
	_, exited := startGroupStub(t, name, dir, clientScript+"exit 0\n", &syscall.SysProcAttr{Setpgid: true}, true)
	client := waitClientPID(t, dir)
	waitExited(t, exited)

	if err := EnsureSidecarStoppedErr(name, 5*time.Second); err != nil {
		t.Fatalf("EnsureSidecarStoppedErr = %v, want nil", err)
	}
	if !sidecarProcessExists(client) {
		t.Error("the stop signalled a process of a group in another session")
	}
	if pidFilePresent(t, name) {
		t.Error("PID file still present after the stop")
	}
}

// sidecarWithStopper is a fake sidecar that starts the fake client and a
// helper process in its process group. The helper stops the session of
// the sidecar, as `prism cleanup` that the sidecar runs for the host API
// does.
const sidecarWithStopper = clientScript + `trap 'exit 0' TERM
"$STUB_HELPER" -test.run='^TestStopOwnSessionHelperProcess_3380$' &
while :; do sleep 0.05; done
`

// A stop that the sidecar started, in the process group of the sidecar,
// leaves the group before the SIGKILL. It does not kill itself, and it
// does not wait for itself.
func TestEnsureSidecarStoppedErr_StopFromInsideTheGroup_3380(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	const name = "prism-test@group-self"
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dir := t.TempDir()
	t.Setenv("STUB_HELPER", self)
	t.Setenv("PRISM_GROUP_HELPER_STATE", state)
	t.Setenv("PRISM_GROUP_HELPER_SESSION", name)
	t.Setenv("PRISM_GROUP_HELPER_RESULT", filepath.Join(dir, "result"))
	_, exited := startGroupStub(t, name, dir, sidecarWithStopper, &syscall.SysProcAttr{Setsid: true}, true)
	client := waitClientPID(t, dir)

	waitStubFile(t, filepath.Join(dir, "result"))
	waitExited(t, exited)
	data, err := os.ReadFile(filepath.Join(dir, "result"))
	if err != nil {
		t.Fatalf("read the result of the helper: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "ok" {
		t.Errorf("the stop in the helper returned %q, want ok", got)
	}
	if sidecarProcessExists(client) {
		t.Errorf("the client (pid %d) still runs after the stop", client)
	}
}

// TestStopOwnSessionHelperProcess_3380 is the helper process of
// TestEnsureSidecarStoppedErr_StopFromInsideTheGroup_3380. It does nothing
// in a normal test run.
func TestStopOwnSessionHelperProcess_3380(t *testing.T) {
	result := os.Getenv("PRISM_GROUP_HELPER_RESULT")
	if result == "" {
		return
	}
	t.Setenv("XDG_STATE_HOME", os.Getenv("PRISM_GROUP_HELPER_STATE"))
	out := "ok"
	if err := EnsureSidecarStoppedErr(os.Getenv("PRISM_GROUP_HELPER_SESSION"), 2*time.Second); err != nil {
		out = err.Error()
	}
	if err := os.WriteFile(result+".tmp", []byte(out+"\n"), 0o644); err == nil {
		_ = os.Rename(result+".tmp", result)
	}
	os.Exit(0)
}
