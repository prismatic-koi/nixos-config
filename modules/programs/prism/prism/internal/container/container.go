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
	"github.com/prismatic-koi/prism/internal/usage"
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
// configs into the per-session work dir instead (session_work_dir.go) with
// stable host key paths.
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

	// AgentModel is the per-role model override for this session's agent
	// role (e.g. "anthropic/claude-sonnet-4-6"), sourced from
	// `prism spawn --model-override <role>=<model>` and carried to
	// `prism agent-run` as --agent-model. populatePIConfig writes it; when
	// it is non-empty PIInvocation emits it as `--model <AgentModel>` in
	// place of PIModel, so the per-role entry wins over both the profile
	// slot and the session-wide `--model` flag. Empty means no per-role
	// override for this role, and PIModel is used unchanged.
	AgentModel string

	// PluginHostPath is unused. Kept for struct compatibility.
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
	// sessions' sockets. On Darwin this field is still set but
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
	// retries and gives up — pane scrollback dies with the
	// pane, the log file survives. The log lives in the same per-session run
	// directory as the host-API socket, so the existing bwrap bind-mount and
	// sandbox-exec SBPL subpath grant for that directory cover it — no new
	// mount or allow rule is needed.
	AgentRunLogPath string

	// ContainersEnabled is the per-session runtime gate for the filtering
	// podman API socket proxy. When true, the
	// per-isolator BuildArgs / SBPL generator emits:
	//
	// bwrap:
	//   --setenv CONTAINER_HOST unix://<PodmanProxySockPath>
	//   --setenv DOCKER_HOST    unix://<PodmanProxySockPath>
	//   --bind   <sessionDir>/container-scratch <sessionDir>/container-scratch
	//
	// sandbox-exec:
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
	// surface. See the podman-proxy security spec for the threat model.
	ContainersEnabled bool

	// PodmanProxySockPath is the absolute host path of the per-session
	// filtering podman API socket created by the sidecar's podman-proxy
	// goroutine. The socket sits in the same per-session
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
	// server env. When empty, the file fallback is skipped.
	GitHubTokenPath string

	// GitHubTokenPaths maps <ACCOUNT>_<ROLE> keys (e.g. PRISMATIC_KOI_WORKER,
	// THANKYOU_PAYROLL_COORDINATOR) to the absolute host path of the
	// corresponding sops-decrypted GitHub token file. This is the primary
	// source of truth for GitHub token resolution:
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

	// GitLabTokenPath is the absolute path to the sops-decrypted GitLab token
	// secret file on the host (e.g. ~/.config/sops-nix/secrets/gitlab_token on
	// Darwin). credentialEnvVars reads the file at spawn time and injects the
	// contents as GITLAB_TOKEN, so the value never depends on a shell having
	// expanded the host's "$(cat <path>)" session variable — the same
	// file-first shape GitHubTokenPaths uses. The path is also the
	// source that admits `gitlab_token` to the sandbox-exec secrets.d
	// allowlist (collectSecretsDAllowlistNames). Empty means no GitLab token
	// is configured on this host: no env var is injected and no secrets.d
	// exception is emitted.
	GitLabTokenPath string

	// GrafanaConfigPath is the absolute host path to the sops-decrypted pi
	// grafana MCP config bundle (e.g.
	// ~/.config/sops-nix/secrets/grafana_config_home on Darwin). It is the
	// SAME value prism injects into the sandbox as GRAFANA_MCP_CONFIG_PATH;
	// the sandbox-exec spawn path copies it off AgentEnvVars so the profile
	// generator has a named source rather than an ad-hoc map lookup.
	//
	// Its only consumer is collectSecretsDAllowlistNames, which uses it to
	// admit the bundle's secret NAME to the sandbox-exec secrets.d allowlist —
	// unlike GitLabTokenPath, prism never reads this file host-side. The pi
	// grafana extension reads it itself, inside the sandbox, which is exactly
	// the "an in-sandbox consumer reads it" test that inventory applies.
	//
	// Empty means no exception is emitted and the bundle stays denied. That
	// covers a host with nx.programs.prism.pi.grafana.enable = false AND every
	// review role, whose GRAFANA_MCP_CONFIG_PATH is stripped by
	// internal/config/agent_env_roles.go — so the file allowlist
	// tracks the capability gate with no second list to maintain.
	//
	// The bwrap isolator does NOT read this field: it derives the same path
	// inline from AgentEnvVars to emit its --ro-bind.
	GrafanaConfigPath string

	// InitialPrompt is the initial prompt to deliver to the agent at startup.
	// When non-empty, it is appended to the agent command as
	// --agent <AgentRole> --prompt <text> so that the agent starts the session
	// with the prompt already in flight, visible in the TUI from the start.
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
	// config directory (~/.pi/agent) — a single shared mount of the user's
	// ~/.pi/agent. When non-empty and Harness == "pi", BuildArgs
	// (bwrap) bind-mounts this directory read-WRITE into the sandbox at
	// PIAgentConfigSandboxDir and sets PI_CODING_AGENT_DIR to the in-sandbox
	// path so PI discovers settings.json / themes / AGENTS.md / skills /
	// auth.json. Writes to auth.json, atlassian-mcp-oauth.json, and sessions/
	// reach the host via the same parent bind — there is no separate overlay.
	// The mount is read-WRITE because proper-lockfile's auth.json.lock mkdir on
	// the parent dir needs write access; see pi_invocation.go top-of-file for
	// the full rationale. The role system-prompt is injected at runtime by the
	// prism PI extension, not via this directory.
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
	// BuildArgs.  credentialEnvVars can fail when a
	// configured GitHub token file path is unreadable, and the spawn must
	// fail with a diagnostic naming the path (never the value) rather than
	// silently proceeding with no token.  BuildArgs cannot return an error,
	// so it stashes the error here and Prepare surfaces it — same pattern
	// as piBwrapErr.
	credentialsErr error

	// worktreeGitDirErr holds any error produced when WorktreeGitDir does not
	// exist on disk at BuildArgs time. WorktreeGitDir is resolved from the
	// worktree's authoritative .git pointer, so a missing directory is a real
	// error and must be reported, not silently skipped. BuildArgs
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
// same /tmp/prism-gitconfig-prism-repo-feat file.
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
// It is a free function so that exported helpers can share the same path
// logic without requiring a Manager receiver.
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

// SandboxSshHosts are the forge hosts the generated sandbox ssh config
// carries a stanza for, in emission order. Both map to the SAME mounted
// access key: modules/programs/git.nix points github.com and gitlab.com at
// ~/.ssh/prismatic-koi-ed25519 on the host, so gitlab.com needs no new key
// material inside the sandbox — only a host stanza.
//
// No other SSH host is expected inside an agent sandbox. Adding one here
// grants the sandbox git-over-SSH reach to that host with the access key,
// so treat any addition as a sandbox-boundary change.
var SandboxSshHosts = []string{"github.com", "gitlab.com"}

// sandboxSshConfig renders the generated sandbox ssh config: one stanza per
// SandboxSshHosts entry, every stanza pointing at identityFile.
//
// It is the single source of truth for both generators — writeSshConfig
// (bwrap, sandbox-$HOME-relative generic key path) and writeSshConfigToDir
// (sandbox-exec, stable host key path). The two differ ONLY in the
// identityFile they pass; keeping the body here is what stops the two
// copies drifting.
func sandboxSshConfig(identityFile string) string {
	var b strings.Builder
	for i, host := range SandboxSshHosts {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Host " + host + "\n")
		b.WriteString("  StrictHostKeyChecking accept-new\n")
		b.WriteString("  IdentityFile " + identityFile + "\n")
		b.WriteString("  IdentitiesOnly yes\n")
	}
	return b.String()
}

// writeSshConfig generates a minimal ~/.ssh/config for the sandbox and writes
// it to a temp file. The file is later mounted at $HOME/.ssh/config:ro inside
// the sandbox, where $HOME depends on the isolation mode (see isolationMode).
//
// The config handles the forge hosts in SandboxSshHosts (github.com and
// gitlab.com); other SSH hosts are not expected inside agent sandboxes. The
// IdentityFile path is $HOME/.ssh/access-key using the sandbox-specific
// $HOME prefix, which matches the generic name used by the bwrap bind-mount
// (<hostHome>/.ssh/access-key). Both stanzas share that one key — the host
// maps github.com and gitlab.com to the same access key.
//
// Only the bwrap Prepare path calls this; sandbox-exec generates its ssh
// config into the per-session work dir instead (writeSshConfigToDir in
// session_work_dir.go) with the stable host key path.
func (m *Manager) writeSshConfig(mode isolationMode) error {
	identityFile := filepath.Join(sandboxHome(m, mode), ".ssh", "access-key")
	sshConfig := sandboxSshConfig(identityFile)
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
// Missing identity → return error, refuse to write the gitconfig. Without a
// configured [user] section, git falls back to OS-level identity guessing
// (`<sandbox-user>@<sandbox-host>`), which GitHub then aggregates into noisy
// `Co-authored-by:` trailers on squash merge.
//
// Missing signing keys → signing sections omitted, warning logged.
//
// The mode argument controls the paths embedded in the generated file:
//
//   - isolationBwrap: signingKey = <hostHome>/.ssh/signing-key.pub,
//     allowedSignersFile = <hostHome>/.ssh/allowed_signers (paths inside
//     the bwrap sandbox, where the agent runs as the host user).
//
// Note: the sandbox-exec path does not consume this writer —
// writeGitconfigToDir (session_work_dir.go) generates the work-dir gitconfig
// with stable host key paths instead.
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
// work-dir destinations, used by sandbox-exec).
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

	// [user] section — required. Missing identity is a hard error:
	// without a configured [user] section, git falls back to
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

// PrepareBwrap writes the per-session temp files (SSH config, gitconfig)
// and returns the
// complete bwrap argument list via bwrapIsolator.BuildArgs. It does NOT write
// the gitdir fixup files (prism-gitdir-*, prism-wt-gitdir-*) because bwrap
// uses Dst==Src mounts, so the host git paths are visible at their exact
// locations inside the sandbox without remapping.
//
// Call this from "prism agent-run" in the tmux pane for bwrap mode. The
// returned args slice is suitable for passing directly to exec.Exec("bwrap").
//
// The body lives in bwrapIsolator.Prepare; this method is a thin dispatcher
// that resolves the bwrap isolator from the registry and forwards to its
// Prepare method.
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
// $HOME inside the sandbox is the real host home;
// cmd/agent_run_sandbox_exec_darwin.go constructs the env after this call.
//
// The body lives in sandboxExecIsolator.Prepare; this method is a thin
// dispatcher.
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
// (~/.local/share/pi/prism-sessions/<name>/) is created. The bwrap path
// shares the host pi data dir directly and does not need a per-session dir.
func (m *Manager) prepareVolumeDirs(perSessionState bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// ── Critical directories ──────────────────────────────────────────────
	// These are unconditionally bound by the sandbox. A single
	// MkdirAll failure here is fatal — return immediately so the caller
	// sees the real cause rather than a confusing runtime abort.

	// Per-session pi state directory (created only when perSessionState is set).
	if perSessionState {
		piSessionDir := filepath.Join(home, ".local", "share", "pi", "prism-sessions", m.name)
		if err := os.MkdirAll(piSessionDir, 0o755); err != nil {
			return fmt.Errorf("container: failed to create per-session pi state dir %q: %w", piSessionDir, err)
		}
	}

	// Per-session host-API socket directory.
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

	// prism usage snapshot directory: pre-create so the RO
	// bind-mount emitted by StandardSandboxMounts is always active, even on a
	// host that has never captured a snapshot. Without this, a session
	// spawned before the first capture gets no mount (the entry is
	// OptionalIfMissing because bwrap aborts on missing bind sources), and
	// the bottom-bar usage segment would stay blank for the whole life of
	// that session even after the host wrote a snapshot. Same reasoning as
	// the clipboard staging dir above.
	//
	// Mode 0700 matches internal/usage's own dirMode: the snapshots are the
	// user's account rate-limit figures and no other host user needs to list
	// or read them. Resolved through usage.DirForHome so the pre-created
	// directory is the same one the mount source, the writer
	// (usage.DefaultDir) and the reader (prism.ts) resolve.
	//
	// Failure is logged, not fatal — the OptionalIfMissing guard then skips
	// the mount and the session still starts. That is what keeps the nix
	// build sandbox (HOME=/homeless-shelter, unwritable) working.
	if usageDir := usage.DirForHome(home); usageDir != "" {
		if err := os.MkdirAll(usageDir, 0o700); err != nil {
			log.Printf("container: failed to create usage snapshot dir %q (optional): %v", usageDir, err)
		}
	}

	// Go module cache (~/go/pkg/mod) and build cache (~/.cache/go-build):
	// pre-create so the RW binds emitted by
	// StandardSandboxMounts are always active, including on a machine that
	// has never run go outside a sandbox. Without this the entries are
	// skipped (they are OptionalIfMissing because bwrap aborts on a missing
	// bind source), go builds into the ephemeral sandbox interior, the host
	// directories are still never created — and the cache stays cold for
	// every future session too. Same reasoning as the clipboard and usage
	// dirs above.
	//
	// This is the Linux half of the pair; the Darwin half is
	// ensureGoCacheDirs, called from sandboxExecIsolator.Prepare.
	// Both walk the shared list in go_cache.go and both create through
	// createGoCacheDirs, which logs and continues — a failure here (the
	// unwritable HOME of the nix build sandbox) must not stop a session
	// starting.
	createGoCacheDirs("bwrap", goCacheDirsForGOOS(home, goosLinux))

	return nil
}
