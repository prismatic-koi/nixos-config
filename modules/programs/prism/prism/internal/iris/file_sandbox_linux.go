// file_sandbox_linux.go — Linux implementation of the per-tool file sandbox.
//
// On Linux we use bubblewrap (bwrap) to create an isolated subprocess with
// the per-tool mount set described in the daemon-mode design doc §5.
//
// The mount set is assembled using the MountSpec machinery from
// internal/container/mounts.go (specifically container.AppendBwrapBind).
// StandardSandboxMounts is NOT called — D-4 builds a fresh minimal list
// that deliberately excludes pi-process credentials.

package iris

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/container"
)

// runInFileSandbox runs name+argv inside a bwrap file-tool sandbox.
// worktreeReadOnly controls whether the worktree is mounted RO or RW.
// Returns (combined stdout+stderr, exitOK, error).
func (d *toolDispatcher) runInFileSandbox(ctx context.Context, worktreeReadOnly bool, name string, argv ...string) (string, bool, error) {
	mountArgs := fileToolMounts(d.worktree, d.tmpDir, worktreeReadOnly)

	// Look up the tool binary path and resolve through symlinks — bwrap needs
	// the real /nix/store/... path for some tools on NixOS.
	resolvedName, err := resolveBinaryPath(name)
	if err != nil {
		return "", false, fmt.Errorf("resolve binary %q: %w", name, err)
	}

	return runInBwrap(ctx, mountArgs, d.worktree, nil, resolvedName, argv...)
}

// runInFileSandboxWithStdin is like runInFileSandbox but pipes stdin into the
// subprocess.  Used by the edit and write executors which pass file content
// via stdin rather than as a command-line argument.
func (d *toolDispatcher) runInFileSandboxWithStdin(ctx context.Context, worktreeReadOnly bool, stdin string, name string, argv ...string) (string, bool, error) {
	mountArgs := fileToolMounts(d.worktree, d.tmpDir, worktreeReadOnly)

	resolvedName, err := resolveBinaryPath(name)
	if err != nil {
		return "", false, fmt.Errorf("resolve binary %q: %w", name, err)
	}

	return runInBwrap(ctx, mountArgs, d.worktree, strings.NewReader(stdin), resolvedName, argv...)
}

// runInBwrap builds and executes the bwrap command.
// stdinReader, if non-nil, is piped to the subprocess's stdin.
func runInBwrap(ctx context.Context, mountArgs []string, cwd string, stdinReader interface{ Read([]byte) (int, error) }, name string, argv ...string) (string, bool, error) {
	args := []string{
		"--clearenv",
		"--unshare-pid",
		"--proc", "/proc",
		"--dev", "/dev",
		"--die-with-parent",
	}
	args = append(args, mountArgs...)
	args = append(args, fileToolEnvArgs()...)
	if cwd != "" {
		args = append(args, "--chdir", cwd)
	}
	args = append(args, "--")
	args = append(args, name)
	args = append(args, argv...)

	cmd := exec.CommandContext(ctx, "bwrap", args...)
	if stdinReader != nil {
		cmd.Stdin = stdinReader
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return string(out), false, nil
		}
		return string(out), false, fmt.Errorf("bwrap: %w", err)
	}
	return string(out), true, nil
}

// fileToolMounts returns the bwrap mount argument list for a file tool
// subprocess.  It uses container.AppendBwrapBind (the shared mounts.go
// emitter) to translate each MountSpec into the correct --ro-bind / --bind
// triple, satisfying the AC requiring MountSpec plumbing reuse.
//
// The mount set is built from scratch (not from StandardSandboxMounts) so
// that pi-process credentials are deliberately excluded.
//
// Mount ordering matters in bwrap — later mounts shadow earlier ones on
// overlapping paths.  The ordering used here is:
//
//  1. System RO roots (/nix, /etc, /bin, /run/current-system, ~/.nix-profile)
//  2. Per-session /tmp (backed by tmpDir) — replaces the sandbox's /tmp
//  3. Sensitive /etc subtrees shadowed by empty tmpfs
//  4. Worktree (LAST) — placed after the /tmp replacement so that when the
//     worktree is under /tmp (e.g. t.TempDir() in tests), this bind-mount
//     restores the worktree path inside the sandbox namespace.
//
// In production, worktrees are never under /tmp, so the ordering is
// invisible.  In tests, we also pre-create the parent directory inside
// tmpDir so the mount-point destination exists.
func fileToolMounts(worktree, tmpDir string, worktreeReadOnly bool) []string {
	var args []string
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// System RO roots via MountSpec -> AppendBwrapBind.
	// Emitted first so they can be shadowed by later mounts.
	systemSpecs := []container.MountSpec{
		{HostPath: "/nix", SandboxPath: "/nix", ReadOnly: true},
		{HostPath: "/etc", SandboxPath: "/etc", ReadOnly: true},
		{HostPath: "/bin", SandboxPath: "/bin", ReadOnly: true},
		{
			HostPath:          "/run/current-system",
			SandboxPath:       "/run/current-system",
			ReadOnly:          true,
			OptionalIfMissing: true,
		},
		{
			HostPath:          home + "/.nix-profile",
			SandboxPath:       home + "/.nix-profile",
			ReadOnly:          true,
			OptionalIfMissing: true,
		},
	}
	for _, spec := range systemSpecs {
		args = container.AppendBwrapBind(args, spec)
	}

	// Per-session /tmp (RW) backed by tmpDir on the host.
	// --bind maps the host tmpDir to the sandbox /tmp so that writes to /tmp
	// inside the sandbox persist to tmpDir on the host and survive across
	// tool calls in the same session.
	if tmpDir != "" {
		if mkErr := os.MkdirAll(tmpDir, 0o755); mkErr == nil {
			args = append(args, "--bind", tmpDir, "/tmp")
		}
	} else {
		args = append(args, "--tmpfs", "/tmp")
	}

	// When the worktree is under the host's /tmp (which happens in tests using
	// t.TempDir()), the worktree bind below will try to mount at a destination
	// path inside the sandbox's /tmp (= tmpDir).  bwrap requires the
	// mount-point destination directory to exist inside the sandbox namespace.
	// Since tmpDir is a fresh empty directory, we pre-create the parent
	// directories for the worktree inside tmpDir so the mount point is valid.
	if tmpDir != "" && worktree != "" && strings.HasPrefix(worktree, "/tmp/") {
		relToTmp := worktree[len("/tmp/"):]
		mountPoint := filepath.Join(tmpDir, relToTmp)
		// Create the directory that will serve as the bind-mount destination.
		// If this fails (permissions, disk space), bwrap will produce a
		// descriptive error at mount time.
		_ = os.MkdirAll(mountPoint, 0o755)
	}

	// Shadow sensitive /etc subtrees with empty tmpfs.
	// Applied after the /etc ro-bind so bwrap's ordered mount processing
	// shadows these subtrees inside the sandbox.
	for _, sensitiveDir := range []string{
		"/etc/wireguard",
		"/etc/wpa_supplicant",
		"/etc/ssh",
	} {
		if _, err := os.Stat(sensitiveDir); err == nil {
			args = append(args, "--tmpfs", sensitiveDir)
		}
	}

	// Worktree (RO or RW) — mounted LAST.
	// Placing the worktree bind last ensures it shadows the /tmp replacement
	// when the worktree is under /tmp (which happens in tests using
	// t.TempDir()).  In production, worktrees are never under /tmp.
	if worktree != "" {
		spec := container.MountSpec{
			HostPath:    worktree,
			SandboxPath: worktree,
			ReadOnly:    worktreeReadOnly,
		}
		args = container.AppendBwrapBind(args, spec)
	}

	return args
}
