package session

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The agent of a sandbox-exec session runs in the sandbox of its profile,
// and so does each process that the agent starts. sandbox-exec has no PID
// namespace. A process that leaves the process group of the agent with
// setsid(2), or that a double fork gives to launchd, does not stop with
// the agent or with the tmux session (#3375). It keeps the rights of the
// sandbox, so it can still write the worktree and the git paths in .bare
// that the profile grants.
//
// The sandbox is a mark that such a process cannot remove. The kernel
// gives the sandbox of a process to each child, across exec and setsid. A
// process in a sandbox cannot leave it, and cannot apply a second one. So
// the stop of a session ends each process whose sandbox is the sandbox of
// the session.

// SandboxProcessStopTimeout is the time that StopSandboxProcesses waits
// for the processes in the sandbox of a session to exit.
const SandboxProcessStopTimeout = 10 * time.Second

// sandboxProcessTermGrace is the time from the first SIGTERM to the first
// SIGKILL. The tmux kill sends SIGHUP to the agent just before the stop,
// so the agent can still finish its exit.
const sandboxProcessTermGrace = 2 * time.Second

const sandboxProcessPoll = 25 * time.Millisecond

// sandboxCleanScans is the number of scans in a row that must find no
// process before the stop reports success. One clean scan is proof (see
// newSandboxFinder). The second one costs one poll.
const sandboxCleanScans = 2

// sandboxZombieSettle is the time that a zombie must stay a zombie before
// a scan ignores it. A process of the session that exits before the scan
// checks it is a zombie until its parent reaps it, and its child can then
// be missing from the list. launchd reaps an orphan at once, so a zombie
// that stays is a zombie of a parent that runs, and the scan checks that
// parent.
const sandboxZombieSettle = 500 * time.Millisecond

// SandboxProcessError reports a process in the sandbox of a session that
// prism cannot end, or a scan that cannot prove that no such process runs.
type SandboxProcessError struct {
	Session string
	// PIDs are the processes in the sandbox that still run at the timeout,
	// or the process whose sandbox check failed.
	PIDs []int
	// Unclear are the processes that exited before the last scan could
	// check their sandbox.
	Unclear []int
	// Err is the error of the list or of the sandbox check, if any.
	Err error
}

func (e *SandboxProcessError) Error() string {
	if e.Err != nil {
		if len(e.PIDs) == 0 {
			return fmt.Sprintf("prism cannot list the processes in the sandbox of session %q: %v", e.Session, e.Err)
		}
		return fmt.Sprintf("prism cannot check the sandbox of %s for session %q: %v", pidList(e.PIDs), e.Session, e.Err)
	}
	var parts []string
	if len(e.PIDs) > 0 {
		parts = append(parts, fmt.Sprintf("%s in the sandbox of session %q still %s after SIGKILL", pidList(e.PIDs), e.Session, runVerb(e.PIDs)))
	}
	if len(e.Unclear) > 0 {
		parts = append(parts, fmt.Sprintf("%s exited before prism could check %s sandbox. A process of session %q can start a new process and exit faster than prism can check it", pidList(e.Unclear), possessive(e.Unclear), e.Session))
	}
	return strings.Join(parts, ". ") + ". Stop each process of the session, then try again"
}

func (e *SandboxProcessError) Unwrap() error { return e.Err }

func pidList(pids []int) string {
	ids := make([]string, len(pids))
	for i, pid := range pids {
		ids[i] = strconv.Itoa(pid)
	}
	if len(ids) == 1 {
		return "process " + ids[0]
	}
	return "processes " + strings.Join(ids, ", ")
}

func runVerb(pids []int) string {
	if len(pids) == 1 {
		return "runs"
	}
	return "run"
}

func possessive(pids []int) string {
	if len(pids) == 1 {
		return "its"
	}
	return "their"
}

// StopSandboxProcesses ends each process that runs in the sandbox of
// sessionName (#3375). It sends SIGTERM, and SIGKILL after a grace of 2
// seconds, until two scans in a row find no such process. It returns a
// *SandboxProcessError at the timeout, which names each process that still
// runs and each process that the last scan could not check. It also
// returns one when it cannot list the processes or check a sandbox,
// because prism then cannot prove that no process of the session runs.
//
// Call it after the tmux kill of the session: it also ends the agent. On
// Linux it does nothing, because bwrap runs the agent in a PID namespace of
// its own, and the kernel ends each process of the namespace with it.
func StopSandboxProcesses(sessionName string, timeout time.Duration) error {
	find, err := platformSandboxFinder(sessionName)
	if err != nil {
		return &SandboxProcessError{Session: sessionName, Err: err}
	}
	if find == nil {
		return nil
	}
	return stopSandboxProcesses(sessionName, find, timeout, sandboxProcessTermGrace)
}

// signalSandboxProcess sends sig to pid. Tests replace it.
var signalSandboxProcess = func(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// sandboxScan is the result of one scan.
type sandboxScan struct {
	// found are the processes in the sandbox. The scan sent each one the
	// signal when it found it.
	found []int
	// unclear are the processes of the list that exited before the scan
	// could check them, and the zombies that are not settled.
	unclear []int
}

// sandboxFinder runs one scan and sends sig to each process that it finds.
type sandboxFinder func(sig syscall.Signal) (sandboxScan, error)

// procProbe is the kernel access of a scan. Tests replace each function.
type procProbe struct {
	// list returns the PID of each process of this user, zombies
	// included, from one list that the kernel makes under its process
	// list lock.
	list func() ([]int, error)
	// inSandbox reports whether pid runs in the sandbox of the session.
	// The error wraps syscall.ESRCH when pid exited or is a zombie.
	inSandbox func(pid int) (bool, error)
	// zombie reports whether pid is a zombie, and its start time.
	zombie func(pid int) (isZombie bool, start time.Time)
}

type zombieKey struct {
	pid   int
	start time.Time
}

// newSandboxFinder returns a finder that scans with p. self is the PID of
// this process, which the scan skips.
//
// One scan with no found and no unclear process proves that no process of
// the sandbox runs. The list is atomic, so each process of the sandbox
// that runs at the list is in it. The scan finds it, or the process exits
// first and is unclear. A process of the sandbox that starts after the
// list has a parent in the sandbox that ran at the list, because only a
// process in the sandbox starts a process in it.
func newSandboxFinder(p procProbe, self int) sandboxFinder {
	zombieSince := map[zombieKey]time.Time{}
	return func(sig syscall.Signal) (sandboxScan, error) {
		pids, err := p.list()
		if err != nil {
			return sandboxScan{}, err
		}
		var s sandboxScan
		now := time.Now()
		for _, pid := range pids {
			if pid <= 1 || pid == self {
				continue
			}
			in, err := p.inSandbox(pid)
			switch {
			case err == nil && in:
				// Signal at once: a scan that waits gives the process
				// time to start a child.
				_ = signalSandboxProcess(pid, sig)
				s.found = append(s.found, pid)
			case err == nil:
			case errors.Is(err, syscall.ESRCH):
				if isZombie, start := p.zombie(pid); isZombie {
					k := zombieKey{pid, start}
					since, seen := zombieSince[k]
					if !seen {
						zombieSince[k] = now
						since = now
					}
					if now.Sub(since) >= sandboxZombieSettle {
						continue
					}
				}
				s.unclear = append(s.unclear, pid)
			default:
				return sandboxScan{}, &SandboxProcessError{PIDs: []int{pid}, Err: err}
			}
		}
		return s, nil
	}
}

// stopSandboxProcesses is the body of StopSandboxProcesses.
func stopSandboxProcesses(sessionName string, find sandboxFinder, timeout, grace time.Duration) error {
	start := time.Now()
	clean := 0
	for {
		elapsed := time.Since(start)
		sig := syscall.SIGTERM
		if elapsed >= grace {
			sig = syscall.SIGKILL
		}
		scan, err := find(sig)
		if err != nil {
			var se *SandboxProcessError
			if errors.As(err, &se) {
				se.Session = sessionName
				return se
			}
			return &SandboxProcessError{Session: sessionName, Err: err}
		}
		if len(scan.found) == 0 && len(scan.unclear) == 0 {
			clean++
			if clean >= sandboxCleanScans {
				return nil
			}
		} else {
			clean = 0
			if elapsed >= timeout {
				return &SandboxProcessError{Session: sessionName, PIDs: scan.found, Unclear: scan.unclear}
			}
		}
		time.Sleep(sandboxProcessPoll)
	}
}
