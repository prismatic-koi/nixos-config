package session

// StartSidecar launches a detached `prism sidecar` process alongside opencode.
//
// It derives the prism binary path via os.Executable, creates the log and PID
// directories under $XDG_STATE_HOME/prism/ (falling back to
// ~/.local/state/prism/), then starts the process detached (no tmux window).
//
// Log file : $XDG_STATE_HOME/prism/logs/<session>-sidecar.log
// PID file : $XDG_STATE_HOME/prism/run/<session>-sidecar.pid
//
// Returns an error only if the process could not be started. The caller
// (setupFullLayout) treats this as non-fatal and logs a warning.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// sidecarStateDir returns the $XDG_STATE_HOME/prism base directory.
func sidecarStateDir() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism"), nil
}

// SidecarLogPath returns the log file path for the named session's sidecar.
func SidecarLogPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "logs", sessionName+"-sidecar.log"), nil
}

// SidecarPIDPath returns the PID file path for the named session's sidecar.
func SidecarPIDPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", sessionName+"-sidecar.pid"), nil
}

// StartSidecar launches a detached `prism sidecar` process for the given
// session on the given port. It writes the sidecar PID to a PID file and
// redirects stdout/stderr to a log file.
//
// Returns an error if the process cannot be started. Callers should treat
// this as non-fatal — the spawn/restore still succeeds without the sidecar.
func StartSidecar(sessionName string, port int) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve prism binary: %w", err)
	}

	logPath, err := SidecarLogPath(sessionName)
	if err != nil {
		return err
	}
	pidPath, err := SidecarPIDPath(sessionName)
	if err != nil {
		return err
	}

	// Ensure log and run directories exist.
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}

	// Open (or create) the log file.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open sidecar log: %w", err)
	}
	// logFile is passed to the child process; close the parent's handle after
	// Start() hands it off to the child.
	defer logFile.Close()

	opencodeURL := fmt.Sprintf("http://localhost:%d", port)
	cmd := exec.Command(self, "sidecar",
		"--session", sessionName,
		"--opencode-url", opencodeURL,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// No Stdin — keep the child fully detached from the terminal.
	cmd.Stdin = nil
	// Start the child in its own session so it is fully detached from the
	// parent's controlling terminal and survives the spawning pane exiting.
	// Process.Release() alone is insufficient — it only drops the Go runtime's
	// handle to the process, but does not change the process's session/PGID.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sidecar: %w", err)
	}

	// Write PID file.
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		// Non-fatal: the process is running — log but don't stop it.
		fmt.Fprintf(os.Stderr, "warning: could not write sidecar PID file %s: %v\n", pidPath, err)
	}

	// Release the Go runtime's reference to the child process so the GC
	// finalizer does not call waitpid on the still-running sidecar. Without
	// this, the Go runtime would attempt to reap the child when the os.Process
	// object is garbage collected, which would conflict with a long-lived
	// detached process. (Setsid: true above handles the session/PGID detach.)
	if err := cmd.Process.Release(); err != nil {
		// This is cosmetic — the child is already running.
		fmt.Fprintf(os.Stderr, "warning: sidecar process release: %v\n", err)
	}

	return nil
}
