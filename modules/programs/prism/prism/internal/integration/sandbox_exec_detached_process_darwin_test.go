//go:build darwin

package integration_test

// sandbox_exec_detached_process_darwin_test.go — a process that the agent
// detaches ends at the stop of its session (#3375).
//
// sandbox-exec has no PID namespace. A process that calls setsid(2) and
// whose parent exits leaves the process group of the agent, and launchd
// becomes its parent. It keeps the sandbox of the session, so it can still
// write the worktree. session.StopSandboxProcesses finds it by its sandbox:
// the sandbox allows a write in the run dir of the session (section 6 of
// generateProfile) and denies a write in the run root.
//
// The probe starts such a process under the production profile. The
// process ignores SIGTERM and SIGHUP, as under nohup, and appends to a
// file in the worktree until it ends. The paired negative takes the
// run-dir grant out of the profile, and the stop then does not find the
// process.

import (
	"bytes"
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

	"golang.org/x/sys/unix"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/sandboxcheck"
	"github.com/prismatic-koi/prism/internal/session"
)

const (
	detachOwnSession   = "prism-test@3375-own"
	detachOtherSession = "prism-test@3375-other"
)

// detachProbeSrc is the probe. "detach <pid-file> <mark-file>" starts
// "hold" in a new session, with /dev/null as its standard files, and exits.
// "hold" ignores SIGTERM and SIGHUP, writes its PID, and appends a byte to
// the mark file every 20 ms for 2 minutes. "loop <mark-file> <stop-file>"
// ignores SIGTERM and SIGHUP, appends a byte to the mark file, starts a new
// "loop" in a new session, and exits, until the stop file exists. Each
// process of the loop lives a few milliseconds.
const detachProbeSrc = `package main

import (
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) != 4 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "detach":
		null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			println(err.Error())
			os.Exit(1)
		}
		fd := null.Fd()
		_, err = syscall.ForkExec(os.Args[0], []string{os.Args[0], "hold", os.Args[2], os.Args[3]},
			&syscall.ProcAttr{Files: []uintptr{fd, fd, fd}, Sys: &syscall.SysProcAttr{Setsid: true}})
		if err != nil {
			println(err.Error())
			os.Exit(1)
		}
	case "loop":
		signal.Ignore(syscall.SIGTERM, syscall.SIGHUP)
		if _, err := os.Stat(os.Args[3]); err == nil {
			os.Exit(0)
		}
		if f, err := os.OpenFile(os.Args[2], os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
			_, _ = f.Write([]byte{'x'})
			_ = f.Close()
		}
		null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			os.Exit(1)
		}
		fd := null.Fd()
		_, _ = syscall.ForkExec(os.Args[0], []string{os.Args[0], "loop", os.Args[2], os.Args[3]},
			&syscall.ProcAttr{Files: []uintptr{fd, fd, fd}, Sys: &syscall.SysProcAttr{Setsid: true}})
	case "hold":
		signal.Ignore(syscall.SIGTERM, syscall.SIGHUP)
		tmp := os.Args[2] + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(1)
		}
		if err := os.Rename(tmp, os.Args[2]); err != nil {
			os.Exit(1)
		}
		for end := time.Now().Add(2 * time.Minute); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			if f, err := os.OpenFile(os.Args[3], os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
				_, _ = f.Write([]byte{'x'})
				_ = f.Close()
			}
		}
	}
}
`

// buildDetachProbe compiles the probe into a t.TempDir(), which section 2
// of the profile lets the sandbox run.
func buildDetachProbe(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "probe.go")
	if err := os.WriteFile(src, []byte(detachProbeSrc), 0o600); err != nil {
		t.Fatalf("write probe source: %v", err)
	}
	bin := filepath.Join(dir, "probe")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the detach probe: %v\n%s", err, out)
	}
	return bin
}

// newDetachManager returns a profile Manager for sessionName, with the
// production run dir of the session. The worktree is under HOME, so that
// the profile rule for it matches the real path. Set XDG_STATE_HOME first.
func newDetachManager(t *testing.T, sessionName string) (*container.Manager, string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatalf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-3375-detach-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })
	bareRoot := filepath.Join(wrap, "repo")
	worktree := filepath.Join(bareRoot, "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("MkdirAll(worktree): %v", err)
	}
	hostAPI, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(hostAPI), 0o700); err != nil {
		t.Fatalf("MkdirAll run dir: %v", err)
	}
	return container.New(container.Config{
		SessionName:     sessionName,
		InstanceID:      "integ-3375-" + session.SessionDirName(sessionName+t.Name()),
		Worktree:        worktree,
		BareRoot:        bareRoot,
		HostAPISockPath: hostAPI,
		GitUserName:     "test-user",
		GitUserEmail:    "test@example.com",
	}), worktree
}

// runDirOf returns the run dir of sessionName.
func runDirOf(t *testing.T, sessionName string) string {
	t.Helper()
	hostAPI, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	return filepath.Dir(hostAPI)
}

// detached is a process that the probe started in a new session.
type detached struct {
	pid  int
	mark string
}

// startDetached runs "probe detach" in dir, under profilePath, or with no
// sandbox when profilePath is empty. It returns when the held process has
// written its PID and its first mark. A cleanup kills the process if it
// still runs.
func startDetached(t *testing.T, profilePath, probe, dir string) detached {
	t.Helper()
	pidFile := filepath.Join(dir, "held.pid")
	mark := filepath.Join(dir, "late")
	args := []string{probe, "detach", pidFile, mark}
	if profilePath != "" {
		args = append([]string{sandboxExecPath, "-f", profilePath}, args...)
	}
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("run the detach probe: %v\n%s", err, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr != nil {
				t.Fatalf("PID file %s holds %q: %v", pidFile, data, convErr)
			}
			t.Cleanup(func() {
				if processIsProbe(pid) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			if info, statErr := os.Stat(mark); statErr == nil && info.Size() > 0 {
				return detached{pid: pid, mark: mark}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the held process did not write %s and %s", pidFile, mark)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processIsProbe reports whether pid runs the probe and is not a zombie.
func processIsProbe(pid int) bool {
	if syscall.Kill(pid, 0) == syscall.ESRCH {
		return false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_stat == 5 { // SZOMB
		return false
	}
	comm := kp.Proc.P_comm[:]
	if i := bytes.IndexByte(comm, 0); i >= 0 {
		comm = comm[:i]
	}
	return string(comm) == "probe"
}

// requireDetached fails the test unless launchd is the parent of p and p
// leads a session of its own, so that a stop of the process group of the
// agent does not reach it.
func requireDetached(t *testing.T, p detached) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", p.pid)
		if err != nil {
			t.Fatalf("kern.proc.pid %d: %v", p.pid, err)
		}
		sid, sidErr := unix.Getsid(p.pid)
		if kp.Eproc.Ppid == 1 && sidErr == nil && sid == p.pid {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d is not detached: parent %d, session %d (%v)", p.pid, kp.Eproc.Ppid, sid, sidErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitGone reports whether p stops within timeout.
func waitGone(p detached, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for processIsProbe(p.pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// markGrows reports whether the mark file of p grows within 500 ms.
func markGrows(t *testing.T, p detached) bool {
	t.Helper()
	before, err := os.Stat(p.mark)
	if err != nil {
		t.Fatalf("stat %s: %v", p.mark, err)
	}
	time.Sleep(500 * time.Millisecond)
	after, err := os.Stat(p.mark)
	if err != nil {
		t.Fatalf("stat %s: %v", p.mark, err)
	}
	return after.Size() > before.Size()
}

// TestSandboxExecDetachedProcess_StopEndsSetsidChild_3375: under the
// production profile, a process that calls setsid, ignores SIGTERM and
// SIGHUP, and has launchd as its parent writes the worktree. The stop of
// the session ends it, and the worktree file then stays as it is.
func TestSandboxExecDetachedProcess_StopEndsSetsidChild_3375(t *testing.T) {
	requireSandboxExec(t)
	probe := buildDetachProbe(t)
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))
	m, worktree := newDetachManager(t, detachOwnSession)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)

	p := startDetached(t, profilePath, probe, worktree)
	requireDetached(t, p)
	if !markGrows(t, p) {
		t.Fatal("the detached process does not write the worktree before the stop, so the test proves nothing")
	}
	if inBox, err := sandboxcheck.Sandboxed(p.pid); err != nil || !inBox {
		t.Fatalf("sandboxcheck.Sandboxed(%d) = %v, %v, want true", p.pid, inBox, err)
	}
	if inBox, err := sandboxcheck.Sandboxed(os.Getpid()); err != nil || inBox {
		t.Fatalf("sandboxcheck.Sandboxed(test process) = %v, %v, want false", inBox, err)
	}

	pids, err := sandboxcheck.ListPIDs(os.Getuid())
	if err != nil || !slices.Contains(pids, p.pid) || !slices.Contains(pids, os.Getpid()) {
		t.Fatalf("sandboxcheck.ListPIDs does not hold the process %d and the test process: %v", p.pid, err)
	}
	// The stop finds the process by these two answers of its sandbox.
	runDir := runDirOf(t, detachOwnSession)
	if ok, err := sandboxcheck.Allows(p.pid, "file-write-data", runDir); err != nil || !ok {
		t.Fatalf("the sandbox of the process does not allow a write of the run dir %s: %v, %v", runDir, ok, err)
	}
	if ok, err := sandboxcheck.Allows(p.pid, "file-write-data", filepath.Dir(runDir)); err != nil || ok {
		t.Fatalf("the sandbox of the process does not deny a write of the run root: %v, %v", ok, err)
	}

	if err := session.StopSandboxProcesses(detachOwnSession, session.SandboxProcessStopTimeout); err != nil {
		t.Fatalf("StopSandboxProcesses: %v", err)
	}
	if !waitGone(p, 5*time.Second) {
		t.Fatalf("the detached process %d still runs after the stop of its session", p.pid)
	}
	if markGrows(t, p) {
		t.Error("the worktree file still grows after the stop of the session")
	}
}

// TestSandboxExecDetachedProcess_StopSparesOtherProcesses_3375: the stop
// of a session ends no process in the sandbox of another session and no
// process with no sandbox.
func TestSandboxExecDetachedProcess_StopSparesOtherProcesses_3375(t *testing.T) {
	requireSandboxExec(t)
	probe := buildDetachProbe(t)
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))
	own, ownWorktree := newDetachManager(t, detachOwnSession)
	other, otherWorktree := newDetachManager(t, detachOtherSession)
	ownPrepared, _ := preparePositiveProfile(t, own)
	otherPrepared, _ := preparePositiveProfile(t, other)

	ownProc := startDetached(t, writeProbePositiveProfile(t, ownPrepared), probe, ownWorktree)
	otherProc := startDetached(t, writeProbePositiveProfile(t, otherPrepared), probe, otherWorktree)
	hostProc := startDetached(t, "", probe, t.TempDir())

	if err := session.StopSandboxProcesses(detachOwnSession, session.SandboxProcessStopTimeout); err != nil {
		t.Fatalf("StopSandboxProcesses: %v", err)
	}
	if !waitGone(ownProc, 5*time.Second) {
		t.Fatalf("the detached process %d of the stopped session still runs", ownProc.pid)
	}
	if !processIsProbe(otherProc.pid) || !markGrows(t, otherProc) {
		t.Errorf("the stop of %q ended process %d in the sandbox of %q", detachOwnSession, otherProc.pid, detachOtherSession)
	}
	if !processIsProbe(hostProc.pid) || !markGrows(t, hostProc) {
		t.Errorf("the stop of %q ended process %d, which has no sandbox", detachOwnSession, hostProc.pid)
	}
}

// TestSandboxExecDetachedProcess_NotFoundWithoutRunDirGrant_3375 is the
// paired negative. With the run-dir rules of the profile pointed at a path
// that does not exist, the stop does not find the process, so the positive
// test depends on the run-dir grant of the profile.
func TestSandboxExecDetachedProcess_NotFoundWithoutRunDirGrant_3375(t *testing.T) {
	requireSandboxExec(t)
	probe := buildDetachProbe(t)
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))
	m, worktree := newDetachManager(t, detachOwnSession)
	runDir := runDirOf(t, detachOwnSession)
	mutatedPath := withMutatedProbeProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, sbplQuoteForTest(runDir), sbplQuoteForTest(runDir+".prism-3375-disabled"))
	})

	p := startDetached(t, mutatedPath, probe, worktree)
	if err := session.StopSandboxProcesses(detachOwnSession, session.SandboxProcessStopTimeout); err != nil {
		t.Fatalf("StopSandboxProcesses: %v", err)
	}
	if !processIsProbe(p.pid) || !markGrows(t, p) {
		t.Error("the stop ended the process without the run-dir grant, so the positive test does not show how the stop finds the process")
	}
}

// TestSandboxExecDetachedProcess_ForkLoopEndsOrRefuses_3375: under the
// production profile, a loop of processes that each start the next one in
// a new session and exit at once writes the worktree. Such a process can
// exit before a scan checks it. The stop must end the loop or refuse, and
// it must never report success while the loop runs.
func TestSandboxExecDetachedProcess_ForkLoopEndsOrRefuses_3375(t *testing.T) {
	requireSandboxExec(t)
	probe := buildDetachProbe(t)
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))
	m, worktree := newDetachManager(t, detachOwnSession)
	prepared, _ := preparePositiveProfile(t, m)
	profilePath := writeProbePositiveProfile(t, prepared)
	mark := filepath.Join(worktree, "late")
	stop := filepath.Join(t.TempDir(), "stop")
	loop := detached{mark: mark}
	t.Cleanup(func() {
		// The loop exits when it sees the stop file.
		_ = os.WriteFile(stop, nil, 0o600)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			before, _ := os.Stat(mark)
			time.Sleep(300 * time.Millisecond)
			after, _ := os.Stat(mark)
			if before == nil || after == nil || after.Size() == before.Size() {
				return
			}
		}
		t.Error("the loop did not stop at the stop file")
	})

	if out, err := exec.Command(sandboxExecPath, "-f", profilePath, probe, "loop", mark, stop).CombinedOutput(); err != nil {
		t.Fatalf("run the loop probe: %v\n%s", err, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if info, err := os.Stat(mark); err == nil && info.Size() >= 20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop did not write the worktree, so the test proves nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}

	err := session.StopSandboxProcesses(detachOwnSession, session.SandboxProcessStopTimeout)
	if err == nil {
		if markGrows(t, loop) {
			t.Fatal("the stop reported success, and the loop still writes the worktree")
		}
		t.Log("the stop ended the loop")
		return
	}
	var se *session.SandboxProcessError
	if !errors.As(err, &se) || se.Session != detachOwnSession || !strings.Contains(err.Error(), strconv.Quote(detachOwnSession)) {
		t.Fatalf("error = %v, want a *SandboxProcessError that names the session", err)
	}
	t.Logf("the stop refused: %v", err)
}

// TestSandboxExecDetachedProcess_ZombieDoesNotBlockTheStop_3375: a zombie
// of this user that no parent reaps does not make the stop of a session
// fail. The stop waits until the zombie settles, then reports success.
func TestSandboxExecDetachedProcess_ZombieDoesNotBlockTheStop_3375(t *testing.T) {
	requireSandboxExec(t)
	t.Setenv("XDG_STATE_HOME", stateHomeOutsideTmp(t))
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", cmd.Process.Pid)
		if err == nil && kp.Proc.P_stat == 5 { // SZOMB
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d did not become a zombie: %v", cmd.Process.Pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	if err := session.StopSandboxProcesses(detachOwnSession, session.SandboxProcessStopTimeout); err != nil {
		t.Fatalf("StopSandboxProcesses with a zombie of this user: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the stop took %v with a zombie of this user, want less than 3s", took)
	}
}
