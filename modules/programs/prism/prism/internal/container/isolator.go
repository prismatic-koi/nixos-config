// Package container manages the podman container lifecycle for prism sidecar.
// This file defines the Isolator interface, which is the seam between the
// Manager and the underlying isolation mechanism. The only implementation
// currently is podmanIsolator, which wraps rootless podman.
package container

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Isolator is the interface that wraps the isolation-specific operations needed
// by Manager to create and manage an agent session container.
//
// An Isolator is responsible for:
//   - Building the argument list for launching the isolated process.
//   - Executing the launch command.
//   - Shutting down the isolated process cleanly.
//   - Checking whether the isolated process has exited.
//   - Dumping logs from the isolated process for diagnostics.
//
// Manager constructs one Isolator per session and calls its methods
// exclusively — no direct exec.CommandContext("podman", ...) calls remain in
// container.go after this interface is in place.
type Isolator interface {
	// BuildRunArgs returns the complete argument list for launching the isolated
	// session process (e.g. all arguments after the "podman" binary for a
	// podman run invocation). The returned slice must not be modified by the
	// caller.
	BuildRunArgs() []string

	// Run launches the isolated process with the given argument list, using the
	// provided context for cancellation. args is the value returned by
	// BuildRunArgs. Returns an error if the process fails to start or exits
	// with a non-zero status.
	Run(ctx context.Context, args []string) error

	// Shutdown stops and removes the isolated process. It must use a
	// background context internally so cleanup proceeds even when the parent
	// context is already cancelled.
	Shutdown()

	// HasExited returns (true, exitCode) when the isolated process has
	// terminated, or (false, 0) when it is still running or its state cannot
	// be determined.
	HasExited() (bool, int)

	// DumpLogs writes the isolated process's stdout/stderr to the sidecar log
	// so startup failures are visible without racing the cleanup path.
	DumpLogs()
}

// podmanIsolator implements Isolator using rootless podman.
type podmanIsolator struct {
	// name is the container name (stable, derived from the session name).
	name string
}

// newPodmanIsolator returns an Isolator that manages a rootless podman
// container with the given stable container name.
func newPodmanIsolator(name string) Isolator {
	return &podmanIsolator{name: name}
}

// BuildRunArgs returns the argument list for "podman run …" built by the
// Manager. The Manager calls this and passes the result to Run.
//
// Note: the argument construction logic lives in Manager.buildRunArgs, not
// here, because it depends on Manager state (cfg, allowedSignersReady,
// claudeCredentialsReady, temp file paths, etc.). BuildRunArgs is therefore
// a thin forwarder — it is not called by Manager directly; instead Manager
// calls its own buildRunArgs and hands the result to Run.
//
// This method satisfies the interface contract but is only called when an
// Isolator is used standalone (e.g. in tests that exercise the interface
// directly). In the normal Manager flow, Manager.buildRunArgs() is called
// and the result is passed to Run.
func (p *podmanIsolator) BuildRunArgs() []string {
	// This method is intentionally minimal: the real arg-building logic lives
	// in Manager.buildRunArgs() which has access to all the Manager state
	// (cfg fields, temp file paths, allowedSignersReady flag, etc.). The
	// interface requires this method so that future isolator implementations
	// can build their own arg lists independently.
	return nil
}

// Run executes "podman <args...>" and waits for it to complete. Stdout and
// stderr are forwarded to the sidecar's stderr log. Returns a wrapped error
// on failure, preserving the same message shape as the pre-refactor inline
// code: "container: podman run %q: %w".
func (p *podmanIsolator) Run(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "podman", args...)
	cmd.Stdout = os.Stderr // forward container stdout to sidecar's stderr log
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container: podman run %q: %w", p.name, err)
	}
	return nil
}

// Shutdown stops and removes the podman container. It uses a background
// context with a 30-second timeout so cleanup proceeds even after the parent
// context has been cancelled.
func (p *podmanIsolator) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stopCmd := exec.CommandContext(ctx, "podman", "stop", "--time", "10", p.name)
	if out, err := stopCmd.CombinedOutput(); err != nil && !IsNoSuchContainerError(string(out)) {
		log.Printf("container: stop %q: %v — %s", p.name, err, strings.TrimSpace(string(out)))
	}

	rmCmd := exec.CommandContext(ctx, "podman", "rm", "--force", p.name)
	if out, err := rmCmd.CombinedOutput(); err != nil && !IsNoSuchContainerError(string(out)) {
		log.Printf("container: rm %q: %v — %s", p.name, err, strings.TrimSpace(string(out)))
	}
}

// HasExited checks whether the podman container has stopped. Returns
// (true, exitCode) when the container is in "exited" state, or (false, 0)
// when it is still running or when the inspect call fails.
func (p *podmanIsolator) HasExited() (bool, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "inspect",
		"--format", "{{.State.Status}} {{.State.ExitCode}}",
		p.name,
	).Output()
	if err != nil {
		return false, 0
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 2 && fields[0] == "exited" {
		code := 0
		fmt.Sscanf(fields[1], "%d", &code)
		return true, code
	}
	return false, 0
}

// DumpLogs fetches and logs the container's stdout/stderr via "podman logs".
// It is called on startup failure so that error output is visible without
// racing the container removal in Shutdown.
func (p *podmanIsolator) DumpLogs() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "logs", p.name).CombinedOutput()
	if err != nil {
		log.Printf("container: could not fetch logs for %q: %v", p.name, err)
		return
	}
	log.Printf("container: logs for %q:\n%s", p.name, string(out))
}
