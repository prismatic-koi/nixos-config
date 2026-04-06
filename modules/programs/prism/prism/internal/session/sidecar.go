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
	"runtime"
	"strconv"
	"strings"
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

// SidecarReadyPath returns the readiness signal file path for the named session.
// The sidecar creates this file after the container is healthy. The tmux pane
// startup script polls for its existence before running "opencode attach".
//
// Ready file: $XDG_STATE_HOME/prism/run/<session>-sidecar.ready
func SidecarReadyPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", sessionName+"-sidecar.ready"), nil
}

// SidecarSessionPath returns the path to the file where the sidecar writes the
// opencode session ID after creating it via prompt delivery (#487). The tmux
// pane startup script reads this file (if present) and passes -s <sid> to
// "opencode attach" so it opens directly into the agent's session.
//
// Session file: $XDG_STATE_HOME/prism/run/<session>-sidecar.sid
func SidecarSessionPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", sessionName+"-sidecar.sid"), nil
}

// SidecarPIDPath returns the PID file path for the named session's sidecar.
func SidecarPIDPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", sessionName+"-sidecar.pid"), nil
}

// FindSidecarPID scans running processes to find the sidecar for sessionName
// by matching its command-line arguments. It looks for a process whose argument
// list contains both "sidecar" and "--session" followed by sessionName.
//
// On Linux, /proc/<pid>/cmdline is scanned for efficiency. On other platforms
// (macOS), "ps aux" is used as a fallback.
//
// Returns the PID of the matching process, or 0 if not found. Returns 0 without
// error if the scan is not supported on the current platform.
func FindSidecarPID(sessionName string) int {
	if runtime.GOOS == "linux" {
		return findSidecarPIDLinux(sessionName)
	}
	return findSidecarPIDPS(sessionName)
}

// findSidecarPIDLinux scans /proc/<pid>/cmdline entries to find the sidecar
// process. Only processes owned by the current user are inspected (others
// would be unreadable and return permission errors).
func findSidecarPIDLinux(sessionName string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // not a PID directory
		}
		cmdlineData, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue // process may have exited or is unreadable
		}
		if isSidecarCmdline(string(cmdlineData), sessionName) {
			return pid
		}
	}
	return 0
}

// findSidecarPIDPS uses "ps aux" to find the sidecar process.  This is the
// fallback used on macOS and other non-Linux platforms.
func findSidecarPIDPS(sessionName string) int {
	out, err := exec.Command("ps", "aux").Output()
	if err != nil {
		return 0
	}
	// Each line: USER PID %CPU %MEM VSZ RSS TT STAT STARTED TIME COMMAND...
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "sidecar") {
			continue
		}
		if !strings.Contains(line, "--session") {
			continue
		}
		if !strings.Contains(line, sessionName) {
			continue
		}
		// Parse the PID from column 1 (0-indexed).
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		// Verify it looks like a prism sidecar (not a grep or unrelated match).
		// Reconstruct the command part (fields[10:]) and do a precise check.
		if len(fields) > 10 {
			cmdPart := strings.Join(fields[10:], " ")
			if isSidecarCmdline(cmdPart, sessionName) {
				return pid
			}
		}
	}
	return 0
}

// isSidecarCmdline returns true when the raw cmdline string (NUL-separated on
// Linux, space-separated on macOS) contains both the "sidecar" subcommand and
// "--session <sessionName>" as consecutive tokens.
//
// The raw Linux cmdline bytes use NUL as the argument separator. Replacing NUL
// with a space gives a space-separated string we can work with uniformly.
//
// Both "sidecar" and the session name are matched as exact tokens — never as
// substrings. This prevents false positives when a session name itself contains
// the word "sidecar" (e.g. repo@fix-cleanup-orphaned-sidecar).
func isSidecarCmdline(cmdline, sessionName string) bool {
	// Normalise NUL-separators (Linux /proc/<pid>/cmdline) to spaces.
	normalised := strings.ReplaceAll(cmdline, "\x00", " ")
	tokens := strings.Fields(normalised)
	// Require "sidecar" as an exact token (the prism subcommand), not a
	// substring. A session name like "repo@fix-sidecar-bug" must not qualify.
	hasSidecar := false
	for _, tok := range tokens {
		if tok == "sidecar" {
			hasSidecar = true
			break
		}
	}
	if !hasSidecar {
		return false
	}
	// Check for "--session <sessionName>" as adjacent tokens.
	for i, tok := range tokens {
		if tok == "--session" && i+1 < len(tokens) && tokens[i+1] == sessionName {
			return true
		}
	}
	return false
}

// KillSidecar reads the PID file for the named session, sends SIGTERM to the
// recorded process, and removes the PID file. It handles missing or stale PID
// files gracefully — no error is returned in those cases.
//
// When no PID file is found, KillSidecar falls back to FindSidecarPID to
// locate an orphaned sidecar process by its command-line arguments. This
// handles the case where the PID file was lost (e.g. sidecar crashed before
// writing it, or the file was manually removed).
//
// This function is safe to call from test teardown: if StartSidecar was never
// reached (e.g. the test failed before the session was created) the missing PID
// file is treated as a no-op.  If the process has already exited (ESRCH), the
// stale PID file is removed without attempting the kill.  If the PID has been
// recycled to an unrelated process (/proc/<pid>/cmdline does not contain
// "prism"), the kill is skipped and the file is removed.
func KillSidecar(sessionName string) {
	pidPath, err := SidecarPIDPath(sessionName)
	if err != nil {
		// Can't derive the path — nothing to clean up.
		return
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		// PID file absent — try to find an orphaned sidecar by process args.
		if pid := FindSidecarPID(sessionName); pid != 0 {
			fmt.Fprintf(os.Stderr, "[prism] no PID file for session %q — found orphaned sidecar pid %d via process scan, sending SIGTERM\n", sessionName, pid)
			if killErr := syscall.Kill(pid, syscall.SIGTERM); killErr != nil && killErr != syscall.ESRCH {
				fmt.Fprintf(os.Stderr, "warning: kill orphaned sidecar pid %d: %v\n", pid, killErr)
			}
		}
		return
	}

	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		// Corrupt PID file — remove it and move on.
		_ = os.Remove(pidPath)
		return
	}

	// Guard against same-user PID recycling: verify the PID belongs to a
	// prism process by checking /proc/<pid>/cmdline before sending SIGTERM.
	// This is Linux-specific (codebase targets NixOS), so if the file is
	// unreadable (e.g. the process is already gone) we fall through and let
	// the subsequent Kill handle ESRCH gracefully.
	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		if !strings.Contains(string(cmdline), "prism") {
			// PID has been recycled to an unrelated process — do not kill it.
			fmt.Fprintf(os.Stderr, "warning: sidecar pid %d does not appear to be a prism process — skipping kill\n", pid)
			_ = os.Remove(pidPath)
			return
		}
	}

	// Send SIGTERM; ignore ESRCH (no such process — already gone).
	// The PID file is removed unconditionally after the kill attempt.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "warning: kill sidecar pid %d: %v\n", pid, err)
	}

	_ = os.Remove(pidPath)
}

// StartSidecarOpts holds optional parameters for launching a sidecar process.
type StartSidecarOpts struct {
	// Port is the allocated opencode serve port.
	Port int
	// ContainerMode, when true, passes --container to the sidecar so it creates
	// and manages a podman container.
	ContainerMode bool
	// AgentRole is "worker" or "coordinator". Passed via --agent-role when in
	// container mode to select the appropriate credential set.
	AgentRole string
	// Worktree is the absolute path to the git worktree. Used as PRISM_WORKTREE
	// in the sidecar's environment so it can mount the correct directory.
	Worktree string
	// PluginHostPath is the host path to the prism-hooks.ts plugin file.
	// Passed via --plugin-path in container mode.
	PluginHostPath string
	// InitialPrompt is the spawn prompt to deliver to the agent after container
	// readiness. Passed via --initial-prompt in container mode (#487).
	// Empty string means no prompt delivery.
	InitialPrompt string
}

// StartSidecar launches a detached `prism sidecar` process for the given
// session on the given port. It writes the sidecar PID to a PID file and
// redirects stdout/stderr to a log file.
//
// Returns an error if the process cannot be started. Callers should treat
// this as non-fatal — the spawn/restore still succeeds without the sidecar.
func StartSidecar(sessionName string, port int) error {
	return StartSidecarWithOpts(sessionName, StartSidecarOpts{Port: port})
}

// StartSidecarWithOpts launches a detached `prism sidecar` process with full
// option control. It writes the sidecar PID to a PID file and redirects
// stdout/stderr to a log file.
func StartSidecarWithOpts(sessionName string, opts StartSidecarOpts) error {
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

	opencodeURL := fmt.Sprintf("http://localhost:%d", opts.Port)

	// Build the sidecar command arguments.
	cmdArgs := []string{"sidecar",
		"--session", sessionName,
		"--opencode-url", opencodeURL,
	}

	if opts.ContainerMode {
		cmdArgs = append(cmdArgs, "--container")
		cmdArgs = append(cmdArgs, "--port", strconv.Itoa(opts.Port))
		if opts.AgentRole != "" {
			cmdArgs = append(cmdArgs, "--agent-role", opts.AgentRole)
		}
		if opts.PluginHostPath != "" {
			cmdArgs = append(cmdArgs, "--plugin-path", opts.PluginHostPath)
		}
		if opts.InitialPrompt != "" {
			cmdArgs = append(cmdArgs, "--initial-prompt", opts.InitialPrompt)
		}
	}

	cmd := exec.Command(self, cmdArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// No Stdin — keep the child fully detached from the terminal.
	cmd.Stdin = nil

	// Inject PRISM_WORKTREE so the sidecar can resolve the worktree path.
	// Inherit the full environment and add/override PRISM_WORKTREE.
	env := os.Environ()
	if opts.Worktree != "" {
		env = append(env, "PRISM_WORKTREE="+opts.Worktree)
	}
	cmd.Env = env

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
