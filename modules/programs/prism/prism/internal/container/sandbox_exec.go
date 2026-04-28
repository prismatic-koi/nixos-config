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
// The profile shape is locked in by issue #1012. It includes:
//   - Read-only system roots (/nix, /usr, /System, /Library, /private/etc, …)
//   - Deny of sensitive /private/etc subtrees (wireguard, wpa_supplicant)
//   - Process and IPC primitives required by node/opencode
//   - (allow network*)
//   - (allow file-read* file-write* (subpath "<STAGING_HOME>") ...) for the
//     staging HOME, worktree, bare repo, and host-API socket dir
//   - (allow file-read* (literal "<resolved_target>") ...) for every symlink
//     target in the staging HOME, resolved via filepath.EvalSymlinks
//   - (deny file-read* file-write* (subpath "$HOME/.aws")) to keep the host
//     ~/.aws invisible (only the staged entries are accessible)
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
	sb.WriteString("(version 1)\n")
	sb.WriteString("(deny default)\n")
	sb.WriteString("\n")

	// ── Read-only system roots ────────────────────────────────────────────
	// Mirror the bwrap baseline ro-binds (/nix, /etc, /run/current-system,
	// /bin, /run/wrappers) for the macOS layout: /nix on macOS is the Nix
	// store; /usr, /System, /Library, /private/etc, /private/var/db/dyld,
	// and /private/var/db/timezone are the read-only OS paths that node and
	// opencode resolve at startup. See #1012 — Design — SBPL profile shape.
	//
	// Both /etc and /private/etc are required: on macOS /etc is a top-level
	// symlink to /private/etc, but sandbox-exec does NOT transparently follow
	// that symlink for access checks — paths starting with /etc/... are checked
	// against the allow rules separately from paths starting with /private/etc/...
	// This means execvp(2) on /etc/profiles/per-user/<user>/bin/opencode (the
	// canonical Nix per-user profile path) would be denied if only /private/etc
	// is allowed. Adding /etc here fixes issue #1187.
	sb.WriteString("(allow file-read*\n")
	sb.WriteString("  (subpath \"/nix\")\n")
	sb.WriteString("  (subpath \"/usr\")\n")
	sb.WriteString("  (subpath \"/System\")\n")
	sb.WriteString("  (subpath \"/Library\")\n")
	sb.WriteString("  (subpath \"/etc\")\n")
	sb.WriteString("  (subpath \"/private/etc\")\n")
	sb.WriteString("  (subpath \"/private/var/db/dyld\")\n")
	sb.WriteString("  (subpath \"/private/var/db/timezone\"))\n")
	sb.WriteString("\n")

	// ── Selective shadowing of sensitive subtrees ─────────────────────────
	// Symmetric to bwrap's --tmpfs shadow of /etc/wireguard and
	// /etc/wpa_supplicant. Under sandbox-exec, an explicit (deny ...) inside
	// an otherwise-allow scope wins by precedence rules.
	//
	// Both /etc/... and /private/etc/... forms must be denied: the same
	// symlink non-transparency that required adding (subpath "/etc") to the
	// allow list also means that (subpath "/private/etc/wireguard") alone does
	// NOT block access via the /etc/wireguard path. Both path forms are
	// independently evaluated by the kernel. See issue #1187.
	sb.WriteString("(deny file-read* file-write*\n")
	sb.WriteString("  (subpath \"/etc/wireguard\")\n")
	sb.WriteString("  (subpath \"/etc/wpa_supplicant\")\n")
	sb.WriteString("  (subpath \"/private/etc/wireguard\")\n")
	sb.WriteString("  (subpath \"/private/etc/wpa_supplicant\"))\n")
	sb.WriteString("\n")

	// ── Host ~/.aws deny — keep host credentials invisible ────────────────
	// Only the staged entries (symlinked through the staging HOME) are
	// accessible to the sandbox. The host's raw ~/.aws subtree is explicitly
	// denied so that accidental reads cannot bypass the staging remapping.
	// This is a security requirement per issue #1017.
	if home != "" {
		awsPath := filepath.Join(home, ".aws")
		sb.WriteString("(deny file-read* file-write*\n")
		sb.WriteString("  (subpath " + quoteSBPL(awsPath) + "))\n")
		sb.WriteString("\n")
	}

	// ── Staging HOME + worktree + bare repo + host-API socket dir (RW) ───
	// These are the paths the sandbox must be able to read and write.
	// Locked in #1012 and #1017.
	if stagingErr == nil {
		sb.WriteString("(allow file-read* file-write*\n")
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

	// ── Process and IPC primitives required by node/opencode ──────────────
	// Locked in #1012. PID/UTS/IPC isolation is a documented gap on macOS;
	// this is defence in depth, not adversarial workload isolation.
	sb.WriteString("(allow process-exec* process-fork signal mach-lookup mach-register\n")
	sb.WriteString("       sysctl-read iokit-open ipc-posix-shm)\n")
	sb.WriteString("\n")

	// ── Network ───────────────────────────────────────────────────────────
	// Locked in #1012 — match bwrap. Restriction to specific hosts is a
	// future concern, applied symmetrically.
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
