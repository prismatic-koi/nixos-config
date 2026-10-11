package prismcontainer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/prismatic-koi/prism/internal/container"
)

// LabelInstanceID is the podman label that records the session incarnation
// that owns a container. Prism decides ownership and counts containers by
// this label, never by name. No agent input sets a label.
const LabelInstanceID = "prism.instance-id"

// WorkspaceDir is the mount target of the session worktree.
const WorkspaceDir = "/workspace"

// Fixed resource limits. --memory-swap equals --memory, so the container
// gets no swap in addition to its memory.
const (
	MemoryLimit = "4g"
	CPULimit    = "2"
	PidsLimit   = "1024"
)

// conmonTimeoutGrace is added to the request timeout to give podman's own
// --timeout. Prism stops the container at the request timeout. The podman
// timeout stops it if the prism process dies first.
const conmonTimeoutGrace = time.Minute

// Caller is the session that asks for a container. Prism resolves every
// field on the host. No field comes from agent input.
type Caller struct {
	SessionName string
	InstanceID  string
	Worktree    string
}

func (c Caller) validate(mount Mount) error {
	if err := c.validateIdentity(); err != nil {
		return err
	}
	if mount == MountNone {
		return nil
	}
	if c.Worktree == "" {
		return fmt.Errorf("session %q has no worktree to mount: use --mount none", c.SessionName)
	}
	// podman splits --volume on ":" and its options on ",".
	if !strings.HasPrefix(c.Worktree, "/") || strings.ContainsAny(c.Worktree, ":,") ||
		strings.IndexFunc(c.Worktree, unicode.IsControl) >= 0 {
		return fmt.Errorf("worktree path %q cannot be given to podman as a mount source: use --mount none", c.Worktree)
	}
	// Podman follows a symlink in any component of the mount source. Run
	// checks again under the host lock, just before podman starts.
	if err := c.validateWorktree(); err != nil {
		return fmt.Errorf("%w: use --mount none", err)
	}
	return nil
}

func (c Caller) validateIdentity() error {
	if c.SessionName == "" {
		return fmt.Errorf("prism cannot determine the calling session")
	}
	if container.InstanceTokenForID(c.InstanceID) == "" {
		return fmt.Errorf("session %q has no valid instance ID, so prism cannot label its container", c.SessionName)
	}
	return nil
}

// validateWorktree refuses a worktree path that is not a directory or that
// goes through a symlink. A symlink there can redirect the mount source of
// a run and the paths of a build. The sandbox-exec profile stops an agent
// from putting one there (section 21b). This check refuses one that is
// there already.
func (c Caller) validateWorktree() error {
	info, err := os.Lstat(c.Worktree)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("worktree %q is not a directory (a symlink is refused)", c.Worktree)
	}
	if real, err := filepath.EvalSymlinks(c.Worktree); err != nil || real != filepath.Clean(c.Worktree) {
		return fmt.Errorf("worktree path %q goes through a symlink (a symlink is refused)", c.Worktree)
	}
	return nil
}

// LabelService marks a service container of `prism container start`. Its
// value is the handle that start prints. exec and logs act only on a
// container that carries it.
const LabelService = "prism.service"

// containerName returns a new container name with the session prefix.
func containerName(c Caller) (string, error) {
	name, _, err := serviceName(c)
	return name, err
}

// serviceName returns a new container name with the session prefix, and
// its random suffix, which is the handle of a service container.
func serviceName(c Caller) (name, handle string, err error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("container name: %w", err)
	}
	handle = hex.EncodeToString(b[:])
	return container.ResourceNamePrefixForOwner(c.InstanceID, c.SessionName) + handle, handle, nil
}

// fixedOptions returns the options that run and start share. No agent
// input reaches them.
func fixedOptions(c Caller, name string) []string {
	return []string{
		"--name", name,
		"--label", LabelInstanceID + "=" + c.InstanceID,
		"--memory", MemoryLimit,
		"--memory-swap", MemoryLimit,
		"--cpus", CPULimit,
		"--pids-limit", PidsLimit,
		"--security-opt", "no-new-privileges",
		"--pull", "never",
	}
}

// appendWorkload appends the worktree mount, the environment, the image,
// and the command. The image is validated and cannot start with "-", and
// podman stops option parsing at the image.
func appendWorkload(args []string, c Caller, v validRun) []string {
	switch v.mount {
	case MountRO:
		args = append(args, "--volume", c.Worktree+":"+WorkspaceDir+":ro", "--workdir", WorkspaceDir)
	case MountRW:
		args = append(args, "--volume", c.Worktree+":"+WorkspaceDir+":rw", "--workdir", WorkspaceDir)
	}
	for _, e := range v.env {
		args = append(args, "--env", e)
	}
	args = append(args, v.image)
	return append(args, v.command...)
}

// runArgs builds the podman argument vector of `prism container run`. It is
// the only builder: the host-API route and the host-mode route both reach
// it through Run.
//
// The agent controls only v. The fixed options come first and v cannot
// remove them. v cannot add an option either: the image is validated and
// cannot start with "-", podman stops option parsing at the image, and every
// value that follows a flag is one argument.
func runArgs(c Caller, v validRun, name, cidFile string) []string {
	args := append([]string{"run", "--rm"}, fixedOptions(c, name)...)
	args = append(args,
		"--timeout", strconv.FormatInt(int64((v.timeout+conmonTimeoutGrace)/time.Second), 10),
		"--cidfile", cidFile,
	)
	return appendWorkload(args, c, v)
}

// startArgs builds the podman argument vector of `prism container start`.
// It is the only builder: the host-API route and the host-mode route both
// reach it through Start. It has the fixed options of run, with these
// differences: --detach in place of --rm, and no --timeout, because a
// service container runs until stop or cleanup.
//
// Each port is published on 127.0.0.1 only, with a random host port. The
// agent gives the container port only, as one validated integer.
func startArgs(c Caller, v validStart, name, handle, cidFile string) []string {
	args := append([]string{"run", "--detach"}, fixedOptions(c, name)...)
	args = append(args, "--label", LabelService+"="+handle, "--cidfile", cidFile)
	for _, p := range v.ports {
		args = append(args, "--publish", PublishHost+"::"+strconv.Itoa(p)+"/tcp")
	}
	return appendWorkload(args, c, v.validRun)
}

// execArgs builds the podman argument vector of `prism container exec`.
// id is a container ID that prism resolved, never agent text. podman exec
// stops option parsing at the container, so the command cannot add an
// option.
func execArgs(id string, command []string) []string {
	return append([]string{"exec", id}, command...)
}

// The CPU limit of a build step. podman build has no --cpus, so the limit
// is a CFS quota of CPULimit periods per period.
const (
	buildCPUPeriod = "100000"
	buildCPUQuota  = "200000"
)

// maxImageSessionFold limits the session name part of a built image name.
// The part is decoration, like the session name in a container name.
const maxImageSessionFold = 40

// imageNamePrefix returns the name prefix of the images that the session
// builds: localhost/prism-<instance token>-<folded session name>-. An
// image name must be lower case and must not hold "@", "/", "~", or "_-",
// so the session name is folded more than in a container name.
//
// The localhost/ domain is mandatory. Without it, `prism container run`
// reads the name as a Docker Hub name, and its --pull never then fails.
func imageNamePrefix(c Caller) string {
	fold := []byte(strings.ToLower(c.SessionName))
	for i, ch := range fold {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			fold[i] = '-'
		}
	}
	if len(fold) > maxImageSessionFold {
		fold = fold[:maxImageSessionFold]
	}
	return "localhost/" + container.ResourceNamePrefixRoot + container.InstanceTokenForID(c.InstanceID) + "-" + string(fold) + "-"
}

// buildArgs builds the podman argument vector of `prism container build`.
// It is the only builder: the host-API route and the host-mode route both
// reach it through Build. The build executor adds options and changes
// nothing else. ScopeExecutor (Linux) adds --signature-policy after
// "build", and runs the vector as `podman unshare bwrap ... -- podman
// --cgroup-manager=cgroupfs build --cgroup-parent <scope>/build ...`
// (buildns.go). PlainExecutor (macOS) adds --ulimit nproc after "build".
//
// The agent controls only v. Every path is a prism-owned copy, never an
// agent path. A --build-arg value is one argument after its flag, so it
// cannot add an option. No input reaches --volume, --secret, --network,
// --device, or --cap-add.
//
// --label marks the final image and --layer-label marks each intermediate
// image, so that cleanup finds both by the label. podman build adds the
// --label value as the last instruction, so a LABEL in the Containerfile
// cannot change it.
func buildArgs(c Caller, v validBuild, image, contextDir, file string) []string {
	label := LabelInstanceID + "=" + c.InstanceID
	args := []string{
		"build",
		"--file", file,
		"--tag", image,
		"--label", label,
		"--layer-label", label,
		"--memory", MemoryLimit,
		"--memory-swap", MemoryLimit,
		"--cpu-period", buildCPUPeriod,
		"--cpu-quota", buildCPUQuota,
		"--security-opt", "no-new-privileges",
	}
	for _, a := range v.buildArgs {
		args = append(args, "--build-arg", a)
	}
	return append(args, contextDir)
}
