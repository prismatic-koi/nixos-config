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

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/proglog"
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
	// Prompt is passed to the agent via --prompt at startup.
	//
	// In host mode, when PromptFilePath is also set, BuildAgentCmd
	// emits `--prompt "$(cat <quoted PromptFilePath>)"` rather than
	// inlining Prompt onto the launch command. Prompt is still required
	// (non-empty) to enable the substitution; the field's value is not
	// embedded in the command in that case. See #1064.
	Prompt string
	// PromptFilePath, when non-empty AND IsolationMode is "host" AND Prompt
	// is non-empty, makes buildDirectAgentCmd emit
	// `--prompt "$(cat <PromptFilePath>)"` so the prompt content does not
	// travel through the tmux command line. The file at PromptFilePath must
	// already contain the prompt bytes (caller-owned: see
	// session.WriteInitialPrompt). Ignored for non-host isolation modes —
	// those route the prompt through the host-API or sidecar instead.
	// See #1064 for the failure mode this guards against.
	PromptFilePath string
	// Agent is the agent name (e.g. "coordinator", "worker").
	// When empty, DefaultAgent is called to derive a default from the directory.
	Agent string
	// Headless: if true, the session is created but no client is switched to it.
	Headless bool
	// Fresh: if true, the agent skips any stored session ID and starts fresh.
	Fresh bool
	// Layout controls which window layout to set up on creation.
	Layout Layout
	// SessionName is the canonical prism session name. When set, it is passed
	// to the agent via the PRISM_SESSION_NAME environment variable so the plugin
	// can skip its own session-name derivation.
	SessionName string
	// Port is the allocated harness serve port. When non-zero, BuildAgentCmd
	// includes --port <n> and --hostname 127.0.0.1 in the agent launch command.
	// In container mode, the port is also passed to the sidecar so it knows which
	// host port to bind.
	Port int
	// IsolationMode is the resolved isolation mode for this session.
	// Valid values: "bwrap", "sandbox-exec", "host" (see
	// config.ValidIsolationModes).
	IsolationMode string
	// PluginHostPath is the host-side path to the agent plugin file.
	// Empty string = no plugin. Retained for back-compat; no current
	// isolation mode consumes it.
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
	// AgentEnvVars holds environment variables to prepend to the agent
	// command string in host-mode sessions. Each entry is emitted as
	// KEY=<quoted-value> before PRISM_SESSION_NAME so that the sh -c
	// invocation in tmux new-window receives the restricted env vars without
	// needing zsh aliases.
	//
	// Loaded from the agent_env_vars key of profiles.json (written by Nix).
	// Ignored in sandboxed modes — bwrap and sandbox-exec deliver env vars
	// via their own injection paths (bwrap --setenv, sandbox-exec profile).
	AgentEnvVars map[string]string
	// RuntimeEnvVars holds harness-specific environment variables to
	// prepend to the agent command in host-mode sessions. Populated from
	// harness.Harness.RuntimeEnv() by callers that have a harness instance.
	// When empty, no harness-specific env vars are injected.
	// These are prepended outermost (before AgentEnvVars and
	// PRISM_SESSION_NAME).
	RuntimeEnvVars map[string]string
	// HarnessName is the registered harness name (e.g. "pi"). When
	// non-empty it is forwarded to the sidecar via --harness so the sidecar
	// can call harness.ShapeOf to determine its own transport shape. When
	// empty, the sidecar defaults to "pi".
	HarnessName string
	// HarnessPipeSockPath is the Unix socket path for the PI harness pipe
	// (TransportSocketPipe). When non-empty and isolation mode is "host",
	// agentPaneEnvVars injects PRISM_HARNESS_PIPE=unix://<path> into the
	// agent tmux pane so the PI extension can connect to the sidecar socket.
	// Ignored for bwrap/sandbox-exec (those modes set PRISM_HARNESS_PIPE via
	// their own paths).
	HarnessPipeSockPath string
	// ModelsByRole is the per-role model override map (C.2). When non-empty
	// it is forwarded to the sidecar via repeated --model-override flags.
	// Nil means no per-role overrides.
	//
	// The entry for THIS session's role (Agent) also selects the model pi
	// runs on, and outranks Model below: in host mode buildDirectAgentCmd
	// emits it as pi's `--model`; in bwrap and sandbox-exec BuildAgentCmd
	// puts it on AgentPaneOpts.AgentModel, which reaches pi's argv via
	// `prism agent-run --agent-model` → populatePIConfig →
	// container.Config.AgentModel → PIInvocation. Entries for other roles are
	// a no-op for this session. Issue #2863.
	ModelsByRole map[string]string
	// HarnessSessionID is the persisted harness-specific session UUID to
	// resume when launching the harness (e.g. pi's session UUID). Populated
	// by `prism restore` (and `prism restart`, which calls Restore) from
	// agent_status.harness_session_id; left empty by `prism spawn` and
	// `prism switch`.
	//
	// Consumed in host mode by buildDirectAgentCmd (which calls
	// container.ResolvePIResumeSession and appends `--session <id>` to the
	// direct pi invocation when the on-disk JSONL is found). In bwrap and
	// sandbox-exec the equivalent plumbing lives in cmd/agent_run.go and
	// cmd/agent_run_sandbox_exec_darwin.go, which re-read the value from
	// agent_status and pass it into container.Config.HarnessSessionID for
	// PIInvocation — those paths do not depend on this Opts field because
	// the bwrap/sandbox-exec tmux pane runs `prism agent-run`, which
	// reconstructs its container config from the DB rather than carrying
	// the launch opts forward.
	//
	// Issue #1838.
	HarnessSessionID string
	// Worktree is the absolute worktree path the session was created in. It
	// is required only by the host-mode pi-resume path (buildDirectAgentCmd
	// passes it into container.ResolvePIResumeSession so the encoded-cwd
	// component of the on-disk pi sessions directory resolves correctly).
	// Other launch shapes derive the worktree from the DB or from the
	// container-mode plumbing and do not consult this field.
	Worktree string
	// PIExtensionDir is the host-side absolute path to the directory that
	// contains the prism PI extension file (cfg.PIExtensionDir, populated by
	// Nix via piExtensionDir in config.json). When non-empty AND the harness
	// is pi, buildDirectAgentCmd appends --extension <dir>/prism.ts to the
	// host-mode launch command so the extension loads and the prism↔pi
	// integration surface (role prompt, sidecar bridge, status bar) works
	// the same as it does under bwrap / sandbox-exec. Closes #2065.
	//
	// Production callers MUST set this for host-mode pi launches —
	// ValidatePILaunchOpts (called from SpawnSession and Create+LayoutFull)
	// rejects an empty value with a clear error mirroring the container-path
	// guard at cmd/agent_run.go:730. buildDirectAgentCmd remains a pure
	// string emitter and omits --extension when this field is empty so test
	// fixtures and the validator-bypass paths in restore (with the legacy
	// agent-pane shape) don't need to fake a directory; the fail-fast policy
	// is centralised in ValidatePILaunchOpts, not the emitter.
	PIExtensionDir string

	// Model, when non-empty, is the CLI-supplied model override (`prism spawn
	// --model <X>`). In host mode, buildDirectAgentCmd appends `--model <X>`
	// to the pi argv so it wins over the profile slot's model. In bwrap and
	// sandbox-exec modes the value is threaded into AgentPaneOpts so the
	// `prism agent-run` tmux pane command carries it forward to the
	// populatePIConfig override path. Empty value omits the flag and the
	// profile slot's model is used unchanged. Issue #2086.
	//
	// A ModelsByRole entry for this session's role outranks this field on
	// every isolation mode (issue #2863).
	Model string

	// Variant, when non-empty, is the CLI-supplied variant override (`prism
	// spawn --variant <Y>`). Semantics mirror Model: in host mode the value
	// is appended as `--variant <Y>` (pi consumes it as a thinking/reasoning
	// level via the harness's --thinking mapping at the populatePIConfig
	// override path); in bwrap and sandbox-exec modes it flows through
	// AgentPaneOpts. Issue #2086.
	Variant string

	// Provider, when non-empty, is the CLI-supplied routing-provider override
	// (`prism spawn --provider <P>`). In host mode, buildDirectAgentCmd
	// appends `--provider <P>` to the pi argv so it wins over the profile
	// slot's provider. In bwrap and sandbox-exec modes the value is threaded
	// into AgentPaneOpts so the `prism agent-run` tmux pane command carries it
	// forward to the populatePIConfig override path. Empty value omits the
	// flag and the profile slot's provider is used unchanged. Scoped to the pi
	// harness at every emit site. Issue #2852.
	Provider string
}

// ValidatePILaunchOpts checks Opts against the requirements for a host-mode
// pi launch. It is the host-mode analogue of the container-path guard at
// cmd/agent_run.go:730: when the prism PI extension cannot be located the
// session must fail fast with a clear error rather than launch pi with no
// extension (and therefore no role-prompt injection, no sidecar bridge, no
// status bar — the #2065 silent-degradation shape).
//
// Returns nil for every non-host isolation mode (container modes route
// through PIInvocation which has its own guard upstream) and for every
// non-pi harness (non-pi launches do not load the prism extension).
//
// Called from SpawnSession (every spawn-side entry point) and from Create
// when Layout == LayoutFull (the switch / restore entry points). Bare and
// Scratchpad layouts skip this check because they do not launch an agent
// pane and therefore have no pi to misconfigure.
func ValidatePILaunchOpts(opts Opts) error {
	if effectiveIsolationMode(opts) != "host" {
		return nil
	}
	if opts.HarnessName != "" && opts.HarnessName != "pi" {
		return nil
	}
	if opts.PIExtensionDir == "" {
		return fmt.Errorf("pi: PIExtensionDir is not set in prism config — ensure the prism PI extension is configured in Nix (piExtensionDir in config.json)")
	}
	return nil
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
	// window 0 "edit" (nvim), window 1 "agent" (pi), window 2 "term".
	LayoutFull

	// LayoutAgentOnly sets up a minimal two-window layout: window 0 is a
	// bare shell, window 1 runs the agent command. Used by review-style
	// sessions (spawned via SpawnSession) that do not need an editor or
	// terminal window — the worktree is read-only for them.
	LayoutAgentOnly
)

// DefaultAgent returns the agent to use for the given directory.
// If explicit is non-empty it is returned unchanged.
// Otherwise the directory tree is walked upward (via git.BareRoot) to find
// the nearest ancestor with a .bare entry (prism bare+worktree layout):
//   - an ancestor has .bare AND basename == "main"  → "coordinator"
//   - an ancestor has .bare AND basename ≠ "main"   → "worker"
//   - no ancestor has .bare                          → "" (non-worktree path)
//
// The walk is depth-agnostic: it correctly resolves worktrees nested more
// than one level below the bare root (e.g. a branch name containing "/",
// such as "feat/my-thing", produces a worktree at <bare>/feat/my-thing —
// two levels below the bare root, not one). See issue #2510.
//
// Because the walk is depth-agnostic, it resolves a role for ANY directory
// beneath a bare root, not only worktree roots — e.g. <bare>/main/subdir
// resolves as if it were a worktree ("worker" unless basename == "main").
// The bare root itself does NOT resolve this way: BareRoot starts its walk
// at filepath.Dir(directory), so BareRoot(<bare>) looks one level above
// <bare> and returns "" unless that ancestor is itself a bare repo. Passing
// the bare root therefore falls through to the non-worktree-path case below
// ("", not "worker"). Callers are expected to pass a worktree root. Passing
// a subdirectory of a worktree yields "worker"/"coordinator" rather than "",
// which is a different fallback than the non-worktree-path case documented
// below.
// All current callers (agent_run.go, pr.go, spawn.go, switch.go,
// switch_project.go) pass a worktree root obtained from git worktree
// listings or picker selections, so this is not currently reachable.
//
// Callers must treat "" as "no --agent flag; fall back to the coordinator
// role" — see the lookupRole fallback in cmd/pr.go and cmd/restore.go.
func DefaultAgent(directory, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if git.BareRoot(directory) != "" {
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
			proglog.Warnf("[prism] warning: DefaultAgentForSession: DB error reading root_agent_name for %q: %v — using directory heuristic\n", sessionName, err)
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

// effectiveIsolationMode returns the resolved isolation mode for opts.
// Defaults to "host" when IsolationMode is empty.
func effectiveIsolationMode(opts Opts) string {
	if opts.IsolationMode != "" {
		return opts.IsolationMode
	}
	return "host"
}

// BuildAgentCmd returns the agent launch command string for the given opts.
//
// Isolation mode determines the command:
//   - "bwrap":        "<abs-path>/prism agent-run --session <session-name>"
//   - "sandbox-exec": "<abs-path>/prism agent-run --session <session-name>"
//   - "host":         direct agent invocation (default)
//
// D4 (issue #1133): the per-mode switch collapses into a single
// Isolator.AgentPaneCmd dispatch. Unknown / unregistered modes fall back to
// the direct (host-shape) command — matching the pre-refactor "default" arm.
//
// Returns a non-nil error when the bwrap / sandbox-exec branch cannot
// resolve the prism binary's absolute path (os.Executable failure). Host
// mode and the unknown-mode fallback never fail. Issue #2260: callers must
// propagate the error rather than fall back to a bare "prism" — silent
// fallback would re-introduce the PATH-shadow class the fix exists to
// eliminate.
func BuildAgentCmd(opts Opts) (string, error) {
	mode := config.IsolationMode(effectiveIsolationMode(opts))
	direct := buildDirectAgentCmd(opts)
	iso, err := container.For(mode, container.ConstructorOpts{Name: opts.SessionName})
	if err != nil {
		// Unknown mode: behave like the pre-refactor default arm.
		return direct, nil
	}
	return iso.AgentPaneCmd(container.AgentPaneOpts{
		SessionName: opts.SessionName,
		DirectCmd:   direct,
		// Model / Variant overrides (issue #2086) are forwarded into the
		// tmux pane command for bwrap and sandbox-exec so that `prism
		// agent-run` receives them as explicit flags and can override the
		// active profile slot's model/variant on the final pi argv. The
		// host isolator's AgentPaneCmd returns DirectCmd unchanged, which
		// already carries the flags via buildDirectAgentCmd above.
		Model:   opts.Model,
		Variant: opts.Variant,
		// AgentModel is the per-role `--model-override` entry for THIS
		// session's role (issue #2863), forwarded as its own
		// `prism agent-run --agent-model` flag so PIInvocation can apply the
		// published precedence at the point it renders pi's argv. An entry
		// naming any other role does not match here and is a no-op.
		AgentModel: roleModelOverride(opts),
		// Provider (issue #2852) rides the same seam. HarnessName is
		// forwarded alongside it because appendAgentRunOverrides gates the
		// provider clause on a pi harness name.
		Provider:    opts.Provider,
		HarnessName: opts.HarnessName,
	})
}

// harnessBinary returns the binary name to invoke for the given harness.
// For "pi" (or empty) the binary is pi.
func harnessBinary(harnessName string) string {
	switch harnessName {
	case "pi", "":
		return "pi"
	default:
		return harnessName
	}
}

// roleModelOverride returns the `prism spawn --model-override <role>=<model>`
// entry that applies to this session, or "" when none does (issue #2863).
//
// A session runs exactly one role, so at most one entry of the map can apply:
// the one keyed by opts.Agent. Entries naming any other role belong to other
// sessions of the same fan-out and are a no-op here. An empty opts.Agent
// cannot match, because parseModelOverrides rejects an empty role key.
//
// The single lookup lives in one helper so the host emitter
// (buildDirectAgentCmd) and the sandboxed emitter (BuildAgentCmd →
// AgentPaneOpts) cannot disagree about which entry applies.
func roleModelOverride(opts Opts) string {
	if len(opts.ModelsByRole) == 0 || opts.Agent == "" {
		return ""
	}
	return opts.ModelsByRole[opts.Agent]
}

// buildDirectAgentCmd returns the direct-launch command for the session
// harness (pre-container mode). For harness="" or "pi" this is a pi
// invocation.
//
// Host-mode pi-resume (issue #1838): when opts.HarnessSessionID is non-empty
// and the harness is pi, the launcher calls container.ResolvePIResumeSession
// to look up the on-disk session JSONL under ~/.pi/agent/sessions and, if
// found, appends `--session <id>` immediately before any --prompt argument so
// pi reopens the prior conversation. Missing file falls back silently to a
// fresh conversation (the resolver writes a tagged warning to the per-session
// agent-run.log). Non-pi harnesses and empty IDs skip the resume path
// entirely.
func buildDirectAgentCmd(opts Opts) string {
	binary := harnessBinary(opts.HarnessName)
	agent := opts.Agent
	var cmd string
	if agent != "" {
		cmd = binary + " --agent " + agent
	} else {
		cmd = binary
	}
	// --extension <dir>/prism.ts for pi harnesses (#2065). Container modes
	// (bwrap / sandbox-exec) route through container.PIInvocation, which
	// emits --extension unconditionally. Host mode previously launched pi
	// with no --extension flag at all, so the prism PI extension never
	// loaded and role-prompt injection (plus the sidecar bridge, status
	// bar, doom-loop guard, and review-cycle tracking) silently no-op'd.
	//
	// Scoped to pi (or empty harness, which defaults to pi). Non-pi
	// harnesses do not have a prism extension and must not receive this
	// flag — it would either be unknown or claim an unrelated meaning.
	if (opts.HarnessName == "pi" || opts.HarnessName == "") && opts.PIExtensionDir != "" {
		extPath := container.PIExtensionHostPath(opts.PIExtensionDir)
		if extPath != "" {
			cmd += " --extension " + shellQuote(extPath)
		}
	}
	// --exclude-tools <names> for role-scoped builtin tool restriction
	// (issue #2531), host-mode mirror of container.PIInvocation. See
	// internal/config/agent_tool_roles.go for the role list and rationale.
	if (opts.HarnessName == "pi" || opts.HarnessName == "") && agent != "" {
		if excluded := config.ExcludedToolsForRole(agent); len(excluded) > 0 {
			cmd += " --exclude-tools " + shellQuote(strings.Join(excluded, ","))
		}
	}
	// CLI overrides for model and variant (issue #2086). Scoped to the pi
	// harness (or empty, which defaults to pi). The flag pair must appear
	// before the positional prompt so pi parses them as named flags rather
	// than as positional message bytes. Empty values omit the flag and the
	// profile slot's model/variant is used unchanged (no-regression default
	// path).
	if container.IsPIHarness(opts.HarnessName) {
		// --provider first, mirroring container.PIInvocation's flag order so
		// the host-mode and sandboxed argvs read the same way (issue #2852).
		if opts.Provider != "" {
			cmd += " --provider " + shellQuote(opts.Provider)
		}
		// Model axis, highest rung first (issue #2863): the per-role
		// `--model-override` entry for this session's role beats the
		// session-wide `--model`, which beats the profile slot (resolved by
		// pi itself in host mode). Host mode emits ONE `--model` flag, so the
		// precedence is applied here rather than by PIInvocation, which is
		// the equivalent single argv-rendering point for the sandboxed modes.
		model := opts.Model
		if roleModel := roleModelOverride(opts); roleModel != "" {
			model = roleModel
		}
		if model != "" {
			cmd += " --model " + shellQuote(model)
		}
		if opts.Variant != "" {
			cmd += " --thinking " + shellQuote(opts.Variant)
		}
	}
	// Append --session <id> for host-mode pi-resume (issue #1838).
	//
	// Three guards stack here:
	//
	//  1. effectiveIsolationMode(opts) == "host" — BuildAgentCmd calls this
	//     helper for every mode (the result is discarded for bwrap and
	//     sandbox-exec by their AgentPaneCmd, which substitutes
	//     `prism agent-run --session <name>`). The gate keeps this host-side
	//     probe from running redundantly on every restore: bwrap and
	//     sandbox-exec resume via prism agent-run's DB-read + PIInvocation
	//     path, which performs the same host-root lookup itself — post-#1985
	//     (bwrap) and post-#2210 (sandbox-exec) all modes resolve to the same
	//     host PI sessions root ($PI_CODING_AGENT_DIR/sessions or
	//     ~/.pi/agent/sessions), so a lookup here would succeed but its
	//     result would be discarded, and any transient miss would spuriously
	//     write a misleading "resume failed" line to the agent-run.log via
	//     piLogResumeWarning even though the actual resume happens in
	//     agent-run. (Review round 2 / review-context; comment refreshed for
	//     #2210.)
	//  2. HarnessName ∈ {"pi", ""} — ResolvePIResumeSession's encoded-cwd /
	//     sessions-root layout is pi-specific.
	//  3. HarnessSessionID != "" — empty IDs are a silent no-op (AC5).
	//
	// Inserted before --prompt so the flag pair stays adjacent to the binary
	// and the positional prompt (if any) remains the last token.
	if effectiveIsolationMode(opts) == "host" &&
		opts.HarnessSessionID != "" &&
		(opts.HarnessName == "pi" || opts.HarnessName == "") {
		resumeCfg := container.Config{
			SessionName:      opts.SessionName,
			Worktree:         opts.Worktree,
			HarnessSessionID: opts.HarnessSessionID,
		}
		if container.ResolvePIResumeSession(resumeCfg) {
			cmd += " --session " + shellQuote(opts.HarnessSessionID)
		}
	}
	if opts.Prompt != "" {
		// #1064: when PromptFilePath is supplied, route the prompt through
		// $(cat …) so the prompt content is loaded inside the pane shell
		// from disk rather than carried on the tmux command line. Keeps
		// the launch command small (a few hundred bytes) regardless of
		// prompt size; the prompt itself reaches the agent via argv after
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
	if opts.SessionName != "" {
		cmd = "PRISM_SESSION_NAME=" + shellQuote(opts.SessionName) + " " + cmd
	}
	// Prepend agent env vars before PRISM_SESSION_NAME, in sorted key order
	// for determinism.
	if len(opts.AgentEnvVars) > 0 {
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
	// only — sandboxed sessions receive env vars via bwrap --setenv or the
	// sandbox-exec profile.
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

// Host-mode launch-command size thresholds (#1064 / #1092).
//
// HostLaunchCmdSafeBound is the maximum constructed launch-command size
// SpawnSession will hand to tmux without rejecting up-front. Above this,
// SpawnSession exits non-zero with HostLaunchCmdTooLargeError before any
// tmux state is created.
//
// The empirical failure threshold for tmux arg-handling is somewhere above
// 12 KB and below ~64 KB depending on build / terminal config. The kernel's
// per-argv ARG_MAX is far higher — 128 KiB on Linux and 256 KiB on Darwin —
// so the binding constraint here is tmux's own command parser, not execve.
//
// 4 KiB was the original (pre-#1092) bound, chosen when the only spawned
// shape was LayoutFull and the prompt body was already pulled off the
// launch command via the $(cat …) plumbing (see initial_prompt.go). It
// turned out to be too tight once review fan-outs (LayoutAgentOnly) started
// going through the same guard with the role prompt still inlined into a
// `-e PRISM_INITIAL_PROMPT=<huge>` pair on tmux's argv.
//
// Post-#1092 the role prompt is also delivered via a per-session file
// (PRISM_INITIAL_PROMPT_FILE), so the launch command size is bounded by
// boilerplate (session name, env-var prefixes, harness paths) regardless
// of prompt size. 16 KiB is comfortably above any realistic boilerplate
// total, and well below tmux's empirical ceiling — exotic prompts or
// huge AgentEnvVars are still surfaced as a clear pre-spawn failure rather
// than left to fail silently inside tmux.
const HostLaunchCmdSafeBound = 16 * 1024

// HostLaunchCmdWarnThreshold is the launch-command size above which
// SpawnSession enriches a readiness-gate timeout error with a hint that
// prompt size may be the cause (see #1064 AC-7). Most healthy host-mode
// launch commands are a few hundred bytes (env-var prefixes plus the
// agent invocation); 1 KB is "unusual but not necessarily broken" and
// big enough to be worth mentioning when a timeout fires.
const HostLaunchCmdWarnThreshold = 1024

// HostLaunchCmdTooLargeError is returned by SpawnSession when the
// constructed launch command exceeds HostLaunchCmdSafeBound. It carries the
// actual size, the safe bound, and the session name so the operator can
// pattern-match the message and mechanically extract the numbers (#1064
// AC-6). The error fires before any tmux state is created; callers can
// treat the spawn as having had no observable side effects on the tmux
// server.
//
// The type retains its "HostLaunchCmd…" name for back-compat with #1064
// callers (IsHostLaunchCmdTooLarge etc.) even though the post-#1092 guard
// also covers bwrap/sandbox-exec review fan-outs whose size budget is
// dominated by the env-var pair tmux carries on `new-window -e`.
type HostLaunchCmdTooLargeError struct {
	SessionName string
	CmdSize     int
	SafeBound   int
}

func (e *HostLaunchCmdTooLargeError) Error() string {
	return fmt.Sprintf(
		"launch command for session %q is %d bytes, above the safe bound of %d bytes — "+
			"tmux cannot reliably deliver commands above this size and would fail silently. "+
			"Workaround: spawn with a small placeholder prompt (e.g. --prompt 'wait') and send the "+
			"real prompt via `prism prompt %s --prompt-file <path>` once the session is alive. "+
			"See issues #1064 and #1092.",
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
		if SidecarAlive(name) {
			// The DB row is quiet but the sidecar is responsive. Paused-by-
			// design states (escalated, reviewing, waiting) freeze last_seen
			// while the session is healthy — do not kill it (issue #2255).
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
	// Fail-fast guard for the host-mode pi launch (#2065 edge-case AC).
	// Only applies to LayoutFull, which is the layout that actually launches
	// an agent pane. LayoutBare and LayoutScratchpad have no agent and
	// legitimately leave PIExtensionDir empty (scratchpad sessions are pure
	// shells; bare sessions are restore-time placeholders for non-LayoutFull
	// rows). Running the guard for those layouts would block the dashboard's
	// dead-session recovery path with a misleading error.
	if opts.Layout == LayoutFull {
		if err := ValidatePILaunchOpts(opts); err != nil {
			return fmt.Errorf("session.Create %q: %w", name, err)
		}
	}

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
//
// When opts.PromptFilePath is non-empty (the post-#1092/#1195 path),
// PRISM_INITIAL_PROMPT_FILE carries the path to the prompt file and the
// prompt body itself is NOT inlined into tmux's argv. `prism agent-run`
// reads the file when it sees the env var and feeds the contents to
// the agent's --prompt path. This keeps the launch-command size O(1) in
// prompt size.
//
// SpawnSession always writes the prompt file when there is a non-empty
// prompt, regardless of isolation mode or layout, so the legacy
// PRISM_INITIAL_PROMPT inline branch below is exercised only by direct
// callers that have not opted into the file path (e.g. test code).
// Every production callsite goes through SpawnSession and receives a
// file path.
//
// Skipped entirely for host mode: the host-mode launch path reads the
// prompt directly via $(cat …) (see buildDirectAgentCmd / #1064), so
// no agent-pane env var is needed at all.
//
// Returns nil when no env vars are needed, producing no -e flags in tmux.
func agentPaneEnvVars(opts Opts) map[string]string {
	if effectiveIsolationMode(opts) == "host" {
		// In host mode the prompt is delivered via $(cat …) in the launch
		// command (buildDirectAgentCmd / #1064), so no PRISM_INITIAL_PROMPT
		// env var is needed. However, for socket-pipe harnesses (e.g. "pi")
		// we must inject PRISM_HARNESS_PIPE so the PI extension can find the
		// sidecar Unix socket. bwrap and sandbox-exec set this via their own
		// paths; only inject here for host mode.
		if opts.HarnessPipeSockPath != "" {
			envs := map[string]string{
				"PRISM_HARNESS_PIPE": "unix://" + opts.HarnessPipeSockPath,
			}
			// Durable give-up diagnostics (#2357): host mode has no
			// agent-run process, but the per-session run dir (which also
			// holds the pipe socket above) is the durable home for the PI
			// extension's first-connect give-up line. Best-effort — on
			// resolve failure the extension falls back to pane-only logging.
			if logPath, err := AgentRunLogPath(opts.SessionName); err == nil {
				envs["PRISM_AGENT_RUN_LOG"] = logPath
			}
			return envs
		}
		return nil
	}
	if opts.Prompt == "" {
		return nil
	}
	if opts.PromptFilePath != "" {
		return map[string]string{
			"PRISM_INITIAL_PROMPT_FILE": opts.PromptFilePath,
		}
	}
	return map[string]string{
		"PRISM_INITIAL_PROMPT": opts.Prompt,
	}
}

// setupFullLayout configures the three-window layout for a project session:
// window 0 "edit" (nvim auto-launched), window 1 "agent" (pi),
// window 2 "term". Seeds agent_status via prism event tmux-session-start.
//
// In bwrap mode (isolation mode "bwrap"), the sidecar is started (for
// SSE, state machine, and host-API) but the agent window runs
// "prism agent-run --session <name>" directly — no readiness wait, because
// bwrap is owned by the tmux pane and doesn't use a sidecar-written ready file.
func setupFullLayout(name, directory string, opts Opts) error {
	mode := effectiveIsolationMode(opts)

	_ = tmux.RenameWindow(name+":0", "edit")

	nvimCmd := NvimCmd(directory)
	_ = tmux.SendKeys(name+":0", nvimCmd)

	// Start the sidecar before creating the agent window.
	// In bwrap and sandbox-exec mode the sidecar handles SSE, state
	// transitions, and host-API — we must start it before the pane command so
	// readiness signalling and prompt delivery are in place.
	// In host mode the sidecar connects to the directly-launched agent.
	if opts.Port == 0 {
		fmt.Fprintf(os.Stderr, "warning: sidecar skipped for %q — no port allocated\n", name)
	} else {
		sidecarOpts := StartSidecarOpts{
			Port:           opts.Port,
			IsolationMode:  mode,
			AgentRole:      opts.Agent,
			Worktree:       directory,
			PluginHostPath: opts.PluginHostPath,
			InitialPrompt:  opts.Prompt,
			InstanceID:     opts.InstanceID,
			HarnessName:    opts.HarnessName,
			ModelsByRole:   opts.ModelsByRole,
		}
		if err := StartSidecarWithOpts(name, sidecarOpts); err != nil {
			// Non-fatal: log and continue. The session is created regardless.
			fmt.Fprintf(os.Stderr, "warning: could not start sidecar for %q: %v\n", name, err)
		}
	}

	// Build the agent window command.
	// No readiness wait is needed — "prism agent-run" runs immediately
	// and the sidecar does not write a ready file for bwrap sessions.
	// opts.SessionName must be set by the caller before setupFullLayout is
	// invoked — both ensureAndSwitch and restoreProjectSession do this.
	// BuildAgentCmd uses it to prefix PRISM_SESSION_NAME for the plugin.
	//
	// Propagate the worktree path into opts so buildDirectAgentCmd (host
	// mode) can resolve the encoded-cwd component of the pi sessions
	// directory for conversation resume (issue #1838). Other isolation
	// modes ignore opts.Worktree — their `prism agent-run` dispatch reads
	// the worktree from the DB row instead.
	opts.Worktree = directory
	agentCmd, err := BuildAgentCmd(opts)
	if err != nil {
		return fmt.Errorf("setupFullLayout: build agent command for %q: %w", name, err)
	}

	// Persist isolation_mode BEFORE opening the agent window. This is the
	// critical ordering fix: prism agent-run in window 1 reads isolation_mode
	// from agent_status immediately on start. If we write isolation_mode only
	// after the window exists (as the old post-ensureAndSwitch block in
	// cmd/spawn.go did), prism agent-run races and sees NULL → dies with a
	// mode mismatch error. Writing here, synchronously before NewWindow,
	// removes the race entirely. See issue #894.
	if mode != "" {
		// Always write isolation_mode when we have a non-empty mode.
		if d, dbErr := openDB(); dbErr == nil {
			if setErr := d.SetIsolationMode(name, mode); setErr != nil {
				fmt.Fprintf(os.Stderr, "warning: setupFullLayout: set isolation_mode for %q: %v\n", name, setErr)
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
	//
	// The agent window IS the session — if this call fails, the session has
	// no usable pane and callers that later WaitForReady will silently time
	// out after 30s with no diagnostic. Surface the error so SpawnSession's
	// layoutErr cleanup path (KillSidecar + cleanupHalfAliveSession +
	// tmux.KillSession + spawn_failed event) runs instead. Previously this
	// call discarded its error, which is the reason issue #2510 presents as
	// a bare "not ready within 30s" timeout rather than a clear message —
	// whatever the underlying trigger turns out to be, the discarded error
	// is what hides it.
	if err := tmux.NewWindow(name, 1, "agent", directory, agentCmd, agentPaneEnvVars(opts)); err != nil {
		return fmt.Errorf("setupFullLayout: create agent window for %q: %w", name, err)
	}

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

	// Cosmetic windows below: the term window and window-selection are
	// convenience-only. Their failure does not break the session — the
	// agent window (created above with a checked error) is the load-bearing
	// pane. A missing term window means the user has to open one
	// themselves; a failed SelectWindow leaves the client on window 0
	// (edit) instead of window 1 (agent). Neither warrants tearing down a
	// working session, so the errors here are deliberately discarded.
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
//
// Sanitisation: tmux uses "." as the session.window.pane target separator, ":" as
// another separator, and whitespace as a token boundary. All three are replaced
// with "_" to produce a safe component. "/" is replaced with "--" to preserve
// the existing convention for branch hierarchies.
func worktreeBranchComponent(dir string) string {
	ref, err := git.SymbolicRef(dir)
	if err == nil {
		branch := strings.TrimPrefix(ref, "refs/heads/")
		return sanitiseBranchComponent(branch)
	}
	if hash, err := git.ShortHash(dir); err == nil {
		return sanitiseBranchComponent(hash)
	}
	return sanitiseBranchComponent(filepath.Base(dir))
}

// sanitiseBranchComponent replaces characters that are unsafe in tmux session
// names with safe substitutes:
//   - "/" → "--"  (preserve existing convention for branch hierarchies)
//   - "." → "_"   (tmux uses "." as session.window.pane separator)
//   - ":" → "_"   (tmux uses ":" as a target separator)
//   - whitespace → "_"
func sanitiseBranchComponent(s string) string {
	s = strings.ReplaceAll(s, "/", "--")
	s = strings.ReplaceAll(s, ".", "_")
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "\t", "_")
	return s
}
