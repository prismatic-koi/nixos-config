//go:build darwin

package cmd

// lifecycle_darwin.go — parent-death watcher for sandbox-exec sessions on macOS.
//
// sandbox-exec has no kernel-level "die with parent" equivalent (unlike Linux's
// PR_SET_PDEATHSIG or bwrap's --die-with-parent). This file provides the
// watchParentDeathAndKill function that installs a kqueue(2)-based watcher on
// the parent PID and, when the parent exits, sends SIGTERM followed by SIGKILL
// (after a 3-second grace period) to the child process.
//
// Fallback: if kqueue setup fails for any reason, watchParentDeathAndKill falls
// back to a 1-second heartbeat using kill(parentPID, 0) — non-fatal, and
// satisfies the same ≤5-second-exit bound per the AC.
//
// Security: the parent PID is captured once at startup via os.Getppid() by the
// caller (runAgentRunSandboxExec) before any goroutines or child processes are
// involved. It is never sourced from environment variables or user-controlled
// input. The child PID is the os.Process we spawned — not an external value.

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// watchParentDeathAndKill installs a parent-death watcher for the given child
// process. It blocks until either the parent (parentPID) exits or the child
// exits on its own (signalled by closing childExited). When the parent exits
// first, it sends SIGTERM to the child, waits up to graceTimeout, then sends
// SIGKILL if the child is still running.
//
// parentPID must be the PID of the process whose death should trigger child
// termination. It is captured once at startup (by the caller via os.Getppid())
// and must not originate from user-controlled input.
//
// childProc is the os.Process of the child launched by the caller. The caller
// guarantees this is the process it spawned — not an externally supplied PID.
//
// childExited is closed when the child has exited (caller closes it after
// cmd.Wait() returns). watchParentDeathAndKill returns early when childExited
// is closed so it does not send spurious signals to an already-dead process.
func watchParentDeathAndKill(parentPID int, childProc *os.Process, childExited <-chan struct{}, graceTimeout time.Duration) {
	if err := watchParentKqueue(parentPID, childProc, childExited, graceTimeout); err != nil {
		// kqueue setup or wait failed — log a warning and fall back to heartbeat.
		log.Printf("[agent-run] warning: kqueue parent-death watcher failed (%v) — falling back to 1-second heartbeat", err)
		watchParentHeartbeat(parentPID, childProc, childExited, graceTimeout)
	}
}

// watchParentKqueue implements the kqueue-based parent-death watcher.
// It creates a kqueue, registers an EVFILT_PROC/NOTE_EXIT kevent on parentPID,
// and waits for the parent to exit. On parent exit it kills the child.
// Returns an error if kqueue or kevent setup fails (triggering fallback).
func watchParentKqueue(parentPID int, childProc *os.Process, childExited <-chan struct{}, graceTimeout time.Duration) error {
	kq, err := syscall.Kqueue()
	if err != nil {
		return fmt.Errorf("kqueue: %w", err)
	}
	defer syscall.Close(kq) //nolint:errcheck

	// Register NOTE_EXIT on the parent PID. EV_ADD | EV_ONESHOT: add the
	// event and fire only once (exactly what we need — we only care about
	// the parent's first exit event).
	//
	// EVFILT_PROC with NOTE_EXIT fires when the process identified by
	// Kevent_t.Ident exits.
	change := syscall.Kevent_t{
		Ident:  uint64(parentPID),
		Filter: syscall.EVFILT_PROC,
		Flags:  syscall.EV_ADD | syscall.EV_ONESHOT,
		Fflags: syscall.NOTE_EXIT,
		Data:   0,
		Udata:  nil,
	}
	if _, err := syscall.Kevent(kq, []syscall.Kevent_t{change}, nil, nil); err != nil {
		return fmt.Errorf("kevent register: %w", err)
	}

	// Block on kevent(2) in a goroutine so we can also watch childExited via
	// a select. The goroutine sends nil on eventCh when NOTE_EXIT fires for
	// parentPID, or an error if kevent returns without a NOTE_EXIT event.
	// Closing kq (deferred above) unblocks the kevent call when childExited
	// fires first — the resulting error from the goroutine is then discarded.
	eventCh := make(chan error, 1)
	go func() {
		events := make([]syscall.Kevent_t, 1)
		n, err := syscall.Kevent(kq, nil, events, nil)
		if err != nil {
			eventCh <- fmt.Errorf("kevent wait: %w", err)
			return
		}
		if n > 0 && events[0].Fflags&syscall.NOTE_EXIT != 0 {
			eventCh <- nil // parent exited
			return
		}
		// Unexpected: kevent returned but no NOTE_EXIT flag.
		var fflags uint32
		if n > 0 {
			fflags = events[0].Fflags
		}
		eventCh <- fmt.Errorf("kevent: unexpected event (n=%d, fflags=0x%x)", n, fflags)
	}()

	select {
	case <-childExited:
		// Child exited on its own — nothing to do. The deferred Close(kq)
		// unblocks the kevent goroutine; its error result is discarded.
		return nil
	case err := <-eventCh:
		if err != nil {
			return err
		}
		// Parent exited — kill the child.
		killChild(childProc, graceTimeout)
		return nil
	}
}

// watchParentHeartbeat is the fallback liveness check used when kqueue setup
// fails. It polls kill(parentPID, 0) every second. When the parent is gone
// (ESRCH), it kills the child. Satisfies the ≤5-second-exit AC because the
// check fires within 1 second of parent death and killChild uses at most a
// 3-second grace before SIGKILL — worst-case total is ~4 seconds.
func watchParentHeartbeat(parentPID int, childProc *os.Process, childExited <-chan struct{}, graceTimeout time.Duration) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-childExited:
			// Child exited on its own — nothing to do.
			return
		case <-ticker.C:
			// kill(parentPID, 0) returns ESRCH when the process no longer exists.
			err := syscall.Kill(parentPID, 0)
			if err == syscall.ESRCH {
				// Parent is gone — kill the child.
				killChild(childProc, graceTimeout)
				return
			}
			// EPERM: process exists but we can't signal it — still alive.
			// nil or any other error: treat as "still alive" and continue.
		}
	}
}

// killChild sends SIGTERM to the child process, waits up to graceTimeout,
// then sends SIGKILL if the child has not exited. The child is identified by
// the os.Process the caller spawned — never an externally supplied PID.
func killChild(childProc *os.Process, graceTimeout time.Duration) {
	if childProc == nil {
		return
	}
	_ = childProc.Signal(syscall.SIGTERM)

	// Poll for child exit via kill(pid, 0) to avoid racing with cmd.Wait()
	// in the main goroutine (calling Wait() from two goroutines is not safe).
	// ESRCH indicates the process has exited and been reaped.
	exitedAfterTerm := make(chan struct{})
	go func() {
		defer close(exitedAfterTerm)
		for {
			time.Sleep(100 * time.Millisecond)
			if err := syscall.Kill(childProc.Pid, 0); err == syscall.ESRCH {
				return
			}
		}
	}()

	select {
	case <-exitedAfterTerm:
		// Child exited cleanly after SIGTERM — done.
	case <-time.After(graceTimeout):
		// Grace period expired — send SIGKILL.
		_ = childProc.Signal(syscall.SIGKILL)
	}
}

// forwardSignalsToSandboxExec forwards SIGTERM, SIGINT, and SIGHUP from
// agent-run to the sandbox-exec child process group until stopCh is closed.
// This is the sandbox-exec equivalent of forwardSignalsToBwrap in agent_run.go.
func forwardSignalsToSandboxExec(proc *os.Process, stopCh <-chan struct{}) {
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	for {
		select {
		case <-stopCh:
			return
		case sig := <-sigCh:
			if proc == nil {
				continue
			}
			// Send to the process group (negative PGID = group of sandbox-exec).
			_ = syscall.Kill(-proc.Pid, sig.(syscall.Signal))
		}
	}
}
