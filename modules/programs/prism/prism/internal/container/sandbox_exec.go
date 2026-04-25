// Package container manages the podman container lifecycle for prism sidecar.
// This file defines sandboxExecIsolator, an Apple sandbox-exec-based
// implementation of the Isolator interface. It is symmetric to bwrapIsolator
// in bwrap.go: BuildRunArgs() is a no-op stub, BuildArgs(m *Manager) is the
// concrete argument builder that has access to the Manager's config and state.
//
// This file implements PR 2 of the sandbox-exec design (issue #1012):
// minimal read-only profile that lets opencode launch and read system paths.
// Staging HOME, credentials, caches, and write paths are deferred to PR 3
// (#1017). Concurrency cap and lifecycle hardening are deferred to PR 4
// (#1018). New top-level allow/deny clauses introduced beyond what is listed
// in the issue body require a comment pointing back at #1012.
package container

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// BuildRunArgs satisfies the Isolator interface. It returns nil because the
// real argument construction requires Manager state and is implemented by the
// concrete BuildArgs(m *Manager) method below.
func (s *sandboxExecIsolator) BuildRunArgs() []string {
	return nil
}

// generateProfile returns the SBPL profile content for this session.
//
// The profile shape is locked in by issue #1012 and is exactly what is
// listed in the body of issue #1016 for this PR — minimal read-only
// system roots, the two deny subpaths for sensitive /private/etc subtrees,
// the process and IPC primitives required by node/opencode, and (allow
// network*). Staging-HOME, worktree, credential, and cache rules belong to
// the next PR (#1017) per the locked design.
//
// New top-level allow/deny clauses introduced beyond what is sketched in
// #1012 require a comment in the generator pointing at the issue.
func generateProfile(m *Manager) string {
	// Reference m to keep the symmetric BuildArgs(m) signature pattern. The
	// minimal read-only profile in this PR has no Manager-derived
	// substitutions; PR 3 (#1017) wires in staging HOME, worktree, and
	// credential paths that come from m.cfg.
	_ = m

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
	sb.WriteString("(allow file-read*\n")
	sb.WriteString("  (subpath \"/nix\")\n")
	sb.WriteString("  (subpath \"/usr\")\n")
	sb.WriteString("  (subpath \"/System\")\n")
	sb.WriteString("  (subpath \"/Library\")\n")
	sb.WriteString("  (subpath \"/private/etc\")\n")
	sb.WriteString("  (subpath \"/private/var/db/dyld\")\n")
	sb.WriteString("  (subpath \"/private/var/db/timezone\"))\n")
	sb.WriteString("\n")

	// ── Selective shadowing of sensitive subtrees ─────────────────────────
	// Symmetric to bwrap's --tmpfs shadow of /etc/wireguard and
	// /etc/wpa_supplicant. Under sandbox-exec, an explicit (deny ...) inside
	// an otherwise-allow scope wins by precedence rules.
	sb.WriteString("(deny file-read* file-write*\n")
	sb.WriteString("  (subpath \"/private/etc/wireguard\")\n")
	sb.WriteString("  (subpath \"/private/etc/wpa_supplicant\"))\n")
	sb.WriteString("\n")

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

// sandboxExecProfilePath returns the host path for the SBPL profile temp
// file written before sandbox-exec is launched. The path is namespaced by
// the session name so concurrent sessions never collide.
func (m *Manager) sandboxExecProfilePath() string {
	return filepath.Join(os.TempDir(), "prism-sandbox-exec-profile-"+m.name+".sb")
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

	args := []string{"sandbox-exec", "-f", profilePath, "opencode"}

	// Match the bwrap port-fallback rule for parity. AllocatedPort is
	// populated by agent-run from the DB's harness_port column in normal
	// operation; ContainerPort is the fallback for the theoretical case
	// where AllocatedPort is unset.
	opencodePort := cfg.AllocatedPort
	if opencodePort == 0 {
		opencodePort = ContainerPort
	}
	args = append(args,
		"--port", fmt.Sprintf("%d", opencodePort),
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
