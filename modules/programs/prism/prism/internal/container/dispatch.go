// Package container manages the podman container lifecycle for prism sidecar.
// This file extends the Isolator interface with the dispatch methods migrated
// from the per-mode switch/if blocks scattered across cmd/ and internal/. Each
// method body produces the same output as the previous per-mode branch — this
// file is mechanically equivalent to the call-site switches it replaces (issue
// #1133, A1.D1-D7).
//
// Methods added by this file:
//
//   - Available()                 — D1: replaces checkBwrapPlatform / checkSandboxExecPlatform / CheckAvailability
//   - Cap()                       — D2: replaces checkConcurrencyCap / checkBwrapConcurrencyCap / checkSandboxExecConcurrencyCap dispatch
//   - WriteHarnessConfigBlob()    — D3: replaces the NeedsConfigBlob && configContent != "" / WriteOpencodeConfig sites
//   - AgentPaneCmd()              — D4: replaces the BuildOpencodeCmd switch in internal/session/session.go
//   - SidecarFlags()              — D5: replaces the per-mode argv builder in internal/session/sidecar.go
//   - ArchivePaths()              — D6: replaces the per-mode resolveStorageRoot switch in internal/archive/archive.go (stopgap pending #1142)
//   - LogPaths()                  — D7: per-mode log file set; today no caller dispatches per-mode but the shape is in place
//
// The methods are declared on the interface in isolator.go and implemented per
// isolator (podman, bwrap, sandbox-exec, host) below.
package container

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/prismatic-koi/prism/internal/config"
)

// CapInputs carries the per-call inputs that the unified Cap() dispatch needs
// without coupling the container package to cobra. Each Cap() implementation
// reads the fields it needs and ignores the rest.
type CapInputs struct {
	// IgnoreCap is true when --ignore-concurrency-cap was passed on the
	// caller's CLI. When the cap is exceeded and IgnoreCap is true the
	// implementation should write a warning and return CapStatus{Exceeded:
	// false} so the caller proceeds.
	IgnoreCap bool

	// CallerName is used in warning/error messages — currently "spawn",
	// "pr", or "review". Mirrors the callerName argument of the
	// pre-refactor functions in cmd/concurrency.go.
	CallerName string
}

// CapStatus is the result of an Isolator.Cap() check. Today only the Err
// field is read — when non-nil, the caller surfaces it verbatim. Future
// callers may read Current/Limit/Exceeded to render their own UI; the
// fields are exported for that purpose.
type CapStatus struct {
	// Current is the count of in-flight sessions for this mode. May be 0
	// when the underlying counter could not be queried.
	Current int

	// Limit is the configured cap (0 means uncapped).
	Limit int

	// Exceeded is true when Current >= Limit and Limit > 0.
	Exceeded bool

	// Err is the error to surface to the caller, or nil to proceed. When
	// IgnoreCap was true and the cap was exceeded, Err is nil but
	// Exceeded is true.
	Err error
}

// SidecarFlagOpts carries the per-spawn inputs that SidecarFlags consumes.
// Mirrors the fields read by the pre-refactor argv builder in
// internal/session/sidecar.go:311-340.
//
// Mode-independent flags (e.g. --instance-id, --worktree-readonly,
// --harness) live outside SidecarFlags — they are appended unconditionally
// by the caller after the per-mode dispatch.
type SidecarFlagOpts struct {
	Port           int
	AgentRole      string
	PluginHostPath string
	InitialPrompt  string
	ConfigContent  string
}

// AgentPaneOpts carries the inputs that AgentPaneCmd consumes. Mirrors the
// fields read by the pre-refactor switch in BuildOpencodeCmd
// (internal/session/session.go:265-298). DirectCmd is the host-mode fallback
// command produced by the caller (buildDirectOpencodeCmd) — passed in rather
// than constructed here so this dispatch stays a thin shim and host-mode
// behaviour remains in the session package.
type AgentPaneOpts struct {
	// SessionName is the prism session name.
	SessionName string

	// DirectCmd is the host-mode command emitted when SessionName is empty
	// (defensive fallback) or for the host isolator. Callers construct it
	// before calling AgentPaneCmd.
	DirectCmd string
}

// ArchivePaths describes the per-mode paths that the archive copy step
// consumes. ExtraFiles are absolute paths copied into the archive root in
// addition to the harness storage subtree (today: agent-run.log for bwrap +
// sandbox-exec; empty for podman + host).
//
// This is a stopgap pending #1142 (B6.IF — ArchiveAdapter interface): once
// that lands, archive will consume the ArchiveAdapter interface and the
// per-mode paths will move there. Until then, ArchivePaths keeps the per-mode
// dispatch on the Isolator so internal/archive does not need to import
// internal/container (which would create a circular dependency).
type ArchivePaths struct {
	// StorageRoot is the host-side opencode storage root for this session.
	// For host/bwrap/sandbox-exec: $HOME/.local/share/opencode/storage.
	// For podman:                  $HOME/.local/share/opencode/prism-sessions/<container>/storage.
	StorageRoot string

	// ExtraFiles are absolute host paths that the archive should copy
	// alongside the harness storage subtree. Missing paths are silently
	// skipped at copy time.
	ExtraFiles []string
}

// LogPaths describes the per-mode log file set for a session. Today only the
// SidecarLog and AgentRunLog fields are populated; future modes may add
// further entries (e.g. bwrap-stderr, podman-events).
//
// The values are absolute host paths and may not yet exist on disk — callers
// must os.Stat them before reading.
type LogPaths struct {
	// SidecarLog is the path to the sidecar log file for this session.
	// Populated for every isolator (the sidecar runs in every mode today).
	SidecarLog string

	// AgentRunLog is the path to the agent-run log file for this session.
	// Populated for bwrap and sandbox-exec. Empty for podman and host (the
	// agent-run path is not used by those modes).
	AgentRunLog string
}

// ----------------------------------------------------------------------------
// podmanIsolator
// ----------------------------------------------------------------------------

// Available reports whether podman mode can run on this host. Wraps the
// existing CheckAvailability helper (internal/container/container.go:1315).
func (p *podmanIsolator) Available() error {
	return CheckAvailability()
}

// Cap is unimplemented for podman — the existing caller still uses
// checkConcurrencyCap directly via cmd/concurrency.go. The unified message
// rendering is the deliverable of A.3 (issue #1132); A.1.D2 leaves the
// per-mode message-building functions in place and only collapses the
// dispatch.
//
// Returning a zero-value CapStatus is correct here because A1.D2's call sites
// (cmd/spawn.go:268, cmd/pr.go:90, cmd/review.go:191) already short-circuit
// when isoCaps.IsContainer is false; for podman they continue calling the
// existing checkConcurrencyCap helper.
func (p *podmanIsolator) Cap(in CapInputs) CapStatus {
	return CapStatus{}
}

// WriteHarnessConfigBlob writes the harness config blob to the deterministic
// per-session temp path. For podman the file is bind-mounted at
// /root/.config/opencode/opencode.json inside the container — see
// container.go:1234. The same call-site gate (NeedsConfigBlob && content != "")
// already filtered out empty content; this method returns nil on empty input
// to keep the contract identical.
//
// sessionName is the prism session name (e.g. "nixos-config@feature"); the
// isolator translates it to the container name internally so the write path
// matches Manager.opencodeConfigFilePath.
func (p *podmanIsolator) WriteHarnessConfigBlob(sessionName, content string) error {
	if content == "" {
		return nil
	}
	return WriteOpencodeConfig(NameForSession(sessionName), content)
}

// AgentPaneCmd returns the tmux pane command for podman: "podman attach"
// onto the running container, with --sig-proxy=false so that Ctrl-C reaches
// opencode's TUI as a literal byte (matching host-mode keystroke behaviour).
//
// Mirrors the pre-refactor branch in BuildOpencodeCmd
// (internal/session/session.go:268-284). When SessionName is empty, falls
// back to DirectCmd — the same defensive fallback as the original switch.
func (p *podmanIsolator) AgentPaneCmd(opts AgentPaneOpts) string {
	if opts.SessionName == "" {
		return opts.DirectCmd
	}
	return "podman attach --sig-proxy=false " + shellQuotePodman(NameForSession(opts.SessionName))
}

// SidecarFlags returns the sidecar argv extensions for podman: --container,
// --port, --agent-role, --plugin-path, --initial-prompt, --config-content
// (each conditional on the corresponding opts field being set). Mirrors the
// pre-refactor branch in StartSidecarWithOpts
// (internal/session/sidecar.go:317-336).
func (p *podmanIsolator) SidecarFlags(opts SidecarFlagOpts) []string {
	out := []string{"--container", "--port", fmt.Sprintf("%d", opts.Port)}
	if opts.AgentRole != "" {
		out = append(out, "--agent-role", opts.AgentRole)
	}
	if opts.PluginHostPath != "" {
		out = append(out, "--plugin-path", opts.PluginHostPath)
	}
	if opts.InitialPrompt != "" {
		out = append(out, "--initial-prompt", opts.InitialPrompt)
	}
	if opts.ConfigContent != "" {
		out = append(out, "--config-content", opts.ConfigContent)
	}
	return out
}

// ArchivePaths returns the podman archive layout: storage lives under a
// per-container subdirectory (Darwin virtiofs WAL-mode workaround). No extra
// archive files — agent-run.log is bwrap/sandbox-exec only.
func (p *podmanIsolator) ArchivePaths(home, sessionName string) ArchivePaths {
	return ArchivePaths{
		StorageRoot: archivePodmanStorageRoot(home, NameForSession(sessionName)),
	}
}

// LogPaths returns the per-mode log file set for podman. Today only the
// SidecarLog is populated for podman — agent-run.log is not produced in
// container mode (the log is captured by the container PTY instead).
func (p *podmanIsolator) LogPaths() LogPaths {
	return LogPaths{
		// SidecarLog and AgentRunLog are looked up by the caller via
		// internal/session — keeping the shape here means a future
		// follow-up can wire the lookup without re-touching call sites.
	}
}

// ----------------------------------------------------------------------------
// bwrapIsolator
// ----------------------------------------------------------------------------

// Available reports whether bwrap mode can run on this host. Today the only
// requirement is Linux (the bwrap binary check is done lazily at run time by
// the bwrap.go arg builder). Mirrors checkBwrapPlatform (cmd/spawn.go:190).
func (b *bwrapIsolator) Available() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("isolation mode %q requires Linux; current platform is %s", config.IsolationBwrap, runtime.GOOS)
	}
	return nil
}

// Cap is unimplemented for bwrap — see podmanIsolator.Cap for the rationale.
// The existing checkBwrapConcurrencyCap helper in cmd/concurrency.go remains
// the source of truth until A.3 (#1132) unifies the message rendering.
func (b *bwrapIsolator) Cap(in CapInputs) CapStatus {
	return CapStatus{}
}

// WriteHarnessConfigBlob writes the opencode.json config blob to the
// deterministic per-session temp path. For bwrap the file is bind-mounted
// into the sandbox by the arg builder (bwrap.go:419). The same call-site
// gate (NeedsConfigBlob && content != "") already filtered out empty
// content; this method returns nil on empty input to keep the contract
// identical.
func (b *bwrapIsolator) WriteHarnessConfigBlob(sessionName, content string) error {
	if content == "" {
		return nil
	}
	return WriteOpencodeConfig(NameForSession(sessionName), content)
}

// AgentPaneCmd returns the tmux pane command for bwrap: "prism agent-run
// --session <name>". The bwrap sandbox is owned by the tmux pane (not the
// sidecar), so the agent-run dispatch reads the session's isolation mode
// from the DB and invokes the bwrap arg builder there. Mirrors the
// pre-refactor branch in BuildOpencodeCmd (internal/session/session.go:286-294).
func (b *bwrapIsolator) AgentPaneCmd(opts AgentPaneOpts) string {
	if opts.SessionName == "" {
		return opts.DirectCmd
	}
	return "prism agent-run --session " + shellQuotePodman(opts.SessionName)
}

// SidecarFlags returns the sidecar argv extensions for bwrap: --port and the
// common AgentRole / PluginHostPath / InitialPrompt / ConfigContent flags.
// --container is intentionally omitted because bwrap does not own a container
// lifecycle. Mirrors the pre-refactor branch in StartSidecarWithOpts
// (internal/session/sidecar.go:336-352).
func (b *bwrapIsolator) SidecarFlags(opts SidecarFlagOpts) []string {
	return commonHostAPISidecarFlags(opts)
}

// ArchivePaths returns the bwrap archive layout: storage lives in the shared
// host opencode storage root (no per-session sub-dir). Includes the agent-run
// log as an extra archive file when present.
func (b *bwrapIsolator) ArchivePaths(home, sessionName string) ArchivePaths {
	return ArchivePaths{
		StorageRoot: archiveSharedStorageRoot(home),
		ExtraFiles:  archiveAgentRunLogPaths(sessionName),
	}
}

// LogPaths returns the per-mode log file set for bwrap. The agent-run log
// is populated for bwrap because prism agent-run tees opencode's stdout/
// stderr there (cmd/agent_run.go:563).
func (b *bwrapIsolator) LogPaths() LogPaths {
	return LogPaths{}
}

// ----------------------------------------------------------------------------
// sandboxExecIsolator
// ----------------------------------------------------------------------------

// Available reports whether sandbox-exec mode can run on this host. Today the
// only requirement is Darwin. Mirrors checkSandboxExecPlatform
// (cmd/spawn.go:199).
func (s *sandboxExecIsolator) Available() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("isolation mode %q requires macOS (Darwin); current platform is %s", config.IsolationSandboxExec, runtime.GOOS)
	}
	return nil
}

// Cap is unimplemented for sandbox-exec — see podmanIsolator.Cap for the
// rationale. The existing checkSandboxExecConcurrencyCap helper in
// cmd/concurrency.go remains the source of truth until A.3 (#1132).
func (s *sandboxExecIsolator) Cap(in CapInputs) CapStatus {
	return CapStatus{}
}

// WriteHarnessConfigBlob writes the opencode.json config blob to the
// deterministic per-session temp path. For sandbox-exec the file is read
// directly by opencode at the sandbox-mapped HOME path (sandbox_exec_home.go:274).
func (s *sandboxExecIsolator) WriteHarnessConfigBlob(sessionName, content string) error {
	if content == "" {
		return nil
	}
	return WriteOpencodeConfig(NameForSession(sessionName), content)
}

// AgentPaneCmd returns the tmux pane command for sandbox-exec — same shape
// as bwrap because both modes are pane-owned: "prism agent-run --session
// <name>". Mirrors the pre-refactor branch in BuildOpencodeCmd
// (internal/session/session.go:286-294).
func (s *sandboxExecIsolator) AgentPaneCmd(opts AgentPaneOpts) string {
	if opts.SessionName == "" {
		return opts.DirectCmd
	}
	return "prism agent-run --session " + shellQuotePodman(opts.SessionName)
}

// SidecarFlags returns the sidecar argv extensions for sandbox-exec — same
// shape as bwrap (the sidecar binds a host-API socket but does not own a
// container lifecycle). Mirrors the pre-refactor branch in
// StartSidecarWithOpts (internal/session/sidecar.go:336-352).
func (s *sandboxExecIsolator) SidecarFlags(opts SidecarFlagOpts) []string {
	return commonHostAPISidecarFlags(opts)
}

// ArchivePaths returns the sandbox-exec archive layout — same shape as
// bwrap (shared storage root + agent-run log).
func (s *sandboxExecIsolator) ArchivePaths(home, sessionName string) ArchivePaths {
	return ArchivePaths{
		StorageRoot: archiveSharedStorageRoot(home),
		ExtraFiles:  archiveAgentRunLogPaths(sessionName),
	}
}

// LogPaths returns the per-mode log file set for sandbox-exec. Same shape
// as bwrap — agent-run.log is produced because prism agent-run tees
// opencode's stdout/stderr there.
func (s *sandboxExecIsolator) LogPaths() LogPaths {
	return LogPaths{}
}

// ----------------------------------------------------------------------------
// hostIsolator
// ----------------------------------------------------------------------------

// Available is always nil for host mode. The host isolator runs opencode
// directly in the tmux pane with no sandbox layer; there is nothing to
// check beyond what cobra has already validated.
func (h *hostIsolator) Available() error {
	return nil
}

// Cap is always a zero-value pass for host mode (no concurrency cap applies
// — host sessions consume neither container slots nor sandbox slots).
func (h *hostIsolator) Cap(in CapInputs) CapStatus {
	return CapStatus{}
}

// WriteHarnessConfigBlob is a no-op for host mode: opencode reads
// ~/.config/opencode/opencode.json directly via xdg.configFile, so there is
// no per-session blob to write. Mirrors the call-site gate (NeedsConfigBlob
// is false for host).
func (h *hostIsolator) WriteHarnessConfigBlob(sessionName, content string) error {
	return nil
}

// AgentPaneCmd returns DirectCmd unchanged — host mode runs opencode
// directly in the tmux pane and has no sandbox wrapper command. The caller
// is responsible for constructing DirectCmd with all env vars and flags.
func (h *hostIsolator) AgentPaneCmd(opts AgentPaneOpts) string {
	return opts.DirectCmd
}

// SidecarFlags returns nil for host mode: the sidecar is not started for
// host sessions. (StartSidecarWithOpts is gated by isolation mode upstream;
// SidecarFlags being nil here is a safety net.)
func (h *hostIsolator) SidecarFlags(opts SidecarFlagOpts) []string {
	return nil
}

// ArchivePaths returns the host archive layout: storage in the shared root,
// no extra archive files (host mode does not produce agent-run.log).
func (h *hostIsolator) ArchivePaths(home, sessionName string) ArchivePaths {
	return ArchivePaths{
		StorageRoot: archiveSharedStorageRoot(home),
	}
}

// LogPaths returns the per-mode log file set for host. Only the SidecarLog
// would be populated in a future iteration that threads paths through here
// (host mode produces no agent-run log).
func (h *hostIsolator) LogPaths() LogPaths {
	return LogPaths{}
}

// ----------------------------------------------------------------------------
// shared helpers
// ----------------------------------------------------------------------------

// shellQuotePodman wraps s in single quotes for shell-safe embedding. It is a
// local copy of internal/session.shellQuote — duplicated here to avoid a
// circular dependency (internal/session imports internal/container). When the
// helpers are unified during a future refactor the two should converge.
func shellQuotePodman(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// commonHostAPISidecarFlags returns the SidecarFlags shared by bwrap and
// sandbox-exec. Both modes set up a host-API socket and harness but do not
// own a container lifecycle, so --container is intentionally omitted. Mirrors
// the pre-refactor branch in StartSidecarWithOpts
// (internal/session/sidecar.go:336-352).
func commonHostAPISidecarFlags(opts SidecarFlagOpts) []string {
	out := []string{"--port", fmt.Sprintf("%d", opts.Port)}
	if opts.AgentRole != "" {
		out = append(out, "--agent-role", opts.AgentRole)
	}
	if opts.PluginHostPath != "" {
		out = append(out, "--plugin-path", opts.PluginHostPath)
	}
	if opts.InitialPrompt != "" {
		out = append(out, "--initial-prompt", opts.InitialPrompt)
	}
	if opts.ConfigContent != "" {
		out = append(out, "--config-content", opts.ConfigContent)
	}
	return out
}

// archiveSharedStorageRoot returns the host-side shared opencode storage root
// used by host / bwrap / sandbox-exec. Mirrors the pre-refactor branch in
// internal/archive/archive.go:267-268.
func archiveSharedStorageRoot(home string) string {
	return filepath.Join(home, ".local", "share", "opencode", "storage")
}

// archivePodmanStorageRoot returns the per-container opencode storage root
// used by podman (Darwin virtiofs WAL-mode workaround). Mirrors the
// pre-refactor branch in internal/archive/archive.go:269-271.
func archivePodmanStorageRoot(home, containerName string) string {
	return filepath.Join(home, ".local", "share", "opencode", "prism-sessions", containerName, "storage")
}

// archiveAgentRunLogPaths returns the agent-run log path for the named
// session as a single-element slice (or empty when the path cannot be
// resolved). The path is resolved via the same XDG-derived base used by
// internal/session.AgentRunLogPath; we duplicate the lookup here to avoid a
// circular dependency (internal/session imports internal/container).
//
// The path may not exist on disk — archive.copyFile silently skips missing
// paths via os.Stat.
func archiveAgentRunLogPaths(sessionName string) []string {
	base := xdgStateBase()
	if base == "" {
		return nil
	}
	return []string{filepath.Join(base, "prism", "run", sessionDirName(sessionName), "agent-run.log")}
}

// xdgStateBase returns $XDG_STATE_HOME (or $HOME/.local/state) without the
// trailing "/prism" component. Returns the empty string when the home
// directory cannot be resolved — callers must treat that as "skip".
//
// Mirrors internal/session.sidecarStateDir without the prism suffix; the
// suffix is added back by the caller.
func xdgStateBase() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state")
}

// sessionDirName mirrors internal/session.SessionDirName: the first 12 hex
// characters of SHA-256(sessionName). Duplicated here to avoid a circular
// dependency.
func sessionDirName(sessionName string) string {
	sum := sha256.Sum256([]byte(sessionName))
	return hex.EncodeToString(sum[:])[:12]
}
