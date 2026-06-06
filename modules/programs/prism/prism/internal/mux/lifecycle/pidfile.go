package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// pidFileName is the basename of the daemon's PID file. Kept distinct
// from any session-scoped PID file so listing
// $XDG_STATE_HOME/prism/run/ for the daemon's pid is unambiguous.
const pidFileName = "mux.pid"

// DefaultPIDPath returns the canonical PID-file path for the prism mux
// daemon: $XDG_STATE_HOME/prism/run/mux.pid.
//
// Resolution order mirrors the sidecar's SidecarPIDPath
// (internal/session/sidecar.go) and the persist layer's DefaultPath
// (internal/mux/persist/persist.go) so the three state-file conventions
// stay aligned:
//
//  1. $XDG_STATE_HOME/prism/run/mux.pid — if XDG_STATE_HOME is set.
//  2. $HOME/.local/state/prism/run/mux.pid — fallback.
//
// XDG_STATE_HOME is checked first so the nix build sandbox
// (HOME=/homeless-shelter is unwritable) and the test suite (which
// sets XDG_STATE_HOME=t.TempDir()) both work without touching $HOME.
//
// Returns ("", error) only when neither is discoverable.
func DefaultPIDPath() (string, error) {
	base, err := stateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", pidFileName), nil
}

// stateBaseDir resolves $XDG_STATE_HOME/prism, falling back to
// $HOME/.local/state/prism. Returns an error only when neither is
// discoverable. The fallback shape matches internal/session.sidecarStateDir.
func stateBaseDir() (string, error) {
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		return filepath.Join(stateHome, "prism"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("lifecycle: cannot resolve state dir — neither XDG_STATE_HOME nor HOME is set")
	}
	return filepath.Join(home, ".local", "state", "prism"), nil
}

// writePIDFile writes pid to path atomically: write to a sibling
// tempfile in the same directory, then rename onto path. Same-directory
// rename is atomic on every filesystem prism supports, so a crash
// mid-write cannot leave a torn PID file for status / stop to trip on.
//
// The parent directory is created with 0o700 if missing — matches the
// sidecar's run/ directory mode (internal/session/sidecar.go writes
// 0o755, but the run/ directory itself need not be world-readable; we
// follow the more restrictive of the two existing conventions).
//
// The trailing newline matches every other PID file in the tree (see
// SidecarPIDPath callers).
func writePIDFile(path string, pid int) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("lifecycle: create pid dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, pidFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("lifecycle: create pid tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.WriteString(strconv.Itoa(pid) + "\n"); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("lifecycle: write pid tempfile: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("lifecycle: chmod pid tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("lifecycle: close pid tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("lifecycle: rename pid file: %w", err)
	}
	return nil
}

// readPIDFile reads and parses path. Returns (0, os.ErrNotExist) when
// path does not exist — wrapped so callers branch with errors.Is.
// Returns (0, errInvalidPIDFile) when the contents are not a positive
// integer; callers treat that as "stale, clean up and proceed".
var errInvalidPIDFile = errors.New("lifecycle: pid file is not a positive integer")

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// os.ErrNotExist passes through verbatim so callers can branch
		// on it with errors.Is.
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, errInvalidPIDFile
	}
	return pid, nil
}

// processAlive reports whether pid identifies a live, non-zombie process
// the current user may signal. On Linux the canonical answer is in
// /proc/<pid>/status: a process in state Z has already exited and is
// just awaiting reap, so for the purpose of "is the previous mux daemon
// still alive" it must be treated as gone. Falling back to kill(pid, 0)
// alone would incorrectly report a zombie as alive and refuse the next
// start with ErrAlreadyRunning.
//
// On non-Linux platforms (Darwin), /proc is not available; kill(pid, 0)
// is the best we have. The mux daemon path always ends with either
// systemd / launchd reaping it (no zombie window) or Setsid-detached
// reaping by init (negligible zombie window), so the failure mode is
// vanishingly rare in practice there.
//
// EPERM (kernel-validated PID owned by another user) counts as gone:
// our daemon would never appear under another UID, so the recorded PID
// has been recycled to an unrelated process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err == nil {
			// State:\tZ marks a zombie — already exited.
			if strings.Contains(string(data), "State:\tZ") {
				return false
			}
			return true
		}
		// /proc unreadable — fall through to kill(pid, 0). On modern
		// kernels the only way ReadFile fails for a live pid is ENOENT
		// (process gone), but be paranoid.
	}
	return syscall.Kill(pid, 0) == nil
}
