package prismcontainer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// Runner runs the podman CLI. Production uses ExecRunner. Tests inject a
// fake.
type Runner interface {
	// Run runs podman with args and returns its exit code. A non-nil error
	// means podman did not run to completion: it did not start, or ctx
	// ended first.
	Run(ctx context.Context, stdout, stderr io.Writer, args ...string) (int, error)
}

// ExecRunner runs the podman binary from PATH.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, stdout, stderr io.Writer, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, "podman", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// A podman descendant can keep the output pipes open after podman exits.
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// psEntry is the part of `podman ps --format json` that prism reads.
type psEntry struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// active reports whether the container counts against a limit. A container
// that has exited but is not yet removed does not run.
func (e psEntry) active() bool {
	switch strings.ToLower(e.State) {
	case "exited", "stopped":
		return false
	}
	return true
}

// listLabelled returns every container on the host that carries the prism
// instance label, with any value.
func listLabelled(ctx context.Context, r Runner) ([]psEntry, error) {
	var stdout, stderr bytes.Buffer
	code, err := r.Run(ctx, &stdout, &stderr, "ps", "--all",
		"--filter", "label="+LabelInstanceID, "--format", "json")
	if err != nil {
		return nil, &podmanError{detail: err.Error()}
	}
	if code != 0 {
		return nil, &podmanError{detail: fmt.Sprintf("podman ps exited %d: %s", code, strings.TrimSpace(stderr.String()))}
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return nil, nil
	}
	var entries []psEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, &podmanError{detail: fmt.Sprintf("parse podman ps output: %v", err)}
	}
	return entries, nil
}

// podmanError reports that prism did not get a usable answer from podman.
type podmanError struct{ detail string }

func (e *podmanError) Error() string { return e.detail }

// unreachableMessage returns the error text for a podman that prism cannot
// reach, with the recovery step for the host OS.
func unreachableMessage(goos, detail string) string {
	hint := "run `systemctl --user status podman.socket` on this Linux host"
	if goos == "darwin" {
		hint = "run `podman machine start` on this macOS host"
	}
	return fmt.Sprintf("podman is not reachable (%s). To fix this, %s, then try again", detail, hint)
}

// removeContainer force-removes a container by name. A missing container
// is not an error.
func removeContainer(r Runner, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	code, err := r.Run(ctx, io.Discard, &stderr, "rm", "--force", "--ignore", "--time", "0", name)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("podman rm exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func ownedSet(instanceIDs []string) map[string]bool {
	owned := make(map[string]bool, len(instanceIDs))
	for _, id := range instanceIDs {
		if id != "" {
			owned[id] = true
		}
	}
	return owned
}

// SweepSession stops every running build, then removes every container,
// then removes every built image, of the session incarnations instanceIDs.
// The builds stop first, so that no build adds an image after the image
// sweep. Each step runs even when an earlier one fails.
func SweepSession(ctx context.Context, r Runner, e BuildExecutor, instanceIDs []string) error {
	_, buildErr := StopBuilds(ctx, e, instanceIDs)
	_, ctrErr := SweepInstances(ctx, r, instanceIDs)
	_, imgErr := SweepImages(ctx, r, instanceIDs)
	return errors.Join(buildErr, ctrErr, imgErr)
}

// StopBuilds stops every running build of the session incarnations
// instanceIDs. It returns the number of builds stopped.
func StopBuilds(ctx context.Context, e BuildExecutor, instanceIDs []string) (int, error) {
	var stopped int
	var errs []error
	for id := range ownedSet(instanceIDs) {
		if container.InstanceTokenForID(id) == "" {
			continue
		}
		n, err := e.StopBuilds(ctx, buildUnitPrefix(id))
		stopped += n
		if err != nil {
			errs = append(errs, fmt.Errorf("stop builds: %w", err))
		}
	}
	return stopped, errors.Join(errs...)
}

// imageEntry is the part of `podman images --format json` that prism reads.
type imageEntry struct {
	ID     string            `json:"Id"`
	Labels map[string]string `json:"Labels"`
}

// SweepImages force-removes every image, intermediate images included,
// whose instance label names one of instanceIDs. --force also removes a
// container that uses the image. It returns the number of images removed.
func SweepImages(ctx context.Context, r Runner, instanceIDs []string) (int, error) {
	owned := ownedSet(instanceIDs)
	if len(owned) == 0 {
		return 0, nil
	}
	var stdout, stderr bytes.Buffer
	code, err := r.Run(ctx, &stdout, &stderr, "images", "--all",
		"--filter", "label="+LabelInstanceID, "--format", "json")
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("podman images exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	var images []imageEntry
	if out := bytes.TrimSpace(stdout.Bytes()); len(out) > 0 {
		if err := json.Unmarshal(out, &images); err != nil {
			return 0, fmt.Errorf("parse podman images output: %v", err)
		}
	}
	var ids []string
	for _, img := range images {
		if owned[img.Labels[LabelInstanceID]] && img.ID != "" {
			ids = append(ids, img.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	stderr.Reset()
	code, err = r.Run(ctx, io.Discard, &stderr, append([]string{"rmi", "--force", "--ignore"}, ids...)...)
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("podman rmi exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return len(ids), nil
}

// SweepInstances force-removes every container whose instance label names
// one of instanceIDs. It returns the number of containers removed.
func SweepInstances(ctx context.Context, r Runner, instanceIDs []string) (int, error) {
	if len(instanceIDs) == 0 {
		return 0, nil
	}
	owned := ownedSet(instanceIDs)
	entries, err := listLabelled(ctx, r)
	if err != nil {
		return 0, err
	}
	var ids []string
	for _, e := range entries {
		if owned[e.Labels[LabelInstanceID]] && e.ID != "" {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	args := append([]string{"rm", "--force", "--ignore", "--time", "0"}, ids...)
	var stderr bytes.Buffer
	code, err := r.Run(ctx, io.Discard, &stderr, args...)
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("podman rm exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return len(ids), nil
}
