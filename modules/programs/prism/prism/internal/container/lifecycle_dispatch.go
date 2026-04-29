// Package container manages the podman container lifecycle for prism sidecar.
// This file extends the Isolator interface with the lifecycle dispatch methods
// migrated from the per-mode branches in container.go, lifecycle.go, cmd/cleanup.go,
// cmd/reset.go, and cmd/agent_run.go (issue #1140, A1.L1-L6).
//
// Methods added by this file:
//
//   - EnsureRemoved()   — L1: replaces Manager.EnsureRemoved + cleanup.go's open-coded podman stop/rm.
//   - WriteGitconfig()  — L2: replaces (*Manager).writeGitconfig(mode) per-mode switch.
//   - Reset()           — L3: replaces resetRemovePodmanContainers in cmd/reset.go.
//   - Prepare()         — L4: replaces Manager.PrepareBwrap / Manager.PrepareSandboxExec.
//   - Create()          — L5: replaces Manager.Create body.
//   - AgentRun()        — L6: replaces cmd/agent_run.go's per-mode dispatch switch.
//
// The methods are declared on the interface in isolator.go and implemented per
// isolator (podman, bwrap, sandbox-exec, host) below.
package container

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
)

// AgentRunOpts carries the per-call inputs that a registered AgentRun handler
// needs without coupling the container package to internal/db, internal/session,
// internal/git, or internal/harness. The cmd-level dispatch in
// cmd/agent_run.go fills this in and passes it to iso.AgentRun. The handlers
// themselves live in the cmd package and are registered with the container
// package at init() time via RegisterAgentRunHandler.
//
// Keeping the handlers in cmd/ is a deliberate scope decision: moving the
// bodies into internal/container would create a circular dependency with
// internal/session (which imports internal/container today). The dispatch
// shape — `iso.AgentRun(ctx, opts)` — is the deliverable; the body location
// is an implementation detail.
type AgentRunOpts struct {
	// SessionName is the prism session name (e.g. "nixos-config@feature").
	SessionName string

	// StartTime is the wall-clock entry time captured by the cmd
	// dispatcher. Handlers use it to emit `[timing]` markers symmetric to
	// the podman path's sidecar-side instrumentation. Zero means "not
	// captured" — handlers fall back to time.Now() in that case.
	StartTime time.Time

	// LogFile is the per-session agent-run log file (already opened in
	// append mode by the cmd dispatcher). May be nil when the log file
	// cannot be opened — handlers tolerate nil.
	LogFile *os.File
}

// AgentRunHandler is the function signature that cmd/agent_run.go registers
// with the container package at init() time, one entry per per-mode dispatch
// path. The handler is responsible for the body that previously lived in the
// per-mode branch of runAgentRun.
type AgentRunHandler func(ctx context.Context, opts AgentRunOpts) error

var (
	agentRunHandlersMu sync.Mutex
	agentRunHandlers   = map[config.IsolationMode]AgentRunHandler{}
)

// RegisterAgentRunHandler registers fn as the AgentRun body for mode. The
// cmd package calls this at init() time for the bwrap and sandbox-exec
// modes — the only two modes for which `prism agent-run` is the entry
// point. Re-registering panics: the registration is intended to be a
// once-per-process action.
func RegisterAgentRunHandler(mode config.IsolationMode, fn AgentRunHandler) {
	agentRunHandlersMu.Lock()
	defer agentRunHandlersMu.Unlock()
	if _, exists := agentRunHandlers[mode]; exists {
		panic(fmt.Sprintf("container: AgentRun handler for %q already registered", mode))
	}
	agentRunHandlers[mode] = fn
}

// lookupAgentRunHandler returns the registered handler for mode (or nil if
// none is registered). Used by per-isolator AgentRun bodies below.
func lookupAgentRunHandler(mode config.IsolationMode) AgentRunHandler {
	agentRunHandlersMu.Lock()
	defer agentRunHandlersMu.Unlock()
	return agentRunHandlers[mode]
}

// ----------------------------------------------------------------------------
// podmanIsolator
// ----------------------------------------------------------------------------

// EnsureRemoved tears down the podman container with this isolator's name and
// cleans up the legacy set of per-session temp files (gitdir, wt-gitdir,
// ssh-config, gitconfig, allowed-signers). Mirrors the pre-refactor body of
// cmd/cleanup.go's removeContainerIfExists — same podman stop/rm sequence,
// same temp-file unlink list. Errors for "no such container" are silently
// swallowed, exactly as before.
//
// The opencode-config, Claude credentials, and sandbox-exec staging files
// are NOT removed here. They are owned by the Manager-level lifecycle
// (Manager.EnsureRemoved) which retains the comprehensive cleanup list to
// preserve the pre-refactor "Create resets every per-session artefact"
// behaviour. The cleanup.go shortcut path historically did not touch
// opencode-config either; tests like TestRestoreSession_PodmanMode_TempFileWritten
// assert that the opencode-config written by restore.go survives a subsequent
// removeContainerIfExists call.
//
// m may be nil for callers that do not own a Manager (cmd/cleanup.go's
// removeContainerIfExists). When m is nil the implementation falls back to
// the per-session temp paths derived from the isolator's name (which is the
// container name for podman — see NameForSession).
func (p *podmanIsolator) EnsureRemoved(ctx context.Context, m *Manager) {
	// Clean up the legacy set of per-session temp files (gitdir, wt-gitdir,
	// ssh-config, gitconfig, allowed-signers). The opencode-config and
	// sandbox-exec specific files are deliberately excluded here.
	cleanupLegacyTempFiles(p.name)

	// Check the container's instance label when we have our own InstanceID.
	// This detects ownership mismatches where a container from a previous
	// session incarnation is being cleaned up by a new one.
	if m != nil && m.cfg.InstanceID != "" {
		inspectCtx, inspectCancel := context.WithTimeout(ctx, 5*time.Second)
		out, inspectErr := exec.CommandContext(inspectCtx, "podman", "inspect",
			"--format", `{{index .Config.Labels "prism.instance-id"}}`,
			p.name,
		).Output()
		inspectCancel()
		if inspectErr == nil {
			containerInstanceID := strings.TrimSpace(string(out))
			if containerInstanceID != "" && containerInstanceID != m.cfg.InstanceID {
				log.Printf("container: warning: container %q has instance-id %q but current session has %q — removing anyway",
					p.name, containerInstanceID, m.cfg.InstanceID)
			}
		}
	}

	// Stop the container (ignore errors — may not be running).
	stopCmd := exec.CommandContext(ctx, "podman", "stop", "--time", "10", p.name)
	if out, err := stopCmd.CombinedOutput(); err != nil {
		// Only log if it looks like a real error (not "no such container").
		if !IsNoSuchContainerError(string(out)) {
			log.Printf("container: stop existing %q: %v — %s", p.name, err, strings.TrimSpace(string(out)))
		}
	}

	// Remove the container (ignore errors — may not exist).
	rmCmd := exec.CommandContext(ctx, "podman", "rm", "--force", p.name)
	if out, err := rmCmd.CombinedOutput(); err != nil {
		if !IsNoSuchContainerError(string(out)) {
			log.Printf("container: rm existing %q: %v — %s", p.name, err, strings.TrimSpace(string(out)))
		}
	}
}

// WriteGitconfig generates a minimal .gitconfig for the podman container and
// writes it to the per-session temp path. The container runs as root, so the
// signingKey and allowedSignersFile paths use /root/.ssh/* — see the
// container.go writeGitconfig body (lines 425-550) for the full rationale.
func (p *podmanIsolator) WriteGitconfig(m *Manager) error {
	return m.writeGitconfig(isolationPodman)
}

// Reset performs the heavier "wipe everything matching prism-*" cleanup
// invoked by `prism reset`. Lists every container with the "prism-" name
// prefix and removes them with `podman rm -f`. If podman is not available
// the function is a no-op.
//
// Mirrors the pre-refactor resetRemovePodmanContainers (cmd/reset.go:129)
// — same prefix scan, same single `podman rm -f <ids...>` invocation.
func (p *podmanIsolator) Reset(ctx context.Context) error {
	podmanBin, err := exec.LookPath("podman")
	if err != nil {
		// podman not installed — nothing to do.
		fmt.Println("  podman not found — skipping container cleanup.")
		return nil
	}

	// Fetch "name\tid" for every container (including stopped ones).
	out, err := exec.CommandContext(ctx, podmanBin, "ps", "-a", "--format", "{{.Names}}\t{{.ID}}").Output()
	if err != nil {
		// If podman fails (e.g. no running machine), treat as no containers.
		fmt.Printf("  podman ps failed: %v — skipping container cleanup.\n", err)
		return nil
	}

	var prismContainers []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		// parts[0] may be "name" or "name1,name2,..."; check the first name only.
		firstName := strings.SplitN(parts[0], ",", 2)[0]
		if strings.HasPrefix(firstName, "prism-") {
			prismContainers = append(prismContainers, strings.TrimSpace(parts[1]))
		}
	}

	if len(prismContainers) == 0 {
		fmt.Println("  no prism- containers found.")
		return nil
	}

	fmt.Printf("  removing %d container(s)\n", len(prismContainers))
	args := append([]string{"rm", "-f"}, prismContainers...)
	rmOut, err := exec.CommandContext(ctx, podmanBin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman rm -f: %w\n%s", err, strings.TrimSpace(string(rmOut)))
	}
	fmt.Println("  containers removed.")
	return nil
}

// Prepare is not the entry point for podman — Manager.Create handles podman
// session start. Returns an error so callers that route through the registry
// surface the misuse loudly rather than silently doing nothing.
func (p *podmanIsolator) Prepare(ctx context.Context, m *Manager) ([]string, error) {
	return nil, fmt.Errorf("container: podman does not use Prepare; use Create")
}

// Create is the body of the podman session-start path. Mirrors the
// pre-refactor Manager.Create (internal/container/lifecycle.go:79):
// EnsureRemoved → write gitdir/wt-gitdir → write SSH config → write
// gitconfig → write opencode config → write Claude credentials → pre-create
// volume dirs → buildRunArgs → isolator.Run.
func (p *podmanIsolator) Create(ctx context.Context, m *Manager) error {
	createStart := time.Now()

	// Remove any stale container first (AC-15). Use the Manager-level
	// EnsureRemoved so the comprehensive (mode-agnostic) temp-file unlink
	// runs — this matches the pre-refactor behaviour where Create cleared
	// every per-session temp file before writing fresh ones.
	m.EnsureRemoved(ctx)
	log.Printf("[timing] EnsureRemoved: %s", time.Since(createStart).Round(time.Millisecond))

	// Write corrected git pointer files for the container.
	if m.cfg.BareRoot != "" && m.cfg.WorktreeGitDir != "" {
		branch := filepath.Base(m.cfg.WorktreeGitDir)

		// Forward pointer: /workspace/.git → /prism-git/worktrees/<branch> (#492).
		gitdirContent := "gitdir: /prism-git/worktrees/" + branch + "\n"
		if err := os.WriteFile(m.gitdirFilePath(), []byte(gitdirContent), 0o644); err != nil {
			return fmt.Errorf("container: write gitdir file: %w", err)
		}

		// Back-pointer: worktrees/<branch>/gitdir → /workspace/.git
		wtGitdirContent := "/workspace/.git\n"
		if err := os.WriteFile(m.worktreeGitdirFilePath(), []byte(wtGitdirContent), 0o644); err != nil {
			return fmt.Errorf("container: write worktree gitdir file: %w", err)
		}
	}

	// Write a minimal SSH config for the container.
	if err := m.writeSshConfig(isolationPodman); err != nil {
		return err
	}

	// Write a minimal .gitconfig for the container.
	t0 := time.Now()
	if err := p.WriteGitconfig(m); err != nil {
		return fmt.Errorf("container: write gitconfig: %w", err)
	}
	log.Printf("[timing] writeGitconfig: %s", time.Since(t0).Round(time.Millisecond))

	// Write the opencode config file for the container.
	if m.cfg.ConfigContent != "" {
		if err := os.WriteFile(m.opencodeConfigFilePath(), []byte(m.cfg.ConfigContent), 0o644); err != nil {
			return fmt.Errorf("container: write opencode config: %w", err)
		}
	}

	// On Darwin, extract Claude Code credentials from the macOS Keychain.
	m.writeClaudeCredentials()

	// Pre-create directories that buildRunArgs() will reference as volume mount sources.
	if err := m.prepareVolumeDirs(true); err != nil {
		log.Printf("container: prepareVolumeDirs partial failure: %v", err)
	}

	// Build the podman run arguments.
	t0 = time.Now()
	args := m.buildRunArgs()
	log.Printf("[timing] buildRunArgs: %s", time.Since(t0).Round(time.Millisecond))

	log.Printf("container: creating %q: podman %s", p.name, strings.Join(redactArgs(args), " "))

	podmanStart := time.Now()
	if err := m.isolator.Run(ctx, args); err != nil {
		return err
	}
	log.Printf("[timing] podman run: %s (total to here: %s)",
		time.Since(podmanStart).Round(time.Millisecond),
		time.Since(createStart).Round(time.Millisecond))
	log.Printf("[timing] Create total: %s", time.Since(createStart).Round(time.Millisecond))

	return nil
}

// AgentRun is not the entry point for podman: the podman path uses the
// sidecar's restart loop and `podman attach`, not `prism agent-run`. Returns
// an error so callers surface the misuse loudly. This replaces the manual
// `else` arm in cmd/agent_run.go's per-mode switch (issue #1140 AC).
func (p *podmanIsolator) AgentRun(ctx context.Context, opts AgentRunOpts) error {
	return fmt.Errorf("agent-run: session %q has isolation mode %q; this command is only for bwrap and sandbox-exec sessions",
		opts.SessionName, config.IsolationPodman)
}

// ----------------------------------------------------------------------------
// bwrapIsolator
// ----------------------------------------------------------------------------

// EnsureRemoved cleans up the per-session temp files written by PrepareBwrap.
// Bwrap sessions do not own a container lifecycle — there is nothing to stop
// or rm — so this method is a temp-file unlink only. Mirrors the per-session
// list in cmd/cleanup.go:1055-1059 (the legacy 5-file cleanup; the
// opencode-config file is intentionally excluded — see podmanIsolator.EnsureRemoved
// for the rationale).
func (b *bwrapIsolator) EnsureRemoved(ctx context.Context, m *Manager) {
	cleanupLegacyTempFiles(b.name)
}

// WriteGitconfig generates a minimal .gitconfig for the bwrap sandbox. bwrap
// runs as the host user, so the signingKey and allowedSignersFile paths
// embed the host user's $HOME (not /root). See container.go writeGitconfig
// for the full rationale.
func (b *bwrapIsolator) WriteGitconfig(m *Manager) error {
	return m.writeGitconfig(isolationBwrap)
}

// Reset is a no-op for bwrap today. Orphan-agent-run reaping (the bwrap
// equivalent of "wipe every prism-* container") is a future implementation
// — A1 §7 names the shape, the cleanup work lands later.
func (b *bwrapIsolator) Reset(ctx context.Context) error {
	return nil
}

// Prepare writes the per-session temp files (SSH config, gitconfig,
// opencode.json config) that bwrap needs at start time and returns the
// complete bwrap argument list. Mirrors the pre-refactor body of
// Manager.PrepareBwrap (internal/container/container.go:581).
//
// Like the previous implementation, this also pre-creates bind-mount source
// directories so bwrap (which silently fails on missing sources) can find
// them.
func (b *bwrapIsolator) Prepare(ctx context.Context, m *Manager) ([]string, error) {
	// Write a minimal SSH config for bwrap.
	if err := m.writeSshConfig(isolationBwrap); err != nil {
		return nil, fmt.Errorf("container: bwrap: %w", err)
	}

	// Write the gitconfig.
	if err := b.WriteGitconfig(m); err != nil {
		return nil, fmt.Errorf("container: bwrap: write gitconfig: %w", err)
	}

	// Write the opencode config file, if provided.
	if m.cfg.ConfigContent != "" {
		if err := os.WriteFile(m.opencodeConfigFilePath(), []byte(m.cfg.ConfigContent), 0o644); err != nil {
			return nil, fmt.Errorf("container: bwrap: write opencode config: %w", err)
		}
	}

	// Pre-create directories referenced as bind-mount sources.
	if err := m.prepareVolumeDirs(false); err != nil {
		log.Printf("container: bwrap: prepareVolumeDirs partial failure: %v", err)
	}

	// Build the bwrap args.
	return b.BuildArgs(m), nil
}

// Create is not the entry point for bwrap — the bwrap path uses Prepare +
// `prism agent-run`. Returns an error so callers that route through the
// registry surface the misuse loudly.
func (b *bwrapIsolator) Create(ctx context.Context, m *Manager) error {
	return fmt.Errorf("container: bwrap does not use Create; use Prepare + prism agent-run")
}

// AgentRun dispatches to the registered bwrap handler in cmd/agent_run.go.
// Returns an error if no handler is registered (programming error: cmd/
// must call RegisterAgentRunHandler at init() time).
func (b *bwrapIsolator) AgentRun(ctx context.Context, opts AgentRunOpts) error {
	fn := lookupAgentRunHandler(config.IsolationBwrap)
	if fn == nil {
		return errors.New("container: no AgentRun handler registered for bwrap")
	}
	return fn(ctx, opts)
}

// ----------------------------------------------------------------------------
// sandboxExecIsolator
// ----------------------------------------------------------------------------

// EnsureRemoved cleans up the legacy per-session temp files. The
// sandbox-exec specific files (SBPL profile, staging HOME, Claude
// credentials, opencode config) are owned by the Manager-level lifecycle
// (Manager.EnsureRemoved); EnsureRemoved here mirrors the legacy cleanup.go
// behaviour which only touched the 5-file legacy set.
func (s *sandboxExecIsolator) EnsureRemoved(ctx context.Context, m *Manager) {
	cleanupLegacyTempFiles(s.name)
}

// WriteGitconfig generates a minimal .gitconfig for the sandbox-exec sandbox.
// sandbox-exec shares the host user's $HOME, so the signingKey and
// allowedSignersFile paths embed the host user's $HOME (not /root). The
// existing isolationBwrap branch in container.go's writeGitconfig already
// handles the "host $HOME" path layout — sandbox-exec is structurally the
// same here, so we re-use it.
func (s *sandboxExecIsolator) WriteGitconfig(m *Manager) error {
	return m.writeGitconfig(isolationBwrap)
}

// Reset is a no-op for sandbox-exec today — same rationale as bwrap.Reset.
func (s *sandboxExecIsolator) Reset(ctx context.Context) error {
	return nil
}

// Prepare prepares the per-session staging HOME, writes the SBPL profile,
// and returns the complete sandbox-exec argument list. Mirrors the
// pre-refactor body of Manager.PrepareSandboxExec
// (internal/container/container.go:637).
func (s *sandboxExecIsolator) Prepare(ctx context.Context, m *Manager) ([]string, error) {
	// Populate the staging HOME with symlinks. Non-fatal if the staging HOME
	// cannot be created (e.g. read-only home, as in the nix sandbox build
	// environment): log and continue with a degraded profile.
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		log.Printf("container: sandbox-exec: prepare staging home: %v — launching with degraded profile", err)
	}
	_ = stagingHome // consumed by generateProfile via m.sandboxExecHomePath()

	// On Darwin, extract Claude Code credentials from the Keychain.
	m.writeClaudeCredentials()

	if _, err := writeProfile(m); err != nil {
		return nil, err
	}

	return s.BuildArgs(m), nil
}

// Create is not the entry point for sandbox-exec — the sandbox-exec path
// uses Prepare + `prism agent-run`. Returns an error so callers that route
// through the registry surface the misuse loudly.
func (s *sandboxExecIsolator) Create(ctx context.Context, m *Manager) error {
	return fmt.Errorf("container: sandbox-exec does not use Create; use Prepare + prism agent-run")
}

// AgentRun dispatches to the registered sandbox-exec handler in
// cmd/agent_run.go. Returns an error if no handler is registered.
func (s *sandboxExecIsolator) AgentRun(ctx context.Context, opts AgentRunOpts) error {
	fn := lookupAgentRunHandler(config.IsolationSandboxExec)
	if fn == nil {
		return errors.New("container: no AgentRun handler registered for sandbox-exec")
	}
	return fn(ctx, opts)
}

// ----------------------------------------------------------------------------
// hostIsolator
// ----------------------------------------------------------------------------

// EnsureRemoved is a no-op for host mode: there is no sandbox layer, no
// container, and no per-session temp files (the agent reads the host's
// own ~/.config/opencode/ directly).
func (h *hostIsolator) EnsureRemoved(ctx context.Context, m *Manager) {}

// WriteGitconfig is a no-op for host mode: the agent reads the host's own
// ~/.gitconfig directly, so there is nothing to materialise.
func (h *hostIsolator) WriteGitconfig(m *Manager) error { return nil }

// Reset is a no-op for host mode: there is no sandbox state to wipe.
func (h *hostIsolator) Reset(ctx context.Context) error { return nil }

// Prepare is not the entry point for host mode — host runs opencode
// directly in the tmux pane via the BuildOpencodeCmd (DirectCmd) path.
// Returns an error so callers route correctly.
func (h *hostIsolator) Prepare(ctx context.Context, m *Manager) ([]string, error) {
	return nil, fmt.Errorf("container: host does not use Prepare; opencode launches directly in the tmux pane")
}

// Create is a no-op for host mode: opencode is launched directly by the
// tmux pane command, not via a Create call.
func (h *hostIsolator) Create(ctx context.Context, m *Manager) error { return nil }

// AgentRun is not the entry point for host mode. Returns an error so the
// dispatch in cmd/agent_run.go surfaces the misuse loudly. This replaces
// the manual `else` arm in the per-mode switch (issue #1140 AC).
func (h *hostIsolator) AgentRun(ctx context.Context, opts AgentRunOpts) error {
	return fmt.Errorf("agent-run: session %q has isolation mode %q; this command is only for bwrap and sandbox-exec sessions",
		opts.SessionName, config.IsolationHost)
}

// ----------------------------------------------------------------------------
// shared helpers
// ----------------------------------------------------------------------------

// cleanupLegacyTempFiles removes the legacy set of per-session temp files
// previously inlined in cmd/cleanup.go's removeContainerIfExists (the
// 5-file list: gitdir, wt-gitdir, ssh-config, gitconfig, allowed-signers).
// The removals are best-effort — missing files are silently ignored.
//
// The opencode-config, Claude credentials, and sandbox-exec staging files
// are deliberately NOT cleaned here: those are owned by the Manager-level
// lifecycle (Manager.EnsureRemoved retains the full cleanup list). Tests
// that exercise the cleanup.go shortcut path
// (TestRestoreSession_PodmanMode_TempFileWritten) rely on the
// opencode-config file surviving this cleanup.
func cleanupLegacyTempFiles(name string) {
	_ = os.Remove(sessionTempPath("gitdir", "", name))
	_ = os.Remove(sessionTempPath("wt-gitdir", "", name))
	_ = os.Remove(sessionTempPath("ssh-config", "", name))
	_ = os.Remove(sessionTempPath("gitconfig", "", name))
	_ = os.Remove(sessionTempPath("allowed-signers", "", name))
}
