package cmd

// prism container — run containers on the host with options that prism
// sets. See internal/prismcontainer for the argument builder, the limits,
// and the audit log.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/review"
)

const (
	containerRunUse   = "run [flags] <image> -- [command...]"
	containerBuildUse = "build [flags] [context]"
	containerStartUse = "start [flags] <image> -- [command...]"
	containerExecUse  = "exec [flags] <handle> -- <command...>"
)

// containerServiceClientTimeout is the host-API client timeout of start,
// logs, stop, and list. It covers the image pull of start.
const containerServiceClientTimeout = 12 * time.Minute

// containerRunClientMargin is added to the request timeout for the host-API
// client, so that the sidecar finishes the remove and the audit line before
// the client gives up.
const containerRunClientMargin = 2 * time.Minute

var containerCmd = &cobra.Command{
	Use:   "container",
	Short: "Run containers, build images, and start service containers on the host with options that prism sets",
	Long: `Run containers, build images, and start service containers on the host
with options that prism sets.

prism container is the way to run a container, build an image, or start a
service container from a prism session. Prism builds the podman command itself from a small set of
inputs. Prism sets every other option, including the resource limits and
the ownership label.

Load the prism-container skill for the full rules.`,
}

var containerBuildCmd = &cobra.Command{
	Use:   containerBuildUse,
	Short: "Build an image from a Containerfile in the worktree",
	Long: `Build an image from a Containerfile in the worktree.

CONTEXT is the build context directory, relative to the worktree. The
default is the worktree root. Without --file, prism uses Containerfile,
then Dockerfile, in the context directory. Prism builds from a copy of the
context and of the Containerfile.

The image name is localhost/prism-<instance token>-<session>-<tag>. With no
--tag, prism makes a tag. On success, stdout holds the image name only, and
the build output goes to stderr. Give the image name to prism container run.

Each build step has the same memory, swap, CPU, and process limits as
prism container run. A build counts as one container toward the
per-session limit and the host-wide limit (default 4, set by the Nix option
` + prismcontainer.HostLimitNixOption + `).

Exit codes: 0 when the build succeeds; the podman build exit code when the
build fails; 124 when --timeout expires; 125 when prism refuses the request
or podman fails.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runContainerBuild,
}

var containerRunCmd = &cobra.Command{
	Use:   containerRunUse,
	Short: "Run one command in a new container, then remove the container",
	Long: `Run one command in a new container, then remove the container.

Put "--" between the image and the command. Prism passes the command to the
container as an argument vector; no shell parses it. The container stdout and
stderr go to the stdout and stderr of this command, and the exit code is the
exit code of the container command.

The session worktree is mounted at /workspace (read-only by default), and
/workspace is the working directory. Prism refuses --mount rw for a session
with a read-only worktree (a review agent or an investigator). A short image
name such as "alpine" means docker.io/library/alpine.

Limits: one running container per session, and a host-wide limit (default
4, set by the Nix option ` + prismcontainer.HostLimitNixOption + `). A request
over a limit is refused at once.

Exit codes: the container exit code; 124 when --timeout expires; 125 when
prism refuses the request or podman fails.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runContainerRun,
}

var containerStartCmd = &cobra.Command{
	Use:   containerStartUse,
	Short: "Start a service container that runs until stop or cleanup",
	Long: `Start a service container, for example a database, that runs until
prism container stop or prism cleanup removes it.

The image, the command, --env, and --mount work as in prism container run.
Each --port publishes one container port on 127.0.0.1 with a random host
port. On success, stdout holds one line "handle HANDLE" and one line
"port CONTAINER_PORT 127.0.0.1:HOST_PORT" for each --port. Give the handle
to prism container exec, logs, and stop. Connect to 127.0.0.1:HOST_PORT
from the sandbox.

A service container has no timeout. It counts as one container toward the
per-session limit and the host-wide limit (default 4, set by the Nix option
` + prismcontainer.HostLimitNixOption + `) while it runs. Stop it when you no
longer need it.

Exit codes: 0 when the container started; 125 when prism refuses the
request, the pull fails, or podman fails.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runContainerStart,
}

var containerExecCmd = &cobra.Command{
	Use:   containerExecUse,
	Short: "Run one command in a service container",
	Long: `Run one command in a service container that prism container start
started. HANDLE is the handle that start printed.

Put "--" between the handle and the command. No shell parses the command.
The stdout, the stderr, and the exit code are those of the command. exec
starts no container, so no limit refuses it.

Exit codes: the exit code of the command; 124 when --timeout expires; 125
when prism refuses the request or podman fails.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runContainerExec,
}

var containerLogsCmd = &cobra.Command{
	Use:   "logs <handle>",
	Short: "Print the logs of a service container",
	Long: `Print the logs of a service container that prism container start
started. The container stdout goes to stdout, and the container stderr
goes to stderr. Prism keeps the last 1 MiB of each stream.`,
	Args: cobra.ExactArgs(1),
	RunE: runContainerLogs,
}

var containerStopCmd = &cobra.Command{
	Use:   "stop <handle>",
	Short: "Stop and remove a service container",
	Long: `Stop and remove a service container that prism container start
started. Podman gives the service 10 seconds to stop, then kills it.`,
	Args: cobra.ExactArgs(1),
	RunE: runContainerStop,
}

var containerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the containers and the built images of this session",
	Long: `List the containers and the built images of this session.

For a service container, HANDLE is the handle for exec, logs, and stop.
For a container of prism container run, HANDLE is the container name.
Give a built image name to prism container run or prism container start.`,
	Args: cobra.NoArgs,
	RunE: runContainerList,
}

func init() {
	f := containerRunCmd.Flags()
	f.String("mount", string(prismcontainer.MountRO), "How the session worktree appears at /workspace: none, ro, or rw")
	f.StringArray("env", nil, "Set an environment variable in the container, as KEY=VALUE (repeatable)")
	f.Duration("timeout", prismcontainer.DefaultTimeout, "Stop and remove the container after this time (maximum 60m)")
	// Parsing stops at the image, so that a command argument is never read
	// as a prism flag.
	f.SetInterspersed(false)
	containerCmd.AddCommand(containerRunCmd)

	b := containerBuildCmd.Flags()
	b.StringP("file", "f", "", "Containerfile, relative to the worktree (default: Containerfile, then Dockerfile, in the context)")
	b.StringArray("build-arg", nil, "Set a build argument, as KEY=VALUE (repeatable)")
	b.StringP("tag", "t", "", "Image name after the session prefix, as NAME or NAME:TAG (default: a generated name)")
	b.Duration("timeout", prismcontainer.DefaultBuildTimeout, "Stop the build after this time (maximum 60m)")
	containerCmd.AddCommand(containerBuildCmd)

	s := containerStartCmd.Flags()
	s.String("mount", string(prismcontainer.MountRO), "How the session worktree appears at /workspace: none, ro, or rw")
	s.StringArray("env", nil, "Set an environment variable in the container, as KEY=VALUE (repeatable)")
	s.StringArray("port", nil, "Publish a container port on 127.0.0.1 with a random host port (repeatable)")
	s.SetInterspersed(false)
	containerCmd.AddCommand(containerStartCmd)

	e := containerExecCmd.Flags()
	e.Duration("timeout", prismcontainer.DefaultTimeout, "Stop podman exec after this time (maximum 60m)")
	e.SetInterspersed(false)
	containerCmd.AddCommand(containerExecCmd)

	containerCmd.AddCommand(containerLogsCmd, containerStopCmd, containerListCmd)

	rootCmd.AddCommand(containerCmd)
}

func runContainerBuild(cmd *cobra.Command, args []string) error {
	file, _ := cmd.Flags().GetString("file")
	buildArgs, _ := cmd.Flags().GetStringArray("build-arg")
	tag, _ := cmd.Flags().GetString("tag")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	req := prismcontainer.BuildRequest{
		File:           file,
		BuildArgs:      buildArgs,
		Tag:            tag,
		TimeoutSeconds: timeoutSeconds(timeout),
	}
	if len(args) == 1 {
		req.Context = args[0]
	}

	var res prismcontainer.BuildResult
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		if err := proxyToHostAPIWithTimeout(apiURL, "/container/build", req, &res, timeout+containerRunClientMargin); err != nil {
			return newExitErr(prismcontainer.ExitRefused, "prism container build: "+err.Error())
		}
	} else {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		res = buildContainerHostMode(ctx, req)
	}
	return finishContainerBuild(os.Stdout, os.Stderr, res)
}

// finishContainerBuild writes the result and returns the exit code as an
// error, so that main exits with it.
func finishContainerBuild(stdout, stderr io.Writer, res prismcontainer.BuildResult) error {
	prismcontainer.WriteBuildResult(stdout, stderr, res)
	if res.ExitCode != 0 {
		return newExitErr(res.ExitCode, "")
	}
	return nil
}

// parseContainerRunArgs splits the positional arguments into the image and
// the command. A command must follow "--".
func parseContainerRunArgs(args []string) (string, []string, error) {
	return parseContainerArgs("run", "image", args)
}

// parseContainerArgs splits the positional arguments of `prism container
// <sub>` into the first argument (an image or a handle) and the command.
// A command must follow "--".
func parseContainerArgs(sub, first string, args []string) (string, []string, error) {
	head, rest := args[0], args[1:]
	if len(rest) == 0 {
		return head, nil, nil
	}
	if rest[0] != "--" {
		return "", nil, fmt.Errorf("put -- between the %s and the command: prism container %s %s -- %s", first, sub, head, rest[0])
	}
	return head, rest[1:], nil
}

func runContainerRun(cmd *cobra.Command, args []string) error {
	image, command, err := parseContainerRunArgs(args)
	if err != nil {
		return newExitErr(prismcontainer.ExitRefused, "prism container run: "+err.Error())
	}
	mount, _ := cmd.Flags().GetString("mount")
	env, _ := cmd.Flags().GetStringArray("env")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	req := prismcontainer.RunRequest{
		Image:          image,
		Command:        command,
		Env:            env,
		Mount:          prismcontainer.Mount(mount),
		TimeoutSeconds: timeoutSeconds(timeout),
	}

	var res prismcontainer.RunResult
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		if err := proxyToHostAPIWithTimeout(apiURL, "/container/run", req, &res, timeout+containerRunClientMargin); err != nil {
			return newExitErr(prismcontainer.ExitRefused, "prism container run: "+err.Error())
		}
	} else {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		res = runContainerHostMode(ctx, req)
	}
	return finishContainerRun(os.Stdout, os.Stderr, res)
}

// timeoutSeconds converts --timeout for the request. Run validates the
// value, so that a refusal writes an audit line. A part second rounds up.
// Zero means "default" on the wire, so a zero or negative flag value maps
// to -1, which Run refuses.
func timeoutSeconds(d time.Duration) int64 {
	if d <= 0 {
		return -1
	}
	return int64((d + time.Second - 1) / time.Second)
}

// finishContainerRun writes the result and returns the exit code as an
// error, so that main exits with the container exit code.
func finishContainerRun(stdout, stderr io.Writer, res prismcontainer.RunResult) error {
	prismcontainer.WriteResult(stdout, stderr, res)
	if res.ExitCode != 0 {
		return newExitErr(res.ExitCode, "")
	}
	return nil
}

// runContainerHostMode runs the request in this process, for a host-mode
// session that has no PRISM_HOST_API. Prism resolves the caller from the
// session name and the database, the same facts the sidecar route reads
// from its own config.
func runContainerHostMode(ctx context.Context, req prismcontainer.RunRequest) prismcontainer.RunResult {
	caller, msg := hostModeContainerCaller()
	if msg != "" {
		return prismcontainer.RunResult{ExitCode: prismcontainer.ExitRefused, Message: "refused: " + msg}
	}
	return prismcontainer.Run(ctx, hostModeContainerDeps(), caller, req)
}

// buildContainerHostMode is the host-mode route of `prism container build`.
func buildContainerHostMode(ctx context.Context, req prismcontainer.BuildRequest) prismcontainer.BuildResult {
	caller, msg := hostModeContainerCaller()
	if msg != "" {
		return prismcontainer.BuildResult{ExitCode: prismcontainer.ExitRefused, Message: "refused: " + msg}
	}
	return prismcontainer.Build(ctx, hostModeContainerDeps(), caller, req)
}

// hostModeContainerCaller resolves the calling session from the session
// name and the database. It returns a refusal reason when it cannot.
func hostModeContainerCaller() (prismcontainer.Caller, string) {
	session := review.LookupParentSession()
	if session == "" {
		return prismcontainer.Caller{}, "prism cannot determine the calling session: run from inside a prism session"
	}
	d, err := openDB()
	if err != nil {
		return prismcontainer.Caller{}, "open db: " + err.Error()
	}
	status, err := d.CurrentStatus(session)
	if err != nil || status == nil {
		d.Close()
		return prismcontainer.Caller{}, fmt.Sprintf("session %q is not in the prism database", session)
	}
	readOnly, err := d.WorktreeReadOnly(session)
	d.Close()
	if err != nil {
		return prismcontainer.Caller{}, err.Error()
	}
	caller := prismcontainer.Caller{SessionName: session, Worktree: status.Worktree, WorktreeReadOnly: readOnly}
	if status.InstanceID != nil {
		caller.InstanceID = *status.InstanceID
	}
	return caller, ""
}

func hostModeContainerDeps() prismcontainer.Deps {
	return prismcontainer.Deps{
		Runner:        currentPrismContainerRunner(),
		BuildExecutor: currentPrismContainerBuildExecutor(),
		HostLimit:     config.LoadFresh().ContainerHostLimit,
	}
}

// prismContainerRunnerForTest replaces the podman runner of the host-mode
// route and of the cleanup sweep. Nil selects prismcontainer.ExecRunner.
var prismContainerRunnerForTest prismcontainer.Runner

func currentPrismContainerRunner() prismcontainer.Runner {
	if prismContainerRunnerForTest != nil {
		return prismContainerRunnerForTest
	}
	return prismcontainer.ExecRunner{}
}

// prismContainerBuildExecutorForTest replaces the build executor of the
// host-mode route and of the cleanup sweep. Nil selects the executor of
// the host platform.
var prismContainerBuildExecutorForTest prismcontainer.BuildExecutor

func currentPrismContainerBuildExecutor() prismcontainer.BuildExecutor {
	if prismContainerBuildExecutorForTest != nil {
		return prismContainerBuildExecutorForTest
	}
	return prismcontainer.DefaultBuildExecutor(runtime.GOOS, currentPrismContainerRunner(), config.LoadFresh().MachineMountAllowlist())
}

// containerRequest sends req to the sidecar endpoint of a sandboxed
// session. For a host-mode session it calls host in this process, with
// the caller that the database names. refused makes the result of a
// request that did not reach prismcontainer.
func containerRequest[Req, Res any](endpoint string, req Req, clientTimeout time.Duration,
	host func(context.Context, prismcontainer.Deps, prismcontainer.Caller, Req) Res, refused func(string) Res) Res {
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		var res Res
		if err := proxyToHostAPIWithTimeout(apiURL, endpoint, req, &res, clientTimeout); err != nil {
			return refused(err.Error())
		}
		return res
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	caller, msg := hostModeContainerCaller()
	if msg != "" {
		return refused("refused: " + msg)
	}
	return host(ctx, hostModeContainerDeps(), caller, req)
}

func refusedRun(msg string) prismcontainer.RunResult {
	return prismcontainer.RunResult{ExitCode: prismcontainer.ExitRefused, Message: msg}
}

// exitWith returns the exit code as an error, so that main exits with it.
func exitWith(code int) error {
	if code != 0 {
		return newExitErr(code, "")
	}
	return nil
}

func runContainerStart(cmd *cobra.Command, args []string) error {
	image, command, err := parseContainerArgs("start", "image", args)
	if err != nil {
		return newExitErr(prismcontainer.ExitRefused, "prism container start: "+err.Error())
	}
	mount, _ := cmd.Flags().GetString("mount")
	env, _ := cmd.Flags().GetStringArray("env")
	ports, _ := cmd.Flags().GetStringArray("port")
	req := prismcontainer.StartRequest{Image: image, Command: command, Env: env, Mount: prismcontainer.Mount(mount), Ports: ports}
	res := containerRequest("/container/start", req, containerServiceClientTimeout, prismcontainer.Start,
		func(msg string) prismcontainer.StartResult {
			return prismcontainer.StartResult{ExitCode: prismcontainer.ExitRefused, Message: msg}
		})
	prismcontainer.WriteStartResult(os.Stdout, os.Stderr, res)
	return exitWith(res.ExitCode)
}

func runContainerExec(cmd *cobra.Command, args []string) error {
	handle, command, err := parseContainerArgs("exec", "handle", args)
	if err == nil && len(command) == 0 {
		err = fmt.Errorf("put -- and a command after the handle: prism container exec %s -- COMMAND", handle)
	}
	if err != nil {
		return newExitErr(prismcontainer.ExitRefused, "prism container exec: "+err.Error())
	}
	timeout, _ := cmd.Flags().GetDuration("timeout")
	req := prismcontainer.ExecRequest{Handle: handle, Command: command, TimeoutSeconds: timeoutSeconds(timeout)}
	res := containerRequest("/container/exec", req, timeout+containerRunClientMargin, prismcontainer.Exec, refusedRun)
	prismcontainer.WriteCommandResult("exec", os.Stdout, os.Stderr, res)
	return exitWith(res.ExitCode)
}

func runContainerLogs(_ *cobra.Command, args []string) error {
	res := containerRequest("/container/logs", prismcontainer.HandleRequest{Handle: args[0]}, containerServiceClientTimeout, prismcontainer.Logs, refusedRun)
	prismcontainer.WriteCommandResult("logs", os.Stdout, os.Stderr, res)
	return exitWith(res.ExitCode)
}

func runContainerStop(_ *cobra.Command, args []string) error {
	res := containerRequest("/container/stop", prismcontainer.HandleRequest{Handle: args[0]}, containerServiceClientTimeout, prismcontainer.Stop, refusedRun)
	prismcontainer.WriteCommandResult("stop", os.Stdout, os.Stderr, res)
	return exitWith(res.ExitCode)
}

func runContainerList(_ *cobra.Command, _ []string) error {
	res := containerRequest("/container/list", struct{}{}, containerServiceClientTimeout,
		func(ctx context.Context, d prismcontainer.Deps, c prismcontainer.Caller, _ struct{}) prismcontainer.ListResult {
			return prismcontainer.List(ctx, d, c)
		},
		func(msg string) prismcontainer.ListResult {
			return prismcontainer.ListResult{ExitCode: prismcontainer.ExitRefused, Message: msg}
		})
	prismcontainer.WriteListResult(os.Stdout, os.Stderr, res)
	return exitWith(res.ExitCode)
}
