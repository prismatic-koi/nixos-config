package prismcontainer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// BuildExecutor runs one podman build. It is the only part of `prism
// container build` that differs between platforms. Build gives every
// executor the same argument vector and reads the same results back, so
// the agent sees the same command on Linux and on macOS.
type BuildExecutor interface {
	// Build runs podman with args, where args[0] is "build", and writes the
	// podman output to out. unit names the build for StopBuilds. When ctx
	// ends, Build stops the build and returns a non-nil error.
	Build(ctx context.Context, unit string, out io.Writer, args []string) (int, error)
	// StopBuilds stops every running build whose unit name starts with
	// prefix, and returns the number of builds it stopped.
	StopBuilds(ctx context.Context, prefix string) (int, error)
}

// stopFailedError reports that an executor did not confirm that every
// process of a build stopped. cause is the error of the build itself, or
// nil when podman exited on its own.
type stopFailedError struct {
	cause error
	err   error
}

func (e *stopFailedError) Error() string {
	if e.cause == nil {
		return e.err.Error()
	}
	return e.cause.Error() + "; " + e.err.Error()
}

func (e *stopFailedError) Unwrap() []error { return []error{e.cause, e.err} }

// DefaultBuildExecutor returns the executor for goos: ScopeExecutor on
// Linux, PlainExecutor on every other platform.
func DefaultBuildExecutor(goos string, r Runner) BuildExecutor {
	if goos == "linux" {
		return ScopeExecutor{}
	}
	if r == nil {
		r = ExecRunner{}
	}
	return PlainExecutor{Runner: r}
}

// buildUnitPrefix returns the unit name prefix of every build of one
// session incarnation.
func buildUnitPrefix(instanceID string) string {
	return "prism-build-" + container.InstanceTokenForID(instanceID) + "-"
}

// insertAfterBuild returns args with opts inserted after args[0].
func insertAfterBuild(args []string, opts ...string) []string {
	out := make([]string, 0, len(args)+len(opts))
	out = append(out, args[0])
	out = append(out, opts...)
	return append(out, args[1:]...)
}

// PlainExecutor runs podman build directly. It is the macOS executor: the
// build runs in the podman machine VM, and the host has no handle on the
// processes of a build step.
//
// A build step that runs when ctx ends can continue in the VM until it
// exits. Its memory and CPU limits still apply, and the VM size bounds it.
// StopBuilds cannot reach it, so it reports zero builds.
type PlainExecutor struct{ Runner Runner }

// Build adds --ulimit nproc as the process limit: podman build has no
// --pids-limit. RLIMIT_NPROC counts the processes of one user in the
// rootless user namespace, so other containers of that user count too.
func (e PlainExecutor) Build(ctx context.Context, _ string, out io.Writer, args []string) (int, error) {
	return e.Runner.Run(ctx, out, out, insertAfterBuild(args, "--ulimit", "nproc="+PidsLimit+":"+PidsLimit)...)
}

func (PlainExecutor) StopBuilds(context.Context, string) (int, error) { return 0, nil }

// scopeScript runs as the command of the systemd scope. It moves itself
// into a leaf cgroup of the scope and then runs podman build with
// --cgroup-parent in the scope.
//
// Without --cgroup-parent, crun puts a rootless build step into a new
// cgroup next to the cgroup of podman, which is outside the scope, and a
// scope kill does not reach it. The scope cgroup must hold no process, or
// the kernel does not let crun enable controllers for the build cgroup.
const scopeScript = `cg=
while IFS= read -r line; do
	case $line in 0::*) cg=${line#0::} ;; esac
done < /proc/self/cgroup
if [ -z "$cg" ] || [ "$cg" = / ]; then
	echo "prism: the build scope has no cgroup v2 path" >&2
	exit 125
fi
mkdir "/sys/fs/cgroup$cg/podman" || exit 125
echo $$ > "/sys/fs/cgroup$cg/podman/cgroup.procs" || exit 125
sub=$1
shift
exec podman "$sub" --cgroup-parent "$cg/build" "$@"
`

// ScopeExecutor runs each build in a transient systemd user scope (Linux).
// Every process of the build, build steps included, is in the cgroup tree
// of the scope, so a kill of the scope stops all of them. A build step
// outlives a killed podman otherwise: buildah moves it to the systemd
// user manager when podman dies.
type ScopeExecutor struct {
	// SystemdRun and Systemctl name the binaries. Empty selects the
	// names in PATH.
	SystemdRun string
	Systemctl  string
}

func (e ScopeExecutor) systemdRun() string {
	if e.SystemdRun != "" {
		return e.SystemdRun
	}
	return "systemd-run"
}

func (e ScopeExecutor) systemctl() string {
	if e.Systemctl != "" {
		return e.Systemctl
	}
	return "systemctl"
}

// scopeEnv returns the environment for systemd-run and systemctl. Both
// need the bus of the user manager. A sidecar environment can lack
// XDG_RUNTIME_DIR, so the standard path is filled in.
func scopeEnv() []string {
	env := os.Environ()
	if os.Getenv("XDG_RUNTIME_DIR") == "" && os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		env = append(env, fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid()))
	}
	return env
}

func (e ScopeExecutor) Build(ctx context.Context, unit string, out io.Writer, args []string) (int, error) {
	runArgs := []string{
		"--user", "--scope", "--collect", "--quiet",
		"--unit", unit,
		"--property", "Delegate=yes",
		"--property", "TasksMax=" + PidsLimit,
		"--", "/bin/sh", "-c", scopeScript, "prism-container-build",
	}
	cmd := exec.Command(e.systemdRun(), append(runArgs, args...)...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = scopeEnv()
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		// A build step can outlive podman. Kill the scope after every
		// build, so that nothing of the build keeps running.
		stopErr := e.stopUnit(unit)
		code, err := scopeExitCode(err)
		if stopErr != nil {
			return code, &stopFailedError{cause: err, err: stopErr}
		}
		return code, err
	case <-ctx.Done():
		stopErr := e.stopUnit(unit)
		_ = cmd.Process.Kill()
		<-done
		// The scope can appear after the first kill, while systemd-run
		// still registers it.
		if err := e.stopUnit(unit); err != nil {
			stopErr = err
		}
		if stopErr != nil {
			return -1, &stopFailedError{cause: ctx.Err(), err: stopErr}
		}
		return -1, ctx.Err()
	}
}

// scopeExitCode converts the result of systemd-run. A podman that a signal
// killed did not run to completion, so that is an error, not an exit code.
func scopeExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return -1, err
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -1, fmt.Errorf("podman build was killed by signal %s", ws.Signal())
	}
	return exitErr.ExitCode(), nil
}

// stopUnit kills every process of the scope of one build. A scope that is
// gone is not an error.
func (e ScopeExecutor) stopUnit(unit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.systemctl(), "--user", "kill", "--signal=SIGKILL", unit+".scope")
	cmd.Env = scopeEnv()
	out, err := cmd.CombinedOutput()
	if err != nil && !bytes.Contains(out, []byte("not loaded")) {
		return fmt.Errorf("prism did not confirm that build %s is stopped: systemctl kill: %v: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (e ScopeExecutor) StopBuilds(ctx context.Context, prefix string) (int, error) {
	cmd := exec.CommandContext(ctx, e.systemctl(), "--user", "list-units", "--all", "--plain", "--no-legend", "--no-pager",
		"--type=scope", prefix+"*")
	cmd.Env = scopeEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("systemctl list-units: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var stopped int
	var errs []error
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], prefix) {
			continue
		}
		if err := e.stopUnit(strings.TrimSuffix(fields[0], ".scope")); err != nil {
			errs = append(errs, err)
			continue
		}
		stopped++
	}
	return stopped, errors.Join(errs...)
}
