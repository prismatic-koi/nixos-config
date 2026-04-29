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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

// sessionDirHashLen is the number of hex characters used as the per-session
// run/ subdirectory name. 12 hex chars = 6 bytes = 48 bits of entropy from
// SHA-256, more than enough to disambiguate the small set of concurrent
// session names a single user runs (collision probability ≈ N²/2^49).
//
// The hashed directory name keeps the host-API socket path under sun_path
// budgets on every platform: with the default state dir
// "$HOME/.local/state/prism/run/" + 12 hex + "/hostapi.sock", the path is
// roughly 58 bytes — comfortably under Darwin's 104-byte limit and Linux's
// 108-byte limit even when $HOME is unusually long. See issue #1050 for the
// full path arithmetic and the regression test in this package.
const sessionDirHashLen = 12

// SessionDirName returns the deterministic per-session directory name used
// under $XDG_STATE_HOME/prism/run/ for files that must be co-located with the
// host-API Unix socket (currently hostapi.sock and agent-run.log).
//
// The directory name is the first 12 hex characters of SHA-256(sessionName).
// This keeps the resulting socket path under sun_path budgets on every
// platform regardless of how long the session name itself is — see #1050.
//
// The mapping is pure and deterministic, so cleanup, debugging
// (`prism logs <session>`), and bwrap/podman bind-mount construction can all
// re-derive the directory from the session name without any persisted lookup.
func SessionDirName(sessionName string) string {
	sum := sha256.Sum256([]byte(sessionName))
	return hex.EncodeToString(sum[:])[:sessionDirHashLen]
}

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
// startup script polls for its existence before running "podman attach".
//
// Ready file: $XDG_STATE_HOME/prism/run/<session>-sidecar.ready
func SidecarReadyPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", sessionName+"-sidecar.ready"), nil
}

// SidecarPIDPath returns the PID file path for the named session's sidecar.
func SidecarPIDPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", sessionName+"-sidecar.pid"), nil
}

// SidecarHostAPIPath returns the Unix socket path for the session's host-API server.
// Each session gets its own subdirectory under run/ so that the podman container can
// mount only that directory — providing socket isolation without exposing other
// sessions' sockets (security fix #960). The subdirectory is pre-created by
// container.prepareVolumeDirs before podman run, so the directory already exists
// when podman evaluates the bind-mount, even though the socket file inside it is
// created later by the sidecar (after the container becomes healthy).
//
// The directory name is a 12-hex-char SHA-256 prefix of the session name (see
// SessionDirName) rather than the session name itself. This keeps the resulting
// socket path under the platform sun_path limits — historically the full session
// name (e.g. "<repo>@<long-branch>~review-N-review-<role>") could push the path
// past Linux's 108-byte and Darwin's 104-byte budgets, causing bind(2) to fail
// with EINVAL. See #1050 for the path arithmetic and regression test.
//
// Socket path: $XDG_STATE_HOME/prism/run/<12-hex-of-sha256(session)>/hostapi.sock
func SidecarHostAPIPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", SessionDirName(sessionName), "hostapi.sock"), nil
}

// AgentRunLogPath returns the agent-run log file path for the named session.
// The log captures the stdout and stderr of the bwrap sandbox (and the opencode
// harness running inside it) for the lifetime of the session. It lives alongside
// hostapi.sock in the per-session run directory so that the directory is already
// created by the time agent-run opens the file (the sidecar pre-creates it via
// container.prepareVolumeDirs, and agent-run falls back to creating it if needed).
//
// The directory name is the same SessionDirName-derived 12-hex prefix used by
// SidecarHostAPIPath, so the log and the socket always live in the same
// per-session directory regardless of how long the session name is.
//
// Log path: $XDG_STATE_HOME/prism/run/<12-hex-of-sha256(session)>/agent-run.log
func AgentRunLogPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", SessionDirName(sessionName), "agent-run.log"), nil
}

// KillSidecar reads the PID file for the named session, sends SIGTERM to the
// recorded process, and removes the PID file. It handles missing or stale PID
// files gracefully — no error is returned in those cases.
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
		// PID file absent — sidecar was never started or already cleaned up.
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
	// prism sidecar process by checking /proc/<pid>/cmdline before sending
	// SIGTERM. This is Linux-specific (codebase targets NixOS), so if the
	// file is unreadable (e.g. the process is already gone) we fall through
	// and let the subsequent Kill handle ESRCH gracefully.
	//
	// The check matches the invariant argv shape of a prism-spawned sidecar:
	//
	//   <binary> sidecar --session <name> ...
	//
	// We require both "sidecar" and "--session" to appear in the cmdline.
	// A previous implementation checked for "prism" in the binary path, but
	// that produced false negatives under `go test`, where the sidecar is
	// spawned by re-invoking the test binary (e.g. "cmd.test sidecar …") —
	// the cmdline does not contain "prism" at all, so KillSidecar skipped
	// the kill and sidecars leaked after every test run.
	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		cmdlineStr := string(cmdline)
		if !strings.Contains(cmdlineStr, "sidecar") || !strings.Contains(cmdlineStr, "--session") {
			// PID has been recycled to an unrelated process — do not kill it.
			fmt.Fprintf(os.Stderr, "warning: sidecar pid %d does not appear to be a prism sidecar process — skipping kill\n", pid)
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
	// IsolationMode is the resolved isolation mode for this session. When set,
	// it is passed to the sidecar via --isolation-mode so the sidecar can
	// branch on it (e.g. skip container creation for "bwrap", "sandbox-exec",
	// and "host"). Valid values: "podman", "bwrap", "sandbox-exec", "host".
	IsolationMode string
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
	// ConfigContent is the JSON blob for the container's opencode.json config
	// file. When non-empty and IsolationMode is "podman", it is forwarded to
	// the sidecar via --config-content so the container can write it to a
	// temp file and mount it at /root/.config/opencode/opencode.json.
	//
	// In host/bwrap/sandbox-exec mode, OPENCODE_CONFIG_CONTENT is injected
	// directly by buildDirectOpencodeCmd (prepended to the opencode shell
	// command) and does not need to go through the sidecar.
	ConfigContent string
	// InstanceID is the UUID instance identifier for this session incarnation.
	// When non-empty, it is passed to the sidecar via --instance-id so that
	// the sidecar can use it for container labels and bus message scoping
	// without needing to read it back from the DB (which would race with the
	// tmux-session-start event that writes instance_id to agent_status).
	InstanceID string
	// WorktreeReadOnly, when true, mounts the worktree read-only inside the
	// container. Set for review agent containers so they cannot modify the
	// branch under review. Passed via --worktree-readonly to the sidecar.
	WorktreeReadOnly bool
	// HarnessName is the registered harness name (e.g. "opencode"). When
	// non-empty it is forwarded to the sidecar via --harness so the sidecar
	// can call harness.ShapeOf to determine its own transport shape. When
	// empty, the sidecar defaults to "opencode".
	HarnessName string
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

	// Pass --isolation-mode when set; the sidecar uses this to branch on
	// container creation, harness selection, and host-API socket setup.
	// The flag survives the registry move because the spawned sidecar still
	// needs to look up its own isolator after re-exec.
	if opts.IsolationMode != "" {
		cmdArgs = append(cmdArgs, "--isolation-mode", opts.IsolationMode)
	}

	// D5 (issue #1133): the per-mode argv branches collapse into a single
	// Isolator.SidecarFlags dispatch.
	//
	//   - podman:               --container --port --agent-role --plugin-path …
	//   - bwrap, sandbox-exec:  --port --agent-role --plugin-path …  (no --container)
	//   - host:                 nil (the sidecar is not started for host)
	//
	// The pre-refactor branch lived at internal/session/sidecar.go:317-352.
	if opts.IsolationMode != "" {
		iso, isoErr := container.For(config.IsolationMode(opts.IsolationMode), container.ConstructorOpts{Name: sessionName})
		if isoErr == nil {
			cmdArgs = append(cmdArgs, iso.SidecarFlags(container.SidecarFlagOpts{
				Port:           opts.Port,
				AgentRole:      opts.AgentRole,
				PluginHostPath: opts.PluginHostPath,
				InitialPrompt:  opts.InitialPrompt,
				ConfigContent:  opts.ConfigContent,
			})...)
		}
	}
	if opts.InstanceID != "" {
		cmdArgs = append(cmdArgs, "--instance-id", opts.InstanceID)
	}
	if opts.WorktreeReadOnly {
		cmdArgs = append(cmdArgs, "--worktree-readonly")
	}
	if opts.HarnessName != "" {
		cmdArgs = append(cmdArgs, "--harness", opts.HarnessName)
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
