package review

import (
	"os/exec"
	"syscall"
)

// detachProcess sets the SysProcAttr so that the subprocess is created in a
// new session (setsid), detaching it from the parent's process group and
// controlling terminal. This ensures the monitor process survives after the
// parent `prism review` command exits.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
