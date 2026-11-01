package session

// Tests for #3375: the stop of a session ends each process in the sandbox
// of the session, also a process that left the process group of the agent.
// The tests replace the kernel access. The Darwin access, with the real
// sandbox, has its tests in internal/integration
// (TestSandboxExecDetachedProcess_*).

import (
	"errors"
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

// fakeSandbox is a set of fake processes in the sandbox. A process in
// ignoreTerm stops only at SIGKILL. Each other process stops at SIGTERM.
type fakeSandbox struct {
	mu         sync.Mutex
	running    []int
	ignoreTerm map[int]bool
	signals    []string
	// spawn is a process that a running process starts at the first
	// SIGTERM, before it stops.
	spawn int
}

func (f *fakeSandbox) probe() procProbe {
	return procProbe{
		list: func() ([]int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return slices.Clone(f.running), nil
		},
		inSandbox: func(pid int) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !slices.Contains(f.running, pid) {
				return false, syscall.ESRCH
			}
			return true, nil
		},
		zombie: func(int) (bool, time.Time) { return false, time.Time{} },
	}
}

func (f *fakeSandbox) find() sandboxFinder { return newSandboxFinder(f.probe(), -1) }

func (f *fakeSandbox) signal(pid int, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, strconv.Itoa(pid)+":"+sig.String())
	if f.spawn != 0 && sig == syscall.SIGTERM {
		f.running = append(f.running, f.spawn)
		f.ignoreTerm[f.spawn] = true
		f.spawn = 0
	}
	if sig == syscall.SIGKILL || !f.ignoreTerm[pid] {
		f.running = slices.DeleteFunc(f.running, func(p int) bool { return p == pid })
	}
	return nil
}

func (f *fakeSandbox) install(t *testing.T) {
	t.Helper()
	prev := signalSandboxProcess
	signalSandboxProcess = f.signal
	t.Cleanup(func() { signalSandboxProcess = prev })
}

func (f *fakeSandbox) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.signals)
}

func TestStopSandboxProcesses_NoProcessSendsNoSignal_3375(t *testing.T) {
	f := &fakeSandbox{ignoreTerm: map[int]bool{}}
	f.install(t)
	if err := stopSandboxProcesses("s", f.find(), time.Second, 0); err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if got := f.sent(); len(got) != 0 {
		t.Errorf("signals = %v, want none", got)
	}
}

// A process that stops at SIGTERM gets no SIGKILL.
func TestStopSandboxProcesses_SIGTERMFirst_3375(t *testing.T) {
	f := &fakeSandbox{running: []int{101, 102}, ignoreTerm: map[int]bool{}}
	f.install(t)
	if err := stopSandboxProcesses("s", f.find(), 5*time.Second, time.Second); err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if got, want := f.sent(), []string{"101:terminated", "102:terminated"}; !slices.Equal(got, want) {
		t.Errorf("signals = %v, want %v", got, want)
	}
}

// AC: a process that ignores SIGTERM, as under nohup, gets SIGKILL after
// the grace. A process that a process starts during the stop is found at
// the next scan and also stopped.
func TestStopSandboxProcesses_SIGKILLAfterGraceAndNewChild_3375(t *testing.T) {
	f := &fakeSandbox{running: []int{201}, ignoreTerm: map[int]bool{201: true}, spawn: 202}
	f.install(t)
	grace := 100 * time.Millisecond
	start := time.Now()
	if err := stopSandboxProcesses("s", f.find(), 5*time.Second, grace); err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Errorf("the stop took %v, want at least the grace %v before SIGKILL", elapsed, grace)
	}
	got := f.sent()
	for _, want := range []string{"201:terminated", "201:killed", "202:killed"} {
		if !slices.Contains(got, want) {
			t.Errorf("signals = %v, want %s", got, want)
		}
	}
	if i, j := slices.Index(got, "201:terminated"), slices.Index(got, "201:killed"); i > j {
		t.Errorf("signals = %v, want SIGTERM before SIGKILL", got)
	}
}

// AC: when a process still runs at the timeout, the stop fails with an
// error that names the process and the session.
func TestStopSandboxProcesses_RefusesWhenAProcessStillRuns_3375(t *testing.T) {
	f := &fakeSandbox{running: []int{301, 302}, ignoreTerm: map[int]bool{}}
	prev := signalSandboxProcess
	signalSandboxProcess = func(int, syscall.Signal) error { return syscall.EPERM }
	t.Cleanup(func() { signalSandboxProcess = prev })

	err := stopSandboxProcesses("repo@feat", f.find(), 100*time.Millisecond, 0)
	var se *SandboxProcessError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *SandboxProcessError", err)
	}
	if !slices.Equal(se.PIDs, []int{301, 302}) || se.Session != "repo@feat" {
		t.Errorf("error = %+v, want PIDs [301 302] and session repo@feat", se)
	}
	for _, want := range []string{"301", "302", strconv.Quote("repo@feat")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// AC: when prism cannot list the processes or check a sandbox, it cannot
// prove that no process of the session runs, so the stop fails.
func TestStopSandboxProcesses_ListOrCheckErrorRefuses_3375(t *testing.T) {
	listErr := errors.New("proc_listpids failed")
	find := newSandboxFinder(procProbe{list: func() ([]int, error) { return nil, listErr }}, -1)
	err := stopSandboxProcesses("repo@feat", find, time.Second, 0)
	var se *SandboxProcessError
	if !errors.As(err, &se) || !errors.Is(err, listErr) || !strings.Contains(err.Error(), strconv.Quote("repo@feat")) {
		t.Errorf("error = %v, want a *SandboxProcessError for the session that wraps the list error", err)
	}

	checkErr := errors.New("sandbox_check: invalid argument")
	find = newSandboxFinder(procProbe{
		list:      func() ([]int, error) { return []int{401}, nil },
		inSandbox: func(int) (bool, error) { return false, checkErr },
	}, -1)
	err = stopSandboxProcesses("repo@feat", find, time.Second, 0)
	for _, want := range []string{"401", strconv.Quote("repo@feat"), "invalid argument"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %s", err, want)
		}
	}
}

// AC (security review of #3375): a process that exits before the scan can
// check it is unclear, not clean. A loop that starts a child and exits is
// always in the list as such a process, so the stop never reports success
// while the loop runs. It refuses at the timeout and names the processes.
func TestStopSandboxProcesses_ForkAndExitLoopRefuses_3375(t *testing.T) {
	var mu sync.Mutex
	next := 1000
	find := newSandboxFinder(procProbe{
		// The atomic list holds the loop process that runs at the list.
		list: func() ([]int, error) {
			mu.Lock()
			defer mu.Unlock()
			next++
			return []int{50, next}, nil
		},
		// It starts a child and exits before the check, and launchd
		// reaps it at once.
		inSandbox: func(pid int) (bool, error) {
			if pid == 50 {
				return false, nil
			}
			return false, syscall.ESRCH
		},
		zombie: func(int) (bool, time.Time) { return false, time.Time{} },
	}, -1)

	err := stopSandboxProcesses("repo@loop", find, 200*time.Millisecond, 0)
	var se *SandboxProcessError
	if !errors.As(err, &se) || len(se.Unclear) == 0 {
		t.Fatalf("error = %v, want a refusal that names the unclear processes", err)
	}
	for _, want := range []string{strconv.Itoa(se.Unclear[0]), strconv.Quote("repo@loop"), "exited before prism could check"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// A zombie that is new in this stop is unclear: its child can be missing
// from the list. A zombie that stays a zombie for the settle time is
// ignored, so a zombie of an unrelated process does not block the stop.
func TestStopSandboxProcesses_ZombieUnclearUntilSettled_3375(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	find := newSandboxFinder(procProbe{
		list:      func() ([]int, error) { return []int{60}, nil },
		inSandbox: func(int) (bool, error) { return false, syscall.ESRCH },
		zombie:    func(int) (bool, time.Time) { return true, start },
	}, -1)

	scan, err := find(syscall.SIGTERM)
	if err != nil || !slices.Equal(scan.unclear, []int{60}) {
		t.Fatalf("first scan = %+v, %v, want the new zombie 60 unclear", scan, err)
	}
	began := time.Now()
	if err := stopSandboxProcesses("s", find, 5*time.Second, 0); err != nil {
		t.Fatalf("error = %v, want nil once the zombie settled", err)
	}
	if waited := time.Since(began); waited > sandboxZombieSettle+time.Second {
		t.Errorf("the stop waited %v for a zombie, want about %v", waited, sandboxZombieSettle)
	}
}

// One clean scan is not enough: a process that the next scan finds is
// stopped before the stop reports success.
func TestStopSandboxProcesses_TwoCleanScans_3375(t *testing.T) {
	f := &fakeSandbox{ignoreTerm: map[int]bool{}}
	f.install(t)
	scans := 0
	base := f.find()
	find := func(sig syscall.Signal) (sandboxScan, error) {
		scans++
		if scans == 2 {
			f.mu.Lock()
			f.running = append(f.running, 701)
			f.mu.Unlock()
		}
		return base(sig)
	}
	if err := stopSandboxProcesses("s", find, 5*time.Second, 0); err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if got := f.sent(); !slices.Contains(got, "701:killed") {
		t.Errorf("signals = %v, want 701 stopped", got)
	}
}

// The stop sends real signals: a real process in a session of its own that
// ignores SIGTERM and SIGHUP stops at SIGKILL.
func TestStopSandboxProcesses_KillsRealDetachedProcess_3375(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("/bin/sh", "-c", `trap "" TERM HUP; : > "$READY"; while :; do sleep 0.05; done`)
	cmd.Env = append(os.Environ(), "READY="+ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	// A SIGTERM before the trap would end the shell, and the test would
	// then pass with no SIGKILL.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the shell did not set its traps")
		}
	}

	runs := func() bool {
		select {
		case <-exited:
			return false
		default:
			return true
		}
	}
	find := newSandboxFinder(procProbe{
		list: func() ([]int, error) { return []int{pid}, nil },
		inSandbox: func(int) (bool, error) {
			if !runs() {
				return false, syscall.ESRCH
			}
			return true, nil
		},
		zombie: func(int) (bool, time.Time) { return false, time.Time{} },
	}, -1)
	// Once the shell is reaped, its PID is gone from a real list.
	listed := find
	find = func(sig syscall.Signal) (sandboxScan, error) {
		if !runs() {
			return sandboxScan{}, nil
		}
		return listed(sig)
	}
	if err := stopSandboxProcesses("s", find, 5*time.Second, 100*time.Millisecond); err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("the process still runs after the stop")
	}
}
