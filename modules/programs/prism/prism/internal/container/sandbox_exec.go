// Package container manages the podman container lifecycle for prism sidecar.
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

	// mu guards exited/exitCode. The sandbox-exec process for a session is
	// launched by agent-run via syscall.Exec, so this Isolator's Run path is
	// unused in the production flow — these fields exist solely to satisfy
	// the Isolator interface and to give future tests a place to record
	// terminal state.
	mu       sync.Mutex
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
// This is a (version 3) SBPL profile — see the migration design doc at
// docs/reviews/F1-sandbox-exec-version-3-migration.md and issue #1200.
//
// The profile shape includes:
//   - Cryptex graft points (dyld shared cache, macOS 15+)
//   - Read-only system roots with file-test-existence, file-map-executable,
//     file-read-metadata alongside file-read* (required by dyld/AMFI)
//   - /bin, /sbin, and /var/... symlink-alias forms (v3 additions)
//   - /tmp read-write for xcrun and transient files
//   - Deny of sensitive /private/etc subtrees (wireguard, wpa_supplicant),
//     both /etc/... and /private/etc/... forms (symlink non-transparency)
//   - Host ~/.aws deny (only staged entries accessible)
//   - Staging HOME / worktree / bare repo / host-API socket dir (RW)
//   - Symlink target allows (read-only) for every symlink in the staging HOME
//   - Process and IPC primitives required by dyld, AMFI, and opencode
//   - (allow network*)
//
// New top-level allow/deny clauses introduced beyond what is sketched in
// #1012 require a comment in the generator pointing at the issue.
func generateProfile(m *Manager) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	// Derive the staging HOME path. When it cannot be determined (e.g. no
	// home dir) the staging-HOME-derived rules are simply omitted.
	stagingHome, stagingErr := m.sandboxExecHomePath()

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
	// /usr, /bin, /sbin — standard Apple-signed utility directories.
	// /System, /Library — OS frameworks, dylibs, and shared data.
	// /Applications/Xcode.app — xcrun (called by /usr/bin/git shim).
	// /etc, /private/etc — both forms: sandbox-exec does NOT transparently
	//   follow the /etc → /private/etc symlink (PR #1193 / issue #1187).
	// /private/var/db/dyld, /var/db/dyld — dyld shared-cache DB;
	//   /var is a symlink to /private/var; both forms needed. See F.1 §2.
	// /private/var/db/timezone, /var/db/timezone — timezone data.
	// /private/var/select, /var/select — xcode-select developer_dir.
	// /private/var/folders, /var/folders — Darwin per-user TMPDIR (xcrun).
	// /dev/null, /dev/random, /dev/urandom — device nodes.
	// /dev/dtracehelper — Apple-signed binaries probe at startup.
	// /             — required by libignition's openat(2) root probe.
	// file-test-existence, file-map-executable, file-read-metadata added
	//   alongside file-read* for dyld to probe and map code-signed binaries.
	//   See F.1 §2 rule 2 and migration delta §6.
	sb.WriteString("(allow file-read* file-test-existence file-map-executable file-read-metadata\n")
	sb.WriteString("  (subpath \"/nix\")\n")
	sb.WriteString("  (subpath \"/usr\")\n")
	sb.WriteString("  (subpath \"/bin\")\n")
	sb.WriteString("  (subpath \"/sbin\")\n")
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
	sb.WriteString("  (literal \"/dev/null\")\n")
	sb.WriteString("  (literal \"/dev/random\")\n")
	sb.WriteString("  (literal \"/dev/urandom\")\n")
	sb.WriteString("  (literal \"/dev/dtracehelper\")\n")
	sb.WriteString("  (literal \"/\"))\n")
	sb.WriteString("\n")

	// ── 3. /tmp read-write ────────────────────────────────────────────────
	// /tmp and /private/tmp are used by xcrun, git, and other tools for
	// transient files. Both symlink forms needed. See F.1 §2 rule 3.
	sb.WriteString("(allow file-read* file-write* file-test-existence\n")
	sb.WriteString("  (subpath \"/private/tmp\")\n")
	sb.WriteString("  (subpath \"/tmp\"))\n")
	sb.WriteString("\n")

	// ── 4. Sensitive /etc subtree denies ──────────────────────────────────
	// These deny rules must follow the broad /etc and /private/etc allows
	// above. In SBPL, more-specific rules override broader ones. Both
	// /etc/... and /private/etc/... forms needed due to symlink
	// non-transparency (same pattern as PR #1193 for v1). See F.1 §2 rule 4.
	sb.WriteString("(deny file-read* file-write*\n")
	sb.WriteString("  (subpath \"/etc/wireguard\")\n")
	sb.WriteString("  (subpath \"/etc/wpa_supplicant\")\n")
	sb.WriteString("  (subpath \"/private/etc/wireguard\")\n")
	sb.WriteString("  (subpath \"/private/etc/wpa_supplicant\"))\n")
	sb.WriteString("\n")

	// ── 5. Host ~/.aws deny — keep host credentials invisible ─────────────
	// The staging HOME contains symlinks to the staged .aws credential
	// entries (per issue #1017). The host raw ~/.aws subtree must remain
	// read-denied so that path traversal cannot bypass the staging map.
	if home != "" {
		awsPath := filepath.Join(home, ".aws")
		sb.WriteString("(deny file-read* file-write*\n")
		sb.WriteString("  (subpath " + quoteSBPL(awsPath) + "))\n")
		sb.WriteString("\n")
	}

	// ── 6. Staging HOME + worktree + bare repo + host-API socket (RW) ────
	// Session-specific read-write paths. Locked in #1012 and #1017.
	// file-test-existence and file-read-metadata added alongside file-read*
	// and file-write* for dyld/AMFI compatibility in the v3 profile.
	if stagingErr == nil {
		sb.WriteString("(allow file-read* file-write* file-test-existence file-read-metadata\n")
		sb.WriteString("  (subpath " + quoteSBPL(stagingHome) + ")\n")
		// Worktree — the git checkout the agent works in.
		if m.cfg.Worktree != "" {
			sb.WriteString("  (subpath " + quoteSBPL(m.cfg.Worktree) + ")\n")
		}
		// Bare repo — the .bare directory holding git objects and refs.
		if m.cfg.BareRoot != "" {
			bareDir := filepath.Join(m.cfg.BareRoot, ".bare")
			sb.WriteString("  (subpath " + quoteSBPL(bareDir) + ")\n")
		}
		// Host-API socket directory — the sidecar's per-session socket dir.
		if m.cfg.HostAPISockPath != "" {
			sockDir := filepath.Dir(m.cfg.HostAPISockPath)
			sb.WriteString("  (subpath " + quoteSBPL(sockDir) + ")\n")
		}
		// opencode shared state dir — ~/.local/share/opencode (SQLite DB, logs,
		// snapshots). Must use (subpath ...) not (literal ...) so the sandbox can
		// read/write files inside the directory (not just the directory node itself).
		if home != "" {
			opencodeDataDir := filepath.Join(home, ".local", "share", "opencode")
			sb.WriteString("  (subpath " + quoteSBPL(opencodeDataDir) + ")\n")
		}
		sb.WriteString(")\n")
		sb.WriteString("\n")
	}

	// ── Symlink target allows (read-only) ─────────────────────────────────
	// For every symlink in the staging HOME, resolve its target via
	// filepath.EvalSymlinks and emit an allow rule. Locked in #1012 and #1017.
	//
	// Rule type selection:
	//   - Directory targets → (subpath ...) so the sandbox can access files
	//     within (not just the directory node itself).
	//   - File targets → (literal ...) to allow the specific file.
	//
	// Targets that fall under the denied ~HOME/.aws subtree are excluded from
	// this block to avoid the Apple SBPL literal-over-subpath precedence issue
	// (a more-specific (literal) allow defeats a broader (subpath) deny).
	if stagingErr == nil {
		targets, targErr := collectStagingHomeSymlinkTargets(stagingHome)
		if targErr != nil {
			log.Printf("container: sandbox-exec: collect symlink targets: %v", targErr)
		}
		if len(targets) > 0 {
			sb.WriteString("(allow file-read*\n")
			for _, t := range targets {
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

	// ── 8. /dev/null write access ─────────────────────────────────────────
	// Apple-signed binaries (git, ssh) open /dev/null for writing during
	// startup. The file-read* above covers reads; writes need an explicit
	// allow. See F.1 §2 rule 8.
	sb.WriteString("(allow file-write-data\n")
	sb.WriteString("  (literal \"/dev/null\"))\n")
	sb.WriteString("\n")

	// ── 9. Process operations ─────────────────────────────────────────────
	// process-exec*  — execvp(2). Required for any child process to launch.
	// process-fork   — fork(2). git and ssh use it for child processes.
	// process-info*  — required by AMFI when validating the certificate chain
	//   of Apple-signed binaries (process-info-codesignature, process-info-pidinfo).
	//   Without this, git and ssh abort with SIGABRT during dyld init. See F.1 §2.
	// signal         — allow the sandboxed process to send signals to itself.
	// mach-lookup    — bootstrap service lookups (logd, opendirectoryd, etc.).
	// mach-register  — per-pid Mach name registration (opencode IPC).
	// sysctl-read    — system library init queries (kern.*, hw.*, machdep.*).
	// NOTE: iokit-open is REMOVED in v3 (not needed, see F.1 §4.1 / §2 note).
	// NOTE: ipc-posix-shm is REMOVED (unbound variable in v3 — replaced below).
	sb.WriteString("(allow process-exec* process-fork process-info* signal mach-lookup mach-register\n")
	sb.WriteString("       sysctl-read)\n")
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
	sb.WriteString("(allow network*)\n")

	return sb.String()
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
//	sandbox-exec -f <profile_path> opencode --port <port> --hostname 127.0.0.1 [--agent X] [--prompt Y]
//
// The first element ("sandbox-exec") is argv[0]; the caller invokes
// syscall.Exec("/usr/bin/sandbox-exec", args, env). After -f and the profile
// path comes the harness binary (opencode) and its arguments — the inner
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

	// The sandbox-exec wrapper precedes the harness invocation. HarnessInvocation
	// returns ["opencode", "--port", ..., "--hostname", "127.0.0.1", ...] and
	// handles the AllocatedPort ∥ ContainerPort fallback rule (matching bwrap).
	args := []string{"sandbox-exec", "-f", profilePath}
	args = append(args, HarnessInvocation(cfg)...)

	return args
}

// Run is a no-op stub for the production flow: agent-run launches
// sandbox-exec via syscall.Exec, replacing its own process image, so this
// Isolator's Run is never reached. The method exists to satisfy the
// Isolator interface and is kept symmetric with bwrap.Run for future tests.
func (s *sandboxExecIsolator) Run(ctx context.Context, args []string) error {
	return fmt.Errorf("container: sandbox-exec %q: Run is not implemented; agent-run uses syscall.Exec", s.name)
}

// Shutdown is a no-op for sandbox-exec: the production flow uses
// syscall.Exec from agent-run, so the sandbox-exec child is owned by the
// tmux pane's process tree and terminates when the pane dies. Lifecycle
// hardening (signal forwarding, kqueue parent-death watcher) is the
// subject of PR 4 (#1018).
func (s *sandboxExecIsolator) Shutdown() {}

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
