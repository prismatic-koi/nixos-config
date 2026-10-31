package prismcontainer

import (
	"os/exec"
	"runtime"
	"syscall"
)

// runPodman runs cmd with Pdeathsig SIGKILL. When the prism process that
// runs podman dies, also by SIGKILL, the kernel kills the podman client.
// Otherwise a podman client of a killed sidecar can create a container
// after the container sweep of the stop.
//
// The kernel sends Pdeathsig when the thread that started the child exits,
// not the process. The goroutine holds its thread until podman exits, so
// that no other goroutine can end that thread first.
func runPodman(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return cmd.Run()
}
