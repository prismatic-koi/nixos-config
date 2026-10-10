package prismcontainer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	// Preflight checks the host before a build. An error refuses the
	// build, and its text tells the agent why.
	Preflight(ctx context.Context) error
	// Build runs podman with args, where args[0] is "build", and writes the
	// podman output to out. The last element of args is the context dir of
	// the build copy. unit names the build for StopBuilds. When ctx ends,
	// Build stops the build and returns a non-nil error.
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
// Linux, PlainExecutor on every other platform. machineAllowlist is the
// list of Mac paths that the podman machine can mount (PlainExecutor).
func DefaultBuildExecutor(goos string, r Runner, machineAllowlist []string) BuildExecutor {
	if r == nil {
		r = ExecRunner{}
	}
	if goos == "linux" {
		return ScopeExecutor{Runner: r}
	}
	return PlainExecutor{Runner: r, MountAllowlist: machineAllowlist}
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
// processes of a build step. Preflight refuses a build when the machine
// mounts a Mac path outside MountAllowlist (machinecheck.go).
//
// A build step that runs when ctx ends can continue in the VM until it
// exits. Its memory and CPU limits still apply, and the VM size bounds it.
// StopBuilds cannot reach it, so it reports zero builds.
type PlainExecutor struct {
	Runner Runner
	// MountAllowlist holds the absolute Mac paths that the machine can
	// mount: a mount source must be one of them or inside one.
	MountAllowlist []string
	// ReadFile reads the machine config file. Nil selects os.ReadFile.
	ReadFile func(string) ([]byte, error)
}

// Build adds --ulimit nproc as the process limit: podman build has no
// --pids-limit. RLIMIT_NPROC counts the processes of one user in the
// rootless user namespace, so other containers of that user count too.
func (e PlainExecutor) Build(ctx context.Context, _ string, out io.Writer, args []string) (int, error) {
	return e.runner().Run(ctx, out, out, insertAfterBuild(args, "--ulimit", "nproc="+PidsLimit+":"+PidsLimit)...)
}

func (PlainExecutor) StopBuilds(context.Context, string) (int, error) { return 0, nil }

// scopeScript runs as the command of the systemd scope. It moves itself
// into a leaf cgroup of the scope, puts the cgroup path of the build in
// place of cgroupToken, and runs its arguments: the namespaced podman
// build command (buildns.go).
//
// Without --cgroup-parent, crun puts a build step into a cgroup outside the
// scope, and a scope kill does not reach it. The scope cgroup must hold no
// process, or the kernel does not let crun enable controllers for the build
// cgroup. --cgroup-manager=cgroupfs is mandatory: with the systemd manager,
// podman build gives crun --systemd-cgroup, and crun then reads the
// --cgroup-parent path as a systemd slice name, which it is not.
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
n=$#
for a; do
	if [ "$a" = "` + cgroupToken + `" ]; then a="$cg/build"; fi
	set -- "$@" "$a"
done
shift "$n"
exec "$@"
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
	// Podman and Bwrap are absolute paths. Empty selects the binary in
	// PATH, with every symlink resolved.
	Podman string
	Bwrap  string
	// Runner runs `podman unshare true` and `podman info` before the
	// build. Nil selects ExecRunner.
	Runner Runner
}

// binary returns the absolute path of a binary: set, or name in PATH with
// every symlink resolved, so that it works in the build namespace.
func binary(set, name string) (string, error) {
	if set != "" {
		return set, nil
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s is not in PATH", name)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %v", p, err)
	}
	return real, nil
}

// Preflight makes sure that the binaries of a Linux build exist.
func (e ScopeExecutor) Preflight(context.Context) error {
	for _, b := range []struct{ set, name string }{{e.SystemdRun, "systemd-run"}, {e.Podman, "podman"}, {e.Bwrap, "bwrap"}} {
		if _, err := binary(b.set, b.name); err != nil {
			return fmt.Errorf("a Linux build needs %s on the host: %v", b.name, err)
		}
	}
	return nil
}

func (e ScopeExecutor) runner() Runner {
	if e.Runner == nil {
		return ExecRunner{}
	}
	return e.Runner
}

// buildView returns the file view of the build whose context dir is
// contextDir. The context dir must be in the build copy dir of prism, so
// that the executor never binds another host path.
func (e ScopeExecutor) buildView(ctx context.Context, contextDir, policy string) (buildView, error) {
	stageRoot, err := container.PrismContainerBuildStageDirPath()
	if err != nil {
		return buildView{}, err
	}
	stageDir := filepath.Dir(contextDir)
	if filepath.Dir(stageDir) != stageRoot {
		return buildView{}, fmt.Errorf("the build context %q is not in the build copy dir %s", contextDir, stageRoot)
	}
	// The pause process keeps the user namespace of podman. Start it now,
	// outside the build scope, so that the scope kill does not stop it.
	var stderr bytes.Buffer
	if code, err := e.runner().Run(ctx, io.Discard, &stderr, "unshare", "true"); err != nil || code != 0 {
		return buildView{}, &podmanError{detail: fmt.Sprintf("podman unshare true failed (exit %d, %v): %s", code, err, strings.TrimSpace(stderr.String()))}
	}
	graphroot, runroot, err := readPodmanStore(ctx, e.runner())
	if err != nil {
		return buildView{}, err
	}
	varTmp := filepath.Join(stageDir, "vartmp")
	if err := os.MkdirAll(varTmp, 0o700); err != nil {
		return buildView{}, err
	}
	return buildView{
		Graphroot:  graphroot,
		Runroot:    runroot,
		RuntimeDir: runtimeDir(),
		ConfigDir:  userContainersConfigDir(),
		StageDir:   stageDir,
		VarTmp:     varTmp,
		Policy:     policy,
	}, nil
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

// BuildPolicy is the signature policy of a Linux build. It rejects every
// transport except a registry and the local store. It is a second refusal
// behind the Containerfile check. The build namespace (buildns.go) is the
// boundary. The macOS podman client has no --signature-policy.
//
// The policy cannot accept containers-storage for the own store only. The
// image that buildah commits has the policy identity "", which matches the
// "" scope only, and a reference to another store falls back to that
// scope too. Thus a scope per store refuses every commit. The build
// namespace stops another store: its path does not exist there.
const BuildPolicy = `{
  "default": [{"type": "reject"}],
  "transports": {
    "docker": {"": [{"type": "insecureAcceptAnything"}]},
    "containers-storage": {"": [{"type": "insecureAcceptAnything"}]}
  }
}
`

// writeBuildPolicy writes BuildPolicy into the prism-container state tree
// and returns its path. A rename puts the file in place, so a build that
// reads it at the same time reads a whole file.
func writeBuildPolicy() (string, error) {
	path, err := container.PrismContainerBuildPolicyPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".build-policy-")
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(BuildPolicy)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(f.Name())
		return "", werr
	}
	return path, nil
}

func (e ScopeExecutor) Build(ctx context.Context, unit string, out io.Writer, args []string) (int, error) {
	policy, err := writeBuildPolicy()
	if err != nil {
		return -1, fmt.Errorf("write the signature policy of the build: %w", err)
	}
	podman, err := binary(e.Podman, "podman")
	if err != nil {
		return -1, err
	}
	bwrap, err := binary(e.Bwrap, "bwrap")
	if err != nil {
		return -1, err
	}
	view, err := e.buildView(ctx, args[len(args)-1], policy)
	if err != nil {
		return -1, err
	}
	command := namespacedBuildCommand(podman, bwrap, view, insertAfterBuild(args, "--signature-policy", policy))
	runArgs := []string{
		"--user", "--scope", "--collect", "--quiet",
		"--unit", unit,
		"--property", "Delegate=yes",
		"--property", "TasksMax=" + PidsLimit,
	}
	// systemd stops the scope after the request timeout plus a grace time.
	// This stops the build when the prism process dies first, as podman
	// --timeout does for a run.
	if deadline, ok := ctx.Deadline(); ok {
		limit := time.Until(deadline) + conmonTimeoutGrace
		runArgs = append(runArgs, "--property", fmt.Sprintf("RuntimeMaxSec=%d", int64((limit+time.Second-1)/time.Second)))
	}
	runArgs = append(runArgs, "--", "/bin/sh", "-c", scopeScript, "prism-container-build")
	cmd := exec.Command(e.systemdRun(), append(runArgs, command...)...)
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
