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
//   - HOME, USER, LOGNAME, LANG, LC_ALL: forwarded when the host has them
//     set; omitted entirely when unset, so the sandbox does not receive a
//     spurious empty string.
//   - SHELL: forced to /bin/sh regardless of the host value. The host's
//     SHELL is typically zsh, and NixOS' /etc/zshenv unconditionally sources
//     set-environment which runs `cat /run/secrets/...` for every sops secret.
//     Inside bwrap those paths don't exist, so the cat fails AND (critically)
//     overwrites any --setenv'd GITHUB_TOKEN with an empty string — breaking
//     every `git push` and `gh` call the agent tries. /bin/sh on NixOS is a
//     symlink to bash; bash's non-interactive -c invocation does not source
//     /etc/profile (login-shell only) or /etc/bashrc (interactive-only), so
//     the injected env survives. We pin /bin/sh (not /bin/bash) because
//     /bin/ inside the sandbox only contains the NixOS-provided /bin/sh
//     symlink — bash itself lives in the Nix store at a hash-prefixed path,
//     not at /bin/bash. Pinning SHELL to /bin/sh means any tool that uses
//     $SHELL (opencode's bash tool, most TUIs) gets a clean shell that
//     doesn't wipe credentials.
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
	for _, key := range []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL"} {
		if val := os.Getenv(key); val != "" {
			args = append(args, "--setenv", key, val)
		}
	}

	// SHELL — forced to /bin/sh. See godoc above for the zsh/set-environment
	// reason. /bin/sh is always available inside the sandbox (NixOS ships
	// /bin/sh as a symlink to bash) because BuildArgs ro-binds /bin from
	// the host.
	args = append(args, "--setenv", "SHELL", "/bin/sh")

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
	// --clearenv (first) wipes the inherited environment so that nothing from
	// the invoking shell leaks into the sandbox. Every var the sandbox needs
	// must be re-introduced via an explicit --setenv below (see
	// standardSandboxEnvArgs and credentialEnvVars). Without --clearenv, bwrap
	// forwards the full host environment — which on a prism coordinator
	// includes role-specific GitHub tokens (PRISM_GITHUB_TOKEN_*) and other
	// credentials that must never reach a sandboxed agent. Podman does not
	// have this problem because its default is to pass nothing from the host;
	// --clearenv gives bwrap the same baseline.
	//
	// --unshare-ipc is intentionally omitted: SQLite WAL mode uses a -shm
	// shared memory file that relies on mmap() coherency across processes.
	// --unshare-ipc creates a private IPC namespace which breaks that
	// coherency between concurrent bwrap sessions, causing subsequent
	// sessions to hang after DB migration completes (see issue #906).
	args := []string{
		"--clearenv",
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

	// ── opencode shared state dir (read-write) ─────────────────────────────
	// bwrap sessions share the host's ~/.local/share/opencode/ directly: a
	// single SQLite DB across all sessions (host mode, bwrap mode, and the
	// non-prism opencode CLI). The per-session isolation used by the podman
	// path exists solely to work around a virtiofs WAL-mode locking issue on
	// Darwin — that path doesn't apply here because bwrap is Linux-only and
	// SQLite WAL works correctly across concurrent processes on a shared
	// filesystem.
	opencodeDataDir := filepath.Join(home, ".local", "share", "opencode")
	args = append(args, "--bind", opencodeDataDir, opencodeDataDir)

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
	// All SSH artefacts are remapped to canonical generic paths under
	// $HOME/.ssh/ inside the sandbox. Agents inside both bwrap and podman
	// sandboxes see the same filenames (access-key, signing-key, signing-key.pub,
	// allowed_signers, known_hosts) — only the $HOME prefix differs (/root for
	// podman, <hostHome> for bwrap). The generated ~/.ssh/config and
	// ~/.gitconfig (see writeSshConfig and writeGitconfig) reference these
	// canonical paths using the same prefix via sandboxHome().
	sshDir := filepath.Join(home, ".ssh")

	accessKeyName := cfg.SshAccessKeyName
	if accessKeyName == "" {
		accessKeyName = "prismatic-koi-ed25519"
	}
	if resolved, err := filepath.EvalSymlinks(filepath.Join(sshDir, accessKeyName)); err == nil {
		args = append(args, "--ro-bind", resolved, filepath.Join(sshDir, "access-key"))
	}

	signingKeyName := cfg.SshSigningKeyName
	if signingKeyName == "" {
		signingKeyName = "prismatic-koi-ed25519-signingkey"
	}
	signingKeyResolved, errPriv := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName))
	signingKeyPubResolved, errPub := filepath.EvalSymlinks(filepath.Join(sshDir, signingKeyName+".pub"))
	if errPriv == nil && errPub == nil {
		args = append(args,
			"--ro-bind", signingKeyResolved, filepath.Join(sshDir, "signing-key"),
			"--ro-bind", signingKeyPubResolved, filepath.Join(sshDir, "signing-key.pub"),
		)
		if m.allowedSignersReady {
			args = append(args, "--ro-bind", m.allowedSignersFilePath(), filepath.Join(sshDir, "allowed_signers"))
		}
	}

	// ── known_hosts (read-only, conditional) ────────────────────────────────
	if resolved, err := filepath.EvalSymlinks(filepath.Join(sshDir, "known_hosts")); err == nil {
		args = append(args, "--ro-bind", resolved, filepath.Join(sshDir, "known_hosts"))
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
//
// The bwrap child process runs with a filtered environment (see
// minimalBwrapExecEnv) so that secrets from the invoking process do not
// appear in the bwrap process's own /proc/<pid>/environ. This pairs with
// the --clearenv flag in the baseline args (see BuildArgs) which wipes
// the sandbox interior env. Defence-in-depth: either layer alone would
// block the leak; both are kept so regressions in one layer are caught
// by the other.
func (b *bwrapIsolator) Run(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "bwrap", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = minimalBwrapExecEnv(os.Environ())

	b.mu.Lock()
	b.cmd = cmd
	b.mu.Unlock()

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container: bwrap run %q: %w", b.name, err)
	}
	return nil
}

// minimalBwrapExecEnv filters a hostEnv slice (K=V pairs, as returned by
// os.Environ()) down to a minimal allow-list that the bwrap process itself
// needs. The returned env is what bwrap's /proc/<pid>/environ contains; it
// is NOT the sandbox interior env (bwrap starts the sandbox with --clearenv
// and rebuilds the interior env from explicit --setenv pairs in BuildArgs).
//
// See cmd/agent_run.go for the corresponding filter at the syscall.Exec
// call site. Both filters use the same allow-list; keep them in sync.
func minimalBwrapExecEnv(hostEnv []string) []string {
	allow := map[string]bool{
		"PATH":    true,
		"HOME":    true,
		"USER":    true,
		"LOGNAME": true,
		"TERM":    true,
		"LANG":    true,
		"LC_ALL":  true,
	}
	out := make([]string, 0, len(allow))
	for _, kv := range hostEnv {
		eq := -1
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				eq = i
				break
			}
		}
		if eq <= 0 {
			continue
		}
		if allow[kv[:eq]] {
			out = append(out, kv)
		}
	}
	return out
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
