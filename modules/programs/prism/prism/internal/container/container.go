// Package container manages the podman container lifecycle for prism sidecar.
//
// The sidecar (running on the host) creates a podman container running
// the agent in combined TUI + HTTP mode, health-checks it until the HTTP
// endpoint responds, and stops/removes the container on shutdown. The tmux
// agent window attaches to the container's PTY via "podman attach" so the
// agent TUI is visible to the user.
//
// Health check: we probe GET /global/health (not GET /) because the root URL
// may redirect to an external URL on some harnesses, adding network latency
// on every container startup. /global/health is in ControlPlaneRoutes and
// returns immediately with no external I/O.
//
// Design notes:
//   - All podman operations use exec.Command("podman", ...) — no daemon or
//     socket is required from Go's perspective, just a podman binary on PATH.
//   - The container name is derived from the prism session name so it is
//     predictable and idempotent.
//   - Credentials are injected as environment variables, never as mounted files.
//   - The agent serve container port (ContainerPort) is bound to 127.0.0.1
//     only — not 0.0.0.0. The host-API TCP listener (Darwin only) intentionally
//     binds 0.0.0.0 so the gvproxy bridge interface can reach it from the VM.
package container

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
)

const (
	// ContainerPort is the port the agent harness listens on inside the container.
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
// The three sandboxes mount the SSH artefacts at different in-sandbox paths:
//
//   - podman runs as root, so canonical paths are /root/.ssh/{access-key,
//     signing-key,signing-key.pub,allowed_signers} — the agent's $HOME is /root.
//   - bwrap runs as the host user, so canonical paths are
//     $HOME/.ssh/{access-key,signing-key,signing-key.pub,allowed_signers}
//     where $HOME is the host user's home directory.
//   - sandbox-exec also runs as the host user but with a per-session staging
//     HOME (e.g. ~/.local/state/prism/sandbox-exec/<instance>) — the agent
//     sees this as $HOME and the SSH artefacts are referenced under it.
//
// Agents inside all three sandboxes see the same generic filenames
// (access-key, signing-key, …); only the $HOME prefix differs. The isolation
// mode lets the generators substitute the correct prefix into the config
// files they write.
type isolationMode int

const (
	isolationBwrap isolationMode = iota
	isolationSandboxExec
)

// sandboxHome returns the in-sandbox $HOME directory for the given isolation
// mode and Manager. For bwrap this is the host user's home directory, because
// bwrap shares the host user namespace. For sandbox-exec it is the per-session
// staging HOME (which is what the agent sees as $HOME inside the sandbox);
// when the staging HOME cannot be resolved the helper falls back to the host
// home so that callers always receive a non-empty path.
//
// m may be nil for callers that do not yet have a Manager (e.g. early
// dispatch paths that only need the static bwrap mapping); in that case
// sandbox-exec falls back to the host home.
func sandboxHome(m *Manager, mode isolationMode) string {
	switch mode {
	case isolationBwrap:
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = os.Getenv("HOME")
		}
		return home
	case isolationSandboxExec:
		if m != nil {
			if stagingHome, err := m.sandboxExecHomePath(); err == nil && stagingHome != "" {
				return stagingHome
			}
		}
		// Fallback: host home. Used in the rare case where the staging
		// HOME cannot be derived (no instance ID, no UserHomeDir). The
		// resulting gitconfig still points at $HOME-relative SSH paths,
		// matching the bwrap layout — which is the closest valid mapping
		// for sandbox-exec when staging is unavailable.
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
	// prompt (e.g. "anthropic/claude-sonnet-4-6"). When empty, the harness
	// default model for the session is used (which may differ from the host
	// harness config and cause "model not supported" errors).
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

	// HarnessPipeSockPath is the host-side path to the PI harness pipe Unix
	// socket (Linux only). When non-empty and HarnessPipeTCPPort is zero,
	// PRISM_HARNESS_PIPE is set to unix://<HarnessPipeSockPath>.
	// The socket lives in the same per-session directory as the host-API socket
	// so the existing bind-mount for that directory covers it too — no new
	// bind-mount is needed. On Darwin this field is still set but
	// HarnessPipeTCPPort takes precedence.
	HarnessPipeSockPath string

	// HarnessPipeTCPPort is the host-side TCP port for the PI harness pipe
	// listener (Darwin only). When non-zero, PRISM_HARNESS_PIPE is set to
	// tcp://127.0.0.1:<HarnessPipeTCPPort> for sandbox-exec sessions (both
	// the sidecar and the sandboxed extension run on the host loopback, so
	// host.containers.internal — a podman/gvproxy convention — must not be
	// used here). On Linux this is zero.
	HarnessPipeTCPPort int

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

	// SshBin is the absolute path to the ssh binary for GIT_SSH_COMMAND in
	// sandbox-exec sessions. When non-empty, it is used instead of bare "ssh"
	// so that the Nix-built openssh (which uses its own libresolv/libldns) is
	// invoked rather than /usr/bin/ssh (which uses Apple's libnetwork.dylib and
	// requires system-network sandbox rules). When empty, falls back to "ssh".
	SshBin string

	// InitialPrompt is the initial prompt to deliver to the agent at startup.
	// When non-empty, it is appended to the agent command as
	// --agent <AgentRole> --prompt <text> so that the agent starts the session
	// with the prompt already in flight, visible in the TUI from the start.
	// This replaces the previous POST /session + prompt_async HTTP delivery
	// which created a second session invisible to the TUI (RFC #691 Phase 1a).
	InitialPrompt string

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

	// Harness is the harness name from the session DB row (e.g. "pi").
	// When empty, "pi" is assumed for back-compat. Used by
	// BuildArgs to select the correct sandbox-terminator invocation.
	Harness string

	// PIAgentConfigHostDir is the absolute host path to the per-session
	// PI agent config staging directory (e.g.
	// ~/.local/state/prism/run/<hash>/pi-agent/). When non-empty and
	// Harness == "pi", BuildArgs (bwrap) bind-mounts this directory read-only
	// into the sandbox at PIAgentConfigSandboxDir and sets PI_CODING_AGENT_DIR
	// to that in-sandbox path. PI discovers APPEND_SYSTEM.md automatically.
	PIAgentConfigHostDir string

	// PIAgentConfigSandboxDir is the in-sandbox path at which the PI agent
	// config directory is mounted. Defaults to /run/prism/pi-agent when empty.
	PIAgentConfigSandboxDir string

	// PIExtensionHostDir is the absolute host path to the directory containing
	// the prism PI extension file(s). When non-empty and Harness == "pi",
	// BuildArgs (bwrap) bind-mounts this directory read-only into the sandbox
	// at PIExtensionSandboxDir and passes the extension to PI via --extension.
	PIExtensionHostDir string

	// PIExtensionSandboxDir is the in-sandbox path at which the PI extension
	// directory is mounted. Defaults to /etc/prism/pi-extensions when empty.
	PIExtensionSandboxDir string

	// PIProvider is the provider string to pass to PI via --provider.
	// Derived from the profile slot's Provider field.
	PIProvider string

	// PIModel is the model string to pass to PI via --model.
	// Derived from the profile slot's Model field.
	PIModel string

	// PIThinking is the thinking/reasoning level to pass to PI via --thinking.
	// Derived from the profile slot's Thinking field. When empty, the flag is
	// omitted and PI uses its own default.
	PIThinking string

	// HarnessSessionID is the persisted harness-specific session UUID to resume
	// when launching the harness (e.g. pi's session UUID written by the sidecar
	// on the `session_status` frame). When non-empty and Harness == "pi",
	// PIInvocation looks up the on-disk session JSONL using the mode-aware
	// sessions-root resolver and, if found, appends `--session <HarnessSessionID>`
	// so pi reopens the prior conversation. When empty (fresh session, no prior
	// incarnation, or the harness failed to start last time) the flag is omitted
	// and pi starts a new conversation.
	//
	// Populated by `prism restore` (and `prism restart`, which calls Restore).
	// `prism spawn` / `prism switch` leave this empty by design — those paths
	// create new sessions.
	HarnessSessionID string

	// PIBinaryPath is the absolute host path to the pi binary
	// (e.g. /nix/store/.../bin/pi or from the Nix profile at
	// /etc/profiles/per-user/<user>/bin/pi). When non-empty and
	// Harness == "pi", BuildArgs (bwrap) bind-mounts this path read-only into
	// the sandbox and PIInvocation uses it as argv[0] so that pi
	// is accessible inside the bwrap namespace. An empty PIBinaryPath for a
	// harness=pi session is treated as a configuration error: populatePIConfig
	// returns a clear error rather than falling back to a bare "pi" name that
	// would fail with ENOENT inside the sandbox.
	PIBinaryPath string
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

	// Manager manages the lifecycle of a single agent session sandbox.
type Manager struct {
	cfg            Config
	name           string
	healthCheckURL string
	httpClient     *http.Client
	// isolator is the Isolator implementation used to run, shut down, and
	// inspect the sandbox process. It is set to a hostIsolator by default in
	// New() and can be replaced in tests or by PrepareBwrap/PrepareSandboxExec.
	isolator Isolator
	// allowedSignersReady is true when writeGitconfig successfully wrote the
	// allowed_signers temp file. buildRunArgs uses this to gate the bind-mount
	// so that bwrap is never given a source path that doesn't exist on disk.
	allowedSignersReady bool
	// claudeCredentialsReady is true when writeClaudeCredentials successfully
	// extracted Claude credentials from the macOS Keychain and wrote them to
	// a temp file. buildRunArgs uses this to gate the bind-mount so that
	// bwrap is never given a source path that doesn't exist on disk.
	// This field is only ever true on Darwin.
	claudeCredentialsReady bool

	// piBwrapErr holds any error produced by appendPIBwrapMounts during
	// BuildArgs. BuildArgs cannot return an error, so the error is stored
	// here and checked by Prepare after BuildArgs returns.
	piBwrapErr error
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
		isolator:       newHostIsolator(name),
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
// It is a free function so that exported helpers like HarnessConfigFilePath
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

// harnessConfigFilePath returns the host path for the temporary opencode.json
// config file written before container start. The file is mounted read-only
// at /root/.config/opencode/opencode.json inside the container so that plugin
// paths (e.g. "./plugins/my-plugin") resolve correctly relative to the config
// file location.
func (m *Manager) harnessConfigFilePath() string {
	return HarnessConfigFilePath(m.name)
}

// HarnessConfigFilePath returns the host path for the temporary opencode.json
// config file for the given session name. The path is deterministic and
// derived from the session name, so callers outside the Manager (e.g.
// cmd/spawn.go) can write the file before the Manager is constructed.
// Delegates to sessionTempPath so all per-session paths share one naming rule.
func HarnessConfigFilePath(sessionName string) string {
	return sessionTempPath("harness-config", "", sessionName)
}

// WriteHarnessConfig writes content to the temp opencode.json file for the
// given session name. It creates or overwrites the file with mode 0o644.
// Returns a wrapped error on failure.
func WriteHarnessConfig(sessionName, content string) error {
	path := HarnessConfigFilePath(sessionName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("container: write harness config for session %q: %w", sessionName, err)
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
	identityFile := filepath.Join(sandboxHome(m, mode), ".ssh", "access-key")
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
//   - isolationSandboxExec: signingKey = <stagingHome>/.ssh/signing-key.pub,
//     allowedSignersFile = <stagingHome>/.ssh/allowed_signers (paths inside
//     the sandbox-exec staging HOME, which is what the agent sees as $HOME
//     under sandbox-exec).
//
// All three sandboxes mount or stage the same underlying host key files —
// only the $HOME prefix differs. Generic filenames (signing-key.pub,
// allowed_signers, …) are used in every case so agents see a uniform layout
// regardless of mode.
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
	sandboxSshDir := filepath.Join(sandboxHome(m, mode), ".ssh")
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
// Post A1.L4 (issue #1140): the body of this method moved into
// bwrapIsolator.Prepare; this method is now a thin dispatcher that resolves
// the bwrap isolator from the registry and forwards to its Prepare method.
func (m *Manager) PrepareBwrap() ([]string, error) {
	iso, err := For(config.IsolationBwrap, ConstructorOpts{Name: m.name})
	if err != nil {
		return nil, fmt.Errorf("container: bwrap: %w", err)
	}
	return iso.Prepare(context.Background(), m)
}

// PrepareSandboxExec prepares the per-session staging HOME, writes the SBPL
// profile, and returns the complete sandbox-exec argument list.
//
// The returned args slice is suitable for passing directly to
// syscall.Exec("/usr/bin/sandbox-exec", args, env). The first element of
// args is "sandbox-exec" itself (argv[0] under syscall.Exec).
//
// The env passed to syscall.Exec should set HOME=<staging_home> so that
// the agent and its tools find credentials and config at their canonical paths
// inside the staging HOME. agent_run.go constructs that env after this call.
//
// Post A1.L4 (issue #1140): the body of this method moved into
// sandboxExecIsolator.Prepare; this method is now a thin dispatcher.
func (m *Manager) PrepareSandboxExec() ([]string, error) {
	iso, err := For(config.IsolationSandboxExec, ConstructorOpts{Name: m.name})
	if err != nil {
		return nil, fmt.Errorf("container: sandbox-exec: %w", err)
	}
	return iso.Prepare(context.Background(), m)
}

// prepareVolumeDirs eagerly creates host directories that buildRunArgs() will
// reference as bind-mount sources. podman exits 125 ("statfs: no such file or
// directory") if any bind-mount source is absent, so we create them here —
// before buildRunArgs() is called — so that buildRunArgs() itself remains a
// pure argument builder with no filesystem side-effects.
//
// Directories are classified as critical or optional:
//   - Critical directories are unconditionally bound by the container runtime
//     (bwrap --bind, podman --volume). A missing critical dir causes the
//     runtime to abort at exec time with an unhelpful error, so we return an
//     error immediately so the caller sees the real cause.
//   - Optional directories are guarded by an os.Stat check in buildRunArgs
//     and are skipped when absent. A failure to create them is logged but does
//     not fail the call — the container still starts, just without that mount.
//
// perSessionState controls whether the per-session pi state directory
// (~/.local/share/pi/prism-sessions/<name>/) is created. The podman path
// requires it (Darwin virtiofs WAL-mode locking workaround); the bwrap path
// shares the host pi data dir directly and does not need a per-session dir.
func (m *Manager) prepareVolumeDirs(perSessionState bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// ── Critical directories ──────────────────────────────────────────────
	// These are unconditionally bound by the container runtime. A single
	// MkdirAll failure here is fatal — return immediately so the caller
	// sees the real cause rather than a confusing runtime abort.

	// Per-session pi state directory (podman only, critical because podman
	// always binds it via --volume).
	if perSessionState {
		piSessionDir := filepath.Join(home, ".local", "share", "pi", "prism-sessions", m.name)
		if err := os.MkdirAll(piSessionDir, 0o755); err != nil {
			return fmt.Errorf("container: failed to create per-session pi state dir %q: %w", piSessionDir, err)
		}
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
			return fmt.Errorf("container: failed to create host-API socket dir %q: %w", sockDir, err)
		}
	}

	// ── Optional directories ──────────────────────────────────────────────
	// These are guarded by os.Stat in buildRunArgs and skipped when absent.
	// Log failures but do not fail the call — the container still starts.

	// Agent plugin/model cache.
	piCacheDir := filepath.Join(home, ".cache", "pi")
	if err := os.MkdirAll(piCacheDir, 0o755); err != nil {
		log.Printf("container: failed to create pi cache dir %q (optional): %v", piCacheDir, err)
	}

	// bun transpiler cache.
	bunCacheDir := filepath.Join(home, ".cache", "bun")
	if err := os.MkdirAll(bunCacheDir, 0o755); err != nil {
		log.Printf("container: failed to create bun cache dir %q (optional): %v", bunCacheDir, err)
	}

	// Clipboard staging directory: pre-create so that the bind-mount in
	// buildRunArgs() is always active, even on the first paste. Without this,
	// a first-ever paste on a fresh system would write the file host-side but
	// the container would not see it (the bind-mount only fires when the dir
	// exists at container spawn time). Creating it eagerly here ensures the
	// directory always exists before buildRunArgs() runs its os.Stat check.
	clipboardCacheDir := filepath.Join(home, ".cache", "prism", "clipboard")
	if err := os.MkdirAll(clipboardCacheDir, 0o755); err != nil {
		log.Printf("container: failed to create clipboard staging dir %q (optional): %v", clipboardCacheDir, err)
	}

	return nil
}

