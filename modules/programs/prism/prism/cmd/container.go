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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/review"
)

const containerRunUse = "run [flags] <image> -- [command...]"

// containerRunClientMargin is added to the request timeout for the host-API
// client, so that the sidecar finishes the remove and the audit line before
// the client gives up.
const containerRunClientMargin = 2 * time.Minute

var containerCmd = &cobra.Command{
	Use:   "container",
	Short: "Run containers on the host with options that prism sets",
	Long: `Run containers on the host with options that prism sets.

prism container is the way to run a container from a prism session. Prism
builds the podman command itself: the agent gives the image, the command,
environment variables, the worktree mount mode, and a timeout. Prism sets
every other option, including the resource limits and the ownership label.

Load the prism-container skill for the full rules.`,
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
/workspace is the working directory. A short image name such as "alpine"
means docker.io/library/alpine.

Limits: one running container per session, and a host-wide limit (default
4, set by the Nix option ` + prismcontainer.HostLimitNixOption + `). A request
over a limit is refused at once.

Exit codes: the container exit code; 124 when --timeout expires; 125 when
prism refuses the request or podman fails.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runContainerRun,
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
	rootCmd.AddCommand(containerCmd)
}

// parseContainerRunArgs splits the positional arguments into the image and
// the command. A command must follow "--".
func parseContainerRunArgs(args []string) (string, []string, error) {
	image, rest := args[0], args[1:]
	if len(rest) == 0 {
		return image, nil, nil
	}
	if rest[0] != "--" {
		return "", nil, fmt.Errorf("put -- between the image and the command: prism container run %s -- %s", image, rest[0])
	}
	return image, rest[1:], nil
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
// session that has no sidecar socket. Prism resolves the caller from the
// session name and the database, the same facts the sidecar route reads
// from its own config.
func runContainerHostMode(ctx context.Context, req prismcontainer.RunRequest) prismcontainer.RunResult {
	refused := func(msg string) prismcontainer.RunResult {
		return prismcontainer.RunResult{ExitCode: prismcontainer.ExitRefused, Message: "refused: " + msg}
	}
	session := review.LookupParentSession()
	if session == "" {
		return refused("prism cannot determine the calling session: run from inside a prism session")
	}
	d, err := openDB()
	if err != nil {
		return refused("open db: " + err.Error())
	}
	status, err := d.CurrentStatus(session)
	d.Close()
	if err != nil || status == nil {
		return refused(fmt.Sprintf("session %q is not in the prism database", session))
	}
	caller := prismcontainer.Caller{SessionName: session, Worktree: status.Worktree}
	if status.InstanceID != nil {
		caller.InstanceID = *status.InstanceID
	}
	return prismcontainer.Run(ctx, prismcontainer.Deps{
		Runner:    currentPrismContainerRunner(),
		HostLimit: config.LoadFresh().ContainerHostLimit,
	}, caller, req)
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
