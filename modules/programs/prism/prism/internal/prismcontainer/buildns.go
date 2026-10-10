package prismcontainer

// The file view of a Linux build.
//
// The security boundary of `prism container build` on Linux is that the
// build cannot reach host files. podman build runs in a mount namespace
// that holds only an allowlist of host paths: the podman binaries and
// config, the podman store, the build copy, and the cgroup tree of the
// build scope. A transport source such as tarball:/home/..., oci:/...,
// dir:/..., or containers-storage:[driver@/other/root+...] then fails,
// because the path does not exist in the namespace. The Containerfile
// check and the signature policy refuse most of these sources before, with
// a clear message, but they are not the boundary.
//
// Rootless podman joins the user and mount namespace of its pause process
// at start, which undoes a mount restriction made outside podman. It does
// not join when it already runs as root in a user namespace with
// CAP_SYS_ADMIN, or when _CONTAINERS_USERNS_CONFIGURED is set. Thus the
// build runs as
//
//	podman unshare  bwrap <allowlist>  --  podman build ...
//
// `podman unshare` enters the user namespace of podman (as root, with
// _CONTAINERS_USERNS_CONFIGURED=done). bwrap runs as root there, so it
// makes a new mount namespace and a new pid namespace without a new user
// namespace, and the inner podman stays in them. The new pid namespace
// with its own /proc is mandatory: with the host /proc, /proc/<pid>/root
// of a process of the same user gives the host files back.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cgroupToken stands for the cgroup path of the build in the argument
// vector. The scope script replaces it, because only the script knows the
// path of its scope.
const cgroupToken = "@PRISM_BUILD_CGROUP@"

// systemPaths are bound read-only when they exist. They hold the binaries
// and the libraries that podman, crun, and their helpers use.
var systemPaths = []string{"/nix", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/run/current-system", "/run/wrappers"}

// etcPaths are the entries of /etc that podman reads. They are bound
// read-only when they exist. The rest of /etc is not visible.
var etcPaths = []string{
	"/etc/containers", "/etc/ssl", "/etc/pki", "/etc/static",
	"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf",
	"/etc/passwd", "/etc/group", "/etc/localtime",
}

// runtimeSubdirs are the directories under XDG_RUNTIME_DIR that rootless
// podman, crun, and the rootless network use. They are bound read-write
// when they exist.
var runtimeSubdirs = []string{"containers", "libpod", "netns", "crun", "podman"}

// buildView holds the host paths that a Linux build can see.
type buildView struct {
	// Graphroot and Runroot are the podman store, read-write.
	Graphroot string
	Runroot   string
	// RuntimeDir is XDG_RUNTIME_DIR.
	RuntimeDir string
	// ConfigDir is the containers config dir of the user, read-only.
	ConfigDir string
	// StageDir is the build copy, read-only.
	StageDir string
	// VarTmp is a per-build dir, mounted read-write at /var/tmp. buildah
	// writes temporary layers and RUN --mount=type=cache dirs there.
	VarTmp string
	// Policy is the signature policy file, read-only.
	Policy string
}

// bwrapArgs returns the bwrap options that make the file view of v. The
// order matters: a parent bind comes before a bind inside it.
func bwrapArgs(v buildView) []string {
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--proc", "/proc",
		"--dev", "/dev",
		"--dev-bind-try", "/dev/net/tun", "/dev/net/tun",
		"--dev-bind-try", "/dev/fuse", "/dev/fuse",
		"--tmpfs", "/tmp",
		"--ro-bind", "/sys", "/sys",
		"--bind", "/sys/fs/cgroup", "/sys/fs/cgroup",
	}
	for _, p := range systemPaths {
		args = append(args, "--ro-bind-try", p, p)
	}
	for _, p := range etcPaths {
		args = append(args, "--ro-bind-try", p, p)
	}
	if v.ConfigDir != "" {
		args = append(args, "--ro-bind-try", v.ConfigDir, v.ConfigDir)
	}
	if v.RuntimeDir != "" {
		for _, d := range runtimeSubdirs {
			p := filepath.Join(v.RuntimeDir, d)
			args = append(args, "--bind-try", p, p)
		}
	}
	args = append(args,
		"--bind", v.Graphroot, v.Graphroot,
		"--bind", v.Runroot, v.Runroot,
		"--ro-bind", v.StageDir, v.StageDir,
		"--bind", v.VarTmp, "/var/tmp",
		"--ro-bind", v.Policy, v.Policy,
		"--setenv", "TMPDIR", "/tmp",
	)
	return args
}

// namespacedBuildCommand returns the command that runs podman build in the
// file view of v. args is the build argument vector, args[0] == "build".
// The cgroup parent of the build is cgroupToken.
func namespacedBuildCommand(podman, bwrap string, v buildView, args []string) []string {
	cmd := []string{podman, "unshare", bwrap}
	cmd = append(cmd, bwrapArgs(v)...)
	cmd = append(cmd, "--", podman, "--cgroup-manager=cgroupfs", "build", "--cgroup-parent", cgroupToken)
	return append(cmd, args[1:]...)
}

// podmanStore is the part of `podman info --format json` that prism reads.
type podmanStore struct {
	Store struct {
		GraphRoot string `json:"graphRoot"`
		RunRoot   string `json:"runRoot"`
	} `json:"store"`
}

// readPodmanStore returns the graph root and the run root of the podman of
// the host user.
func readPodmanStore(ctx context.Context, r Runner) (string, string, error) {
	var stdout, stderr bytes.Buffer
	code, err := r.Run(ctx, &stdout, &stderr, "info", "--format", "json")
	if err != nil {
		return "", "", &podmanError{detail: err.Error()}
	}
	if code != 0 {
		return "", "", &podmanError{detail: fmt.Sprintf("podman info exited %d: %s", code, strings.TrimSpace(stderr.String()))}
	}
	var info podmanStore
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		return "", "", fmt.Errorf("parse podman info output: %v", err)
	}
	g, rr := info.Store.GraphRoot, info.Store.RunRoot
	if !filepath.IsAbs(g) || !filepath.IsAbs(rr) {
		return "", "", fmt.Errorf("podman info gives no absolute store paths (graphRoot %q, runRoot %q)", g, rr)
	}
	return g, rr, nil
}

// userContainersConfigDir returns the containers config dir of the user.
func userContainersConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "containers")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "containers")
	}
	return ""
}

// runtimeDir returns XDG_RUNTIME_DIR, or the standard path for the user.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return fmt.Sprintf("/run/user/%d", os.Getuid())
}
