//go:build !linux

package prismcontainer

import "os/exec"

// runPodman runs cmd. This platform has no Pdeathsig. The stop of a
// session sends SIGKILL to the process group of the sidecar instead
// (session.EnsureSidecarStoppedErr).
func runPodman(cmd *exec.Cmd) error {
	return cmd.Run()
}
