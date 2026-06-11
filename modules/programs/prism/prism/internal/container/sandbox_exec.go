// Package container manages sandbox lifecycle and mount preparation for
// prism agent sessions.
// This file defines sandboxExecIsolator, an Apple sandbox-exec-based
// implementation of the Isolator interface. It is symmetric to bwrapIsolator
// in bwrap.go: BuildRunArgs() is a no-op stub, BuildArgs(m *Manager) is the
// concrete argument builder that has access to the Manager's config and state.
//
// This file implements PR 3 of the sandbox-exec design (issue #1012):
// staging HOME, credentials, caches, and write paths. PR 2 (#1016) landed
// the minimal read-only profile. Concurrency cap and lifecycle hardening are
// deferred to PR 4 (#1018). New top-level allow/deny clauses introduced
// beyond what is listed in the issue body require a comment pointing back at
// #1012.
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

// sandboxExecIsolator implements Isolator using Apple's sandbox-exec. It
// satisfies the interface through Run, Shutdown, HasExited, and DumpLogs.
// BuildRunArgs() returns nil (a no-op stub) because the real argument
// construction is performed by BuildArgs(m *Manager), which has access to the
// Manager's config and state.
//
// Symmetric to bwrapIsolator (see bwrap.go). The interface is not widened —
// BuildArgs is a concrete method that takes the Manager directly, mirroring
// the bwrap pattern.
type sandboxExecIsolator struct {
	// name is the stable session identifier (used for log messages).
	name string

	// mu guards cmd, exited, and exitCode. The production sandbox-exec
	// child is launched by cmd/agent_run_sandbox_exec_darwin.go (which
	// owns its own Wait loop), so the cmd field is populated only when
	// the Isolator's Run path is exercised — currently used by tests that
	// want to verify Shutdown delivers SIGTERM/SIGKILL via the shared
	// gracefulShutdown helper (shutdown.go, A2.GR).
	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   bool
	exitCode int
}

// newSandboxExecIsolator returns an Isolator backed by Apple's sandbox-exec
// for the given session name. The returned value satisfies the Isolator
// interface.
func newSandboxExecIsolator(name string) Isolator {
	return &sandboxExecIsolator{name: name}
}

// Name returns config.IsolationSandboxExec — the registry key for this isolator.
func (s *sandboxExecIsolator) Name() config.IsolationMode {
	return config.IsolationSandboxExec
}

// Capabilities returns the sandbox-exec feature flags:
//   - NeedsConfigBlob: config blob is injected as an env var before agent-run.
//   - NeedsHostAPISocket: the sidecar binds the host-API socket for in-sandbox proxy calls.
//   - RestartOnExit is false: sandbox-exec replaces the agent-run process via
//     syscall.Exec, so the sidecar does not observe process exit to restart.
func (s *sandboxExecIsolator) Capabilities() Capabilities {
	return Capabilities{
		IsContainer:                false,
		OwnsContainerLifecycle:     false,
		NeedsConfigBlob:            true,
		NeedsHostAPISocket:         true,
		UsesContainerHarness:       false,
		RestartOnExit:              false,
		NeedsStartupConnectTimeout: false,
		NeedsReadinessWait:         false,
		EmitsTmuxStatusColumns:     false,
	}
}

// BuildRunArgs satisfies the Isolator interface. It returns nil because the
// real argument construction requires Manager state and is implemented by the
// concrete BuildArgs(m *Manager) method below.
func (s *sandboxExecIsolator) BuildRunArgs() []string {
	return nil
}

// generateProfile returns the SBPL profile content for this session.
//
// This is a (version 3) SBPL profile — see the migration that actioned this
// in PR #1201 (issue #1200).
//
// The profile shape includes:
//   - Cryptex graft points (dyld shared cache, macOS 15+)
//   - Read-only system roots with file-test-existence, file-map-executable,
//     file-read-metadata alongside file-read* (required by dyld/AMFI)
//   - /bin, /sbin, and /var/... symlink-alias forms (v3 additions)
//   - /tmp read-write for xcrun and transient files
//   - Deny of sensitive /private/etc subtrees (wireguard, wpa_supplicant, ssh),
//     both /etc/... and /private/etc/... forms (symlink non-transparency)
//   - sops secrets.d deny (read + write) with named re-allow exceptions for
//     exactly the inventoried agent-needed secret names (issue #2211)
//   - Host ~/.aws deny with sso/cli carve-outs (config/credentials are
//     delivered via env vars at the host XDG paths — issue #2234)
//   - Literal RO grant for the real ~/.ssh/known_hosts — never (subpath ~/.ssh)
//     (issue #2213)
//   - RW subpath grant for ~/.config/claude — claude-code's config dir,
//     reached via the CLAUDE_CONFIG_DIR env var (issue #2243)
//   - Session work dir (covers the nested staging HOME) / worktree / bare
//     repo / host-API socket dir (RW)
//   - Symlink target allows (RW for cache/credential dirs, RO for key/config) for every symlink in the staging HOME
//   - Process and IPC primitives required by dyld, AMFI, and the agent
//   - (allow network*)
//
// New top-level allow/deny clauses introduced beyond what is sketched in
// #1012 require a comment in the generator pointing at the issue.
func generateProfile(m *Manager) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// Derive the staging HOME path (used for the symlink-target collection
	// below) and the per-session work dir (issue #2213; the staging HOME is
	// nested under it at <sessionDir>/home/). When they cannot be determined
	// (e.g. no home dir) the derived rules are simply omitted.
	stagingHome, stagingErr := m.sandboxExecHomePath()
	sessionDir, sessionDirErr := m.sessionWorkDirPath()

	var sb strings.Builder
	// ── Version 3 header and deny-default ────────────────────────────────
	// (version 3) unlocks syscall-unix, syscall-mach, system-mac-syscall,
	// system-fcntl, and the split ipc-posix-shm-read*/write* operations —
	// all required for Apple-signed binaries (dyld, AMFI). See #1200 / F.1.
	sb.WriteString("(version 3)\n")
	sb.WriteString("(deny default)\n")
	sb.WriteString("\n")

	// ── 1. Cryptex graft points ───────────────────────────────────────────
	// macOS 15+ stores the dyld shared cache under:
	//   /System/Volumes/Preboot/Cryptexes/OS/System/Library/dyld/
	// Both /System/Volumes/Preboot/Cryptexes (the Preboot-volume graft) and
	// /System/Cryptexes (the live-FS boot alias) must be readable AND
	// map-executable for dyld to bootstrap any binary. See F.1 §2 rule 1.
	sb.WriteString("(allow file-read* file-test-existence file-map-executable\n")
	sb.WriteString("  (subpath \"/System/Volumes/Preboot/Cryptexes\")\n")
	sb.WriteString("  (subpath \"/System/Cryptexes\"))\n")
	sb.WriteString("\n")

	// ── 2. Standard system read-only roots ───────────────────────────────
	// /nix            — Nix store. All Nix-built binaries live here.
	//   This also covers the PI extension directory (cfg.PIExtensionHostDir),
	//   which is a Nix store path resolved from piExtensionDir in config.json
	//   (issue #1213). No separate SBPL rule is needed for the PI extension.
	// /usr, /bin, /sbin — standard Apple-signed utility directories.
	// /System, /Library — OS frameworks, dylibs, and shared data.
	// /Applications/Xcode.app — xcrun (called by /usr/bin/git shim).
	// /etc, /private/etc — both forms: sandbox-exec does NOT transparently
	//   follow the /etc → /private/etc symlink (PR #1193 / issue #1187).
	// /private/var/db/dyld, /var/db/dyld — dyld shared-cache DB;
	//   /var is a symlink to /private/var; both forms needed. See F.1 §2.
	// /private/var/db/timezone, /var/db/timezone — timezone data.
	// /private/var/select, /var/select — xcode-select developer_dir.
	// /private/var/folders, /var/folders — Darwin per-user TMPDIR. Listed
	//   here for read access; write access is granted separately below because
	//   bun (pi's runtime) extracts native dylibs (libopentui, etc.) to
	//   hidden files under the user TMPDIR and dlopen()s them — requiring both
	//   file-write* and file-map-executable on that subtree.
	// /dev/null, /dev/random, /dev/urandom — device nodes.
	// /dev/dtracehelper — Apple-signed binaries probe at startup.
	// /             — required by libignition's openat(2) root probe.
	// /$bunfs       — bun's virtual in-process asset filesystem. pi is
	//   packaged as a bun single-file executable; bun serves bundled assets
	//   (locale files, etc.) from a synthetic /$bunfs/ path. The kernel
	//   returns EPERM for open(2) calls on /$bunfs/* under deny-default even
	//   though the path is not a real filesystem — the sandbox gates it at the
	//   syscall level before bun's VFS intercept can handle it.
	// file-test-existence, file-map-executable, file-read-metadata added
	//   alongside file-read* for dyld to probe and map code-signed binaries.
	//   See F.1 §2 rule 2 and migration delta §6.
	sb.WriteString("(allow file-read* file-test-existence file-map-executable file-read-metadata\n")
	sb.WriteString("  (subpath \"/nix\")\n")
	sb.WriteString("  (subpath \"/usr\")\n")
	sb.WriteString("  (subpath \"/bin\")\n")
	sb.WriteString("  (subpath \"/sbin\")\n")
	sb.WriteString("  (subpath \"/opt/homebrew\")\n")
	sb.WriteString("  (subpath \"/System\")\n")
	sb.WriteString("  (subpath \"/Library\")\n")
	sb.WriteString("  (subpath \"/Applications/Xcode.app\")\n")
	sb.WriteString("  (subpath \"/private/etc\")\n")
	sb.WriteString("  (subpath \"/etc\")\n")
	sb.WriteString("  (subpath \"/private/var/db/dyld\")\n")
	sb.WriteString("  (subpath \"/private/var/db/timezone\")\n")
	sb.WriteString("  (subpath \"/private/var/select\")\n")
	sb.WriteString("  (subpath \"/private/var/folders\")\n")
	sb.WriteString("  (subpath \"/var/db/dyld\")\n")
	sb.WriteString("  (subpath \"/var/db/timezone\")\n")
	sb.WriteString("  (subpath \"/var/select\")\n")
	sb.WriteString("  (subpath \"/var/folders\")\n")
	sb.WriteString("  (subpath \"/$bunfs\")\n")
	sb.WriteString("  (literal \"/dev/null\")\n")
	sb.WriteString("  (literal \"/dev/random\")\n")
	sb.WriteString("  (literal \"/dev/urandom\")\n")
	sb.WriteString("  (literal \"/dev/dtracehelper\")\n")
	// /var itself — /var is a symlink to /private/var; sandbox-exec does not
	// transparently follow top-level symlinks for stat(2) calls, so
	// file-read-metadata on /var (not just its subdirs) must be explicit.
	sb.WriteString("  (literal \"/var\")\n")
	sb.WriteString("  (literal \"/\"))\n")
	sb.WriteString("\n")

	// ── 3. /tmp read-write ────────────────────────────────────────────────
	// /tmp and /private/tmp are used by xcrun, git, and other tools for
	// transient files. Both symlink forms needed. See F.1 §2 rule 3.
	//
	// file-map-executable is required: pi's TUI (OpenTUI) and the
	// file-watcher binding extract native .dylib/.node files to /tmp and
	// dlopen() them. dlopen calls mmap(PROT_EXEC) on the extracted file,
	// which requires file-map-executable. Without it the sandbox returns
	// EPERM on mmap, the TUI fails to initialise, and session.created is
	// never emitted — causing the sidecar readiness gate to time out.
	sb.WriteString("(allow file-read* file-write* file-test-existence file-map-executable\n")
	sb.WriteString("  (subpath \"/private/tmp\")\n")
	sb.WriteString("  (subpath \"/tmp\"))\n")
	sb.WriteString("\n")

	// ── 3b. Darwin per-user TMPDIR read-write ─────────────────────────────
	// bun (pi's runtime) extracts native dylibs — libopentui, the
	// file-watcher binding, etc. — to hidden files under the Darwin per-user
	// TMPDIR (/private/var/folders/<hash>/T/) and dlopen()s them. Without
	// file-write* the extraction fails silently (EPERM) and the TUI library
	// is never loaded, causing pi to crash with exit 255.
	// file-map-executable is required for the subsequent dlopen(PROT_EXEC).
	//
	// We allow only the specific per-user TMPDIR (os.TempDir()), NOT all of
	// /private/var/folders, to avoid giving the sandbox read access to other
	// users' temp directories. On Darwin, os.TempDir() may return either the
	// /var/folders/... symlink form or the /private/var/folders/... canonical
	// form depending on OS version. sandbox-exec does not follow the
	// /var → /private/var symlink transparently, so both forms must be listed
	// (same pattern as /etc → /private/etc, PR #1193). We emit whichever form
	// os.TempDir() returns plus its counterpart.
	tmpDir := os.TempDir()
	if tmpDir != "" {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-map-executable\n")
		sb.WriteString("  (subpath " + quoteSBPL(tmpDir) + ")\n")
		// Always emit both the /var/... and /private/var/... forms.
		// os.TempDir() may return either form depending on the OS version;
		// sandbox-exec does not follow the /var → /private/var symlink
		// transparently, so both must be listed explicitly.
		if strings.HasPrefix(tmpDir, "/private/var/") {
			varAlias := strings.TrimPrefix(tmpDir, "/private")
			sb.WriteString("  (subpath " + quoteSBPL(varAlias) + ")\n")
		} else if strings.HasPrefix(tmpDir, "/var/") {
			privateAlias := "/private" + tmpDir
			sb.WriteString("  (subpath " + quoteSBPL(privateAlias) + ")\n")
		}
		sb.WriteString(")\n")
		sb.WriteString("\n")
	}

	// ── 3c. sops secrets.d deny + named re-allow exceptions (issue #2211) ─
	// The broad /private/var/folders allows above (sections 2 and 3b) expose
	// the entire home-manager sops-nix secrets tree
	// (~/.config/sops-nix/secrets → /var/folders/<…>/T/secrets.d/<N>/) to the
	// sandbox: the daily-driver GitHub PAT, full-power AWS config, admin kube
	// configs, gitlab/hass/notion/syncthing keys, the non-prism SSH keys, and
	// the secrets.d/age-keys.txt copy. None of those are read in-sandbox —
	// GITHUB_TOKEN, OPENROUTER_API_KEY, etc. are resolved host-side by
	// agent-run (credentialEnvVars, including the #2029 GitHubTokenPath file
	// fallback) and injected into the sandbox env as VALUES before
	// sandbox-exec starts.
	//
	// Shape:
	//   1. deny file-write* on the secrets.d subtree, no exceptions — nothing
	//      in-sandbox ever writes a secret; this also protects the allowlisted
	//      names from in-sandbox tampering.
	//   2. deny file-read* on the secrets.d subtree, with require-not
	//      exceptions for exactly the inventoried agent-needed secret NAMES
	//      (collectSecretsDAllowlistNames: ssh access key + .pub, signing key
	//      + .pub, aws readonly-config, aws credentials, kube agents-config —
	//      each derived from its stable host source path, so a source that is
	//      absent or not sops-backed simply produces no exception).
	//
	// The exceptions live inside the deny rule itself (require-all +
	// require-not) rather than as separate (allow ...) rules emitted after
	// the deny, so the narrowing does not depend on inter-rule precedence
	// between a regex allow and a regex deny: an allowlisted path simply does
	// not match this deny and falls through to the broad allow above. The
	// deny-after-broader-allow precedence this rule does rely on is the same
	// proven shape as the /private/etc/ssh denies in section 4 (integration
	// tested by sandbox_exec_denies_darwin_test.go).
	//
	// Rotation safety (#1410/#1573): sops rotates secrets.d/<N> → <N+1> on
	// every activation, but secret NAMES are stable. The exception regexes
	// match any counter ([0-9]+), so allowlisted reads survive a rotation
	// mid-session and denied reads stay denied — by construction. The deny
	// prefixes are static regexes covering both the /var and /private/var
	// symlink forms (same dual-form pattern as 3b) and do not depend on
	// os.TempDir(), so an unset or odd TMPDIR cannot silently re-expose the
	// real tree.
	//
	// Accepted residual: file-test-existence on the subtree stays allowed
	// (via the section-2 allow), so an in-sandbox process can probe which
	// secret NAMES exist — but cannot read or write their content.
	const (
		secretsDDenyRegexVarForm     = `^/var/folders/.*/secrets\.d/`
		secretsDDenyRegexPrivateForm = `^/private/var/folders/.*/secrets\.d/`
	)
	sb.WriteString("(deny file-write*\n")
	sb.WriteString("  (regex #\"" + secretsDDenyRegexVarForm + "\")\n")
	sb.WriteString("  (regex #\"" + secretsDDenyRegexPrivateForm + "\"))\n")
	sb.WriteString("\n")
	sb.WriteString("(deny file-read*\n")
	sb.WriteString("  (require-all\n")
	sb.WriteString("    (require-any\n")
	sb.WriteString("      (regex #\"" + secretsDDenyRegexVarForm + "\")\n")
	sb.WriteString("      (regex #\"" + secretsDDenyRegexPrivateForm + "\"))\n")
	for _, name := range collectSecretsDAllowlistNames(m, home) {
		sb.WriteString("    (require-not (regex #\"/secrets\\.d/[0-9]+/" + regexQuotePath(name) + "$\"))\n")
	}
	sb.WriteString("))\n")
	sb.WriteString("\n")

	// ── 4. Sensitive /etc subtree denies ──────────────────────────────────
	// These deny rules must follow the broad /etc and /private/etc allows
	// above. In SBPL, more-specific rules override broader ones. Both
	// /etc/... and /private/etc/... forms needed due to symlink
	// non-transparency (same pattern as PR #1193 for v1). See F.1 §2 rule 4.
	sb.WriteString("(deny file-read* file-write*\n")
	sb.WriteString("  (subpath \"/etc/wireguard\")\n")
	sb.WriteString("  (subpath \"/etc/wpa_supplicant\")\n")
	sb.WriteString("  (subpath \"/etc/ssh\")\n")
	sb.WriteString("  (subpath \"/private/etc/wireguard\")\n")
	sb.WriteString("  (subpath \"/private/etc/wpa_supplicant\")\n")
	sb.WriteString("  (subpath \"/private/etc/ssh\"))\n")
	sb.WriteString("\n")

	// ── 5. Host ~/.aws deny with sso/ and cli/ carve-outs ────────────────
	// The aws config/credentials are delivered via env vars at the host XDG
	// paths (issue #2234) — nothing agent-needed lives at the raw ~/.aws
	// canonical paths. The host raw ~/.aws subtree must remain read-denied
	// so that path traversal cannot reach host credentials there.
	//
	// Exception: ~/.aws/sso and ~/.aws/cli must remain accessible so that
	// AWS SSO auth and tools that read SSO tokens (e.g. kubectl) work inside
	// the sandbox. The staging HOME creates symlinks for these two subdirs
	// pointing at the host paths, and collectStagingHomeSymlinkTargets emits
	// per-symlink allow rules for them. The more-specific allow rules below
	// override the broad deny for exactly these two subdirs (SBPL evaluates
	// more-specific rules as overrides of broader ones). See issue #1380.
	if home != "" {
		awsPath := filepath.Join(home, ".aws")
		sb.WriteString("(deny file-read* file-write*\n")
		sb.WriteString("  (subpath " + quoteSBPL(awsPath) + "))\n")
		sb.WriteString("\n")
		// Carve-outs: allow sso/ and cli/ subtrees within ~/.aws. These
		// more-specific allow rules override the broad deny above.
		// file-write* is required: the aws CLI writes STS token cache entries
		// to ~/.aws/cli/cache/ and refreshes SSO tokens in ~/.aws/sso/. Without
		// write access the CLI fails with EPERM (no STS token cache), and kubectl
		// against EKS also breaks because its exec-credential plugin shells out
		// to aws and gets EPERM. Mirrors bwrap's --bind (RW) treatment — see the
		// comment above awsSSOReadOnly/awsCLIReadOnly in mounts.go. (issue #1558).
		awsSSOPath := filepath.Join(home, ".aws", "sso")
		awsCLIPath := filepath.Join(home, ".aws", "cli")
		sb.WriteString("(allow file-read* file-write*\n")
		sb.WriteString("  (subpath " + quoteSBPL(awsSSOPath) + ")\n")
		sb.WriteString("  (subpath " + quoteSBPL(awsCLIPath) + "))\n")
		sb.WriteString("\n")
	}

	// ── 5b. Nix flake trusted-settings read (issue #2201) ────────────────
	// Flake-CLI nix commands consult $XDG_DATA_HOME/nix/trusted-settings.json
	// whenever the target flake declares a nixConfig block (e.g. this repo's
	// extra-substituters / extra-trusted-public-keys). XDG_DATA_HOME inside
	// the sandbox points at the real host ~/.local/share — see the env
	// assembly in cmd/agent_run_sandbox_exec_darwin.go — so under
	// deny-default the read fails EPERM and nix aborts the entire eval,
	// making every flake CLI command unusable on such repos.
	//
	// The file holds the user's accept/ignore decisions for flake-provided
	// config settings; its contents are not secret. The allow is read-only
	// and single-file (literal, not subpath): nix only ever writes the file
	// from an interactive "permanently mark this value as trusted" prompt
	// (readTrustedList/writeTrustedList in nix's src/libflake/config.cc),
	// and persisting trust decisions from inside a sandbox should remain
	// blocked. When the file does not exist, nix's pathExists probe returns
	// false (the literal allow covers the stat, so it yields ENOENT rather
	// than EPERM) and nix proceeds with an empty trusted list — its normal
	// missing-file path.
	if home != "" {
		nixTrustedSettings := filepath.Join(home, ".local", "share", "nix", "trusted-settings.json")
		sb.WriteString("(allow file-read* file-test-existence\n")
		sb.WriteString("  (literal " + quoteSBPL(nixTrustedSettings) + "))\n")
		sb.WriteString("\n")
	}

	// ── 5c. Real ~/.ssh/known_hosts read-only literal (issue #2213) ──────
	// The generated <sessionDir>/ssh-config is passed to ssh via -F
	// (GIT_SSH_COMMAND), and openssh resolves its default UserKnownHostsFile
	// against the real home (getpwuid → pw_dir, not $HOME) — so ssh inside
	// the sandbox reads the real ~/.ssh/known_hosts. Previously that read was
	// only possible via the staging-HOME symlink walk (the per-symlink
	// literal emitted by collectStagingHomeSymlinkTargets); this explicit
	// grant decouples it from the staging mechanism ahead of Step 5 of #2132.
	//
	// The grant is read-only and single-file. NEVER widen this to
	// (subpath ~/.ssh): the real ~/.ssh may hold non-sops private keys (e.g.
	// the daily-driver key) that must stay unreadable in-sandbox. With
	// StrictHostKeyChecking accept-new, ssh's attempt to append an unknown
	// host key fails (no write grant) with a non-fatal warning — accepted.
	if home != "" {
		knownHosts := filepath.Join(home, ".ssh", "known_hosts")
		sb.WriteString("(allow file-read* file-test-existence\n")
		sb.WriteString("  (literal " + quoteSBPL(knownHosts) + "))\n")
		sb.WriteString("\n")
	}

	// ── 5d. Claude config dir read-write (issue #2243) ───────────────────
	// claude-code resolves its config dir (and .claude.json) via the
	// CLAUDE_CONFIG_DIR env var at the host XDG path ~/.config/claude
	// (declared in agent.envVars by the nix module — Step 3c of #2132; the
	// .claude write-through staging symlink is gone). Unlike the aws/kube
	// XDG configs, ~/.config/claude is a plain host directory — NOT a sops
	// symlink — so the #2211 secrets.d allowlist plays no part here: this
	// explicit RW subpath grant is the sole capability for the path.
	// Read-write because claude-code writes config, history, and OAuth
	// token refreshes under it; mirrors bwrap's --bind (RW) treatment of
	// the same dir in mounts.go StandardSandboxMounts.
	// Emitted even when the dir does not yet exist — sandbox-exec silently
	// ignores (subpath ...) rules for non-existent paths (same shape as the
	// ~/.pi/agent rule in 6a); the nix module's hm activation creates the
	// dir on managed hosts.
	if home != "" {
		claudeConfigDir := filepath.Join(home, ".config", "claude")
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + quoteSBPL(claudeConfigDir) + "))\n")
		sb.WriteString("\n")
	}

	// ── 6. Session work dir + worktree + bare repo + host-API socket (RW) ─
	// Session-specific read-write paths. Locked in #1012 and #1017.
	// file-test-existence and file-read-metadata added alongside file-read*
	// and file-write* for dyld/AMFI compatibility in the v3 profile.
	//
	// The first entry is the per-session work dir (issue #2213) — it covers
	// the generated ssh-config / gitconfig / allowed_signers AND the staging
	// HOME nested under it at <sessionDir>/home/ (the staging HOME remains
	// in place until Step 5 of #2132 deletes it).
	if sessionDirErr == nil {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + quoteSBPL(sessionDir) + ")\n")
		// Worktree — the git checkout the agent works in.
		if m.cfg.Worktree != "" {
			sb.WriteString("  (subpath " + quoteSBPL(m.cfg.Worktree) + ")\n")
		}
		// Bare repo root — full RW access so the agent can probe for project
		// config files (e.g. .opencode/, AGENTS.md) at the top level and git
		// can write pack files, ref updates, etc. in .bare/.
		if m.cfg.BareRoot != "" {
			sb.WriteString("  (subpath " + quoteSBPL(m.cfg.BareRoot) + ")\n")
		}
		// Host-API socket directory — the sidecar's per-session socket dir.
		// (Post #2034: the per-session pi-agent/ staging subdirectory is gone;
		// PI now reads from ~/.pi/agent directly via the shared mount. The
		// run-dir (subpath ...) rule below still covers agent-run.log,
		// hostapi.sock, and pipe.sock.)
		if m.cfg.HostAPISockPath != "" {
			sockDir := filepath.Dir(m.cfg.HostAPISockPath)
			sb.WriteString("  (subpath " + quoteSBPL(sockDir) + ")\n")
		}
		// pi shared state dirs — both XDG locations pi writes to:
		// pi data and state directories
		// Must use (subpath ...) not (literal ...) so the sandbox can
		// read/write files inside the directories (not just the directory nodes).
		if home != "" {
			piDataDir := filepath.Join(home, ".local", "share", "pi")
			sb.WriteString("  (subpath " + quoteSBPL(piDataDir) + ")\n")
			piStateDir := filepath.Join(home, ".local", "state", "pi")
			sb.WriteString("  (subpath " + quoteSBPL(piStateDir) + ")\n")
		}
		sb.WriteString(")\n")
		sb.WriteString("\n")
	}

	// ── 6a. PI agent dir allow (pi sessions only) ────────────────────────
	// OAuth token refresh inside pi-coding-agent uses proper-lockfile with
	// realpath:true, which resolves auth.json through any symlink and then
	// calls mkdir(<resolved-auth-path>.lock) to acquire the lock. That mkdir
	// requires write permission on the *parent directory* ~/.pi/agent/, not
	// just on the auth.json file itself. A (literal ...) rule on auth.json
	// alone is therefore insufficient — the sandbox denies the mkdir and the
	// refresh silently fails (EPERM after ~30 s of retries).
	//
	// We widen the rule to (subpath ~/.pi/agent) so that:
	//   - auth.json reads and writes are permitted (token refresh writes back
	//     to the host file).
	//   - mkdir auth.json.lock (the proper-lockfile lock dir) is permitted
	//     because the parent dir is now writable.
	//
	// The rule is still gated on Harness == "pi" so non-pi sessions are not
	// exposed to the pi credential directory. It is always emitted for pi
	// sessions even when ~/.pi/agent does not yet exist — sandbox-exec
	// silently ignores (subpath ...) rules for non-existent paths, so pi
	// simply prompts for /login rather than crashing on a fresh install.
	if m.cfg.Harness == "pi" && home != "" {
		piAgentDir := filepath.Join(home, ".pi", "agent")
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + quoteSBPL(piAgentDir) + "))\n")
		sb.WriteString("\n")
	}

	// ── 6b. BareRoot ancestor probe allows ───────────────────────────────
	// opencode's fs.up() walk probes multiple targets (.opencode, .git) at
	// every ancestor of the worktree with no stop parameter. Under deny-default
	// any EPERM (not ENOENT) is fatal. We grant file-test-existence on:
	//   1. Every ancestor of BareRoot up to (not including) HOME — these are
	//      sibling worktree directories with no sensitive data, so subpath is safe.
	//   2. HOME itself and HOME/.opencode/.git — the walk reaches HOME and probes
	//      these paths before stopping (HOME is the natural filesystem root for
	//      a user repo checkout). We use literal rules for HOME-level paths to
	//      avoid granting broad access to the real home directory.
	if m.cfg.BareRoot != "" && home != "" {
		dir := filepath.Dir(m.cfg.BareRoot)
		var ancestors []string
		for dir != "/" && dir != "." && dir != home {
			ancestors = append(ancestors, dir)
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		if len(ancestors) > 0 {
			sb.WriteString("(allow file-test-existence file-read-metadata\n")
			for _, a := range ancestors {
				sb.WriteString("  (subpath " + quoteSBPL(a) + ")\n")
			}
			// pi's fromDirectory walk has no stop parameter and walks all
			// the way to filesystem root, probing .opencode and .git at each
			// level. Emit literal allows for the probe targets at every ancestor
			// from HOME up to / so the walk returns ENOENT rather than EPERM.
			// We use literals (not subpath) for HOME and above to avoid granting
			// broad read access to directories containing sensitive content.
			// opencode probes many arbitrary filenames (opencode.json, tui.jsonc,
			// .opencode, .git, etc.) at every ancestor directory all the way to
			// filesystem root. Granting (subpath ...) for file-test-existence is
			// safe: file-test-existence only allows stat()/access(F_OK), not reads.
			// Sensitive paths (.aws, wireguard, etc.) are protected by explicit
			// deny file-read*/file-write* rules which are not overridden by a
			// file-test-existence allow.
			// Use (subpath home) to cover all probes inside the home directory,
			// and (subpath parent) for each ancestor above home up to root —
			// these are OS-level directories (/Users, /) with no user-sensitive
			// file contents.
			sb.WriteString("  (subpath " + quoteSBPL(home) + ")\n")
			cur := filepath.Dir(home)
			for {
				sb.WriteString("  (subpath " + quoteSBPL(cur) + ")\n")
				parent := filepath.Dir(cur)
				if parent == cur || parent == "" {
					break
				}
				cur = parent
			}
			sb.WriteString(")\n")
			sb.WriteString("\n")
		}
	}

	// ── Symlink target allows (RW and RO) ────────────────────────────────
	// For every symlink in the staging HOME, resolve its target via
	// filepath.EvalSymlinks and emit an allow rule. Locked in #1012 and #1017.
	//
	// Rule type selection:
	//   - Directory targets → (subpath ...) so the sandbox can access files
	//     within (not just the directory node itself).
	//   - File targets → (literal ...) to allow the specific file.
	//   - Writable targets (cache dirs, write-through credential dirs) →
	//     file-read* file-write* file-test-existence. Mirrors bwrap --bind (RW).
	//   - RO targets (.ssh, .config/opencode) →
	//     file-read* only. Mirrors bwrap --ro-bind.
	//
	// Targets that fall under the denied ~HOME/.aws subtree are excluded from
	// this block to avoid the Apple SBPL literal-over-subpath precedence issue
	// (a more-specific (literal) allow defeats a broader (subpath) deny).
	if stagingErr == nil {
		targets, targErr := collectStagingHomeSymlinkTargets(stagingHome)
		if targErr != nil {
			log.Printf("container: sandbox-exec: collect symlink targets: %v", targErr)
		}

		// Split into RW and RO sets.
		var rwTargets, roTargets []StagingSymlinkTarget
		for _, t := range targets {
			if t.Writable {
				rwTargets = append(rwTargets, t)
			} else {
				roTargets = append(roTargets, t)
			}
		}

		if len(rwTargets) > 0 {
			sb.WriteString("(allow file-read* file-write* file-test-existence\n")
			for _, t := range rwTargets {
				if t.IsDir {
					sb.WriteString("  (subpath " + quoteSBPL(t.ResolvedPath) + ")\n")
				} else {
					sb.WriteString("  (literal " + quoteSBPL(t.ResolvedPath) + ")\n")
				}
			}
			sb.WriteString(")\n")
			sb.WriteString("\n")
		}

		if len(roTargets) > 0 {
			sb.WriteString("(allow file-read*\n")
			for _, t := range roTargets {
				if t.IsDir {
					sb.WriteString("  (subpath " + quoteSBPL(t.ResolvedPath) + ")\n")
				} else {
					sb.WriteString("  (literal " + quoteSBPL(t.ResolvedPath) + ")\n")
				}
			}
			sb.WriteString(")\n")
			sb.WriteString("\n")
		}
	}

	// ── 8. /dev/null and /dev/dtracehelper write access ──────────────────
	// Apple-signed binaries (git, ssh) open /dev/null for writing during
	// startup. The file-read* above covers reads; writes need an explicit
	// allow. See F.1 §2 rule 8.
	// /dev/dtracehelper also requires file-write-data: Apple binaries call
	// write(2) on it at startup to check for DTrace. The file-read* literal
	// above covers the open(2); this covers the write(2).
	sb.WriteString("(allow file-write-data\n")
	sb.WriteString("  (literal \"/dev/null\")\n")
	sb.WriteString("  (literal \"/dev/dtracehelper\"))\n")
	sb.WriteString("\n")

	// ── 8c. Apple-internal path probe ─────────────────────────────────────
	// Apple-signed binaries probe /AppleInternal/XBS/.isChrooted at startup
	// to detect build-farm environments. Under deny-default this returns
	// EPERM (not ENOENT), which some binaries treat as fatal. Allowing
	// file-test-existence lets the probe return ENOENT harmlessly.
	sb.WriteString("(allow file-test-existence\n")
	sb.WriteString("  (literal \"/AppleInternal/XBS/.isChrooted\"))\n")
	sb.WriteString("\n")

	// ── 8b. TTY ioctl ─────────────────────────────────────────────────────
	// pi calls tcsetattr(2) to put the terminal into raw mode for the
	// TUI. Under deny-default this is blocked (errno EPERM) which prevents
	// the TUI from rendering and causes the sidecar to never see a
	// session.created event (pi detects no TTY and exits the TUI path
	// without emitting one). (allow file-ioctl (subpath "/dev")) covers the
	// TIOCGETA/TIOCSETA ioctls on /dev/ttysXXX and /dev/ptmx without
	// granting any file-read/file-write access to /dev beyond what the
	// system-read allow above already provides.
	sb.WriteString("(allow file-ioctl\n")
	sb.WriteString("  (subpath \"/dev\"))\n")
	sb.WriteString("\n")

	// ── 9. Process operations ─────────────────────────────────────────────
	// process-exec*  — execvp(2). Required for any child process to launch.
	// process-fork   — fork(2). git and ssh use it for child processes.
	// process-info*  — required by AMFI when validating the certificate chain
	//   of Apple-signed binaries (process-info-codesignature, process-info-pidinfo).
	//   Without this, git and ssh abort with SIGABRT during dyld init. See F.1 §2.
	// mach-lookup    — bootstrap service lookups (logd, opendirectoryd, etc.).
	//   The rule is intentionally UNQUALIFIED (no (global-name ...) filter), which
	//   subsumes the WindowServer bootstrap port chromium connects to in headed
	//   mode (com.apple.windowserver.active) called out as a separate clause in
	//   issue #2021 §4. Tightening mach-lookup to an enumerated global-name set
	//   is a defensible follow-up but requires empirically enumerating every name
	//   dyld + AMFI + securityd + CFNetwork + libsystem_kernel + node + chromium
	//   probe at startup — a much larger surface than the v1/v2 rule shape was
	//   ever scoped to. The headed-mode WindowServer requirement from #2021 §4 is
	//   already satisfied by the unqualified form.
	// mach-register  — per-pid Mach name registration (pi IPC).
	// sysctl-read    — system library init queries (kern.*, hw.*, machdep.*).
	// NOTE: signal is emitted as its own clause below so the (target ...)
	//   qualifiers can be expressed cleanly — see §9a.
	// NOTE: iokit-open is emitted as its own enumerated-class clause below —
	//   see §9b (issue #2021).
	// NOTE: ipc-posix-shm is REMOVED (unbound variable in v3 — replaced below).
	sb.WriteString("(allow process-exec* process-fork process-info* mach-lookup mach-register\n")
	sb.WriteString("       sysctl-read)\n")
	sb.WriteString("\n")

	// ── 9a. signal — self + children (issue #2021) ───────────────────────
	// Self-signaling is required for normal exit and tcsetpgrp-style TTY
	// management. Children-signaling is required for playwright-cli's
	// node-side launcher to clean up its chromium grandchild (the launcher
	// calls process.kill(child.pid) and fails with `kill EPERM` if it lacks
	// signal rights over its child process group).
	//
	// (target children) permits signalling processes the sandboxed process
	// spawned (transitively, including grandchildren). It does NOT widen the
	// surface to other host PIDs — (target others) is deliberately NOT used.
	sb.WriteString("(allow signal (target self) (target children))\n")
	sb.WriteString("\n")

	// ── 9b. iokit-open-user-client — Chromium user-client classes (#2021) ─
	// Chromium / firefox / webkit require iokit access on a small set of
	// IOKit user-client classes during framework init. Without this,
	// chromium SIGSEGVs in IONotificationPortGetRunLoopSource at
	// ChromeMain+~50ms — the canonical fingerprint of iokit denial.
	//
	// The v3 predicate is `iokit-open-user-client` (unbound in v1 as
	// `iokit-open`; v3 split it into `iokit-open-user-client` for opening
	// IOService user-client connections and the broader `iokit-open` for
	// arbitrary opens, the latter being itself unbound in v3 — see
	// /System/Library/Sandbox/Profiles/application.sb for Apple's own usage).
	//
	// The allow set is enumerated by class name rather than unqualified to
	// preserve the deny-default posture for everything else (AppleAVE,
	// IOBluetoothHCIController, etc.). Each class entry corresponds to a
	// specific IOKit subsystem Chromium probes at startup:
	//
	//   IOSurfaceRoot                  — Metal / IOSurface framebuffer
	//   IOHIDLibUserClient             — HID input (mouse, keyboard, trackpad)
	//   IOAudioEngineUserClient        — AudioComponent init (no audio routing)
	//   IOFramebufferSharedUserClient  — windowing system framebuffer
	//   RootDomainUserClient           — power-management state notifications
	//
	// The unqualified form `(allow iokit-open-user-client)` MUST NOT be
	// used — it would open the door to AppleAVE, AppleIOAccelerator, and
	// Bluetooth-HCI user-client classes that are not needed by any
	// sandboxed workload.
	sb.WriteString("(allow iokit-open-user-client\n")
	sb.WriteString("  (iokit-user-client-class \"IOSurfaceRoot\")\n")
	sb.WriteString("  (iokit-user-client-class \"IOHIDLibUserClient\")\n")
	sb.WriteString("  (iokit-user-client-class \"IOAudioEngineUserClient\")\n")
	sb.WriteString("  (iokit-user-client-class \"IOFramebufferSharedUserClient\")\n")
	sb.WriteString("  (iokit-user-client-class \"RootDomainUserClient\"))\n")
	sb.WriteString("\n")

	// ── 12. POSIX shared memory ────────────────────────────────────────────
	// CRITICAL: The v1 (allow ipc-posix-shm) is an UNBOUND VARIABLE in v3.
	// Use the split read*/write* variants instead. See F.1 §2 rule 12.
	// ipc-posix-shm-read*  — required by dyld and notification_center.
	// ipc-posix-shm-write* — required by CoreFoundation / libobjc init.
	sb.WriteString("(allow ipc-posix-shm-read* ipc-posix-shm-write*)\n")
	sb.WriteString("\n")

	// ── 14. syscall-unix and syscall-mach ─────────────────────────────────
	// CRITICAL: In (version 3) with deny-default, individual syscalls are
	// gated by the sandbox policy. Without (allow syscall-unix), dyld aborts
	// with SIGABRT during the libignition phase. See F.1 §2 rule 14.
	// (allow syscall-mach) is required for Mach trap calls used by
	// CoreFoundation and libdispatch.
	sb.WriteString("(allow syscall-unix syscall-mach)\n")
	sb.WriteString("\n")

	// ── 15. system-mac-syscall ────────────────────────────────────────────
	// Required for AMFI certificate chain validation. See F.1 §2 rule 15.
	sb.WriteString("(allow system-mac-syscall)\n")
	sb.WriteString("\n")

	// ── 16. system-fcntl ──────────────────────────────────────────────────
	// Required for F_ADDFILESIGS_RETURN, F_CHECK_LV, and F_GETPATH, which
	// dyld calls when mapping code-signed executables. See F.1 §2 rule 16.
	sb.WriteString("(allow system-fcntl)\n")
	sb.WriteString("\n")

	// ── 17. Network ───────────────────────────────────────────────────────
	// Unchanged from v1. Matches the bwrap baseline. Restriction to
	// specific hosts/ports is a future concern per #1012.
	//
	// Note: GIT_SSH_COMMAND is set to the Nix-built openssh binary (cfg.SshBin,
	// baked into config.json by prism-tui.nix). Nix openssh links against its
	// own libresolv/libldns (Nix store paths under /nix, already allowed) rather
	// than Apple's libnetwork.dylib. This means the system-network macro rules
	// (dafsaData.bin, com.apple.netsrc, AF_SYSTEM/AF_ROUTE sockets) are NOT
	// needed for SSH git operations — they would only be needed if /usr/bin/ssh
	// were used. See issue #1012 and the GIT_SSH_COMMAND block in
	// cmd/agent_run_sandbox_exec_darwin.go for the full rationale.
	sb.WriteString("(allow network*)\n")
	sb.WriteString("\n")

	// ── 18. Socket options ────────────────────────────────────────────────
	// CRITICAL: (allow network*) in v3 does NOT cover setsockopt/getsockopt.
	// pi's HTTP server calls setsockopt(SOL_SOCKET, SO_REUSEADDR)
	// when binding its listener port. Without this, setsockopt returns EPERM
	// and the server never binds — the sidecar sees connection refused for
	// the full 30-second readiness timeout. socket-option-get is included
	// symmetrically (getsockopt is called by node's net module at startup).
	sb.WriteString("(allow socket-option-set socket-option-get)\n")
	sb.WriteString("\n")

	// ── 19. Dynamic code generation ───────────────────────────────────────
	// CRITICAL: Bun (pi's runtime) uses a JIT compiler. Under
	// deny-default in (version 3), JIT requires explicit permission via
	// (allow dynamic-code-generation). Without it, the sandbox sends SIGABRT
	// to the process the moment it attempts to mark pages executable for JIT
	// output — before any JS code runs, making this a silent crash with no
	// error output.
	sb.WriteString("(allow dynamic-code-generation)\n")
	sb.WriteString("\n")

	// ── 20. /dev/tty and /dev/autofs_nowait reads ─────────────────────────
	// /dev/tty — bash and other shells open /dev/tty for terminal control
	//   at startup (isatty(2), tcgetattr). Without file-read-data the open
	//   fails with EPERM causing shell init errors.
	// /dev/autofs_nowait — probed by CoreFoundation/NSBundle at startup.
	//   Non-fatal if denied but causes log noise; allow file-read-data only.
	sb.WriteString("(allow file-read-data\n")
	sb.WriteString("  (literal \"/dev/tty\")\n")
	sb.WriteString("  (literal \"/dev/autofs_nowait\"))\n")
	sb.WriteString("\n")

	// ── 21. User preferences read ─────────────────────────────────────────
	// CoreFoundation reads user preferences (CFPreferences) at startup for
	// locale, font smoothing, and other settings. Without this the framework
	// emits log warnings but continues; allowing it silences the denials.
	sb.WriteString("(allow user-preference-read)\n")

	return sb.String()
}

// collectSecretsDAllowlistNames returns the secrets.d-relative names of the
// agent-needed secrets, derived from the stable host source paths the
// sandbox legitimately reads through (issue #2211):
//
//	~/.ssh/<SshAccessKeyName>            — ssh auth (generated ssh-config IdentityFile)
//	~/.ssh/<SshAccessKeyName>.pub        — openssh public-half probe
//	~/.ssh/<SshSigningKeyName>           — commit signing (ssh-keygen -Y sign)
//	~/.ssh/<SshSigningKeyName>.pub       — gitconfig user.signingKey
//	~/.config/aws/readonly-config        — read via AWS_CONFIG_FILE env (#2234)
//	~/.config/aws/credentials            — read via AWS_SHARED_CREDENTIALS_FILE env (#2234)
//	~/.config/kube/agents-config         — read via KUBECONFIG env (#2235)
//
// Each source is resolved via filepath.EvalSymlinks; when the resolved
// target is a sops secrets.d path (…/secrets.d/<N>/<name>), <name> is
// returned. Sources that are absent, unresolvable, or not sops-backed are
// skipped — they need no exception because the secrets.d deny never covers
// them. The returned names are deduplicated and keep source order so the
// emitted profile is deterministic.
//
// This list is the enforcement half of the inventory in issue #2211: every
// other name under secrets.d/<N>/ (github_token, the role PATs, aws-config,
// workkube, gitlab_token, …) stays denied. Do NOT add a source here merely
// because a secret exists — only because an in-sandbox consumer reads it.
func collectSecretsDAllowlistNames(m *Manager, home string) []string {
	if home == "" {
		return nil
	}
	accessKeyName := m.cfg.SshAccessKeyName
	if accessKeyName == "" {
		accessKeyName = "prismatic-koi-ed25519"
	}
	signingKeyName := m.cfg.SshSigningKeyName
	if signingKeyName == "" {
		signingKeyName = "prismatic-koi-ed25519-signingkey"
	}
	sources := []string{
		filepath.Join(home, ".ssh", accessKeyName),
		filepath.Join(home, ".ssh", accessKeyName+".pub"),
		filepath.Join(home, ".ssh", signingKeyName),
		filepath.Join(home, ".ssh", signingKeyName+".pub"),
		filepath.Join(home, ".config", "aws", "readonly-config"),
		filepath.Join(home, ".config", "aws", "credentials"),
		filepath.Join(home, ".config", "kube", "agents-config"),
	}
	seen := map[string]bool{}
	var names []string
	for _, src := range sources {
		resolved, err := filepath.EvalSymlinks(src)
		if err != nil {
			continue // absent on host — no exception needed (mirrors symlinkIfExists)
		}
		name, ok := secretsDRelativeName(resolved)
		if !ok || seen[name] {
			continue
		}
		if strings.Contains(name, `"`) {
			// A double-quote cannot be safely embedded in the SBPL #"…"
			// regex literal. No real sops secret name contains one; skip
			// defensively rather than emit a malformed profile.
			log.Printf("container: sandbox-exec: skipping secrets.d allowlist name with embedded quote: %q", name)
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// secretsDRelativeName extracts the secrets.d-relative secret name from a
// resolved sops target path of the form …/secrets.d/<N>/<name…> where <N>
// is the all-digits generation counter. Returns ("", false) when the path
// is not a sops secrets.d path.
func secretsDRelativeName(resolved string) (string, bool) {
	const marker = "/secrets.d/"
	idx := strings.Index(resolved, marker)
	if idx < 0 {
		return "", false
	}
	rest := resolved[idx+len(marker):] // "<N>/<name…>"
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return "", false
	}
	for _, c := range rest[:slash] {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	name := rest[slash+1:]
	if name == "" || strings.HasSuffix(name, "/") {
		return "", false
	}
	return name, true
}

// regexQuotePath escapes the AppleMatch/POSIX-ERE metacharacters in s so it
// matches literally inside an SBPL (regex #"…") filter. Only basic escapes
// are used (backslash before the metacharacter) — no perl-style \Q…\E,
// which Apple's regex engine does not support.
func regexQuotePath(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '.', '+', '*', '?', '(', ')', '[', ']', '{', '}', '|', '^', '$':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// quoteSBPL returns the path quoted for inclusion in an SBPL expression.
// SBPL uses double-quoted strings; any embedded double-quote or backslash is
// escaped. In practice macOS paths almost never contain these characters, but
// we escape defensively.
func quoteSBPL(path string) string {
	escaped := strings.ReplaceAll(path, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return "\"" + escaped + "\""
}

// sandboxExecProfilePath returns the host path for the SBPL profile temp
// file written before sandbox-exec is launched. The path is namespaced by
// the session name so concurrent sessions never collide.
func (m *Manager) sandboxExecProfilePath() string {
	return m.tempPath("sandbox-exec-profile", ".sb")
}

// writeProfile materialises the generated SBPL profile to a temp file and
// returns its absolute path. The file is owned by the invoking user and
// readable only by them (0600) — sandbox-exec reads the file before exec'ing
// the harness, so the user's own read access is sufficient.
func writeProfile(m *Manager) (string, error) {
	path := m.sandboxExecProfilePath()
	content := generateProfile(m)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("container: sandbox-exec: write profile %s: %w", path, err)
	}
	return path, nil
}

// BuildArgs constructs the sandbox-exec argument list:
//
//	sandbox-exec -f <profile_path> pi [--agent X] [--prompt Y]
//
// The first element ("sandbox-exec") is argv[0]; the caller invokes
// syscall.Exec("/usr/bin/sandbox-exec", args, env). After -f and the profile
// path comes the harness binary (pi) and its arguments — the inner
// command sandbox-exec executes inside the SBPL sandbox.
//
// The harness invocation mirrors bwrap.go:BuildArgs (see lines 608–636) so
// that the sandbox interior behaves identically across isolation modes:
//
//   - --port: cfg.AllocatedPort, falling back to ContainerPort when 0.
//   - --hostname 127.0.0.1: same rationale as bwrap (host network namespace
//     is shared on both modes; binding 0.0.0.0 would be overly broad).
//   - --agent <role>: appended when cfg.AgentRole is non-empty.
//   - --prompt <text>: appended when cfg.InitialPrompt is non-empty.
//
// The minimal read-only profile in this PR does not yet wire HOME, working
// directory, or environment overrides through the profile generator —
// those land in PR 3 (#1017). The harness still inherits the env passed to
// syscall.Exec, which agent-run filters via minimalIsolatedExecEnv.
func (s *sandboxExecIsolator) BuildArgs(m *Manager) []string {
	cfg := m.cfg

	profilePath := m.sandboxExecProfilePath()

	args := []string{"sandbox-exec", "-f", profilePath}
	args = append(args, PIInvocation(cfg)...)

	return args
}

// Run is unused in the production flow — agent-run launches sandbox-exec
// directly as a supervised child of the tmux pane (see
// cmd/agent_run_sandbox_exec_darwin.go) rather than going through the
// Isolator's Run path. The method satisfies the Isolator interface and
// stores the resulting *exec.Cmd in s.cmd so that Shutdown can deliver
// SIGTERM/SIGKILL via the shared gracefulShutdown helper. Returns an
// error after the child exits.
//
// The first argv element from BuildArgs is "sandbox-exec" (matching the
// bwrap convention); we pass args[1:] to exec.CommandContext because Go
// prepends argv[0] from the binary path.
func (s *sandboxExecIsolator) Run(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("container: sandbox-exec %q: Run called with empty args", s.name)
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", args[1:]...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container: sandbox-exec run %q: %w", s.name, err)
	}
	return nil
}

// Shutdown sends SIGTERM to the sandbox-exec child if it is still running,
// waits up to 30 seconds, and sends SIGKILL if the process has not exited.
// The SIGTERM-then-grace-then-SIGKILL body is shared with bwrap via the
// gracefulShutdown helper (shutdown.go, A2.GR).
//
// In the production agent-run flow the sandbox-exec child is owned by
// cmd/agent_run_sandbox_exec_darwin.go (which manages its own supervised
// Wait loop with kqueue parent-death watching). The Isolator's Shutdown is
// therefore a no-op when no Run-managed child has been registered (s.cmd
// is nil) — preserving the pre-A2.GR behaviour. When a future test or
// caller invokes Run on this isolator, Shutdown will deliver the same
// SIGTERM-then-SIGKILL sequence as bwrap.
func (s *sandboxExecIsolator) Shutdown() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	gracefulShutdown(cmd, defaultGracefulShutdownGrace)
}

// HasExited returns the recorded exit state. In the production flow this is
// always (false, 0) because agent-run replaces its own process via
// syscall.Exec — the Manager-level Isolator never observes the child.
func (s *sandboxExecIsolator) HasExited() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited, s.exitCode
}

// DumpLogs logs a message indicating that log capture is not implemented
// for sandbox-exec (stdout/stderr inherit agent-run's terminal in the
// production flow).
func (s *sandboxExecIsolator) DumpLogs() {
	log.Printf("container: sandbox-exec %q: live output is forwarded via the inherited terminal during syscall.Exec", s.name)
}

func init() {
	MustRegister(config.IsolationSandboxExec, func(opts ConstructorOpts) Isolator {
		return newSandboxExecIsolator(opts.Name)
	})
}

// MinimalIsolatedExecEnv filters a hostEnv slice (K=V pairs, as returned by
// os.Environ()) down to the minimal allow-list that the isolation harness
// (bwrap on Linux, sandbox-exec on Darwin) needs. It is the same logic as
// the original minimalBwrapExecEnv — the filter is identical across modes,
// so a single helper keeps both call sites in sync.
//
// The returned env is what the harness *process* sees on the host; it is
// NOT the sandbox interior env. In bwrap mode the sandbox interior is
// rebuilt via --setenv pairs in BuildArgs after --clearenv. In sandbox-exec
// mode the sandbox shares the harness env, so this filter is the only line
// of defence against host-shell secrets reaching the sandbox interior in
// this PR.
//
// See cmd/agent_run.go for the corresponding filter at the syscall.Exec
// call site. Both filters use the same allow-list; keep them in sync.
//
// Exported because cmd/agent_run.go also filters env at the syscall.Exec
// boundary using exactly this allow-list.
func MinimalIsolatedExecEnv(hostEnv []string) []string {
	allow := map[string]bool{
		"PATH":      true,
		"HOME":      true,
		"USER":      true,
		"LOGNAME":   true,
		"TERM":      true,
		"COLORTERM": true,
		"LANG":      true,
		"LC_ALL":    true,
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
