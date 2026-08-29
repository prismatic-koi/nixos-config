// Package container manages sandbox lifecycle and mount preparation for
// prism agent sessions.
// This file defines the shared gracefulShutdown helper used by the bwrap and
// sandbox-exec Isolator.Shutdown implementations.
//
// Both bwrap and sandbox-exec sessions are supervised children rather than
// long-lived container resources. When Manager.Shutdown is invoked on a bwrap
// or sandbox-exec
// session, the polite teardown shape is the same: SIGTERM → grace period →
// SIGKILL.
package container

import (
	"os/exec"
	"syscall"
	"time"
)

// defaultGracefulShutdownGrace is the SIGTERM-to-SIGKILL grace period used
// by bwrap and sandbox-exec when no caller-supplied value is provided
// (30 seconds).
const defaultGracefulShutdownGrace = 30 * time.Second

// gracefulShutdown sends SIGTERM to cmd's process, waits up to gracePeriod
// for it to exit, and sends SIGKILL if the process has not exited within
// the grace window. It is the shared body for the bwrap and sandbox-exec
// Isolator.Shutdown implementations.
//
// Concurrency contract: gracefulShutdown calls cmd.Wait() in its own
// internal goroutine. Callers must not also call cmd.Wait() — doing so
// would race the helper's wait observation. The bwrap and sandbox-exec
// production paths that own a Wait() call (e.g. cmd/agent_run.go,
// cmd/agent_run_sandbox_exec_darwin.go) drive shutdown via their own
// signal-handling rather than calling this helper; this helper is reached
// only via the Isolator.Shutdown() public API, which is invoked from the
// Manager.Shutdown path that does not own a concurrent Wait.
//
// gracePeriod ≤ 0 falls back to defaultGracefulShutdownGrace.
//
// cmd may be nil or have a nil Process — the helper is a no-op in those
// cases.
func gracefulShutdown(cmd *exec.Cmd, gracePeriod time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if gracePeriod <= 0 {
		gracePeriod = defaultGracefulShutdownGrace
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd.Wait()
	}()

	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(gracePeriod):
		_ = cmd.Process.Kill()
		<-done
	}
}
