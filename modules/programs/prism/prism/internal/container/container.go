// Package container manages the podman container lifecycle for prism sidecar.
//
// The sidecar (running on the host) creates a podman container running
// "opencode serve --port 4096", health-checks it until the HTTP endpoint
// responds, and stops/removes the container on shutdown.
//
// Design notes:
//   - All podman operations use exec.Command("podman", ...) — no daemon or
//     socket is required from Go's perspective, just a podman binary on PATH.
//   - The container name is derived from the prism session name so it is
//     predictable and idempotent.
//   - Credentials are injected as environment variables, never as mounted files.
//   - The container port is bound to 127.0.0.1 only — not 0.0.0.0.
package container

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ContainerPort is the port opencode serve listens on inside the container.
	ContainerPort = 4096

	// Image is the container image name used for all agent containers.
	// The localhost/ prefix is required for podman to resolve locally-loaded
	// images correctly on both Linux and Darwin. Without it, podman falls
	// through to unqualified-search registries and tries docker.io/library/,
	// which fails for images that were loaded via `podman load`.
	Image = "localhost/prism-agent:latest"

	// DefaultHealthCheckTimeout is the maximum time to wait for the container
	// to become healthy before giving up.
	DefaultHealthCheckTimeout = 60 * time.Second

	// healthCheckInterval is the pause between consecutive health-check probes.
	healthCheckInterval = 500 * time.Millisecond
)

// Config holds the parameters for creating and managing a container.
type Config struct {
	// SessionName is the prism session name (e.g. "nixos-config@feature").
	// Used to derive a stable container name.
	SessionName string

	// Worktree is the absolute path to the git worktree on the host.
	// Mounted read-write at /workspace inside the container.
	Worktree string

	// AllocatedPort is the host port to bind to ContainerPort inside the container.
	AllocatedPort int

	// AgentRole is "worker" or "coordinator". Controls which GitHub token is
	// injected into the container.
	AgentRole string

	// AgentModel is the model identifier to use when delivering the initial
	// prompt (e.g. "anthropic/claude-sonnet-4-6"). When empty, opencode's
	// default model for the session is used (which may differ from the host
	// opencode config and cause "model not supported" errors).
	AgentModel string

	// ConfigContent is the JSON blob for the OPENCODE_CONFIG_CONTENT environment
	// variable. When non-empty, it is injected as an env var into the container
	// so that opencode serve picks up the correct model and variant overrides at
	// runtime (precedence level 6 in opencode's config merge).
	//
	// Previously this was done via --model and --variant flags on opencode serve,
	// but those flags are not accepted by the serve subcommand. Using
	// OPENCODE_CONFIG_CONTENT works for both serve and the TUI/attach path.
	ConfigContent string

	// PluginHostPath is the absolute path to the prism-hooks plugin file on the
	// host (e.g. ~/.config/opencode/plugins/prism-hooks.ts). It is mounted
	// read-only into the container's opencode plugin directory.
	PluginHostPath string

	// ContainerWorkerConfigPath is the Nix store path to the pre-built worker
	// opencode config directory. When non-empty and AgentRole is not
	// "coordinator", this directory is bind-mounted at /root/.config/opencode
	// (read-only) instead of mirroring the host config item-by-item.
	ContainerWorkerConfigPath string

	// ContainerCoordinatorConfigPath is the Nix store path to the pre-built
	// coordinator opencode config directory. When non-empty and AgentRole is
	// "coordinator", this directory is bind-mounted at /root/.config/opencode
	// (read-only) instead of mirroring the host config item-by-item.
	ContainerCoordinatorConfigPath string

	// BareRoot is the absolute path to the bare git repo root on the host
	// (the directory containing .bare/). When set, the bare repo and the
	// worktree's private git state are mounted into the container so that
	// git works correctly without needing to follow the absolute gitdir
	// pointer in the worktree's .git file.
	//
	// The .bare directory is mounted at /prism-git inside the container,
	// mirroring the relative layout git expects: the worktree private state
	// is at /prism-git/worktrees/<branch>, and commondir ("../..") resolves
	// naturally to /prism-git (the bare repo).
	BareRoot string
	// WorktreeGitDir is the absolute path to the worktree's private git
	// state directory on the host (e.g. <BareRoot>/.bare/worktrees/<branch>).
	// Mounted read-write at /prism-git/worktrees/<branch> inside the container.
	WorktreeGitDir string

	// HostAPISockPath is the absolute host path to the sidecar's host-API Unix socket.
	// When non-empty, the socket's parent directory is bind-mounted into the container
	// at /var/run/prism-host and PRISM_HOST_API is set to
	// unix:///var/run/prism-host/<sockfilename> so that prism CLI commands inside
	// the container can proxy tmux operations to the host sidecar.
	HostAPISockPath string

	// HealthCheckTimeout overrides DefaultHealthCheckTimeout when non-zero.
	HealthCheckTimeout time.Duration

	// HTTPClient is used for health-check probes. Defaults to a short-timeout
	// client when nil.
	HTTPClient *http.Client

	// GitUserName is the git user.name to write into the container's .gitconfig.
	// When empty, the [user] section is omitted from the generated gitconfig.
	GitUserName string

	// GitUserEmail is the git user.email to write into the container's .gitconfig.
	// When empty, the [user] section is omitted from the generated gitconfig.
	GitUserEmail string

	// SshAccessKeyName is the filename (not full path) of the SSH access key in
	// ~/.ssh/. When empty, defaults to "prismatic-koi-ed25519".
	SshAccessKeyName string

	// SshSigningKeyName is the base filename (not full path) of the SSH signing
	// key in ~/.ssh/. The public key is derived by appending ".pub". When empty,
	// defaults to "prismatic-koi-ed25519-signingkey".
	SshSigningKeyName string
}

// NameForSession returns the stable podman container name for a session.
// The name is derived from the session name with "@", "/", and "." replaced
// by "-" and a "prism-" prefix, e.g. "prism-nixos-config-feature".
func NameForSession(sessionName string) string {
	safe := strings.ReplaceAll(sessionName, "@", "-")
	safe = strings.ReplaceAll(safe, "/", "-")
	safe = strings.ReplaceAll(safe, ".", "-")
	return "prism-" + safe
}

// containerName is the unexported alias kept for internal use.
func containerName(sessionName string) string {
	return NameForSession(sessionName)
}

// Manager manages the lifecycle of a single podman container for a session.
type Manager struct {
	cfg            Config
	name           string
	healthCheckURL string
	httpClient     *http.Client
	// allowedSignersReady is true when writeGitconfig successfully wrote the
	// allowed_signers temp file. buildRunArgs uses this to gate the bind-mount
	// so that podman is never given a source path that doesn't exist on disk.
	allowedSignersReady bool
}

// New creates a Manager for the given config. It does not start the container.
func New(cfg Config) *Manager {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Manager{
		cfg:            cfg,
		name:           containerName(cfg.SessionName),
		healthCheckURL: fmt.Sprintf("http://127.0.0.1:%d/", cfg.AllocatedPort),
		httpClient:     httpClient,
	}
}

// Name returns the container name.
func (m *Manager) Name() string { return m.name }

// EnsureRemoved stops and removes any existing container with the same name,
// and cleans up any temp files created by Create. It is safe to call when no
// such container exists — errors for "no such container" are silently ignored.
func (m *Manager) EnsureRemoved(ctx context.Context) {
	// Clean up temp files created by Create.
	_ = os.Remove(m.gitdirFilePath())
	_ = os.Remove(m.worktreeGitdirFilePath())
	_ = os.Remove(m.sshConfigFilePath())
	_ = os.Remove(m.gitconfigFilePath())
	_ = os.Remove(m.allowedSignersFilePath())
	// Stop the container (ignore errors — may not be running).
	stopCmd := exec.CommandContext(ctx, "podman", "stop", "--time", "10", m.name)
	if out, err := stopCmd.CombinedOutput(); err != nil {
		// Only log if it looks like a real error (not "no such container").
		if !IsNoSuchContainerError(string(out)) {
			log.Printf("container: stop existing %q: %v — %s", m.name, err, strings.TrimSpace(string(out)))
		}
	}

	// Remove the container (ignore errors — may not exist).
	rmCmd := exec.CommandContext(ctx, "podman", "rm", "--force", m.name)
	if out, err := rmCmd.CombinedOutput(); err != nil {
		if !IsNoSuchContainerError(string(out)) {
			log.Printf("container: rm existing %q: %v — %s", m.name, err, strings.TrimSpace(string(out)))
		}
	}
}

// gitdirFilePath returns the host path for the temporary corrected .git pointer
// file written before container start. The file is named after the container
// so it is stable and can be cleaned up by EnsureRemoved.
func (m *Manager) gitdirFilePath() string {
	return filepath.Join(os.TempDir(), "prism-gitdir-"+m.name)
}

// sshConfigFilePath returns the host path for the temporary SSH config
// written before container start. The container needs a minimal SSH config
// for git push/fetch over SSH remotes, but the host's ~/.ssh/config is a
// nix store symlink with wrong ownership (nobody:nogroup, 0444) which SSH
// rejects. We write a simple config that points to the mounted key.
func (m *Manager) sshConfigFilePath() string {
	return filepath.Join(os.TempDir(), "prism-ssh-config-"+m.name)
}

// gitconfigFilePath returns the host path for the temporary .gitconfig
// written before container start. The container needs a minimal gitconfig
// for commit identity and SSH signing. Mounted read-only at /root/.gitconfig.
func (m *Manager) gitconfigFilePath() string {
	return filepath.Join(os.TempDir(), "prism-gitconfig-"+m.name)
}

// allowedSignersFilePath returns the host path for the temporary
// allowed_signers file written before container start. The file is mounted
// read-only at /root/.ssh/allowed_signers and is required for
// git verify-commit to work with SSH signing.
func (m *Manager) allowedSignersFilePath() string {
	return filepath.Join(os.TempDir(), "prism-allowed-signers-"+m.name)
}

// writeGitconfig generates a minimal .gitconfig for the container and writes
// it to a temp file. The file is later mounted at /root/.gitconfig:ro.
//
// Sections included:
//   - [user] — only when GitUserName and GitUserEmail are both non-empty;
//     includes signingKey when the signing public key can be resolved.
//   - [commit] / [gpg] — only when the signing keys are available.
//   - [push] — autoSetupRemote = true (always).
//   - [init] — defaultBranch = main (always).
//
// Missing identity → [user] section omitted, warning logged (AC-14).
// Missing signing keys → signing sections omitted, warning logged (AC-13).
func (m *Manager) writeGitconfig() error {
	// Reset allowedSignersReady so that a retry or second call doesn't carry
	// stale state from a previous successful write into a new call that may fail.
	m.allowedSignersReady = false

	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	sshDir := filepath.Join(home, ".ssh")

	// Check whether signing keys are resolvable.
	signingKeyName := m.cfg.SshSigningKeyName
	if signingKeyName == "" {
		signingKeyName = "prismatic-koi-ed25519-signingkey"
	}
	signingKeyPriv := filepath.Join(sshDir, signingKeyName)
	signingKeyPub := filepath.Join(sshDir, signingKeyName+".pub")
	_, errPriv := filepath.EvalSymlinks(signingKeyPriv)
	_, errPub := filepath.EvalSymlinks(signingKeyPub)
	hasSigning := errPriv == nil && errPub == nil
	if !hasSigning {
		// Log each missing key separately so the message is not misleading when
		// only one of the two is unresolvable.
		if errPriv != nil {
			log.Printf("container: signing key (private) not resolvable: %v; omitting signing config from gitconfig", errPriv)
		}
		if errPub != nil {
			log.Printf("container: signing key (public) not resolvable: %v; omitting signing config from gitconfig", errPub)
		}
	}

	var sb strings.Builder

	// [user] section — only when identity is present.
	if m.cfg.GitUserName != "" && m.cfg.GitUserEmail != "" {
		sb.WriteString("[user]\n")
		sb.WriteString("    name = " + m.cfg.GitUserName + "\n")
		sb.WriteString("    email = " + m.cfg.GitUserEmail + "\n")
		if hasSigning {
			sb.WriteString("    signingKey = /root/.ssh/signing-key.pub\n")
		}
	} else {
		log.Printf("container: git identity (name=%q, email=%q) missing; omitting [user] section from gitconfig",
			m.cfg.GitUserName, m.cfg.GitUserEmail)
	}

	// [commit] and [gpg] sections — only when signing keys are available AND
	// identity is present. Without identity the [user] section is omitted, which
	// means signingKey is never set; writing gpgsign=true in that case would
	// cause every git commit to fail.
	if hasSigning && m.cfg.GitUserName != "" && m.cfg.GitUserEmail != "" {
		// Read the signing public key content to build the allowed_signers file.
		// Only write [gpg "ssh"] allowedSignersFile when the file was actually
		// produced — if it can't be written, podman must not be given a
		// bind-mount source path that doesn't exist on disk.
		pubKeyContent, err := os.ReadFile(signingKeyPub)
		if err != nil {
			log.Printf("container: failed to read signing public key %q: %v; skipping allowed_signers", signingKeyPub, err)
		} else {
			allowedSignersContent := m.cfg.GitUserEmail + " " + strings.TrimSpace(string(pubKeyContent)) + "\n"
			if err := os.WriteFile(m.allowedSignersFilePath(), []byte(allowedSignersContent), 0o644); err != nil {
				log.Printf("container: failed to write allowed_signers file: %v", err)
			} else {
				m.allowedSignersReady = true
			}
		}

		// Only enable signing config when allowed_signers was successfully
		// written — without it, commits would be signed but unverifiable.
		if m.allowedSignersReady {
			sb.WriteString("\n[commit]\n")
			sb.WriteString("    gpgsign = true\n")
			sb.WriteString("\n[gpg]\n")
			sb.WriteString("    format = ssh\n")
			sb.WriteString("\n[gpg \"ssh\"]\n")
			sb.WriteString("    allowedSignersFile = /root/.ssh/allowed_signers\n")
		}
	}

	// [push] and [init] — always included.
	sb.WriteString("\n[push]\n")
	sb.WriteString("    autoSetupRemote = true\n")
	sb.WriteString("\n[init]\n")
	sb.WriteString("    defaultBranch = main\n")

	if err := os.WriteFile(m.gitconfigFilePath(), []byte(sb.String()), 0o644); err != nil {
		return err
	}
	return nil
}

// worktreeGitdirFilePath returns the host path for the temporary corrected
// worktree back-pointer file. The worktrees/<branch>/gitdir file is a
// reverse pointer from the git metadata back to the worktree's .git file.
// On the host it contains an absolute host path (e.g.
// /home/user/code/repo/branch/.git) which doesn't exist inside the container.
// We write a temp file with the container-internal path (/workspace/.git) and
// bind-mount it over the original so that nix/libgit2 can resolve the full
// worktree chain.
func (m *Manager) worktreeGitdirFilePath() string {
	return filepath.Join(os.TempDir(), "prism-wt-gitdir-"+m.name)
}

// Create creates and starts the podman container for this session.
// It first calls EnsureRemoved to handle any stale container with the same name.
// Returns an error if podman create or start fails.
func (m *Manager) Create(ctx context.Context) error {
	// Remove any stale container first (AC-15).
	m.EnsureRemoved(ctx)

	// Write corrected git pointer files for the container.
	if m.cfg.BareRoot != "" && m.cfg.WorktreeGitDir != "" {
		branch := filepath.Base(m.cfg.WorktreeGitDir)

		// Forward pointer: /workspace/.git → /prism-git/worktrees/<branch> (#492).
		// The host's .git file contains an absolute host path that doesn't exist
		// inside the container.
		gitdirContent := "gitdir: /prism-git/worktrees/" + branch + "\n"
		if err := os.WriteFile(m.gitdirFilePath(), []byte(gitdirContent), 0o644); err != nil {
			return fmt.Errorf("container: write gitdir file: %w", err)
		}

		// Back-pointer: worktrees/<branch>/gitdir → /workspace/.git
		// This file is the reverse pointer from git metadata back to the
		// worktree checkout. On the host it contains the host worktree path
		// (e.g. /home/user/code/repo/branch/.git) which doesn't exist in the
		// container. nix/libgit2 follows this pointer when resolving the
		// working tree, so it must point to the container-internal path.
		wtGitdirContent := "/workspace/.git\n"
		if err := os.WriteFile(m.worktreeGitdirFilePath(), []byte(wtGitdirContent), 0o644); err != nil {
			return fmt.Errorf("container: write worktree gitdir file: %w", err)
		}
	}

	// Write a minimal SSH config for the container. The host's ~/.ssh/config
	// is a nix store symlink with wrong ownership that SSH rejects. This
	// config only needs to handle GitHub; other SSH hosts are not expected
	// inside agent containers.
	sshConfig := "Host github.com\n  StrictHostKeyChecking accept-new\n  IdentityFile /root/.ssh/access-key\n  IdentitiesOnly yes\n"
	if err := os.WriteFile(m.sshConfigFilePath(), []byte(sshConfig), 0o600); err != nil {
		return fmt.Errorf("container: write ssh config: %w", err)
	}

	// Write a minimal .gitconfig for the container. The host's git config lives
	// in ~/.config/git/config (managed by home-manager) and is not mounted into
	// containers. We generate a minimal config with identity, signing, and
	// convenience settings.
	if err := m.writeGitconfig(); err != nil {
		return fmt.Errorf("container: write gitconfig: %w", err)
	}

	// Build the podman run arguments.
	args := m.buildRunArgs()

	log.Printf("container: creating %q: podman %s", m.name, strings.Join(redactArgs(args), " "))

	cmd := exec.CommandContext(ctx, "podman", args...)
	cmd.Stdout = os.Stderr // forward container stdout to sidecar's stderr log
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container: podman run %q: %w", m.name, err)
	}

	return nil
}

// WaitHealthy polls the container's HTTP endpoint until it responds with a
// successful status code (2xx) or the timeout expires. Returns nil when the
// container is healthy.
func (m *Manager) WaitHealthy(ctx context.Context) error {
	timeout := m.cfg.HealthCheckTimeout
	if timeout == 0 {
		timeout = DefaultHealthCheckTimeout
	}

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("container: health check timed out after %s for %q", timeout, m.name)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if m.isHealthy(ctx) {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthCheckInterval):
		}
	}
}

// isHealthy performs a single HTTP probe and returns true on success.
func (m *Manager) isHealthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.healthCheckURL, nil)
	if err != nil {
		return false
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Shutdown stops and removes the container. Intended to be called on SIGTERM.
// It uses a background context so the cleanup proceeds even if the parent ctx
// is already cancelled.
func (m *Manager) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("container: shutting down %q", m.name)

	stopCmd := exec.CommandContext(ctx, "podman", "stop", "--time", "10", m.name)
	if out, err := stopCmd.CombinedOutput(); err != nil && !IsNoSuchContainerError(string(out)) {
		log.Printf("container: stop %q: %v — %s", m.name, err, strings.TrimSpace(string(out)))
	}

	rmCmd := exec.CommandContext(ctx, "podman", "rm", "--force", m.name)
	if out, err := rmCmd.CombinedOutput(); err != nil && !IsNoSuchContainerError(string(out)) {
		log.Printf("container: rm %q: %v — %s", m.name, err, strings.TrimSpace(string(out)))
	}

	// Clean up temp files created by Create.
	_ = os.Remove(m.gitdirFilePath())
	_ = os.Remove(m.worktreeGitdirFilePath())
	_ = os.Remove(m.sshConfigFilePath())
	_ = os.Remove(m.gitconfigFilePath())
	_ = os.Remove(m.allowedSignersFilePath())

	log.Printf("container: %q removed", m.name)
}

// buildRunArgs constructs the podman run argument list. Exported for testing.
func (m *Manager) buildRunArgs() []string {
	cfg := m.cfg

	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// Container port binding: localhost only (AC-6).
	portBinding := fmt.Sprintf("127.0.0.1:%d:%d", cfg.AllocatedPort, ContainerPort)

	// Volume mounts (AC-2, AC-3, AC-4, AC-5).
	worktreeMount := cfg.Worktree + ":/workspace:Z"
	opencodeStateMount := filepath.Join(home, ".local", "share", "opencode") +
		":/root/.local/share/opencode:Z"
	// opencodeConfigDir is the host's opencode config directory, used only in
	// the legacy fallback path when role-specific config derivation paths are
	// not configured (ContainerWorkerConfigPath / ContainerCoordinatorConfigPath).
	opencodeConfigDir := filepath.Join(home, ".config", "opencode")

	// opencode cache — mount the whole directory so plugins, models.json,
	// package.json, and bun.lock are all available without network access.
	opencodeCacheMount := filepath.Join(home, ".cache", "opencode") +
		":/root/.cache/opencode:ro"
	// bun transpiler cache — required for bun to load plugins without
	// re-transpiling on every container start.
	bunCacheMount := filepath.Join(home, ".cache", "bun") +
		":/root/.cache/bun:ro"
	// Claude credentials — required for Anthropic provider auth. Mounted
	// read-write so the opencode-claude-auth plugin can write back refreshed
	// OAuth tokens to .credentials.json inside the container.
	claudeMount := filepath.Join(home, ".claude") + ":/root/.claude"
	// Nix eval cache — pre-populated git cache from the host so flake input
	// tarballs (nixpkgs, home-manager, etc.) don't need to be re-fetched and
	// unpacked on every container start. Read-write because nix writes to
	// SQLite databases (fetcher-cache, binary-cache, eval-cache) during
	// evaluation; the :Z label handles SELinux relabeling.
	nixCacheMount := filepath.Join(home, ".cache", "nix") + ":/root/.cache/nix:Z"

	args := []string{
		"run",
		"--detach",
		"--name", m.name,

		// Network: pasta (rootless default on podman 5.x) provides outbound
		// NAT via the host's network, but the container cannot reach host
		// loopback services or other containers directly. Declaring it explicitly
		// makes the network policy declarative rather than relying on the
		// podman default — satisfying AC-12.
		"--network", "pasta",

		// Port — bound to localhost only (AC-6).
		"--publish", portBinding,

		// Worktree read-write.
		"--volume", worktreeMount,
		// opencode state — shared with host, read-write.
		"--volume", opencodeStateMount,
		// opencode cache — plugins, models, bun.lock from host, read-only.
		"--volume", opencodeCacheMount,
		// bun transpiler cache — pre-compiled plugin modules from host.
		"--volume", bunCacheMount,
		// Claude credentials — read-write for auth plugin token refresh.
		"--volume", claudeMount,
		// Nix eval cache — flake input tarballs pre-unpacked from the host.
		"--volume", nixCacheMount,

		// Nix daemon socket — lets the container's nix CLI delegate store
		// operations to the host's nix-daemon, reusing the host's /nix/store
		// cache and persisting new build outputs for reuse across container
		// restarts. The host's nix-daemon trusts @wheel users (nix-options.nix);
		// in rootless podman, container root maps to the host user who is in
		// the wheel group, so the daemon accepts the connection.
		//
		// NIX_CONFIG sets store=daemon so nix uses the host daemon for all
		// store operations. The container image includes a nix wrapper script
		// that automatically injects --eval-store auto when the socket is
		// present, so evaluation happens locally (can see /workspace) while
		// builds/fetches go through the daemon.
		"--volume", "/nix/var/nix/daemon-socket/socket:/nix/var/nix/daemon-socket/socket",
		"--env", "NIX_CONFIG=store = daemon",

		// Prism context — tells prism CLI commands running inside the container
		// where the worktree and bare repo are mounted. PRISM_SPAWN_PATH is the
		// existing escape hatch used by prism spawn / list-sessions to infer the
		// current repo without a tmux pane path. PRISM_BARE_ROOT is a new var
		// that resolveBareRoot uses as a direct bare-root override when the
		// parent-walk heuristic cannot find .bare (which is always the case
		// inside a container where the bare repo is at /prism-git, not a parent
		// of /workspace).
		"--env", "PRISM_SPAWN_PATH=/workspace",
		"--env", "PRISM_BARE_ROOT=/prism-git",

		// Work inside the worktree by default.
		"--workdir", "/workspace",
	}

	// Host-API socket: mount the sidecar's Unix socket into the container and
	// tell prism CLI where to find it. Container root can access the socket because
	// rootless podman maps container root to the host user UID (same mechanism as
	// the nix-daemon socket mount already in use above).
	if cfg.HostAPISockPath != "" {
		args = append(args,
			"--volume", filepath.Dir(cfg.HostAPISockPath)+":/var/run/prism-host:Z",
			"--env", "PRISM_HOST_API=unix:///var/run/prism-host/"+filepath.Base(cfg.HostAPISockPath),
		)
	}

	// Mount the opencode config directory into /root/.config/opencode.
	//
	// Preferred path: use the role-appropriate pre-built Nix derivation as a
	// single read-only bind mount. This avoids symlink-resolution complexity
	// and ensures worker/coordinator containers get distinct, minimal configs.
	//
	// Fallback (backward compat): when the role-appropriate config path is
	// empty (old config without the new keys, or opencode.enable = false),
	// mirror the host config item-by-item, resolving Nix store symlinks so
	// they are accessible inside the container.
	//
	// AC-18: if either role-specific path is absent, fall back. In particular:
	//   - coordinator role + empty coordinator path → fallback (not worker path)
	//   - worker/unknown role + empty worker path → fallback
	var roleConfigPath string
	if cfg.AgentRole == "coordinator" {
		roleConfigPath = cfg.ContainerCoordinatorConfigPath
		// empty → stays "" → legacy fallback
	} else {
		roleConfigPath = cfg.ContainerWorkerConfigPath
		// empty → stays "" → legacy fallback
	}

	if roleConfigPath != "" {
		// Single bind mount of the pre-built config derivation (read-only).
		args = append(args, "--mount",
			"type=bind,src="+roleConfigPath+",dst=/root/.config/opencode,ro")
	} else {
		// Legacy fallback: mount each ~/.config/opencode entry individually,
		// resolving Nix store symlinks so they are accessible inside the container.
		// The whole-dir mount is intentionally omitted — it would create a
		// read-only container directory that prevents --mount from adding
		// resolved symlink targets inside it.
		//
		// For each entry:
		//   - Symlinks → resolve to real Nix store path and mount that
		//   - Real entries → mount directly
		//   - Directories → use --mount type=bind (podman creates dest automatically)
		//   - Regular files → use --volume
		if entries, err := os.ReadDir(opencodeConfigDir); err == nil {
			for _, entry := range entries {
				// Skip the plugins directory — it contains symlinks into the Nix
				// store that are not available inside the container. Individual
				// plugin files are mounted separately below (see PluginHostPath).
				if entry.Name() == "plugins" {
					continue
				}
				hostPath := filepath.Join(opencodeConfigDir, entry.Name())
				resolved, err := filepath.EvalSymlinks(hostPath)
				if err != nil {
					continue
				}
				containerPath := "/root/.config/opencode/" + entry.Name()
				if fi, err := os.Stat(resolved); err == nil && fi.IsDir() {
					args = append(args, "--mount",
						"type=bind,src="+resolved+",dst="+containerPath+",ro")
				} else {
					args = append(args, "--volume", resolved+":"+containerPath+":ro")
				}
			}
		}
	}

	// Mount individual SSH keys and a generated config for git push/fetch and
	// commit signing over SSH.
	// The host's ~/.ssh/ contains sops-nix symlinks to /run/secrets/ssh/ and a
	// nix store symlink for the config — neither resolves inside the container.
	// We mount only the specific files we need (resolving symlinks to real
	// paths) and overlay a generated config with correct permissions.
	// Note: the whole ~/.ssh directory is NOT mounted — only individual files.
	sshDir := filepath.Join(home, ".ssh")

	// Access key (git push/fetch): <SshAccessKeyName> → /root/.ssh/access-key
	accessKeyName := m.cfg.SshAccessKeyName
	if accessKeyName == "" {
		accessKeyName = "prismatic-koi-ed25519"
	}
	if resolved, err := filepath.EvalSymlinks(filepath.Join(sshDir, accessKeyName)); err == nil {
		args = append(args, "--volume", resolved+":/root/.ssh/access-key:ro")
	} else {
		log.Printf("container: access key not resolvable (%v); SSH push/fetch will not work", err)
	}

	// Signing key private (commit signing): <SshSigningKeyName> → /root/.ssh/signing-key
	// Signing key public (gitconfig signingKey): <SshSigningKeyName>.pub → /root/.ssh/signing-key.pub
	//
	// Note: writeGitconfig (called immediately before buildRunArgs in Create)
	// also resolves these symlinks to determine hasSigning. The resolutions are
	// intentionally independent — writeGitconfig decides what to write into the
	// gitconfig file, while this block decides what volumes to mount. Both must
	// agree for the container to work, and they do because Create() calls them
	// sequentially with no symlink changes possible between the two calls.
	signingKeyName := m.cfg.SshSigningKeyName
	if signingKeyName == "" {
		signingKeyName = "prismatic-koi-ed25519-signingkey"
	}
	signingKeyResolved, errPriv := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName))
	signingKeyPubResolved, errPub := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName+".pub"))
	if errPriv == nil && errPub == nil {
		args = append(args,
			"--volume", signingKeyResolved+":/root/.ssh/signing-key:ro",
			"--volume", signingKeyPubResolved+":/root/.ssh/signing-key.pub:ro",
		)
		// allowed_signers file (git verify-commit): only mount when the file
		// was successfully written by writeGitconfig (tracked via
		// allowedSignersReady). This ensures podman is never given a
		// bind-mount source path that doesn't exist on disk.
		if m.allowedSignersReady {
			args = append(args, "--volume", m.allowedSignersFilePath()+":/root/.ssh/allowed_signers:ro")
		}
	}

	// known_hosts: unchanged
	if resolved, err := filepath.EvalSymlinks(filepath.Join(sshDir, "known_hosts")); err == nil {
		args = append(args, "--volume", resolved+":/root/.ssh/known_hosts:ro")
	}

	// Generated SSH config (points to /root/.ssh/access-key).
	args = append(args, "--volume", m.sshConfigFilePath()+":/root/.ssh/config:ro")

	// Generated .gitconfig (identity, signing, convenience settings).
	args = append(args, "--volume", m.gitconfigFilePath()+":/root/.gitconfig:ro")

	// Git mounts: when BareRoot is set, mount the bare repo and the
	// worktree's private git state so that git works inside the container
	// without following the absolute host path in the worktree's .git file.
	//
	// Layout mirrors what git expects from the commondir pointer:
	//   /prism-git                        ← bare repo (.bare/)
	//   /prism-git/worktrees/<branch>     ← worktree private state
	//
	// The commondir file in the worktree private state says "../.." which
	// from /prism-git/worktrees/<branch> resolves to /prism-git — correct.
	//
	// A corrected .git pointer file (written by Create before container start)
	// is bind-mounted over /workspace/.git so that opencode's internal git
	// library — which reads .git directly rather than honouring GIT_DIR — also
	// resolves to the correct container-internal path (#492).
	if cfg.BareRoot != "" && cfg.WorktreeGitDir != "" {
		branch := filepath.Base(cfg.WorktreeGitDir)
		// Bare repo — read-write so git can write new objects on commit.
		bareMount := filepath.Join(cfg.BareRoot, ".bare") + ":/prism-git:Z"
		// Worktree private state (HEAD, index, logs, etc.) — read-write.
		worktreeGitMount := cfg.WorktreeGitDir + ":/prism-git/worktrees/" + branch + ":Z"
		// Corrected .git pointer — read-only overlay over the host's .git file.
		gitdirMount := m.gitdirFilePath() + ":/workspace/.git:ro"
		// Corrected worktree back-pointer — overlays the gitdir file inside
		// the worktree metadata so nix/libgit2 resolves to /workspace/.git
		// instead of the host path.
		wtGitdirMount := m.worktreeGitdirFilePath() + ":/prism-git/worktrees/" + branch + "/gitdir:ro"
		args = append(args,
			"--volume", bareMount,
			"--volume", worktreeGitMount,
			"--volume", gitdirMount,
			"--volume", wtGitdirMount,
		)
	}

	// Plugin mount (AC-5): mount the prism-hooks plugin if a path was provided.
	if cfg.PluginHostPath != "" {
		// The container's opencode config is at /root/.config/opencode (from the
		// opencode config volume). The plugin lives at the same relative path
		// inside the container.
		containerPluginPath := "/root/.config/opencode/plugins/" + filepath.Base(cfg.PluginHostPath)
		args = append(args, "--volume", cfg.PluginHostPath+":"+containerPluginPath+":ro")
	}

	// Inject credentials as environment variables (AC-10, AC-11).
	for _, kv := range m.credentialEnvVars() {
		args = append(args, "--env", kv)
	}

	// OPENCODE_CONFIG_CONTENT: inject as an env var when set. This overrides
	// model and variant at runtime (precedence level 6 in opencode's config
	// merge). Previously --model and --variant were appended to the opencode
	// serve command, but serve does not accept those flags — env var injection
	// is the correct and more general approach.
	if cfg.ConfigContent != "" {
		args = append(args, "--env", "OPENCODE_CONFIG_CONTENT="+cfg.ConfigContent)
	}

	// Image and command: opencode serve on the container port.
	args = append(args, Image,
		"opencode", "serve",
		"--port", fmt.Sprintf("%d", ContainerPort),
		"--hostname", "0.0.0.0",
	)

	return args
}

// credentialEnvVars returns the environment variable assignments to inject into
// the container based on the agent role and current host environment.
// Only vars that are set on the host are forwarded — unset vars are skipped.
func (m *Manager) credentialEnvVars() []string {
	var vars []string

	// LLM API keys — forwarded for all agent roles.
	llmKeys := []string{
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_API_KEY",
		"GITHUB_COPILOT_TOKEN",
		"DEEPSEEK_API_KEY",
		"OPENROUTER_API_KEY",
	}
	for _, k := range llmKeys {
		if v := os.Getenv(k); v != "" {
			vars = append(vars, k+"="+v)
		}
	}

	// GitHub token — scope differs by role (AC-11).
	// Workers use PRISM_WORKER_GITHUB_TOKEN (read + PR scope).
	// Coordinators use PRISM_COORDINATOR_GITHUB_TOKEN (broader merge scope).
	// Both fall back to GITHUB_TOKEN if the role-specific var is unset.
	switch m.cfg.AgentRole {
	case "coordinator":
		if tok := os.Getenv("PRISM_COORDINATOR_GITHUB_TOKEN"); tok != "" {
			vars = append(vars, "GITHUB_TOKEN="+tok)
		} else if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
			vars = append(vars, "GITHUB_TOKEN="+tok)
		}
	default: // worker and unknown roles
		if tok := os.Getenv("PRISM_WORKER_GITHUB_TOKEN"); tok != "" {
			vars = append(vars, "GITHUB_TOKEN="+tok)
		} else if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
			vars = append(vars, "GITHUB_TOKEN="+tok)
		}
	}

	// Note: GIT_AUTHOR_NAME, GIT_AUTHOR_EMAIL, GIT_COMMITTER_NAME, and
	// GIT_COMMITTER_EMAIL are intentionally NOT injected. The container now
	// has a generated .gitconfig with a [user] section (sourced from prism
	// config). Env vars override gitconfig and would mask a broken gitconfig.

	// Note: GIT_DIR and GIT_COMMON_DIR are intentionally NOT injected.
	// Instead, Create() writes a corrected .git pointer file and bind-mounts it
	// over /workspace/.git so all tools — including opencode's internal git
	// library which reads .git directly rather than honouring GIT_DIR — resolve
	// the correct container-internal path (#492).
	// GIT_COMMON_DIR breaks ref lookup in the git version used in the container
	// image and is therefore also omitted.

	return vars
}

// redactArgs returns a copy of args where any value immediately following a
// "--env" element has its value (the part after the first "=") replaced with
// "***". This prevents API keys and tokens from appearing in log output.
// The original slice is not modified.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, arg := range out {
		if arg == "--env" && i+1 < len(out) {
			kv := out[i+1]
			if eq := strings.IndexByte(kv, '='); eq >= 0 {
				out[i+1] = kv[:eq] + "=***"
			}
		}
	}
	return out
}

// IsNoSuchContainerError returns true when the podman output indicates the
// container does not exist (so the error can be silently ignored).
func IsNoSuchContainerError(output string) bool {
	return strings.Contains(output, "no such container") ||
		strings.Contains(output, "No such container") ||
		strings.Contains(output, "Error: no such container") ||
		strings.Contains(output, "Error response from daemon: No such container")
}

// CheckAvailability verifies that:
//  1. podman is on PATH,
//  2. the podman socket is reachable (podman info succeeds), and
//  3. the localhost/prism-agent:latest image is loaded.
//
// Returns a descriptive error for the first failing check, including a hint to
// use --host-mode as an escape hatch. Returns nil when all checks pass.
func CheckAvailability() error {
	// 1. Podman binary on PATH.
	if _, err := exec.LookPath("podman"); err != nil {
		return fmt.Errorf(
			"container mode requires podman but it was not found on PATH\n" +
				"hint: use --host-mode to run opencode directly without a container",
		)
	}

	// 2. Podman socket / daemon reachable.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	infoCmd := exec.CommandContext(ctx, "podman", "info", "--format", "json")
	if out, err := infoCmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		return fmt.Errorf(
			"container mode requires the podman socket to be running but it is not reachable: %s\n"+
				"hint: use --host-mode to run opencode directly without a container",
			msg,
		)
	}

	// 3. localhost/prism-agent:latest image loaded.
	imagesCtx, imagesCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer imagesCancel()
	imagesCmd := exec.CommandContext(imagesCtx, "podman", "images", "--quiet", Image)
	out, err := imagesCmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		return fmt.Errorf(
			"container mode requires the %q image but the check failed: %s\n"+
				"hint: use --host-mode to run opencode directly without a container",
			Image, msg,
		)
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf(
			"container mode requires the %q image but it is not loaded\n"+
				"hint: use --host-mode to run opencode directly without a container",
			Image,
		)
	}

	return nil
}
