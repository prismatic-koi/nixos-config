package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procRoot is the procfs mount that readPIDArgv reads. Tests point it at a
// fake tree.
var procRoot = "/proc"

// readPIDArgv returns the argument vector of pid from
// <procRoot>/<pid>/cmdline.
func readPIDArgv(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), nil
}

// pidIsZombie is false. On Linux, sidecarProcessExists reads the zombie
// state from /proc/<pid>/status, and calls pidIsZombie only when it cannot
// read that file.
func pidIsZombie(int) bool { return false }

// liveGroupMembers returns each process in the process group pgid that has
// not exited, from <procRoot>/<pid>/stat.
func liveGroupMembers(pgid int) ([]groupMember, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var members []groupMember
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			// The process exited after the directory read.
			continue
		}
		state, pgrp, sid, ok := parseProcStat(data)
		if !ok || pgrp != pgid || state == 'Z' || state == 'X' {
			continue
		}
		members = append(members, groupMember{pid: pid, sid: sid})
	}
	return members, nil
}

// parseProcStat returns the state, the process group, and the session of
// a /proc/<pid>/stat line. The command name is in parentheses and can hold
// spaces and parentheses, so the fields start after the last ')'.
func parseProcStat(data []byte) (state byte, pgrp, sid int, ok bool) {
	s := string(data)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, 0, 0, false
	}
	// Fields after the name: state, ppid, pgrp, session, ...
	f := strings.Fields(s[end+1:])
	if len(f) < 4 || len(f[0]) == 0 {
		return 0, 0, 0, false
	}
	pgrp, err1 := strconv.Atoi(f[2])
	sid, err2 := strconv.Atoi(f[3])
	if err1 != nil || err2 != nil {
		return 0, 0, 0, false
	}
	return f[0][0], pgrp, sid, true
}
