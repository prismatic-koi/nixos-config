// Package container manages the podman container lifecycle for prism sidecar.
// This file defines bwrapIsolator, a bubblewrap-based implementation of the
// Isolator interface. It is a pure addition — nothing constructs bwrapIsolator
// at runtime yet. The wiring (CLI flag / config field) is intentionally deferred
// to a follow-up PR (#877).
package container

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// bwrapIsolator implements Isolator using bubblewrap (bwrap). It satisfies the
// interface through Run, Shutdown, HasExited, and DumpLogs. BuildRunArgs()
// returns nil (a no-op stub) because the real argument construction is
// performed by BuildArgs(m *Manager), which has access to the Manager's config
// and state.
//
// Design decision (option 2 from issue #876): the interface is not widened yet.
// BuildRunArgs() is a stub; BuildArgs is a concrete method. The widening is
// deferred to the wiring PR so this PR stays a pure addition.
type bwrapIsolator struct {
	// name is the stable session identifier (same as the container name, used
	// for log messages and process identification).
	name string

	// mu guards cmd and logBuf.
	mu     sync.Mutex
	cmd    *exec.Cmd
	logBuf strings.Builder
}

// newBwrapIsolator returns an Isolator backed by bubblewrap for the given
// session name. The returned value satisfies the Isolator interface.
func newBwrapIsolator(name string) Isolator {
	return &bwrapIsolator{name: name}
}

// BuildRunArgs satisfies the Isolator interface. It returns nil because the
// real argument construction requires Manager state and is implemented by the
// concrete BuildArgs(m *Manager) method below.
func (b *bwrapIsolator) BuildRunArgs() []string {
	return nil
}

// fallbackPATH returns the PATH value used when os.Getenv("PATH") is empty.
// It covers the per-user home-manager profile (when USER is set), the NixOS
// system profile, and standard POSIX paths.
//
// The per-user entry (/etc/profiles/per-user/<user>/bin) is prepended only
// when os.Getenv("USER") is non-empty — an empty USER would produce the
// invalid path /etc/profiles/per-user//bin.
func fallbackPATH() string {
	parts := []string{
		"/run/current-system/sw/bin",
		"/nix/var/nix/profiles/default/bin",
		"/usr/bin",
		"/bin",
	}
	user := os.Getenv("USER")
	if user != "" {
		// Per-user home-manager profile — prepend so it takes priority.
		perUser := "/etc/profiles/per-user/" + user + "/bin"
		parts = append([]string{perUser}, parts...)
	}
	return strings.Join(parts, ":")
}

// standardSandboxEnvArgs returns the --setenv pairs for the standard set of
// environment variables that must be propagated from the host into the bwrap
// sandbox. This ensures binaries resolve on PATH and tools behave correctly.
//
// Rules:
//   - PATH: always emitted; falls back to fallbackPATH when the host value is
//     empty (e.g. in a stripped environment).
//   - HOME, USER, LOGNAME, LANG, LC_ALL, SHELL: forwarded when the host has
//     them set; omitted entirely when unset, so the sandbox does not receive a
//     spurious empty string.
//
// TERM is NOT included here — it is handled separately by BuildArgs so that
// its fixed value ("xterm-256color") is always used regardless of the host.
func standardSandboxEnvArgs() []string {
	var args []string

	// PATH — always emitted, with fallback when host value is empty.
	pathVal := os.Getenv("PATH")
	if pathVal == "" {
		pathVal = fallbackPATH()
	}
	args = append(args, "--setenv", "PATH", pathVal)

	// Optional vars — only emitted when the host has them set.
	for _, key := range []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "SHELL"} {
		if val := os.Getenv(key); val != "" {
			args = append(args, "--setenv", key, val)
		}
	}

	return args
}

// BuildArgs constructs the bwrap argument list that is equivalent to the
// podman run arguments built by Manager.buildRunArgs(), translating podman
// syntax into bubblewrap equivalents:
//
//   - --volume SRC:DST:ro → --ro-bind SRC DST
//   - --volume SRC:DST[:Z] → --bind SRC DST
//   - --env K=V → --setenv K V
//   - --workdir X → --chdir X
//
// Key design decision: Dst == Src for all mounts (no /workspace remap).
// Worktrees mount at their host path directly inside the bwrap sandbox.
//
// The returned slice begins with the baseline namespace flags and ends with
// -- followed by the opencode invocation.
//
// # Mounts not included (deferred or out of scope)
//
//   - WorktreeReadOnly handling (review agents under bwrap — deferred to a
//     future PR; bwrap review support is tracked separately).
//   - Darwin Keychain credentials (darwin-only, not applicable on Linux).
func (b *bwrapIsolator) BuildArgs(m *Manager) []string {
	cfg := m.cfg

	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// ── Baseline namespace flags ────────────────────────────────────────────
	// These establish the minimal sandbox: private PID and UTS namespaces;
	// a fresh /proc and /dev; a tmpfs on /tmp; and a guarantee that the
	// sandbox dies when the parent process exits.
	//
	// --unshare-ipc is intentionally omitted: SQLite WAL mode uses a -shm
	// shared memory file that relies on mmap() coherency across processes.
	// --unshare-ipc creates a private IPC namespace which breaks that
	// coherency between concurrent bwrap sessions, causing subsequent
	// sessions to hang after DB migration completes (see issue #906).
	args := []string{
		"--unshare-pid",
		"--unshare-uts",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--die-with-parent",
	}

	// ── System binary roots (read-only, unconditional) ─────────────────────
	// These mounts make all NixOS-managed binaries reachable inside the
	// sandbox. Without them, PATH entries (even if correctly set via
	// --setenv) point at directories that simply do not exist inside the
	// sandbox namespace, causing execvp to fail with ENOENT.
	//
	// This is a fundamentally different class of mount from the
	// cfg-derived paths below: there is no filepath.EvalSymlinks pass,
	// no conditional stat(), and no Manager config field involved — these
	// are fixed system locations that must always be present on a NixOS
	// host for any sandboxed binary to resolve.
	for _, sysRoot := range []string{
		"/nix",                // Nix store — all /nix/store/… paths live here
		"/etc",                // System config + /etc/profiles/per-user/$USER symlink farm
		"/run/current-system", // Active NixOS system profile
		"/bin",                // /bin/sh — required; not provided by --proc/--dev/--tmpfs
		"/run/wrappers",       // NixOS security wrappers (sudo, ping, …)
	} {
		args = append(args, "--ro-bind", sysRoot, sysRoot)
	}

	// ── Per-user nix profiles (read-only, conditional) ──────────────────────
	// These locations hold the home-manager per-user profile (opencode,
	// git, nix, …). They are conditional because they may not exist on
	// all hosts (e.g. fresh installs, CI, Darwin). Use os.Stat — no
	// EvalSymlinks — matching the pattern for other conditional mounts.
	nixProfilePaths := []string{
		filepath.Join(home, ".nix-profile"),
		filepath.Join(home, ".local", "state", "nix", "profile"),
	}
	for _, p := range nixProfilePaths {
		if _, err := os.Stat(p); err == nil {
			args = append(args, "--ro-bind", p, p)
		}
	}

	// ── Worktree (read-write) ───────────────────────────────────────────────
	// Dst == Src: the worktree mounts at its host path inside the sandbox.
	// No /workspace remap — this is the key bwrap design difference.
	if cfg.Worktree != "" {
		args = append(args, "--bind", cfg.Worktree, cfg.Worktree)
	}

	// ── Bare repo and worktree private git state (read-write) ──────────────
	// Dst == Src: both directories mount at their host paths inside the
	// sandbox, not at the /prism-git canonical container path used by the
	// podman path. This is correct bwrap behaviour — the git commondir pointer
	// inside WorktreeGitDir/commondir already points at the absolute host path
	// for the bare dir, so no remapping is needed. PRISM_BARE_ROOT is updated
	// below to point at the actual host path rather than /prism-git.
	if cfg.BareRoot != "" && cfg.WorktreeGitDir != "" {
		bareDir := filepath.Join(cfg.BareRoot, ".bare")
		if _, err := os.Stat(bareDir); err == nil {
			args = append(args, "--bind", bareDir, bareDir)
		}
		// Worktree private git state (HEAD, index, logs, etc.) — read-write.
		if _, err := os.Stat(cfg.WorktreeGitDir); err == nil {
			args = append(args, "--bind", cfg.WorktreeGitDir, cfg.WorktreeGitDir)
		}
	}

	// ── ~/.claude (read-write) ──────────────────────────────────────────────
	claudeDir := filepath.Join(home, ".claude")
	args = append(args, "--bind", claudeDir, claudeDir)

	// ── ~/.mcp-auth (read-write, conditional) ──────────────────────────────
	mcpAuthDir := filepath.Join(home, ".mcp-auth")
	if _, err := os.Stat(mcpAuthDir); err == nil {
		args = append(args, "--bind", mcpAuthDir, mcpAuthDir)
	}

	// ── opencode session state dir (read-write) ─────────────────────────────
	opencodeSessionDir := filepath.Join(home, ".local", "share", "opencode", "prism-sessions", m.name)
	args = append(args, "--bind", opencodeSessionDir, opencodeSessionDir)

	// ── Nix daemon socket dir (read-write) ──────────────────────────────────
	// Mount the parent directory, not the socket file directly (same pattern
	// as the podman path — avoids statfs ENOTSUP on certain filesystems).
	nixDaemonSocketDir := "/nix/var/nix/daemon-socket"
	args = append(args, "--bind", nixDaemonSocketDir, nixDaemonSocketDir)

	// ── ~/.cache/nix (read-write) ────────────────────────────────────────────
	nixCacheDir := filepath.Join(home, ".cache", "nix")
	args = append(args, "--bind", nixCacheDir, nixCacheDir)

	// ── AWS readonly-config (read-only, conditional) ─────────────────────────
	// Remapped: host ~/.config/aws/readonly-config → sandbox $HOME/.aws/config
	// (the canonical path the AWS CLI reads by default). The symlink is resolved
	// to the real Nix store path so the bwrap bind-mount source exists.
	awsReadonlyConfig := filepath.Join(home, ".config", "aws", "readonly-config")
	if resolved, err := filepath.EvalSymlinks(awsReadonlyConfig); err == nil {
		args = append(args, "--ro-bind", resolved, filepath.Join(home, ".aws", "config"))
	}

	// ── Kube agents-config (read-only, conditional) ─────────────────────────
	// Remapped: host ~/.config/kube/agents-config → sandbox $HOME/.kube/config
	// (the canonical path kubectl reads by default). Only the agents-readonly
	// kubeconfig is exposed — the admin kubeconfig is never mounted.
	kubeAgentsConfig := filepath.Join(home, ".config", "kube", "agents-config")
	if resolved, err := filepath.EvalSymlinks(kubeAgentsConfig); err == nil {
		args = append(args, "--ro-bind", resolved, filepath.Join(home, ".kube", "config"))
	}

	// ── SSH keys (read-only, conditional) ───────────────────────────────────
	sshDir := filepath.Join(home, ".ssh")

	accessKeyName := cfg.SshAccessKeyName
	if accessKeyName == "" {
		accessKeyName = "prismatic-koi-ed25519"
	}
	if resolved, err := filepath.EvalSymlinks(filepath.Join(sshDir, accessKeyName)); err == nil {
		args = append(args, "--ro-bind", resolved, resolved)
	}

	signingKeyName := cfg.SshSigningKeyName
	if signingKeyName == "" {
		signingKeyName = "prismatic-koi-ed25519-signingkey"
	}
	signingKeyResolved, errPriv := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName))
	signingKeyPubResolved, errPub := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName+".pub"))
	if errPriv == nil && errPub == nil {
		args = append(args,
			"--ro-bind", signingKeyResolved, signingKeyResolved,
			"--ro-bind", signingKeyPubResolved, signingKeyPubResolved,
		)
		if m.allowedSignersReady {
			allowedSignersPath := m.allowedSignersFilePath()
			args = append(args, "--ro-bind", allowedSignersPath, allowedSignersPath)
		}
	}

	// ── known_hosts (read-only, conditional) ────────────────────────────────
	if resolved, err := filepath.EvalSymlinks(filepath.Join(sshDir, "known_hosts")); err == nil {
		args = append(args, "--ro-bind", resolved, resolved)
	}

	// ── Generated SSH config (read-only) ────────────────────────────────────
	// Remapped: generated temp file → sandbox $HOME/.ssh/config (the canonical
	// path SSH reads). The generated config contains absolute paths to the keys,
	// which are mounted at their host paths (Dst==Src), so the config resolves
	// correctly inside the sandbox.
	sshConfigPath := m.sshConfigFilePath()
	args = append(args, "--ro-bind", sshConfigPath, filepath.Join(home, ".ssh", "config"))

	// ── Generated .gitconfig (read-only) ────────────────────────────────────
	// Remapped: generated temp file → sandbox $HOME/.gitconfig (the canonical
	// path git reads for user identity, signing config, and convenience settings).
	gitconfigPath := m.gitconfigFilePath()
	args = append(args, "--ro-bind", gitconfigPath, filepath.Join(home, ".gitconfig"))

	// ── opencode.json (read-only, conditional) ──────────────────────────────
	// Remapped: generated temp file → sandbox $HOME/.config/opencode/opencode.json
	// (the canonical path opencode reads for its configuration). This ensures
	// the role-specific config (with correct model, agent identity, providers)
	// is visible inside the sandbox at the path opencode expects.
	//
	// The check is file-existence-based (os.Stat) rather than string-based
	// (cfg.ConfigContent != "") so that files written at spawn time by
	// cmd/spawn.go (via container.WriteOpencodeConfig) are picked up even when
	// this Manager instance was reconstructed by prism agent-run without
	// ConfigContent in memory (see issue #900).
	opencodeConfigPath := m.opencodeConfigFilePath()
	if _, err := os.Stat(opencodeConfigPath); err == nil {
		args = append(args, "--ro-bind", opencodeConfigPath, filepath.Join(home, ".config", "opencode", "opencode.json"))
	}

	// ── opencode config allowlist (read-only, conditional) ──────────────────
	// Mount specific files from ~/.config/opencode/ so agents, skills, and
	// plugins defined in the Nix module are available inside the sandbox.
	// opencode.json is NOT mounted from the host — the sandbox uses the temp
	// file above (ConfigContent). Matching the podman buildRunArgs allowlist.
	//
	// agents/ is excluded for review containers: the host agents/ directory
	// contains review-*.md files with "mode: subagent" front-matter that
	// overrides the "mode: primary" declaration in the container's
	// opencode.json, causing opencode to fall back to the wrong agent. Review
	// containers embed their role prompt inline via opencode.json instead.
	// For non-review containers (worker, coordinator, etc.) agents/ is mounted
	// so that @review-* subagent invocation works and all agents are accessible.
	opencodeConfigDir := filepath.Join(home, ".config", "opencode")
	opencodeAllowlist := []string{
		"AGENTS.md",
		"plugins",
		"skills",
		"command",
		"tui.json",
		".gitignore",
		"mcp-atlassian-slim-proxy.mjs",
	}
	if !strings.HasPrefix(cfg.AgentRole, "review-") {
		opencodeAllowlist = append(opencodeAllowlist, "agents")
	}
	for _, entry := range opencodeAllowlist {
		p := filepath.Join(opencodeConfigDir, entry)
		if _, err := os.Stat(p); err == nil {
			args = append(args, "--ro-bind", p, p)
		}
	}

	// ── auth.json overlay (read-write, conditional) ──────────────────────────
	// Share the host's OAuth token file across sessions. The opencode-claude-auth
	// plugin writes back refreshed credentials, so it must be read-write.
	// Mounted at the exact host path (Dst==Src).
	opencodeAuthJSON := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if _, err := os.Stat(opencodeAuthJSON); err == nil {
		args = append(args, "--bind", opencodeAuthJSON, opencodeAuthJSON)
	}

	// ── opencode plugin cache (read-only, conditional) ───────────────────────
	opencodeCacheDir := filepath.Join(home, ".cache", "opencode")
	if _, err := os.Stat(opencodeCacheDir); err == nil {
		args = append(args, "--ro-bind", opencodeCacheDir, opencodeCacheDir)
	}

	// ── bun transpiler cache (read-only, conditional) ───────────────────────
	bunCacheDir := filepath.Join(home, ".cache", "bun")
	if _, err := os.Stat(bunCacheDir); err == nil {
		args = append(args, "--ro-bind", bunCacheDir, bunCacheDir)
	}

	// ── Additional AWS mounts (read-only, conditional) ───────────────────────
	// Match the podman buildRunArgs pattern: credentials file and SSO/CLI
	// cache dirs are conditionally mounted when present on the host.
	//
	// Remapped: host ~/.config/aws/credentials → sandbox $HOME/.aws/credentials
	// (the canonical path the AWS CLI reads by default).
	awsCredentials := filepath.Join(home, ".config", "aws", "credentials")
	if resolved, err := filepath.EvalSymlinks(awsCredentials); err == nil {
		args = append(args, "--ro-bind", resolved, filepath.Join(home, ".aws", "credentials"))
	}
	awsSSOCacheDir := filepath.Join(home, ".aws", "sso")
	if _, err := os.Stat(awsSSOCacheDir); err == nil {
		args = append(args, "--bind", awsSSOCacheDir, awsSSOCacheDir)
	}
	awsCLICacheDir := filepath.Join(home, ".aws", "cli")
	if _, err := os.Stat(awsCLICacheDir); err == nil {
		args = append(args, "--bind", awsCLICacheDir, awsCLICacheDir)
	}

	// ── Clipboard staging dir (read-only, conditional) ───────────────────────
	// Images staged by `prism clipboard paste-image` on the host are placed here
	// and bind-mounted read-only so opencode's stat() call resolves without
	// modification. Dst == Src (no remap needed).
	clipboardCacheDir := filepath.Join(home, ".cache", "prism", "clipboard")
	if _, err := os.Stat(clipboardCacheDir); err == nil {
		args = append(args, "--ro-bind", clipboardCacheDir, clipboardCacheDir)
	}

	// ── Environment variables ────────────────────────────────────────────────
	// Translate --env K=V (podman) → --setenv K V (bwrap).
	// Inject the same set of env vars as the podman path.
	for _, kv := range m.credentialEnvVars() {
		k, v, _ := strings.Cut(kv, "=")
		args = append(args, "--setenv", k, v)
	}

	// NIX_CONFIG: tell nix to use the host daemon for store operations.
	args = append(args, "--setenv", "NIX_CONFIG", "store = daemon")

	// TERM: xterm-256color for full SGR mouse support in the TUI.
	args = append(args, "--setenv", "TERM", "xterm-256color")

	// Standard env vars: PATH (with fallback), HOME, USER, LOGNAME, LANG,
	// LC_ALL, SHELL. These ensure that binaries resolve correctly inside the
	// sandbox and that tools behave as they do on the host.
	args = append(args, standardSandboxEnvArgs()...)

	// Prism context variables.
	// PRISM_SPAWN_PATH: use the actual host worktree path (not /workspace).
	// In the bwrap sandbox the worktree is mounted at its host path, so
	// prism CLI commands inside the sandbox see the same absolute path the
	// host does — no remap required.
	if cfg.Worktree != "" {
		args = append(args, "--setenv", "PRISM_SPAWN_PATH", cfg.Worktree)
	} else {
		args = append(args, "--setenv", "PRISM_SPAWN_PATH", "/workspace")
	}
	// PRISM_BARE_ROOT: use the actual host bare repo root (not /prism-git).
	// Dst==Src mounts mean the bare dir is visible at its host path inside
	// the sandbox. Set PRISM_BARE_ROOT to cfg.BareRoot so that
	// resolveBareRoot finds it at the correct path.
	// TODO(#877): confirm the correct value when wiring bwrap end-to-end.
	if cfg.BareRoot != "" {
		args = append(args, "--setenv", "PRISM_BARE_ROOT", cfg.BareRoot)
	} else {
		args = append(args, "--setenv", "PRISM_BARE_ROOT", "/prism-git")
	}
	args = append(args, "--setenv", "PRISM_SESSION_NAME", cfg.SessionName)

	// OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS: 15-minute bash timeout.
	args = append(args, "--setenv", "OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS", "900000")

	// Host-API env var.
	if cfg.HostAPITCPPort != 0 {
		args = append(args,
			"--setenv", "PRISM_HOST_API",
			fmt.Sprintf("http://host.containers.internal:%d", cfg.HostAPITCPPort),
		)
	} else if cfg.HostAPISockPath != "" {
		sockDir := filepath.Dir(cfg.HostAPISockPath)
		args = append(args, "--bind", sockDir, sockDir)
		args = append(args, "--setenv", "PRISM_HOST_API", "unix://"+cfg.HostAPISockPath)
	}

	// ── Working directory ────────────────────────────────────────────────────
	// --chdir points at the worktree source path (not /workspace).
	args = append(args, "--chdir", cfg.Worktree)

	// ── Terminator: -- opencode --port <port> --hostname 127.0.0.1 ──────────
	// bwrap uses 127.0.0.1 (not 0.0.0.0): the host network namespace is shared
	// (no --unshare-net), so binding to 0.0.0.0 would be overly broad.
	args = append(args, "--",
		"opencode",
		"--port", fmt.Sprintf("%d", ContainerPort),
		"--hostname", "127.0.0.1",
	)

	if cfg.AgentRole != "" {
		args = append(args, "--agent", cfg.AgentRole)
	}
	if cfg.InitialPrompt != "" {
		args = append(args, "--prompt", cfg.InitialPrompt)
	}

	return args
}

// Run launches "bwrap <args...>" and waits for it to complete. Stdout and
// stderr are forwarded to the sidecar's stderr log. Returns a wrapped error
// on failure.
func (b *bwrapIsolator) Run(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "bwrap", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	b.mu.Lock()
	b.cmd = cmd
	b.mu.Unlock()

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container: bwrap run %q: %w", b.name, err)
	}
	return nil
}

// Shutdown sends SIGTERM to the bwrap process if it is still running. It uses
// a background context with a 30-second timeout so cleanup proceeds even after
// the parent context has been cancelled.
func (b *bwrapIsolator) Shutdown() {
	b.mu.Lock()
	cmd := b.cmd
	b.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd.Wait()
	}()

	// Send SIGTERM and wait up to 30 seconds for the process to exit.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// HasExited returns (true, exitCode) when the bwrap process has terminated,
// or (false, 0) when it is still running or has not been started.
func (b *bwrapIsolator) HasExited() (bool, int) {
	b.mu.Lock()
	cmd := b.cmd
	b.mu.Unlock()

	if cmd == nil || cmd.ProcessState == nil {
		return false, 0
	}
	if cmd.ProcessState.Exited() {
		return true, cmd.ProcessState.ExitCode()
	}
	return false, 0
}

// DumpLogs logs a message indicating that log capture is not yet implemented
// for bwrap (stdout/stderr are forwarded live to the sidecar log during Run).
func (b *bwrapIsolator) DumpLogs() {
	log.Printf("container: bwrap %q: live output was forwarded to sidecar stderr during Run", b.name)
}
