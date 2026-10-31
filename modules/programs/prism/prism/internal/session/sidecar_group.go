package session

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The sidecar runs in a session of its own (Setsid in StartSidecarWithOpts),
// so its PID is also the ID of its process group and of its session. Each
// process that the sidecar starts, a podman client of `prism container`
// for example, is in that group unless it calls setsid or setpgid. Such a
// process does not stop with the sidecar. A podman client that still runs
// after the stop can create a container after the container sweep (#3380).

const sidecarGroupPoll = 25 * time.Millisecond

// signalGroup sends sig to the process group pgid. Tests replace it.
var signalGroup = func(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}

// groupMember is one process of a process group that has not exited.
type groupMember struct {
	pid int
	// sid is the session ID of the process, or 0 when prism cannot read it.
	sid int
}

// SidecarGroupError reports a process in the process group of a sidecar
// that still runs after the sidecar stopped.
type SidecarGroupError struct {
	Session string
	// PGID is the process group of the sidecar, which is the PID of the
	// sidecar.
	PGID int
	// PIDs are the processes of the group that still run. It is empty when
	// prism cannot list the group.
	PIDs []int
	// Signalled is false when the sidecar had exited before the stop. Then
	// prism sends the group no signal, because it cannot prove that the
	// group is the group of the sidecar.
	Signalled bool
	// ListErr is the error of the last list of the group, if any.
	ListErr error
}

func (e *SidecarGroupError) Error() string {
	if len(e.PIDs) == 0 {
		return fmt.Sprintf("prism cannot prove that the process group %d of the sidecar of session %q is empty: %v", e.PGID, e.Session, e.ListErr)
	}
	ids := make([]string, len(e.PIDs))
	for i, pid := range e.PIDs {
		ids[i] = strconv.Itoa(pid)
	}
	what, runs, it := "process "+ids[0], "runs", "it"
	if len(ids) > 1 {
		what, runs, it = "processes "+strings.Join(ids, ", "), "run", "them"
	}
	if e.Signalled {
		return fmt.Sprintf("%s that the sidecar of session %q started still %s after SIGKILL (process group %d)", what, e.Session, runs, e.PGID)
	}
	return fmt.Sprintf("%s in the process group %d of the exited sidecar of session %q still %s. prism cannot prove that the sidecar started %s, so it sends no signal. Stop %s, then try again", what, e.PGID, e.Session, runs, it, it)
}

// stopSidecarGroup waits until no process of the process group pgid of the
// sidecar of sessionName runs, and returns nil. A zombie has exited, so it
// does not count. When kill is true, it sends SIGKILL to the group at each
// poll. It returns a *SidecarGroupError when a process still runs at the
// timeout.
//
// Give kill=true only when this stop found pid pgid to be the live sidecar
// of the session. The kernel does not give the number pgid to a new process
// while a process, a zombie included, is in the group. So a repeated SIGKILL
// reaches only the group of the sidecar. It stops when the group is empty.
//
// With kill=false, the sidecar exited before this stop, so the number pgid
// can since belong to another process. The group of the sidecar is in the
// session pgid, and setpgid cannot move a process to another session. So a
// group whose processes are in another session is not the group of the
// sidecar, and stopSidecarGroup returns nil for it.
func stopSidecarGroup(sessionName string, pgid int, kill bool, timeout time.Duration) error {
	if err := leaveGroup(pgid); err != nil {
		return fmt.Errorf("stop the process group %d of the sidecar of session %q: this process is in the group and cannot leave it: %w", pgid, sessionName, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		if signalGroup(pgid, 0) == syscall.ESRCH {
			return nil
		}
		members, listErr := liveGroupMembers(pgid)
		if listErr == nil && len(members) == 0 {
			return nil
		}
		if !kill && listErr == nil && !allInSession(members, pgid) {
			return nil
		}
		if kill {
			_ = signalGroup(pgid, syscall.SIGKILL)
		}
		if !time.Now().Before(deadline) {
			e := &SidecarGroupError{Session: sessionName, PGID: pgid, Signalled: kill, ListErr: listErr}
			for _, m := range members {
				e.PIDs = append(e.PIDs, m.pid)
			}
			return e
		}
		time.Sleep(sidecarGroupPoll)
	}
}

// leaveGroup moves this process to a process group of its own when it is
// in the group pgid. The sidecar can run the stop of its own session, for
// example `prism cleanup` through the host API. Without the move, the stop
// kills itself with the group, or waits for itself until the timeout.
func leaveGroup(pgid int) error {
	if syscall.Getpgrp() != pgid {
		return nil
	}
	return syscall.Setpgid(0, 0)
}

// currentBootTime returns the time of the system boot. Tests replace it.
var currentBootTime = bootTime

// pidFileBeforeBoot reports whether the PID file of sessionName is older
// than the system boot. Its PID then names a process of an earlier boot,
// and a group with that ID is not the group of the sidecar. A double-fork
// daemon, for example, can leave a group and a session with the ID of a
// PID that is gone. If prism cannot read a time, it returns false.
func pidFileBeforeBoot(sessionName string) bool {
	pidPath, err := SidecarPIDPath(sessionName)
	if err != nil {
		return false
	}
	info, err := os.Stat(pidPath)
	if err != nil {
		return false
	}
	boot, err := currentBootTime()
	if err != nil {
		return false
	}
	return info.ModTime().Before(boot)
}

// allInSession reports whether each member is in session sid. A member
// whose session prism cannot read counts as in the session.
func allInSession(members []groupMember, sid int) bool {
	for _, m := range members {
		if m.sid != 0 && m.sid != sid {
			return false
		}
	}
	return true
}
