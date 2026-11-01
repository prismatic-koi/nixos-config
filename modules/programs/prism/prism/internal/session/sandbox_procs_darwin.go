//go:build darwin

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/prismatic-koi/prism/internal/rundir"
	"github.com/prismatic-koi/prism/internal/sandboxcheck"
)

// sandboxProbeOp is the operation of the sandbox check. The section-6 rule
// of the sandbox-exec profile allows it on the run dir node of the
// session. Section 21m denies only the create, unlink, mode, flag, and ACL
// writes of that node.
const sandboxProbeOp = "file-write-data"

// sandboxProbePaths returns the two paths of the sandbox check of
// sessionName. inside is the run dir of the session. The section-6 rule of
// the sandbox-exec profile (generateProfile in
// internal/container/sandbox_exec.go) lets the session write it, and no
// rule lets another session write it. outside is the run root, which no
// profile lets a session write.
//
// sandbox_check denies each operation on a path that does not exist, also
// for a process with no sandbox. So a missing run dir hides each process
// of the session, and this function makes the run dir when it is missing.
func sandboxProbePaths(sessionName string) (inside, outside string, err error) {
	sock, err := SidecarHostAPIPath(sessionName)
	if err != nil {
		return "", "", err
	}
	inside = filepath.Dir(sock)
	if err := rundir.EnsureDir(inside); err != nil {
		return "", "", fmt.Errorf("run dir of session %q: %w", sessionName, err)
	}
	return inside, filepath.Dir(inside), nil
}

// platformSandboxFinder returns the finder of the processes of this user
// that run in the sandbox of sessionName. The list comes from
// proc_listpids (real user ID), so a process that ran a setuid program is
// in it too. A process is in the sandbox of the session when its sandbox
// allows a write of the run dir of the session and denies a write of the
// run root. A process with no sandbox, or with a sandbox that allows each
// write, fails the second check. The sandbox of another session fails the
// first.
func platformSandboxFinder(sessionName string) (sandboxFinder, error) {
	inside, outside, err := sandboxProbePaths(sessionName)
	if err != nil {
		return nil, err
	}
	uid := os.Getuid()
	return newSandboxFinder(procProbe{
		list:      func() ([]int, error) { return sandboxcheck.ListPIDs(uid) },
		inSandbox: func(pid int) (bool, error) { return inSessionSandbox(pid, inside, outside) },
		zombie:    zombieStart,
	}, os.Getpid()), nil
}

// inSessionSandbox reports whether the sandbox of pid allows a write at
// inside and denies a write at outside.
func inSessionSandbox(pid int, inside, outside string) (bool, error) {
	sandboxed, err := sandboxcheck.Sandboxed(pid)
	if err != nil || !sandboxed {
		return false, err
	}
	allowed, err := sandboxcheck.Allows(pid, sandboxProbeOp, inside)
	if err != nil || !allowed {
		return false, err
	}
	allowed, err = sandboxcheck.Allows(pid, sandboxProbeOp, outside)
	if err != nil {
		return false, err
	}
	return !allowed, nil
}

// zombieStart reports whether pid is a zombie, and its start time.
func zombieStart(pid int) (bool, time.Time) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_stat != szomb {
		return false, time.Time{}
	}
	return true, time.Unix(kp.Proc.P_starttime.Unix())
}
