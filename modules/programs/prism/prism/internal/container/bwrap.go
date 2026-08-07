// Package container manages sandbox lifecycle and mount preparation for
// prism agent sessions.
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

	"github.com/prismatic-koi/prism/internal/config"
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

// Name returns config.IsolationBwrap — the registry key for this isolator.
func (b *bwrapIsolator) Name() config.IsolationMode {
	return config.IsolationBwrap
}

// Capabilities returns the bwrap feature flags:
//   - NeedsConfigBlob: config blob must be written to disk before agent-run.
//   - NeedsHostAPISocket: the sidecar binds the host-API socket for in-sandbox proxy calls.
//   - RestartOnExit: the sidecar restart-loop fires to relaunch agent-run on exit.
//   - NeedsStartupConnectTimeout: bwrap-specific startup-connect timeout in the sidecar.
func (b *bwrapIsolator) Capabilities() Capabilities {
	return Capabilities{
		IsContainer:                false,
		OwnsContainerLifecycle:     false,
		NeedsConfigBlob:            true,
		NeedsHostAPISocket:         true,
		UsesContainerHarness:       false,
		RestartOnExit:              true,
		NeedsStartupConnectTimeout: true,
		NeedsReadinessWait:         false,
		EmitsTmuxStatusColumns:     false,
	}
}

// BuildRunArgs satisfies the Isolator interface. It returns nil because the
// real argument construction requires Manager state and is implemented by the
// concrete BuildArgs(m *Manager) method below.
func (b *bwrapIsolator) BuildRunArgs() []string {
	return nil
}

// bwrapKubeCacheDir is the in-sandbox KUBECACHEDIR value for bwrap sessions:
// kubectl's cache lands on the per-session /tmp tmpfs (ephemeral, never
// host-visible). See the KUBECACHEDIR comment in BuildArgs (issue #2235).
const bwrapKubeCacheDir = "/tmp/kube-cache"

// bwrapGlabConfigDir is the in-sandbox glab (GitLab CLI) config directory
// for bwrap sessions. glab defaults to $HOME/.config/glab-cli and aborts
// with "failed to read configuration" when that path is unreadable; no such
// path exists inside the bwrap namespace. GLAB_CONFIG_DIR points glab at the
// per-session /tmp tmpfs instead, so its config stays ephemeral and is never
// written through to the host — the bwrap equivalent of sandbox-exec's
// <sessionDir>/glab-cli redirect (issue #2668).
const bwrapGlabConfigDir = "/tmp/glab-cli"

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
//     $SHELL (the agent bash tool, most TUIs) gets a clean shell that
//     doesn't wipe credentials.
//
// TERM is NOT included here — it is handled separately by BuildArgs, where
// the host's TERM is passed through verbatim (falling back to xterm-256color
// only when TERM is unset on the host). COLORTERM is similarly handled there.
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

// BuildArgs constructs the bwrap argument list for the session sandbox:
// bind-mounts (--ro-bind / --bind), environment injection (--setenv), and
// namespace flags.
//   - --workdir X → --chdir X
//
// Key design decision: Dst == Src for all mounts (no /workspace remap).
// Worktrees mount at their host path directly inside the bwrap sandbox.
//
// The returned slice begins with the baseline namespace flags and ends with
// -- followed by the agent invocation.
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
	// credentials that must never reach a sandboxed agent. --clearenv gives
	// bwrap a pass-nothing-from-the-host baseline.
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

	// ── Security: shadow sensitive /etc subtrees with empty tmpfs ──────────
	// The /etc ro-bind above exposes all of /etc to the sandbox, including
	// directories that contain secrets agents have no legitimate need for.
	// bwrap applies mounts in order, so a --tmpfs placed after the /etc
	// bind-mount shadows that subtree with an empty, unwritable tmpfs inside
	// the sandbox. The host filesystem is unaffected.
	//
	// /etc/wireguard/ — WireGuard interface configs contain private keys,
	// peer endpoints, and DNS in plaintext. An agent in possession of the
	// private key could reconstruct the full VPN tunnel or exfiltrate it.
	//
	// /etc/wpa_supplicant/ — wpa_supplicant configs may contain Wi-Fi
	// credentials and network identifiers. Agents have no need for these.
	//
	// /etc/ssh/ — contains SSH host private keys (ssh_host_*_key) and
	// system-wide ssh_config. On NixOS the system ssh_config includes a
	// nobody-owned systemd drop-in via Include; OpenSSH enforces strict
	// ownership and rejects it, causing git push over SSH to fail. Shadowing
	// the entire subtree is safe: the generated ~/.ssh/config already
	// provides all the SSH configuration an agent needs.
	//
	// The --tmpfs mounts are conditional on the directory existing on the
	// host: bwrap requires the mount-point to already exist inside the
	// namespace (the /etc ro-bind makes the host tree visible, so the check
	// is a plain os.Stat on the host path). On machines where these
	// directories were never created (e.g. wgnord is disabled and
	// impermanence has not yet run), the mount is simply omitted — there
	// are no secrets to shadow in that case.
	for _, sensitiveEtcDir := range []string{
		"/etc/wireguard",
		"/etc/wpa_supplicant",
		"/etc/ssh", // systemd drop-ins are nobody-owned; OpenSSH rejects them
	} {
		if _, err := os.Stat(sensitiveEtcDir); err == nil {
			args = append(args, "--tmpfs", sensitiveEtcDir)
		}
	}

	// ── Per-user nix profiles (read-only, conditional) ──────────────────────
	// These locations hold the home-manager per-user profile (pi,
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
	// sandbox (no canonical-path remapping). This is correct bwrap behaviour
	// — the git commondir pointer
	// inside WorktreeGitDir/commondir already points at the absolute host path
	// for the bare dir, so no remapping is needed. PRISM_BARE_ROOT is updated
	// below to point at the actual host path rather than /prism-git.
	if cfg.BareRoot != "" && cfg.WorktreeGitDir != "" {
		bareDir := filepath.Join(cfg.BareRoot, ".bare")
		if _, err := os.Stat(bareDir); err == nil {
			args = append(args, "--bind", bareDir, bareDir)
		}
		// Worktree private git state (HEAD, index, logs, etc.) — read-write.
		// A missing directory here is a real error, not a silent skip: as of
		// issue #2518 WorktreeGitDir is resolved from the worktree's own
		// authoritative .git gitdir pointer, so if it does not exist on disk
		// something is genuinely wrong (e.g. corrupted worktree bookkeeping)
		// and the sandbox must not silently start without the agent's git
		// state bound in. BuildArgs cannot return an error, so the error is
		// stashed on the Manager and surfaced by Prepare.
		if _, err := os.Stat(cfg.WorktreeGitDir); err == nil {
			args = append(args, "--bind", cfg.WorktreeGitDir, cfg.WorktreeGitDir)
		} else {
			m.worktreeGitDirErr = fmt.Errorf("worktree git dir %q: %w", cfg.WorktreeGitDir, err)
		}
	}

	// ── Standard sandbox mounts via the shared spec walk ─────────────────
	// StandardSandboxMounts (mounts.go, A2.M1) returns the mode-agnostic
	// mount set for ~/.config/claude, ~/.mcp-auth, ~/.cache/nix, AWS config, AWS
	// credentials, AWS SSO/CLI cache, kube config, the clipboard
	// staging dir, prism profiles.json, and the prism usage snapshot dir
	// (issue #2572). The bwrap appendBind appender (mounts.go) translates
	// each MountSpec into the correct --bind / --ro-bind triple.
	//
	// Note on ordering: StandardSandboxMounts emits ~/.config/claude, ~/.mcp-auth,
	// ~/.cache/nix, ~/.cache/bun, AWS config, AWS credentials, AWS SSO, AWS CLI,
	// kube config, clipboard-staging. The pre-A2.M1 bwrap order interleaved the
	// AWS group with the SSH/known_hosts/SSH-config group; the new order
	// keeps the AWS entries adjacent and emits all of them before the SSH
	// keys. Bwrap argument order does not affect the runtime mount layout
	// (bwrap evaluates arguments left-to-right but each --bind is
	// independent), so this is a behaviour-preserving reordering.
	for _, spec := range StandardSandboxMounts(cfg, home, home, isolationBwrap) {
		args = AppendBwrapBind(args, spec)
	}

	// NOTE: An unconditional --bind of ~/.local/share/pi used to live here as
	// part of the opencode→pi rename in #1609. PI does NOT use that XDG path —
	// it lives at ~/.pi/agent/ (see internal/harness/pi/archive.go). The mount
	// was dead and broke fresh installs where the source directory did not
	// exist (bwrap aborts on missing --bind sources). Removed locally to
	// unblock `nh switch`; proper cleanup of the sandbox_exec.go SBPL rules,
	// container.go per-session subdir, and dispatch.go archiveSharedStorageRoot
	// to follow in a PR.

	// ── Nix daemon socket dir (read-write) ──────────────────────────────────
	// Mount the parent directory, not the socket file directly
	// (avoids statfs ENOTSUP on certain filesystems).
	// Kept inline because the absolute system path (not HOME-relative) is
	// outside the StandardSandboxMounts shape.
	nixDaemonSocketDir := "/nix/var/nix/daemon-socket"
	args = append(args, "--bind", nixDaemonSocketDir, nixDaemonSocketDir)

	// ── SSH keys (read-only, conditional) ───────────────────────────────────
	// All SSH artefacts are remapped to canonical generic paths under
	// $HOME/.ssh/ inside the sandbox. Agents inside both bwrap and
	// sandbox-exec sandboxes see the same filenames (access-key, signing-key,
	// signing-key.pub, allowed_signers, known_hosts) — only the $HOME prefix
	// differs. The generated ~/.ssh/config and
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
	// Note on sops rotation and Linux bind-mount inode semantics (issue #1412):
	//
	// On NixOS, the signing key is managed by sops-nix. The symlink chain is:
	//   ~/.ssh/prismatic-koi-ed25519-signingkey
	//     → /run/secrets/ssh/prismatic-koi-ed25519-signingkey   (stable sops intermediate)
	//     → /run/secrets.d/<N>/ssh/prismatic-koi-ed25519-signingkey  (concrete sops file)
	//
	// sops-nix's pruneGenerations removes old secrets.d/<N>/ directories on
	// nixos-rebuild switch (keepGenerations = 1 by default). This raised the
	// question of whether EvalSymlinks here is fragile — mirroring the Darwin
	// sandbox-exec fix in PR #1411.
	//
	// On Linux, bwrap bind-mounts are inode-based, not path-based:
	//   1. filepath.EvalSymlinks resolves to /run/secrets.d/<N>/ssh/signingkey.
	//   2. The kernel bind-mount (MS_BIND) increments the inode's reference count.
	//   3. When pruneGenerations removes secrets.d/<N>/, it unlinks the directory
	//      entries but cannot free the inodes as long as any reference exists.
	//   4. The bwrap sandbox's bind mount holds that reference — the file remains
	//      readable inside the sandbox for the duration of the session.
	//   5. After the session exits and the bind mount is released, the inode is
	//      freed (refcount drops to 0). No memory leak.
	//
	// For new sessions spawned after a rotation, filepath.EvalSymlinks resolves
	// to the new /run/secrets.d/<N+1>/ path — so new sessions are always correct.
	//
	// This is fundamentally different from the Darwin/sandbox-exec case: SBPL
	// profile rules are path-based (evaluated at each file access), so a stale
	// concrete path in the profile causes every access to fail after rotation.
	// bwrap bind mounts are inode-based, so they survive the rotation cleanly.
	//
	// EvalSymlinks is therefore the correct approach here — no fix is needed.
	// The historical sandbox-exec symlink-staging fix (symlinkIfExists vs
	// symlinkIfResolvable, #1410/#1573) did not apply to bwrap for this reason;
	// the staging mechanism itself was deleted in Step 5 of #2132.
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

	// ── pi grafana MCP secret bundle (RO, EvalSymlinks, Src≠Dst, conditional) ─
	// The pi grafana extension reads its selected sops-decrypted config
	// bundle from the file path delivered via the GRAFANA_MCP_CONFIG_PATH
	// env var (populated by nx.programs.prism.pi.grafana in
	// modules/programs/prism/default.nix). Because prism's Go isolators
	// inject agent.envVars values verbatim with no shell in the loop, the
	// env var carries a plain absolute host path — typically the sops-nix
	// symlink `/run/secrets/<name>` on Linux — and the extension calls
	// readFileSync(process.env.GRAFANA_MCP_CONFIG_PATH) inside the sandbox.
	//
	// Src≠Dst is load-bearing here: the env-var value IS the sops symlink,
	// but /run/secrets/ is not part of any other bind, so the symlink path
	// only exists inside the sandbox because THIS bind creates it. Emit
	// Src = EvalSymlinks(sops symlink) = /run/secrets.d/<N>/<name>
	// (concrete file), Dst = the env-var path itself. bwrap auto-creates
	// the mount point and its parent directory. Same shape as the AWS /
	// kube XDG binds in mounts.go: Src is the sops-resolved concrete file;
	// Dst is the stable path the env var references. Binding Dst==Src
	// (as an earlier iteration of this block did) leaves the extension's
	// env-var path unreachable inside the sandbox — readFileSync ENOENTs
	// and no tools ever register.
	//
	// EvalSymlinks pins the sops-nix concrete /run/secrets.d/<N>/<name>
	// path at bind time; because bwrap bind mounts are inode-based on
	// Linux, a sops rotation mid-session keeps the bound file readable —
	// same rotation-safety argument as the SSH signing key bind above.
	//
	// Absent / unresolvable path silently skips the bind; the extension
	// fails-gracefully with a ctx.ui.notify on the pi side (per issue
	// #2452 edge-case AC).
	if grafanaPath := cfg.AgentEnvVars["GRAFANA_MCP_CONFIG_PATH"]; grafanaPath != "" {
		if resolved, err := filepath.EvalSymlinks(grafanaPath); err == nil {
			args = append(args, "--ro-bind", resolved, grafanaPath)
		}
	}

	// AWS SSO cache, AWS CLI cache, kube config, the bun transpiler cache,
	// and the clipboard staging dir are all emitted earlier in this function
	// via the StandardSandboxMounts walk (A2.M1) — the inline blocks that
	// previously duplicated those mounts here have been removed (the bun
	// cache block was converged in #2245, Step 3e of #2132). The aws
	// config/credentials canonical-path ($HOME/.aws/*) binds were dropped in
	// #2234; the files are now bound Dst==Src at the host XDG paths and
	// reached via the AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE env vars
	// pointing there.

	// ── Environment variables ────────────────────────────────────────────────
	// Inject the per-session env vars as --setenv K V pairs.
	// credentialEnvVars returns an error only when a configured GitHub token
	// file path is unreadable — that is a hard failure per issue #2348 (the
	// spawn must NOT silently proceed with a broken/missing token).  BuildArgs
	// cannot return an error, so we stash it on the Manager and surface it in
	// Prepare, following the same pattern as m.piBwrapErr.
	credEnv, credErr := m.credentialEnvVars()
	if credErr != nil {
		m.credentialsErr = credErr
	}
	for _, kv := range credEnv {
		k, v, _ := strings.Cut(kv, "=")
		args = append(args, "--setenv", k, v)
	}

	// Inject profile-level agent env vars (AgentEnvVars, with KUBECONFIG
	// suppressed) and harness-specific runtime env vars
	// (RuntimeEnv). The bwrap appender uses "--setenv K V" syntax (distinct
	// argv elements — no shell quoting needed; special characters are verbatim).
	// See env.go:AppendStandardEnv for the suppression rationale.
	args = AppendStandardEnv(args, cfg, func(a []string, k, v string) []string {
		return append(a, "--setenv", k, v)
	})

	// NIX_CONFIG: tell nix to use the host daemon for store operations.
	args = append(args, "--setenv", "NIX_CONFIG", "store = daemon")

	// KUBECACHEDIR: redirect kubectl's discovery/http cache to the sandbox's
	// private /tmp tmpfs (issue #2235, Step 3b of #2132). With the canonical
	// $HOME/.kube/config bind gone, no kube path exists in-namespace and
	// kubectl's default $HOME/.kube/cache would land wherever the root tmpfs
	// happens to permit. /tmp is an explicit per-session tmpfs (--tmpfs /tmp
	// in the baseline flags), so the cache is guaranteed writable, ephemeral,
	// and never written through to the host — the bwrap equivalent of
	// sandbox-exec's <sessionDir>/kube-cache redirect (the session work dir
	// is a sandbox-exec mechanism; bwrap deliberately does not materialise
	// host-side per-session state for a throwaway cache). kubectl MkdirAll's
	// the directory on first use.
	args = append(args, "--setenv", "KUBECACHEDIR", bwrapKubeCacheDir)

	// GLAB_CONFIG_DIR: redirect glab's config dir to the sandbox's private
	// /tmp tmpfs (issue #2668). glab reads its config on EVERY invocation and
	// exits non-zero when the read fails, so without this redirect glab is
	// unusable inside the sandbox. The GitLab credential itself arrives as
	// the GITLAB_TOKEN value injected by credentialEnvVars — never from a
	// config file. glab MkdirAll's the directory on first use.
	args = append(args, "--setenv", "GLAB_CONFIG_DIR", bwrapGlabConfigDir)

	// TERM: pass through the host's TERM so that the sandbox sees the same
	// terminal type as the tmux pane (e.g. tmux-256color). In bwrap mode the
	// host terminfo tree is already bind-mounted (/etc, /nix, /run/current-system),
	// so any terminfo entry the host has is resolvable inside the sandbox.
	// Fallback to xterm-256color only if TERM is unset on the host (shouldn't
	// happen from a tmux pane, but guard anyway).
	{
		termVal := os.Getenv("TERM")
		if termVal == "" {
			termVal = "xterm-256color"
		}
		args = append(args, "--setenv", "TERM", termVal)
	}

	// COLORTERM: pass through the host's COLORTERM when set so that TUI
	// libraries receive the truecolor signal. Only injected when the host
	// has it set — no fabricated value is inserted when it is absent.
	if colorterm := os.Getenv("COLORTERM"); colorterm != "" {
		args = append(args, "--setenv", "COLORTERM", colorterm)
	}

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

	// Host-API env var.
	if cfg.HostAPITCPPort != 0 {
		args = append(args,
			"--setenv", "PRISM_HOST_API",
			fmt.Sprintf("http://host.containers.internal:%d", cfg.HostAPITCPPort),
		)
	} else if cfg.HostAPISockPath != "" {
		// Bind the session's own per-session socket DIRECTORY (not the individual
		// socket file and not the shared run/ directory). Security fix #960:
		// SidecarHostAPIPath now places each session's socket in its own
		// subdirectory (run/<session>/hostapi.sock), so binding only that
		// directory means the sandbox cannot see other sessions' sockets.
		//
		// A directory bind (not file bind) is required here: the sidecar
		// creates the socket file after startup,
		// and a file-level bind would pin the original inode — meaning the sandbox
		// would not see the new socket after os.Remove + net.Listen replaced it.
		// Binding the directory makes the socket file appear inside the sandbox
		// once the sidecar calls net.Listen (same inode-transparency behaviour as
		// a directory mount). The per-session directory is pre-created by
		// prepareVolumeDirs before this code runs.
		//
		// P2.SIDECAR: the harness pipe socket (pipe.sock) co-locates with the
		// host-API socket in the same per-session directory. This single bind-mount
		// covers both sockets — no additional bind-mount is needed.
		sockDir := filepath.Dir(cfg.HostAPISockPath)
		args = append(args, "--bind", sockDir, sockDir)
		args = append(args, "--setenv", "PRISM_HOST_API", "unix://"+cfg.HostAPISockPath)
	}

	// Harness pipe env var (P2.SIDECAR #1209).
	// On Linux the pipe socket lives in the same per-session directory as the
	// host-API socket, so the bind-mount above already exposes it. No extra
	// bind is needed. On Darwin (HarnessPipeTCPPort != 0), TCP is used instead.
	if cfg.HarnessPipeTCPPort != 0 {
		args = append(args,
			"--setenv", "PRISM_HARNESS_PIPE",
			fmt.Sprintf("tcp://host.containers.internal:%d", cfg.HarnessPipeTCPPort),
		)
	} else if cfg.HarnessPipeSockPath != "" {
		args = append(args, "--setenv", "PRISM_HARNESS_PIPE", "unix://"+cfg.HarnessPipeSockPath)
	}

	// Durable give-up diagnostics (#2357): expose the host-side agent-run log
	// path so the PI prism extension can append a diagnostic line if it
	// exhausts its first-connect retries. The log lives in the per-session
	// run directory that the PRISM_HOST_API block above bind-mounts at its
	// host path, so no extra bind is needed.
	if cfg.AgentRunLogPath != "" {
		args = append(args, "--setenv", "PRISM_AGENT_RUN_LOG", cfg.AgentRunLogPath)
	}

	// ── Containers-enabled surface (#2317 / #2321) ───────────────────
	// Wires the per-session FILTERED podman socket and the writable
	// container-scratch dir into the sandbox. Three argv emissions are
	// gated on cfg.ContainersEnabled:
	//
	//   --setenv CONTAINER_HOST unix://<PodmanProxySockPath>
	//   --setenv DOCKER_HOST    unix://<PodmanProxySockPath>   (docker-CLI compat)
	//   --bind   <sessionDir>/container-scratch <sessionDir>/container-scratch
	//
	// Critical security invariant (#2321 greppable AC): the REAL upstream
	// podman socket path ($XDG_RUNTIME_DIR/podman/podman.sock or whatever
	// the sidecar resolved) must NEVER appear in the rendered argv — only
	// the filtered PodmanProxySockPath the sidecar's proxy goroutine
	// listens on. The proxy is the agent's only container API surface; a
	// stray --bind of the upstream socket would let the agent `connect()`
	// directly and bypass every default-deny policy in
	// internal/podmanproxy. Do not add any code path here that reads or
	// references $XDG_RUNTIME_DIR or any host-side podman socket path.
	//
	// No new bind-mount is needed for the proxy socket itself: it lives in
	// the per-session run directory that the PRISM_HOST_API block above
	// already binds (HostAPISockPath's parent dir), so the proxy socket
	// file appears inside the sandbox the moment the sidecar's listener
	// creates it.
	if cfg.ContainersEnabled && cfg.PodmanProxySockPath != "" {
		containerHost := "unix://" + cfg.PodmanProxySockPath
		args = append(args, "--setenv", "CONTAINER_HOST", containerHost)
		args = append(args, "--setenv", "DOCKER_HOST", containerHost)

		// The bind source is <sessionDir>/container-scratch where
		// <sessionDir> = SessionWorkDirPath(InstanceID) (or the manager's
		// fallback when InstanceID is empty). bwrapIsolator.Prepare has
		// already validated the sessionDir resolves and mkdir'd the scratch
		// subdir; if resolution fails here we record the error on the
		// Manager so Prepare can surface it (BuildArgs has no error
		// return). Mirrors the appendPIBwrapMounts error-stashing pattern.
		if sessionDir, err := m.sessionWorkDirPath(); err == nil {
			scratch := SessionWorkDirContainerScratchPath(sessionDir)
			args = append(args, "--bind", scratch, scratch)
		} else {
			m.piBwrapErr = fmt.Errorf("resolve session work dir for container-scratch: %w", err)
		}
	}

	// ── PI-specific bind mounts (harness=pi only) ────────────────────────
	// Note: the host's ~/.pi/agent/sessions/ directory is now overlaid onto
	// $PI_CODING_AGENT_DIR/sessions/ inside the sandbox by
	// appendPIBwrapMounts (called below), so a dedicated --bind of
	// ~/.pi/agent/sessions onto its own host path is no longer needed — pi
	// inside the sandbox reaches the host directory via PI_CODING_AGENT_DIR,
	// not via the host home path. See #1985.
	//
	// These must be appended before the "--" terminator so bwrap processes
	// them as namespace arguments rather than as parts of the inner command.
	if cfg.Harness == "pi" {
		// Use ~/.config/prism/pi-extensions as the sandbox path for the
		// extension directory. /etc is ro-bind-mounted from the host so
		// bwrap cannot create /etc/prism inside the sandbox; .config/prism
		// follows the same pattern as other prism config mounts.
		if cfg.PIExtensionSandboxDir == "" {
			cfg.PIExtensionSandboxDir = filepath.Join(home, ".config", "prism", "pi-extensions")
		}
		// Same for the pi-agent config dir.
		if cfg.PIAgentConfigSandboxDir == "" {
			cfg.PIAgentConfigSandboxDir = filepath.Join(home, ".config", "prism", "pi-agent")
		}
		var piErr error
		args, piErr = appendPIBwrapMounts(args, cfg)
		if piErr != nil {
			// appendPIBwrapMounts already returns a descriptive error; wrap
			// with the bwrap context. BuildArgs cannot return an error (no
			// error return value in this method). Store on the Manager so
			// that Prepare can surface it after calling BuildArgs.
			m.piBwrapErr = piErr
		}
	}

	// ── Working directory ────────────────────────────────────────────────────
	// --chdir points at the worktree source path (not /workspace).
	args = append(args, "--chdir", cfg.Worktree)

	// ── Terminator: -- <harness invocation> ─────────────────────────────────
	// For pi: pi --port <port> --hostname 127.0.0.1
	// For PI:       pi --provider <p> --model <m> --extension ...
	//
	// bwrap uses 127.0.0.1 (not 0.0.0.0): the host network namespace is shared
	// (no --unshare-net), so binding to 0.0.0.0 would be overly broad.
	//
	// The port is cfg.AllocatedPort (the per-session host port picked by prism)
	// rather than the fixed ContainerPort. Because bwrap shares the host network
	// namespace, every sandbox binds directly to a host port — two sessions both
	// trying to bind ContainerPort (4096) would collide with EADDRINUSE and the
	// second would silently fail with "Failed to start server on port 4096" in
	// the agent log. ContainerPort is retained as a fallback for the
	// theoretical case where AllocatedPort is unset (e.g. a malformed session
	// row); in normal operation cfg.AllocatedPort is always populated by
	// agent-run from the DB's harness_port column.
	args = append(args, "--")
	args = append(args, PIInvocation(cfg)...)

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
// This is a thin alias over minimalIsolatedExecEnv (sandbox_exec.go) — the
// allow-list is identical across bwrap and sandbox-exec, so both call sites
// share a single helper. The alias is retained so the existing tests and
// callers in this file continue to read naturally as a bwrap-specific
// concern.
//
// See cmd/agent_run.go for the corresponding filter at the syscall.Exec
// call site. Both filters use the same allow-list; keep them in sync.
func minimalBwrapExecEnv(hostEnv []string) []string {
	return MinimalIsolatedExecEnv(hostEnv)
}

// Shutdown sends SIGTERM to the bwrap process if it is still running, waits
// up to 30 seconds, and sends SIGKILL if the process has not exited. The
// SIGTERM-then-grace-then-SIGKILL body is shared with sandbox-exec via the
// gracefulShutdown helper (shutdown.go, A2.GR).
func (b *bwrapIsolator) Shutdown() {
	b.mu.Lock()
	cmd := b.cmd
	b.mu.Unlock()
	gracefulShutdown(cmd, defaultGracefulShutdownGrace)
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

func init() {
	MustRegister(config.IsolationBwrap, func(opts ConstructorOpts) Isolator {
		return newBwrapIsolator(opts.Name)
	})
}
