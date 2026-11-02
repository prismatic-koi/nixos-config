package session

// StartSidecar launches a detached `prism sidecar` process alongside the agent.
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
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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
// 108-byte limit even when $HOME is unusually long. A regression test in
// this package pins the path arithmetic.
const sessionDirHashLen = 12

// SessionDirName returns the deterministic per-session directory name used
// under $XDG_STATE_HOME/prism/run/ for files that must be co-located with the
// host-API Unix socket (currently hostapi.sock and agent-run.log).
//
// The directory name is the first 12 hex characters of SHA-256(sessionName).
// This keeps the resulting socket path under sun_path budgets on every
// platform regardless of how long the session name itself is.
//
// The mapping is pure and deterministic, so cleanup, debugging
// (`prism logs <session>`), and sandbox bind-mount construction can all
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
// No current isolation mode writes or polls this file. The path helper is
// retained so cleanup can remove stale files.
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
// Each session gets its own subdirectory under run/ so that the sandbox can
// bind-mount only that directory — providing socket isolation without exposing
// other sessions' sockets — socket isolation. The subdirectory is pre-created
// by container.prepareVolumeDirs, so the directory already exists when the
// sandbox evaluates the bind-mount, even though the socket file inside it is
// created later by the sidecar.
//
// The directory name is a 12-hex-char SHA-256 prefix of the session name (see
// SessionDirName) rather than the session name itself. This keeps the resulting
// socket path under the platform sun_path limits. The full session name
// (e.g. "<repo>@<long-branch>~review-N-review-<role>") can push the path past
// Linux's 108-byte and Darwin's 104-byte budgets, causing bind(2) to fail
// with EINVAL.
//
// Socket path: $XDG_STATE_HOME/prism/run/<12-hex-of-sha256(session)>/hostapi.sock
func SidecarHostAPIPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", SessionDirName(sessionName), "hostapi.sock"), nil
}

// SidecarHarnessPipePath returns the Unix socket path for the session's
// PI harness pipe socket. This socket is the sidecar-side end of the
// bidirectional JSONL protocol defined in P2.WIRE.
//
// The socket lives in the same per-session directory as the host-API socket
// (hostapi.sock), so the existing bind-mount entry in bwrap (which exposes
// the per-session run directory inside the sandbox) covers this socket too —
// no additional bind-mount is needed (see internal/container/bwrap.go,
// the PRISM_HOST_API block). The same applies to the Darwin sandbox-exec
// SBPL subpath rule.
//
// The directory name is the same SessionDirName-derived 12-hex prefix used
// by SidecarHostAPIPath, keeping the socket path well under sun_path limits
// on both Linux (108 bytes) and Darwin (104 bytes).
//
// Socket path: $XDG_STATE_HOME/prism/run/<12-hex-of-sha256(session)>/pipe.sock
func SidecarHarnessPipePath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", SessionDirName(sessionName), "pipe.sock"), nil
}

// SidecarHarnessPipeSecretPath returns the path of the file that holds the
// harness pipe secret of the session. The sidecar writes a new random secret
// to this file each time it starts, and refuses a hello frame without it
// (#3356). The PI extension reads the file and sends the secret in hello.
//
// The file is in the per-session run dir. The sandbox of the session can
// read that dir (bwrap bind, sandbox-exec subpath rule), and the sandbox of
// another session cannot. On Darwin the pipe is a loopback TCP port that
// every session can reach, so this file is what stops a session from
// handshaking as another session.
//
// Path: $XDG_STATE_HOME/prism/run/<12-hex-of-sha256(session)>/pipe.secret
func SidecarHarnessPipeSecretPath(sessionName string) (string, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", SessionDirName(sessionName), "pipe.secret"), nil
}

// The log captures the stdout and stderr of the bwrap sandbox (and the agent
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
// recorded process, and removes the PID file. It reports whether it sent
// SIGTERM. It handles missing or stale PID files gracefully — no error is
// returned in those cases.
//
// This function is safe to call from test teardown: if StartSidecar was never
// reached (e.g. the test failed before the session was created) the missing PID
// file is treated as a no-op.  If the process has already exited, the
// stale PID file is removed without attempting the kill.  If the PID has been
// recycled to an unrelated process (its command line does not carry the
// sidecar markers), the kill is skipped and the file is removed.  If the
// command line of a live PID cannot be read, the kill is skipped, a warning
// names the PID, and the file is removed.
func KillSidecar(sessionName string) bool {
	_, owner := killSidecar(sessionName)
	return owner == pidOwnerSidecar
}

// killSidecar is the body of KillSidecar. It returns the PID in the PID
// file (0 when the file is absent or does not hold a PID) and the owner of
// that PID. It sent SIGTERM only when the owner is pidOwnerSidecar.
func killSidecar(sessionName string) (int, pidOwner) {
	pidPath, err := SidecarPIDPath(sessionName)
	if err != nil {
		// Can't derive the path — nothing to clean up.
		return 0, pidOwnerGone
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		// PID file absent — sidecar was never started or already cleaned up.
		return 0, pidOwnerGone
	}

	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		// Corrupt PID file — remove it and move on.
		_ = os.Remove(pidPath)
		return 0, pidOwnerGone
	}

	// Guard against same-user PID recycling: verify the PID belongs to a
	// prism sidecar process by checking its command line before sending
	// SIGTERM. The check matches the invariant argv shape of a prism-spawned
	// sidecar:
	//
	//   <binary> sidecar --session <name> ...
	//
	// We require both "sidecar" and "--session" to appear in the cmdline.
	// Do not match on "prism" in the binary path: under `go test` the sidecar
	// is spawned by re-invoking the test binary (e.g. "cmd.test sidecar …"),
	// whose cmdline does not contain "prism" at all, so KillSidecar would skip
	// the kill and sidecars would leak after every test run.
	owner := classifySidecarPID(pid, sessionName, func(argv []string) bool {
		cmdline := strings.Join(argv, "\x00")
		return strings.Contains(cmdline, "sidecar") && strings.Contains(cmdline, "--session")
	})
	switch owner {
	case pidOwnerOther:
		// PID has been recycled to an unrelated process — do not kill it.
		fmt.Fprintf(os.Stderr, "warning: sidecar pid %d does not appear to be a prism sidecar process — skipping kill\n", pid)
		_ = os.Remove(pidPath)
		return pid, owner
	case pidOwnerGone, pidOwnerUnknown:
		// Fail closed: do not signal a PID that prism cannot prove is a
		// sidecar.
		_ = os.Remove(pidPath)
		return pid, owner
	}

	// Send SIGTERM; ignore ESRCH (no such process — already gone).
	// The PID file is removed unconditionally after the kill attempt.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "warning: kill sidecar pid %d: %v\n", pid, err)
	}

	_ = os.Remove(pidPath)
	return pid, owner
}

// ErrSidecarOwnerUnknown is in the error of a stop that cannot read the
// command line of the live PID. That process can still be the sidecar.
var ErrSidecarOwnerUnknown = errors.New("cannot prove that the process is not the sidecar")

// StopSidecar stops the sidecar of sessionName and reports whether it exited
// inside timeout after SIGTERM. It also returns true when no sidecar of the
// session runs: the PID file is absent, or its PID is gone or belongs to
// another process. The result does not report the process group of the
// sidecar: StopSidecarReportErr gives the full result.
//
// If the sidecar is still alive at the timeout, StopSidecar sends SIGKILL
// and returns false. A sidecar that is slow to stop still writes to the
// database: its Shutdown stamps sessions.ended_at and then writes a final
// state event. prism restore must not reopen a row that such a sidecar can
// end again, and the sidecar must not write an event after its row ended.
// SIGKILL goes only to a PID that StopSidecar confirms as this session's
// sidecar.
//
// If StopSidecar cannot read the command line of a live PID, it sends no
// signal and writes a warning that names the PID. If this user owns that
// PID, StopSidecar returns false: that PID can still be the sidecar.
//
// The PID file stays while the sidecar runs. Another stop that runs at the
// same time then still finds the process, and does not report it as gone.
func StopSidecar(sessionName string, timeout time.Duration) bool {
	exited, _ := stopSidecarPID(sessionName, readSidecarPID(sessionName), timeout, true)
	return exited
}

// StopSidecarReport stops the sidecar of sessionName as StopSidecar does.
// pid is the PID in the PID file, or 0 when the file holds no PID. exited
// is the result of StopSidecar. gone is the result of EnsureSidecarStopped:
// it is false when StopSidecarReport sent no signal because it cannot read
// the command line of a live PID, when the sidecar still runs after
// SIGKILL, and when a process in the process group of the sidecar still
// runs.
//
// When gone is false, the PID file stays, as with EnsureSidecarStopped. A
// caller that then does not start a new sidecar needs the file: the next
// stop finds the same process, and does not report it as gone (#3303).
func StopSidecarReport(sessionName string, timeout time.Duration) (pid int, exited, gone bool) {
	pid, exited, err := StopSidecarReportErr(sessionName, timeout)
	return pid, exited, err == nil
}

// StopSidecarReportErr is StopSidecarReport, but it returns the reason in
// place of gone=false, as EnsureSidecarStoppedErr does.
func StopSidecarReportErr(sessionName string, timeout time.Duration) (pid int, exited bool, err error) {
	pid = readSidecarPID(sessionName)
	exited, err = stopSidecarPID(sessionName, pid, timeout, false)
	return pid, exited, err
}

// SidecarStopTimeout is the SIGTERM wait, and then the SIGKILL wait, that
// the release, the session-closed hook, parent cleanup, and the ended-session
// sweep give EnsureSidecarStopped.
const SidecarStopTimeout = 3 * time.Second

// EnsureSidecarStopped stops the sidecar of sessionName as StopSidecar does,
// and reports whether no sidecar of the session runs when it returns. A
// sidecar that exits only after SIGKILL counts as stopped. It also stops
// the processes that the sidecar started: see EnsureSidecarStoppedErr.
//
// It returns false when the sidecar still runs, when a process of its
// process group still runs, or when prism cannot read the command line of
// the PID in the PID file. The PID file then stays, so
// that a later stop finds the process again. A caller that ends the session
// must keep the row and the port when it returns false: the process can
// still write to the row and hold the port.
func EnsureSidecarStopped(sessionName string, timeout time.Duration) bool {
	return EnsureSidecarStoppedErr(sessionName, timeout) == nil
}

// EnsureSidecarStoppedErr is EnsureSidecarStopped, but it returns an error
// that names the PID and the session in place of false.
//
// After the sidecar exits, it sends SIGKILL to the process group of the
// sidecar, and waits up to timeout until no process of the group runs. If
// a process still runs, it returns a *SidecarGroupError and the PID file
// stays. A caller that sweeps the containers of the session must sweep
// only after a nil return: a podman client of the sidecar that still runs
// can create a container after the sweep.
//
// When the PID file names a PID that no longer runs, the sidecar exited
// before this stop. It then sends no signal, but it still waits for the
// group, and returns a *SidecarGroupError when a process of the group
// still runs at the timeout. An empty group returns at once.
func EnsureSidecarStoppedErr(sessionName string, timeout time.Duration) error {
	_, err := stopSidecarPID(sessionName, readSidecarPID(sessionName), timeout, false)
	return err
}

// stopSidecarPID is the shared body of StopSidecar, EnsureSidecarStopped,
// and the ended-session sweep. It acts on pid, which the caller read from
// the PID file, and does not read the file again: a sidecar that starts
// for the session in the meantime writes a new PID, and that sidecar must
// get no signal.
//
// exited reports an exit after SIGTERM, inside timeout. A nil err reports
// that pid does not run the sidecar of the session at return, and that no
// process of its process group runs (stopSidecarGroup). dropUnknown
// removes the PID file of a PID whose command line prism cannot read.
func stopSidecarPID(sessionName string, pid int, timeout time.Duration, dropUnknown bool) (exited bool, err error) {
	owner := pidOwnerGone
	if pid > 0 && sidecarProcessExists(pid) {
		owner = sidecarPIDOwner(pid, sessionName)
	}
	if owner == pidOwnerGone && pid > 0 && !pidFileBeforeBoot(sessionName) {
		if err := sidecarGroupStopped(sessionName, pid, false, timeout); err != nil {
			return false, err
		}
	}
	// EPERM means another user owns pid. The sidecar runs as this user, so a
	// stale PID file names that process.
	if owner == pidOwnerGone || owner == pidOwnerOther || syscall.Kill(pid, 0) == syscall.EPERM {
		removeSidecarPIDFileIf(sessionName, pid)
		return true, nil
	}
	if owner == pidOwnerUnknown {
		if dropUnknown {
			removeSidecarPIDFileIf(sessionName, pid)
		}
		return false, fmt.Errorf("sidecar pid %d (session %q): cannot read its command line: %w", pid, sessionName, ErrSidecarOwnerUnknown)
	}

	deadline := time.Now().Add(timeout)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "warning: kill sidecar pid %d: %v\n", pid, err)
	}
	if waitSidecarExit(pid, deadline) {
		if err := sidecarGroupStopped(sessionName, pid, true, timeout); err != nil {
			unlinkSidecarSockets(sessionName)
			return true, err
		}
		removeSidecarPIDFileIf(sessionName, pid)
		// Unlink only after the exit. See unlinkSidecarSockets for the
		// listener race that this order closes.
		unlinkSidecarSockets(sessionName)
		if sockPath, err := SidecarHostAPIPath(sessionName); err == nil {
			waitForSocketGone(sockPath, deadline)
		}
		return true, nil
	}

	if sidecarPIDOwner(pid, sessionName) == pidOwnerSidecar {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			fmt.Fprintf(os.Stderr, "warning: SIGKILL sidecar pid %d (session %q): %v\n", pid, sessionName, err)
		}
		waitSidecarExit(pid, time.Now().Add(timeout))
	}
	if sidecarProcessExists(pid) {
		err := fmt.Errorf("sidecar pid %d (session %q) still runs after SIGTERM and SIGKILL", pid, sessionName)
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		return false, err
	}
	unlinkSidecarSockets(sessionName)
	if err := sidecarGroupStopped(sessionName, pid, true, timeout); err != nil {
		return false, err
	}
	removeSidecarPIDFileIf(sessionName, pid)
	return false, nil
}

// sidecarGroupStopped runs stopSidecarGroup, and writes a warning when a
// process of the group still runs. The PID file of pid then stays, so that
// a later stop waits for the group again. A sidecar that exits on SIGTERM
// removes its own PID file, so sidecarGroupStopped writes the file again.
func sidecarGroupStopped(sessionName string, pid int, kill bool, timeout time.Duration) error {
	err := stopSidecarGroup(sessionName, pid, kill, timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		restoreSidecarPIDFile(sessionName, pid)
	}
	return err
}

// restoreSidecarPIDFile writes pid to the PID file of sessionName when the
// file is absent. It does not replace a file that a new sidecar of the
// session wrote.
func restoreSidecarPIDFile(sessionName string, pid int) {
	pidPath, err := SidecarPIDPath(sessionName)
	if err != nil {
		return
	}
	f, err := os.OpenFile(pidPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.WriteString(strconv.Itoa(pid) + "\n"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: write the sidecar PID file %s again: %v\n", pidPath, err)
	}
}

// waitSidecarExit polls until pid no longer runs or deadline passes. It
// reports whether pid exited.
func waitSidecarExit(pid int, deadline time.Time) bool {
	for {
		if !sidecarProcessExists(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// RemoveOwnSidecarPIDFile removes the sidecar PID file of sessionName when
// it holds the PID of this process. A sidecar calls it as it exits. A file
// that holds another PID belongs to a newer sidecar of the session, so it
// stays.
//
// The file also stays while another process of the process group of this
// sidecar runs, for example a podman client of a request in progress. A
// later stop finds the group only through the file, and it waits for the
// group before the container sweep (stopSidecarGroup).
func RemoveOwnSidecarPIDFile(sessionName string) {
	self := os.Getpid()
	if pid := ownGroupProcess(self); pid != 0 {
		fmt.Fprintf(os.Stderr, "warning: sidecar of session %q: process %d of its process group still runs, so the PID file stays\n", sessionName, pid)
		return
	}
	removeSidecarPIDFileIf(sessionName, self)
}

// ownGroupProcess returns the PID of a process other than self that runs in
// the process group of self, or 0. It returns 0 when self does not lead a
// process group, as a sidecar that StartSidecarWithOpts started does. If
// prism cannot list the group, it returns self, so that the file stays.
func ownGroupProcess(self int) int {
	if syscall.Getpgrp() != self {
		return 0
	}
	members, err := liveGroupMembers(self)
	if err != nil {
		return self
	}
	for _, m := range members {
		if m.pid != self {
			return m.pid
		}
	}
	return 0
}

// readSidecarPID returns the PID in the sidecar PID file of sessionName, or
// 0 when the file is absent or does not hold a PID.
func readSidecarPID(sessionName string) int {
	pidPath, err := SidecarPIDPath(sessionName)
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// removeSidecarPIDFileIf removes the sidecar PID file of sessionName only
// when it still holds pid. A pid of 0 matches a file that holds no PID. A
// sidecar that starts for the session writes a new PID into the file, and
// a stop of the old PID must not remove that file.
func removeSidecarPIDFileIf(sessionName string, pid int) {
	pidPath, err := SidecarPIDPath(sessionName)
	if err != nil {
		return
	}
	if _, statErr := os.Stat(pidPath); statErr != nil {
		return
	}
	if readSidecarPID(sessionName) != pid {
		return
	}
	_ = os.Remove(pidPath)
}

type pidOwner int

const (
	// pidOwnerUnknown: the process runs, but prism cannot read its command
	// line. Send it no signal.
	pidOwnerUnknown pidOwner = iota
	pidOwnerSidecar
	pidOwnerOther
	// pidOwnerGone: the process no longer runs, or is a zombie.
	pidOwnerGone
)

// pidArgv returns the argument vector of a process. Tests replace it.
var pidArgv = readPIDArgv

// classifySidecarPID reads the command line of pid and reports
// pidOwnerSidecar when isSidecar accepts it. Fail closed: if the command
// line cannot be read and pid still runs, it writes a warning that names
// pid and returns pidOwnerUnknown. Do not signal pid in that case.
func classifySidecarPID(pid int, sessionName string, isSidecar func(argv []string) bool) pidOwner {
	argv, err := pidArgv(pid)
	if err == nil {
		if isSidecar(argv) {
			return pidOwnerSidecar
		}
		return pidOwnerOther
	}
	if !sidecarProcessExists(pid) {
		return pidOwnerGone
	}
	fmt.Fprintf(os.Stderr, "warning: sidecar pid %d (session %q): cannot read its command line (%v), so prism cannot prove that it is a prism sidecar; sending no signal\n", pid, sessionName, err)
	return pidOwnerUnknown
}

// sidecarPIDOwner reports whether pid runs `<binary> sidecar --session
// <sessionName>`.
//
// A command line that is readable but empty is pidOwnerOther. A kernel
// thread has an empty command line, and after a reboot the PID in a stale
// PID file can belong to one. That is not a sidecar, so restore must not
// wait for it to stop.
func sidecarPIDOwner(pid int, sessionName string) pidOwner {
	return classifySidecarPID(pid, sessionName, func(argv []string) bool {
		hasSidecar := false
		for i, a := range argv {
			if a == "sidecar" {
				hasSidecar = true
			}
			if hasSidecar && a == "--session" && i+1 < len(argv) && argv[i+1] == sessionName {
				return true
			}
		}
		return false
	})
}

// unlinkSidecarSockets removes the host-API and harness pipe socket files for
// the given session, ignoring errors. Call it only *after* the sidecar
// process has been confirmed gone, so that a dial by path then gets ENOENT
// regardless of the kernel's internal listener state. This closes the race
// where /proc/<pid>/status reports State:Z (so sidecarProcessExists returns
// false) before the kernel has fully torn down the bound Unix socket from its
// listener table. Do not unlink before the exit: if SIGTERM was never sent,
// the listener stays alive on its FD, but a dial by path gets ENOENT, so the
// stale-zombie regression test would report a false success.
func unlinkSidecarSockets(sessionName string) {
	if sockPath, err := SidecarHostAPIPath(sessionName); err == nil {
		_ = os.Remove(sockPath)
	}
	if pipePath, err := SidecarHarnessPipePath(sessionName); err == nil {
		_ = os.Remove(pipePath)
	}
}

// sidecarAliveDialTimeout bounds each liveness dial in SidecarAlive. Kept
// short (250ms, matching the sidecar's own duplicate-start probe) so callers
// on interactive paths (prism switch) are not stalled by a wedged socket.
const sidecarAliveDialTimeout = 250 * time.Millisecond

// SidecarAlive reports whether a live sidecar process is currently accepting
// connections on one of sessionName's Unix sockets. The sidecar binds
// hostapi.sock in every isolation mode, and pipe.sock for a socket-pipe
// harness that does not use TCP. A live listener on either socket is enough.
//
// This is the authoritative liveness signal for sessions whose agent_status
// row has gone quiet: paused-by-design states (escalated awaiting coordinator
// guidance, reviewing awaiting verdicts, waiting on a permission prompt) stop
// producing state transitions, so last_seen freezes while the sidecar is
// perfectly healthy. Classifying such a session as a stale zombie purely on
// last_seen age — and then SIGTERMing its sidecar — severs every coordination
// channel for the session (prompt delivery, finish notifications, event
// recording).
//
// A dead socket (absent path, or a tombstone file whose listener is gone —
// ECONNREFUSED) returns false, preserving the existing stale-zombie restart
// behaviour for genuinely dead sidecars.
func SidecarAlive(sessionName string) bool {
	paths := make([]string, 0, 2)
	if sockPath, err := SidecarHostAPIPath(sessionName); err == nil {
		paths = append(paths, sockPath)
	}
	if pipePath, err := SidecarHarnessPipePath(sessionName); err == nil {
		paths = append(paths, pipePath)
	}
	for _, p := range paths {
		conn, err := net.DialTimeout("unix", p, sidecarAliveDialTimeout)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

// waitForSocketGone polls the given Unix socket path with short dials until
// either a dial returns an error (the listener is gone) or the deadline is
// reached. Errors are intentionally swallowed — this function is best-effort
// defence in depth; the primary close-signal is the os.Remove of the socket
// path in unlinkSidecarSockets, which the caller has already arranged.
func waitForSocketGone(sockPath string, deadline time.Time) {
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("unix", sockPath, 25*time.Millisecond)
		if dialErr != nil {
			return
		}
		conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
}

// sidecarProcessExists reports whether the given PID is still running
// (excluding zombies, which have already exited and are just awaiting reap).
//
// On Linux it reads /proc/<pid>/status for accurate zombie detection:
// a process in state Z has already exited and is just waiting to be reaped.
//
// On other platforms (e.g. Darwin), it falls back to kill(pid, 0): signal 0
// is never delivered but the kernel validates that the PID exists and is
// owned by the same user — ESRCH means the process is gone, nil means it
// is still running. kill(pid, 0) succeeds for a zombie, so pidIsZombie
// then reads the process state (kern.proc.pid on Darwin). The command line
// of a zombie cannot be read, so without this check a sidecar that exited
// and waits for its parent reads as a live PID of unknown owner.
func sidecarProcessExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Linux path: /proc gives accurate zombie detection.
	statusData, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err == nil {
		// A zombie ("State:\tZ") has already exited; treat as gone.
		if strings.Contains(string(statusData), "State:\tZ") {
			return false
		}
		return true
	}
	// Non-Linux (or /proc unavailable): use kill(pid, 0).
	// ESRCH → process is gone; any other result (nil, EPERM) → still running.
	if syscall.Kill(pid, 0) == syscall.ESRCH {
		return false
	}
	return !pidIsZombie(pid)
}

// StartSidecarOpts holds optional parameters for launching a sidecar process.
type StartSidecarOpts struct {
	// Port is the allocated harness serve port.
	Port int
	// IsolationMode is the resolved isolation mode for this session. When set,
	// it is passed to the sidecar via --isolation-mode so the sidecar can
	// branch on it. Valid values: "bwrap", "sandbox-exec", "host" (see
	// config.ValidIsolationModes).
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
	// readiness. Passed via --initial-prompt in container mode.
	// Empty string means no prompt delivery.
	InitialPrompt string
	// InstanceID is the UUID instance identifier for this session incarnation.
	// When non-empty, it is passed to the sidecar via --instance-id so that
	// the sidecar can use it for container labels and bus message scoping
	// without needing to read it back from the DB (which would race with the
	// tmux-session-start event that writes instance_id to agent_status).
	InstanceID string
	// HarnessName is the registered harness name (e.g. "pi"). When
	// non-empty it is forwarded to the sidecar via --harness so the sidecar
	// can call harness.ShapeOf to determine its own transport shape. When
	// empty, the sidecar defaults to "pi".
	HarnessName string
	// ModelsByRole is the per-role model override map (C.2). When non-empty
	// it is forwarded to the sidecar via repeated --model-override role=model
	// flags so the harness adapter applies per-role overrides.
	ModelsByRole map[string]string
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

	harnessURL := fmt.Sprintf("http://localhost:%d", opts.Port)

	// Build the sidecar command arguments.
	cmdArgs := []string{"sidecar",
		"--session", sessionName,
		"--harness-url", harnessURL,
	}

	// Pass --isolation-mode when set; the sidecar uses this to branch on
	// container creation, harness selection, and host-API socket setup.
	// The spawned sidecar needs the flag to look up its own isolator after
	// re-exec.
	if opts.IsolationMode != "" {
		cmdArgs = append(cmdArgs, "--isolation-mode", opts.IsolationMode)
	}

	// The per-mode argv branches are a single Isolator.SidecarFlags dispatch:
	//
	//   - bwrap, sandbox-exec:  --port --agent-role --plugin-path …
	//   - host:                 --port --agent-role --plugin-path …  (same as bwrap/sandbox-exec)
	if opts.IsolationMode != "" {
		iso, isoErr := container.For(config.IsolationMode(opts.IsolationMode), container.ConstructorOpts{Name: sessionName})
		if isoErr == nil {
			cmdArgs = append(cmdArgs, iso.SidecarFlags(container.SidecarFlagOpts{
				Port:           opts.Port,
				AgentRole:      opts.AgentRole,
				PluginHostPath: opts.PluginHostPath,
				InitialPrompt:  opts.InitialPrompt,
			})...)
		}
	}
	if opts.InstanceID != "" {
		cmdArgs = append(cmdArgs, "--instance-id", opts.InstanceID)
	}
	if opts.HarnessName != "" {
		cmdArgs = append(cmdArgs, "--harness", opts.HarnessName)
	}
	for role, model := range opts.ModelsByRole {
		cmdArgs = append(cmdArgs, "--model-override", role+"="+model)
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
	// The stop of the sidecar needs Setsid too: stopSidecarGroup takes the
	// PID of the sidecar as the ID of its process group and of its session.
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
