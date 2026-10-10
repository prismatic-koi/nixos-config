// Package prismcontainer runs containers and image builds for prism
// sessions.
//
// The agent never talks to podman. It gives prism a small set of inputs
// (RunRequest, BuildRequest), and prism builds the podman argument vector
// on the host with a fixed set of options (runArgs, buildArgs). A sandboxed
// session reaches this package through its sidecar (POST /container/run,
// POST /container/build). A host-mode session calls it directly. Both
// routes call Run or Build, so they cannot differ.
package prismcontainer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// Exit codes for results that prism makes, not the container command.
const (
	ExitTimeout = 124
	ExitRefused = 125
)

// RunResult is the outcome of one `prism container run` request. It is also
// the wire schema of the POST /container/run response.
type RunResult struct {
	ExitCode      int    `json:"exit_code"`
	Stdout        []byte `json:"stdout,omitempty"`
	Stderr        []byte `json:"stderr,omitempty"`
	StdoutDropped int64  `json:"stdout_dropped,omitempty"`
	StderrDropped int64  `json:"stderr_dropped,omitempty"`
	// Message is the text from prism itself: a refusal, a timeout, or an
	// error. Empty when the container command ran to completion.
	Message string `json:"message,omitempty"`
}

// Deps holds the host-side dependencies of Run. Zero values select the
// production defaults.
type Deps struct {
	Runner Runner
	// BuildExecutor runs podman build. Nil selects DefaultBuildExecutor
	// for GOOS.
	BuildExecutor BuildExecutor
	// MachineMountAllowlist holds the Mac paths that the podman machine
	// can mount, for the default macOS build executor.
	MachineMountAllowlist []string
	HostLimit             int
	GOOS                  string
	Now                   func() time.Time
	LockWait              time.Duration
	CreateWait            time.Duration
}

func (d Deps) withDefaults() Deps {
	if d.Runner == nil {
		d.Runner = ExecRunner{}
	}
	if d.HostLimit < 1 {
		d.HostLimit = DefaultHostLimit
	}
	if d.GOOS == "" {
		d.GOOS = runtime.GOOS
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.LockWait <= 0 {
		d.LockWait = time.Minute
	}
	if d.CreateWait <= 0 {
		d.CreateWait = 30 * time.Second
	}
	if d.BuildExecutor == nil {
		d.BuildExecutor = DefaultBuildExecutor(d.GOOS, d.Runner, d.MachineMountAllowlist)
	}
	return d
}

type runOutcome struct {
	code int
	err  error
}

// Run runs one container for caller c and returns its output and exit code.
// It removes the container before it returns. When ctx ends, Run stops and
// removes the container.
func Run(ctx context.Context, d Deps, c Caller, req RunRequest) RunResult {
	d = d.withDefaults()
	stdout := newTailBuffer(OutputLimit)
	stderr := newTailBuffer(OutputLimit)
	result := func(code int, msg string) RunResult {
		r := RunResult{ExitCode: code, Message: msg}
		r.Stdout, r.StdoutDropped = stdout.snapshot()
		r.Stderr, r.StderrDropped = stderr.snapshot()
		return r
	}

	// Without a valid instance ID there is no audit log and no label, so
	// fail closed before anything else.
	if container.InstanceTokenForID(c.InstanceID) == "" {
		return result(ExitRefused, "refused: "+c.validate(MountNone).Error())
	}
	audit, err := openAuditLog(c.InstanceID)
	if err != nil {
		return result(ExitRefused, "refused: prism cannot write the audit log: "+err.Error())
	}
	defer audit.close()

	entry := AuditEntry{
		Session:        c.SessionName,
		InstanceID:     c.InstanceID,
		Command:        "run",
		Image:          req.Image,
		Args:           req.Command,
		Mount:          string(req.Mount),
		TimeoutSeconds: req.TimeoutSeconds,
	}
	finish := func(decision string, code int, ran bool, msg string) RunResult {
		entry.Decision = decision
		entry.Reason = msg
		if ran {
			entry.ExitCode = &code
		}
		if err := audit.write(entry, d.Now()); err != nil {
			msg = joinMessages(msg, "warning: prism did not write the audit log: "+err.Error())
		}
		return result(code, msg)
	}
	refuse := func(reason string) RunResult {
		return finish(DecisionRefused, ExitRefused, false, "refused: "+reason)
	}

	v, err := req.validate()
	if err != nil {
		return refuse(err.Error())
	}
	entry.Image = v.image
	entry.EnvKeys = v.envKeys
	entry.Mount = string(v.mount)
	entry.TimeoutSeconds = int64(v.timeout / time.Second)
	if err := c.validate(v.mount); err != nil {
		return refuse(err.Error())
	}

	runCtx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	stopped := func() RunResult {
		if ctx.Err() != nil {
			return finish(DecisionAllowed, ExitRefused, false, "the request was cancelled, so prism stopped the run and removed its container")
		}
		return finish(DecisionAllowed, ExitTimeout, false,
			fmt.Sprintf("the --timeout of %s expired, so prism stopped the run and removed its container", v.timeout))
	}
	podmanFailed := func(err error) RunResult {
		if runCtx.Err() != nil {
			return stopped()
		}
		return finish(DecisionError, ExitRefused, false, unreachableMessage(d.GOOS, err.Error()))
	}
	limitCheckFailed := func(err error) RunResult {
		var pe *podmanError
		if errors.As(err, &pe) {
			return podmanFailed(err)
		}
		return finish(DecisionError, ExitRefused, false, err.Error())
	}

	// Check the limits before the pull, so that a refusal comes at once.
	if msg, err := limitRefusal(runCtx, d.Runner, c.InstanceID, d.HostLimit); err != nil {
		return limitCheckFailed(err)
	} else if msg != "" {
		return refuse(msg)
	}

	// Pull outside the host lock: a pull can take minutes.
	code, err := ensureImage(runCtx, d.Runner, v.image, stderr)
	if err != nil {
		return podmanFailed(err)
	}
	if code != 0 {
		return finish(DecisionAllowed, code, true, fmt.Sprintf("the podman pull of image %s failed", v.image))
	}

	lockPath, err := container.PrismContainerLockPath()
	if err != nil {
		return finish(DecisionError, ExitRefused, false, err.Error())
	}
	lock, err := acquireHostLock(runCtx, lockPath, d.LockWait)
	if err != nil {
		if runCtx.Err() != nil {
			return stopped()
		}
		return finish(DecisionError, ExitRefused, false, err.Error())
	}
	defer lock.release()
	if msg, err := limitRefusal(runCtx, d.Runner, c.InstanceID, d.HostLimit); err != nil {
		return limitCheckFailed(err)
	} else if msg != "" {
		return refuse(msg)
	}
	// The pull and the lock wait can take minutes. Check the worktree path
	// again as late as possible.
	if err := c.validate(v.mount); err != nil {
		return refuse(err.Error())
	}

	name, err := containerName(c)
	if err != nil {
		return finish(DecisionError, ExitRefused, false, err.Error())
	}
	cidDir, err := makeCIDDir()
	if err != nil {
		return finish(DecisionError, ExitRefused, false, "create cidfile dir: "+err.Error())
	}
	defer os.RemoveAll(cidDir)
	cidFile := filepath.Join(cidDir, "cid")
	entry.Container = name

	done := make(chan runOutcome, 1)
	args := runArgs(c, v, name, cidFile)
	go func() {
		code, err := d.Runner.Run(runCtx, stdout, stderr, args...)
		done <- runOutcome{code: code, err: err}
	}()
	// Hold the lock until podman has created the container, so that the
	// next limit check counts it.
	outcome, finished := waitForCreate(runCtx, cidFile, done, d.CreateWait)
	lock.release()
	if !finished {
		outcome = <-done
	}

	removeWarning := ensureRemoved(d.Runner, name)
	if outcome.err != nil {
		if runCtx.Err() != nil {
			return stopped()
		}
		return finish(DecisionError, ExitRefused, false,
			joinMessages("podman run did not complete: "+outcome.err.Error(), removeWarning))
	}
	return finish(DecisionAllowed, outcome.code, true, removeWarning)
}

// makeCIDDir creates a per-run directory for the podman cidfile in the
// prism-container state tree. It must not be os.TempDir(): the
// sandbox-exec profile grants the agent write access there.
func makeCIDDir() (string, error) {
	parent, err := container.PrismContainerCIDDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, "run-")
}

// ensureImage pulls image when the host does not have it. It returns the
// exit code of the pull, or 0 when no pull was needed. The pull error text
// goes to stderr.
func ensureImage(ctx context.Context, r Runner, image string, stderr io.Writer) (int, error) {
	code, err := r.Run(ctx, io.Discard, io.Discard, "image", "exists", image)
	if err != nil {
		return -1, err
	}
	if code == 0 {
		return 0, nil
	}
	return r.Run(ctx, io.Discard, stderr, "pull", "--quiet", image)
}

// waitForCreate returns when podman has written the container ID to
// cidFile, when the run ends, when ctx ends, or when maxWait passes. The
// boolean is true when the run ended, and the outcome is then valid.
func waitForCreate(ctx context.Context, cidFile string, done <-chan runOutcome, maxWait time.Duration) (runOutcome, bool) {
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case o := <-done:
			return o, true
		case <-ctx.Done():
			return runOutcome{}, false
		case <-deadline.C:
			return runOutcome{}, false
		case <-tick.C:
			if info, err := os.Stat(cidFile); err == nil && info.Size() > 0 {
				return runOutcome{}, false
			}
		}
	}
}

// ensureRemoved removes the container after the run. `podman run --rm`
// has usually removed it already. The explicit remove covers a timeout, a
// cancelled request, and a podman failure. It returns a warning, or "" when
// the container is gone.
func ensureRemoved(r Runner, name string) string {
	err := removeContainer(r, name)
	if err == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var discard bytes.Buffer
	if code, existsErr := r.Run(ctx, &discard, &discard, "container", "exists", name); existsErr == nil && code == 1 {
		return ""
	}
	return fmt.Sprintf("warning: prism did not confirm that container %s is removed: %v", name, err)
}

func joinMessages(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n" + b
}
