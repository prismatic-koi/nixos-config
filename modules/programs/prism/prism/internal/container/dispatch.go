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
//   - WriteHarnessConfigBlob()    — D3: replaces the NeedsConfigBlob && configContent != "" / WriteHarnessConfig sites
//   - AgentPaneCmd()              — D4: replaces the BuildOpencodeCmd switch in internal/session/session.go
//   - SidecarFlags()              — D5: replaces the per-mode argv builder in internal/session/sidecar.go
//   - ArchivePaths()              — D6: replaces the per-mode resolveStorageRoot switch in internal/archive/archive.go (stopgap pending #1142)
//   - LogPaths()                  — D7: per-mode log file set; today no caller dispatches per-mode but the shape is in place
//
// The methods are declared on the interface in isolator.go and implemented per
// isolator (podman, bwrap, sandbox-exec, host) below.
package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
)

// CapStatus is the outcome of an Isolator.Cap probe. It generalises
// container.CheckResult over all isolation modes.
//
// Replaces the previous CapInputs/CapStatus pair that embedded IgnoreCap and
// CallerName in the inputs — those concerns are now handled by Check().
type CapStatus struct {
	// Mode is the isolation mode that produced this status. Used by
	// RenderError / RenderWarning for the noun in messages.
	Mode config.IsolationMode

	// Limit is the configured cap. Zero means uncapped. For podman:
	// DefaultConcurrencyCap. For bwrap: Config.BwrapConcurrencyCap.
	// For sandbox-exec: Config.SandboxExecConcurrencyCap. For host: 0.
	Limit int

	// Count is the number of in-flight sessions of this mode at probe time.
	Count int

	// Exceeded is true when Count >= Limit and Limit > 0. False when
	// Limit == 0 regardless of Count.
	Exceeded bool

	// InFlight is the per-session detail list rendered into the warning/error
	// message. May be empty if the probe failed and the implementation chose
	// not to enumerate; in that case Note carries the explanatory message.
	InFlight []InFlightSession

	// Note carries any non-fatal context from the probe — e.g. a DB error
	// description when the session list could not be fetched. Empty when the
	// probe ran cleanly.
	Note string
}

// modeNoun returns the user-facing noun for the isolation mode, used in
// cap warning/error messages.
func modeNoun(mode config.IsolationMode) string {
	switch mode {
	case config.IsolationBwrap:
		return "bwrap sessions"
	case config.IsolationSandboxExec:
		return "sandbox-exec sessions"
	default:
		return string(mode) + " sessions"
	}
}

// RenderError returns the error string shown when Exceeded is true and
// --ignore-concurrency-cap was NOT passed.
//
// Replaces container.FormatExceededError (internal/container/concurrency.go)
// and the inline strings.Builder block at cmd/concurrency.go for bwrap/sandbox-exec.
func (s CapStatus) RenderError() string {
	noun := modeNoun(s.Mode)
	var sb strings.Builder
	fmt.Fprintf(&sb, "error: prism concurrency cap reached (%d %s already in flight)\n", s.Count, noun)
	if len(s.InFlight) > 0 {
		sb.WriteString("\nActive ")
		sb.WriteString(noun)
		sb.WriteString(":\n")
		for _, sess := range s.InFlight {
			fmt.Fprintf(&sb, "  %-40s (%s)\n", sess.Name, sess.Role)
		}
	}
	sb.WriteString("\nHint: wait for a worker to finish and be cleaned up, or re-run with\n")
	sb.WriteString("      --ignore-concurrency-cap to bypass this guard.")
	return sb.String()
}

// RenderWarning returns the warning string shown when Exceeded is true and
// --ignore-concurrency-cap WAS passed.
//
// Replaces container.FormatExceededWarning and the inline strings.Builder
// blocks in cmd/concurrency.go for bwrap/sandbox-exec.
func (s CapStatus) RenderWarning() string {
	noun := modeNoun(s.Mode)
	var sb strings.Builder
	fmt.Fprintf(&sb, "[prism] warning: concurrency cap exceeded (%d/%d %s in flight) — proceeding because --ignore-concurrency-cap was passed\n", s.Count, s.Limit, noun)
	if len(s.InFlight) > 0 {
		sb.WriteString("[prism] in-flight ")
		sb.WriteString(noun)
		sb.WriteString(":\n")
		for _, sess := range s.InFlight {
			fmt.Fprintf(&sb, "[prism]   %-40s (%s)\n", sess.Name, sess.Role)
		}
	}
	return sb.String()
}

// Check applies the --ignore-concurrency-cap policy to the CapStatus.
//
// If Note is non-empty, it is written to stderr as a warning first.
// If Exceeded is false (or Limit == 0), nil is returned.
// If Exceeded is true and ignoreCap is false, returns a non-nil error with
// the RenderError message.
// If Exceeded is true and ignoreCap is true, writes the RenderWarning message
// to stderr and returns nil.
//
// This is the unified entry point that replaces the per-mode cap-check
// helpers in cmd/concurrency.go. Call sites become:
//
//	if err := iso.Cap(ctx, dbPath()).Check(ignoreCap); err != nil { return err }
func (s CapStatus) Check(ignoreCap bool) error {
	if s.Note != "" {
		fmt.Fprintf(os.Stderr, "[prism] warning: %s\n", s.Note)
	}
	if !s.Exceeded {
		return nil
	}
	if ignoreCap {
		fmt.Fprint(os.Stderr, s.RenderWarning())
		return nil
	}
	return fmt.Errorf("%s", s.RenderError())
}

// dbSessionsForMode opens the DB at dbPath, counts active sessions for the
// given mode, fetches the session list, and returns a slice of InFlightSession.
// Non-fatal: on DB error returns (nil, nil, warning string).
func dbSessionsForMode(dbPath string, mode config.IsolationMode) (count int, sessions []InFlightSession, note string) {
	d, err := db.Open(dbPath)
	if err != nil {
		return 0, nil, fmt.Sprintf("could not open DB for %s cap check: %v", mode, err)
	}
	defer d.Close()

	n, err := d.ActiveSessionCountForMode(string(mode))
	if err != nil {
		return 0, nil, fmt.Sprintf("could not count active %s sessions: %v", mode, err)
	}

	rows, listErr := d.ActiveSessionsForMode(string(mode))
	if listErr != nil {
		// Count succeeded but listing failed — return count with empty list.
		return n, nil, ""
	}

	var inFlight []InFlightSession
	for _, s := range rows {
		inFlight = append(inFlight, InFlightSession{
			Name: s.SessionName,
			Role: roleFor(s.SessionName, s.RootAgentName),
		})
	}
	return n, inFlight, ""
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
	// StorageRoot is the host-side agent storage root for this session.
	// For host/bwrap/sandbox-exec: $HOME/.local/share/pi/storage.
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

// Cap probes the bwrap concurrency cap from the DB only. Reads the cap value
// from config.Load().BwrapConcurrencyCap; returns CapStatus{Limit: 0} when
// the cap is configured to 0 (uncapped sentinel).
//
// Calls db.ActiveSessionCountForMode("bwrap") and
// db.ActiveSessionsForMode("bwrap").
func (b *bwrapIsolator) Cap(ctx context.Context, dbPath string) CapStatus {
	limit := config.Load().BwrapConcurrencyCap
	if limit == 0 {
		return CapStatus{Mode: config.IsolationBwrap, Limit: 0}
	}
	count, inFlight, note := dbSessionsForMode(dbPath, config.IsolationBwrap)
	return CapStatus{
		Mode:     config.IsolationBwrap,
		Limit:    limit,
		Count:    count,
		Exceeded: count >= limit,
		InFlight: inFlight,
		Note:     note,
	}
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
	return WriteHarnessConfig(NameForSession(sessionName), content)
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
// host agent storage root (no per-session sub-dir). Includes the agent-run
// log as an extra archive file when present.
func (b *bwrapIsolator) ArchivePaths(home, sessionName string) ArchivePaths {
	return ArchivePaths{
		StorageRoot: archiveSharedStorageRoot(home),
		ExtraFiles:  archiveAgentRunLogPaths(sessionName),
	}
}

// LogPaths returns the per-mode log file set for bwrap. The agent-run log
// is populated for bwrap because prism agent-run tees the agent's stdout/
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

// Cap probes the sandbox-exec concurrency cap from the DB only. Reads the cap
// value from config.Load().SandboxExecConcurrencyCap; returns CapStatus{Limit:
// 0} when the cap is configured to 0 (uncapped sentinel).
//
// Calls db.ActiveSessionCountForMode("sandbox-exec") and
// db.ActiveSessionsForMode("sandbox-exec").
func (s *sandboxExecIsolator) Cap(ctx context.Context, dbPath string) CapStatus {
	limit := config.Load().SandboxExecConcurrencyCap
	if limit == 0 {
		return CapStatus{Mode: config.IsolationSandboxExec, Limit: 0}
	}
	count, inFlight, note := dbSessionsForMode(dbPath, config.IsolationSandboxExec)
	return CapStatus{
		Mode:     config.IsolationSandboxExec,
		Limit:    limit,
		Count:    count,
		Exceeded: count >= limit,
		InFlight: inFlight,
		Note:     note,
	}
}

// WriteHarnessConfigBlob writes the opencode.json config blob to the
// deterministic per-session temp path. For sandbox-exec the file is read
// directly by the agent at the sandbox-mapped HOME path (sandbox_exec_home.go:274).
func (s *sandboxExecIsolator) WriteHarnessConfigBlob(sessionName, content string) error {
	if content == "" {
		return nil
	}
	return WriteHarnessConfig(NameForSession(sessionName), content)
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
// the agent's stdout/stderr there.
func (s *sandboxExecIsolator) LogPaths() LogPaths {
	return LogPaths{}
}

// ----------------------------------------------------------------------------
// hostIsolator
// ----------------------------------------------------------------------------

// Available is always nil for host mode. The host isolator runs the agent
// directly in the tmux pane with no sandbox layer; there is nothing to
// check beyond what cobra has already validated.
func (h *hostIsolator) Available() error {
	return nil
}

// Cap always returns an uncapped CapStatus for host mode (no concurrency cap
// applies — host sessions consume neither container slots nor sandbox slots).
func (h *hostIsolator) Cap(ctx context.Context, dbPath string) CapStatus {
	return CapStatus{Mode: config.IsolationHost, Limit: 0}
}

// WriteHarnessConfigBlob is a no-op for host mode: opencode reads
// ~/.config/opencode/opencode.json directly via xdg.configFile, so there is
// no per-session blob to write. Mirrors the call-site gate (NeedsConfigBlob
// is false for host).
func (h *hostIsolator) WriteHarnessConfigBlob(sessionName, content string) error {
	return nil
}

// AgentPaneCmd returns DirectCmd unchanged — host mode runs the agent
// directly in the tmux pane and has no sandbox wrapper command. The caller
// is responsible for constructing DirectCmd with all env vars and flags.
func (h *hostIsolator) AgentPaneCmd(opts AgentPaneOpts) string {
	return opts.DirectCmd
}

// SidecarFlags returns the sidecar argv extensions for host mode. Host
// sessions use the same host-API socket path as bwrap/sandbox-exec, so the
// common helper is shared. --agent-role is included when opts.AgentRole is
// non-empty so that agent_status.agent_name is written for host-isolation
// sessions (including the pi harness).
func (h *hostIsolator) SidecarFlags(opts SidecarFlagOpts) []string {
	return commonHostAPISidecarFlags(opts)
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

// commonHostAPISidecarFlags returns the SidecarFlags shared by bwrap,
// sandbox-exec, and host. All three modes set up a host-API socket and harness
// but do not own a container lifecycle, so --container is intentionally
// omitted. Mirrors the pre-refactor branch in StartSidecarWithOpts
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

// archiveSharedStorageRoot returns the host-side shared pi storage root
// used by host / bwrap / sandbox-exec.
func archiveSharedStorageRoot(home string) string {
	return filepath.Join(home, ".local", "share", "pi", "storage")
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
