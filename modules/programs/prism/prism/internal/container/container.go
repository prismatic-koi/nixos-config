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
	Image = "prism-agent:latest"

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

	// PluginHostPath is the absolute path to the prism-hooks plugin file on the
	// host (e.g. ~/.config/opencode/plugins/prism-hooks.ts). It is mounted
	// read-only into the container's opencode plugin directory.
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

	// HealthCheckTimeout overrides DefaultHealthCheckTimeout when non-zero.
	HealthCheckTimeout time.Duration

	// HTTPClient is used for health-check probes. Defaults to a short-timeout
	// client when nil.
	HTTPClient *http.Client
}

// containerName returns the stable podman container name for a session.
// The name is derived from the session name with "@" replaced by "-" and
// a "prism-" prefix, e.g. "prism-nixos-config-feature".
func containerName(sessionName string) string {
	safe := strings.ReplaceAll(sessionName, "@", "-")
	safe = strings.ReplaceAll(safe, "/", "-")
	safe = strings.ReplaceAll(safe, ".", "-")
	return "prism-" + safe
}

// Manager manages the lifecycle of a single podman container for a session.
type Manager struct {
	cfg            Config
	name           string
	healthCheckURL string
	httpClient     *http.Client
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

// EnsureRemoved stops and removes any existing container with the same name.
// It is safe to call when no such container exists — errors for "no such
// container" are silently ignored.
func (m *Manager) EnsureRemoved(ctx context.Context) {
	// Stop the container (ignore errors — may not be running).
	stopCmd := exec.CommandContext(ctx, "podman", "stop", "--time", "10", m.name)
	if out, err := stopCmd.CombinedOutput(); err != nil {
		// Only log if it looks like a real error (not "no such container").
		if !isNoSuchContainer(string(out)) {
			log.Printf("container: stop existing %q: %v — %s", m.name, err, strings.TrimSpace(string(out)))
		}
	}

	// Remove the container (ignore errors — may not exist).
	rmCmd := exec.CommandContext(ctx, "podman", "rm", "--force", m.name)
	if out, err := rmCmd.CombinedOutput(); err != nil {
		if !isNoSuchContainer(string(out)) {
			log.Printf("container: rm existing %q: %v — %s", m.name, err, strings.TrimSpace(string(out)))
		}
	}
}

// Create creates and starts the podman container for this session.
// It first calls EnsureRemoved to handle any stale container with the same name.
// Returns an error if podman create or start fails.
func (m *Manager) Create(ctx context.Context) error {
	// Remove any stale container first (AC-15).
	m.EnsureRemoved(ctx)

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
	if out, err := stopCmd.CombinedOutput(); err != nil && !isNoSuchContainer(string(out)) {
		log.Printf("container: stop %q: %v — %s", m.name, err, strings.TrimSpace(string(out)))
	}

	rmCmd := exec.CommandContext(ctx, "podman", "rm", "--force", m.name)
	if out, err := rmCmd.CombinedOutput(); err != nil && !isNoSuchContainer(string(out)) {
		log.Printf("container: rm %q: %v — %s", m.name, err, strings.TrimSpace(string(out)))
	}

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
	opencodeConfigMount := filepath.Join(home, ".config", "opencode") +
		":/root/.config/opencode:ro"

	args := []string{
		"run",
		"--detach",
		"--name", m.name,

		// Network: slirp4netns (rootless default on Linux) provides outbound
		// NAT via the host's network, but the container cannot reach host
		// loopback services or other containers directly. Declaring it explicitly
		// makes the network policy declarative rather than relying on the
		// podman default — satisfying AC-12.
		"--network", "slirp4netns",

		// Port — bound to localhost only (AC-6).
		"--publish", portBinding,

		// Worktree read-write.
		"--volume", worktreeMount,
		// opencode state — shared with host, read-write.
		"--volume", opencodeStateMount,
		// opencode config — read-only.
		"--volume", opencodeConfigMount,

		// Work inside the worktree by default.
		"--workdir", "/workspace",
	}

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
	// GIT_DIR is set to /prism-git/worktrees/<branch> so git uses that dir
	// for HEAD/index/etc. without touching the absolute .git file on disk.
	if cfg.BareRoot != "" && cfg.WorktreeGitDir != "" {
		branch := filepath.Base(cfg.WorktreeGitDir)
		// Bare repo — read-write so git can write new objects on commit.
		bareMount := filepath.Join(cfg.BareRoot, ".bare") + ":/prism-git:Z"
		// Worktree private state (HEAD, index, logs, etc.) — read-write.
		worktreeGitMount := cfg.WorktreeGitDir + ":/prism-git/worktrees/" + branch + ":Z"
		args = append(args,
			"--volume", bareMount,
			"--volume", worktreeGitMount,
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

	// Git author identity — forwarded so git commits inside the container have
	// proper attribution.
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		if v := os.Getenv(k); v != "" {
			vars = append(vars, k+"="+v)
		}
	}

	// GIT_DIR override — injected when the bare+worktree layout is used so
	// that git inside the container uses the mounted worktree private state
	// at /prism-git/worktrees/<branch> instead of following the absolute host
	// path stored in the worktree's .git file.
	if m.cfg.BareRoot != "" && m.cfg.WorktreeGitDir != "" {
		branch := filepath.Base(m.cfg.WorktreeGitDir)
		vars = append(vars, "GIT_DIR=/prism-git/worktrees/"+branch)
	}
	// Note: GIT_COMMON_DIR is intentionally NOT injected. Although the original
	// issue called for it, testing showed it breaks ref lookup in the git version
	// used in the container image — git cannot resolve branch refs via rev-parse
	// when GIT_COMMON_DIR is set. The commondir file in the worktree private state
	// directory already performs the same function via a relative path ("../.."),
	// making the env var redundant and harmful.

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

// isNoSuchContainer returns true when the podman output indicates the container
// does not exist (so the error can be silently ignored).
func isNoSuchContainer(output string) bool {
	return strings.Contains(output, "no such container") ||
		strings.Contains(output, "No such container") ||
		strings.Contains(output, "Error: no such container") ||
		strings.Contains(output, "Error response from daemon: No such container")
}
