// Package container manages sandbox lifecycle and mount preparation for
// prism agent sessions.
//
// Each isolation mode ("bwrap", "sandbox-exec", "host" — see
// config.ValidIsolationModes) is implemented as an Isolator (isolator.go).
// The Manager prepares the shared per-session artefacts (SSH config,
// gitconfig, bind-mount sources) and dispatches lifecycle operations
// (create, shutdown, cleanup) to the registered Isolator for the session's
// mode.
//
// Health check: we probe GET /global/health (not GET /) because the root URL
// may redirect to an external URL on some harnesses, adding network latency
// on every startup. /global/health is in ControlPlaneRoutes and
// returns immediately with no external I/O.
//
// Design notes:
//   - The sandbox name is derived from the prism session name so it is
//     predictable and idempotent.
//   - Credentials are injected as environment variables, never as mounted files.
//   - The agent serve port (ContainerPort) is bound to 127.0.0.1
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
	// on first use by the systemd/launchd service. The container runtime
	// resolves the correct arch (amd64/arm64) automatically from the
	// multi-arch manifest.
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
// Only bwrap consumes the mode-based generators today: it runs as the host
// user, so canonical paths are
// $HOME/.ssh/{access-key,signing-key,signing-key.pub,allowed_signers}
// where $HOME is the host user's home directory. sandbox-exec generates its
// configs into the per-session work dir instead (session_work_dir.go,
// issue #2213) with stable host key paths — the former isolationSandboxExec
// mode value was deleted with the staging HOME in Step 5 of #2132.
type isolationMode int

const (
	isolationBwrap isolationMode = iota
)

// sandboxHome returns the in-sandbox $HOME directory for the given isolation
// mode and Manager. For bwrap this is the host user's home directory, because
// bwrap shares the host user namespace.
//
// m may be nil for callers that do not yet have a Manager (e.g. early
// dispatch paths that only need the static bwrap mapping).
func sandboxHome(_ *Manager, mode isolationMode) string {
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
	// host.containers.internal — a gvproxy container-VM convention — must not
	// be used here). On Linux this is zero.
	HarnessPipeTCPPort int

	// AgentRunLogPath is the host-side path to the per-session agent-run log
	// (run/<sessionDirName>/agent-run.log). When non-empty it is exposed to
	// the sandboxed agent as PRISM_AGENT_RUN_LOG so the PI prism extension
	// can append a durable diagnostic line when it exhausts its first-connect
	// retries and gives up (issue #2357) — pane scrollback dies with the
	// pane, the log file survives. The log lives in the same per-session run
	// directory as the host-API socket, so the existing bwrap bind-mount and
	// sandbox-exec SBPL subpath grant for that directory cover it — no new
	// mount or allow rule is needed.
	AgentRunLogPath string

	// ContainersEnabled is the per-session runtime gate for the filtering
	// podman API socket proxy (#2317 / #2321 / #2322). When true, the
	// per-isolator BuildArgs / SBPL generator emits:
	//
	// bwrap (Step 4 / #2321):
	//   --setenv CONTAINER_HOST unix://<PodmanProxySockPath>
	//   --setenv DOCKER_HOST    unix://<PodmanProxySockPath>
	//   --bind   <sessionDir>/container-scratch <sessionDir>/container-scratch
	//
	// sandbox-exec (Step 5 / #2322):
	//   (allow file-read* file-write* (literal "<PodmanProxySockPath>"))
	//   CONTAINER_HOST=unix://<PodmanProxySockPath>   (env injection)
	//   DOCKER_HOST=unix://<PodmanProxySockPath>      (env injection)
	//   <sessionDir>/container-scratch rides on the existing
	//   (subpath <sessionDir>) RW grant in the SBPL profile.
	//
	// Both isolators' Prepare hook (lifecycle_dispatch.go) call
	// PrepareSessionWorkDir + mkdirs the container-scratch subdirectory so
	// the source path exists on disk before the sandbox is execed. Defaults
	// to false; sourced from agent_status.containers_enabled (the live DB
	// gate read by cmd/agent_run.go on the bwrap path and
	// cmd/agent_run_sandbox_exec_darwin.go on the sandbox-exec path).
	//
	// Critically, the upstream podman socket path is NEVER passed into the
	// sandbox — only the filtered PodmanProxySockPath the sidecar's proxy
	// goroutine listens on. The proxy is the agent's only container API
	// surface. See #2317 §4 for the threat model.
	ContainersEnabled bool

	// PodmanProxySockPath is the absolute host path of the per-session
	// filtering podman API socket created by the sidecar's podman-proxy
	// goroutine (#2317 §3b / #2320). The socket sits in the same per-session
	// run directory as HostAPISockPath, so on bwrap the directory bind that
	// already exposes the host-API socket also exposes this socket — no
	// additional bind-mount is required. On sandbox-exec the SBPL
	// generator emits a literal RW allow on this exact path; the (literal
	// …), not (subpath …), keeps any future content of the run dir isolated
	// from the sandboxed process. Typically populated via
	// session.SidecarPodmanProxyPath. Consumed only when ContainersEnabled
	// is true; ignored otherwise.
	PodmanProxySockPath string

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

	// GitHubTokenPath is the absolute path to the sops-decrypted GitHub token
	// secret file on the host (e.g. ~/.config/sops-nix/secrets/github_token on
	// Darwin). Used as a last-resort fallback by credentialEnvVars when neither
	// the role-specific PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE> var nor the inherited
	// GITHUB_TOKEN yields a non-empty value — this rescues agents from a Darwin
	// sops launchd decrypt race that freezes an empty GITHUB_TOKEN into the tmux
	// server env (#2029). When empty, the file fallback is skipped.
	GitHubTokenPath string

	// GitHubTokenPaths maps <ACCOUNT>_<ROLE> keys (e.g. PRISMATIC_KOI_WORKER,
	// THANKYOU_PAYROLL_COORDINATOR) to the absolute host path of the
	// corresponding sops-decrypted GitHub token file. This is the primary
	// source of truth for GitHub token resolution as of issue #2348:
	// credentialEnvVars reads the file at spawn time so the token value never
	// depends on shell expansion having happened for the PRISM_GITHUB_TOKEN_*
	// env vars — which is what broke every session under the boot-restore
	// path (tmux started from a systemd unit, `$(cat …)` never expanded).
	// When a key IS present in this map and the file at that path is missing
	// or unreadable, credentialEnvVars errors out with a diagnostic naming
	// the path (never the value). When the map is empty or the key is absent,
	// credentialEnvVars falls back to the env-var chain (with a $(-literal
	// guard).
	GitHubTokenPaths map[string]string

	// InitialPrompt is the initial prompt to deliver to the agent at startup.
	// When non-empty, it is appended to the agent command as
	// --agent <AgentRole> --prompt <text> so that the agent starts the session
	// with the prompt already in flight, visible in the TUI from the start.
	// This replaces the previous POST /session + prompt_async HTTP delivery
	// which created a second session invisible to the TUI (RFC #691 Phase 1a).
	InitialPrompt string

	// RuntimeEnv holds harness-specific environment variables to inject
	// into the sandbox. Each entry is emitted in the isolator's native
	// syntax (e.g. --setenv KEY VALUE for bwrap). Populated from
	// harness.Harness.RuntimeEnv() by the sidecar at container creation
	// time. When nil, no harness-specific env vars are injected.
	RuntimeEnv map[string]string

	// AgentEnvVars holds the profile-level environment variables to inject
	// into the agent shell. Each entry is emitted in the isolator's native
	// syntax (e.g. --setenv KEY VALUE for bwrap). Sourced from profiles.json agent_env_vars
	// (written by Nix). These carry entries such as GIT_EDITOR=true,
	// KUBECONFIG, and AWS_CONFIG_FILE into the sandboxed agent.
	// When nil or empty, no profile env vars are injected.
	AgentEnvVars map[string]string

	// Harness is the harness name from the session DB row (e.g. "pi").
	// When empty, "pi" is assumed for back-compat. Used by
	// BuildArgs to select the correct sandbox-terminator invocation.
	Harness string

	// PIAgentConfigHostDir is the absolute host path to the shared PI agent
	// config directory (~/.pi/agent). Since design #2031 PR3 (#2034) the
	// per-session staging dir has been collapsed into a single shared mount of
	// the user's ~/.pi/agent. When non-empty and Harness == "pi", BuildArgs
	// (bwrap) bind-mounts this directory read-WRITE into the sandbox at
	// PIAgentConfigSandboxDir and sets PI_CODING_AGENT_DIR to the in-sandbox
	// path so PI discovers settings.json / themes / AGENTS.md / skills /
	// auth.json. Writes to auth.json, atlassian-mcp-oauth.json, and sessions/
	// reach the host via the same parent bind — there is no separate overlay
	// (the pre-#2034 "RO parent + RW file overlays" design was rejected
	// because proper-lockfile's auth.json.lock mkdir on the parent dir needs
	// write access; see pi_invocation.go top-of-file for the full rationale).
	// The role system-prompt is injected at runtime by the prism PI extension,
	// not via this directory (design #2031).
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

// NameForSession returns the stable sandbox name for a session.
// The name is derived from the session name with "@", "/", ".", and "~"
// replaced by "-" and a "prism-" prefix, e.g. "prism-nixos-config-feature".
//
// The "~" replacement is needed for review agent session names which are
// structured as "<parent>~review-<N>~<agentName>" — it keeps the name within
// the conservative charset [a-zA-Z0-9][a-zA-Z0-9_.-]*.
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

	// piBwrapErr holds any error produced by appendPIBwrapMounts during
	// BuildArgs. BuildArgs cannot return an error, so the error is stored
	// here and checked by Prepare after BuildArgs returns.
	piBwrapErr error

	// credentialsErr holds any error produced by credentialEnvVars during
	// BuildArgs.  As of issue #2348 credentialEnvVars can fail when a
	// configured GitHub token file path is unreadable, and the spawn must
	// fail with a diagnostic naming the path (never the value) rather than
	// silently proceeding with no token.  BuildArgs cannot return an error,
	// so it stashes the error here and Prepare surfaces it — same pattern
	// as piBwrapErr.
	credentialsErr error

	// worktreeGitDirErr holds any error produced when WorktreeGitDir does not
	// exist on disk at BuildArgs time. Previously this case was silently
	// skipped (an os.Stat failure just meant the --bind was never emitted),
	// which combined with a derived-and-possibly-wrong WorktreeGitDir path to
	// make the failure invisible (issue #2518). Now that WorktreeGitDir is
	// resolved from the worktree's authoritative .git pointer, a missing
	// directory is a real error and must be reported, not skipped. BuildArgs
	// cannot return an error, so it stashes the error here and Prepare
	// surfaces it — same pattern as piBwrapErr.
	worktreeGitDirErr error
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

// tempDir returns the directory under which per-session temp artefact files
// are created. It is a test seam: production code never reassigns it, so
// every real binary uses the prior default, os.TempDir. The container test
// suite replaces it once in TestMain (container_test.go) with a per-process
// directory — fixture session names (e.g. "repo@feat") repeat across the
// suite, so without per-process namespacing two concurrent `go test`
// processes in different worktrees on the same host would read/write the
// same /tmp/prism-gitconfig-prism-repo-feat file (issue #2222).
var tempDir = os.TempDir

// sessionTempPath is the package-level building block for per-session temp
// file paths. All per-session artefact files follow the shape:
//
//	<tempDir()>/prism-<stem>-<session_name><suffix>
//
// where tempDir() is os.TempDir() outside tests (see the tempDir seam).
//
// stem identifies the artefact (e.g. "gitdir", "ssh-config"); suffix is ""
// for most artefacts and ".sb" for the sandbox-exec SBPL profile.
//
// It is a free function so that exported helpers like HarnessConfigFilePath
// can share the same path logic without requiring a Manager receiver.
func sessionTempPath(stem, suffix, name string) string {
	return filepath.Join(tempDir(), "prism-"+stem+"-"+name+suffix)
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

// writeSshConfig generates a minimal ~/.ssh/config for the sandbox and writes
// it to a temp file. The file is later mounted at $HOME/.ssh/config:ro inside
// the sandbox, where $HOME depends on the isolation mode (see isolationMode).
//
// The config only needs to handle GitHub; other SSH hosts are not expected
// inside agent sandboxes. The IdentityFile path is $HOME/.ssh/access-key
// using the sandbox-specific $HOME prefix, which matches the generic name
// used by the bwrap bind-mount (<hostHome>/.ssh/access-key).
//
// Only the bwrap Prepare path calls this; sandbox-exec generates its ssh
// config into the per-session work dir instead (writeSshConfigToDir in
// session_work_dir.go, issue #2213) with the stable host key path.
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
//   - [user] — always; name and email come from cfg.GitUserName /
//     cfg.GitUserEmail. Includes signingKey when the signing public key can
//     be resolved.
//   - [commit] / [gpg] — only when the signing keys are available.
//   - [push] — autoSetupRemote = true (always).
//   - [init] — defaultBranch = main (always).
//
// Missing identity → return error, refuse to write the gitconfig (issue
// #1960). The previous behaviour of "warn and skip [user]" caused git inside
// the sandbox to fall back to OS-level identity guessing
// (`<sandbox-user>@<sandbox-host>`), which GitHub then aggregated into noisy
// `Co-authored-by:` trailers on squash merge.
//
// Missing signing keys → signing sections omitted, warning logged (AC-13).
//
// The mode argument controls the paths embedded in the generated file:
//
//   - isolationBwrap: signingKey = <hostHome>/.ssh/signing-key.pub,
//     allowedSignersFile = <hostHome>/.ssh/allowed_signers (paths inside
//     the bwrap sandbox, where the agent runs as the host user).
//
// Note: the sandbox-exec path does not consume this writer —
// writeGitconfigToDir (session_work_dir.go) generates the work-dir gitconfig
// with stable host key paths instead (issue #2213, Step 2 of #2132).
func (m *Manager) writeGitconfig(mode isolationMode) error {
	// Canonical in-sandbox paths for the signing artefacts. Both paths live
	// at $HOME/.ssh/<generic-name> where $HOME depends on the isolation mode
	// (see sandboxHome). Agents inside the sandbox see generic filenames —
	// signing-key.pub / allowed_signers — regardless of which sandbox layer
	// they're running under.
	sandboxSshDir := filepath.Join(sandboxHome(m, mode), ".ssh")
	return m.writeGitconfigArtefacts(
		filepath.Join(sandboxSshDir, "signing-key.pub"),
		filepath.Join(sandboxSshDir, "allowed_signers"),
		m.gitconfigFilePath(),
		m.allowedSignersFilePath(),
	)
}

// writeGitconfigArtefacts is the canonical gitconfig generator shared by the
// per-mode writeGitconfig (bwrap layout, temp-file destinations) and the
// session-work-dir writer writeGitconfigToDir (stable host key paths,
// work-dir destinations — issue #2213, used by sandbox-exec).
//
// Parameters:
//
//   - embedSigningKeyPub — the path embedded as user.signingKey in the
//     generated gitconfig (what git/ssh-keygen will read inside the sandbox).
//   - embedAllowedSigners — the path embedded as gpg.ssh.allowedSignersFile.
//   - gitconfigDst — where the generated gitconfig is written on the host.
//   - allowedSignersDst — where the generated allowed_signers is written on
//     the host.
//
// The signing availability check and the allowed_signers content always read
// the host's stable sops symlink paths (~/.ssh/<SshSigningKeyName>{,.pub});
// only the embedded paths and destinations vary by caller.
func (m *Manager) writeGitconfigArtefacts(embedSigningKeyPub, embedAllowedSigners, gitconfigDst, allowedSignersDst string) error {
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

	// [user] section — required. Missing identity is a hard error (issue
	// #1960): without a configured [user] section, git falls back to
	// OS-level identity guessing inside the sandbox (e.g. `worker@prism`,
	// `bot@local`), and any commit produced by the session is authored as
	// that synthetic identity. GitHub then aggregates the synthetic
	// identity into a `Co-authored-by:` trailer when the PR is squash
	// merged. Refusing to write the gitconfig forces the operator to fix
	// the upstream config (nix → prism-tui.nix extractor → config.json) at
	// session-start time rather than discovering the noise post-merge.
	if m.cfg.GitUserName == "" || m.cfg.GitUserEmail == "" {
		return fmt.Errorf(
			"container: git identity missing (name=%q, email=%q): refusing to start session without [user] in gitconfig — "+
				"set git_user_name and git_user_email in ~/.config/prism/config.json (Nix users: ensure programs.git.includes[].contents.user.name and .email are set; "+
				"prism-tui.nix extracts these into config.json at switch time)",
			m.cfg.GitUserName, m.cfg.GitUserEmail,
		)
	}
	sb.WriteString("[user]\n")
	sb.WriteString("    name = " + m.cfg.GitUserName + "\n")
	sb.WriteString("    email = " + m.cfg.GitUserEmail + "\n")
	if hasSigning {
		sb.WriteString("    signingKey = " + embedSigningKeyPub + "\n")
	}

	// [commit] and [gpg] sections — only when signing keys are available.
	// (Identity is now guaranteed present by the check above.)
	if hasSigning {
		// Read the signing public key content to build the allowed_signers file.
		// Only write [gpg "ssh"] allowedSignersFile when the file was actually
		// produced — if it can't be written, the sandbox must not be given a
		// bind-mount source path that doesn't exist on disk.
		pubKeyContent, err := os.ReadFile(signingKeyPub)
		if err != nil {
			log.Printf("container: failed to read signing public key %q: %v; skipping allowed_signers", signingKeyPub, err)
		} else {
			allowedSignersContent := m.cfg.GitUserEmail + " " + strings.TrimSpace(string(pubKeyContent)) + "\n"
			if err := os.WriteFile(allowedSignersDst, []byte(allowedSignersContent), 0o644); err != nil {
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
			sb.WriteString("    allowedSignersFile = " + embedAllowedSigners + "\n")
		}
	}

	// [push] and [init] — always included.
	sb.WriteString("\n[push]\n")
	sb.WriteString("    autoSetupRemote = true\n")
	sb.WriteString("\n[init]\n")
	sb.WriteString("    defaultBranch = main\n")

	if err := os.WriteFile(gitconfigDst, []byte(sb.String()), 0o644); err != nil {
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

// PrepareBwrap writes the per-session temp files (SSH config, gitconfig,
// opencode.json config) and returns the
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

// PrepareSandboxExec prepares the per-session work dir, writes the SBPL
// profile, and returns the complete sandbox-exec argument list.
//
// The returned args slice is suitable for passing directly to
// syscall.Exec("/usr/bin/sandbox-exec", args, env). The first element of
// args is "sandbox-exec" itself (argv[0] under syscall.Exec).
//
// $HOME inside the sandbox is the real host home (Step 5 of #2132);
// cmd/agent_run_sandbox_exec_darwin.go constructs the env after this call.
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

// prepareVolumeDirs eagerly creates host directories that the sandbox
// argument builders will reference as bind-mount sources. A sandbox launch
// fails with an unhelpful error if any bind-mount source is absent, so we
// create them here — before the argument builders run — so that the builders
// themselves remain pure with no filesystem side-effects.
//
// Directories are classified as critical or optional:
//   - Critical directories are unconditionally bound by the sandbox
//     (bwrap --bind). A missing critical dir causes the
//     runtime to abort at exec time with an unhelpful error, so we return an
//     error immediately so the caller sees the real cause.
//   - Optional directories are guarded by an os.Stat check in the argument
//     builders and are skipped when absent. A failure to create them is
//     logged but does not fail the call — the sandbox still starts, just
//     without that mount.
//
// perSessionState controls whether the per-session pi state directory
// (~/.local/share/pi/prism-sessions/<name>/) is created. The removed legacy
// container path required it (Darwin virtiofs WAL-mode locking workaround);
// the bwrap path shares the host pi data dir directly and does not need a
// per-session dir.
func (m *Manager) prepareVolumeDirs(perSessionState bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// ── Critical directories ──────────────────────────────────────────────
	// These are unconditionally bound by the sandbox. A single
	// MkdirAll failure here is fatal — return immediately so the caller
	// sees the real cause rather than a confusing runtime abort.

	// Per-session pi state directory (legacy container path only).
	if perSessionState {
		piSessionDir := filepath.Join(home, ".local", "share", "pi", "prism-sessions", m.name)
		if err := os.MkdirAll(piSessionDir, 0o755); err != nil {
			return fmt.Errorf("container: failed to create per-session pi state dir %q: %w", piSessionDir, err)
		}
	}

	// Per-session host-API socket directory (security fix #960).
	// Each session places its socket in its own subdirectory
	// (~/.local/state/prism/run/<sessionName>/hostapi.sock) instead of the
	// shared run/ directory. The directory must be pre-created here so it
	// exists before the sandboxed process starts: it must pre-exist before
	// the sidecar calls net.Listen("unix", sockPath); the sandbox is exec'd
	// after.
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
