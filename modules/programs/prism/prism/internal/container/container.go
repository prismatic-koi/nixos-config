// Package container manages the podman container lifecycle for prism sidecar.
//
// The sidecar (running on the host) creates a podman container running
// "opencode --port 4096 --hostname 0.0.0.0" (combined TUI + HTTP mode),
// health-checks it until the HTTP endpoint responds, and stops/removes the
// container on shutdown. The tmux agent window attaches to the container's
// PTY via "podman attach" so the opencode TUI is visible to the user.
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

// isolationMode identifies which sandbox layer will consume the temp files
// (gitconfig, ssh config) that writeGitconfig / writeSshConfig generate.
//
// The two sandboxes mount the SSH artefacts at different in-sandbox paths:
//
//   - podman runs as root, so canonical paths are /root/.ssh/{access-key,
//     signing-key,signing-key.pub,allowed_signers} — the agent's $HOME is /root.
//   - bwrap runs as the host user, so canonical paths are
//     $HOME/.ssh/{access-key,signing-key,signing-key.pub,allowed_signers}
//     where $HOME is the host user's home directory.
//
// Agents inside both sandboxes see the same generic filenames (access-key,
// signing-key, …); only the $HOME prefix differs. The isolation mode lets the
// generators substitute the correct prefix into the config files they write.
type isolationMode int

const (
	isolationPodman isolationMode = iota
	isolationBwrap
)

// sandboxHome returns the in-sandbox $HOME directory for the given isolation
// mode. For podman this is always /root (the image's user). For bwrap this is
// the host user's home directory, because bwrap shares the host user namespace
// and does not switch to root.
func sandboxHome(mode isolationMode) string {
	switch mode {
	case isolationBwrap:
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = os.Getenv("HOME")
		}
		return home
	default:
		return "/root"
	}
}

// Config holds the parameters for creating and managing a container.
type Config struct {
	// SessionName is the prism session name (e.g. "nixos-config@feature").
	// Used to derive a stable container name.
	SessionName string

	// Worktree is the absolute path to the git worktree on the host.
	// Mounted at /workspace inside the container. When WorktreeReadOnly is
	// true, the mount is read-only; otherwise it is read-write.
	Worktree string

	// WorktreeReadOnly, when true, mounts the worktree read-only inside
	// the container. Used by review agent containers so agents cannot
	// modify the branch under review.
	WorktreeReadOnly bool

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
	// to unix:///var/run/prism-host/<sockfilename>. The socket path uses a per-session
	// subdirectory (run/<sessionName>/hostapi.sock) so that each container sees only
	// its own session's socket directory — not the shared run/ directory containing all
	// sessions' sockets (security fix #960). On Darwin this field is still set but
	// HostAPITCPPort takes precedence over the Unix socket.
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

	// InitialPrompt is the initial prompt to deliver to the agent at startup.
	// When non-empty, it is appended to the opencode command as
	// --agent <AgentRole> --prompt <text> so that opencode starts the session
	// with the prompt already in flight, visible in the TUI from the start.
	// This replaces the previous POST /session + prompt_async HTTP delivery
	// which created a second session invisible to the TUI (RFC #691 Phase 1a).
	InitialPrompt string

	// MemoryMax is the value for podman run --memory (e.g. "8g").
	// When empty, no --memory flag is emitted. This preserves existing
	// behaviour for callers not using the nix module.
	MemoryMax string

	// MemorySwapMax is the value for podman run --memory-swap (e.g. "8g").
	// When empty, no --memory-swap flag is emitted.
	MemorySwapMax string

	// PidsLimit is the value for podman run --pids-limit.
	// When zero, no --pids-limit flag is emitted.
	PidsLimit int

	// RuntimeEnv holds harness-specific environment variables to inject
	// into the container. Each entry is emitted as --env KEY=VALUE (podman)
	// or --setenv KEY VALUE (bwrap). Populated from
	// harness.Harness.RuntimeEnv() by the sidecar at container creation
	// time. When nil, no harness-specific env vars are injected.
	RuntimeEnv map[string]string

	// AgentEnvVars holds the profile-level environment variables to inject
	// into the agent shell. Each entry is emitted as --env KEY=VALUE (podman)
	// or --setenv KEY VALUE (bwrap). Sourced from profiles.json agent_env_vars
	// (written by Nix). These carry entries such as GIT_EDITOR=true,
	// KUBECONFIG, and AWS_CONFIG_FILE into the sandboxed agent.
	// When nil or empty, no profile env vars are injected.
	AgentEnvVars map[string]string
}

// NameForSession returns the stable podman container name for a session.
// The name is derived from the session name with "@", "/", ".", and "~"
// replaced by "-" and a "prism-" prefix, e.g. "prism-nixos-config-feature".
//
// The "~" replacement is needed for review agent session names which are
// structured as "<parent>~review-<N>~<agentName>" — without it, podman would
// reject the container name (allowed charset: [a-zA-Z0-9][a-zA-Z0-9_.-]*).
func NameForSession(sessionName string) string {
	safe := strings.ReplaceAll(sessionName, "@", "-")
	safe = strings.ReplaceAll(safe, "/", "-")
	safe = strings.ReplaceAll(safe, ".", "-")
	safe = strings.ReplaceAll(safe, "~", "-")
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
	// isolator is the Isolator implementation used to run, shut down, and
	// inspect the container. It is set unconditionally to a podmanIsolator in
	// New() and can be replaced in tests.
	isolator Isolator
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
	name := containerName(cfg.SessionName)
	return &Manager{
		cfg:  cfg,
		name: name,
		// Use /global/health rather than / — the root URL falls through to
		// UIRoutes which proxies to https://app.opencode.ai/ when there is no
		// embedded web UI, adding a 3–4 s network round-trip to every startup.
		// /global/health is in ControlPlaneRoutes and returns immediately with
		// no MCP initialisation, plugin loading, or external network calls.
		healthCheckURL: fmt.Sprintf("http://127.0.0.1:%d/global/health", cfg.AllocatedPort),
		httpClient:     httpClient,
		isolator:       newPodmanIsolator(name),
	}
}

// Name returns the container name.
func (m *Manager) Name() string { return m.name }

// sessionTempPath is the package-level building block for per-session temp
// file paths. All per-session artefact files follow the shape:
//
//	<os.TempDir()>/prism-<stem>-<session_name><suffix>
//
// stem identifies the artefact (e.g. "gitdir", "ssh-config"); suffix is ""
// for most artefacts and ".sb" for the sandbox-exec SBPL profile.
//
// It is a free function so that exported helpers like OpencodeConfigFilePath
// can share the same path logic without requiring a Manager receiver.
func sessionTempPath(stem, suffix, name string) string {
	return filepath.Join(os.TempDir(), "prism-"+stem+"-"+name+suffix)
}

// tempPath returns the host path for a temporary per-session artefact file.
// It is a thin wrapper over sessionTempPath using the Manager's session name.
func (m *Manager) tempPath(stem, suffix string) string {
	return sessionTempPath(stem, suffix, m.name)
}

// gitdirFilePath returns the host path for the temporary corrected .git pointer
// file written before container start. The file is named after the container
// so it is stable and can be cleaned up by EnsureRemoved.
func (m *Manager) gitdirFilePath() string {
	return m.tempPath("gitdir", "")
}

// GitdirFilePath is the exported version of gitdirFilePath for tests.
func (m *Manager) GitdirFilePath() string { return m.gitdirFilePath() }

// sshConfigFilePath returns the host path for the temporary SSH config
// written before container start. The container needs a minimal SSH config
// for git push/fetch over SSH remotes, but the host's ~/.ssh/config is a
// nix store symlink with wrong ownership (nobody:nogroup, 0444) which SSH
// rejects. We write a simple config that points to the mounted key.
func (m *Manager) sshConfigFilePath() string {
	return m.tempPath("ssh-config", "")
}

// SshConfigFilePath is the exported version of sshConfigFilePath for tests.
func (m *Manager) SshConfigFilePath() string { return m.sshConfigFilePath() }

// gitconfigFilePath returns the host path for the temporary .gitconfig
// written before container start. The container needs a minimal gitconfig
// for commit identity and SSH signing. Mounted read-only at /root/.gitconfig.
func (m *Manager) gitconfigFilePath() string {
	return m.tempPath("gitconfig", "")
}

// GitconfigFilePath is the exported version of gitconfigFilePath for tests.
func (m *Manager) GitconfigFilePath() string { return m.gitconfigFilePath() }

// allowedSignersFilePath returns the host path for the temporary
// allowed_signers file written before container start. The file is mounted
// read-only at /root/.ssh/allowed_signers and is required for
// git verify-commit to work with SSH signing.
func (m *Manager) allowedSignersFilePath() string {
	return m.tempPath("allowed-signers", "")
}

// opencodeConfigFilePath returns the host path for the temporary opencode.json
// config file written before container start. The file is mounted read-only
// at /root/.config/opencode/opencode.json inside the container so that plugin
// paths (e.g. "./plugins/my-plugin") resolve correctly relative to the config
// file location.
func (m *Manager) opencodeConfigFilePath() string {
	return OpencodeConfigFilePath(m.name)
}

// OpencodeConfigFilePath returns the host path for the temporary opencode.json
// config file for the given session name. The path is deterministic and
// derived from the session name, so callers outside the Manager (e.g.
// cmd/spawn.go) can write the file before the Manager is constructed.
// Delegates to sessionTempPath so all per-session paths share one naming rule.
func OpencodeConfigFilePath(sessionName string) string {
	return sessionTempPath("opencode-config", "", sessionName)
}

// WriteOpencodeConfig writes content to the temp opencode.json file for the
// given session name. It creates or overwrites the file with mode 0o644.
// Returns a wrapped error on failure.
func WriteOpencodeConfig(sessionName, content string) error {
	path := OpencodeConfigFilePath(sessionName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("container: write opencode config for session %q: %w", sessionName, err)
	}
	return nil
}

// claudeCredentialsFilePath returns the host path for the temporary Claude
// credentials file written before container start (Darwin only). On Darwin,
// Claude Code stores OAuth credentials in the macOS Keychain rather than
// ~/.claude/.credentials.json. We extract the token and write it to a temp
// file so it can be bind-mounted at /root/.claude/.credentials.json inside
// the container where opencode-claude-auth can read it.
func (m *Manager) claudeCredentialsFilePath() string {
	return m.tempPath("claude-creds", "")
}

// writeSshConfig generates a minimal ~/.ssh/config for the sandbox and writes
// it to a temp file. The file is later mounted at $HOME/.ssh/config:ro inside
// the sandbox, where $HOME depends on the isolation mode (see isolationMode).
//
// The config only needs to handle GitHub; other SSH hosts are not expected
// inside agent sandboxes. The IdentityFile path is $HOME/.ssh/access-key
// using the sandbox-specific $HOME prefix, which matches the generic name
// used by both the podman volume mount (/root/.ssh/access-key) and the bwrap
// bind-mount (<hostHome>/.ssh/access-key).
func (m *Manager) writeSshConfig(mode isolationMode) error {
	identityFile := filepath.Join(sandboxHome(mode), ".ssh", "access-key")
	sshConfig := "Host github.com\n" +
		"  StrictHostKeyChecking accept-new\n" +
		"  IdentityFile " + identityFile + "\n" +
		"  IdentitiesOnly yes\n"
	if err := os.WriteFile(m.sshConfigFilePath(), []byte(sshConfig), 0o600); err != nil {
		return fmt.Errorf("container: write ssh config: %w", err)
	}
	return nil
}

// writeGitconfig generates a minimal .gitconfig for the container and writes
// it to a temp file. The file is later mounted at $HOME/.gitconfig:ro inside
// the sandbox, where $HOME depends on the isolation mode (see isolationMode).
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
//
// The mode argument controls the paths embedded in the generated file:
//
//   - isolationPodman: signingKey = /root/.ssh/signing-key.pub,
//     allowedSignersFile = /root/.ssh/allowed_signers (paths inside the
//     podman container, where the agent runs as root).
//   - isolationBwrap: signingKey = <hostHome>/.ssh/signing-key.pub,
//     allowedSignersFile = <hostHome>/.ssh/allowed_signers (paths inside
//     the bwrap sandbox, where the agent runs as the host user).
//
// Both sandboxes mount the same underlying host key files — only the $HOME
// prefix differs. Generic filenames (signing-key.pub, allowed_signers, …)
// are used in both cases so agents see a uniform layout regardless of mode.
func (m *Manager) writeGitconfig(mode isolationMode) error {
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

	// Canonical in-sandbox paths for the signing artefacts. Both paths live
	// at $HOME/.ssh/<generic-name> where $HOME depends on the isolation mode
	// (see sandboxHome). Agents inside the sandbox see generic filenames —
	// signing-key.pub / allowed_signers — regardless of which sandbox layer
	// they're running under.
	sandboxSshDir := filepath.Join(sandboxHome(mode), ".ssh")
	sandboxSigningKeyPub := filepath.Join(sandboxSshDir, "signing-key.pub")
	sandboxAllowedSigners := filepath.Join(sandboxSshDir, "allowed_signers")

	// [user] section — only when identity is present.
	if m.cfg.GitUserName != "" && m.cfg.GitUserEmail != "" {
		sb.WriteString("[user]\n")
		sb.WriteString("    name = " + m.cfg.GitUserName + "\n")
		sb.WriteString("    email = " + m.cfg.GitUserEmail + "\n")
		if hasSigning {
			sb.WriteString("    signingKey = " + sandboxSigningKeyPub + "\n")
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
			sb.WriteString("    allowedSignersFile = " + sandboxAllowedSigners + "\n")
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
	return m.tempPath("wt-gitdir", "")
}

// WorktreeGitdirFilePath is the exported version of worktreeGitdirFilePath for tests.
func (m *Manager) WorktreeGitdirFilePath() string { return m.worktreeGitdirFilePath() }

// PrepareBwrap writes the same temp files that Create() writes for podman
// sessions (SSH config, gitconfig, opencode.json config) and returns the
// complete bwrap argument list via bwrapIsolator.BuildArgs. It does NOT write
// the gitdir fixup files (prism-gitdir-*, prism-wt-gitdir-*) because bwrap
// uses Dst==Src mounts, so the host git paths are visible at their exact
// locations inside the sandbox without remapping.
//
// Call this from "prism agent-run" in the tmux pane for bwrap mode. The
// returned args slice is suitable for passing directly to exec.Exec("bwrap").
//
// Note: this method also calls prepareVolumeDirs() to ensure bind-mount
// sources exist. bwrap (unlike podman) does not emit an exit-125 error for
// missing sources — it simply fails to bind. Eagerly creating the directories
// avoids confusing "No such file or directory" errors at bwrap startup.
func (m *Manager) PrepareBwrap() ([]string, error) {
	// Write a minimal SSH config for bwrap. The bwrap SSH key mount remaps
	// the host signing/access keys to canonical generic paths under
	// $HOME/.ssh/ inside the sandbox (see bwrap.go), so the IdentityFile path
	// is $HOME/.ssh/access-key — same generic name as the podman path, just
	// rooted at the host user's $HOME instead of /root.
	if err := m.writeSshConfig(isolationBwrap); err != nil {
		return nil, fmt.Errorf("container: bwrap: %w", err)
	}

	// Write the gitconfig. Mirrors the Create() path, but with bwrap-mode
	// paths (signingKey and allowedSignersFile rooted at <hostHome>/.ssh/
	// rather than /root/.ssh/).
	if err := m.writeGitconfig(isolationBwrap); err != nil {
		return nil, fmt.Errorf("container: bwrap: write gitconfig: %w", err)
	}

	// Write the opencode config file, if provided.
	if m.cfg.ConfigContent != "" {
		if err := os.WriteFile(m.opencodeConfigFilePath(), []byte(m.cfg.ConfigContent), 0o644); err != nil {
			return nil, fmt.Errorf("container: bwrap: write opencode config: %w", err)
		}
	}

	// Pre-create directories that BuildArgs will reference as bind-mount
	// sources. bwrap silently fails rather than printing a clear error, so
	// we create eagerly here.
	if err := m.prepareVolumeDirs(false); err != nil {
		log.Printf("container: bwrap: prepareVolumeDirs partial failure: %v", err)
	}

	// Build the bwrap args.
	b := &bwrapIsolator{name: m.name}
	return b.BuildArgs(m), nil
}

// PrepareSandboxExec prepares the per-session staging HOME, writes the SBPL
// profile, and returns the complete sandbox-exec argument list.
//
// Steps:
//  1. PrepareSandboxExecHome() — create the staging HOME directory tree and
//     populate it with symlinks to the host credential, config, and cache
//     paths. On Darwin this also calls writeClaudeCredentials() so that the
//     Keychain-extracted credentials file is reachable at
//     $HOME/.claude/.credentials.json via the .claude symlink.
//  2. writeProfile() — generate the SBPL profile with staging HOME,
//     worktree, credential, and AWS deny rules, and write it to a temp file.
//  3. Build and return the sandbox-exec argument list.
//
// The returned args slice is suitable for passing directly to
// syscall.Exec("/usr/bin/sandbox-exec", args, env). The first element of
// args is "sandbox-exec" itself (argv[0] under syscall.Exec).
//
// The env passed to syscall.Exec should set HOME=<staging_home> so that
// opencode and its tools find credentials and config at their canonical paths
// inside the staging HOME. agent_run.go constructs that env after this call.
func (m *Manager) PrepareSandboxExec() ([]string, error) {
	// Populate the staging HOME with symlinks. This must happen before
	// generateProfile() so the profile generator can call
	// collectStagingHomeSymlinkTargets to emit (allow file-read* (literal ...))
	// rules for every resolved symlink target.
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		return nil, fmt.Errorf("container: sandbox-exec: prepare staging home: %w", err)
	}
	_ = stagingHome // consumed by generateProfile via m.sandboxExecHomePath()

	// On Darwin, extract Claude Code credentials from the macOS Keychain and
	// write them to a temp file. The temp file path (m.claudeCredentialsFilePath())
	// is reachable inside the sandbox at $HOME/.claude/.credentials.json via the
	// .claude symlink in the staging HOME (which points at host ~/.claude).
	// The writeClaudeCredentials helper sets m.claudeCredentialsReady on success.
	m.writeClaudeCredentials()

	if _, err := writeProfile(m); err != nil {
		return nil, err
	}

	s := &sandboxExecIsolator{name: m.name}
	return s.BuildArgs(m), nil
}

// prepareVolumeDirs eagerly creates host directories that buildRunArgs() will
// reference as bind-mount sources. podman exits 125 ("statfs: no such file or
// directory") if any bind-mount source is absent, so we create them here —
// before buildRunArgs() is called — so that buildRunArgs() itself remains a
// pure argument builder with no filesystem side-effects.
//
// Individual MkdirAll failures are logged and treated as non-fatal: if the
// directory is still absent when podman runs, podman will produce the real
// error. Returns a non-nil error only when multiple dirs fail.
//
// perSessionOpencode controls whether the per-session opencode state directory
// (~/.local/share/opencode/prism-sessions/<name>/) is created. The podman path
// requires it (Darwin virtiofs WAL-mode locking workaround); the bwrap path
// shares ~/.local/share/opencode/ directly and does not need a per-session dir.
func (m *Manager) prepareVolumeDirs(perSessionOpencode bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	var errs []string

	// Per-session opencode state directory (podman only).
	if perSessionOpencode {
		opencodeSessionDir := filepath.Join(home, ".local", "share", "opencode", "prism-sessions", m.name)
		if err := os.MkdirAll(opencodeSessionDir, 0o755); err != nil {
			log.Printf("container: failed to create per-session opencode state dir %q: %v", opencodeSessionDir, err)
			errs = append(errs, err.Error())
		}
	}

	// opencode plugin/model cache.
	opencodeCacheDir := filepath.Join(home, ".cache", "opencode")
	if err := os.MkdirAll(opencodeCacheDir, 0o755); err != nil {
		log.Printf("container: failed to create opencode cache dir %q: %v", opencodeCacheDir, err)
		errs = append(errs, err.Error())
	}

	// bun transpiler cache.
	bunCacheDir := filepath.Join(home, ".cache", "bun")
	if err := os.MkdirAll(bunCacheDir, 0o755); err != nil {
		log.Printf("container: failed to create bun cache dir %q: %v", bunCacheDir, err)
		errs = append(errs, err.Error())
	}

	// Clipboard staging directory: pre-create so that the bind-mount in
	// buildRunArgs() is always active, even on the first paste. Without this,
	// a first-ever paste on a fresh system would write the file host-side but
	// the container would not see it (the bind-mount only fires when the dir
	// exists at container spawn time). Creating it eagerly here ensures the
	// directory always exists before buildRunArgs() runs its os.Stat check.
	clipboardCacheDir := filepath.Join(home, ".cache", "prism", "clipboard")
	if err := os.MkdirAll(clipboardCacheDir, 0o755); err != nil {
		log.Printf("container: failed to create clipboard staging dir %q: %v", clipboardCacheDir, err)
		errs = append(errs, err.Error())
	}

	// Per-session host-API socket directory (both podman and bwrap, security fix #960).
	// Each session places its socket in its own subdirectory
	// (~/.local/state/prism/run/<sessionName>/hostapi.sock) instead of the
	// shared run/ directory. The directory must be pre-created here so it
	// exists before the sandboxed process starts:
	//   - podman: directory must pre-exist before "podman run" evaluates the
	//     bind-mount source; the sidecar creates the socket inside it later.
	//   - bwrap: directory must pre-exist before the sidecar calls
	//     net.Listen("unix", sockPath); bwrap is exec'd after.
	// Using 0o700 (owner-only) so other users on the host cannot list or
	// access this session's socket directory.
	if m.cfg.HostAPISockPath != "" {
		sockDir := filepath.Dir(m.cfg.HostAPISockPath)
		if err := os.MkdirAll(sockDir, 0o700); err != nil {
			log.Printf("container: failed to create host-API socket dir %q: %v", sockDir, err)
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 1 {
		return fmt.Errorf("%d directories could not be created", len(errs))
	}
	return nil
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
	// Worktree is mounted read-only for review agents (WorktreeReadOnly=true)
	// and read-write for worker/coordinator agents.
	var worktreeMount string
	if cfg.WorktreeReadOnly {
		worktreeMount = cfg.Worktree + ":/workspace:ro,Z"
	} else {
		worktreeMount = cfg.Worktree + ":/workspace:Z"
	}
	// Per-session opencode state directory: isolates each container's SQLite DB,
	// logs, snapshots, and storage so concurrent sessions cannot corrupt each
	// other's state (fixes the virtiofs WAL-mode locking issue on Darwin).
	// The container name is stable and already used for temp file naming, so it
	// gives us a unique, predictable key for the directory.
	// The directory is pre-created by prepareVolumeDirs() (called from Create()
	// before buildRunArgs()) so this is a pure path construction with no I/O.
	opencodeSessionDir := filepath.Join(home, ".local", "share", "opencode", "prism-sessions", m.name)
	opencodeStateMount := opencodeSessionDir + ":/root/.local/share/opencode:Z"

	// auth.json overlay: share the host's OAuth token file across all sessions
	// by bind-mounting it over the per-session dir's auth.json. Only added when
	// the file exists on the host — skipped silently when absent (e.g. after the
	// parent dir was deleted for DB recovery).
	// Mounted read-write (:Z): the opencode-claude-auth plugin calls
	// writeFileSync on auth.json on every load to sync active OAuth credentials.
	// A :ro mount causes EROFS and breaks Anthropic auth inside the container.
	opencodeAuthJSON := filepath.Join(home, ".local", "share", "opencode", "auth.json")

	// opencodeConfigDir is the host's opencode config directory, mounted
	// item-by-item so that agents/ files, skills/, etc. are available inside
	// the container. opencode.json is NOT mounted from the host — the container
	// gets its own generated opencode.json via the ConfigContent temp file.
	opencodeConfigDir := filepath.Join(home, ".config", "opencode")

	// opencode cache — mount the whole directory so plugins, models.json,
	// package.json, and bun.lock are all available without network access.
	// Pre-created by prepareVolumeDirs().
	opencodeCacheDir := filepath.Join(home, ".cache", "opencode")
	opencodeCacheMount := opencodeCacheDir + ":/root/.cache/opencode:ro"
	// bun transpiler cache — required for bun to load plugins without
	// re-transpiling on every container start. Pre-created by prepareVolumeDirs().
	bunCacheDir := filepath.Join(home, ".cache", "bun")
	bunCacheMount := bunCacheDir + ":/root/.cache/bun:ro"
	// Claude credentials — required for Anthropic provider auth. Mounted
	// read-write so the opencode-claude-auth plugin can write back refreshed
	// OAuth tokens to .credentials.json inside the container.
	claudeMount := filepath.Join(home, ".claude") + ":/root/.claude"
	// MCP auth — OAuth tokens written by mcp-remote (used by the Atlassian MCP
	// shim and other MCP servers that use OAuth). Mounted read-write so that
	// token refresh inside the container persists back to the host directory.
	// Source path: ${home}/.mcp-auth — only mounted when it exists on the host.
	mcpAuthDir := filepath.Join(home, ".mcp-auth")
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
	// Clipboard staging directory — images staged by `prism clipboard paste-image`
	// on the host are placed here and bind-mounted read-only into the container at
	// the identical absolute path so that opencode's stat() call resolves without
	// modification. No clipboard tool, socket, or env var is exposed inside the
	// container — clipboard read occurs entirely host-side.
	clipboardCacheDir := filepath.Join(home, ".cache", "prism", "clipboard")

	args := []string{
		"run",
		"--detach",
		// --tty allocates a PTY on the container. The container runs "opencode"
		// in combined TUI + HTTP mode; the tmux agent window uses "podman attach"
		// to bridge this PTY to the user's pane (RFC #691, Phase 1a / Issue #715).
		"--tty",
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
		// opencode state — per-session isolated dir, read-write.
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

	// auth.json overlay: bind-mount the host's opencode auth.json over the
	// per-session dir so OAuth tokens are shared across sessions without sharing
	// the SQLite DB. Only mounted when the file exists — skipped silently when
	// absent (e.g. after the parent dir was deleted for DB recovery). Uses the
	// same bind-mount-inside-mounted-dir pattern as the gitdir overlay.
	// Mounted read-write (:Z, not :ro,Z): the opencode-claude-auth plugin calls
	// writeFileSync on auth.json on every load to sync the active OAuth
	// credentials. A read-only mount causes EROFS and breaks Anthropic auth
	// inside the container. Keeping it writable also means token refreshes
	// written inside one session propagate back to the shared host file and are
	// visible to subsequent sessions — the intended shared-credentials behaviour.
	// :Z applies the SELinux label so podman can bind-mount it on
	// SELinux-enforcing hosts (Fedora/RHEL).
	if _, err := os.Stat(opencodeAuthJSON); err == nil {
		args = append(args, "--volume", opencodeAuthJSON+":/root/.local/share/opencode/auth.json:Z")
	}

	// MCP auth: bind-mount ~/.mcp-auth into the container at /root/.mcp-auth
	// so mcp-remote OAuth tokens obtained on the host are available inside the
	// container. Mounted read-write so token refresh inside the container
	// persists back to the host. Only mounted when the directory exists — hosts
	// without Atlassian MCP configured are unaffected.
	if _, err := os.Stat(mcpAuthDir); err == nil {
		args = append(args, "--volume", mcpAuthDir+":/root/.mcp-auth")
	}

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

	// Clipboard staging directory: bind-mount ~/.cache/prism/clipboard/ at
	// the identical host path inside the container, read-only. Images staged
	// by `prism clipboard paste-image` (running host-side via the tmux keybind)
	// are written here; opencode's drag-drop handler stat()s the path and reads
	// the bytes. The read-only mount ensures the container cannot write to the
	// host's staging directory. Only mounted when the directory exists — skipped
	// silently at container spawn time when no paste has occurred yet (consistent
	// with the conditional-mount pattern used for AWS/MCP-auth/kube config).
	if _, err := os.Stat(clipboardCacheDir); err == nil {
		args = append(args, "--volume", clipboardCacheDir+":"+clipboardCacheDir+":ro")
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
		// the live directory mount. Unlike the host-API socket (which uses a
		// per-session directory mount for isolation — see #960), the Nix daemon
		// socket has no cross-session exposure concern and is mounted directly.
		"--volume", "/nix/var/nix/daemon-socket:/nix/var/nix/daemon-socket",
		"--env", "NIX_CONFIG=store = daemon",

		// Terminal type — set to xterm-256color so the opencode TUI inside the
		// container has accurate terminal capability information. podman sets
		// TERM=xterm (plain) by default when --tty is used without an explicit
		// --env TERM=..., which breaks mouse events and SGR mouse protocol
		// selection (issue #737). xterm-256color is correct because:
		//   - Available in every Linux container (part of ncurses-base)
		//   - Full SGR mouse support (1006 protocol), matching what tmux expects
		//   - 256-colour support, matching the host terminal
		// Do NOT pass through the host's $TERM (e.g. tmux-256color or
		// screen-256color) — those terminfo entries may not exist in the
		// container and would cause opencode to fall back to a minimal profile.
		"--env", "TERM=xterm-256color",

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
		"--env", "PRISM_SESSION_NAME="+cfg.SessionName,

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
	// On Linux (cfg.HostAPITCPPort == 0): the session's own per-session socket
	// directory is mounted (not the entire run/ directory). Each session places its
	// socket at run/<sessionName>/hostapi.sock, so mounting only that directory
	// prevents sandboxed agents from accessing other sessions' sockets (security fix
	// #960). The directory-level mount (rather than a file mount) is required because
	// the socket file is created by the sidecar *after* the container becomes healthy
	// — the directory must pre-exist at podman run time so the bind-mount succeeds,
	// and the socket then appears inside it once the sidecar calls net.Listen("unix",
	// sockPath). The directory is pre-created by prepareVolumeDirs before Create().
	if cfg.HostAPITCPPort != 0 {
		args = append(args,
			"--env", fmt.Sprintf("PRISM_HOST_API=http://host.containers.internal:%d", cfg.HostAPITCPPort),
		)
	} else if cfg.HostAPISockPath != "" {
		sockDir := filepath.Dir(cfg.HostAPISockPath)
		sockBase := filepath.Base(cfg.HostAPISockPath)
		args = append(args,
			"--volume", sockDir+":/var/run/prism-host:Z",
			"--env", "PRISM_HOST_API=unix:///var/run/prism-host/"+sockBase,
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
	// agents/ is excluded for review containers: the host agents/ directory
	// contains review-*.md files with "mode: subagent" front-matter that
	// overrides the "mode: primary" declaration in the container's opencode.json,
	// causing opencode to register coordinator/worker as the only primary agents
	// and fall back to coordinator. Review containers embed their role prompt
	// inline via the opencode.json "prompt" field instead, so the agents/
	// directory is not mounted at all for these containers.
	//
	// For each entry that exists on the host:
	//   - Symlinks → resolve to real Nix store path and mount that
	//   - Directories → use --mount type=bind (podman creates dest automatically)
	//   - Regular files → use --volume
	isReviewContainer := strings.HasPrefix(cfg.AgentRole, "review-")
	configAllowlist := []string{
		"AGENTS.md",
		"plugins",
		"skills",
		"command",
		"tui.json",
		".gitignore",
		"mcp-atlassian-slim-proxy.mjs",
	}
	if !isReviewContainer {
		// agents/ is mounted for worker, coordinator, and all non-review
		// containers so that host-mode @review-* subagent invocation continues
		// to work and all other agents are accessible in those containers.
		configAllowlist = append(configAllowlist, "agents")
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

	// Inject profile-level agent env vars (AgentEnvVars, with KUBECONFIG /
	// AWS_CONFIG_FILE suppressed) and harness-specific runtime env vars
	// (RuntimeEnv). The podman appender uses "--env K=V" syntax.
	// See env.go:AppendStandardEnv for the suppression rationale.
	args = AppendStandardEnv(args, cfg, func(a []string, k, v string) []string {
		return append(a, "--env", k+"="+v)
	})

	// opencode.json: mount the generated config file when ConfigContent is set.
	// The file is written by Create() to a temp path and mounted read-only at
	// /root/.config/opencode/opencode.json. This replaces the previous
	// OPENCODE_CONFIG_CONTENT env var so that relative plugin paths (e.g.
	// "./plugins/my-plugin") resolve correctly from the config file's directory.
	if cfg.ConfigContent != "" {
		args = append(args, "--volume", m.opencodeConfigFilePath()+":/root/.config/opencode/opencode.json:ro")
	}

	// Resource caps: emit --memory, --memory-swap, and --pids-limit only when
	// the corresponding Config field is non-zero / non-empty. This preserves
	// existing behaviour for callers not using the nix module (empty fields →
	// no flag emitted).
	if cfg.MemoryMax != "" {
		args = append(args, "--memory="+cfg.MemoryMax)
	}
	if cfg.MemorySwapMax != "" {
		args = append(args, "--memory-swap="+cfg.MemorySwapMax)
	}
	if cfg.PidsLimit != 0 {
		args = append(args, fmt.Sprintf("--pids-limit=%d", cfg.PidsLimit))
	}

	// Image and command: opencode in combined TUI + HTTP mode.
	// "opencode --port N --hostname 0.0.0.0" launches the monolithic TUI on
	// the container's PTY (allocated via --tty) while simultaneously serving
	// the HTTP/SSE API on ContainerPort for the sidecar. The sidecar still
	// connects to the SSE endpoint on the mapped host port exactly as before.
	// The tmux agent window uses "podman attach" to bridge the PTY (RFC #691,
	// Phase 1a / Issue #715).

	args = append(args, Image,
		"opencode",
		"--port", fmt.Sprintf("%d", ContainerPort),
		"--hostname", "0.0.0.0",
	)

	// Pass --agent and --prompt as separate opencode flags when set.
	// These are always separate: --agent controls the system-prompt/role even
	// when there is no initial prompt (e.g. review agents need their role set
	// so opencode does not default to the "build" agent).
	// These are passed as individual args slice elements (no shell involved)
	// so no quoting is needed.
	if cfg.AgentRole != "" {
		args = append(args, "--agent", cfg.AgentRole)
	}
	if cfg.InitialPrompt != "" {
		args = append(args, "--prompt", cfg.InitialPrompt)
	}

	return args
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
