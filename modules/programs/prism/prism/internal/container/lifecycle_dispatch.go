// Package container manages sandbox lifecycle and mount preparation for
// prism agent sessions.
// This file extends the Isolator interface with the lifecycle dispatch methods
// migrated from the per-mode branches in container.go, lifecycle.go, cmd/cleanup.go,
// cmd/reset.go, and cmd/agent_run.go (issue #1140, A1.L1-L6).
//
// Methods added by this file:
//
//   - EnsureRemoved()   — L1: replaces Manager.EnsureRemoved + cleanup.go's open-coded teardown.
//   - WriteGitconfig()  — L2: replaces (*Manager).writeGitconfig(mode) per-mode switch.
//   - Reset()           — L3: replaces the old container sweep in cmd/reset.go.
//   - Prepare()         — L4: replaces Manager.PrepareBwrap / Manager.PrepareSandboxExec.
//   - Create()          — L5: replaces Manager.Create body.
//   - AgentRun()        — L6: replaces cmd/agent_run.go's per-mode dispatch switch.
//
// The methods are declared on the interface in isolator.go and implemented per
// isolator (bwrap, sandbox-exec, host) below.
package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// the sidecar-side instrumentation. Zero means "not
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
// bwrapIsolator
// ----------------------------------------------------------------------------

// EnsureRemoved cleans up the per-session temp files written by PrepareBwrap.
// Bwrap sessions do not own a container lifecycle — there is nothing to stop
// or rm — so this method is a temp-file unlink only. Mirrors the per-session
// list in cmd/cleanup.go:1055-1059 (the legacy 5-file cleanup; the
// harness-config file is intentionally excluded — see cleanupLegacyTempFiles
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

	// Write the harness config file, if provided.
	if m.cfg.ConfigContent != "" {
		if err := os.WriteFile(m.harnessConfigFilePath(), []byte(m.cfg.ConfigContent), 0o644); err != nil {
			return nil, fmt.Errorf("container: bwrap: write harness config: %w", err)
		}
	}

	// Pre-create directories referenced as bind-mount sources.
	// A non-nil error means a critical directory failed — return immediately
	// so the caller sees the real cause rather than a confusing bwrap exec failure.
	if err := m.prepareVolumeDirs(false); err != nil {
		return nil, fmt.Errorf("container: bwrap: %w", err)
	}

	// ── Containers-enabled prep (#2317 / #2321) ──────────────────────
	// When the session's agent_status.containers_enabled gate is set, the
	// bwrap profile binds <sessionDir>/container-scratch read-write so the
	// agent can use it as a `podman run -v ...` source. The directory must
	// exist before bwrap is execed — bwrap aborts with a confusing error if a
	// bind source is missing. Per #2321 we converge on the same
	// per-session-work-dir story sandbox-exec uses (option (a)) rather than
	// inlining a one-off mkdir here, so the lifecycle (PrepareSessionWorkDir
	// + RemoveSessionWorkDir) is shared between the two isolation modes.
	//
	// Validation order:
	//   1. PodmanProxySockPath must be set (config error if absent).
	//   2. <runDir> = filepath.Dir(PodmanProxySockPath) must already exist on
	//      disk — prepareVolumeDirs above creates the per-session run dir from
	//      HostAPISockPath, and PodmanProxySockPath shares the same parent
	//      directory in the sidecar's path scheme
	//      (session.SidecarPodmanProxyPath). A missing runDir at this point
	//      indicates the two paths disagree and the rendered argv would fail
	//      at bwrap exec time — surface it now as a clear Prepare error.
	//   3. PrepareSessionWorkDir + mkdir of the container-scratch subdir.
	if m.cfg.ContainersEnabled {
		if m.cfg.PodmanProxySockPath == "" {
			return nil, fmt.Errorf("container: bwrap: containers_enabled=true but PodmanProxySockPath is empty")
		}
		runDir := filepath.Dir(m.cfg.PodmanProxySockPath)
		if info, err := os.Stat(runDir); err != nil {
			return nil, fmt.Errorf("container: bwrap: containers_enabled=true but podman proxy run dir %q does not exist: %w", runDir, err)
		} else if !info.IsDir() {
			return nil, fmt.Errorf("container: bwrap: containers_enabled=true but podman proxy run dir %q is not a directory", runDir)
		}
		sessionDir, err := m.PrepareSessionWorkDir()
		if err != nil {
			return nil, fmt.Errorf("container: bwrap: prepare session work dir for containers_enabled: %w", err)
		}
		scratch := SessionWorkDirContainerScratchPath(sessionDir)
		if err := os.MkdirAll(scratch, 0o700); err != nil {
			return nil, fmt.Errorf("container: bwrap: create container-scratch dir %q: %w", scratch, err)
		}
	}

	// Build the bwrap args. For PI sessions, BuildArgs stores any
	// appendPIBwrapMounts error in m.piBwrapErr because BuildArgs cannot return
	// an error. Check and surface it here where we CAN return an error.
	args := b.BuildArgs(m)
	if m.piBwrapErr != nil {
		return nil, fmt.Errorf("container: bwrap: %w", m.piBwrapErr)
	}
	return args, nil
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
// sandbox-exec specific files (SBPL profile, session work dir, harness
// config) are owned by the Manager-level lifecycle (Manager.EnsureRemoved);
// EnsureRemoved here mirrors the legacy cleanup.go behaviour which only
// touched the 5-file legacy set.
func (s *sandboxExecIsolator) EnsureRemoved(ctx context.Context, m *Manager) {
	cleanupLegacyTempFiles(s.name)
}

// WriteGitconfig generates the gitconfig for the sandbox-exec sandbox. Post
// issue #2213 (Step 2 of #2132) the generated gitconfig lives in the
// per-session work dir (<sessionDir>/gitconfig) and embeds the stable sops
// symlink key paths (~/.ssh/<keyname>) rather than staging-HOME paths; the
// dispatcher wires it in via GIT_CONFIG_GLOBAL. This delegates to the same
// work-dir writer that PrepareSessionWorkDir uses.
func (s *sandboxExecIsolator) WriteGitconfig(m *Manager) error {
	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		return fmt.Errorf("container: sandbox-exec: %w", err)
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return fmt.Errorf("container: sandbox-exec: create session work dir %s: %w", sessionDir, err)
	}
	return m.writeGitconfigToDir(sessionDir)
}

// Reset is a no-op for sandbox-exec today — same rationale as bwrap.Reset.
func (s *sandboxExecIsolator) Reset(ctx context.Context) error {
	return nil
}

// Prepare prepares the per-session work dir, writes the SBPL profile, and
// returns the complete sandbox-exec argument list. Mirrors the pre-refactor
// body of Manager.PrepareSandboxExec (internal/container/container.go:637).
//
// If PrepareSessionWorkDir fails, Prepare returns an error immediately and
// does not write the profile or launch the session (issue #1879 hard-fail
// posture): a session without the work-dir configs would have no git
// identity and no ssh auth route, and the #1960 missing-git-identity hard
// error must surface at Prepare time.
func (s *sandboxExecIsolator) Prepare(ctx context.Context, m *Manager) ([]string, error) {
	// Prepare the per-session work dir (issue #2213, Step 2 of #2132): the
	// generated ssh-config / gitconfig / allowed_signers the dispatcher wires
	// in via GIT_SSH_COMMAND / GIT_CONFIG_GLOBAL, plus the chromium Library
	// skeleton (issue #2247).
	if _, err := m.PrepareSessionWorkDir(); err != nil {
		return nil, fmt.Errorf("container: sandbox-exec: cannot prepare session work dir: %w", err)
	}

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

// Prepare is not the entry point for host mode — host runs the agent
// directly in the tmux pane via the BuildOpencodeCmd (DirectCmd) path.
// Returns an error so callers route correctly.
func (h *hostIsolator) Prepare(ctx context.Context, m *Manager) ([]string, error) {
	return nil, fmt.Errorf("container: host does not use Prepare; pi launches directly in the tmux pane")
}

// Create is a no-op for host mode: the agent is launched directly by the
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
// The harness-config, SBPL-profile, and session-work-dir files are
// deliberately NOT cleaned here: those are owned by the Manager-level
// lifecycle (Manager.EnsureRemoved retains the full cleanup list). Tests
// that exercise the cleanup.go shortcut path (see cmd/restore_test.go's
// legacy-mode coverage) rely on the
// harness-config file surviving this cleanup.
func cleanupLegacyTempFiles(name string) {
	_ = os.Remove(sessionTempPath("gitdir", "", name))
	_ = os.Remove(sessionTempPath("wt-gitdir", "", name))
	_ = os.Remove(sessionTempPath("ssh-config", "", name))
	_ = os.Remove(sessionTempPath("gitconfig", "", name))
	_ = os.Remove(sessionTempPath("allowed-signers", "", name))
}
