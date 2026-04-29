// Package container manages the podman container lifecycle for prism sidecar.
// This file defines the Isolator interface, which is the seam between the
// Manager and the underlying isolation mechanism. The only implementation
// currently is podmanIsolator, which wraps rootless podman.
package container

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

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
	// IsContainer replaces (isoMode == IsolationPodman) tests outside the
	// package. True only for podmanIsolator.
	// Cites: cmd/spawn.go:275, cmd/switch.go:1069, cmd/pr.go:77.
	IsContainer bool

	// OwnsContainerLifecycle means the sidecar creates, stops, and removes
	// the container. True only for podmanIsolator.
	// Cites: internal/sidecar/sidecar.go:153-183 (s.cfg.Container == nil branch).
	OwnsContainerLifecycle bool

	// NeedsConfigBlob means the harness config blob must be supplied via
	// env-var or on-disk file before the process starts. True for podman,
	// bwrap, and sandbox-exec; false for host.
	// Cites: cmd/spawn.go:311-357, cmd/switch.go:1069-1108,
	//        cmd/restore.go:269-292, cmd/pr.go:77-170,
	//        internal/review/review.go:1230.
	NeedsConfigBlob bool

	// NeedsHostAPISocket means the sidecar binds the host-API Unix socket for
	// this mode. True for bwrap and sandbox-exec; false for podman and host.
	// Cites: internal/sidecar/sidecar.go:512-547; cmd/sidecar.go:233-249.
	NeedsHostAPISocket bool

	// UsesContainerHarness means the harness adapter is constructed in
	// container mode (podman) vs host mode (bwrap, sandbox-exec, host).
	// Cites: cmd/sidecar.go:288-301.
	UsesContainerHarness bool

	// RestartOnExit means the sidecar restart-loop goroutine is started for
	// this mode. True for podman and bwrap; false for sandbox-exec and host.
	// Cites: cmd/sidecar.go:348.
	RestartOnExit bool

	// NeedsStartupConnectTimeout means the sidecar's startup-connect timeout
	// fires for this mode. True for bwrap; false for all others.
	// Cites: internal/sidecar/sidecar.go:638-680.
	NeedsStartupConnectTimeout bool

	// NeedsReadinessWait means the agent-pane command should be prefixed by
	// the readiness-wait shell command. True for podman; false for others.
	// Cites: internal/session/session.go:539-547.
	NeedsReadinessWait bool

	// EmitsTmuxStatusColumns means the tmux event hooks should seed
	// isolation-specific status columns for sessions in this mode. True for
	// podman; false for bwrap, sandbox-exec, and host.
	// Cites: cmd/event.go (per §6.22 flat appendix).
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
// exclusively — no direct exec.CommandContext("podman", ...) calls remain in
// container.go after this interface is in place.
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
	// The existing per-mode helpers in cmd/concurrency.go remain the source
	// of truth for message rendering until A.3 (#1132) unifies them; today
	// every implementation returns a zero-value CapStatus. The method exists
	// so callers can route through registry.For(mode).Cap(...) without
	// branching on the literal mode value.
	// Cites: cmd/concurrency.go:35-57 (podman), :72-140 (sandbox-exec),
	//        :155-223 (bwrap).
	Cap(in CapInputs) CapStatus

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
}

// podmanIsolator implements Isolator using rootless podman.
type podmanIsolator struct {
	// name is the container name (stable, derived from the session name).
	name string
}

// newPodmanIsolator returns an Isolator that manages a rootless podman
// container with the given stable container name.
func newPodmanIsolator(name string) Isolator {
	return &podmanIsolator{name: name}
}

// Name returns config.IsolationPodman — the registry key for this isolator.
func (p *podmanIsolator) Name() config.IsolationMode {
	return config.IsolationPodman
}

// Capabilities returns the podman feature flags:
//   - IsContainer + OwnsContainerLifecycle: the sidecar manages a real container.
//   - NeedsConfigBlob: config blob is injected as an env var into the container.
//   - UsesContainerHarness: the harness adapter is built in container mode.
//   - RestartOnExit: the sidecar restart-loop fires to keep the container alive.
//   - NeedsReadinessWait: the agent pane waits for the container HTTP endpoint.
//   - EmitsTmuxStatusColumns: podman sessions seed the tmux status columns.
func (p *podmanIsolator) Capabilities() Capabilities {
	return Capabilities{
		IsContainer:                true,
		OwnsContainerLifecycle:     true,
		NeedsConfigBlob:            true,
		NeedsHostAPISocket:         false,
		UsesContainerHarness:       true,
		RestartOnExit:              true,
		NeedsStartupConnectTimeout: false,
		NeedsReadinessWait:         true,
		EmitsTmuxStatusColumns:     true,
	}
}

// BuildRunArgs returns the argument list for "podman run …" built by the
// Manager. The Manager calls this and passes the result to Run.
//
// Note: the argument construction logic lives in Manager.buildRunArgs, not
// here, because it depends on Manager state (cfg, allowedSignersReady,
// claudeCredentialsReady, temp file paths, etc.). BuildRunArgs is therefore
// a thin forwarder — it is not called by Manager directly; instead Manager
// calls its own buildRunArgs and hands the result to Run.
//
// This method satisfies the interface contract but is only called when an
// Isolator is used standalone (e.g. in tests that exercise the interface
// directly). In the normal Manager flow, Manager.buildRunArgs() is called
// and the result is passed to Run.
func (p *podmanIsolator) BuildRunArgs() []string {
	// This method is intentionally minimal: the real arg-building logic lives
	// in Manager.buildRunArgs() which has access to all the Manager state
	// (cfg fields, temp file paths, allowedSignersReady flag, etc.). The
	// interface requires this method so that future isolator implementations
	// can build their own arg lists independently.
	return nil
}

// Run executes "podman <args...>" and waits for it to complete. Stdout and
// stderr are forwarded to the sidecar's stderr log. Returns a wrapped error
// on failure, preserving the same message shape as the pre-refactor inline
// code: "container: podman run %q: %w".
func (p *podmanIsolator) Run(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "podman", args...)
	cmd.Stdout = os.Stderr // forward container stdout to sidecar's stderr log
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("container: podman run %q: %w", p.name, err)
	}
	return nil
}

// Shutdown stops and removes the podman container. It uses a background
// context with a 30-second timeout so cleanup proceeds even after the parent
// context has been cancelled.
func (p *podmanIsolator) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stopCmd := exec.CommandContext(ctx, "podman", "stop", "--time", "10", p.name)
	if out, err := stopCmd.CombinedOutput(); err != nil && !IsNoSuchContainerError(string(out)) {
		log.Printf("container: stop %q: %v — %s", p.name, err, strings.TrimSpace(string(out)))
	}

	rmCmd := exec.CommandContext(ctx, "podman", "rm", "--force", p.name)
	if out, err := rmCmd.CombinedOutput(); err != nil && !IsNoSuchContainerError(string(out)) {
		log.Printf("container: rm %q: %v — %s", p.name, err, strings.TrimSpace(string(out)))
	}
}

// HasExited checks whether the podman container has stopped. Returns
// (true, exitCode) when the container is in "exited" state, or (false, 0)
// when it is still running or when the inspect call fails.
func (p *podmanIsolator) HasExited() (bool, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "inspect",
		"--format", "{{.State.Status}} {{.State.ExitCode}}",
		p.name,
	).Output()
	if err != nil {
		return false, 0
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 2 && fields[0] == "exited" {
		code := 0
		fmt.Sscanf(fields[1], "%d", &code)
		return true, code
	}
	return false, 0
}

// DumpLogs fetches and logs the container's stdout/stderr via "podman logs".
// It is called on startup failure so that error output is visible without
// racing the container removal in Shutdown.
func (p *podmanIsolator) DumpLogs() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "logs", p.name).CombinedOutput()
	if err != nil {
		log.Printf("container: could not fetch logs for %q: %v", p.name, err)
		return
	}
	log.Printf("container: logs for %q:\n%s", p.name, string(out))
}

func init() {
	MustRegister(config.IsolationPodman, func(opts ConstructorOpts) Isolator {
		return newPodmanIsolator(opts.Name)
	})
}
