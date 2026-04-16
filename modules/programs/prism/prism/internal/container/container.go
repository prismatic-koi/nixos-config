// Package container manages the podman container lifecycle for prism sidecar.
//
// The sidecar (running on the host) creates a podman container running
// "opencode serve --port 4096", health-checks it until the HTTP endpoint
// responds, and stops/removes the container on shutdown.
//
// Health check: we probe GET /global/health (not GET /) because the root URL
// falls through to opencode's UIRoutes catch-all, which proxies to
// https://app.opencode.ai/ when there is no embedded web UI — adding a 3–4 s
// network round-trip on every container startup. /global/health is in
// ControlPlaneRoutes and returns immediately with no external I/O.
//
// Design notes:
//   - All podman operations use exec.Command("podman", ...) — no daemon or
//     socket is required from Go's perspective, just a podman binary on PATH.
//   - The container name is derived from the prism session name so it is
//     predictable and idempotent.
//   - Credentials are injected as environment variables, never as mounted files.
//   - The opencode serve container port (ContainerPort) is bound to 127.0.0.1
//     only — not 0.0.0.0. The host-API TCP listener (Darwin only) intentionally
//     binds 0.0.0.0 so the gvproxy bridge interface can reach it from the VM.
package container

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// ContainerPort is the port opencode serve listens on inside the container.
	ContainerPort = 4096

	// Image is the container image name used for all agent containers.
	// The image is published to GHCR as a multi-arch image by CI and pulled
	// on first use by the systemd/launchd service. podman resolves the correct
	// arch (amd64/arm64) automatically from the multi-arch manifest.
	Image = "ghcr.io/prismatic-koi/prism-agent:latest"

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

	// ConfigContent is the JSON blob for the container's opencode.json config
	// file. When non-empty, it is written to a temp file and bind-mounted into
	// the container at /root/.config/opencode/opencode.json so that opencode
	// serve picks up the correct model, variant, and plugin overrides at runtime.
	//
	// Using a mounted config file (rather than the OPENCODE_CONFIG_CONTENT env
	// var) allows plugin paths to be specified as relative paths (e.g.
	// "./plugins/my-plugin") that resolve correctly relative to the config
	// file's location inside the container.
	ConfigContent string

	// PluginHostPath is retained for compatibility but is no longer used by
	// the container — the entire plugins/ directory is now mounted read-only
	// via the config allowlist in buildRunArgs.
	PluginHostPath string

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
	// When non-empty and HostAPITCPPort is zero, the socket's parent directory is
	// bind-mounted into the container at /var/run/prism-host and PRISM_HOST_API is set
	// to unix:///var/run/prism-host/<sockfilename> so that prism CLI commands inside
	// the container can proxy tmux operations to the host sidecar. On Darwin this field
	// is still set but HostAPITCPPort takes precedence over the Unix socket.
	HostAPISockPath string

	// HostAPITCPPort is the host-side TCP port allocated by the sidecar for the
	// host-API TCP listener (Darwin only). When non-zero, buildRunArgs sets
	// PRISM_HOST_API=http://host.containers.internal:<HostAPITCPPort> so the
	// container reaches the sidecar via the gvproxy bridge — no --publish flag
	// is needed. On Linux this field is zero and the Unix socket is used.
	HostAPITCPPort int

	// InstanceID is the UUID instance identifier for the prism session that owns
	// this container. When non-empty it is written as a
	// "prism.instance-id=<uuid>" label on the container so that EnsureRemoved
	// can verify ownership before killing it.
	InstanceID string

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
	// claudeCredentialsReady is true when writeClaudeCredentials successfully
	// extracted Claude credentials from the macOS Keychain and wrote them to
	// a temp file. buildRunArgs uses this to gate the bind-mount so that
	// podman is never given a source path that doesn't exist on disk.
	// This field is only ever true on Darwin.
	claudeCredentialsReady bool
}

// New creates a Manager for the given config. It does not start the container.
func New(cfg Config) *Manager {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Manager{
		cfg:  cfg,
		name: containerName(cfg.SessionName),
		// Use /global/health rather than / — the root URL falls through to
		// UIRoutes which proxies to https://app.opencode.ai/ when there is no
		// embedded web UI, adding a 3–4 s network round-trip to every startup.
		// /global/health is in ControlPlaneRoutes and returns immediately with
		// no MCP initialisation, plugin loading, or external network calls.
		healthCheckURL: fmt.Sprintf("http://127.0.0.1:%d/global/health", cfg.AllocatedPort),
		httpClient:     httpClient,
	}
}

// Name returns the container name.
func (m *Manager) Name() string { return m.name }

// EnsureRemoved stops and removes any existing container with the same name,
// and cleans up any temp files created by Create. It is safe to call when no
// such container exists — errors for "no such container" are silently ignored.
//
// When the Manager was created with a non-empty Config.InstanceID, EnsureRemoved
// first inspects the container's "prism.instance-id" label. If the label exists
// and does not match the current InstanceID, a warning is logged indicating
// that the container belongs to a different session instance. Removal still
// proceeds regardless — the new session needs the container name.
func (m *Manager) EnsureRemoved(ctx context.Context) {
	// Clean up temp files created by Create.
	_ = os.Remove(m.gitdirFilePath())
	_ = os.Remove(m.worktreeGitdirFilePath())
	_ = os.Remove(m.sshConfigFilePath())
	_ = os.Remove(m.gitconfigFilePath())
	_ = os.Remove(m.allowedSignersFilePath())
	_ = os.Remove(m.opencodeConfigFilePath())
	_ = os.Remove(m.claudeCredentialsFilePath())

	// Check the container's instance label when we have our own InstanceID.
	// This detects ownership mismatches where a container from a previous
	// session incarnation is being cleaned up by a new one.
	if m.cfg.InstanceID != "" {
		inspectCtx, inspectCancel := context.WithTimeout(ctx, 5*time.Second)
		out, inspectErr := exec.CommandContext(inspectCtx, "podman", "inspect",
			"--format", `{{index .Config.Labels "prism.instance-id"}}`,
			m.name,
		).Output()
		inspectCancel()
		if inspectErr == nil {
			containerInstanceID := strings.TrimSpace(string(out))
			if containerInstanceID != "" && containerInstanceID != m.cfg.InstanceID {
				log.Printf("container: warning: container %q has instance-id %q but current session has %q — removing anyway",
					m.name, containerInstanceID, m.cfg.InstanceID)
			}
		}
	}

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

// opencodeConfigFilePath returns the host path for the temporary opencode.json
// config file written before container start. The file is mounted read-only
// at /root/.config/opencode/opencode.json inside the container so that plugin
// paths (e.g. "./plugins/my-plugin") resolve correctly relative to the config
// file location.
func (m *Manager) opencodeConfigFilePath() string {
	return filepath.Join(os.TempDir(), "prism-opencode-config-"+m.name)
}

// claudeCredentialsFilePath returns the host path for the temporary Claude
// credentials file written before container start (Darwin only). On Darwin,
// Claude Code stores OAuth credentials in the macOS Keychain rather than
// ~/.claude/.credentials.json. We extract the token and write it to a temp
// file so it can be bind-mounted at /root/.claude/.credentials.json inside
// the container where opencode-claude-auth can read it.
func (m *Manager) claudeCredentialsFilePath() string {
	return filepath.Join(os.TempDir(), "prism-claude-creds-"+m.name)
}

// writeClaudeCredentials extracts Claude Code credentials from the macOS
// Keychain and writes them to a temp file. On Linux, Claude Code stores
// credentials in ~/.claude/.credentials.json which is already inside the
// claudeMount bind-mounted directory and requires no special handling. On
// Darwin, credentials are stored in the Keychain under the service name
// "Claude Code-credentials" and never reach disk, so the container never
// sees them via the directory mount alone.
//
// Sets m.claudeCredentialsReady to true on success so that buildRunArgs can
// add the bind-mount. Logs and returns without error on failure — a missing
// Keychain entry (e.g. not logged in) should surface as an auth error from
// opencode rather than a hard container launch failure.
func (m *Manager) writeClaudeCredentials() {
	m.claudeCredentialsReady = false
	if runtime.GOOS != "darwin" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "security", "find-generic-password",
		"-l", "Claude Code-credentials", "-w").Output()
	if err != nil {
		log.Printf("container: could not extract Claude credentials from macOS Keychain: %v — opencode-claude-auth may fail to authenticate", err)
		return
	}
	creds := strings.TrimSpace(string(out))
	if creds == "" {
		log.Printf("container: macOS Keychain returned empty Claude credentials — run `claude login` to authenticate")
		return
	}
	if err := os.WriteFile(m.claudeCredentialsFilePath(), []byte(creds), 0o600); err != nil {
		log.Printf("container: failed to write Claude credentials temp file: %v", err)
		return
	}
	m.claudeCredentialsReady = true
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
	createStart := time.Now()

	// Remove any stale container first (AC-15).
	m.EnsureRemoved(ctx)
	log.Printf("[timing] EnsureRemoved: %s", time.Since(createStart).Round(time.Millisecond))

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
	t0 := time.Now()
	if err := m.writeGitconfig(); err != nil {
		return fmt.Errorf("container: write gitconfig: %w", err)
	}
	log.Printf("[timing] writeGitconfig: %s", time.Since(t0).Round(time.Millisecond))

	// Write the opencode config file for the container. This replaces the
	// previous OPENCODE_CONFIG_CONTENT env var approach so that relative plugin
	// paths (e.g. "./plugins/my-plugin") resolve correctly from the config
	// file's location at /root/.config/opencode/opencode.json.
	if m.cfg.ConfigContent != "" {
		if err := os.WriteFile(m.opencodeConfigFilePath(), []byte(m.cfg.ConfigContent), 0o644); err != nil {
			return fmt.Errorf("container: write opencode config: %w", err)
		}
	}

	// On Darwin, extract Claude Code credentials from the macOS Keychain and
	// write them to a temp file so the container can find them. On Linux,
	// ~/.claude/.credentials.json is already inside the claudeMount directory.
	m.writeClaudeCredentials()

	// Build the podman run arguments.
	t0 = time.Now()
	args := m.buildRunArgs()
	log.Printf("[timing] buildRunArgs: %s", time.Since(t0).Round(time.Millisecond))

	log.Printf("container: creating %q: podman %s", m.name, strings.Join(redactArgs(args), " "))

	cmd := exec.CommandContext(ctx, "podman", args...)
	cmd.Stdout = os.Stderr // forward container stdout to sidecar's stderr log
	cmd.Stderr = os.Stderr
	podmanStart := time.Now()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container: podman run %q: %w", m.name, err)
	}
	log.Printf("[timing] podman run: %s (total to here: %s)",
		time.Since(podmanStart).Round(time.Millisecond),
		time.Since(createStart).Round(time.Millisecond))
	log.Printf("[timing] Create total: %s", time.Since(createStart).Round(time.Millisecond))

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

	waitStart := time.Now()
	probeCount := 0
	deadline := waitStart.Add(timeout)
	for {
		if time.Now().After(deadline) {
			m.dumpLogs()
			return fmt.Errorf("container: health check timed out after %s for %q", timeout, m.name)
		}
		if ctx.Err() != nil {
			m.dumpLogs()
			return ctx.Err()
		}

		probeCount++
		healthy := m.isHealthy(ctx)
		elapsed := time.Since(waitStart).Round(time.Millisecond)
		if healthy {
			log.Printf("[timing] WaitHealthy probe %d: %s elapsed — ok", probeCount, elapsed)
			log.Printf("[timing] WaitHealthy: %s (%d probes)", time.Since(waitStart).Round(time.Millisecond), probeCount)
			return nil
		}
		log.Printf("[timing] WaitHealthy probe %d: %s elapsed — fail", probeCount, elapsed)

		// Check if the container exited early — no point polling a dead
		// container until the timeout expires.
		if exited, code := m.hasExited(); exited {
			m.dumpLogs()
			return fmt.Errorf("container: %q exited with code %d before becoming healthy", m.name, code)
		}

		select {
		case <-ctx.Done():
			m.dumpLogs()
			return ctx.Err()
		case <-time.After(healthCheckInterval):
		}
	}
}

// dumpLogs writes the container's stdout/stderr to the sidecar log so that
// startup failures are visible without needing to race `podman logs` before
// the container is removed by Shutdown.
func (m *Manager) dumpLogs() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "logs", m.name).CombinedOutput()
	if err != nil {
		log.Printf("container: could not fetch logs for %q: %v", m.name, err)
		return
	}
	log.Printf("container: logs for %q:\n%s", m.name, string(out))
}

// hasExited checks whether the container has already stopped. Returns true and
// the exit code if the container is in an exited state.
func (m *Manager) hasExited() (bool, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", m.name).Output()
	if err != nil {
		return false, 0
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 2 && fields[0] == "exited" {
		code := 0
		fmt.Sscanf(fields[1], "%d", &code)
		return true, code
	}
	return false, 0
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
	_ = os.Remove(m.opencodeConfigFilePath())
	_ = os.Remove(m.claudeCredentialsFilePath())

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
	// opencodeConfigDir is the host's opencode config directory, mounted
	// item-by-item so that agents/ files, skills/, etc. are available inside
	// the container. opencode.json is NOT mounted from the host — the container
	// gets its own generated opencode.json via the ConfigContent temp file.
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

	// AWS — individual files mounted at /root/.aws (the default location the
	// AWS CLI looks in) so `aws` works inside the container without any env var
	// configuration. Only least-privilege files are mounted:
	//
	//   ~/.config/aws/readonly-config  → /root/.aws/config      (profiles/settings)
	//   ~/.config/aws/credentials      → /root/.aws/credentials  (static creds, if present)
	//   ~/.aws/sso                     → /root/.aws/sso          (SSO token cache from `aws sso login`)
	//   ~/.aws/cli                     → /root/.aws/cli          (CLI session cache)
	//
	// The config and credentials files live in the XDG location (managed by
	// sops-nix). The sso/ and cli/ cache dirs are always written to ~/.aws/ by
	// the AWS CLI regardless of AWS_CONFIG_FILE, so they are sourced from there.
	// Admin credentials (the full config, credentials with write-capable roles)
	// are never mounted — only the readonly-config is exposed.
	awsReadonlyConfig := filepath.Join(home, ".config", "aws", "readonly-config")
	awsCredentials := filepath.Join(home, ".config", "aws", "credentials")
	awsSSOCacheDir := filepath.Join(home, ".aws", "sso")
	awsCLICacheDir := filepath.Join(home, ".aws", "cli")
	// Kube agents config — the host stores this at ~/.config/kube/agents-config
	// (XDG-compliant, managed by sops-nix). Mount it at /root/.kube/config (the
	// default path kubectl reads) so `kubectl` works inside the container without
	// any env var configuration. Only the agents/readonly kubeconfig is mounted —
	// the admin kubeconfig is never exposed to agents.
	kubeAgentsConfig := filepath.Join(home, ".config", "kube", "agents-config")

	args := []string{
		"run",
		"--detach",
		"--name", m.name,
	}

	// Tag the container with the session instance ID when available. This
	// allows EnsureRemoved to detect ownership mismatches (a container that
	// belongs to a previous incarnation of the session being cleaned up by a
	// new one). The label is informational — it does not gate removal.
	if cfg.InstanceID != "" {
		args = append(args, "--label", "prism.instance-id="+cfg.InstanceID)
	}

	args = append(args,
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
		// Claude credentials directory — read-write for auth plugin token refresh.
		// On Linux, ~/.claude/.credentials.json lives here.
		// On Darwin, .credentials.json is absent (credentials are in the macOS
		// Keychain); writeClaudeCredentials() extracts and writes it to a temp
		// file that is bind-mounted below at /root/.claude/.credentials.json.
		"--volume", claudeMount,
		// Nix eval cache — flake input tarballs pre-unpacked from the host.
		"--volume", nixCacheMount,
	)

	// AWS: mount individual files/dirs into /root/.aws so the AWS CLI works at
	// its default paths with no env var configuration. Each mount is conditional
	// on the source path existing — hosts without AWS configured are unaffected.
	if resolved, err := filepath.EvalSymlinks(awsReadonlyConfig); err == nil {
		args = append(args, "--volume", resolved+":/root/.aws/config:ro")
	}
	if resolved, err := filepath.EvalSymlinks(awsCredentials); err == nil {
		args = append(args, "--volume", resolved+":/root/.aws/credentials:ro")
	}
	// SSO and CLI cache dirs — written to ~/.aws/ by the AWS CLI regardless of
	// AWS_CONFIG_FILE. Mount read-only so the container can use SSO tokens
	// obtained on the host via `aws sso login` without needing network access
	// to re-authenticate. Mounted as directories (conditional on existence).
	if _, err := os.Stat(awsSSOCacheDir); err == nil {
		args = append(args, "--volume", awsSSOCacheDir+":/root/.aws/sso:ro")
	}
	if _, err := os.Stat(awsCLICacheDir); err == nil {
		args = append(args, "--volume", awsCLICacheDir+":/root/.aws/cli:ro")
	}

	// Kube agents config: mount ~/.config/kube/agents-config at
	// /root/.kube/config (the default file kubectl reads) when the file exists
	// on the host. Only the agents/readonly kubeconfig is exposed — the admin
	// kubeconfig is never mounted into agent containers.
	if resolved, err := filepath.EvalSymlinks(kubeAgentsConfig); err == nil {
		args = append(args, "--volume", resolved+":/root/.kube/config:ro")
	}

	// Darwin: bind-mount the extracted Keychain credentials over
	// /root/.claude/.credentials.json so opencode-claude-auth can find them.
	// On Linux this file already exists inside the claudeMount directory.
	if m.claudeCredentialsReady {
		args = append(args,
			"--volume", m.claudeCredentialsFilePath()+":/root/.claude/.credentials.json:ro",
		)
	}

	args = append(args,
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
		//
		// Mount the parent directory rather than the socket file itself.
		// On Darwin, podman runs inside a VM with virtiofs host shares;
		// statfs on a Unix socket through virtiofs returns ENOTSUP, causing
		// podman to reject the mount. Mounting the directory avoids the
		// socket-level statfs and lets the container reach the socket via
		// the live directory mount — the same pattern used for the host-API
		// socket (see #611 / #612).
		"--volume", "/nix/var/nix/daemon-socket:/nix/var/nix/daemon-socket",
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
	)

	// Host-API: tell the container how to reach the host-API server.
	//
	// On Darwin (cfg.HostAPITCPPort != 0): TCP is used because virtiofs returns
	// ENOTSUP on connect() for Unix sockets mounted into the VM. The sidecar
	// binds a TCP listener on 0.0.0.0:<port> before container creation and passes
	// the port here. The container reaches the sidecar via host.containers.internal
	// (192.168.127.254, the gvproxy bridge IP) — no --publish flag is needed
	// because this is container→host outbound traffic, not inbound port forwarding.
	//
	// On Linux (cfg.HostAPITCPPort == 0): the Unix socket directory is mounted
	// and PRISM_HOST_API is set to the unix:// path (existing behaviour).
	if cfg.HostAPITCPPort != 0 {
		args = append(args,
			"--env", fmt.Sprintf("PRISM_HOST_API=http://host.containers.internal:%d", cfg.HostAPITCPPort),
		)
	} else if cfg.HostAPISockPath != "" {
		args = append(args,
			"--volume", filepath.Dir(cfg.HostAPISockPath)+":/var/run/prism-host:Z",
			"--env", "PRISM_HOST_API=unix:///var/run/prism-host/"+filepath.Base(cfg.HostAPISockPath),
		)
	}

	// Mount an explicit allowlist of host opencode config entries into
	// /root/.config/opencode inside the container.
	//
	// An allowlist (not a denylist) is used deliberately: the bun runtime
	// inside the container writes to package.json and related files at
	// request time, so mounting those read-only causes EROFS errors and
	// HTTP 500 responses that break the health check. Only mount what the
	// container actually needs.
	//
	// Excluded intentionally:
	//   - opencode.json  — mounted separately from the ConfigContent temp file
	//                      (not from the host's opencode.json) so that the
	//                      container gets the role-specific config with correct
	//                      relative plugin paths
	//   - package.json, bun.lock, package-lock.json, node_modules/ — bun
	//                      ecosystem files the container manages itself
	//
	// For each entry that exists on the host:
	//   - Symlinks → resolve to real Nix store path and mount that
	//   - Directories → use --mount type=bind (podman creates dest automatically)
	//   - Regular files → use --volume
	configAllowlist := []string{
		"AGENTS.md",
		"agents",
		"plugins",
		"skills",
		"command",
		"tui.json",
		".gitignore",
		"mcp-atlassian-slim-proxy.mjs",
	}
	for _, name := range configAllowlist {
		hostPath := filepath.Join(opencodeConfigDir, name)
		resolved, err := filepath.EvalSymlinks(hostPath)
		if err != nil {
			// Entry does not exist or symlink is dangling — skip silently.
			continue
		}
		containerPath := "/root/.config/opencode/" + name
		if fi, err := os.Stat(resolved); err == nil && fi.IsDir() {
			args = append(args, "--mount",
				"type=bind,src="+resolved+",dst="+containerPath+",ro")
		} else {
			args = append(args, "--volume", resolved+":"+containerPath+":ro")
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

	// Inject credentials as environment variables (AC-10, AC-11).
	for _, kv := range m.credentialEnvVars() {
		args = append(args, "--env", kv)
	}

	// opencode.json: mount the generated config file when ConfigContent is set.
	// The file is written by Create() to a temp path and mounted read-only at
	// /root/.config/opencode/opencode.json. This replaces the previous
	// OPENCODE_CONFIG_CONTENT env var so that relative plugin paths (e.g.
	// "./plugins/my-plugin") resolve correctly from the config file's directory.
	if cfg.ConfigContent != "" {
		args = append(args, "--volume", m.opencodeConfigFilePath()+":/root/.config/opencode/opencode.json:ro")
	}

	// Image and command: opencode serve on the container port.
	args = append(args, Image,
		"opencode", "serve",
		"--port", fmt.Sprintf("%d", ContainerPort),
		"--hostname", "0.0.0.0",
	)

	return args
}

// githubAccountFromBareRoot returns the GitHub account (organisation or user)
// for the repo by reading the origin remote URL from the bare git dir.
// Returns "" when the bare root is empty, git is unavailable, or the remote
// URL does not match a github.com URL pattern.
//
// Supported URL formats:
//
//	git@github.com:<account>/<repo>.git   (SSH)
//	https://github.com/<account>/<repo>   (HTTPS, with or without .git)
func githubAccountFromBareRoot(bareRoot string) string {
	if bareRoot == "" {
		return ""
	}
	// The bare git dir lives at <bareRoot>/.bare — use --git-dir to run git
	// against it directly without needing to be inside a worktree.
	bareDir := filepath.Join(bareRoot, ".bare")
	cmd := exec.Command("git", "--git-dir", bareDir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		// Also try the bareRoot itself in case it IS the raw bare git dir.
		cmd2 := exec.Command("git", "--git-dir", bareRoot, "remote", "get-url", "origin")
		out, err = cmd2.Output()
		if err != nil {
			return ""
		}
	}
	return githubAccountFromURL(strings.TrimSpace(string(out)))
}

// githubAccountFromURL parses a git remote URL and returns the GitHub account
// (the path segment immediately after "github.com"). Returns "" if the URL is
// not a recognisable github.com URL.
func githubAccountFromURL(remoteURL string) string {
	// SSH: git@github.com:<account>/<repo>[.git]
	if strings.HasPrefix(remoteURL, "git@github.com:") {
		rest := strings.TrimPrefix(remoteURL, "git@github.com:")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) >= 1 && parts[0] != "" {
			return parts[0]
		}
	}
	// HTTPS: https://github.com/<account>/<repo>[.git]
	//        https://x-access-token:TOKEN@github.com/<account>/<repo>[.git]
	if idx := strings.Index(remoteURL, "github.com/"); idx >= 0 {
		rest := remoteURL[idx+len("github.com/"):]
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) >= 1 && parts[0] != "" {
			return parts[0]
		}
	}
	return ""
}

// credentialEnvVars returns the environment variable assignments to inject into
// the container based on the agent role and current host environment.
// Only vars that are set on the host are forwarded — unset vars are skipped.
//
// GitHub token selection (4-PAT architecture):
// The correct token is chosen based on the GitHub account (derived from the
// repo's origin remote URL) and the agent role:
//
//	PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR   — prismatic-koi + coordinator
//	PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER        — prismatic-koi + worker
//	PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_COORDINATOR — thankyou-payroll + coordinator
//	PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER      — thankyou-payroll + worker
//
// Falls back to host GITHUB_TOKEN if the specific token is not set
// (supports host-mode, --host-mode spawns, and migration period).
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

	// GitHub token — 4-PAT architecture: account × role → specific token.
	// Derive the account from the repo's git remote URL.
	account := githubAccountFromBareRoot(m.cfg.BareRoot)
	role := m.cfg.AgentRole

	// Build the env var name: PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE>
	// where account is uppercased with hyphens replaced by underscores.
	var tokenEnvVar string
	if account != "" {
		accountKey := strings.ToUpper(strings.ReplaceAll(account, "-", "_"))
		roleKey := strings.ToUpper(role)
		if roleKey == "WORKER" || roleKey == "COORDINATOR" {
			tokenEnvVar = "PRISM_GITHUB_TOKEN_" + accountKey + "_" + roleKey
		}
	}

	// Try specific token first, then fall back to host GITHUB_TOKEN.
	if tokenEnvVar != "" {
		if tok := os.Getenv(tokenEnvVar); tok != "" {
			vars = append(vars, "GITHUB_TOKEN="+tok)
			return vars
		}
	}
	// Fallback: use host GITHUB_TOKEN (supports --host-mode and migration period).
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		vars = append(vars, "GITHUB_TOKEN="+tok)
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
//  3. the ghcr.io/prismatic-koi/prism-agent:latest image is present locally.
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

	// 3. ghcr.io/prismatic-koi/prism-agent:latest image present locally.
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
