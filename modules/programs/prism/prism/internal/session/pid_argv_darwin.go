package session

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// readPIDArgv returns the argument vector of pid from the kern.procargs2
// sysctl. Darwin has no /proc. Do not use ps(1) here: /bin/ps is setuid
// root, and the sandbox-exec profile of a prism session denies its exec.
// The sysctl works inside that profile.
func readPIDArgv(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.procargs2 %d: %w", pid, err)
	}
	return parseProcArgs2(buf)
}

// parseProcArgs2 decodes a kern.procargs2 buffer. The buffer holds argc as
// a 32-bit integer, the exec path, NUL padding, and then argc
// NUL-terminated arguments. The environment follows the arguments.
func parseProcArgs2(buf []byte) ([]string, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("procargs2: buffer of %d bytes has no argc", len(buf))
	}
	argc := int(int32(binary.NativeEndian.Uint32(buf[:4])))
	if argc < 0 {
		return nil, fmt.Errorf("procargs2: negative argc %d", argc)
	}
	rest := buf[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil, errors.New("procargs2: exec path has no terminator")
	}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	argv := []string{}
	for len(argv) < argc {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, fmt.Errorf("procargs2: %d of %d arguments present", len(argv), argc)
		}
		argv = append(argv, string(rest[:end]))
		rest = rest[end+1:]
	}
	return argv, nil
}

// szomb is the p_stat value of a zombie process (SZOMB in <sys/proc.h>).
const szomb = 5

// pidIsZombie reports whether pid has exited and waits for its parent to
// reap it. On Darwin, kill(pid, 0) succeeds for a zombie.
func pidIsZombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && kp.Proc.P_stat == szomb
}

// liveGroupMembers returns each process in the process group pgid that has
// not exited, from the kern.proc.pgrp sysctl.
func liveGroupMembers(pgid int) ([]groupMember, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.proc.pgrp %d: %w", pgid, err)
	}
	var members []groupMember
	for _, p := range procs {
		if p.Proc.P_stat == szomb {
			continue
		}
		m := groupMember{pid: int(p.Proc.P_pid)}
		if sid, err := unix.Getsid(m.pid); err == nil {
			m.sid = sid
		}
		members = append(members, m)
	}
	return members, nil
}

// bootTime returns the time of the system boot, from the kern.boottime
// sysctl.
func bootTime() (time.Time, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return time.Time{}, fmt.Errorf("sysctl kern.boottime: %w", err)
	}
	return time.Unix(tv.Unix()), nil
}
