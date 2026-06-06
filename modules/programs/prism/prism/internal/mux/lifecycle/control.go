package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// StartOptions configures Start — the daemon-fork entry point used by
// `prismd mux start` (the human-on-the-CLI path, as distinct from the
// systemd / launchd path which calls Run directly via --foreground).
type StartOptions struct {
	// Self is the absolute path to the binary to re-exec. Pass
	// os.Executable() at the CLI layer. Empty is a programming
	// error (no fork target).
	Self string

	// ForegroundArgs is the argv to pass to the re-exec. The CLI
	// builds this as `["mux", "start", "--foreground"]` (plus any
	// path overrides set by the user). Empty is a programming error.
	ForegroundArgs []string

	// LogPath, when non-empty, is the file to which the daemon's
	// stdout / stderr are redirected. Empty means /dev/null. Errors
	// opening the file are returned from Start.
	LogPath string
}

// Start re-execs the current binary with --foreground and detaches.
// The child inherits no controlling terminal, no stdin, and (when
// LogPath is set) writes stdout/stderr to that file. On success the
// child's PID is returned. The caller then polls the PID file or the
// socket to confirm readiness; Start itself returns as soon as fork
// succeeds.
//
// The detach is implemented the same way internal/session/sidecar.go
// detaches the per-session sidecar: SysProcAttr.Setsid puts the child
// in its own session (so SIGHUP on the parent's controlling TTY does
// not propagate), and Process.Release drops Go's runtime handle so
// the GC finaliser does not try to reap the long-running child.
//
// Start does NOT itself probe ErrAlreadyRunning. It expects the
// caller to have already inspected LookupStatus and decided to
// proceed — pushing that branch up to the CLI keeps the user-facing
// error message ("already running, pid N") next to the code that
// formats it.
func Start(opts StartOptions) (int, error) {
	if opts.Self == "" {
		return 0, errors.New("lifecycle: Start: empty Self path")
	}
	if len(opts.ForegroundArgs) == 0 {
		return 0, errors.New("lifecycle: Start: empty ForegroundArgs")
	}

	// Resolve / open the log target before fork — we want any
	// permission / ENOSPC failure to surface as a Start error rather
	// than a silent daemon exit.
	var logFile *os.File
	if opts.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0o700); err != nil {
			return 0, fmt.Errorf("lifecycle: create log dir: %w", err)
		}
		f, err := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return 0, fmt.Errorf("lifecycle: open log file: %w", err)
		}
		logFile = f
	}
	closeLog := func() {
		if logFile != nil {
			_ = logFile.Close()
		}
	}

	cmd := exec.Command(opts.Self, opts.ForegroundArgs...)
	cmd.Stdin = nil
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	} else {
		devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			closeLog()
			return 0, fmt.Errorf("lifecycle: open /dev/null: %w", err)
		}
		defer devNull.Close()
		cmd.Stdout = devNull
		cmd.Stderr = devNull
	}
	// Pass the full environment so the child sees the same
	// XDG_STATE_HOME / PRISM_* the parent had. This is load-bearing
	// for the test suite (which seeds XDG_STATE_HOME to t.TempDir()
	// before invoking Start).
	cmd.Env = os.Environ()
	// Setsid: detach from the parent's session (and therefore from
	// any controlling TTY). The child becomes its own session leader,
	// which is exactly what a long-running daemon wants.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		closeLog()
		return 0, fmt.Errorf("lifecycle: start daemon: %w", err)
	}
	pid := cmd.Process.Pid
	// Drop the Go runtime's reference so the GC finaliser does not
	// try to reap the long-running child. This mirrors the
	// internal/session/sidecar.go detach pattern.
	if err := cmd.Process.Release(); err != nil {
		// Cosmetic — the child is already running. Surface a warning
		// via the returned error so the CLI can log it without
		// failing the start.
		closeLog()
		return pid, fmt.Errorf("lifecycle: release daemon process: %w", err)
	}
	closeLog()
	return pid, nil
}

// WaitForReady polls the PID file at path until either the file is
// present and identifies a live process, or deadline expires. Returns
// (pid, nil) when ready, (0, error) on timeout.
//
// Used by the CLI's `start` path to confirm the forked daemon came up
// successfully before printing "started" to the user. The poll
// interval is short (25 ms) so a fast startup feels instantaneous
// from the shell.
func WaitForReady(path string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		pid, err := readPIDFile(path)
		if err == nil && processAlive(pid) {
			return pid, nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				return 0, fmt.Errorf("lifecycle: pid %d in %s is not alive within %s", pid, path, timeout)
			}
			return 0, fmt.Errorf("lifecycle: daemon did not start within %s: %w", timeout, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// StopOptions configures Stop. The defaults match the issue text: 10 s
// graceful shutdown via SIGTERM, escalate to SIGKILL if the process is
// still alive at the deadline.
type StopOptions struct {
	// PIDPath is the path to the daemon's PID file. Empty means
	// DefaultPIDPath().
	PIDPath string

	// Grace is the SIGTERM-to-SIGKILL deadline. Zero means
	// DefaultStopGrace (10 s).
	Grace time.Duration
}

// Stop sends SIGTERM to the daemon identified by the PID file, waits
// for it to exit (polling at 25 ms intervals up to Grace), and
// escalates to SIGKILL if necessary. The PID file is removed
// unconditionally on the way out — a process that did not respond to
// SIGTERM and was SIGKILLed cannot have written its own final
// snapshot, but the next start will Restore from whatever the last
// periodic snapshot captured.
//
// Returned errors:
//
//   - nil if the daemon was not running (no PID file or stale file).
//   - A wrapped error from the SIGTERM / SIGKILL call when the
//     signal could not be delivered for a reason other than "process
//     is gone".
//   - A "did not exit within grace, SIGKILLed" error when escalation
//     was required. The caller is free to treat this as a success
//     (the process is gone) but the error surfaces so a recurring
//     stuck-shutdown signal is visible in the CLI exit code.
func Stop(opts StopOptions) error {
	pidPath := opts.PIDPath
	if pidPath == "" {
		var err error
		pidPath, err = DefaultPIDPath()
		if err != nil {
			return fmt.Errorf("lifecycle: stop: resolve pid path: %w", err)
		}
	}
	grace := opts.Grace
	if grace <= 0 {
		grace = DefaultStopGrace
	}

	pid, err := readPIDFile(pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Already stopped — not an error.
			return nil
		}
		if errors.Is(err, errInvalidPIDFile) {
			// Garbage PID file — remove and call it stopped.
			_ = os.Remove(pidPath)
			return nil
		}
		return fmt.Errorf("lifecycle: stop: read pid file: %w", err)
	}
	if !processAlive(pid) {
		// Stale PID file — clean up and return success.
		_ = os.Remove(pidPath)
		return nil
	}

	// SIGTERM. ESRCH means the process exited between the alive-check
	// and the signal — that's a race, not a failure.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("lifecycle: stop: send SIGTERM to pid %d: %w", pid, err)
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			_ = os.Remove(pidPath)
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Escalate to SIGKILL. We deliberately do not wait further: the
	// kernel will reap the process; the user already waited Grace
	// seconds. Surface the escalation as an error so it is visible.
	killErr := syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(pidPath)
	if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
		return fmt.Errorf("lifecycle: stop: pid %d did not exit within %s and SIGKILL failed: %w", pid, grace, killErr)
	}
	return fmt.Errorf("lifecycle: stop: pid %d did not exit within %s, SIGKILLed", pid, grace)
}
