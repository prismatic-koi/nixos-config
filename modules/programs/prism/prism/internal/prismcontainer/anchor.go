package prismcontainer

// The macOS build executor: an anchor container.
//
// On macOS the podman CLI is a remote client, and podman build runs in the
// podman machine VM. Buildah runs each RUN step under its own runtime
// process, so a step outlives a killed client. AnchorExecutor first starts
// an anchor container with the fixed limits, then runs podman build with
// --cgroup-parent <anchor cgroup>/build. Each RUN step is then in the
// systemd scope of the anchor, and the pids limit of the scope counts the
// processes of this build only. When prism removes the anchor, systemd
// stops the scope and kills every process in it.
//
// crun creates a --cgroup-parent path that does not exist. A RUN step that
// starts after the scope is gone thus runs with no anchor and no process
// limit. The VM starts no further step of a build after its client
// disconnects. Thus prism stops the client before it removes the anchor,
// on every path:
//
//   - At the end of a build, at the timeout, and at a cancel, the build
//     process stops the client, then removes the anchor.
//   - clientGuardScript kills the client when the prism process dies. The
//     anchor has a podman --timeout, so its scope stops on its own later.
//   - StopBuilds (prism cleanup) pauses the anchor, which keeps the scope.
//     The build process sees the pause, stops its client, and removes the
//     anchor.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// clientGuardName is $0 of clientGuardScript.
const clientGuardName = "prism-container-build-client"

// clientGuardScript runs its arguments, the podman build client, and kills
// the client when its stdin reaches EOF. The stdin is a pipe from prism,
// and the kernel closes it when the prism process dies, also at SIGKILL.
// The script kills the watcher after the client exits, so that the watcher
// cannot signal a reused PID.
const clientGuardScript = `exec 3<&0 </dev/null
"$@" 3<&- &
client=$!
(read -r _ <&3; kill -KILL "$client") 2>/dev/null &
watcher=$!
exec 3<&-
wait "$client"
code=$?
kill "$watcher" 2>/dev/null
exit "$code"
`

// GuardedClient runs the podman build client under clientGuardScript, in a
// process group of its own. When ctx ends, it kills the process group.
type GuardedClient struct {
	// Podman names the podman binary. Empty selects podman in PATH.
	Podman string
}

func (g GuardedClient) Run(ctx context.Context, stdout, stderr io.Writer, args ...string) (int, error) {
	podman := g.Podman
	if podman == "" {
		podman = "podman"
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return -1, err
	}
	// Close the write end only after the guard has exited: its EOF tells
	// the guard that prism is gone.
	defer pw.Close()
	cmd := exec.Command("/bin/sh", append([]string{"-c", clientGuardScript, clientGuardName, podman}, args...)...)
	cmd.Stdin = pr
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Start()
	_ = pr.Close()
	if err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buildExitCode(err)
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return -1, ctx.Err()
	}
}

// defaultStopGrace is the time that StopBuilds waits for a live build
// process to remove its paused anchor. It must fit in the cleanup budget
// of 30 seconds, with time for the container and image sweeps after it.
const defaultStopGrace = 15 * time.Second

// AnchorExecutor runs each build under an anchor container (macOS). See
// the comment at the top of this file. Preflight refuses a build when the
// machine mounts a Mac path outside MountAllowlist (machinecheck.go).
type AnchorExecutor struct {
	// Runner runs the podman calls for the anchor. Nil selects ExecRunner.
	Runner Runner
	// Client runs podman build. Nil selects GuardedClient. A Client must
	// stop podman build before it returns when ctx ends.
	Client Runner
	// MountAllowlist holds the absolute Mac paths that the machine can
	// mount: a mount source must be one of them or inside one.
	MountAllowlist []string
	// ReadFile reads the machine config file. Nil selects os.ReadFile.
	ReadFile func(string) ([]byte, error)
	// StopGrace is the time that StopBuilds waits for a live build process
	// to remove its paused anchor. Zero selects defaultStopGrace.
	StopGrace time.Duration
}

func (e AnchorExecutor) client() Runner {
	if e.Client == nil {
		return GuardedClient{}
	}
	return e.Client
}

func (e AnchorExecutor) stopGrace() time.Duration {
	if e.StopGrace <= 0 {
		return defaultStopGrace
	}
	return e.StopGrace
}

// Build starts the anchor, runs podman build in its scope, and removes the
// anchor. The anchor gets the instance label of the image (buildArgs), so
// that the limits and the cleanup sweep find it.
func (e AnchorExecutor) Build(ctx context.Context, unit string, out io.Writer, args []string) (int, error) {
	id := buildArgsInstanceID(args)
	if container.InstanceTokenForID(id) == "" || !strings.HasPrefix(unit, buildUnitPrefix(id)) {
		return -1, notStarted("prism cannot start the anchor container of build %s: the build arguments hold no valid %s label", unit, LabelInstanceID)
	}
	var pullOut bytes.Buffer
	code, err := ensureImage(ctx, e.runner(), AnchorImage, &pullOut)
	switch {
	case err != nil && ctx.Err() != nil:
		return -1, ctx.Err()
	case err != nil:
		return -1, notStarted("podman did not check the anchor image %s: %v", AnchorImage, err)
	case code != 0:
		return -1, notStarted("the pull of the anchor image %s failed (exit %d): %s", AnchorImage, code, strings.TrimSpace(pullOut.String()))
	}
	cgroup, err := e.startAnchor(ctx, id, unit)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			err = notStarted("%v", err)
		}
		return -1, withStopError(err, e.removeAnchor(unit))
	}
	code, err = e.runClient(ctx, unit, out, insertAfterBuild(args, "--cgroup-parent", cgroup+"/build"))
	return code, withStopError(err, e.removeAnchor(unit))
}

// buildArgsInstanceID returns the value of the instance label in a podman
// build argument vector, or "".
func buildArgsInstanceID(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if v, ok := strings.CutPrefix(args[i+1], LabelInstanceID+"="); ok && args[i] == "--label" {
			return v
		}
	}
	return ""
}

func withStopError(err, stopErr error) error {
	if stopErr == nil {
		return err
	}
	return &stopFailedError{cause: err, err: stopErr}
}

// anchorTimeout is the podman --timeout of the anchor: the time to the
// request deadline plus conmonTimeoutGrace. It stops the build steps when
// the prism process dies first. podman reads --timeout 0 as no timeout,
// so the result is at least one second.
func anchorTimeout(ctx context.Context) time.Duration {
	d := MaxTimeout + conmonTimeoutGrace
	if deadline, ok := ctx.Deadline(); ok {
		d = time.Until(deadline) + conmonTimeoutGrace
	}
	return max(d, time.Second)
}

var (
	containerIDPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	// cgroupPathPattern matches a cgroup path that is one safe
	// --cgroup-parent value.
	cgroupPathPattern = regexp.MustCompile(`^(/[A-Za-z0-9@._:-]+)+$`)
)

// startAnchor starts the anchor container of one build and returns the
// cgroup path of its scope.
func (e AnchorExecutor) startAnchor(ctx context.Context, instanceID, unit string) (string, error) {
	r := e.runner()
	var stdout, stderr bytes.Buffer
	code, err := r.Run(ctx, &stdout, &stderr, anchorRunArgs(instanceID, unit, anchorTimeout(ctx))...)
	if err != nil {
		return "", fmt.Errorf("podman run of the anchor container failed: %v", err)
	}
	if code != 0 {
		return "", fmt.Errorf("podman run of the anchor container exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	stdout.Reset()
	stderr.Reset()
	code, err = r.Run(ctx, &stdout, &stderr, "inspect", "--type", "container", "--format", "{{.Id}} {{.State.Status}} {{.State.CgroupPath}}", unit)
	if err != nil || code != 0 {
		return "", fmt.Errorf("podman inspect of the anchor container failed (exit %d, %v): %s", code, err, strings.TrimSpace(stderr.String()))
	}
	// The cgroup must be the own scope of the anchor. A shared cgroup, for
	// example the parent slice, puts the build steps outside the scope
	// that the removal of the anchor stops.
	f := strings.Fields(stdout.String())
	if len(f) != 3 || f[1] != "running" || !containerIDPattern.MatchString(f[0]) ||
		!cgroupPathPattern.MatchString(f[2]) || path.Clean(f[2]) != f[2] || !strings.Contains(path.Base(f[2]), f[0]) {
		return "", fmt.Errorf("the anchor container has no cgroup of its own that prism can give to podman build: podman inspect printed %q", strings.TrimSpace(stdout.String()))
	}
	return f[2], nil
}

// runClient runs podman build. It stops podman build when ctx ends, or
// when the anchor stops running: StopBuilds pauses the anchor, and the
// anchor timeout stops it.
func (e AnchorExecutor) runClient(ctx context.Context, unit string, out io.Writer, args []string) (int, error) {
	clientCtx, stopClient := context.WithCancel(ctx)
	defer stopClient()
	watchCtx, stopWatch := context.WithCancel(context.Background())
	ended := make(chan string, 1)
	go func() {
		why := e.watchAnchor(watchCtx, unit)
		if why != "" {
			stopClient()
		}
		ended <- why
	}()
	code, err := e.client().Run(clientCtx, out, out, args...)
	stopWatch()
	why := <-ended
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if why != "" && errors.Is(err, context.Canceled) {
		return -1, fmt.Errorf("the anchor container of the build %s, so prism stopped podman build. prism cleanup stops a build in this way", why)
	}
	return code, err
}

// watchAnchor waits until the anchor stops running, and returns the
// reason. It returns "" when ctx ends first. Do not add the condition
// "stopping": podman 5.8 reports it as met for a running container.
func (e AnchorExecutor) watchAnchor(ctx context.Context, unit string) string {
	var stdout, stderr bytes.Buffer
	code, err := e.runner().Run(ctx, &stdout, &stderr, "wait",
		"--condition", "paused", "--condition", "stopped",
		"--condition", "exited", "--condition", "removing", unit)
	switch {
	case ctx.Err() != nil:
		return ""
	case err != nil:
		return fmt.Sprintf("cannot be watched (podman wait: %v)", err)
	case code != 0:
		return fmt.Sprintf("is gone (podman wait exited %d: %s)", code, strings.TrimSpace(stderr.String()))
	}
	return "stopped running: it was paused, or it exited"
}

// removeAnchor removes an anchor container. podman rm returns after
// systemd has stopped the scope of the anchor, so after a nil return no
// build step runs in the scope. A missing anchor is not an error.
func (e AnchorExecutor) removeAnchor(nameOrID string) error {
	r := e.runner()
	err := removeContainer(r, nameOrID)
	if err == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if code, existsErr := r.Run(ctx, io.Discard, io.Discard, "container", "exists", nameOrID); existsErr == nil && code == 1 {
		return nil
	}
	return fmt.Errorf("prism did not confirm that the anchor container %s of a build is removed, so a build step can still run in the podman machine: %v", nameOrID, err)
}

// StopBuilds stops every build whose anchor has a unit name that starts
// with prefix. It pauses each anchor, and it does not remove an anchor at
// once while a prism process holds the marker of the build: that process
// must stop its client first (see the comment at the top of this file).
// After the grace time, StopBuilds removes every anchor that remains.
func (e AnchorExecutor) StopBuilds(ctx context.Context, prefix string) (int, error) {
	r := e.runner()
	entries, err := listLabelled(ctx, r)
	if err != nil {
		return 0, err
	}
	var anchors []psEntry
	for _, en := range entries {
		if unit := en.Labels[LabelBuild]; unit != "" && strings.HasPrefix(unit, prefix) && en.ID != "" {
			anchors = append(anchors, en)
		}
	}
	for _, a := range anchors {
		_, _ = r.Run(ctx, io.Discard, io.Discard, "pause", a.ID)
	}
	deadline := time.Now().Add(e.stopGrace())
	for _, a := range anchors {
		for ctx.Err() == nil && time.Now().Before(deadline) && anchorBuildLive(a) && anchorExists(ctx, r, a.ID) {
			select {
			case <-ctx.Done():
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	var stopped int
	var errs []error
	for _, a := range anchors {
		if err := e.removeAnchor(a.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		stopped++
	}
	return stopped, errors.Join(errs...)
}

// anchorBuildLive reports whether a prism process holds the marker of the
// build of anchor a.
func anchorBuildLive(a psEntry) bool {
	name := anchorBuildName(a)
	if name == "" {
		return false
	}
	p, err := buildMarkerPath(name)
	return err == nil && markerHeld(p)
}

// anchorExists reports whether the container id exists. An error counts as
// a container that exists.
func anchorExists(ctx context.Context, r Runner, id string) bool {
	code, err := r.Run(ctx, io.Discard, io.Discard, "container", "exists", id)
	return err != nil || code != 1
}
