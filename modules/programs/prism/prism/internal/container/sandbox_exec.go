// Package container manages sandbox lifecycle and mount preparation for
// prism agent sessions.
// This file defines sandboxExecIsolator, an Apple sandbox-exec-based
// implementation of the Isolator interface. It is symmetric to bwrapIsolator
// in bwrap.go: BuildRunArgs() is a no-op stub, BuildArgs(m *Manager) is the
// concrete argument builder that has access to the Manager's config and state.
//
// This file implements the sandbox-exec design: explicit SBPL grants on
// real host paths, env-var injection at host XDG paths, and a small
// per-session work dir (session_work_dir.go). $HOME inside the sandbox is
// the REAL host home. New top-level allow/deny clauses require an
// explanatory comment in the generator.
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
	"github.com/prismatic-koi/prism/internal/usage"
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
	// gracefulShutdown helper (shutdown.go).
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
//   - RequiresProfilesFile: profiles.json supplies the per-role slot.
//   - IsSandboxed: the agent runs in a sandbox (see Capabilities.IsSandboxed).
//   - RestartOnExit is false: sandbox-exec replaces the agent-run process via
//     syscall.Exec, so the sidecar does not observe process exit to restart.
func (s *sandboxExecIsolator) Capabilities() Capabilities {
	return Capabilities{
		IsContainer:                false,
		OwnsContainerLifecycle:     false,
		RequiresProfilesFile:       true,
		IsSandboxed:                true,
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

// goCacheDirs returns the Go cache directories the sandbox-exec profile
// grants read-write (section 5k of generateProfile), derived
// from the given home directory. It returns nil when home is empty.
//
// The two entries are the Go toolchain's Darwin DEFAULTS:
//
//	<home>/go/pkg/mod                 GOMODCACHE (GOPATH=<home>/go)
//	<home>/Library/Caches/go-build    GOCACHE    (os.UserCacheDir()/go-build)
//
// The GOOS is pinned to darwin rather than read from runtime.GOOS because
// this profile IS the Darwin isolator — sandbox-exec runs nowhere else
// (config.ValidIsolationModes) — and because generateProfile's assertions
// must produce Darwin paths when the test suite runs on a Linux host.
//
// The path list itself lives in go_cache.go (goCacheDirsForGOOS), which is
// the single platform-aware source of truth shared with the Linux bwrap
// mounts. Within Darwin this function remains the one
// source for the pair: generateProfile grants these paths, the section-22
// deny narrows execution on the one with execDenied set, and
// ensureGoCacheDirs creates them — so the grant and the directory cannot
// drift apart.
func goCacheDirs(home string) []goCacheDir {
	return goCacheDirsForGOOS(home, goosDarwin)
}

// GoToolchainEnvVar is the environment variable Go consults to decide whether
// it may switch to a different toolchain than the one being run.
const GoToolchainEnvVar = "GOTOOLCHAIN"

// GoToolchainLocal pins the in-sandbox toolchain to the one on PATH — the
// nix-provided Go — and disables the auto-download-and-exec path entirely.
const GoToolchainLocal = "local"

// GoToolchainEnv returns the env var pair that makes nix authoritative for the
// Go toolchain inside a sandbox.
//
// # Policy
//
// A sandboxed agent must not silently download an unpinned Go toolchain from
// the internet and execute it out of a shared cache. Nix provides the
// toolchain; GOTOOLCHAIN=local makes that authoritative. When a project's
// go.mod requires a newer toolchain than nix ships, the build fails loudly
//
//	go: go.mod requires go >= 1.27.0 (running go1.26.5)
//
// and the fix is to bump the nix-pinned Go — never to widen the sandbox.
//
// # Why this is load-bearing, not hygiene
//
// Go's built-in default is GOTOOLCHAIN=auto. Under auto, when go.mod's
// go/toolchain directive exceeds the running toolchain, cmd/go downloads
// module golang.org/toolchain@v0.0.1-<ver>.<goos>-<goarch> INTO GOMODCACHE,
// sets the exec bits on the extracted tree, and execs <dir>/bin/go
// (cmd/go/internal/toolchain/select.go: download at :357, exec bits at
// :414-424, execGoToolchain at :458).
//
// That exec lands inside the directory the section-22 deny covers, so without
// this pin the deny would break the very gate section 5k exists to enable —
// failing as "go: exec go1.X.Y: operation not permitted".
// The alternative (carving golang.org/toolchain@* out of the deny) was
// rejected: section 5k grants the module cache read-write, so a predictable
// and agent-writable carve-out path is a bypass that masquerades as
// protection.
//
// The pin cannot be overridden from inside the sandbox: prism forwards no
// GO* variable, and go's env file (~/Library/Application Support/go/env) has
// no grant, so this injected value is the only GOTOOLCHAIN the toolchain
// sees.
func GoToolchainEnv() []string {
	return []string{GoToolchainEnvVar + "=" + GoToolchainLocal}
}

// ensureGoCacheDirs creates the section-5k Go cache directories on the host
// before the sandbox starts.
//
// It is required because a (subpath ...) grant on a path that does not exist
// is a silent no-op, and the sandboxed process cannot create the path itself:
// MkdirAll would first have to mkdir the UNGRANTED parents (~/go, ~/go/pkg,
// ~/Library/Caches) and gets EPERM there. Without this, the documented
// quality gate would still fail on any host that
// has never run go outside a sandbox.
//
// Creation is best-effort and the mode is 0o755 — see createGoCacheDirs in
// go_cache.go, which both isolators share (the Linux half pre-creates the
// same way from prepareVolumeDirs).
func ensureGoCacheDirs() {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	createGoCacheDirs("sandbox-exec", goCacheDirs(home))
}

// generateProfile returns the SBPL profile content for this session.
//
// This is a (version 3) SBPL profile.
//
// The profile shape includes:
//   - Cryptex graft points (dyld shared cache, macOS 15+)
//   - Read-only system roots with file-test-existence, file-map-executable,
//     file-read-metadata alongside file-read* (required by dyld/AMFI)
//   - /bin, /sbin, and /var/... symlink-alias forms (v3 additions)
//   - The per-session TMPDIR read-write (/var/tmp/prism-<hash>) — the only temp
//     tree the session can write. /tmp, /private/tmp, and the user $TMPDIR
//     have no write grant
//   - Deny of sensitive /private/etc subtrees (wireguard, wpa_supplicant, ssh),
//     both /etc/... and /private/etc/... forms (symlink non-transparency)
//   - sops secrets.d deny (read + write) with named re-allow exceptions for
//     exactly the inventoried agent-needed secret names
//   - Host ~/.aws deny with sso/cli carve-outs (config/credentials are
//     delivered via env vars at the host XDG paths; the carve-outs are the
//     sole capability for sso/cli)
//   - Literal RO grant for the real ~/.ssh/known_hosts — never (subpath ~/.ssh)
//   - RW subpath grant for ~/.config/claude — claude-code's config dir,
//     reached via the CLAUDE_CONFIG_DIR env var
//   - RW subpath grants for ~/.cache/nix, ~/.cache/bun, ~/.cache/twg, ~/.npm,
//     ~/.mcp-auth, ~/.config/twg
//   - RO subpath grants for ~/.cache/prism/clipboard and
//     ~/.config/prism/agents, plus an RO grant for the ~/.nix-profile
//     symlink and its resolved target
//   - RO subpath grant for the prism usage snapshot dir
//     ($XDG_STATE_HOME/prism/usage) — the bottom-bar usage reader
//   - RW subpath grants for the two Go cache dirs — ~/go/pkg/mod and
//     ~/Library/Caches/go-build — so the repo AGENTS.md quality gate
//     (`go build ./...` / `go test ./...`) runs as documented
//   - Session work dir / worktree / host-API socket dir (RW) — the work dir
//     (subpath <sessionDir>) and the per-session TMPDIR are the two
//     per-session writable grants
//   - The project root (BareRoot) read-only, and a write of only the git
//     paths that the session needs (section 6e, #3293)
//   - Process and IPC primitives required by dyld, AMFI, and the agent
//   - (allow network*), narrowed by a deny of every Unix-socket connect
//     except an allowlist (section 17b). No shared temp tree is on the
//     allowlist, so a host socket in /private/tmp or in the user $TMPDIR
//     (tmux, the user ssh-agent, AeroSpace, podman, nvim, and any other)
//     is not reachable
//   - A final deny on unlink, create, mode writes, flag writes, and ACL
//     writes of the per-session TMPDIR node (section 21e), so the session
//     cannot swap the dir for a symlink, open it to other users, or lock it
//     against cleanup
//
// New top-level allow/deny clauses require an explanatory comment in the
// generator.
func generateProfile(m *Manager) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// Derive the per-session work dir. When it cannot be
	// determined (e.g. no home dir) the derived rules are simply omitted.
	sessionDir, sessionDirErr := m.sessionWorkDirPath()

	// The git write scope (sections 6e, 21i, and 21j). It reads a file that
	// the session can write, so read it once: the three sections must name
	// the same git dir.
	gitScope, _ := m.gitScope()

	// The per-session TMPDIR, in both /var symlink forms. Empty when the
	// path cannot be derived; Prepare then fails before the profile is used.
	var sessionTmpForms []string
	if sessionTmpDir, err := m.sessionTmpDirPath(); err == nil {
		sessionTmpForms = symlinkForms(sessionTmpDir)
	}

	var sb strings.Builder
	// ── Version 3 header and deny-default ────────────────────────────────
	// (version 3) unlocks syscall-unix, syscall-mach, system-mac-syscall,
	// system-fcntl, and the split ipc-posix-shm-read*/write* operations —
	// all required for Apple-signed binaries (dyld, AMFI). See F.1.
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
	//   No separate SBPL rule is needed for the PI extension.
	// /usr, /bin, /sbin — standard Apple-signed utility directories.
	// /System, /Library — OS frameworks, dylibs, and shared data.
	// /Applications/Xcode.app — xcrun (called by /usr/bin/git shim).
	// /etc, /private/etc — both forms: sandbox-exec does NOT transparently
	//   follow the /etc → /private/etc symlink.
	// /private/var/db/dyld, /var/db/dyld — dyld shared-cache DB;
	//   /var is a symlink to /private/var; both forms needed. See F.1 §2.
	// /private/var/db/timezone, /var/db/timezone — timezone data.
	// /private/var/select, /var/select — xcode-select developer_dir.
	// /private/var/folders, /var/folders — Darwin per-user TMPDIR. Read
	//   only: the sops secrets resolve into it (section 3c narrows the read).
	//   No rule grants a write there. The session writes temp files in its
	//   per-session TMPDIR (section 3).
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

	// ── 3. Per-session TMPDIR read-write ──────────────────────────────────
	// The dispatcher sets TMPDIR and MAC_CHROMIUM_TMPDIR to this dir
	// (session_tmp_dir.go). Thus the tools that read TMPDIR (go test, node,
	// bun, nix) and chromium put their temp files and their Unix sockets
	// here. Section 17b allows a connect to a socket in it.
	//
	// It is the only temp tree that the session can write. /tmp,
	// /private/tmp, and the user $TMPDIR (/var/folders/<a>/<b>/T) have no
	// write grant and no section-17b connect allowance. Thus the session
	// cannot create, rename, or remove a file there, and cannot reach a
	// socket there: tmux, the user ssh-agent, AeroSpace, a .NET diagnostic
	// socket, the podman machine, nvim, or any other (#3150). A connect
	// through a symlink matches the resolved path, and the profile has no
	// file-link allow, so the session cannot plant a hard link either.
	//
	// Do not add a write grant on a shared temp tree, and do not add one to
	// section 17b. A path-based connect deny holds only while the session
	// cannot rename the socket or an ancestor dir of it: with a write grant,
	// the session moves a host socket into its TMPDIR and connects there.
	//
	// The dir of another session is a sibling under /private/var/tmp. No rule
	// covers it, so the session cannot read it, write it, or connect in it.
	//
	// The dir is under /var/tmp, not /tmp: macOS tmp_cleaner deletes an
	// empty dir under /tmp that has not changed for 3 days, and the session
	// cannot make it again (see session_tmp_dir.go).
	//
	// Both /var symlink forms are listed. file-map-executable lets a tool
	// dlopen a native module that it extracts to TMPDIR. The literal
	// metadata allow on each ancestor node of the TMPDIR (/var, /var/tmp,
	// /private/var/tmp, /private/var, /private) lets a tool resolve the /var
	// symlink (realpath, filepath.EvalSymlinks) when the section-6b ancestor
	// allow is absent. It grants stat only, not a directory list.
	//
	// The session can write the node of its own TMPDIR, because a subpath
	// covers its root. Section 21e takes back unlink, create, mode writes,
	// flag writes, and ACL writes on the node.
	if len(sessionTmpForms) > 0 {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata file-map-executable\n")
		for i, p := range sessionTmpForms {
			sb.WriteString("  (subpath " + quoteSBPL(p) + ")")
			if i == len(sessionTmpForms)-1 {
				sb.WriteString(")")
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
		sb.WriteString(ancestorMetadataAllow(sessionTmpForms))
		sb.WriteString("\n")
	}

	// ── 3c. sops secrets.d deny + named re-allow exceptions ─
	// The broad /private/var/folders read allow above (section 2) exposes
	// the entire home-manager sops-nix secrets tree
	// (~/.config/sops-nix/secrets → /var/folders/<…>/T/secrets.d/<N>/) to the
	// sandbox: the daily-driver GitHub PAT, full-power AWS config, admin kube
	// configs, hass/notion/syncthing keys, the non-prism SSH keys, and
	// the secrets.d/age-keys.txt copy. Almost none of those are read
	// in-sandbox — GITHUB_TOKEN, OPENROUTER_API_KEY, etc. are resolved
	// host-side by agent-run (credentialEnvVars, including the
	// GitHubTokenPath file fallback) and injected into the sandbox env as
	// VALUES before sandbox-exec starts. The exceptions are the few secrets
	// that ARE read in-sandbox — gitlab_token and the pi
	// grafana MCP config bundle — see
	// collectSecretsDAllowlistNames.
	//
	// Shape:
	//   1. deny file-write* on the secrets.d subtree, no exceptions — nothing
	//      in-sandbox ever writes a secret; this also protects the allowlisted
	//      names from in-sandbox tampering.
	//   2. deny file-read* on the secrets.d subtree, with require-not
	//      exceptions for exactly the inventoried agent-needed secret NAMES
	//      (collectSecretsDAllowlistNames: ssh access key + .pub, signing key
	//      + .pub, aws readonly-config, aws credentials, kube agents-config,
	//      plus the two config-gated names gitlab_token and the grafana config
	//      bundle — each derived from its stable host source path, so a source
	//      that is absent, unconfigured, or not sops-backed simply produces no
	//      exception).
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
	// Rotation safety: sops rotates secrets.d/<N> → <N+1> on
	// every activation, but secret NAMES are stable. The exception regexes
	// match any counter ([0-9]+), so allowlisted reads survive a rotation
	// mid-session and denied reads stay denied — by construction. The deny
	// prefixes are static regexes covering both the /var and /private/var
	// symlink forms and do not depend on os.TempDir(), so an unset or odd
	// TMPDIR cannot silently re-expose the real tree.
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
	// non-transparency (same pattern for v1). See F.1 §2 rule 4.
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
	// paths — nothing agent-needed lives at the raw ~/.aws
	// canonical paths. The host raw ~/.aws subtree must remain read-denied
	// so that path traversal cannot reach host credentials there.
	//
	// Exception: ~/.aws/sso and ~/.aws/cli must remain accessible so that
	// AWS SSO auth and tools that read SSO tokens (e.g. kubectl) work inside
	// the sandbox. The carve-out allows below are the SOLE in-sandbox
	// capability for them, at the real host paths.
	// The more-specific allow rules below override the broad deny for
	// exactly these two subdirs (SBPL evaluates more-specific rules as
	// overrides of broader ones).
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
		// comment above awsSSOReadOnly/awsCLIReadOnly in mounts.go.
		awsSSOPath := filepath.Join(home, ".aws", "sso")
		awsCLIPath := filepath.Join(home, ".aws", "cli")
		sb.WriteString("(allow file-read* file-write*\n")
		sb.WriteString("  (subpath " + quoteSBPL(awsSSOPath) + ")\n")
		sb.WriteString("  (subpath " + quoteSBPL(awsCLIPath) + "))\n")
		sb.WriteString("\n")
	}

	// ── 5b. Nix flake trusted-settings read ────────────────
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

	// ── 5c. Real ~/.ssh/known_hosts read-only literal ──────
	// The generated <sessionDir>/ssh-config is passed to ssh via -F
	// (GIT_SSH_COMMAND), and openssh resolves its default UserKnownHostsFile
	// against the real home (getpwuid → pw_dir, not $HOME) — so ssh inside
	// the sandbox reads the real ~/.ssh/known_hosts. This explicit grant is
	// the sole capability for that read.
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

	// ── 5d. Claude config dir read-write ───────────────────
	// claude-code resolves its config dir (and .claude.json) via the
	// CLAUDE_CONFIG_DIR env var at the host XDG path ~/.config/claude
	// (declared in agent.envVars by the nix module). Unlike the aws/kube
	// XDG configs, ~/.config/claude is a plain host directory — NOT a sops
	// symlink — so the secrets.d allowlist plays no part here: this
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

	// ── 5e. RW cache/auth dir grants at the real host paths ─
	// Explicit RW subpath grants on the real host paths
	// for ~/.cache/nix, ~/.cache/bun, ~/.cache/twg, ~/.npm, ~/.mcp-auth, and
	// ~/.config/twg. The consumers derive the paths from $HOME /
	// XDG_CACHE_HOME / TWG_CACHE_DIR, which point at the real host paths, so
	// these grants are load-bearing. None of these paths is sops-backed — the
	// allowlist plays no part; these grants are the sole capability.
	//
	// ~/.cache/twg holds the twg CLI help index and last-error.json, reached
	// via TWG_CACHE_DIR (modules/programs/twg.nix). Grant the leaf only,
	// never (subpath ~/.cache) and never the macOS default
	// ~/Library/Caches/twg: a wider grant exposes the host cache tree.
	//
	// ~/.config/twg holds the twg CLI OAuth credentials. twg writes
	// install.json on each run and writes back the access token when it
	// refreshes it, so the grant is RW. Grant the leaf only, never
	// (subpath ~/.config): a wider grant exposes host config and secrets.
	//
	// Mirrors bwrap's --bind (RW) treatment of the same dirs in mounts.go
	// StandardSandboxMounts. Emitted even when a dir does not exist on the
	// host — sandbox-exec silently ignores (subpath ...) rules for
	// non-existent paths (same shape as the ~/.pi/agent rule in 6a), so fresh
	// machines without e.g. ~/.mcp-auth are unaffected.
	if home != "" {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + quoteSBPL(filepath.Join(home, ".cache", "nix")) + ")\n")
		sb.WriteString("  (subpath " + quoteSBPL(filepath.Join(home, ".cache", "bun")) + ")\n")
		sb.WriteString("  (subpath " + quoteSBPL(filepath.Join(home, ".cache", "twg")) + ")\n")
		sb.WriteString("  (subpath " + quoteSBPL(filepath.Join(home, ".npm")) + ")\n")
		sb.WriteString("  (subpath " + quoteSBPL(filepath.Join(home, ".mcp-auth")) + ")\n")
		sb.WriteString("  (subpath " + quoteSBPL(filepath.Join(home, ".config", "twg")) + "))\n")
		sb.WriteString("\n")
	}

	// ── 5f. RO grants at the real host paths ─────────────
	// RO subpath grants on the real host paths for ~/.cache/prism/clipboard
	// (images staged by `prism clipboard paste-image`; the agent reads them at
	// the absolute host path) and ~/.config/prism/agents (role prompt markdown
	// read by the prism PI extension at before_agent_start).
	// Read-only — agents never write their own role
	// prompt and only read staged clipboard images; RO must not silently
	// become RW. Emitted even when a dir does not exist on the host (see 5e).
	if home != "" {
		sb.WriteString("(allow file-read* file-test-existence\n")
		sb.WriteString("  (subpath " + quoteSBPL(filepath.Join(home, ".cache", "prism", "clipboard")) + ")\n")
		sb.WriteString("  (subpath " + quoteSBPL(filepath.Join(home, ".config", "prism", "agents")) + "))\n")
		sb.WriteString("\n")
	}

	// ── 5g. ~/.nix-profile RO grant — symlink node + resolved target ─────
	// ~/.nix-profile is itself a SYMLINK on the host
	// (→ ~/.local/state/nix/profiles/profile → … → /nix/store/…), and SBPL
	// path filters evaluate the RESOLVED target for open(2)-class operations,
	// so the grant must work through resolution:
	//
	//   - The (literal ~/.nix-profile) allow covers operations on the symlink
	//     NODE itself — readlink(2)/lstat(2) — which are evaluated against
	//     the link path, not the target.
	//   - The EvalSymlinks-resolved target gets its own RO rule (subpath for
	//     a dir, literal for a file) so reads THROUGH the link succeed. On
	//     hm/nix-darwin hosts the chain typically lands in /nix/store (already
	//     readable via the §2 /nix allow — the explicit rule is then a
	//     harmless duplicate), but the rule is load-bearing wherever the
	//     profile dir lives outside /nix. Resolution failure (no
	//     ~/.nix-profile on the host) skips the target rule; the literal is
	//     still emitted (sandbox-exec ignores rules for non-existent paths).
	//
	// Read-only throughout — nothing in-sandbox may mutate the profile.
	if home != "" {
		nixProfile := filepath.Join(home, ".nix-profile")
		sb.WriteString("(allow file-read* file-test-existence\n")
		sb.WriteString("  (literal " + quoteSBPL(nixProfile) + ")")
		if resolved, evalErr := filepath.EvalSymlinks(nixProfile); evalErr == nil && resolved != nixProfile {
			sb.WriteString("\n")
			if info, statErr := os.Stat(resolved); statErr == nil && info.IsDir() {
				sb.WriteString("  (subpath " + quoteSBPL(resolved) + ")")
			} else {
				sb.WriteString("  (literal " + quoteSBPL(resolved) + ")")
			}
		}
		sb.WriteString(")\n")
		sb.WriteString("\n")
	}

	// ── 5h. prism profiles.json single-file RO read ────────
	// The CLI's `prism profile list`, `prism profile show`, and the
	// available_profiles section of `prism agent-context` open
	// ~/.config/prism/profiles.json directly via
	// internal/config/profiles.go::LoadProfiles. Under deny-default the
	// read fails EPERM and the user sees a misleading "not found — run the
	// system rebuild" error from inside any sandbox session, even though
	// the file exists on the host. The mutation surface (`prism profile
	// use`) routes through the host API and is unaffected.
	//
	// The file holds the user's declarative model-profile configuration;
	// its contents are not secret — same trust level as the sibling
	// agents/ markdown that 5f already grants. The allow is read-only and
	// single-file (literal, not subpath): the rest of ~/.config/prism/
	// (e.g. ~/.config/prism/accounts/, runtime-mutable state)
	// stays out of the sandbox by default. RO must not silently become
	// RW — nothing in-sandbox may mutate the host's profile config.
	// file-test-existence is included so LoadProfiles' missing-file branch
	// sees ENOENT (preserving the existing host error message verbatim)
	// rather than EPERM when the file is absent on a fresh install
	// before first `nh switch`.
	//
	// Mirrors bwrap's RO single-file mount of the same path in mounts.go
	// StandardSandboxMounts.
	if home != "" {
		profilesJSON := filepath.Join(home, ".config", "prism", "profiles.json")
		sb.WriteString("(allow file-read* file-test-existence\n")
		sb.WriteString("  (literal " + quoteSBPL(profilesJSON) + "))\n")
		sb.WriteString("\n")
	}

	// ── 5i. ~/Library/Keychains/login.keychain-db RO grant ──
	// Keychain-using CLIs that ship as third-party tools (the Datadog `pup`
	// CLI is the canonical example; other Rust binaries built on
	// the `keyring` crate share the code path) reach the user's legacy login
	// keychain through the Security Framework — SecKeychainFindGenericPassword /
	// SecItemCopyMatching.
	// The calling process reads ~/Library/Keychains/login.keychain-db itself.
	// Before the read, the Security framework calls sandbox_check with
	// SANDBOX_CHECK_NO_REPORT for file-read-data on that path. If the sandbox
	// denies the check, the framework skips the keychain and makes no open(2).
	// Without this grant, the lookup returns errSecItemNotFound (-25300) and
	// the kernel logs no deny. The application sees "credential not found".
	// This is the symptom of the pup OAuth token that is unreachable in-sandbox.
	//
	// Shape: single-file (literal ...), NOT
	// (subpath ~/Library/Keychains). The narrowing is load-bearing: the modern
	// UUID-keyed databases (keychain-2.db, user.kb) and the TrustedPeersHelper
	// sibling files in the same directory MUST remain unreadable from inside
	// the sandbox. Read-only — nothing in-sandbox writes to the user keychain.
	//
	// A per-session staging-HOME symlink is deliberately NOT paired with this
	// grant. pup and other keyring-crate consumers go through the Security
	// Framework directly via securityd, which keys the search list off the
	// calling UID — $HOME is never consulted. Only /usr/bin/security
	// dump-keychain would need $HOME, and nothing in-sandbox uses it.
	//
	// Rule is emitted unconditionally (no os.Stat guard): sandbox-exec
	// silently ignores (literal ...) rules for non-existent paths (same
	// pattern as the 5d/5e/5f/5g/5h/6a real-host-path allows), so fresh
	// machines without a user keychain yet remain unaffected.
	if home != "" {
		loginKeychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (literal " + quoteSBPL(loginKeychain) + "))\n")
		sb.WriteString("\n")
	}

	// ── 5j. prism usage snapshot dir RO subpath read ───────
	// The bottom-bar usage segment reads current.json out of this directory
	// (pi/extensions/prism.ts::readUsageSnapshot). Under
	// deny-default the open fails and the reader — which degrades silently
	// by design — renders nothing, so the whole feature was invisible in
	// every sandboxed session. `prism account usage` from inside a session
	// reads the same directory via internal/usage.ReadAll.
	//
	// Path resolution: usage.DirForHome is the single source of truth for
	// the order ($XDG_STATE_HOME first, then <home>/.local/state), shared
	// with usage.DefaultDir (the writer) and usageSnapshotPath() in
	// prism.ts (the reader). Grant the RESOLVED host path rather than a
	// hardcoded ~/.local/state: on a host exporting a non-default
	// $XDG_STATE_HOME the snapshots live elsewhere and the hardcoded path
	// would grant an empty directory.
	//
	// Shape: (subpath <usageDir>) — the LEAF directory only, never a
	// parent. The parent $XDG_STATE_HOME/prism holds prism.db and run/
	// (every session's host-API socket dir, isolated per session);
	// $XDG_STATE_HOME itself holds unrelated
	// application state. Subpath rather than (literal <dir>/current.json)
	// because the writer replaces the file by atomic rename and
	// `prism account usage` also reads the sibling <account>.json files.
	//
	// READ-ONLY — no file-write*. The display only reads, and every writer
	// goes through the sidecar endpoint POST /usage/snapshot,
	// so nothing in-sandbox needs write access. RO also stops a compromised
	// session forging usage figures on the host. RO must not silently
	// become RW. Mirrors bwrap's --ro-bind of the same directory in
	// mounts.go StandardSandboxMounts.
	//
	// Emitted even when the dir does not exist on the host — sandbox-exec
	// silently ignores (subpath ...) rules for non-existent paths (same
	// shape as the 5d/5e/5f grants), so a host that has never captured a
	// snapshot is unaffected and the session starts normally.
	if usageDir := usage.DirForHome(home); usageDir != "" {
		sb.WriteString("(allow file-read* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + quoteSBPL(usageDir) + "))\n")
		sb.WriteString("\n")
	}

	// ── 5k. Go module cache + build cache RW ───────────────
	// The repo AGENTS.md names `go build ./...` and `go test ./...` (run from
	// modules/programs/prism/prism/) as "the first check for any prism code
	// change". Under deny-default both commands fail in a Darwin worker:
	// the Go toolchain writes downloaded modules to GOMODCACHE and compiled
	// packages to GOCACHE, and neither path had a grant. The repo's primary
	// quality gate was therefore unrunnable as documented on one of the two
	// supported platforms.
	//
	// Path resolution: these are the Go DEFAULTS on Darwin, hardcoded rather
	// than read from the host's GOPATH / GOMODCACHE / GOCACHE / GOENV. That
	// is exact, not an approximation: prism builds the sandbox env explicitly
	// (buildSandboxExecHomeEnv in cmd/agent_run_sandbox_exec_darwin.go) and
	// forwards no GO* variable, and go's env file — $HOME/Library/Application
	// Support/go/env — is not granted, so the in-sandbox toolchain always
	// falls back to GOPATH=$HOME/go (→ GOMODCACHE=$HOME/go/pkg/mod) and
	// GOCACHE=os.UserCacheDir()/go-build = $HOME/Library/Caches/go-build.
	// Reading the host's GO* env here would ALSO be a widening vector — a
	// host variable would then steer a sandbox grant — so the generator
	// deliberately does not consult it. goCacheDirs is the single source of
	// truth, shared with ensureGoCacheDirs (see below) so the grant and the
	// created directory cannot drift apart.
	//
	// Scope — the LEAF cache dirs only, never a parent:
	//   - NOT (subpath ~/go): that would expose ~/go/bin, where `go install`
	//     drops binaries that are typically on the host's PATH. A sandboxed
	//     agent must not be able to plant an executable the user later runs.
	//   - NOT (subpath ~/Library/Caches): the user's whole cache tree,
	//     including the daily-driver browser and application caches.
	//   - NOT ~/Library/Application Support/go: GOENV and the Go telemetry
	//     dir stay denied. go tolerates an unreadable env file and telemetry
	//     is best-effort, so nothing needs them.
	//
	// Read-write is load-bearing for both: go writes module downloads plus
	// its cache/lock file under GOMODCACHE, and build/test outputs under
	// GOCACHE. A read-only grant does not make the documented command work.
	//
	// Neither clause carries file-map-executable. It would be pure noise
	// here: section 9 allows process-exec* with no path filter, so execution
	// is already permitted profile-wide and adding the flag to a section-5k
	// clause changes nothing. The execution posture for the module cache is
	// set by the explicit deny in the final section instead — see
	// goCacheDir.execDenied.
	//
	// Accepted risk, stated plainly: these are shared, cross-session,
	// host-visible caches, so a compromised agent could mutate an extracted
	// module or a build-cache entry that a later HOST build consumes (go
	// verifies module zips against go.sum on download, but does not re-verify
	// an already-extracted tree — that is what `go mod verify` is for). This
	// is the same trust class as the §5e ~/.npm grant (npx executes cached
	// JS) and ~/.cache/nix. The alternative — redirecting GOMODCACHE/GOCACHE
	// into the session work dir — trades that for a cold cache per session.
	//
	// Concurrency is safe by design: the Go caches are built for concurrent
	// multi-process access (a lock file under GOMODCACHE, content-addressed
	// entries under GOCACHE), which is exactly what several parallel workers
	// plus the host shell do.
	//
	// Emitted even when a dir does not exist on the host — sandbox-exec
	// silently ignores (subpath ...) rules for non-existent paths (same shape
	// as the 5d/5e/5f/5j grants). ensureGoCacheDirs creates them host-side at
	// Prepare time, because a grant on a non-existent path is a no-op the
	// sandboxed process cannot repair itself.
	//
	// One clause per directory. The two carry identical operations today, so
	// a shared clause would be equivalent; separate clauses keep each
	// directory's exact permission set auditable on its own line and make the
	// paired deny below unambiguous about which path it narrows.
	for _, dir := range goCacheDirs(home) {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + quoteSBPL(dir.path) + "))\n")
		sb.WriteString("\n")
	}

	// ── 6. Session work dir + worktree + host-API socket (RW) ─────────────
	// Session-specific read-write paths.
	// file-test-existence and file-read-metadata added alongside file-read*
	// and file-write* for dyld/AMFI compatibility in the v3 profile.
	//
	// The first entry is the per-session work dir — one of the two
	// per-session writable grants (the other is the section-3 TMPDIR). It
	// covers the generated ssh-config /
	// gitconfig / allowed_signers, kubectl's KUBECACHEDIR cache, glab's
	// GLAB_CONFIG_DIR config dir, AND the chromium Library
	// skeleton (CFFIXED_USER_HOME points chromium's
	// NSHomeDirectory() at <sessionDir>, so its writes land under
	// <sessionDir>/Library/... with NO dedicated rule and NO host-Library
	// grant).
	if sessionDirErr == nil {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + quoteSBPL(sessionDir) + ")\n")
		// Worktree — the git checkout the agent works in. The project root
		// (BareRoot) and the other worktrees in it have no write grant here:
		// see section 6e.
		if m.cfg.Worktree != "" {
			sb.WriteString("  (subpath " + quoteSBPL(m.cfg.Worktree) + ")\n")
		}
		// Host-API socket directory — the sidecar's per-session socket dir.
		// The run-dir (subpath ...) rule covers agent-run.log,
		// hostapi.sock, and pipe.sock. Section 6a covers ~/.pi/agent.
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

	// ── 6a. PI agent dir: read, and write of an exact list (pi sessions only) ─
	// A worker must not create, change, or replace a path in ~/.pi/agent:
	// every later session loads code and prompts from it, host sessions too
	// (#3284). pi_agent_dir.go lists what pi and its extensions write during
	// a normal session, and why. This section allows a read of the whole dir
	// and a write of exactly these paths:
	//   - atlassian-mcp-oauth.json (an in-place write);
	//   - sessions/<encoded-cwd>/ of the worktree, not of another worktree.
	// pi writes auth.json and makes its lock dirs in the per-session agent
	// dir: see section 6d. The host auth.json has no write grant (#3295).
	// The Notion store is not in this dir: see section 6c. Section 21f takes
	// back the create and the unlink of auth.json and of the in-place file.
	//
	// The rule is gated on Harness == "pi" so non-pi sessions are not
	// exposed to the pi credential directory. It is emitted for pi sessions
	// even when ~/.pi/agent does not yet exist — sandbox-exec silently
	// ignores rules for non-existent paths. Prepare (lifecycle_dispatch.go)
	// makes the sessions dir and the credential files on the host first.
	if m.cfg.Harness == "pi" && home != "" {
		sb.WriteString(piAgentDirSBPLAllow(filepath.Join(home, ".pi", "agent"), m.cfg.Worktree))
		sb.WriteString("\n")
	}

	// ── 6c. pi shared state dirs: lock dir and Notion store (pi sessions only) ─
	// Every pi session on the host shares these two dirs: the lock dir of the
	// OAuth token refresh, and the Notion MCP store (pi_shared_state.go,
	// #3289, #3290). A read and a write of each whole dir: the extensions
	// make and remove lock dirs in them, and the Notion extension renames
	// temp files onto its store files. No pi session loads code or prompts
	// from them. prism agent-run makes both dirs on the host and sets the
	// two Config fields. An empty field gives no rule.
	if m.cfg.Harness == "pi" {
		if clause := piSharedStateSBPLAllow(m.cfg.PILockHostDir, m.cfg.PINotionHostDir); clause != "" {
			sb.WriteString(clause)
			sb.WriteString("\n")
		}
	}

	// ── 6d. pi per-session agent dir (pi sessions only) ───────────────────────────
	// PI_CODING_AGENT_DIR of a sandbox-exec session is its per-session dir
	// (pi_auth_session.go, #3295). It holds the auth.json of the session,
	// the lock dirs of pi, and a symlink to each other entry of ~/.pi/agent.
	// A read and a write of the whole dir: only this session reads it, and
	// the sidecar copies only OAuth entries from it to the host auth.json.
	// The symlink targets are in ~/.pi/agent, so section 6a decides which of
	// them pi can write. prism agent-run makes the dir and sets the Config
	// field. An empty field gives no rule.
	if m.cfg.Harness == "pi" {
		if clause := piAuthSessionSBPLAllow(m.cfg.PIAuthSessionDir); clause != "" {
			sb.WriteString(clause)
			sb.WriteString("\n")
		}
	}

	// ── 6e. Project root read, git write scope (#3293) ─────────────────────
	// A read of the whole project root (BareRoot): pi and other tools read
	// project files, for example .agents/skills, in each ancestor of the
	// worktree, and git reads the git dirs of the other worktrees for git
	// worktree list. A write of only the git paths that a commit, a fetch, a
	// push, and a rebase write (git_write_scope.go). The other worktrees,
	// .bare/config, and .bare/hooks have no write grant. A session that can
	// write in another worktree, or in a file that git or pi loads there,
	// changes what the coordinator of the repo runs.
	//
	// A session can still change an object in .bare/objects, which git needs
	// for a commit and a fetch. A later checkout in another worktree then
	// writes the changed object. See the known limits in
	// prism/docs/invariants/sandbox-git-write-scope.md.
	if m.cfg.BareRoot != "" {
		sb.WriteString(gitScope.sbplAllow(m.cfg.BareRoot, m.cfg.WorktreeReadOnly, m.cfg.AgentRole == "coordinator"))
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

	// The sops-backed key/config reads ride the broad /private/var/folders
	// allow narrowed by the secrets.d allowlist (section 3c); every other
	// capability has an explicit real-path grant above.

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
	//   mode (com.apple.windowserver.active) called out as a separate clause.
	//   Tightening mach-lookup to an enumerated global-name set
	//   is a defensible follow-up but requires empirically enumerating every name
	//   dyld + AMFI + securityd + CFNetwork + libsystem_kernel + node + chromium
	//   probe at startup — a much larger surface than the v1/v2 rule shape was
	//   ever scoped to. The headed-mode WindowServer requirement is
	//   already satisfied by the unqualified form.
	// mach-register  — per-pid Mach name registration (pi IPC).
	// sysctl-read    — system library init queries (kern.*, hw.*, machdep.*).
	// NOTE: signal is emitted as its own clause below so the (target ...)
	//   qualifiers can be expressed cleanly — see §9a.
	// NOTE: iokit-open is emitted as its own enumerated-class clause below —
	//   see §9b.
	// NOTE: ipc-posix-shm is REMOVED (unbound variable in v3 — replaced below).
	sb.WriteString("(allow process-exec* process-fork process-info* mach-lookup mach-register\n")
	sb.WriteString("       sysctl-read)\n")
	sb.WriteString("\n")

	// ── 9a. signal — self + children ───────────────────────
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

	// ── 9b. iokit-open-user-client — Chromium user-client classes ─
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

	// ── 9c. iokit-open-service — IOPMrootDomain ──────────────────
	// Current Chrome for Testing acquires its power-management port via
	// iokit-open-service on the IOPMrootDomain registry entry — a different
	// operation class from the iokit-open-user-client RootDomainUserClient
	// allow above (§9b). Without this rule the open is denied and chromium
	// SIGSEGVs (SEGV_ACCERR) during early init — the same observable
	// fingerprint as the user-client denial. Deny-log smoking gun:
	//
	//   Sandbox: Google Chrome for Testing(NNN) deny(1) iokit-open-service IOPMrootDomain
	//
	// The rule shape mirrors Apple's own power-assertions definition in
	// /System/Library/Sandbox/Profiles/appsandbox-common.sb, which pairs
	// exactly these two allows:
	//
	//   (allow iokit-open-service (iokit-registry-entry-class "IOPMrootDomain"))
	//   (allow iokit-open-user-client
	//          (iokit-user-client-class "RootDomainUserClient"))
	//
	// The filter is iokit-registry-entry-class (the class of the IOService
	// being opened), NOT iokit-user-client-class (which filters the
	// user-client connection type and applies to iokit-open-user-client).
	//
	// The unqualified form `(allow iokit-open-service)` MUST NOT be used —
	// it would permit opening arbitrary IOKit services. Only the
	// IOPMrootDomain registry entry is granted.
	sb.WriteString("(allow iokit-open-service\n")
	sb.WriteString("  (iokit-registry-entry-class \"IOPMrootDomain\"))\n")
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
	// (allow network*) grants outbound TCP, UDP, and — the part section 17b
	// narrows — a connect(2) to a Unix domain socket. It matches the bwrap
	// baseline. Restriction of TCP to specific hosts and ports is a future
	// concern.
	//
	// Note: GIT_SSH_COMMAND is set to the Nix-built openssh binary (cfg.SshBin,
	// baked into config.json by prism-tui.nix). Nix openssh links against its
	// own libresolv/libldns (Nix store paths under /nix, already allowed) rather
	// than Apple's libnetwork.dylib. This means the system-network macro rules
	// (dafsaData.bin, com.apple.netsrc, AF_SYSTEM/AF_ROUTE sockets) are NOT
	// needed for SSH git operations — they would only be needed if /usr/bin/ssh
	// were used. See the GIT_SSH_COMMAND block in
	// cmd/agent_run_sandbox_exec_darwin.go for the full rationale.
	sb.WriteString("(allow network*)\n")
	sb.WriteString("\n")

	// ── 17b. Unix-socket connect allowlist ─────────────────────────────────
	// On macOS, a connect(2) to a Unix domain socket is a `network-outbound`
	// operation that carries the socket path. Section 17's unfiltered allow
	// admits every such connect. Without this section, a sandboxed agent
	// reaches the podman machine API socket under $TMPDIR/podman/ and drives
	// the podman machine with no filter (#3066).
	//
	// The macOS mechanism that blocks the connection: a `(subpath "/")` filter
	// on `network-outbound` matches every path-bearing (Unix-domain) connect.
	// A TCP or UDP connect carries an IP endpoint and no path, so the filter
	// never matches it. A deny built on `(subpath "/")` thus blocks
	// Unix-socket connects and leaves TCP (HTTPS, the 127.0.0.1 harness pipe,
	// `prism container start --port`) and DNS open.
	//
	// The deny is an allowlist: a require-all of `(subpath "/")` plus one
	// require-not per allowed path. An allowed path does not match the deny,
	// so section 17's allow admits it. The allowed set:
	//   - the per-session host-API socket dir (hostapi.sock): prism prompt,
	//     prism escalate, prism review;
	//   - mDNSResponder: DNS name resolution;
	//   - syslog: libsystem logging;
	//   - the nix daemon socket: `nix build`;
	//   - the per-session TMPDIR (section 3), the session work dir, and the
	//     worktree: the scratch trees where `go test ./...` binds and dials
	//     its own Unix sockets. Section 5k makes that quality
	//     gate runnable in-sandbox. The in-sandbox TMPDIR is the per-session
	//     TMPDIR, so most test sockets land there. Remove these entries and
	//     about 60 socket tests in internal/sidecar, internal/session, and cmd
	//     fail.
	//
	// The project root (BareRoot) is not in the set. The session cannot
	// write there (section 6e), so a socket there belongs to another session.
	//
	// No shared temp tree is in the set: not /tmp, not /private/tmp, and not
	// the user $TMPDIR (/var/folders). The host sockets there stay denied:
	// the podman machine, tmux, the user ssh-agent, AeroSpace, a .NET
	// diagnostic socket, nvim, and any other that the profile does not name
	// (#3066, #3147, #3150). The session cannot move such a socket into an
	// allowed tree either: it has no write grant on /tmp, /private/tmp, or
	// the user $TMPDIR, so it cannot unlink the socket from its dir.
	sb.WriteString(networkUnixSocketDeny(m, sessionDir, sessionDirErr, sessionTmpForms))

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

	// ── 21b. FINAL DENY — the worktree path cannot be replaced ─
	// Section 6 grants (subpath worktree), and a subpath covers the worktree
	// node too. Without this deny, the agent can rename the worktree away and
	// put a symlink at its path. `prism container run --mount` gives the
	// worktree path to podman on the host, and podman then mounts the symlink
	// target: any directory that the podman machine shares, the whole home
	// directory included.
	//
	// The deny lists the worktree and every ancestor up to /, BareRoot
	// included. An ancestor that no grant covers is denied already. It is
	// listed so that a later grant cannot re-open it. The rules match the
	// directory nodes only, so files below the worktree keep the section-6
	// grant. The deny must follow every allow, because SBPL resolves a
	// conflict in favour of the later rule.
	// TestGenerateProfile_WorktreeEntryDenyFollowsEveryAllow
	// pins the order.
	if m.cfg.Worktree != "" {
		var nodes []string
		for dir := filepath.Clean(m.cfg.Worktree); ; dir = filepath.Dir(dir) {
			nodes = append(nodes, "  (literal "+quoteSBPL(dir)+")")
			if dir == filepath.Dir(dir) {
				break
			}
		}
		sb.WriteString("\n")
		sb.WriteString("(deny file-write-unlink file-write-create\n")
		sb.WriteString(strings.Join(nodes, "\n") + ")\n")
	}

	// ── 21c. FINAL DENY — podman and docker binaries do not run ─
	// `prism container` is the only supported way to run a container from
	// the sandbox (#3065). The pi deny list blocks a `podman` or `docker`
	// command that it can see. This deny also stops a client that the deny
	// list cannot see: a script, `sh -c`, or an absolute path. SBPL matches
	// the path after it resolves symlinks, so the deny also stops a symlink
	// with another name. The nix wrapper of podman runs .podman-wrapped, so
	// the deny lists that name too.
	//
	// The rule matches file names. A copy of the binary under another name
	// still runs, but section 17b denies its connect to the podman machine
	// socket. The deny must follow the section-9 allow, because SBPL resolves
	// a conflict in favour of the later rule.
	// TestGenerateProfile_ContainerCLIExecDenyFollowsProcessExecAllow pins
	// the order.
	sb.WriteString("\n")
	sb.WriteString(containerCLIExecDeny)

	// ── 21d. FINAL DENY — a read-only worktree ─
	// Section 6 grants read-write on the worktree. For a review agent or an
	// investigator, this deny takes the write back. The deny must follow
	// every allow, because SBPL resolves a conflict in favour of the later
	// rule. TestGenerateProfile_WorktreeReadOnlyDenyFollowsEveryAllow pins
	// the order. The worktree git dir is in .bare, not in the worktree.
	// Section 6e grants such a session only FETCH_HEAD in it.
	if m.cfg.WorktreeReadOnly && m.cfg.Worktree != "" {
		sb.WriteString("\n")
		sb.WriteString(worktreeReadOnlyDeny(m.cfg.Worktree))
	}

	// ── 21e. FINAL DENY — the per-session TMPDIR node cannot be replaced, opened, or locked ─
	// Section 3 grants (subpath <TMPDIR>), which covers the TMPDIR node
	// itself. Without this deny, the session can rename its TMPDIR away and
	// put a symlink at the path, because a create at the root of a subpath
	// grant matches the grant. The host side acts on that path: Prepare on a
	// restore and the cleanup removal. With the deny, the path stays the dir
	// that Prepare made, so host-side code never meets a planted symlink.
	//
	// The deny also covers mode writes (chmod), flag writes (chflags), and
	// ACL writes (chmod +a) on the node. Without it, the session can open its
	// TMPDIR to other users with chmod 0777 or with an ACL entry, or lock the
	// node with an immutable flag (chflags uchg) or a deny ACL entry, so that
	// the cleanup removal fails. chmod +a is a separate operation
	// (file-write-acl): it does not change the mode, and file-write-mode does
	// not cover it.
	//
	// The rules are literals, so they match the node only, and files below
	// it keep the section-3 grant: a session can still chmod +x a script in
	// its TMPDIR. /private/var/tmp, the parent, has no write grant, so the
	// chain stops there. The deny must follow every allow, because SBPL
	// resolves a conflict in favour of the later rule.
	// TestGenerateProfile_SessionTmpDirNodeDenyFollowsEveryAllow pins the
	// order.
	if len(sessionTmpForms) > 0 {
		sb.WriteString("\n")
		sb.WriteString(sessionTmpNodeDeny(sessionTmpForms))
	}

	// ── 21f. FINAL DENY — the pi credential files cannot be replaced ─
	// Section 6a grants a write of atlassian-mcp-oauth.json. The extension
	// writes it in place, and Prepare makes it on the host. Without this
	// deny, the session can remove the file and put a symlink at its path. A
	// host pi then follows the symlink and writes its tokens to a file that
	// the session chose (#3284). The deny also names auth.json, which has no
	// write grant (#3295). The deny must follow every allow, because SBPL
	// resolves a conflict in favour of the later rule.
	// TestGenerateProfile_PIAgentCredentialDenyFollowsEveryAllow pins the
	// order.
	if m.cfg.Harness == "pi" && home != "" {
		sb.WriteString("\n")
		sb.WriteString(piAgentDirSBPLDeny(filepath.Join(home, ".pi", "agent")))
	}

	// ── 21g. FINAL DENY — the shared state dir nodes cannot be replaced ─
	// Section 6c grants a subpath of the lock dir and of the Notion store
	// dir, and a subpath covers the dir node too. A session must not rename
	// a dir away and put a symlink at its path: a host pi and prism
	// agent-run then write lock owner files and Notion tokens where the
	// session chose. The deny must follow every allow.
	// TestGenerateProfile_PISharedStateDenyFollowsEveryAllow pins the order.
	if m.cfg.Harness == "pi" {
		if clause := piSharedStateSBPLDeny(m.cfg.PILockHostDir, m.cfg.PINotionHostDir); clause != "" {
			sb.WriteString("\n")
			sb.WriteString(clause)
		}
	}

	// ── 21h. FINAL DENY — the per-session agent dir node cannot be replaced ─
	// Section 6d grants a subpath of the per-session agent dir, and a subpath
	// covers the dir node too. A session must not remove the dir and put a
	// symlink at its path: the sidecar then writes the session auth.json, with
	// the host tokens, where the session chose. The parent dir has no write
	// grant. The deny must follow every allow.
	// TestGenerateProfile_PIAuthSessionDenyFollowsEveryAllow pins the order.
	if m.cfg.Harness == "pi" {
		if clause := piAuthSessionSBPLDeny(m.cfg.PIAuthSessionDir); clause != "" {
			sb.WriteString("\n")
			sb.WriteString(clause)
		}
	}

	// ── 21i. FINAL DENY — the git dir nodes cannot be replaced ─
	// Section 6e grants a subpath of .bare/objects, of three ref and reflog
	// dirs, and of the worktree git dir. A subpath covers the dir node too.
	// A session must not move such a dir away and put a symlink at its path:
	// each other worktree then reads objects and refs from a dir that the
	// session chose. The deny must follow every allow.
	// TestGenerateProfile_GitNodeDenyFollowsEveryAllow pins the order.
	if m.cfg.BareRoot != "" {
		if clause := gitScope.sbplNodeDeny(); clause != "" {
			sb.WriteString("\n")
			sb.WriteString(clause)
		}
	}

	// ── 21j. FINAL DENY — the git dir redirect files cannot be written ─
	// Section 6e grants a subpath of the worktree git dir. commondir,
	// gitdir, and config.worktree in it tell git where the common dir is and
	// which config to read, for each git command in the worktree, prism on
	// the host included. A commondir that names a dir of the session gives
	// git the config and the hooks of that dir. git does not write these
	// files in normal use (git_write_scope.go). The deny must follow every
	// allow. TestGenerateProfile_GitRedirectFileDenyFollowsEveryAllow pins
	// the order.
	if m.cfg.BareRoot != "" {
		if clause := gitScope.sbplRedirectFileDeny(); clause != "" {
			sb.WriteString("\n")
			sb.WriteString(clause)
		}
	}

	// ── 21k. FINAL DENY — the .git file of the worktree cannot be written ─
	// Section 6 grants the worktree, and in a bare+worktree layout the .git
	// file in it names the git dir. prism starts nvim and a shell on the
	// host in each worktree, and they run git there, which follows the file.
	// A .git file that names a git dir of the session runs its config, for
	// example core.fsmonitor, on the host. The deny covers a write, a
	// rename, a removal, and a replace of the file. git does not write it in
	// normal use (git_write_scope.go). The deny must follow every allow.
	// TestGenerateProfile_WorktreeGitFileDenyFollowsEveryAllow pins the
	// order.
	if m.cfg.BareRoot != "" {
		if clause := worktreeGitFileDeny(m.cfg.Worktree); clause != "" {
			sb.WriteString("\n")
			sb.WriteString(clause)
		}
	}

	// ── 22. FINAL DENIES — Go module cache is not executable ─
	// The module cache holds downloaded dependency SOURCE. Section 5k grants
	// it read-write because the toolchain must populate it. Without this deny,
	// a sandboxed process could plant a binary among the dependency sources
	// and run it.
	//
	// PAIRED WITH GoToolchainEnv — do not change one without the other. The
	// claim "nothing in the documented gate executes out of the module cache"
	// holds ONLY because prism injects GOTOOLCHAIN=local. Under Go's default
	// GOTOOLCHAIN=auto, cmd/go downloads a newer toolchain into the module
	// cache and execs <dir>/bin/go from it, which this deny blocks — breaking
	// the gate with the same error shape. See the GoToolchainEnv godoc for
	// the upstream call path and for why a golang.org/toolchain@* carve-out
	// was rejected.
	//
	// THIS SECTION MUST STAY LAST, and it must in particular stay AFTER the
	// section-9 process operations. That is not a stylistic preference — it
	// is the only reason the rule has any effect:
	//
	//   - Section 9 emits (allow process-exec* ...) with NO path filter, so
	//     execution is permitted profile-wide.
	//   - SBPL resolves a conflict in favour of the LATER rule. A deny placed
	//     before section 9 is silently overridden by that allow and the
	//     module cache becomes executable again — with every substring test
	//     still green, because the deny text is present, just outranked.
	//
	// This is the same deny-after-broader-allow shape as the /private/etc/ssh
	// denies in section 4 and the ~/.aws deny in section 5, both of which are
	// integration-tested (sandbox_exec_denies_darwin_test.go).
	// TestGenerateProfile_GoCacheExecDenyFollowsProcessExecAllow pins the
	// ordering so a future section inserted below cannot quietly break it.
	//
	// Expressing this as the ABSENCE of file-map-executable on the module
	// cache's allow clause does NOT work: a planted binary executes from the
	// module cache under the production profile, and from the build cache
	// even with the whole section-5k block stripped. Withholding a flag from
	// one allow clause cannot narrow a capability that a later unqualified
	// allow hands out. Only an explicit deny does.
	//
	// GOCACHE is deliberately NOT denied: cmd/go can serve a linked test
	// binary straight out of the build cache on a warm build, so execution
	// there must keep working.
	var execDenied []goCacheDir
	for _, dir := range goCacheDirs(home) {
		if dir.execDenied {
			execDenied = append(execDenied, dir)
		}
	}
	if len(execDenied) > 0 {
		sb.WriteString("\n")
		sb.WriteString("(deny process-exec* file-map-executable\n")
		for i, dir := range execDenied {
			sb.WriteString("  (subpath " + quoteSBPL(dir.path) + ")")
			if i == len(execDenied)-1 {
				sb.WriteString(")")
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// worktreeReadOnlyDeny is the section-21d clause of generateProfile. SBPL
// matches the path after it resolves symlinks, so the clause also lists the
// resolved path when it is different.
func worktreeReadOnlyDeny(worktree string) string {
	clause := "(deny file-write*\n  (subpath " + quoteSBPL(worktree) + ")"
	if real, err := filepath.EvalSymlinks(worktree); err == nil && real != filepath.Clean(worktree) {
		clause += "\n  (subpath " + quoteSBPL(real) + ")"
	}
	return clause + ")\n"
}

// ancestorMetadataAllow is the section-3 allow of file-read-metadata and
// file-test-existence on each ancestor node of the given paths, as
// literals, from the parent up to and without "/" (section 2 lists "/").
func ancestorMetadataAllow(paths []string) string {
	var nodes []string
	seen := map[string]bool{}
	for _, p := range paths {
		for dir := filepath.Dir(filepath.Clean(p)); dir != "/" && dir != "." && !seen[dir]; dir = filepath.Dir(dir) {
			seen[dir] = true
			nodes = append(nodes, "  (literal "+quoteSBPL(dir)+")")
		}
	}
	if len(nodes) == 0 {
		return ""
	}
	return "(allow file-read-metadata file-test-existence\n" + strings.Join(nodes, "\n") + ")\n"
}

// sessionTmpNodeDeny is the section-21e clause of generateProfile.
func sessionTmpNodeDeny(forms []string) string {
	clause := "(deny file-write-unlink file-write-create file-write-mode file-write-flags file-write-acl"
	for _, p := range forms {
		clause += "\n  (literal " + quoteSBPL(p) + ")"
	}
	return clause + ")\n"
}

// containerCLIExecDeny is the section-21c clause of generateProfile.
const containerCLIExecDeny = "(deny process-exec*\n" +
	"  (regex #\"/podman$\")\n" +
	"  (regex #\"/docker$\")\n" +
	"  (regex #\"/\\.podman-wrapped$\")\n" +
	"  (regex #\"/\\.docker-wrapped$\"))\n"

// networkUnixSocketDeny returns the section-17b allowlist deny. It is the
// single source of the Unix-socket connect policy.
//
// The allowlist deny is `(deny network-outbound (require-all (subpath "/")
// (require-not <allowed>) ...))`. The leading `(subpath "/")` scopes the
// rule to Unix-domain connects: a TCP or UDP connect carries no path, so
// `(subpath "/")` never matches it. Remove that term and the rule denies
// every outbound connection, HTTPS included.
func networkUnixSocketDeny(m *Manager, sessionDir string, sessionDirErr error, sessionTmpForms []string) string {
	var allowed []string
	// Per-session host-API socket dir — hostapi.sock is the one Unix socket a
	// session connects to for prism prompt/escalate/review. Grant the dir, not
	// the single file, so a sibling socket prism may add there also works.
	if m.cfg.HostAPISockPath != "" {
		allowed = append(allowed, "(subpath "+quoteSBPL(filepath.Dir(m.cfg.HostAPISockPath))+")")
	}
	// System sockets. Both the /var/run and /private/var/run forms are listed
	// because /var is a symlink to /private/var and the connect path can arrive
	// in either form (same dual-form reason as the file rules above).
	allowed = append(allowed,
		"(literal \"/private/var/run/mDNSResponder\")",
		"(literal \"/var/run/mDNSResponder\")",
		"(literal \"/private/var/run/syslog\")",
		"(literal \"/var/run/syslog\")",
		"(literal \"/nix/var/nix/daemon-socket/socket\")",
	)
	// Scratch trees for the in-sandbox `go test ./...` quality gate. See the
	// section-17b comment for why these are load-bearing.
	for _, p := range sessionTmpForms {
		allowed = append(allowed, "(subpath "+quoteSBPL(p)+")")
	}
	if sessionDirErr == nil && sessionDir != "" {
		allowed = append(allowed, "(subpath "+quoteSBPL(sessionDir)+")")
	}
	if m.cfg.Worktree != "" {
		allowed = append(allowed, "(subpath "+quoteSBPL(m.cfg.Worktree)+")")
	}

	var sb strings.Builder
	sb.WriteString("(deny network-outbound\n")
	sb.WriteString("  (require-all\n")
	sb.WriteString("    (subpath \"/\")\n")
	for _, a := range allowed {
		sb.WriteString("    (require-not " + a + ")\n")
	}
	sb.WriteString("))\n")
	sb.WriteString("\n")
	return sb.String()
}

// collectSecretsDAllowlistNames returns the secrets.d-relative names of the
// agent-needed secrets, derived from the stable host source paths the
// sandbox legitimately reads through:
//
//	~/.ssh/<SshAccessKeyName>            — ssh auth (generated ssh-config IdentityFile)
//	~/.ssh/<SshAccessKeyName>.pub        — openssh public-half probe
//	~/.ssh/<SshSigningKeyName>           — commit signing (ssh-keygen -Y sign)
//	~/.ssh/<SshSigningKeyName>.pub       — gitconfig user.signingKey
//	~/.config/aws/readonly-config        — read via AWS_CONFIG_FILE env
//	~/.config/aws/credentials            — read via AWS_SHARED_CREDENTIALS_FILE env
//	~/.config/kube/agents-config         — read via KUBECONFIG env
//	cfg.GitLabTokenPath                  — the gitlab_token sops secret
//	cfg.GrafanaConfigPath                — the pi grafana MCP config bundle
//
// The last two entries meet the rule below: an in-sandbox consumer reads them.
//
// GitLab: any zsh the agent starts sources the home-manager session vars,
// which re-derive GITLAB_TOKEN by running `cat <this path>`. With the read
// denied that cat fails and OVERWRITES the injected GITLAB_TOKEN with an
// empty string, so glab breaks in every zsh-launched command — the same
// mechanism the bwrap SHELL=/bin/sh pin works around on Linux. Note what the exception does NOT do: the token
// VALUE is already inside the sandbox as an env var (credentialEnvVars
// injects it), so this grants a second route to a secret the sandbox
// already holds, not a new secret. The exception is one file, by name.
//
// Grafana: the pi grafana MCP extension calls
// readFileSync(process.env.GRAFANA_MCP_CONFIG_PATH) from inside the sandbox
// to get the Grafana URL and API key for the mcp-grafana child process, so
// unlike the GitLab case there is no host-side read and no injected VALUE —
// the in-sandbox read IS the delivery mechanism, and denying it kept
// grafana off Darwin sandbox-exec hosts. cfg.GrafanaConfigPath
// carries the same path prism injects as GRAFANA_MCP_CONFIG_PATH, sourced
// from the role-filtered agent env, so a review role — which has that var
// stripped — gets no exception either. Again one file, by name.
//
// Each source is resolved via filepath.EvalSymlinks; when the resolved
// target is a sops secrets.d path (…/secrets.d/<N>/<name>), <name> is
// returned. Sources that are absent, unresolvable, or not sops-backed are
// skipped — they need no exception because the secrets.d deny never covers
// them. The returned names are deduplicated and keep source order so the
// emitted profile is deterministic.
//
// This list is the enforcement half of the inventory: every
// other name under secrets.d/<N>/ (github_token, the role PATs, aws-config,
// workkube, …) stays denied. Do NOT add a source here merely because a
// secret exists — only because an in-sandbox consumer reads it. gitlab_token
// and the grafana config bundle are in the sources above because an
// in-sandbox consumer reads each; both stay denied on any host (or role)
// that does not configure them.
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
	// The GitLab token source is config-driven, not a fixed path: it is the
	// same gitlab_token_path prism reads host-side to inject GITLAB_TOKEN.
	// A host without nx.programs.gitlab-cli.enable leaves it empty, so no
	// exception is emitted and gitlab_token stays denied.
	if m.cfg.GitLabTokenPath != "" {
		sources = append(sources, m.cfg.GitLabTokenPath)
	}
	// The grafana source is config-driven in the same way: it is the value
	// prism injects as GRAFANA_MCP_CONFIG_PATH, which the sandbox-exec spawn
	// path copies onto the Manager config. A host without
	// nx.programs.prism.pi.grafana.enable leaves it empty, and so does every
	// review role (agent_env_roles.go strips the var), so no exception is
	// emitted and the bundle stays denied.
	if m.cfg.GrafanaConfigPath != "" {
		sources = append(sources, m.cfg.GrafanaConfigPath)
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
//	sandbox-exec -f <profile_path> pi [--agent X] [Y]
//
// The first element ("sandbox-exec") is argv[0]; the caller invokes
// syscall.Exec("/usr/bin/sandbox-exec", args, env). After -f and the profile
// path comes the harness binary (pi) and its arguments — the inner
// command sandbox-exec executes inside the SBPL sandbox.
//
// The harness invocation mirrors bwrap.go:BuildArgs so
// that the sandbox interior behaves identically across isolation modes:
//
//   - --port: cfg.AllocatedPort, falling back to ContainerPort when 0.
//   - --hostname 127.0.0.1: same rationale as bwrap (host network namespace
//     is shared on both modes; binding 0.0.0.0 would be overly broad).
//   - --agent <role>: appended when cfg.AgentRole is non-empty.
//   - <text>: bare positional argument, appended when cfg.InitialPrompt is non-empty.
//
// HOME and the rest of the sandbox env are not wired through the profile
// generator — the dispatcher (cmd/agent_run_sandbox_exec_darwin.go) builds
// the env, starting from the MinimalIsolatedExecEnv allow-list. $HOME is
// the real host home.
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
// gracefulShutdown helper (shutdown.go).
//
// In the production agent-run flow the sandbox-exec child is owned by
// cmd/agent_run_sandbox_exec_darwin.go (which manages its own supervised
// Wait loop with kqueue parent-death watching). The Isolator's Shutdown is
// therefore a no-op when no Run-managed child has been registered (s.cmd
// is nil). When a future test or
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

// SandboxExecShellEnv returns the K=V env-slice entry that pins SHELL to
// /bin/sh for the sandbox-exec interior env, mirroring bwrap's
// standardSandboxEnvArgs pin (bwrap.go) for the same reason.
//
// # Root cause
//
// Inside a Darwin sandbox-exec session, any zsh invocation clobbers an
// injected sops-backed credential env var (e.g. GITHUB_TOKEN, GITLAB_TOKEN)
// to an empty string. zsh sources the home-manager session variables, which
// re-derive each sops-backed var with a literal `cat <sops-path>` command
// substitution. Inside the sandbox, the secrets.d deny (section
// 3c of generateProfile) makes that cat fail, so the re-derivation yields an
// empty string and OVERWRITES the value agent-run injected via credEnv
// (m.CredentialEnvVars, cmd/agent_run_sandbox_exec_darwin.go). /bin/sh
// sources neither /etc/zshenv nor any home-manager session-variables file,
// so an invocation that consults $SHELL (the agent bash tool, most TUIs)
// gets a clean shell that does not wipe credentials.
//
// Unlike bwrap, sandbox-exec has no --clearenv/--setenv rebuild of the
// sandbox interior: the sandbox shares the harness process's own env
// (MinimalIsolatedExecEnv godoc above). MinimalIsolatedExecEnv's allow-list
// does not include SHELL, so without this pin no consumer of $SHELL inside
// the sandbox gets ANY defined value — this is not a case of a zsh value
// leaking through unfiltered, it's a case of nothing setting SHELL at all
// until this pin does.
//
// # Consumer safety check
//
// No sandbox-exec consumer in this tree relies on $SHELL resolving to zsh
// or bash specifically: SessionWorkDirGitEnv/KubeEnv/GlabEnv, CredentialEnvVars,
// and AppendSandboxEnvVarsKV(AgentEnvVars/RuntimeEnv) do not read $SHELL, and
// BuildArgs/PIInvocation pass argv directly to sandbox-exec/pi rather than
// through a shell. The pin is therefore safe to apply unconditionally.
//
// Callers should append this last (or after any other SHELL-setting source)
// so it wins under Go's "last occurrence wins" duplicate-key convention for
// exec env slices — see the GOTOOLCHAIN append in
// cmd/agent_run_sandbox_exec_darwin.go for the sibling case.
func SandboxExecShellEnv() []string {
	return []string{"SHELL=/bin/sh"}
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
// of defence against host-shell secrets reaching the sandbox interior.
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
