package prismcontainer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
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
	if c.SessionName == "" {
		return fmt.Errorf("prism cannot determine the calling session")
	}
	if container.InstanceTokenForID(c.InstanceID) == "" {
		return fmt.Errorf("session %q has no valid instance ID, so prism cannot label its container", c.SessionName)
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
	info, err := os.Stat(c.Worktree)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("worktree %q is not a directory: use --mount none", c.Worktree)
	}
	return nil
}

// containerName returns a new container name with the session prefix.
func containerName(c Caller) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("container name: %w", err)
	}
	return container.ResourceNamePrefixForOwner(c.InstanceID, c.SessionName) + hex.EncodeToString(b[:]), nil
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
	args := []string{
		"run",
		"--rm",
		"--name", name,
		"--label", LabelInstanceID + "=" + c.InstanceID,
		"--memory", MemoryLimit,
		"--memory-swap", MemoryLimit,
		"--cpus", CPULimit,
		"--pids-limit", PidsLimit,
		"--security-opt", "no-new-privileges",
		"--pull", "never",
		"--timeout", strconv.FormatInt(int64((v.timeout+conmonTimeoutGrace)/time.Second), 10),
		"--cidfile", cidFile,
	}
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
