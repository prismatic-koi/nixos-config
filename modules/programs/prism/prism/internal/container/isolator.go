// Package container manages the agent session container lifecycle for prism sidecar.
// This file defines the Isolator interface, which is the seam between the
// Manager and the underlying isolation mechanism. Implementations are
// bwrapIsolator (Linux), sandboxExecIsolator (Darwin), and hostIsolator.
package container

import (
	"context"

	"github.com/prismatic-koi/prism/internal/config"
)

// Capabilities is a flat struct of per-mode feature flags consulted by callers
// that today branch on the literal isolation mode value. A single
// iso.Capabilities() call is cheaper and more readable than a long parade of
// yes/no methods on the interface.
//
// Each flag is cited with the call sites that would collapse to a capability
// query after the per-phase migrations in A.1's §7.
type Capabilities struct {
	// IsContainer is always false now that podman isolation is removed.
	// Retained for call-site compatibility; callers checking IsContainer
	// will consistently receive false.
	IsContainer bool

	// OwnsContainerLifecycle is always false now that podman isolation is
	// removed. Retained for call-site compatibility; the sidecar's container
	// startup branch will never fire.
	OwnsContainerLifecycle bool

	// NeedsConfigBlob means the harness config blob must be supplied via
	// env-var or on-disk file before the process starts. True for bwrap
	// and sandbox-exec; false for host.
	// Cites: cmd/spawn.go:339-392, cmd/switch.go:308-348,
	//        cmd/restore.go:346-393, cmd/pr.go:120-186,
	//        internal/review/run.go:131-149, :356-374.
	NeedsConfigBlob bool

	// NeedsHostAPISocket means the sidecar binds the host-API Unix socket for
	// this mode. True for bwrap and sandbox-exec; false for host.
	// Cites: internal/sidecar/sidecar.go:512-547; cmd/sidecar.go:233-249.
	NeedsHostAPISocket bool

	// UsesContainerHarness means the harness adapter is constructed in
	// container mode vs host mode (bwrap, sandbox-exec, host).
	// Cites: cmd/sidecar.go:288-301.
	UsesContainerHarness bool

	// RestartOnExit means the sidecar restart-loop goroutine is started for
	// this mode. True for bwrap; false for sandbox-exec and host.
	// Cites: cmd/sidecar.go:348.
	RestartOnExit bool

	// NeedsStartupConnectTimeout means the sidecar's startup-connect timeout
	// fires for this mode. True for bwrap; false for all others.
	// Cites: internal/sidecar/sidecar.go:638-680.
	NeedsStartupConnectTimeout bool

	// NeedsReadinessWait means the agent-pane command should be prefixed by
	// the readiness-wait shell command. Always false now that podman is removed.
	// Retained for call-site compatibility.
	NeedsReadinessWait bool

	// EmitsTmuxStatusColumns means the tmux event hooks should seed
	// isolation-specific status columns for sessions in this mode. Always
	// false now that podman is removed.
	// Retained for call-site compatibility.
	EmitsTmuxStatusColumns bool
}

// Isolator is the interface that wraps the isolation-specific operations needed
// by Manager to create and manage an agent session container.
//
// An Isolator is responsible for:
//   - Reporting its identity (Name, Capabilities).
//   - Building the argument list for launching the isolated process.
//   - Executing the launch command.
//   - Shutting down the isolated process cleanly.
//   - Checking whether the isolated process has exited.
//   - Dumping logs from the isolated process for diagnostics.
//
// Manager constructs one Isolator per session and calls its methods
// exclusively.
type Isolator interface {
	// Name returns the canonical mode name as persisted in the database and
	// accepted by --isolation. The value is the IsolationRegistry key.
	// Cites: internal/config/config.go:18-43 (the IsolationMode constants).
	Name() config.IsolationMode

	// Capabilities returns the per-mode feature flags consulted by callers
	// that today branch on the literal mode value. See the Capabilities struct
	// above for the full set of flags and their citations.
	Capabilities() Capabilities

	// BuildRunArgs returns the complete argument list for launching the isolated
	// session process (e.g. all arguments after the "podman" binary for a
	// podman run invocation). The returned slice must not be modified by the
	// caller.
	BuildRunArgs() []string

	// Run launches the isolated process with the given argument list, using the
	// provided context for cancellation. args is the value returned by
	// BuildRunArgs. Returns an error if the process fails to start or exits
	// with a non-zero status.
	Run(ctx context.Context, args []string) error

	// Shutdown stops and removes the isolated process. It must use a
	// background context internally so cleanup proceeds even when the parent
	// context is already cancelled.
	Shutdown()

	// HasExited returns (true, exitCode) when the isolated process has
	// terminated, or (false, 0) when it is still running or its state cannot
	// be determined.
	HasExited() (bool, int)

	// DumpLogs writes the isolated process's stdout/stderr to the sidecar log
	// so startup failures are visible without racing the cleanup path.
	DumpLogs()

	// ----- A1.D1-D7 dispatch methods -----------------------------------------
	//
	// Implementations live in dispatch.go. Each method's body is mechanically
	// equivalent to the per-mode switch/if branch it replaces — see the
	// per-method comments in dispatch.go for the call-site citations.

	// Available reports whether this isolator can run on the current host.
	// Returns nil for "yes" or a wrapped, user-facing error describing the
	// missing prerequisite. For platform-only checks (bwrap → Linux,
	// sandbox-exec → Darwin) the error names the required platform.
	// Cites: cmd/spawn.go:190-204 (checkBwrapPlatform / checkSandboxExecPlatform);
	//        internal/container/container.go:1315 (CheckAvailability for podman).
	Available() error

	// Cap returns the soft concurrency-cap descriptor for this isolator.
	// It is the unified replacement for cmd/concurrency.go's parallel
	// checkConcurrencyCap (podman), checkBwrapConcurrencyCap (bwrap), and
	// checkSandboxExecConcurrencyCap (sandbox-exec) functions.
	//
	// dbPath is the path to prism.db; the implementation uses it to count
	// active sessions of this isolation mode, may merge with a live process
	// probe (podman), or may ignore it entirely (host: uncapped).
	//
	// The returned CapStatus carries the count, limit, exceeded flag, the
	// in-flight session list (for inclusion in the user-facing message), and
	// any per-mode probe-failure context. The caller uses CapStatus.Check(...)
	// to apply the --ignore-concurrency-cap policy.
	//
	// Reads Status.IsolationMode directly (NOT Status.EffectiveIsolationMode())
	// for forward-compatibility with A4.PE's removal.
	//
	// Implementations must NOT have side effects: Cap is called speculatively
	// before any worktree, DB row, or tmux session is created.
	//
	// Cites: cmd/concurrency.go (podman, bwrap, sandbox-exec per-mode helpers).
	Cap(ctx context.Context, dbPath string) CapStatus

	// WriteHarnessConfigBlob writes the harness configuration blob (the
	// role-specific opencode.json) to the deterministic per-session temp
	// path so it can be read back by the sandbox at start time. The same
	// gate that the call site applies (NeedsConfigBlob && content != "")
	// is encoded here as: empty content is a no-op, host returns nil.
	// sessionName is the prism session name; the isolator translates it
	// to the container name internally so the path matches the read site
	// (Manager.opencodeConfigFilePath).
	// Cites: cmd/spawn.go:386-392, cmd/pr.go:171-177, cmd/restore.go:385-388,
	//        cmd/switch.go:316-348 / :400-403, cmd/switch_project.go:161-503.
	WriteHarnessConfigBlob(sessionName, content string) error

	// AgentPaneCmd returns the shell command string emitted into the tmux
	// agent pane for this session.
	//   - podman:                "podman attach --sig-proxy=false <container>"
	//   - bwrap, sandbox-exec:   "prism agent-run --session <session>"
	//   - host:                  the DirectCmd supplied by the caller
	// Cites: internal/session/session.go:265-298 (BuildOpencodeCmd switch).
	AgentPaneCmd(opts AgentPaneOpts) string

	// SidecarFlags returns the per-mode argv extensions appended to the
	// `prism sidecar` command line for sessions that use this mode. The
	// non-conditional --isolation-mode flag is added by the caller (the
	// sidecar still needs to look up its own isolator after re-exec); this
	// method returns the rest.
	// Cites: internal/session/sidecar.go:311-353 (StartSidecarWithOpts argv).
	SidecarFlags(opts SidecarFlagOpts) []string

	// ArchivePaths returns the per-mode storage-root and extra-file set the
	// archive copy step consumes. home is the value of os.UserHomeDir(); it
	// is passed in rather than resolved here so callers can inject a temp
	// directory under test. sessionName is the prism session name (used
	// to derive the per-container storage path on podman and the agent-run
	// log path on bwrap / sandbox-exec).
	//
	// Stopgap pending #1142 (B6.IF — ArchiveAdapter interface): once that
	// lands, the archive-side dispatch moves to ArchiveAdapter and this
	// method may be removed.
	// Cites: internal/archive/archive.go:260-275 (resolveStorageRoot switch).
	ArchivePaths(home, sessionName string) ArchivePaths

	// LogPaths returns the per-mode log-file set for this session. Today
	// the values are zero-initialised — no caller dispatches per-mode for
	// log-path resolution. The method exists so future call sites can
	// route through registry.For(mode).LogPaths() without re-touching the
	// interface.
	// Cites: A1 §3 (LogPaths future shape).
	LogPaths() LogPaths

	// ----- A1.L1-L6 lifecycle methods ----------------------------------------
	//
	// Implementations live in lifecycle_dispatch.go. Each method's body is
	// mechanically equivalent to the per-mode branch it replaces — see the
	// per-method comments in lifecycle_dispatch.go for the call-site citations.

	// EnsureRemoved tears down any per-session state owned by this isolator
	// (containers, sandbox processes, temp files, staging directories).
	// It must use the supplied context for deadlines but proceed on
	// best-effort — errors for "already gone" are silently swallowed by the
	// implementation. m carries the Manager state (temp file paths,
	// InstanceID) consulted by the implementation; it may be nil for
	// callers that do not own a Manager (cmd/cleanup.go's
	// removeContainerIfExists path), in which case implementations fall
	// back to the per-session temp-path helpers in container.go.
	// Cites: internal/container/lifecycle.go:24 (Manager.EnsureRemoved);
	//        cmd/cleanup.go:1026 (removeContainerIfExists).
	EnsureRemoved(ctx context.Context, m *Manager)

	// WriteGitconfig generates a minimal .gitconfig for this isolator's
	// sandbox layout and writes it to the per-session temp path. The
	// in-sandbox $HOME differs per mode — podman runs as root (/root),
	// bwrap/sandbox-exec run as the host user — so each isolator picks
	// the correct $HOME prefix when emitting the signingKey and
	// allowedSignersFile paths. Returns nil on success or a wrapped error.
	// host: no-op (the agent reads the host gitconfig directly).
	// Cites: internal/container/container.go:451 (writeGitconfig with mode);
	//        internal/container/lifecycle.go:121 (Create caller);
	//        internal/container/container.go:594 (PrepareBwrap caller).
	WriteGitconfig(m *Manager) error

	// Reset performs the heavier "wipe everything matching this mode"
	// cleanup invoked by `prism reset`. For podman this is the existing
	// `podman ps` / `podman rm -f prism-*` sweep. For bwrap, sandbox-exec,
	// and host the implementation is a no-op stub today; orphan-agent-run
	// reaping is a future implementation. `prism reset` iterates over the
	// registered isolators and calls Reset on each.
	// Cites: cmd/reset.go:126 (resetRemovePodmanContainers).
	Reset(ctx context.Context) error

	// Prepare materialises any per-session temp files (SSH config,
	// gitconfig, harness config, sandbox staging HOME, SBPL profile) that
	// the sandbox needs at start time and returns the complete argument
	// list for the sandbox launcher. Bwrap returns the bwrap argv;
	// sandbox-exec returns the sandbox-exec argv. Podman/host return nil
	// args and a non-nil error — they do not use this dispatch path
	// (podman uses Create; host runs the agent directly in the tmux pane).
	// Cites: internal/container/container.go:581 (Manager.PrepareBwrap);
	//        internal/container/container.go:637 (Manager.PrepareSandboxExec).
	Prepare(ctx context.Context, m *Manager) ([]string, error)

	// Create starts a new isolated session: writes any pre-launch temp
	// files, builds the launcher arg list, and runs it under the supplied
	// context. Today only podman implements a non-stub body — bwrap and
	// sandbox-exec use Prepare + cmd/agent-run, and host runs the agent
	// directly in the tmux pane. Non-podman implementations return nil
	// (host) or an error noting that Create is not the right entry point
	// for that mode (bwrap, sandbox-exec).
	// Cites: internal/container/lifecycle.go:79 (Manager.Create body).
	Create(ctx context.Context, m *Manager) error

	// AgentRun executes the agent binary inside this isolation mode.
	// Bwrap and sandbox-exec own a real implementation: they reconstruct
	// the container.Config from the supplied AgentRunOpts, materialise
	// temp files, and exec/spawn the sandbox launcher with PTY and signal
	// forwarding. Host returns an error because `prism agent-run`
	// is not the entry point for that mode (host runs the agent directly
	// in the tmux pane).
	// Cites: cmd/agent_run.go:90 (runAgentRun dispatch);
	//        cmd/agent_run.go:128-143 (per-mode switch).
	AgentRun(ctx context.Context, opts AgentRunOpts) error
}
