package prismcontainer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// BuildResult is the outcome of one `prism container build` request. It is
// also the wire schema of the POST /container/build response.
type BuildResult struct {
	ExitCode int `json:"exit_code"`
	// Image is the name of the built image. It is set only when the build
	// succeeded.
	Image         string `json:"image,omitempty"`
	Output        []byte `json:"output,omitempty"`
	OutputDropped int64  `json:"output_dropped,omitempty"`
	// Message is the text from prism itself: a refusal, a timeout, a
	// failure, or a note about the build context.
	Message string `json:"message,omitempty"`
}

// Build builds an image from a Containerfile in the worktree of caller c,
// and returns the name of the image. The image name is the session prefix
// followed by the tag. When ctx ends or the timeout expires, Build stops
// the build.
func Build(ctx context.Context, d Deps, c Caller, req BuildRequest) BuildResult {
	d = d.withDefaults()
	output := newTailBuffer(OutputLimit)
	result := func(code int, image, msg string) BuildResult {
		r := BuildResult{ExitCode: code, Image: image, Message: msg}
		r.Output, r.OutputDropped = output.snapshot()
		return r
	}

	if container.InstanceTokenForID(c.InstanceID) == "" {
		return result(ExitRefused, "", "refused: "+c.validateIdentity().Error())
	}
	audit, err := openAuditLog(c.InstanceID)
	if err != nil {
		return result(ExitRefused, "", "refused: prism cannot write the audit log: "+err.Error())
	}
	defer audit.close()

	entry := AuditEntry{
		Session:        c.SessionName,
		InstanceID:     c.InstanceID,
		Command:        "build",
		Context:        req.Context,
		File:           req.File,
		TimeoutSeconds: req.TimeoutSeconds,
	}
	var notes string
	finish := func(decision string, code int, ran bool, image, msg string) BuildResult {
		msg = joinMessages(notes, msg)
		entry.Decision = decision
		entry.Reason = msg
		if ran {
			entry.ExitCode = &code
		}
		if err := audit.write(entry, d.Now()); err != nil {
			msg = joinMessages(msg, "warning: prism did not write the audit log: "+err.Error())
		}
		return result(code, image, msg)
	}
	refuse := func(reason string) BuildResult {
		return finish(DecisionRefused, ExitRefused, false, "", "refused: "+reason)
	}

	v, err := req.validate()
	if err != nil {
		return refuse(err.Error())
	}
	entry.Context = v.context
	entry.File = v.file
	entry.BuildArgKeys = v.buildArgKeys
	entry.TimeoutSeconds = int64(v.timeout / time.Second)
	if err := c.validateIdentity(); err != nil {
		return refuse(err.Error())
	}
	if c.Worktree == "" {
		return refuse(fmt.Sprintf("session %q has no worktree to build from", c.SessionName))
	}
	if err := c.validateWorktree(); err != nil {
		return refuse(err.Error())
	}
	paths, err := resolveBuildPaths(c.Worktree, v)
	if err != nil {
		return refuse(err.Error())
	}
	entry.File = paths.file

	suffix, err := newBuildSuffix()
	if err != nil {
		return finish(DecisionError, ExitRefused, false, "", err.Error())
	}
	tag := v.tag
	if tag == "" {
		tag = suffix
	}
	image := imageNamePrefix(c) + tag
	entry.Image = image
	unit := buildUnitPrefix(c.InstanceID) + suffix

	runCtx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	stopped := func(warning string) BuildResult {
		if ctx.Err() != nil {
			return finish(DecisionAllowed, ExitRefused, false, "",
				joinMessages("the request was cancelled, so prism stopped the build", warning))
		}
		return finish(DecisionAllowed, ExitTimeout, false, "",
			joinMessages(fmt.Sprintf("the --timeout of %s expired, so prism stopped the build", v.timeout), warning))
	}
	limitCheckFailed := func(err error) BuildResult {
		if runCtx.Err() != nil {
			return stopped("")
		}
		msg := err.Error()
		if pe := (*podmanError)(nil); errors.As(err, &pe) {
			msg = unreachableMessage(d.GOOS, msg)
		}
		return finish(DecisionError, ExitRefused, false, "", msg)
	}

	// The executor checks the host. On macOS it refuses a podman machine
	// that mounts Mac paths outside the allowlist.
	if err := d.BuildExecutor.Preflight(runCtx); err != nil {
		if runCtx.Err() != nil {
			return stopped("")
		}
		return refuse(err.Error())
	}

	lockPath, err := container.PrismContainerLockPath()
	if err != nil {
		return finish(DecisionError, ExitRefused, false, "", err.Error())
	}
	lock, err := acquireHostLock(runCtx, lockPath, d.LockWait)
	if err != nil {
		if runCtx.Err() != nil {
			return stopped("")
		}
		return finish(DecisionError, ExitRefused, false, "", err.Error())
	}
	if msg, err := limitRefusal(runCtx, d.Runner, c.InstanceID, d.HostLimit); err != nil {
		lock.release()
		return limitCheckFailed(err)
	} else if msg != "" {
		lock.release()
		return refuse(msg)
	}
	marker, err := createBuildMarker(c.InstanceID, suffix)
	lock.release()
	if err != nil {
		return finish(DecisionError, ExitRefused, false, "", err.Error())
	}
	defer marker.release()

	// The copy gets the name of the marker and exists only while the
	// marker is held, so a scan can remove the copy of a dead build.
	stage, err := stageBuild(runCtx, c.Worktree, paths, buildName(c.InstanceID, suffix))
	if err != nil {
		if runCtx.Err() != nil {
			return stopped("")
		}
		return refuse(err.Error())
	}
	defer stage.remove()
	for _, n := range stage.notes {
		notes = joinMessages(notes, n)
	}
	// Check the copy, which is what podman reads.
	containerfile, err := os.ReadFile(stage.file)
	if err != nil {
		return finish(DecisionError, ExitRefused, false, "", "read the Containerfile copy: "+err.Error())
	}
	if err := checkContainerfile(containerfile); err != nil {
		return refuse(err.Error())
	}

	args := buildArgs(c, v, image, stage.context, stage.file)
	code, err := d.BuildExecutor.Build(runCtx, unit, output, args)
	warning := ""
	if sf := (*stopFailedError)(nil); errors.As(err, &sf) {
		warning = "warning: " + sf.err.Error()
		err = sf.cause
	}
	if err != nil {
		if runCtx.Err() != nil {
			return stopped(warning)
		}
		return finish(DecisionError, ExitRefused, false, "", joinMessages("podman build did not complete: "+err.Error(), warning))
	}
	if code != 0 {
		msg := fmt.Sprintf("the build failed: podman build exited %d", code)
		if out, _ := output.snapshot(); bytes.Contains(out, []byte("rejected by policy")) {
			msg += ". The signature policy of the build refused an image source: prism builds only from registry images, local images, and build stages. An ONBUILD instruction of a base image can name such a source"
		}
		return finish(DecisionAllowed, code, true, "", joinMessages(msg, warning))
	}
	return finish(DecisionAllowed, 0, true, image, warning)
}

// WriteBuildResult writes a BuildResult to the streams of the `prism
// container build` process. Both routes use it. stdout holds the image
// name only, so that a caller can read it with $(...).
func WriteBuildResult(stdout, stderr io.Writer, r BuildResult) {
	if r.OutputDropped > 0 {
		fmt.Fprintf(stderr, "prism container build: the build output was truncated: it was longer than 1 MiB, so prism kept the last 1 MiB and dropped the first %d bytes\n", r.OutputDropped)
	}
	_, _ = stderr.Write(r.Output)
	if r.Message != "" {
		fmt.Fprintf(stderr, "prism container build: %s\n", r.Message)
	}
	if r.ExitCode == 0 && r.Image != "" {
		fmt.Fprintln(stdout, r.Image)
	}
}
