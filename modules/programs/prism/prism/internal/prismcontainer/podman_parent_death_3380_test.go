package prismcontainer

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestExecRunner_PrismDeathKillsPodman_3380: when the process that runs a
// podman client dies by SIGKILL, the kernel kills the client (Pdeathsig).
// A podman client of a killed sidecar can otherwise create a container
// after the container sweep of the stop. A helper process runs
// ExecRunner, and the test kills the helper.
func TestExecRunner_PrismDeathKillsPodman_3380(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Pdeathsig is Linux only")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	helper := exec.Command(os.Args[0], "-test.run=^TestExecRunnerHelperProcess_3380$")
	helper.Env = append(os.Environ(), "PRISM_EXEC_RUNNER_HELPER_CLIENT="+sleepingClient(t, pidFile))
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitPIDFile(t, pidFile)
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	waitGone(t, pid, "after a SIGKILL of the process that runs ExecRunner")
}

// TestExecRunnerHelperProcess_3380 is the helper process of
// TestExecRunner_PrismDeathKillsPodman_3380. It does nothing in a normal
// test run.
func TestExecRunnerHelperProcess_3380(t *testing.T) {
	client := os.Getenv("PRISM_EXEC_RUNNER_HELPER_CLIENT")
	if client == "" {
		return
	}
	podmanCommand = client
	_, _ = ExecRunner{}.Run(context.Background(), io.Discard, io.Discard, "run")
	os.Exit(0)
}
