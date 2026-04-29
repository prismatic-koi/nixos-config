// Package container manages the podman container lifecycle for prism sidecar.
// This file defines the shared gracefulShutdown helper used by the bwrap and
// sandbox-exec Isolator.Shutdown implementations (issue #1149 A2.GR; design
// proposal A2 §3.7).
//
// Both bwrap and sandbox-exec sessions are supervised children rather than
// long-lived container resources (only podman owns a container that survives
// process death). When Manager.Shutdown is invoked on a bwrap or sandbox-exec
// session, the polite teardown shape is the same: SIGTERM → grace period →
// SIGKILL. Pre-A2.GR this body lived inline inside bwrapIsolator.Shutdown;
// sandboxExecIsolator.Shutdown was a no-op.
//
// Podman's Shutdown stays separate (must-differ per A2 §3.7) because the
// container's lifecycle is driven by `podman stop --time 10` followed by
// `podman rm --force`, not by direct signal delivery.
package container

import (
	"os/exec"
	"syscall"
	"time"
)

// defaultGracefulShutdownGrace is the SIGTERM-to-SIGKILL grace period used
// by bwrap and sandbox-exec when no caller-supplied value is provided. The
// value mirrors the original bwrap.Shutdown timeout (30 seconds).
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
// cases (matching the pre-A2.GR bwrapIsolator.Shutdown behaviour).
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
