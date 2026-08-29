package session

// SpawnSession is the single shared primitive for creating a prism session
// end-to-end. Both `prism spawn` (cmd/spawn.go → ensureAndSwitch) and
// `prism review` (internal/review/review.go per-agent loop) compose over it.
//
// Design:
//
//   - A prism session is the fundamental primitive. A spawn-style command is
//     an abstraction over the primitive. The primitive is uniform; the
//     abstractions are allowed to be rich and divergent.
//
//   - SpawnSession contains NO branching on session-type strings. All variant
//     behaviour flows through SpawnOpts fields (Layout, WorktreeReadOnly,
//     IsolationMode, GroupID, …). If a branch here feels unavoidable, first
//     ask whether it should be a new SpawnOpts field.
//
//   - root_agent_name is written at spawn time from opts.AgentRole — no NULL
//     window.
//
//   - group_id is written from opts.GroupID when non-empty. It is a no-op for
//     single-session spawns (spawn/pr).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// spawnMu serialises concurrent SpawnSession calls for the same session name.
//
// Implementation choice: in-process advisory lock (sync.Map[string]*sync.Mutex).
// This handles the realistic case where all SpawnSession callers (spawn, review
// fan-out) share one process. It does NOT protect against two separate prism
// binaries racing on the same session name — that is a cross-process race that
// would require a DB-side conditional UPDATE. For
// the current usage pattern (all callers in one coordinator process) in-process
// serialisation is sufficient and is the simpler implementation.
//
// Scope: the entire SpawnSession prologue (seed → group_id → instance_id →
// InsertSession → AllocatePort) is protected by the lock for a given session
// name. Releasing the lock after AllocatePort (but before the layout spawner
// runs) is safe: once instance_id and the sessions row are written and the port
// is allocated, the DB state is consistent and idempotent operations from
// concurrent callers (UpsertStatusSeedRootAgentName, InsertSession via INSERT OR
// IGNORE) become no-ops.
var spawnMu sync.Map // key: sessionName, value: *sync.Mutex

// spawnLock returns the per-session-name mutex, creating it if necessary, and
// locks it. The caller must call the returned unlock function when the critical
// section is complete.
func spawnLock(sessionName string) (unlock func()) {
	v, _ := spawnMu.LoadOrStore(sessionName, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// SpawnOpts describes a single session to create end-to-end.
// All fields are available regardless of agent type — SpawnSession does not
// branch on the value of AgentRole (or any other string identifier).
type SpawnOpts struct {
	// SessionName is the canonical tmux/prism session name. Required.
	SessionName string

	// Repo is the short repo name, e.g. "nixos-config". Written to
	// agent_status.repo on initial insert only. May be empty for sessions
	// outside a worktree.
	Repo string

	// Worktree is the absolute path to the directory the session should run
	// in. Required.
	Worktree string

	// AgentRole is the agent identity (e.g. "coordinator", "worker",
	// "review-code"). Written to agent_status.root_agent_name at spawn time
	// so the DB row reflects the agent type from the first moment.
	AgentRole string

	// Prompt is the initial prompt delivered to the agent on startup.
	// Passed via the agent's --prompt CLI flag (host mode) or via the sidecar
	// (container/bwrap mode).
	Prompt string

	// PromptSource is the C.4.SRC discriminator written to
	// spawn_inputs.prompt_source. Values: "cli-positional", "cli-stdin",
	// "proxy-spawn", "review-fanout", or "" (NULL) when not applicable.
	// Set by cmd/spawn.go for CLI spawns; set directly by review fan-out.
	PromptSource string

	// PromptTemplateHash is the C.4.PT identifier written to
	// spawn_inputs.prompt_template_hash. For review fan-out this is
	// "review-fanout:<sha>" where <sha> is the build-time SHA of
	// internal/review/review.go (embedded via -ldflags). For all other
	// spawn paths this is "" (NULL), because the prompt is a free-form
	// user-supplied string with no fixed template.
	PromptTemplateHash string

	// PromptFilePath is set internally by SpawnSession when it has written
	// the prompt to the per-session run directory. Callers
	// should leave this empty; SpawnSession populates it from opts.Prompt
	// before the layout-specific spawn runs.
	//
	// For LayoutFull + host: BuildAgentCmd emits `--prompt "$(cat
	// <path>)"` rather than inlining the prompt body.
	//
	// For LayoutAgentOnly + bwrap/sandbox-exec: spawnAgentPaneEnvVars sets
	// PRISM_INITIAL_PROMPT_FILE=<path> on the agent pane and `prism
	// agent-run` reads the file.
	//
	// Carried on SpawnOpts (rather than as a function-local variable) so
	// the layout spawners — which receive a copy — see the same value
	// the size guard validated against.
	PromptFilePath string

	// Layout selects the tmux window layout created for this session.
	//   - LayoutFull:       3-window layout (edit / agent / term) — spawn path
	//   - LayoutAgentOnly:  2-window layout (shell / agent)       — review path
	Layout Layout

	// IsolationMode is the resolved isolation mode for this session.
	// Valid values: "bwrap", "sandbox-exec", "host" (see
	// config.ValidIsolationModes).
	IsolationMode string

	// PluginHostPath is the host-side path to the agent plugin. Retained
	// for back-compat; no current isolation mode consumes it.
	PluginHostPath string

	// InstanceID is the UUID for this session incarnation. For LayoutFull,
	// SpawnSession auto-generates one when this field is empty and writes it
	// to agent_status before the sidecar starts. For LayoutAgentOnly, the
	// field is passed through verbatim — the sidecar and tmux-session-start
	// hook will generate one downstream if it remains empty.
	InstanceID string

	// WorktreeReadOnly, when true, mounts the worktree read-only inside the
	// container. Set for review agents so they cannot modify the branch
	// under review.
	WorktreeReadOnly bool

	// GroupID, when non-empty, associates this session with a SessionGroup
	// (see db.RegisterGroup). Written to agent_status.group_id. SpawnSession
	// writes it when present but does not create the group itself.
	GroupID string

	// AgentEnvVars are additional env vars prefixed to the agent command
	// in host-mode sessions (see profiles.json agent_env_vars). Ignored in
	// bwrap/sandbox-exec mode — those paths deliver env vars via their own
	// injection mechanisms (bwrap --setenv, sandbox-exec profile).
	AgentEnvVars map[string]string

	// ForceFresh controls the startup guard for LayoutFull. When true, any
	// existing tmux session with the same name is killed before the new
	// session is created. When false, a live existing session is preserved
	// and SpawnSession is a no-op. For LayoutAgentOnly this field is ignored
	// — review agent sessions always create fresh.
	ForceFresh bool

	// Headless, when true, signals to callers that no tmux client switch
	// should happen after the session is created. SpawnSession itself does
	// not call Attach; this field is carried through for composition with
	// higher-level wrappers.
	Headless bool

	// RuntimeEnvVars holds harness-specific environment variables to
	// inject into host-mode sessions. Populated from
	// harness.Harness.RuntimeEnv() by callers that have a harness
	// instance. These are prepended outermost (before AgentEnvVars and
	// PRISM_SESSION_NAME) in the agent command string.
	RuntimeEnvVars map[string]string

	// HarnessName is the registered harness name (e.g. "pi"). When
	// non-empty it is forwarded to the sidecar via --harness so the sidecar
	// can call harness.ShapeOf to determine its own transport shape.
	HarnessName string

	// HarnessPipeSockPath is the Unix socket path for the PI harness pipe
	// (TransportSocketPipe). When non-empty and isolation mode is "host",
	// agentPaneEnvVars injects PRISM_HARNESS_PIPE=unix://<path> into the
	// agent tmux pane. Pre-computed by the caller (cmd/spawn.go) from
	// SidecarHarnessPipePath so that the spawner and sidecar agree on the
	// path deterministically without a round-trip through the sidecar process.
	// Empty for non-socket-pipe harnesses (e.g. HTTP-based ones).
	HarnessPipeSockPath string

	// ModelsByRole is the per-role model override map. When non-empty it is
	// forwarded to the sidecar via repeated --model-override flags so the
	// harness adapter reports the per-role model, and to Opts.ModelsByRole so
	// the entry for AgentRole selects the model pi runs on.
	ModelsByRole map[string]string

	// ReadinessTimeout, when > 0, causes SpawnSession to gate its return on
	// the agent reaching a readiness signal in the prism DB (see
	// WaitForReady). If the gate trips with a timeout, SpawnSession cleans
	// up the half-alive session (kill sidecar, release port, end DB row,
	// kill tmux session) and returns a *ReadinessTimeoutError.
	//
	// Zero means SpawnSession returns as soon as tmux/sidecar are kicked
	// off, leaving the readiness check to the caller. The parallel review
	// fan-out uses zero here and runs WaitForReady in per-agent goroutines
	// after the spawn loop completes — that way one slow agent does not
	// delay the others.
	//
	// Single-worker `prism spawn` sets this to DefaultReadinessTimeout so
	// operators see a clear `failed to start: not ready within 30s` message
	// instead of a "session created" success line followed by an idle
	// session that will never make progress.
	ReadinessTimeout time.Duration

	// AllowEmptyPrompt opts the caller out of the empty-prompt guard for
	// LayoutFull / LayoutAgentOnly. The tmux Prefix+a keybind invokes
	// `prism spawn --attach` with no --prompt so the operator can type the
	// initial prompt to the live agent after the popup attaches; `cmd/spawn.go`
	// sets this field when `PRISM_KEYBIND_SPAWN` is present (the dedicated
	// keybind sentinel). All other callers should leave this false so the
	// "agent pane needs a prompt" guard keeps firing.
	AllowEmptyPrompt bool

	// PIExtensionDir is the host-side absolute path to the directory that
	// contains the prism PI extension file. Forwarded to Opts.PIExtensionDir
	// so buildDirectAgentCmd can emit --extension <dir>/prism.ts on the
	// host-mode pi launch. Empty value falls back to no --extension flag on
	// host mode.
	PIExtensionDir string

	// Model, when non-empty, is the CLI-supplied model override from
	// `prism spawn --model <X>`. Forwarded to Opts.Model and (for bwrap /
	// sandbox-exec) into the AgentPaneOpts that builds the `prism
	// agent-run` tmux pane command.
	Model string

	// Variant, when non-empty, is the CLI-supplied variant override from
	// `prism spawn --variant <Y>`. Semantics mirror Model.
	Variant string

	// Provider, when non-empty, is the CLI-supplied routing-provider override
	// from `prism spawn --provider <P>`. Semantics mirror Model, with one
	// addition: every emit site is gated on a pi harness name.
	Provider string

	// Note: ModelFlag / VariantFlag below are distinct from Model / Variant
	// above. Model / Variant feed harness-routing (the agent-run launch argv);
	// ModelFlag / VariantFlag feed the audit row written to spawn_inputs.
	// In `prism spawn` they carry the same string — keeping them as separate
	// fields lets the audit shape evolve independently of launch-time semantics.

	// ── spawn_inputs audit fields ────────────────────────────────────────
	//
	// These mirror the CLI flag values passed at spawn time and are written
	// to the spawn_inputs table by SpawnSession. Each front door
	// (`prism spawn`, `prism pr`, `prism investigate`, `prism review`) populates
	// the fields it knows about; SpawnSession is the single chokepoint that
	// inserts the row. Pointer fields are nullable in the DB — leave the
	// matching SpawnOpts field as its zero value when the flag was not passed.

	// ProfileName is the resolved active profile name (e.g. "anthropic").
	// Mirrors spawn_inputs.profile_name. Empty when no profile is active.
	ProfileName string

	// ModelFlag is the raw --model flag value (e.g. "anthropic/claude-sonnet-4-7").
	// Mirrors spawn_inputs.model_flag.
	ModelFlag string

	// VariantFlag is the raw --variant flag value (e.g. "high").
	// Mirrors spawn_inputs.variant_flag.
	VariantFlag string

	// ProviderFlag is the raw --provider flag value (e.g. "openrouter").
	// Mirrors spawn_inputs.provider_flag.
	ProviderFlag string

	// AgentFlag is the raw --agent flag value as the user passed it
	// (distinct from AgentRole, which is the resolved role written to
	// agent_status.root_agent_name). Mirrors spawn_inputs.agent_flag.
	AgentFlag string

	// HarnessFlag is the raw --harness flag value (e.g. "pi").
	// Mirrors spawn_inputs.harness_flag.
	HarnessFlag string

	// IsolationFlag is the raw --isolation flag value as the user passed
	// it (distinct from IsolationMode, which is the resolved mode used at
	// launch). Mirrors spawn_inputs.isolation_flag.
	IsolationFlag string

	// HostModeFlag mirrors --host-mode. Mirrors spawn_inputs.host_mode_flag.
	HostModeFlag bool

	// ContainersFlag mirrors --containers. Audit-only:
	// SpawnSession writes it to spawn_inputs.containers_flag and also flips
	// agent_status.containers_enabled to 1 when true so the sidecar starts
	// the per-session filtering podman socket proxy. Defaults to false (the
	// flag was omitted; the runtime gate stays at the schema default of 0).
	ContainersFlag bool

	// PRNumber is the parsed --pr flag value. Zero means no --pr was passed.
	// Mirrors spawn_inputs.pr_number.
	PRNumber int

	// BranchFlag is the raw --branch flag value.
	// Mirrors spawn_inputs.branch_flag.
	BranchFlag string

	// IgnoreConcurrencyCap mirrors --ignore-concurrency-cap.
	// Mirrors spawn_inputs.ignore_concurrency_cap.
	IgnoreConcurrencyCap bool

	// SkillsManifestHash is the C4.SK skills directory hash computed at spawn
	// time. Mirrors spawn_inputs.skills_manifest_hash.
	SkillsManifestHash string

	// AgentPromptHash is the C4.AP agent role file hash computed at spawn
	// time. Mirrors spawn_inputs.agent_prompt_hash.
	AgentPromptHash string

	// AbtestPairID is the shared UUID minted at the spawn-call site when
	// --abtest is used. Both sibling sessions receive the same value so the
	// rows pair via spawn_inputs.abtest_pair_id.
	AbtestPairID string

	// InvokerSession, when non-empty, is the session name of the caller that
	// invoked the spawn — the coordinator running `prism spawn`, the worker
	// running `prism investigate`, the worker running `prism review`, or the
	// invoker whose sidecar host-API /investigate or /spawn handler shelled
	// out to the CLI. It is used by SpawnSession to:
	//
	//   - populate the `from_session` column of the durable
	//     `session.spawn_intent` / `session.spawn_failed` agent_events
	//     payloads written for every spawn attempt;
	//   - address a best-effort `bus_messages` audit row to the invoker on
	//     the failure paths so the caller has a forensic breadcrumb even when
	//     the sidecar hop between them and SpawnSession swallowed the error.
	//
	// Optional: bare `prism spawn` invocations outside a prism session leave
	// this empty. SpawnSession then writes the durable event rows without the
	// invoker field and skips the bus_messages write entirely.
	InvokerSession string
}

// SpawnSession creates a single prism session end-to-end: seeds the
// agent_status row (including root_agent_name and, when set, group_id),
// allocates a port, creates the tmux session, and starts the sidecar.
//
// This is the single entry point for session creation. Both `prism spawn` and
// `prism review`'s per-agent loop compose over it; no other callers should
// call db.AllocatePort + tmux.NewSessionDetached + StartSidecarWithOpts
// directly.
//
// Ordering:
//
//  1. DB seed (UpsertStatusSeedRootAgentName) — writes root_agent_name
//     immediately so the DB row reflects agent identity from the first
//     moment.
//  2. Group-id write (when opts.GroupID != "") — SetGroupID on the same row.
//  3. Port allocation (db.AllocatePort).
//  4. Tmux session + agent window creation (via Create for LayoutFull, or
//     inline for LayoutAgentOnly).
//  5. Sidecar startup — handled inside setupFullLayout for LayoutFull, and
//     called directly here for LayoutAgentOnly.
func SpawnSession(d *db.DB, opts SpawnOpts) error {
	if d == nil {
		return fmt.Errorf("spawn session: db handle is required")
	}
	if opts.SessionName == "" {
		return fmt.Errorf("spawn session: SessionName is required")
	}
	if opts.Worktree == "" {
		return fmt.Errorf("spawn session: Worktree is required")
	}
	// Reject an empty prompt for layouts that require one. LayoutFull and
	// LayoutAgentOnly host an agent pane and need a
	// prompt to drive the agent; without one the session is created
	// successfully but the agent sits idle forever. LayoutBare and
	// LayoutScratchpad are not agent panes — they are plain shells/dashboards
	// — and legitimately have no prompt, so they must continue to accept
	// an empty opts.Prompt.
	//
	// opts.AllowEmptyPrompt opts the caller out of this guard — used by the
	// keybind carve-out where the operator types the initial
	// prompt to the live agent after the popup attaches.
	if opts.Prompt == "" && !opts.AllowEmptyPrompt && (opts.Layout == LayoutFull || opts.Layout == LayoutAgentOnly) {
		return fmt.Errorf("spawn session: Prompt is required for layout %d (LayoutFull or LayoutAgentOnly) — an agent pane cannot start without a prompt", opts.Layout)
	}

	// Fail-fast guard for the host-mode pi launch.
	// Mirrors the container-path guard at cmd/agent_run.go:730: refuse to
	// spawn rather than silently launch pi without --extension. Container
	// modes (bwrap / sandbox-exec) route through PIInvocation which has its
	// own equivalent check; this guard is host-mode only and is suppressed
	// for non-pi harnesses (the prism extension is pi-specific).
	//
	// LayoutBare and LayoutScratchpad never reach here — SpawnSession is
	// only called for LayoutFull and LayoutAgentOnly. Both of those layouts
	// launch an agent pane and therefore require the extension to be wired.
	//
	// The validator reads IsolationMode via effectiveIsolationMode, which
	// defaults empty to "host". Use resolveLayoutIsolationMode here so
	// LayoutAgentOnly callers that leave IsolationMode empty (the review
	// fan-out shape on machines whose default is bwrap) are correctly
	// resolved to the machine default before the host check fires — same
	// resolution path the actual launch uses below at spawnAgentOnlyLayout.
	validationOpts := Opts{
		IsolationMode:  resolveLayoutIsolationMode(opts),
		HarnessName:    opts.HarnessName,
		PIExtensionDir: opts.PIExtensionDir,
	}
	if err := ValidatePILaunchOpts(validationOpts); err != nil {
		return fmt.Errorf("spawn session %q: %w", opts.SessionName, err)
	}

	// Open the per-session startup log as the very first step. Doing this
	// before any other work means the per-session run directory exists from
	// the moment the spawn begins, so any later failure has a place to leave
	// breadcrumbs — even when `prism agent-run` never reaches its own log-open
	// call.
	startup := openStartupLog(opts.SessionName)
	defer startup.close()
	startup.log("spawn-session: begin (role=%q, worktree=%q, layout=%d, isolation=%q)",
		opts.AgentRole, opts.Worktree, opts.Layout, opts.IsolationMode)

	// With a non-empty prompt, write the prompt to the per-session run
	// directory and let the launch path reference it by path rather than
	// inlining the prompt body into the tmux command.
	//
	//   - LayoutFull + host: buildDirectAgentCmd emits
	//     `--prompt "$(cat <path>)"` so tmux's `sh -c <cmd>` stays small.
	//   - LayoutFull / LayoutAgentOnly + bwrap or sandbox-exec: carrying the
	//     prompt as `-e PRISM_INITIAL_PROMPT=<huge>` on the tmux new-window
	//     argv can push role-prompt + boilerplate + bind paths past tmux's
	//     command size budget and produce a `command too long` failure. A
	//     `-e PRISM_INITIAL_PROMPT_FILE=<path>` env var keeps the launch
	//     command's size O(1) in prompt size; `prism agent-run` reads the file
	//     when it sees the env var and feeds the contents to the
	//     bwrap/sandbox-exec --prompt path.
	//
	// All modes (host and sandbox) write the prompt file regardless of
	// layout — see the needsPromptFile gate below.
	mode := resolveLayoutIsolationMode(opts)
	var promptFilePath string
	isSandbox := mode == "bwrap" || mode == "sandbox-exec"
	// needsPromptFile is true for every mode/layout combination that carries a
	// non-empty prompt. The invariant is universal: **every** mode/layout
	// combination with a non-empty prompt uses the file path, including
	// LayoutAgentOnly+host (the Darwin coordinator's review fan-out path). A
	// narrower gate that excluded that cell would let spawnAgentPaneEnvVars
	// fall back to the inline PRISM_INITIAL_PROMPT=<blob> env var and
	// re-introduce the HostLaunchCmdSafeBound failure. LayoutBare and
	// LayoutScratchpad never carry prompts (those layouts are for shells and
	// dashboards, not agent panes), so the `opts.Prompt != ""` guard already
	// excludes them without needing an explicit carve-out.
	needsPromptFile := opts.Prompt != "" && (mode == "host" || isSandbox)
	if needsPromptFile {
		path, writeErr := WriteInitialPrompt(opts.SessionName, opts.Prompt)
		if writeErr != nil {
			// Prompt-delivery setup failures must surface to the operator
			// instead of being swallowed. Bail out before any
			// tmux state is created so a re-spawn after fixing the
			// underlying issue (e.g. disk full) starts from a clean slate.
			startup.log("spawn-session: write initial-prompt FAILED: %v", writeErr)
			return fmt.Errorf("spawn session: write initial prompt: %w", writeErr)
		}
		promptFilePath = path
		startup.log("spawn-session: wrote initial-prompt file (%d bytes) to %s", len(opts.Prompt), path)
	}

	// Pre-spawn size check. Build the launch command preview using the same
	// Opts shape that the layout spawner will reuse below, and reject any
	// session whose tmux-bound command exceeds
	// the safe bound. Doing this BEFORE any tmux state is created means a
	// rejected spawn has no observable side effects on the tmux server.
	//
	// Four combinations are guarded:
	//
	//   - LayoutFull + host: BuildAgentCmd returns the direct agent
	//     invocation. Without the prompt-file plumbing the prompt body
	//     would be inlined onto `sh -c <cmd>`; with it the cmd stays O(1)
	//     in prompt size.
	//
	//   - LayoutAgentOnly + host: review agents on Darwin coordinators run in
	//     host mode. This cell is covered by the unified gate so it cannot
	//     fall back to PRISM_INITIAL_PROMPT inline delivery with its
	//     HostLaunchCmdSafeBound failure mode.
	//
	//   - LayoutFull / LayoutAgentOnly + bwrap or sandbox-exec:
	//     BuildAgentCmd returns `prism agent-run --session <name>`,
	//     but the env-var map (which becomes `-e KEY=VALUE` flags on
	//     tmux new-window) can carry the entire prompt. The "size" measured
	//     here adds the env-var contribution so the guard reflects the bytes
	//     tmux actually sees on its argv.
	hostLaunchCmdSize := 0
	switch {
	case mode == "host" && opts.Layout == LayoutFull:
		previewOpts := buildOptsForLayout(opts, 0, promptFilePath)
		hostLaunchCmd, buildErr := BuildAgentCmd(previewOpts)
		if buildErr != nil {
			removeInitialPrompt(opts.SessionName)
			return fmt.Errorf("spawn session: build host launch command for %q: %w", opts.SessionName, buildErr)
		}
		hostLaunchCmdSize = len(hostLaunchCmd)
		if hostLaunchCmdSize > HostLaunchCmdSafeBound {
			startup.log("spawn-session: host launch command size %d exceeds safe bound %d — rejecting before tmux state is created",
				hostLaunchCmdSize, HostLaunchCmdSafeBound)
			// Best-effort: remove the prompt file we just wrote so a
			// subsequent retry doesn't see a stale file under a recycled
			// session name.
			removeInitialPrompt(opts.SessionName)
			return &HostLaunchCmdTooLargeError{
				SessionName: opts.SessionName,
				CmdSize:     hostLaunchCmdSize,
				SafeBound:   HostLaunchCmdSafeBound,
			}
		}
		startup.log("spawn-session: host launch command size %d (safe bound %d, warn threshold %d)",
			hostLaunchCmdSize, HostLaunchCmdSafeBound, HostLaunchCmdWarnThreshold)

	case mode == "host" && opts.Layout == LayoutAgentOnly:
		// LayoutAgentOnly + host: Darwin coordinator review fan-out.
		// The agent command is `prism agent-run --session <name>` in bwrap
		// and sandbox-exec, but for host mode it is a direct agent
		// invocation — same as LayoutFull + host but without the sidecar-
		// managed 3-window layout. The env-var map carries
		// PRISM_INITIAL_PROMPT_FILE; measure the full contribution so any exotic
		// prompt or large session name is caught here.
		previewEnvs := spawnAgentPaneEnvVars(SpawnOpts{
			Prompt:         opts.Prompt,
			PromptFilePath: promptFilePath,
		})
		size := 0
		for k, v := range previewEnvs {
			size += len(k) + len(v) + 4
		}
		// The host-mode LayoutAgentOnly command is built inside
		// spawnAgentOnlyLayout; route through the same builder so this
		// preview carries every field the real launch does.
		previewSpawnOpts := opts
		previewSpawnOpts.PromptFilePath = promptFilePath
		previewOpts := buildOptsForAgentOnlyLayout(previewSpawnOpts, 0, mode)
		agentOnlyCmd, buildErr := BuildAgentCmd(previewOpts)
		if buildErr != nil {
			removeInitialPrompt(opts.SessionName)
			return fmt.Errorf("spawn session: build host/LayoutAgentOnly launch command for %q: %w", opts.SessionName, buildErr)
		}
		size += len(agentOnlyCmd)
		hostLaunchCmdSize = size
		if size > HostLaunchCmdSafeBound {
			startup.log("spawn-session: host/LayoutAgentOnly launch command size %d exceeds safe bound %d — rejecting before tmux state is created",
				size, HostLaunchCmdSafeBound)
			removeInitialPrompt(opts.SessionName)
			return &HostLaunchCmdTooLargeError{
				SessionName: opts.SessionName,
				CmdSize:     size,
				SafeBound:   HostLaunchCmdSafeBound,
			}
		}
		startup.log("spawn-session: host/LayoutAgentOnly launch command size %d (safe bound %d)",
			size, HostLaunchCmdSafeBound)

	case isSandbox && (opts.Layout == LayoutFull || opts.Layout == LayoutAgentOnly):
		// Mirror what the layout spawner will hand to tmux: the agentCmd
		// (a small `prism agent-run --session <name>` for the sandbox
		// modes) plus each `-e KEY=VALUE` env entry. previewOpts is the
		// minimal shape BuildAgentCmd needs for the sandbox mode.
		previewOpts := Opts{
			Prompt:        opts.Prompt,
			Agent:         opts.AgentRole,
			SessionName:   opts.SessionName,
			Port:          0,
			IsolationMode: mode,
			Model:         opts.Model,
			Variant:       opts.Variant,
			Provider:      opts.Provider,
		}
		previewCmd, buildErr := BuildAgentCmd(previewOpts)
		if buildErr != nil {
			removeInitialPrompt(opts.SessionName)
			return fmt.Errorf("spawn session: build %s launch command for %q: %w", mode, opts.SessionName, buildErr)
		}
		// For the env-var preview, route through the same helper used at
		// spawn time so the file-path branch and the inline branch produce
		// matching size estimates.
		var previewEnvs map[string]string
		if opts.Layout == LayoutFull {
			previewEnvs = agentPaneEnvVars(Opts{
				Prompt:         opts.Prompt,
				PromptFilePath: promptFilePath,
				IsolationMode:  mode,
			})
		} else {
			previewEnvs = spawnAgentPaneEnvVars(SpawnOpts{
				Prompt:         opts.Prompt,
				PromptFilePath: promptFilePath,
			})
		}
		size := len(previewCmd)
		for k, v := range previewEnvs {
			// `-e KEY=VALUE` lands as two argv elements; approximate
			// without re-implementing tmux's quoting.
			size += len(k) + len(v) + 4
		}
		hostLaunchCmdSize = size
		if size > HostLaunchCmdSafeBound {
			startup.log("spawn-session: %s launch command size %d exceeds safe bound %d — rejecting before tmux state is created",
				mode, size, HostLaunchCmdSafeBound)
			removeInitialPrompt(opts.SessionName)
			return &HostLaunchCmdTooLargeError{
				SessionName: opts.SessionName,
				CmdSize:     size,
				SafeBound:   HostLaunchCmdSafeBound,
			}
		}
		startup.log("spawn-session: %s launch command size %d (safe bound %d)",
			mode, size, HostLaunchCmdSafeBound)
	}
	opts.PromptFilePath = promptFilePath

	// Serialise concurrent SpawnSession calls for the same session name.
	// Without this guard, two concurrent callers both execute the
	// non-atomic prologue (seed → instance_id → InsertSession → AllocatePort)
	// in parallel: SetInstanceID is an unconditional UPDATE so the last writer
	// wins; both InsertSession calls succeed (different PKs) leaving one orphan;
	// AllocatePort may pick the same port. The lock ensures only one caller runs
	// the prologue at a time.
	//
	// After acquiring the lock, we check whether the session is already alive
	// (agent_status row exists with a non-nil instance_id and ended_at IS NULL).
	// If it is, the second (and later) concurrent callers return an error
	// immediately without touching the DB — preserving the winning caller's
	// state. This is the minimal correct behaviour: the second caller was
	// redundant (the session already exists) and failing fast is safer than
	// letting it overwrite the winning caller's instance_id.
	//
	// In-process only: cross-process races (two prism binaries spawning the
	// same session name) are not protected here.
	unlockSpawn := spawnLock(opts.SessionName)
	defer unlockSpawn()

	// Post-lock: check for a live session so late arrivals fail fast without
	// corrupting the winner's DB state.
	if existing, checkErr := d.CurrentStatus(opts.SessionName); checkErr == nil && existing != nil {
		if existing.InstanceID != nil && *existing.InstanceID != "" && existing.EndedAt == nil {
			startup.log("spawn-session: session already alive (instance_id=%s) — concurrent duplicate rejected",
				*existing.InstanceID)
			return fmt.Errorf("spawn session: %q is already alive (instance_id=%s) — concurrent spawn rejected",
				opts.SessionName, *existing.InstanceID)
		}
	}

	// Step 1: Seed agent_status with root_agent_name and isolation_mode.
	// Idempotent; later writes by the sidecar and by tmux-session-start
	// COALESCE-preserve the values written here.
	//
	// Resolve effective harness for DB seeding. Pi is the sole harness;
	// if HarnessName is blank fall back to "pi".
	effectiveHarness := opts.HarnessName
	if effectiveHarness == "" {
		effectiveHarness = "pi"
	}
	// Pass the resolved isolation mode so the row is born with isolation_mode
	// set — eliminating the NULL window between seed and SetIsolationMode.
	// ActiveSessionCountForMode counts this row correctly from the moment the
	// seed returns.
	// Seed a fallback dashboard title from the spawn prompt. pi never
	// auto-generates a session title in any mode (see title_fallback.go for
	// the full diagnosis), so a headless spawned worker would otherwise never
	// show one.
	//
	// UpsertStatusSeedRootAgentName is an upsert: on a fresh session name it
	// INSERTs; on a re-spawn of a session name that was cleaned up but left
	// its agent_status row behind (the documented respawn-after-cleanup path
	// — internal/db/respawn_after_cleanup_test.go), it UPDATEs the
	// existing row via ON CONFLICT, and title = COALESCE(excluded.title,
	// title) there behaves exactly like the INSERT branch: a non-nil incoming
	// title always wins. A fallback derived fresh from the new prompt would
	// therefore silently clobber a real harness-reported or human-renamed
	// title left over from the row's previous life. Guard against that by
	// only deriving a fallback when the row currently has no title at all —
	// covers both the fresh-INSERT case (existing == nil) and a prior
	// incarnation that itself never got a title.
	var seedTitle *string
	existing, _ := d.CurrentStatus(opts.SessionName)
	if existing == nil || existing.Title == nil || *existing.Title == "" {
		if t := deriveFallbackTitle(opts.Prompt); t != "" {
			seedTitle = &t
		}
	}
	if err := d.UpsertStatusSeedRootAgentName(
		opts.SessionName, opts.Repo, opts.Worktree, "idle", seedTitle, nil, opts.AgentRole, effectiveHarness, mode,
	); err != nil {
		startup.log("spawn-session: seed status FAILED: %v", err)
		return fmt.Errorf("spawn session: seed status: %w", err)
	}
	startup.log("spawn-session: agent_status seeded (state=idle, isolation_mode=%q, title=%v)", mode, seedTitle != nil)

	// Step 2: Write group_id when set. Single-session spawns leave GroupID
	// empty and this is a no-op.
	if opts.GroupID != "" {
		if err := d.SetGroupID(opts.SessionName, opts.GroupID); err != nil {
			return fmt.Errorf("spawn session: set group_id: %w", err)
		}
	}

	// Step 2b: Flip agent_status.containers_enabled when --containers was
	// passed. This is the runtime gate the sidecar reads at
	// startup to decide whether to start the per-session filtering podman
	// API socket proxy. Only call when the flag is set — the column defaults
	// to 0 in the schema so an unset spawn never enables the proxy.
	//
	// The corresponding immutable audit value lives in
	// spawn_inputs.containers_flag and is written by InsertSpawnInputs below.
	if opts.ContainersFlag {
		if err := d.SetContainersEnabled(opts.SessionName, true); err != nil {
			return fmt.Errorf("spawn session: set containers_enabled: %w", err)
		}
		startup.log("spawn-session: agent_status.containers_enabled=1 (--containers)")
	}

	// FK race: mint instance_id host-side and pre-insert the sessions row
	// BEFORE the sidecar starts, so that the sidecar's first agent_events
	// writes (state_change, session_status, turn_start, …) can satisfy the
	// foreign-key constraint on agent_events.instance_id →
	// sessions(instance_id).
	//
	// Without this, the sessions row is inserted only by the tmux-session-start
	// event hook, which fires asynchronously when the agent window is created.
	// Under concurrent-spawn load the sidecar's mint-or-load-instance_id path
	// wins the race against the hook, the sidecar writes events keyed on an
	// instance_id that has no sessions row yet, every insert fails with
	// FOREIGN KEY constraint failed (787), and the agent's first ~30s of work
	// is silently dropped before the extension reconnect-timeout kills the
	// sidecar.
	//
	// So own the instance_id and the sessions-row insert here, host-side,
	// synchronously, before tmux/sidecar start. Both downstream minting paths
	// (the sidecar's startup-time mint and the tmux-session-start hook's
	// mint+InsertSession) are idempotent — they observe the existing
	// instance_id on agent_status and the existing sessions row (INSERT OR
	// IGNORE) and become safe no-ops.
	if opts.InstanceID == "" {
		opts.InstanceID = uuid.New().String()
		startup.log("spawn-session: minted instance_id %s host-side (#1507)", opts.InstanceID)
	}
	if setErr := d.SetInstanceID(opts.SessionName, opts.InstanceID); setErr != nil {
		// SetInstanceID is a plain UPDATE; the row was just seeded by
		// UpsertStatusSeedRootAgentName above so a zero-row UPDATE here
		// would be a real bug. Surface it.
		startup.log("spawn-session: SetInstanceID FAILED: %v", setErr)
		return fmt.Errorf("spawn session: set instance_id: %w", setErr)
	}
	{
		var agentRolePtr *string
		if opts.AgentRole != "" {
			agentRolePtr = &opts.AgentRole
		}
		var groupIDPtr *string
		if opts.GroupID != "" {
			groupIDPtr = &opts.GroupID
		}
		sessRow := db.Session{
			InstanceID:  opts.InstanceID,
			SessionName: opts.SessionName,
			AgentRole:   agentRolePtr,
			Repo:        opts.Repo,
			Worktree:    opts.Worktree,
			GroupID:     groupIDPtr,
			Harness:     effectiveHarness,
		}
		if insertErr := d.InsertSession(sessRow); insertErr != nil {
			// Pre-inserting the sessions row is the FK-race fix; if it
			// fails we cannot guarantee event writes will succeed. Surface
			// the error rather than letting the spawn proceed into the
			// racy state where events precede the sessions row.
			startup.log("spawn-session: InsertSession FAILED: %v", insertErr)
			return fmt.Errorf("spawn session: insert sessions row: %w", insertErr)
		}
		startup.log("spawn-session: sessions row pre-inserted (instance_id=%s)", opts.InstanceID)
	}

	// Durable spawn-intent event. Written after the sessions row
	// (the FK target for agent_events.instance_id) is committed and before
	// port allocation / layout work runs. Every spawn attempt that reaches
	// this point leaves an audit-log breadcrumb, so failed spawns are no
	// longer forensically invisible. This is the chokepoint for all front
	// doors (CLI spawn, investigate, review fan-out, host-API /spawn) —
	// handler-side writes would miss the CLI path. Best-effort: a write
	// failure here is logged but never aborts the spawn (telemetry must not
	// break the happy path).
	writeSpawnIntentEvent(d, opts, startup)

	// Write spawn_inputs row. SpawnSession is the single chokepoint:
	// every front door (`prism spawn`, `prism pr`, `prism investigate`,
	// `prism review`) populates the audit fields on SpawnOpts and the row is
	// written here, after the sessions row has been pre-inserted (FK target)
	// and before any layout/sidecar side-effects. Non-fatal: spawn_inputs is
	// best-effort telemetry; a failure here must never block session creation.
	//
	// The write happens for every Layout. INSERT OR IGNORE means a duplicate
	// call (e.g. from a caller that has not yet migrated) is silently dropped
	// rather than overwriting the row — the first writer wins, and since this
	// is the canonical writer it always wins.
	si := spawnInputsFromOpts(opts)
	if insertErr := d.InsertSpawnInputs(si); insertErr != nil {
		fmt.Fprintf(os.Stderr,
			"warning: could not write spawn_inputs for %q: %v\n",
			opts.SessionName, insertErr)
	}

	// Step 3: Allocate a port from the configured range. Fails fast if the
	// allocation fails — a session with no port cannot start the agent.
	port, err := d.AllocatePort(opts.SessionName)
	if err != nil {
		startup.log("spawn-session: allocate port FAILED: %v", err)
		return fmt.Errorf("spawn session: allocate port: %w", err)
	}
	startup.log("spawn-session: allocated port %d", port)

	// Step 4 & 5: Create the tmux session and start the sidecar. Both
	// layouts share the same responsibilities — only the window shape and
	// the ownership of the sidecar-start call differ.
	var layoutErr error
	switch opts.Layout {
	case LayoutFull:
		layoutErr = spawnFullLayout(d, opts, port)
	case LayoutAgentOnly:
		layoutErr = spawnAgentOnlyLayout(opts, port)
	default:
		startup.log("spawn-session: unsupported layout %d", opts.Layout)
		return fmt.Errorf("spawn session: unsupported layout %d", opts.Layout)
	}
	if layoutErr != nil {
		startup.log("spawn-session: layout setup FAILED: %v", layoutErr)
		// Auto-clean on layout failure to match the readiness-timeout path.
		// Both failure modes leave the same residue (pre-inserted sessions
		// row, allocated port, prompt file, possibly a partial sidecar process);
		// cleaning here removes operator toil and prevents stale DB rows from
		// blocking a retry with the same session name.
		//
		// Cleanup primitive set (same as readiness-timeout path above):
		//   - KillSidecar:              stops any sidecar that was started before
		//                               the tmux step failed (spawnAgentOnlyLayout
		//                               starts the sidecar first).
		//   - cleanupHalfAliveSession:  marks agent_status ended, releases the
		//                               port, purges bus messages, and writes
		//                               sessions.ended_at.
		//   - tmux.KillSession:         removes any partial tmux session created
		//                               before the failure (e.g. NewSessionDetached
		//                               succeeded but NewWindow failed).
		//   - removeInitialPrompt:      drops the per-session prompt file so a
		//                               retry with the same name starts fresh.
		KillSidecar(opts.SessionName)
		cleanupHalfAliveSession(d, opts.SessionName, opts.InstanceID, db.ReapCauseSpawnFailure)
		_ = tmux.KillSession(opts.SessionName)
		removeInitialPrompt(opts.SessionName)
		// Durable failure event + best-effort bus-message notification to
		// the invoker. Written AFTER cleanupHalfAliveSession so
		// the bus-message row survives — cleanupHalfAliveSession calls
		// PurgeBusMessages(sessionName) which would otherwise delete any
		// undelivered row with from_session = opts.SessionName. The audit
		// row for the failure is more useful than the DB-consistency signal
		// PurgeBusMessages provides (which is aimed at pre-existing queued
		// prompts, not post-hoc failure audit). agent_events writes are
		// unaffected by the cleanup and stay in chronological order. Both
		// writes are best-effort — a telemetry failure must not mask the
		// layout error the operator needs to see.
		writeSpawnFailedEvent(d, opts, "layout", layoutErr, startup)
		return fmt.Errorf("%w — to remove side effects run: prism cleanup --yes --session %s",
			layoutErr, opts.SessionName)
	}
	startup.log("spawn-session: tmux session and sidecar kicked off — handing control to agent pane (further bwrap stderr in agent-run.log)")

	// Step 6: readiness gate. When the caller opted in by
	// setting opts.ReadinessTimeout > 0, block here until the sidecar
	// observes the first SSE event from the agent (i.e. the agent actually
	// bound its port and the sidecar connected). On timeout, clean up the
	// half-alive session so a second spawn attempt with the same name does
	// not see stale state, and surface a *ReadinessTimeoutError so callers
	// can render a "<role> failed to start: not ready within <timeout>"
	// message instead of the optimistic "session created" line.
	//
	// Callers that prefer to run the gate themselves (e.g. the parallel
	// review fan-out in internal/review/review.go) leave ReadinessTimeout=0
	// and call WaitForReady directly in goroutines, so per-agent gates run
	// concurrently and one slow agent does not delay the others.
	if opts.ReadinessTimeout > 0 {
		// Lost-prompt guard: when the spawn carries an
		// initial prompt, raise the readiness bar so a bare
		// state_change->active (which fires on harness handshake even when
		// the prompt is lost between the spawn driver and the agent's
		// input queue) does not satisfy the gate. The strict path waits for
		// turn_start / msg_user / msg_assistant evidence that the agent is
		// actually processing the prompt. Without this, a lost-prompt spawn
		// returns success and leaves a silently-broken session—the most
		// dangerous failure mode the issue describes.
		requirePromptDelivered := opts.Prompt != ""
		if requirePromptDelivered {
			startup.log("spawn-session: waiting for readiness (timeout=%s, require prompt-delivered #1507)", opts.ReadinessTimeout)
		} else {
			startup.log("spawn-session: waiting for readiness (timeout=%s)", opts.ReadinessTimeout)
		}
		readyErr := WaitForReadyWithOpts(d, opts.SessionName, ReadinessOpts{
			Timeout:                opts.ReadinessTimeout,
			RequirePromptDelivered: requirePromptDelivered,
		})
		if readyErr != nil {
			startup.log("spawn-session: readiness gate FAILED: %v", readyErr)
			// Enrich the readiness-gate error when the host-mode
			// launch command was unusually large. The bare timeout message
			// ("not ready within 30s") leaves the operator without a hint
			// for why the agent never came up; for the size-driven failure
			// mode the cause is structural and predictable, so name it.
			if mode == "host" && hostLaunchCmdSize > HostLaunchCmdWarnThreshold {
				// errors.As (rather than a bare type assertion) so a future
				// refactor that wraps the readiness error keeps enrichment
				// firing — same shape IsReadinessTimeout uses.
				var rte *ReadinessTimeoutError
				if errors.As(readyErr, &rte) {
					rte.Hint = fmt.Sprintf(
						"host-mode launch command was %d bytes, above the typical safe range of %d bytes; "+
							"a prompt-size issue is a likely cause (see issue #1064)",
						hostLaunchCmdSize, HostLaunchCmdWarnThreshold,
					)
				}
			}
			// Clean up: the sidecar is still running and reporting
			// `connection refused` to its own log, but the agent never came
			// up. KillSidecar releases the sidecar process, the DB cleanup
			// releases the port and marks the row ended, and tmux.KillSession
			// releases the pane. All three are best-effort and idempotent.
			KillSidecar(opts.SessionName)
			cleanupHalfAliveSession(d, opts.SessionName, opts.InstanceID, db.ReapCauseReadinessGate, readyErr.Error())
			_ = tmux.KillSession(opts.SessionName)
			// Drop the per-session prompt file so a retry starts fresh.
			removeInitialPrompt(opts.SessionName)
			// Durable failure event + best-effort bus-message notification
			// to the invoker. Written AFTER cleanupHalfAliveSession
			// so the bus_messages row is not caught by that helper's purge
			// of undelivered messages for this session (from_session =
			// opts.SessionName). See the layout-failure branch above for the
			// full rationale. Both writes are best-effort.
			writeSpawnFailedEvent(d, opts, "readiness", readyErr, startup)
			return readyErr
		}
		startup.log("spawn-session: ready")
	}
	return nil
}

// resolveLayoutIsolationMode returns the resolved isolation mode for the
// SpawnOpts, mirroring the lookup in BuildAgentCmd / spawnAgentOnlyLayout.
// Kept as a small helper so SpawnSession can decide whether to write the
// initial-prompt file and whether to apply the launch-command size guard
// without duplicating the precedence logic.
//
// For LayoutAgentOnly, when IsolationMode is not set, fall back to the same
// machine default that spawnAgentOnlyLayout uses
// (config.Load().DefaultIsolationMode). This keeps the prompt-file gate
// aligned with the layout's actual mode — otherwise a caller that leaves
// IsolationMode empty for a bwrap-default machine would get the inline
// PRISM_INITIAL_PROMPT path here while spawnAgentOnlyLayout runs the agent
// under bwrap, re-introducing the command-size failure by another route.
func resolveLayoutIsolationMode(opts SpawnOpts) string {
	if opts.IsolationMode != "" {
		return opts.IsolationMode
	}
	if opts.Layout == LayoutAgentOnly {
		return string(config.Load().DefaultIsolationMode)
	}
	return "host"
}

// buildOptsForLayout returns the Opts struct that spawnFullLayout (and the
// pre-spawn size guard in SpawnSession) hands to BuildAgentCmd. Centralised
// here so the size-guard preview and the actual layout invocation cannot
// drift — both share the same constructed command.
//
// The size guard calls this with port=0 (the port is allocated only after
// the guard runs), while the real launch path passes the allocated port.
// The few-byte difference (--port <N> --hostname 127.0.0.1 contributes <30
// bytes) is well within the safe bound (16 KiB) and the 1 KiB warn
// threshold, so the guard's verdict cannot flip between preview and launch
// in practice.
func buildOptsForLayout(opts SpawnOpts, port int, promptFilePath string) Opts {
	return Opts{
		Prompt:              opts.Prompt,
		PromptFilePath:      promptFilePath,
		Agent:               opts.AgentRole,
		SessionName:         opts.SessionName,
		Port:                port,
		IsolationMode:       opts.IsolationMode,
		PluginHostPath:      opts.PluginHostPath,
		InstanceID:          opts.InstanceID,
		AgentEnvVars:        opts.AgentEnvVars,
		RuntimeEnvVars:      opts.RuntimeEnvVars,
		Layout:              LayoutFull,
		ForceFresh:          opts.ForceFresh,
		Headless:            opts.Headless,
		HarnessName:         opts.HarnessName,
		HarnessPipeSockPath: opts.HarnessPipeSockPath,
		ModelsByRole:        opts.ModelsByRole,
		PIExtensionDir:      opts.PIExtensionDir,
		// CLI overrides flow into buildDirectAgentCmd (host) and AgentPaneOpts
		// (bwrap / sandbox-exec) via BuildAgentCmd.
		Model:   opts.Model,
		Variant: opts.Variant,
		// Provider override rides the same seam.
		Provider: opts.Provider,
	}
}

// cleanupHalfAliveSession releases the DB resources for a session that was
// spawned but failed its readiness gate. Mirrors the cleanupAgentSession
// helper in internal/review/review.go but lives here so the session package
// can self-contain the readiness-failure cleanup path without an import
// cycle. All operations are best-effort and tolerant of missing rows.
//
// Importantly, this transitions the agent_status state to "error" so that
// db.GroupCompleted treats the row as terminal — without that, a review
// monitor watching the group would block indefinitely on the half-alive
// member's "idle" state.
//
// instanceID is the host-minted UUID pre-inserted into sessions by
// SpawnSession. When non-empty, cleanupHalfAliveSession also writes
// sessions.ended_at and sessions.end_state so the two tables stay in
// lock-step, avoiding the zombie-incarnation drift class.
//
// cause names the path that is closing the row, recorded as a session_reaped
// event. This helper is on the review-agent lifecycle:
// internal/review's spawn loop calls SpawnSession, so a review agent that
// fails its layout step or SpawnSession's own inline readiness gate is closed
// here, in state "error". Without the record, that row is indistinguishable in
// the round report from a force-terminate. detail is optional free text.
//
// The record is guarded on ended_at so this helper never claims a close it did
// not perform — db.SetEnded below carries the same `AND ended_at IS NULL`
// guard, and SessionEndCauses returns the LATEST recorded cause, so an
// unguarded record on an already-closed row would overwrite the true cause
// with a false one.
func cleanupHalfAliveSession(d *db.DB, sessionName, instanceID string, cause db.SessionReapCause, detail ...string) {
	st, lookupErr := d.CurrentStatus(sessionName)
	if lookupErr == nil && st != nil && st.EndedAt == nil {
		d.RecordReapBestEffort(sessionName, cause, detail...)
	}
	if lookupErr == nil && st != nil {
		// UpsertStatus is idempotent and validates the transition (idle →
		// error is allowed; error → error is a no-op-shaped repeat). We
		// preserve repo/worktree from the existing row so we do not blank
		// them out with empty strings.
		_ = d.UpsertStatus(sessionName, st.Repo, st.Worktree, "error", nil, nil)
	}
	_ = d.ReleasePort(sessionName)
	_ = d.SetEnded(sessionName)
	_ = d.PurgeBusMessages(sessionName)

	// Keep sessions in lock-step with agent_status: write ended_at and
	// end_state to the sessions row so consumers that join on
	// sessions.ended_at IS NULL do not see a zombie incarnation for every
	// readiness-timeout.
	//
	// end_state "readiness-timeout" is chosen over the generic "error" to
	// make the specific failure mode visible in audit queries and dashboards —
	// a readiness-timeout is a distinct failure class (the agent process never
	// reached the harness handshake) and deserves its own token.
	//
	// This is best-effort: if the UPDATE fails (e.g. the row was never
	// inserted due to an earlier error, or the DB is temporarily locked),
	// we log and continue — the other cleanup steps above have already run.
	if instanceID != "" {
		if updErr := d.UpdateSessionEnded(instanceID, "readiness-timeout"); updErr != nil {
			fmt.Fprintf(os.Stderr,
				"warning: cleanupHalfAliveSession: UpdateSessionEnded(%s): %v\n",
				instanceID, updErr)
		}
	}
}

// spawnFullLayout delegates to Create, which handles the 3-window spawn-path
// layout: edit (nvim), agent (pi), and term. Create also fires the
// tmux-session-start hook (which idempotently re-seeds root_agent_name) and
// starts the sidecar from inside setupFullLayout.
func spawnFullLayout(d *db.DB, opts SpawnOpts, port int) error {
	createOpts := buildOptsForLayout(opts, port, opts.PromptFilePath)
	createOpts.DB = d
	return Create(opts.SessionName, opts.Worktree, createOpts)
}

// spawnAgentPaneEnvVars builds the env-var map for the LayoutAgentOnly agent
// tmux pane. It mirrors session.agentPaneEnvVars but takes SpawnOpts directly,
// since the two layout paths use different opts structs.
//
// When opts.PromptFilePath is non-empty, PRISM_INITIAL_PROMPT_FILE carries
// the path to the prompt file and the prompt body itself is NOT inlined into
// tmux's argv. `prism agent-run` reads the file when it sees the env var and
// feeds the contents to the agent's --prompt path. This keeps the tmux launch
// command O(1) in prompt size.
//
// SpawnSession always writes the prompt file when there is a non-empty prompt,
// regardless of isolation mode or layout, so the PRISM_INITIAL_PROMPT inline
// branch below is exercised only by direct callers of this helper (e.g. test
// code) that pass Prompt without going through SpawnSession. Every production
// callsite goes through SpawnSession and receives a file path.
//
// Returns nil when no env vars are needed, producing no -e flags in tmux
// (an empty-string entry would override an inherited value, which is not
// the desired behaviour).
func spawnAgentPaneEnvVars(opts SpawnOpts) map[string]string {
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

// agentOnlyAgentEnvVars resolves the profile-level AgentEnvVars map for the
// agent-only layout, filtered for opts.AgentRole.
//
// When the caller supplies an explicit map, that map is filtered. Otherwise
// the map is loaded from profiles.json through config.AgentEnvVarsForRole —
// the same resolver `prism agent-run` uses for bwrap and sandbox-exec, so a
// given role gets the same env map in host mode and in a sandbox.
//
// Loading is best-effort: a missing or malformed profiles.json yields a nil
// map and the session starts without profile env vars.
func agentOnlyAgentEnvVars(opts SpawnOpts) map[string]string {
	if opts.AgentEnvVars != nil {
		return config.FilterAgentEnvVarsForRole(opts.AgentRole, opts.AgentEnvVars)
	}
	return config.AgentEnvVarsForRole(opts.AgentRole)
}

// buildOptsForAgentOnlyLayout returns the Opts that spawnAgentOnlyLayout
// hands to BuildAgentCmd. It is the agent-only-layout analogue of
// buildOptsForLayout, and exists for the same reason that one does: the
// SpawnOpts → Opts field mapping is the seam a refactor silently drops a
// field on, and a mapping inside a function that also starts a sidecar,
// writes the DB, and creates tmux windows cannot be pinned by a test.
//
// Keep the mapping here, not inline in the caller: every field has a
// forwarding test, and a field dropped from an inline literal is silent.
//
// mode is the caller's already-resolved isolation mode, not opts.IsolationMode:
// spawnAgentOnlyLayout substitutes the machine default for an empty value
// before it gets here, and the resolved value is what must reach BuildAgentCmd.
func buildOptsForAgentOnlyLayout(opts SpawnOpts, port int, mode string) Opts {
	return Opts{
		Prompt:         opts.Prompt,
		PromptFilePath: opts.PromptFilePath, // set by SpawnSession; keeps agentCmd O(1) in prompt size for host mode
		Agent:          opts.AgentRole,
		SessionName:    opts.SessionName,
		Port:           port,
		IsolationMode:  mode,
		PluginHostPath: opts.PluginHostPath,
		RuntimeEnvVars: opts.RuntimeEnvVars,
		PIExtensionDir: opts.PIExtensionDir,
		// CLI overrides for review-style agent-only layouts.
		Model:   opts.Model,
		Variant: opts.Variant,
		// Provider override on the same seam.
		Provider: opts.Provider,
		// ModelsByRole: the review fan-out is the primary user of
		// `--model-override`, and every reviewer lands on this layout. This
		// field must be forwarded so the entry for a reviewer's own role
		// reaches BuildAgentCmd and selects the model.
		ModelsByRole: opts.ModelsByRole,
		// AgentEnvVars: the role-filtered profile env vars. Both host mode and
		// the sandboxed modes resolve the map through the same role filter, so
		// they agree. The field is load-bearing for host mode only — the
		// sandboxed modes run `prism agent-run`, which resolves the same map
		// itself.
		AgentEnvVars: agentOnlyAgentEnvVars(opts),
	}
}

// spawnAgentOnlyLayout creates a 2-window tmux session for a review-style
// agent: window 0 is a bare shell, window 1 runs the agent command. The
// sidecar is started directly (it is owned by the session, not by a tmux
// hook — review sessions do not run the tmux-session-start seeding hook).
//
// No nvim/term windows — review agents do not need an editor or terminal;
// the worktree is read-only for them.
func spawnAgentOnlyLayout(opts SpawnOpts, port int) error {
	mode := opts.IsolationMode
	// When IsolationMode is not set, resolve the machine default from config
	// rather than silently falling back to host. A silent host fallback breaks
	// bwrap sessions: review agents would run unsandboxed with the invoking
	// session's own agent identity and trigger a recursive review explosion.
	if mode == "" {
		mode = string(config.Load().DefaultIsolationMode)
	}

	// Start the sidecar BEFORE creating the agent window — it handles SSE,
	// state, and host-API, and starting it up-front keeps the ordering
	// consistent across modes.
	sidecarOpts := StartSidecarOpts{
		Port:             port,
		IsolationMode:    mode,
		AgentRole:        opts.AgentRole,
		Worktree:         opts.Worktree,
		PluginHostPath:   opts.PluginHostPath,
		InitialPrompt:    opts.Prompt,
		InstanceID:       opts.InstanceID,
		WorktreeReadOnly: opts.WorktreeReadOnly,
		HarnessName:      opts.HarnessName,
		ModelsByRole:     opts.ModelsByRole,
	}
	if err := StartSidecarWithOpts(opts.SessionName, sidecarOpts); err != nil {
		// Non-fatal: log and continue, matching the LayoutFull behaviour
		// inside setupFullLayout. The session still gets created.
		fmt.Fprintf(os.Stderr,
			"warning: could not start sidecar for %q: %v\n",
			opts.SessionName, err)
	}

	// Build the agent command. BuildAgentCmd produces the right shape
	// for the resolved isolation mode (prism agent-run for bwrap/sandbox-exec,
	// direct pi for host).
	buildOpts := buildOptsForAgentOnlyLayout(opts, port, mode)
	agentCmd, err := BuildAgentCmd(buildOpts)
	if err != nil {
		return fmt.Errorf("spawnAgentOnlyLayout: build agent command for %q: %w", opts.SessionName, err)
	}

	// Persist isolation_mode to the DB BEFORE creating the agent window.
	// prism agent-run (the bwrap entry point) reads isolation_mode from
	// agent_status immediately on startup to validate the session mode. If the
	// write happens after tmux.NewWindow, agent-run races and sees NULL →
	// agent-run rejects the bwrap session with a mode-mismatch error.
	// Writing here, synchronously before NewWindow, removes the
	// race. Mirrors the identical write in setupFullLayout (session.go).
	// Non-fatal: a DB failure is logged and spawn continues.
	if mode != "" {
		if d, dbErr := openDB(); dbErr == nil {
			if setErr := d.SetIsolationMode(opts.SessionName, mode); setErr != nil {
				fmt.Fprintf(os.Stderr,
					"warning: spawnAgentOnlyLayout: set isolation_mode for %q: %v\n",
					opts.SessionName, setErr)
			}
			d.Close()
		} else {
			fmt.Fprintf(os.Stderr,
				"warning: spawnAgentOnlyLayout: could not open DB to write isolation_mode for %q: %v\n",
				opts.SessionName, dbErr)
		}
	}

	// Create the tmux session (window 0 = bare shell) and the agent window
	// (window 1).
	if err := tmux.NewSessionDetached(opts.SessionName, opts.Worktree); err != nil {
		return fmt.Errorf("spawn session: new tmux session %q: %w", opts.SessionName, err)
	}
	if err := tmux.NewWindow(opts.SessionName, 1, "agent", opts.Worktree, agentCmd, spawnAgentPaneEnvVars(opts)); err != nil {
		return fmt.Errorf("spawn session: new agent window for %q: %w", opts.SessionName, err)
	}
	_ = tmux.SelectWindow(opts.SessionName, 1)
	return nil
}

// spawnInputsFromOpts builds the db.SpawnInputs row written by SpawnSession
// from the audit-field subset of SpawnOpts. Each pointer field is set only
// when the corresponding flag was passed (non-empty / non-zero) so that
// genuinely-unset flags remain NULL in the DB — matching the schema's
// nullability and downstream `prism stats` / abtest queries which test for
// IS NULL / IS NOT NULL explicitly.
//
// Exported test-side via the spawnInputsFromOptsForTest shim in
// spawn_test.go so cmd/-level tests can assert the flag→column mapping
// without spinning up a real tmux/sidecar.
func spawnInputsFromOpts(opts SpawnOpts) db.SpawnInputs {
	si := db.SpawnInputs{
		InstanceID: opts.InstanceID,
		CreatedAt:  time.Now().UnixMilli(),
	}
	if opts.ProfileName != "" {
		s := opts.ProfileName
		si.ProfileName = &s
	}
	if opts.ModelFlag != "" {
		s := opts.ModelFlag
		si.ModelFlag = &s
	}
	if opts.VariantFlag != "" {
		s := opts.VariantFlag
		si.VariantFlag = &s
	}
	if opts.ProviderFlag != "" {
		s := opts.ProviderFlag
		si.ProviderFlag = &s
	}
	if opts.AgentFlag != "" {
		s := opts.AgentFlag
		si.AgentFlag = &s
	}
	if opts.HarnessFlag != "" {
		s := opts.HarnessFlag
		si.HarnessFlag = &s
	}
	if opts.IsolationFlag != "" {
		s := opts.IsolationFlag
		si.IsolationFlag = &s
	}
	// IsolationMode is the resolved effective mode the session actually ran
	// under — always populated when known so that `prism stats compare`'s
	// Spawn Inputs block surfaces a meaningful value even when the user
	// relied on the default and omitted --isolation (the common case).
	// Distinct from IsolationFlag above, which preserves the raw CLI value
	// (nil when omitted) as a separate audit trail.
	if opts.IsolationMode != "" {
		s := opts.IsolationMode
		si.IsolationMode = &s
	}
	si.HostModeFlag = opts.HostModeFlag
	si.ContainersFlag = opts.ContainersFlag
	if opts.PRNumber != 0 {
		n := opts.PRNumber
		si.PRNumber = &n
	}
	if opts.BranchFlag != "" {
		s := opts.BranchFlag
		si.BranchFlag = &s
	}
	si.IgnoreConcurrencyCap = opts.IgnoreConcurrencyCap
	if len(opts.ModelsByRole) > 0 {
		if encoded, err := json.Marshal(opts.ModelsByRole); err == nil {
			s := string(encoded)
			si.ModelVariantOverrides = &s
		}
	}
	if opts.SkillsManifestHash != "" {
		s := opts.SkillsManifestHash
		si.SkillsManifestHash = &s
	}
	if opts.PromptTemplateHash != "" {
		s := opts.PromptTemplateHash
		si.PromptTemplateHash = &s
	}
	if opts.AgentPromptHash != "" {
		s := opts.AgentPromptHash
		si.AgentPromptHash = &s
	}
	if opts.Prompt != "" {
		s := opts.Prompt
		si.PromptText = &s
	}
	if opts.PromptSource != "" {
		s := opts.PromptSource
		si.PromptSource = &s
	}
	if opts.AbtestPairID != "" {
		s := opts.AbtestPairID
		si.AbtestPairID = &s
	}
	return si
}

// SpawnInputsFromOpts is the test-only export of spawnInputsFromOpts. It lets
// cmd/-level unit tests assert the flag-to-column mapping without spinning up
// a real tmux/sidecar/DB stack — they build a SpawnOpts, call this, and
// assert on the returned db.SpawnInputs.
//
// Not for production use: production code should go through SpawnSession,
// which is the single chokepoint that writes the row.
func SpawnInputsFromOpts(opts SpawnOpts) db.SpawnInputs {
	return spawnInputsFromOpts(opts)
}

// Event types written by SpawnSession. Named constants so tests and
// downstream tooling (prism audit, prism review recovery, dashboard) can key
// off them without stringly-typed drift.
const (
	// EventSpawnIntent is the durable agent_events row written after the
	// sessions row is pre-inserted and before layout/port work runs. It
	// records that a spawn attempt reached the SpawnSession chokepoint; a
	// spawn that fails after this row is written surfaces as a
	// (spawn_intent, spawn_failed) pair, and a successful spawn surfaces as
	// a lone spawn_intent (plus the usual session_start / state_change
	// events downstream).
	EventSpawnIntent = "session.spawn_intent"

	// EventSpawnFailed is the durable agent_events row written when
	// SpawnSession aborts after EventSpawnIntent was written. The payload
	// names the failing step (layout or readiness) so forensic queries can
	// distinguish "the tmux/sidecar setup blew up" from "the agent never
	// signalled ready".
	EventSpawnFailed = "session.spawn_failed"
)

// spawnEventPayload is the JSON payload carried by both the
// session.spawn_intent and session.spawn_failed durable agent_events rows.
// Only session_name and instance_id are guaranteed; the other fields are
// filled in as the spawn progresses.
type spawnEventPayload struct {
	SessionName    string `json:"session_name"`
	InstanceID     string `json:"instance_id"`
	AgentRole      string `json:"agent_role,omitempty"`
	InvokerSession string `json:"invoker_session,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Layout         string `json:"layout,omitempty"`
	OccurredAt     string `json:"occurred_at"`
	// FailingStep names which spawn-pipeline step aborted. Only present on
	// EventSpawnFailed rows. Values: "layout", "readiness".
	FailingStep string `json:"failing_step,omitempty"`
	// Error is the underlying error string on failure rows. Only present on
	// EventSpawnFailed rows.
	Error string `json:"error,omitempty"`
}

// layoutLabel returns a short human-readable name for a Layout value, used
// in the durable event payloads. Zero-value / unknown layouts render as
// "unknown" rather than an integer so downstream queries stay readable.
func layoutLabel(l Layout) string {
	switch l {
	case LayoutFull:
		return "full"
	case LayoutAgentOnly:
		return "agent-only"
	}
	return "unknown"
}

// writeSpawnIntentEvent writes the durable EventSpawnIntent row into
// agent_events. Best-effort: a write failure is logged to the startup log
// and stderr but does NOT abort the spawn — telemetry must never break the
// happy path.
func writeSpawnIntentEvent(d *db.DB, opts SpawnOpts, startup *startupLogger) {
	payload := spawnEventPayload{
		SessionName:    opts.SessionName,
		InstanceID:     opts.InstanceID,
		AgentRole:      opts.AgentRole,
		InvokerSession: opts.InvokerSession,
		Repo:           opts.Repo,
		Layout:         layoutLabel(opts.Layout),
		OccurredAt:     time.Now().UTC().Format(time.RFC3339),
	}
	writeSpawnEvent(d, opts, EventSpawnIntent, payload, startup)
}

// writeSpawnFailedEvent writes the durable EventSpawnFailed row into
// agent_events AND, when opts.InvokerSession is non-empty, a best-effort
// bus_messages audit row so the invoker's forensic trail names the failure.
// Both writes are best-effort — a write failure here must never propagate to
// the caller.
func writeSpawnFailedEvent(d *db.DB, opts SpawnOpts, failingStep string, cause error, startup *startupLogger) {
	payload := spawnEventPayload{
		SessionName:    opts.SessionName,
		InstanceID:     opts.InstanceID,
		AgentRole:      opts.AgentRole,
		InvokerSession: opts.InvokerSession,
		Repo:           opts.Repo,
		Layout:         layoutLabel(opts.Layout),
		OccurredAt:     time.Now().UTC().Format(time.RFC3339),
		FailingStep:    failingStep,
	}
	if cause != nil {
		payload.Error = cause.Error()
	}
	writeSpawnEvent(d, opts, EventSpawnFailed, payload, startup)

	// Bus-message notification to the invoker (design fork #3: durable row
	// PLUS a bus message when the invoker is known). A bare CLI spawn with
	// no invoker sends no bus message.
	if opts.InvokerSession == "" {
		return
	}
	var invokerInstance *string
	if inv, err := d.CurrentStatus(opts.InvokerSession); err == nil && inv != nil && inv.InstanceID != nil && *inv.InstanceID != "" {
		invokerInstance = inv.InstanceID
	}
	text := fmt.Sprintf("spawn of %q failed at step %q: %v", opts.SessionName, failingStep, cause)
	msg := db.BusMessage{
		ID:           uuid.New().String(),
		FromSession:  opts.SessionName,
		ToSession:    opts.InvokerSession,
		ToInstanceID: invokerInstance,
		Repo:         opts.Repo,
		Text:         text,
		Urgency:      "normal",
		SentAt:       time.Now(),
	}
	if err := d.WriteBusMessage(msg); err != nil {
		startup.log("spawn-session: write spawn_failed bus message FAILED: %v", err)
		fmt.Fprintf(os.Stderr,
			"warning: could not write spawn_failed bus message for %q -> %q: %v\n",
			opts.SessionName, opts.InvokerSession, err)
	}
}

// writeSpawnEvent is the shared marshalling+insert path for both the intent
// and failed event writers. Best-effort: a marshal or DB error is logged but
// never propagated.
func writeSpawnEvent(d *db.DB, opts SpawnOpts, eventType string, payload spawnEventPayload, startup *startupLogger) {
	body, err := json.Marshal(payload)
	if err != nil {
		startup.log("spawn-session: marshal %s payload FAILED: %v", eventType, err)
		fmt.Fprintf(os.Stderr, "warning: marshal %s payload for %q: %v\n", eventType, opts.SessionName, err)
		return
	}
	var instancePtr *string
	if opts.InstanceID != "" {
		id := opts.InstanceID
		instancePtr = &id
	}
	ev := db.Event{
		ID:          uuid.New().String(),
		SessionName: opts.SessionName,
		Repo:        opts.Repo,
		Worktree:    opts.Worktree,
		InstanceID:  instancePtr,
		Type:        eventType,
		Payload:     string(body),
		CreatedAt:   time.Now(),
	}
	if err := d.WriteEvent(ev); err != nil {
		startup.log("spawn-session: write %s event FAILED: %v", eventType, err)
		fmt.Fprintf(os.Stderr,
			"warning: could not write %s event for %q: %v\n",
			eventType, opts.SessionName, err)
	}
}
