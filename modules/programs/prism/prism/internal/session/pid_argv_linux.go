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
