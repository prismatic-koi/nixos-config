// Package session provides composable session lifecycle operations for prism.
// It extracts the core create/attach/name logic that was previously
// embedded in the monolithic ensureAndSwitchSession function in cmd/switch.go.
package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// testDBPath overrides dbPath() during tests. Set via SetTestDBPath.
var testDBPath string

// SetTestDBPath overrides the DB path used by the session package's openDB.
// Only for use in tests. Call t.Cleanup(func() { SetTestDBPath("") }) to
// restore after each test.
func SetTestDBPath(p string) { testDBPath = p }

// dbPath returns the path to prism.db, honouring $XDG_STATE_HOME.
// During tests, testDBPath (set via SetTestDBPath) takes precedence.
func dbPath() string {
	if testDBPath != "" {
		return testDBPath
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "prism.db")
}

// openDB opens prism.db, returning it or an error.
func openDB() (*db.DB, error) {
	return db.Open(dbPath())
}

// Opts carries optional parameters for session creation.
type Opts struct {
	// Prompt is passed to opencode via --prompt at startup.
	//
	// In host mode, when PromptFilePath is also set, BuildOpencodeCmd
	// emits `--prompt "$(cat <quoted PromptFilePath>)"` rather than
	// inlining Prompt onto the launch command. Prompt is still required
	// (non-empty) to enable the substitution; the field's value is not
	// embedded in the command in that case. See #1064.
	Prompt string
	// PromptFilePath, when non-empty AND IsolationMode is "host" AND Prompt
	// is non-empty, makes buildDirectOpencodeCmd emit
	// `--prompt "$(cat <PromptFilePath>)"` so the prompt content does not
	// travel through the tmux command line. The file at PromptFilePath must
	// already contain the prompt bytes (caller-owned: see
	// session.WriteInitialPrompt). Ignored for non-host isolation modes —
	// those route the prompt through the host-API or sidecar instead.
	// See #1064 for the failure mode this guards against.
	PromptFilePath string
	// Agent is the opencode agent name (e.g. "coordinator", "worker").
	// When empty, DefaultAgent is called to derive a default from the directory.
	Agent string
	// Headless: if true, the session is created but no client is switched to it.
	Headless bool
	// Fresh: if true, opencode skips any stored session ID and starts fresh.
	Fresh bool
	// OpencodeSession is the opencode session ID to resume; "" means fresh start.
	OpencodeSession string
	// Layout controls which window layout to set up on creation.
	Layout Layout
	// SessionName is the canonical prism session name. When set, it is passed
	// to opencode via the PRISM_SESSION_NAME environment variable so the plugin
	// can skip its own session-name derivation.
	SessionName string
	// Port is the allocated opencode serve port. When non-zero, BuildOpencodeCmd
	// includes --port <n> and --hostname 127.0.0.1 in the opencode launch command.
	// In container mode, the port is also passed to the sidecar so it knows which
	// host port to bind.
	Port int
	// ContainerMode, when true, switches the agent window command from
	// "opencode --agent <name> --port <n>" to "podman attach <container-name>".
	// The sidecar is responsible for starting the container and signalling readiness
	// before the attach command is sent to the tmux pane.
	// Deprecated: use IsolationMode instead. When IsolationMode is set it
	// takes precedence; ContainerMode is kept for callers that have not migrated.
	ContainerMode bool

	// IsolationMode is the resolved isolation mode for this session.
	// Valid values: "podman", "bwrap", "sandbox-exec", "host". When non-empty
	// it overrides ContainerMode.
	IsolationMode string
	// PluginHostPath is the host-side path to the opencode plugin file that
	// is bind-mounted into the container. Empty string = no plugin. Only used
	// when ContainerMode is true.
	PluginHostPath string
	// SkipStatusSeed, when true, causes setupFullLayout to skip the
	// "prism event tmux-session-start" call that seeds agent_status.
	// Used by the restore path, which manages agent_status directly via the
	// already-open DB handle rather than forking a subprocess.
	SkipStatusSeed bool
	// DB, when non-nil, is used by Create's startup guard to check whether an
	// existing tmux session with the same name is live (last_seen within 60s).
	// When ForceFresh is true, a live session is force-killed and a warning is
	// logged before the new session is created.
	// When ForceFresh is false, a live session is treated as precious and Create
	// returns a no-op so the caller can attach to it.
	// Pass nil to skip the DB-based liveness check.
	DB *db.DB
	// ForceFresh, when true, makes Create kill any existing tmux session with
	// the same name (live or stale) and start a fresh one. This is the correct
	// behaviour for `prism spawn`, where a name collision means a stale zombie
	// that should be replaced.
	//
	// When false (the default), Create treats a live existing session (last_seen
	// within 60s) as precious and returns nil immediately so the caller's
	// subsequent Attach() reattaches to it. A stale or zombie session (no DB row,
	// or last_seen older than 60s) is still killed and recreated, giving the user
	// a working session rather than a dead pane.
	//
	// This is the correct behaviour for `prism switch` / `prism launch`, whose
	// contract is "open or attach to" an existing session.
	ForceFresh bool
	// InstanceID is the UUID instance identifier pre-generated by the caller
	// (e.g. ensureAndSwitch) before session.Create is invoked. When non-empty
	// it is passed to the sidecar process via --instance-id so the sidecar
	// can use it for container labels and bus message scoping without needing
	// to re-read it from the DB (which would race with the tmux-session-start
	// event that writes it). When empty, the sidecar reads from the DB at
	// startup (which may return "" if tmux-session-start hasn't run yet).
	InstanceID string
	// ConfigContent is the JSON blob injected via the harness's config env
	// var (e.g. OPENCODE_CONFIG_CONTENT). When non-empty, it is injected
	// into the agent process environment to override model and variant
	// settings at runtime.
	//
	// Generated by config.BuildConfigContent from --profile, --model, and/or
	// --variant flags. When empty, no config env var is injected and the
	// agent runtime uses its baked-in config unchanged.
	ConfigContent string
	// AgentEnvVars holds environment variables to prepend to the opencode
	// command string in host-mode (ContainerMode = false) sessions. Each
	// entry is emitted as KEY=<quoted-value> before PRISM_SESSION_NAME so
	// that the sh -c invocation in tmux new-window receives the restricted
	// env vars without needing zsh aliases.
	//
	// Loaded from the agent_env_vars key of profiles.json (written by Nix).
	// Ignored when ContainerMode is true — container sessions are handled via
	// podman --env flags in the sidecar.
	AgentEnvVars map[string]string
	// ConfigEnvVarName is the environment variable name used to inject
	// serialised config content into the agent runtime (e.g.
	// "OPENCODE_CONFIG_CONTENT" for opencode). Populated from
	// harness.Harness.ConfigEnvVar() by callers that have a harness
	// instance. When empty, config content injection is skipped in
	// buildDirectOpencodeCmd (container-mode callers inject config via
	// mounted files, not env vars).
	ConfigEnvVarName string
	// RuntimeEnvVars holds harness-specific environment variables to
	// prepend to the agent command in host-mode sessions (e.g. opencode's
	// experimental bash-tool timeout). Populated from
	// harness.Harness.RuntimeEnv() by callers that have a harness instance.
	// When empty, no harness-specific env vars are injected.
	// These are prepended outermost (before AgentEnvVars and
	// PRISM_SESSION_NAME).
	RuntimeEnvVars map[string]string
}

// Layout selects the window layout used when creating a new session.
type Layout int

const (
	// LayoutBare sets up a minimal session with only the default shell window.
	// Used by the dashboard dead-session recovery path.
	// This is the zero value; an uninitialised Opts{} will never accidentally
	// trigger the full three-window layout.
	LayoutBare Layout = iota

	// LayoutScratchpad sets up a single "term" window with no agent or editor.
	LayoutScratchpad

	// LayoutFull sets up the standard three-window layout:
	// window 0 "edit" (nvim), window 1 "agent" (opencode), window 2 "term".
	LayoutFull

	// LayoutAgentOnly sets up a minimal two-window layout: window 0 is a
	// bare shell, window 1 runs the agent command. Used by review-style
	// sessions (spawned via SpawnSession) that do not need an editor or
	// terminal window — the worktree is read-only for them.
	LayoutAgentOnly
)

// DefaultAgent returns the agent to use for the given directory.
// If explicit is non-empty it is returned unchanged.
// Otherwise the parent directory is checked for a .bare entry (prism
// bare+worktree layout):
//   - parent has .bare AND basename == "main"  → "coordinator"
//   - parent has .bare AND basename ≠ "main"   → "worker"
//   - parent does NOT have .bare               → "" (non-worktree path)
//
// Callers must treat "" as "no --agent flag, but use coordinator config blob".
func DefaultAgent(directory, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if git.IsBareRepo(filepath.Dir(directory)) {
		if filepath.Base(directory) == "main" {
			return "coordinator"
		}
		return "worker"
	}
	return ""
}

// DefaultAgentForSession returns the agent to use for the given session,
// using a DB-backed read of root_agent_name when available.
// If explicit is non-empty it is returned unchanged.
// Otherwise it reads root_agent_name from the DB; if the row has a non-NULL
// root_agent_name it is returned directly. Falls back to DefaultAgent (directory
// heuristic) for pre-migration rows where root_agent_name is NULL.
// When d is nil, falls back to DefaultAgent unconditionally.
func DefaultAgentForSession(sessionName, directory, explicit string, d *db.DB) string {
	if explicit != "" {
		return explicit
	}
	if d != nil {
		rootName, rowExists, err := d.RootAgentName(sessionName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[prism] warning: DefaultAgentForSession: DB error reading root_agent_name for %q: %v — using directory heuristic\n", sessionName, err)
		} else if rowExists && rootName != "" {
			// DB-backed: return the stored root_agent_name.
			dirBased := DefaultAgent(directory, "")
			if rootName != dirBased {
				fmt.Fprintf(os.Stderr, "[debug] DefaultAgentForSession(%q): DB says %q, directory heuristic says %q\n", sessionName, rootName, dirBased)
			}
			return rootName
		} else if rowExists {
			// Row exists but root_agent_name is NULL — pre-migration row.
			fmt.Fprintf(os.Stderr, "[deprecation] DefaultAgentForSession(%q): root_agent_name is NULL — using directory heuristic\n", sessionName)
		}
		// rowExists=false: no row yet — use directory heuristic silently.
	}
	return DefaultAgent(directory, "")
}

// effectiveIsolationMode returns the resolved isolation mode for opts,
// falling back to ContainerMode for back-compat.
func effectiveIsolationMode(opts Opts) string {
	if opts.IsolationMode != "" {
		return opts.IsolationMode
	}
	if opts.ContainerMode {
		return "podman"
	}
	return "host"
}

// BuildOpencodeCmd returns the opencode launch command string for the given opts.
//
// Isolation mode determines the command:
//   - "podman":       "podman attach --sig-proxy=false <container-name>"
//   - "bwrap":        "prism agent-run --session <session-name>"
//   - "sandbox-exec": "prism agent-run --session <session-name>"
//   - "host":         direct opencode invocation (legacy behaviour)
//
// For back-compat, ContainerMode=true maps to "podman" when IsolationMode is empty.
func BuildOpencodeCmd(opts Opts) string {
	mode := effectiveIsolationMode(opts)
	switch mode {
	case "podman":
		if opts.SessionName == "" {
			// Shouldn't happen — callers must set SessionName before enabling
			// container mode. Fall back to the non-container command.
			return buildDirectOpencodeCmd(opts)
		}
		// Use podman attach to bridge the tmux pane to the container's PTY.
		// The container runs opencode in combined TUI + HTTP mode; "podman attach"
		// connects stdin/stdout to the container PTY so the TUI is fully interactive.
		// --sig-proxy=false prevents podman from forwarding signals (e.g. SIGINT from
		// Ctrl-C) to the container process; instead the ^C byte reaches opencode's TUI
		// as literal stdin input, which it handles as an interrupt keystroke — matching
		// host-mode behaviour where Ctrl-C interrupts the current turn, not the process.
		// The container name is shell-quoted so that any unexpected characters in
		// the session name cannot be interpreted as shell metacharacters when
		// buildReadinessWaitCmd embeds this string in the readiness shell script.
		return "podman attach --sig-proxy=false " + shellQuote(container.NameForSession(opts.SessionName))

	case "bwrap", "sandbox-exec":
		if opts.SessionName == "" {
			// Shouldn't happen — fall back to direct opencode command.
			return buildDirectOpencodeCmd(opts)
		}
		// Both bwrap and sandbox-exec sandboxes are owned by the tmux pane,
		// not the sidecar. "prism agent-run" reads the session's isolation mode
		// from the DB and dispatches to the appropriate sandbox invocation.
		return "prism agent-run --session " + shellQuote(opts.SessionName)

	default: // "host" or any unknown value
		return buildDirectOpencodeCmd(opts)
	}
}

// buildDirectOpencodeCmd returns the opencode direct-launch command (pre-container mode).
func buildDirectOpencodeCmd(opts Opts) string {
	agent := opts.Agent
	var cmd string
	if agent != "" {
		cmd = "opencode --agent " + agent
	} else {
		cmd = "opencode"
	}
	if opts.Port != 0 {
		cmd += fmt.Sprintf(" --port %d --hostname 127.0.0.1", opts.Port)
	}
	if opts.OpencodeSession != "" && !opts.Fresh {
		cmd += " -s " + opts.OpencodeSession
	}
	if opts.Prompt != "" {
		// #1064: when PromptFilePath is supplied, route the prompt through
		// $(cat …) so the prompt content is loaded inside the pane shell
		// from disk rather than carried on the tmux command line. Keeps
		// the launch command small (a few hundred bytes) regardless of
		// prompt size; the prompt itself reaches opencode via argv after
		// the shell substitutes the file contents in. The single double-
		// quotes around the substitution preserve newlines and prevent
		// word-splitting; the file path is single-quoted so any unusual
		// characters in the per-session run dir cannot be interpreted as
		// shell metacharacters.
		if opts.PromptFilePath != "" {
			cmd += ` --prompt "$(cat ` + shellQuote(opts.PromptFilePath) + `)"`
		} else {
			cmd += " --prompt " + shellQuote(opts.Prompt)
		}
	}
	// Prepend config content env var before the opencode command when set.
	// The env var name comes from the harness (e.g. "OPENCODE_CONFIG_CONTENT"
	// for opencode). This overrides model and variant at runtime.
	if opts.ConfigContent != "" && opts.ConfigEnvVarName != "" {
		cmd = opts.ConfigEnvVarName + "=" + shellQuote(opts.ConfigContent) + " " + cmd
	}
	if opts.SessionName != "" {
		cmd = "PRISM_SESSION_NAME=" + shellQuote(opts.SessionName) + " " + cmd
	}
	// Prepend agent env vars before PRISM_SESSION_NAME, in sorted key order
	// for determinism. Only applies to host-mode sessions — container sessions
	// receive env vars via podman --env flags in the sidecar.
	if !opts.ContainerMode && len(opts.AgentEnvVars) > 0 {
		keys := make([]string, 0, len(opts.AgentEnvVars))
		for k := range opts.AgentEnvVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var prefix strings.Builder
		for _, k := range keys {
			prefix.WriteString(k)
			prefix.WriteString("=")
			prefix.WriteString(shellQuote(opts.AgentEnvVars[k]))
			prefix.WriteString(" ")
		}
		cmd = prefix.String() + cmd
	}
	// Prepend harness-specific runtime env vars. These are applied outermost
	// (before AgentEnvVars and PRISM_SESSION_NAME) so they reach the agent
	// runtime regardless of profile-level env overrides. Scoped to host-mode
	// only — container-mode sessions receive env vars via podman --env or
	// bwrap --setenv.
	if len(opts.RuntimeEnvVars) > 0 {
		keys := make([]string, 0, len(opts.RuntimeEnvVars))
		for k := range opts.RuntimeEnvVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var prefix strings.Builder
		for _, k := range keys {
			prefix.WriteString(k)
			prefix.WriteString("=")
			prefix.WriteString(shellQuote(opts.RuntimeEnvVars[k]))
			prefix.WriteString(" ")
		}
		cmd = prefix.String() + cmd
	}
	return cmd
}

// shellQuote wraps s in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// Host-mode launch-command size thresholds (#1064).
//
// HostLaunchCmdSafeBound is the maximum constructed launch-command size
// SpawnSession will hand to tmux without rejecting up-front. Above this,
// SpawnSession exits non-zero with HostLaunchCmdTooLargeError before any
// tmux state is created — the empirical failure threshold for tmux
// arg-handling is somewhere above 12 KB and below ~64 KB depending on
// build / terminal config, so 4 KB is a comfortable conservative ceiling
// that still leaves headroom for realistic prompts after the Option-A
// $(cat …) plumbing pulls the prompt body off the command line. With the
// fix in place, the only way to exceed this is to inflate the command
// itself (huge AgentEnvVars, exotic ConfigContent, etc.) — exactly the
// future regression class this guard exists to surface loudly.
const HostLaunchCmdSafeBound = 4 * 1024

// HostLaunchCmdWarnThreshold is the launch-command size above which
// SpawnSession enriches a readiness-gate timeout error with a hint that
// prompt size may be the cause (see #1064 AC-7). Most healthy host-mode
// launch commands are a few hundred bytes (env-var prefixes plus the
// opencode invocation); 1 KB is "unusual but not necessarily broken" and
// big enough to be worth mentioning when a timeout fires.
const HostLaunchCmdWarnThreshold = 1024

// HostLaunchCmdTooLargeError is returned by SpawnSession when the
// constructed host-mode launch command exceeds HostLaunchCmdSafeBound. It
// carries the actual size, the safe bound, and the session name so the
// operator can pattern-match the message and mechanically extract the
// numbers (#1064 AC-6). The error fires before any tmux state is created;
// callers can treat the spawn as having had no observable side effects on
// the tmux server.
type HostLaunchCmdTooLargeError struct {
	SessionName string
	CmdSize     int
	SafeBound   int
}

func (e *HostLaunchCmdTooLargeError) Error() string {
	return fmt.Sprintf(
		"host-mode launch command for session %q is %d bytes, above the safe bound of %d bytes — "+
			"tmux cannot reliably deliver commands above this size and would fail silently. "+
			"Workaround: spawn with a small placeholder prompt (e.g. --prompt 'wait') and send the "+
			"real prompt via `prism prompt %s --prompt-file <path>` once the session is alive. "+
			"See issue #1064.",
		e.SessionName, e.CmdSize, e.SafeBound, e.SessionName,
	)
}

// IsHostLaunchCmdTooLarge reports whether err is (or wraps) a
// HostLaunchCmdTooLargeError. Used by callers that want to render a
// targeted message for this specific failure mode.
func IsHostLaunchCmdTooLarge(err error) bool {
	var hltl *HostLaunchCmdTooLargeError
	return errors.As(err, &hltl)
}

// startupGuardKillOld implements the session instance startup guard. It checks
// whether a tmux session with the given name is already alive and determines
// the appropriate action based on opts.ForceFresh:
//
//   - ForceFresh=true: kill any existing session (live or stale) and return
//     true so the caller proceeds to create a fresh one. A warning is emitted
//     when the killed session is live (last_seen < 60s). This is the spawn path.
//
//   - ForceFresh=false: treat a live session (last_seen < 60s) as precious —
//     return false so Create becomes a no-op and the caller's Attach() reconnects
//     to the existing session. A stale session (no DB row, or last_seen ≥ 60s)
//     is killed silently and true is returned so a fresh session is created.
//     This is the switch/launch path.
//
// When d is nil and ForceFresh is false, falls back to the legacy no-op: an
// existing session is treated as live and creation is skipped (returns false).
// When d is nil and ForceFresh is true, kills the existing session unconditionally.
func startupGuardKillOld(name string, d *db.DB, forceFresh bool) bool {
	if !tmux.HasSession(name) {
		return true // session does not exist — safe to create
	}

	// Session exists in tmux. Determine liveness via DB when available.
	// Hoist st to function scope so it can be reused in the ForceFresh=true
	// warning path without a second round-trip to the DB.
	var st *db.Status
	isLive := false
	if d != nil {
		var stErr error
		st, stErr = d.CurrentStatus(name)
		if stErr == nil && st != nil {
			isLive = time.Since(st.LastSeen) < 60*time.Second
		}
		// No DB row → treat as stale zombie (isLive stays false).
	}

	if !forceFresh {
		// switch/launch path: attach to live sessions, recreate stale ones.
		if isLive {
			// Live session is precious — skip creation, let caller attach.
			return false
		}
		if d == nil {
			// No DB handle, can't determine liveness — legacy no-op: skip creation.
			return false
		}
		// Stale or zombie session — kill it silently and recreate.
		if killErr := tmux.KillSession(name); killErr != nil {
			fmt.Fprintf(os.Stderr,
				"[prism] warning: could not kill stale session %q: %v\n", name, killErr)
			return false
		}
		return true
	}

	// ForceFresh=true (spawn path): kill existing session regardless of liveness.
	// Reuse st from the query above (no second round-trip).
	if isLive && st != nil {
		age := time.Since(st.LastSeen)
		fmt.Fprintf(os.Stderr,
			"[prism] warning: killing live session %q (last_seen %v ago) to start new instance\n",
			name, age.Round(time.Second))
	}
	if killErr := tmux.KillSession(name); killErr != nil {
		fmt.Fprintf(os.Stderr,
			"[prism] warning: could not kill existing session %q: %v\n", name, killErr)
		return false
	}
	return true
}

// Create creates a new tmux session at the given directory with the given name
// and sets up the window layout specified by opts.Layout.
//
// Behaviour when a tmux session with the same name already exists depends on
// opts.ForceFresh:
//
//   - ForceFresh=true (spawn path): any existing session is killed (with a
//     warning if it is live) and a fresh one is created. This ensures each
//     prism spawn gets a uniquely-identified instance.
//
//   - ForceFresh=false (switch/launch path): a live session (last_seen within
//     60s in opts.DB) is left untouched and Create returns nil so the caller's
//     subsequent Attach() reconnects to it. A stale or zombie session is killed
//     and recreated. When opts.DB is nil, any existing session is treated as
//     live (legacy no-op behaviour).
func Create(name, directory string, opts Opts) error {
	if !startupGuardKillOld(name, opts.DB, opts.ForceFresh) {
		return nil
	}

	if err := tmux.NewSessionDetached(name, directory); err != nil {
		return fmt.Errorf("new-session: %w", err)
	}

	switch opts.Layout {
	case LayoutFull:
		if err := setupFullLayout(name, directory, opts); err != nil {
			return err
		}

	case LayoutScratchpad:
		_ = tmux.RenameWindow(name+":0", "term")

	default:
		// LayoutBare (and any unknown value): no extra setup — leave the
		// default shell window as-is.
	}

	return nil
}

// agentPaneEnvVars builds the env-var map for the agent tmux pane.
// When opts.Prompt is non-empty, PRISM_INITIAL_PROMPT is included so that
// "prism agent-run" can read it and populate container.Config.InitialPrompt,
// activating bwrap's --prompt CLI-append path.
//
// Skipped entirely for host mode: the host-mode launch path reads the prompt
// directly via $(cat …) (see buildDirectOpencodeCmd / #1064), and emitting a
// large PRISM_INITIAL_PROMPT here would re-introduce the same tmux arg-size
// limit the prompt-file plumbing was added to avoid. The variable is also
// unused on the host path — only `prism agent-run` (bwrap entry point)
// consumes it.
//
// Returns nil when no env vars are needed, producing no -e flags in tmux.
func agentPaneEnvVars(opts Opts) map[string]string {
	if opts.Prompt == "" {
		return nil
	}
	if effectiveIsolationMode(opts) == "host" {
		return nil
	}
	return map[string]string{
		"PRISM_INITIAL_PROMPT": opts.Prompt,
	}
}

// setupFullLayout configures the three-window layout for a project session:
// window 0 "edit" (nvim auto-launched), window 1 "agent" (opencode),
// window 2 "term". Seeds agent_status via prism event tmux-session-start.
//
// In podman mode (isolation mode "podman"), the sidecar is started first and
// the agent window runs a readiness-wait loop before running "podman attach".
// This ensures the container is healthy before the PTY bridge tries to connect.
//
// In bwrap mode (isolation mode "bwrap"), the sidecar is still started (for
// SSE, state machine, and host-API) but the agent window runs
// "prism agent-run --session <name>" directly — no readiness wait, because
// bwrap is owned by the tmux pane and doesn't use a sidecar-written ready file.
func setupFullLayout(name, directory string, opts Opts) error {
	mode := effectiveIsolationMode(opts)

	_ = tmux.RenameWindow(name+":0", "edit")

	nvimCmd := NvimCmd(directory)
	_ = tmux.SendKeys(name+":0", nvimCmd)

	// Start the sidecar before creating the agent window.
	// In podman and bwrap mode the sidecar handles SSE, state transitions,
	// and host-API — we must start it before the pane command so readiness
	// signalling and prompt delivery are in place.
	// In host mode the sidecar runs without --container (no container creation).
	if opts.Port == 0 {
		fmt.Fprintf(os.Stderr, "warning: sidecar skipped for %q — no port allocated\n", name)
	} else {
		sidecarOpts := StartSidecarOpts{
			Port:           opts.Port,
			ContainerMode:  mode == "podman",
			IsolationMode:  mode,
			AgentRole:      opts.Agent,
			Worktree:       directory,
			PluginHostPath: opts.PluginHostPath,
			InitialPrompt:  opts.Prompt,
			ConfigContent:  opts.ConfigContent,
			InstanceID:     opts.InstanceID,
		}
		if err := StartSidecarWithOpts(name, sidecarOpts); err != nil {
			// Non-fatal: log and continue. The session is created regardless.
			fmt.Fprintf(os.Stderr, "warning: could not start sidecar for %q: %v\n", name, err)
		}
	}

	// Build the agent window command.
	// In podman mode, prepend a readiness wait so the pane blocks until
	// the sidecar has health-checked the container (AC-18, AC-19, AC-20).
	// In bwrap mode, no readiness wait — "prism agent-run" runs immediately
	// and the sidecar does not write a ready file for bwrap sessions.
	// opts.SessionName must be set by the caller before setupFullLayout is
	// invoked — both ensureAndSwitch and restoreProjectSession do this.
	// BuildOpencodeCmd uses it to prefix PRISM_SESSION_NAME for the plugin.
	agentCmd := BuildOpencodeCmd(opts)
	if mode == "podman" && opts.Port != 0 {
		readyPath, pathErr := SidecarReadyPath(name)
		if pathErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not determine ready path for %q, skipping readiness wait: %v\n", name, pathErr)
		} else {
			// Remove any stale ready file from a previous session lifecycle
			// before the pane script starts polling.
			_ = os.Remove(readyPath)
			agentCmd = buildReadinessWaitCmd(readyPath, agentCmd)
		}
	}

	// Persist isolation_mode (and host_mode for "host") BEFORE opening the
	// agent window. This is the critical ordering fix: prism agent-run in
	// window 1 reads isolation_mode from agent_status immediately on start.
	// If we write isolation_mode only after the window exists (as the old
	// post-ensureAndSwitch block in cmd/spawn.go did), prism agent-run
	// races and sees NULL → falls back to "podman" → dies with a mode
	// mismatch error. Writing here, synchronously before NewWindow, removes
	// the race entirely. See issue #894.
	if mode != "" {
		// Always write isolation_mode when we have a non-empty mode.
		if d, dbErr := openDB(); dbErr == nil {
			if setErr := d.SetIsolationMode(name, mode); setErr != nil {
				fmt.Fprintf(os.Stderr, "warning: setupFullLayout: set isolation_mode for %q: %v\n", name, setErr)
			}
			if mode == "host" {
				if setErr := d.SetHostMode(name, true); setErr != nil {
					fmt.Fprintf(os.Stderr, "warning: setupFullLayout: set host_mode for %q: %v\n", name, setErr)
				}
			}
			d.Close()
		} else {
			fmt.Fprintf(os.Stderr, "warning: setupFullLayout: could not open DB to write isolation_mode for %q: %v\n", name, dbErr)
		}
	}

	// Create the agent window with the command passed directly at window
	// creation time. This runs the command via "sh -c <cmd>", bypassing
	// tmux's command parser — semicolons in the readiness-wait script are
	// delivered verbatim to the shell instead of being consumed by tmux.
	// agentPaneEnvVars returns PRISM_INITIAL_PROMPT when a prompt is set,
	// enabling bwrap's --prompt delivery path via prism agent-run.
	_ = tmux.NewWindow(name, 1, "agent", directory, agentCmd, agentPaneEnvVars(opts))

	if !opts.SkipStatusSeed {
		self, selfErr := os.Executable()
		if selfErr != nil {
			return fmt.Errorf("resolve prism binary: %w", selfErr)
		}
		seedArgs := []string{"event", "tmux-session-start",
			"--session", name,
			"--worktree", directory,
		}
		// When opts.Agent is known at spawn time, pass it as --agent-role so
		// that root_agent_name is seeded in the DB row immediately (before the
		// sidecar's first upsertState() call). Omitting it falls back to the
		// plain UpsertStatus path that leaves root_agent_name as NULL.
		if opts.Agent != "" {
			seedArgs = append(seedArgs, "--agent-role", opts.Agent)
		}
		if err := exec.Command(self, seedArgs...).Run(); err != nil {
			return fmt.Errorf("seed agent_status: %w", err)
		}
	}

	_ = tmux.NewWindow(name, 2, "term", directory, "", nil)

	focusIdx := 1
	if strings.Contains(directory, "obsidian") {
		focusIdx = 0
	}
	_ = tmux.SelectWindow(name, focusIdx)

	return nil
}

// buildReadinessWaitCmd builds a shell command that polls for the sidecar
// readiness file and, once found, runs the given attach command.
// If the wait times out (120s), it prints an error and exits (AC-20).
func buildReadinessWaitCmd(readyPath, attachCmd string) string {
	// Poll every 0.5s for up to 120s (240 iterations).
	// On success, exec the attach command directly.
	return fmt.Sprintf(
		`i=0; while [ ! -f %s ] && [ $i -lt 240 ]; do sleep 0.5; i=$((i+1)); done; `+
			`if [ ! -f %s ]; then `+
			`echo "prism: container did not become ready within 120s" >&2; exit 1; `+
			`fi; `+
			`%s`,
		shellQuote(readyPath), shellQuote(readyPath), attachCmd,
	)
}

// NvimCmd returns the nvim command to run in the edit window for the given
// directory. It auto-selects a file to open based on directory contents.
func NvimCmd(directory string) string {
	nvimCmd := "nvim"
	if des, err := os.ReadDir(directory); err == nil {
		var files []string
		for _, de := range des {
			if !de.IsDir() {
				files = append(files, filepath.Join(directory, de.Name()))
			}
		}
		switch {
		case len(files) == 1:
			nvimCmd = "nvim " + shellQuote(files[0])
		case strings.Contains(directory, "obsidian"):
			nvimCmd = "nvim +'Obsidian today'"
		default:
			readme := filepath.Join(directory, "README.md")
			if _, err := os.Stat(readme); err == nil {
				nvimCmd = "nvim " + shellQuote(readme)
			}
		}
	}
	return nvimCmd
}

// Attach switches a tmux client to the named session, or attaches directly if
// running outside tmux.
func Attach(name string) error {
	if os.Getenv("TMUX") == "" {
		return tmux.AttachSession(name)
	}

	client, _ := tmux.CurrentClient()
	if client != "" {
		return tmux.SwitchClient(client, name)
	}
	_, err := tmux.SwitchClientCurrent(name)
	return err
}

// NameFor derives the tmux session name for a given directory and optional
// project root. dir must already be expanded (no ~).
//
// When projectRoot is set (prism bare+worktree layout), the session name is
// "<repo>@<branch>" where branch is the full branch name with "/" replaced by
// "--" for tmux compatibility.
//
// Falls back to the short commit hash for detached HEAD, and to
// filepath.Base of the worktree directory when git is not available.
func NameFor(dir, projectRoot string) string {
	if projectRoot != "" {
		projName := strings.ReplaceAll(filepath.Base(projectRoot), ".", "_")
		branch := worktreeBranchComponent(dir)
		return projName + "@" + branch
	}
	return strings.ReplaceAll(filepath.Base(dir), ".", "_")
}

// worktreeBranchComponent returns the branch component of a session name for
// the worktree at dir.
func worktreeBranchComponent(dir string) string {
	ref, err := git.SymbolicRef(dir)
	if err == nil {
		branch := strings.TrimPrefix(ref, "refs/heads/")
		return strings.ReplaceAll(branch, "/", "--")
	}
	if hash, err := git.ShortHash(dir); err == nil {
		return hash
	}
	return filepath.Base(dir)
}
