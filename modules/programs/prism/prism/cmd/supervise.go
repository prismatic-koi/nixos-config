package cmd

// supervise.go — shared signal/PTY supervisor for bwrap and sandbox-exec
// agent-run paths (issue #1149 A2.SUP; design proposal A2 §3.6).
//
// The bwrap path (Linux) and the sandbox-exec path (Darwin) both run the
// sandbox launcher as a supervised child of agent-run, which itself is the
// tmux pane's child. Both share three concerns:
//
//  1. Hand the terminal foreground to the child's process group so
//     keypresses and SIGWINCH route to the child rather than agent-run.
//  2. Forward SIGTERM/SIGINT/SIGHUP (and optionally SIGWINCH) to the child
//     process group while the child is alive.
//  3. cmd.Wait() and surface the exit code.
//
// Pre-A2.SUP these steps were inlined in each per-mode dispatch path with
// minor variations (bwrap forwards SIGWINCH; sandbox-exec does not). This
// helper unifies them behind one entry point with a small options struct
// that captures the per-mode-specific knobs without losing the existing
// behaviour of either path.
//
// Note on platform: tcsetpgrpForeground/tcsetpgrpRestore use TIOCGPGRP/
// TIOCSPGRP via syscall.SYS_IOCTL, which is identical on Linux and Darwin.
// The supervisor itself is platform-agnostic; callers that need additional
// platform-specific lifecycle (e.g. the kqueue parent-death watcher on
// Darwin, see lifecycle_darwin.go) wire that in separately.

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// SuperviseOpts carries the per-call inputs that SuperviseChild consumes.
//
// ForwardWinch controls whether SIGWINCH is forwarded to the child's process
// group. The bwrap path sets this to true (matching the pre-A2.SUP
// forwardSignalsToBwrap behaviour). The sandbox-exec path sets it to false
// (matching the pre-A2.SUP forwardSignalsToSandboxExec behaviour, which did
// not subscribe SIGWINCH at all). The audit (A2 §3.6) flags this divergence
// as `[uncertain]` — sandbox-exec on Darwin may need SIGWINCH forwarding for
// Bubble Tea's TIOCGWINSZ-on-stdout requirement; until that is observed in
// practice we preserve each path's pre-A2.SUP behaviour.
//
// OnWinch, when non-nil, is invoked on each SIGWINCH the supervisor receives.
// Currently both call sites pass nil; the field is wired through so a future
// caller (e.g. a slave-PTY resize) can register a callback without re-shaping
// the helper signature.
type SuperviseOpts struct {
	// ForwardWinch enables SIGWINCH subscription and forwarding to the
	// child process group. When false, SIGWINCH is not subscribed by the
	// supervisor at all — the kernel still delivers it directly to the
	// foreground process group (which is the child's after
	// tcsetpgrpForeground), so the child sees window resizes regardless;
	// this flag only controls whether agent-run also observes them.
	ForwardWinch bool

	// OnWinch, when non-nil, is called once per received SIGWINCH (only
	// when ForwardWinch is true — without subscription the supervisor
	// never receives SIGWINCH and cannot invoke the callback).
	OnWinch func()
}

// SuperviseChild runs cmd as the foreground process group on stdinFd. The
// caller has already started the process (cmd.Start) with
// cmd.SysProcAttr.Setpgid = true, so the child has its own process group
// that the supervisor can target with syscall.Kill(-pid, sig).
//
// The supervisor performs three steps in order:
//
//  1. Hand the terminal foreground to the child's pgid via tcsetpgrp on
//     stdinFd. The original foreground pgid is captured for later restore.
//
//  2. Subscribe SIGTERM/SIGINT/SIGHUP (and SIGWINCH when opts.ForwardWinch
//     is true) and forward each to the child's pgid for as long as the
//     child is alive. The forwarding goroutine exits when stopCh is closed
//     by the deferred cleanup below.
//
//  3. Block on cmd.Wait() and capture its error. After Wait returns, the
//     supervisor closes stopCh, restores the original foreground pgid via
//     tcsetpgrp, and returns the wait error verbatim. Callers handle
//     ExitError unwrapping themselves.
//
// On platforms where tcsetpgrpForeground returns 0 (non-TTY stdinFd, or
// non-interactive contexts such as tests), the foreground-restoration step
// is silently skipped — matching the pre-A2.SUP behaviour at the bwrap call
// site.
func SuperviseChild(cmd *exec.Cmd, stdinFd int, opts SuperviseOpts) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	// 1. Foreground the child's process group.
	origPgid := tcsetpgrpForeground(stdinFd, cmd.Process.Pid)

	// 2. Forward signals until stopCh closes.
	stopCh := make(chan struct{})
	go superviseForwardSignals(cmd.Process, stopCh, opts)

	// 3. Wait for the child to exit, then signal the forwarder to stop.
	waitErr := cmd.Wait()
	close(stopCh)

	if origPgid > 0 {
		_ = tcsetpgrpRestore(stdinFd, origPgid)
	}
	return waitErr
}

// superviseForwardSignals subscribes the SIGTERM/SIGINT/SIGHUP set (plus
// SIGWINCH when opts.ForwardWinch is true) and forwards each received
// signal to the child's process group via syscall.Kill(-pid, sig). It exits
// when stopCh is closed.
func superviseForwardSignals(proc *os.Process, stopCh <-chan struct{}, opts SuperviseOpts) {
	sigCh := make(chan os.Signal, 4)
	signals := []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP}
	if opts.ForwardWinch {
		signals = append(signals, syscall.SIGWINCH)
	}
	signal.Notify(sigCh, signals...)
	defer signal.Stop(sigCh)

	for {
		select {
		case <-stopCh:
			return
		case sig := <-sigCh:
			if proc == nil {
				continue
			}
			if sig == syscall.SIGWINCH {
				if opts.OnWinch != nil {
					opts.OnWinch()
				}
				_ = syscall.Kill(-proc.Pid, syscall.SIGWINCH)
				continue
			}
			_ = syscall.Kill(-proc.Pid, sig.(syscall.Signal))
		}
	}
}
