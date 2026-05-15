// bash_sandbox_linux.go — Linux implementation of the per-tool bash sandbox.
//
// On Linux we use bubblewrap (bwrap) to create a restricted subprocess with
// the bash-specific mount set described in the daemon-mode design doc §5.
//
// The bash mount set extends the file-tool baseline with:
//   - ~/.cache/nix (RW)
//   - /nix/var/nix/daemon-socket (RW)
//   - Synthesised ~/.gitconfig (RO) — written to a temp file per call
//   - ~/.ssh/access-key, signing-key, signing-key.pub, allowed_signers,
//     known_hosts (RO, same remap as container/bwrap.go)
//   - Synthesised ~/.ssh/config (RO)
//   - ~/.aws/config, ~/.aws/credentials (RO, EvalSymlinks)
//   - ~/.aws/sso (RW, conditional)
//   - ~/.aws/cli (RW, conditional)
//   - ~/.kube/config (RO, conditional)
//
// Network is permitted (bwrap --share-net is not passed, so network is
// inherited from the parent namespace — i.e. unrestricted).
//
// The mount list is built fresh per tool call; StandardSandboxMounts is NOT
// used — it carries pi-process credentials that must be excluded.

package iris

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/container"
)

// runBashInSandbox runs a bash command in a bwrap sandbox with the D-5 mount
// set and credential injection.  It is the Linux implementation of the bash
// tool executor.
//
// The function:
//  1. Generates a per-call gitconfig and SSH config temp file.
//  2. Builds the bwrap mount argument list (bash mount set per design doc §5).
//  3. Builds the subprocess environment (bashEnv).
//  4. Runs the bash subprocess via runSubprocess (streaming/abort/spill from D-3).
//
// Returns a toolResult.
func (d *toolDispatcher) runBashInSandbox(ctx context.Context, command string) toolResult {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// Generate per-call gitconfig and SSH config temp files.
	gitconfigPath, sshConfigPath, allowedSignersPath, allowedSignersReady, cleanup, err := generateBashSandboxConfigs(home)
	if err != nil {
		return toolResult{Success: false, IsError: true, Output: fmt.Sprintf("bash sandbox: prepare configs: %v", err)}
	}
	defer cleanup()

	// Build the mount argument list.
	mountArgs := bashToolMountsLinux(home, d.worktree, d.tmpDir, gitconfigPath, sshConfigPath, allowedSignersPath, allowedSignersReady)

	// Build bwrap args: clearenv + mounts + setenv pairs.
	var args []string
	args = append(args,
		"--clearenv",
		"--unshare-pid",
		"--proc", "/proc",
		"--dev", "/dev",
		"--die-with-parent",
	)
	args = append(args, mountArgs...)

	// Inject env vars via --setenv. Use the dispatcher's broker so credential
	// resolution is consistent with the audit-log decision recorded against
	// the matching tool_call event (D-7).
	broker := d.broker
	if broker == nil {
		broker = NewCredentialBroker()
	}
	for _, kv := range broker.ResolveBash(d.role, d.bareRoot).Env {
		k, v, _ := strings.Cut(kv, "=")
		args = append(args, "--setenv", k, v)
	}

	// Set working directory to the worktree.
	if d.worktree != "" {
		args = append(args, "--chdir", d.worktree)
	}

	args = append(args, "--", "bash", "-c", command)

	return d.runSubprocess(ctx, d.worktree, nil, "bwrap", args...)
}

// bashToolMountsLinux builds the bwrap mount argument list for the bash tool.
// The mount set is a superset of the file-tool mount set (D-4) with the
// additional mounts required by bash (git, SSH, AWS, kube, nix daemon).
func bashToolMountsLinux(home, worktree, tmpDir, gitconfigPath, sshConfigPath, allowedSignersPath string, allowedSignersReady bool) []string {
	var args []string

	// ── System RO roots (same as file tools) ────────────────────────────────
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

	// ── Per-session /tmp (RW) backed by tmpDir ───────────────────────────────
	if tmpDir != "" {
		if mkErr := os.MkdirAll(tmpDir, 0o755); mkErr == nil {
			args = append(args, "--bind", tmpDir, "/tmp")
		}
	} else {
		args = append(args, "--tmpfs", "/tmp")
	}

	// When the worktree is under the host's /tmp, pre-create the mount-point
	// directory inside tmpDir so bwrap can bind-mount at the correct
	// destination (same pattern as file_sandbox_linux.go).
	if tmpDir != "" && worktree != "" && strings.HasPrefix(worktree, "/tmp/") {
		relToTmp := worktree[len("/tmp/"):]
		mountPoint := filepath.Join(tmpDir, relToTmp)
		_ = os.MkdirAll(mountPoint, 0o755)
	}

	// ── Shadow sensitive /etc subtrees ──────────────────────────────────────
	for _, sensitiveDir := range []string{
		"/etc/wireguard",
		"/etc/wpa_supplicant",
		"/etc/ssh",
	} {
		if _, err := os.Stat(sensitiveDir); err == nil {
			args = append(args, "--tmpfs", sensitiveDir)
		}
	}

	// ── Worktree (RW) — mounted LAST (same ordering as file tools) ──────────
	if worktree != "" {
		spec := container.MountSpec{
			HostPath:    worktree,
			SandboxPath: worktree,
			ReadOnly:    false, // bash needs RW worktree
		}
		args = container.AppendBwrapBind(args, spec)
	}

	// ── ~/.cache/nix (RW) ───────────────────────────────────────────────────
	cacheNix := filepath.Join(home, ".cache", "nix")
	args = container.AppendBwrapBind(args, container.MountSpec{
		HostPath:          cacheNix,
		SandboxPath:       cacheNix,
		ReadOnly:          false,
		OptionalIfMissing: true,
	})

	// ── /nix/var/nix/daemon-socket (RW) ─────────────────────────────────────
	// Mount the whole dir (same as container/bwrap.go pattern).
	const nixDaemonSocketDir = "/nix/var/nix/daemon-socket"
	if _, err := os.Stat(nixDaemonSocketDir); err == nil {
		args = append(args, "--bind", nixDaemonSocketDir, nixDaemonSocketDir)
	}

	// ── SSH keys (RO, remapped to generic names) ────────────────────────────
	sshDir := filepath.Join(home, ".ssh")
	const accessKeyName = "prismatic-koi-ed25519"
	if resolved, err := filepath.EvalSymlinks(filepath.Join(sshDir, accessKeyName)); err == nil {
		args = append(args, "--ro-bind", resolved, filepath.Join(sshDir, "access-key"))
	}
	const signingKeyName = "prismatic-koi-ed25519-signingkey"
	signingKeyResolved, errPriv := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName))
	signingKeyPubResolved, errPub := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName+".pub"))
	if errPriv == nil && errPub == nil {
		args = append(args,
			"--ro-bind", signingKeyResolved, filepath.Join(sshDir, "signing-key"),
			"--ro-bind", signingKeyPubResolved, filepath.Join(sshDir, "signing-key.pub"),
		)
		if allowedSignersReady && allowedSignersPath != "" {
			args = append(args, "--ro-bind", allowedSignersPath, filepath.Join(sshDir, "allowed_signers"))
		}
	}
	if resolved, err := filepath.EvalSymlinks(filepath.Join(sshDir, "known_hosts")); err == nil {
		args = append(args, "--ro-bind", resolved, filepath.Join(sshDir, "known_hosts"))
	}

	// ── Synthesised ~/.ssh/config (RO) ──────────────────────────────────────
	if sshConfigPath != "" {
		args = append(args, "--ro-bind", sshConfigPath, filepath.Join(home, ".ssh", "config"))
	}

	// ── Synthesised ~/.gitconfig (RO) ────────────────────────────────────────
	if gitconfigPath != "" {
		args = append(args, "--ro-bind", gitconfigPath, filepath.Join(home, ".gitconfig"))
	}

	// ── AWS credentials (RO, EvalSymlinks for sops-managed files) ───────────
	// ~/.config/aws/readonly-config → ~/.aws/config
	// ~/.config/aws/credentials     → ~/.aws/credentials
	awsConfigSpecs := []container.MountSpec{
		{
			HostPath:     filepath.Join(home, ".config", "aws", "readonly-config"),
			SandboxPath:  filepath.Join(home, ".aws", "config"),
			ReadOnly:     true,
			EvalSymlinks: true,
		},
		{
			HostPath:     filepath.Join(home, ".config", "aws", "credentials"),
			SandboxPath:  filepath.Join(home, ".aws", "credentials"),
			ReadOnly:     true,
			EvalSymlinks: true,
		},
		// AWS SSO cache (RW, conditional) — needed for SSO token refresh.
		{
			HostPath:          filepath.Join(home, ".aws", "sso"),
			SandboxPath:       filepath.Join(home, ".aws", "sso"),
			ReadOnly:          false,
			OptionalIfMissing: true,
		},
		// AWS CLI cache (RW, conditional) — needed for STS token caching.
		{
			HostPath:          filepath.Join(home, ".aws", "cli"),
			SandboxPath:       filepath.Join(home, ".aws", "cli"),
			ReadOnly:          false,
			OptionalIfMissing: true,
		},
	}
	for _, spec := range awsConfigSpecs {
		args = container.AppendBwrapBind(args, spec)
	}

	// ── ~/.kube/config (RO, conditional) ────────────────────────────────────
	// Kube config: host path is ~/.config/kube/agents-config (sops-managed),
	// sandbox path is ~/.kube/config (canonical kubectl default).
	args = container.AppendBwrapBind(args, container.MountSpec{
		HostPath:     filepath.Join(home, ".config", "kube", "agents-config"),
		SandboxPath:  filepath.Join(home, ".kube", "config"),
		ReadOnly:     true,
		EvalSymlinks: true,
	})

	return args
}
