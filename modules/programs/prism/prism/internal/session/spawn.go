package session

// SpawnSession is the single shared primitive for creating a prism session
// end-to-end. Both `prism spawn` (cmd/spawn.go → ensureAndSwitch) and
// `prism review` (internal/review/review.go per-agent loop) compose over it.
//
// Design (see #849 §3.1 and #859):
//
//   - A prism session is the fundamental primitive. A spawn-style command is
//     an abstraction over the primitive. The primitive is uniform; the
//     abstractions are allowed to be rich and divergent.
//
//   - SpawnSession contains NO branching on session-type strings. All variant
//     behaviour flows through SpawnOpts fields (Layout, WorktreeReadOnly,
//     ContainerMode, GroupID, …). If a branch here feels unavoidable, first
//     ask whether it should be a new SpawnOpts field.
//
//   - root_agent_name is written at spawn time from opts.AgentRole — no NULL
//     window (relies on #857 / Issue B).
//
//   - group_id is written from opts.GroupID when non-empty. It is a no-op for
//     single-session spawns (spawn/pr); Issue E (#860) will wire it into
//     review.go once this primitive lands.

import (
	"fmt"
	"os"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

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
	// Passed via opencode's --prompt CLI flag (host mode) or via the sidecar
	// (container/bwrap mode).
	Prompt string

	// ConfigContent is the JSON blob injected as OPENCODE_CONFIG_CONTENT.
	// Generated upstream from --profile/--model/--variant or from per-role
	// container profiles. Empty string means no override.
	ConfigContent string

	// Layout selects the tmux window layout created for this session.
	//   - LayoutFull:       3-window layout (edit / agent / term) — spawn path
	//   - LayoutAgentOnly:  2-window layout (shell / agent)       — review path
	Layout Layout

	// ContainerMode, when true, runs opencode inside a podman container.
	// Deprecated alias for IsolationMode == "podman"; both are accepted for
	// back-compat.
	ContainerMode bool

	// IsolationMode is the resolved isolation mode for this session.
	// Valid values: "podman", "bwrap", "host". When non-empty it overrides
	// ContainerMode.
	IsolationMode string

	// PluginHostPath is the host-side path to the opencode plugin bind-mounted
	// into the container.
	PluginHostPath string

	// InstanceID is the UUID for this session incarnation. When empty,
	// callers that need a stable instance_id should generate one and
	// populate this field before calling SpawnSession; otherwise the sidecar
	// / tmux-session-start hook will generate one.
	InstanceID string

	// WorktreeReadOnly, when true, mounts the worktree read-only inside the
	// container. Set for review agents so they cannot modify the branch
	// under review.
	WorktreeReadOnly bool

	// GroupID, when non-empty, associates this session with a SessionGroup
	// (see db.RegisterGroup). Written to agent_status.group_id. This is the
	// hook for Issue E (#860) — SpawnSession writes it when present but does
	// not create the group itself.
	GroupID string

	// AgentEnvVars are additional env vars prefixed to the opencode command
	// in host-mode sessions (see profiles.json agent_env_vars). Ignored in
	// container/bwrap mode — those paths deliver env vars via podman --env
	// in the sidecar.
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
// Ordering is preserved from the pre-extraction call sites:
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

	// Step 1: Seed agent_status with root_agent_name. Idempotent; later
	// writes by the sidecar and by tmux-session-start COALESCE-preserve the
	// value written here.
	if err := d.UpsertStatusSeedRootAgentName(
		opts.SessionName, opts.Repo, opts.Worktree, "idle", nil, nil, opts.AgentRole,
	); err != nil {
		return fmt.Errorf("spawn session: seed status: %w", err)
	}

	// Step 2: Write group_id when set (hook for Issue E — single-session
	// spawns leave GroupID empty and this is a no-op).
	if opts.GroupID != "" {
		if err := d.SetGroupID(opts.SessionName, opts.GroupID); err != nil {
			return fmt.Errorf("spawn session: set group_id: %w", err)
		}
	}

	// Generate an instance_id if the caller did not pre-populate one. The
	// sidecar needs it via --instance-id to tag container labels and bus
	// messages; writing it to the DB here keeps the DB row in sync before
	// the tmux-session-start hook fires.
	if opts.InstanceID == "" && opts.Layout == LayoutFull {
		opts.InstanceID = uuid.New().String()
		if err := d.SetInstanceID(opts.SessionName, opts.InstanceID); err != nil {
			// Non-fatal: instance isolation degrades gracefully. Log and
			// continue — the sidecar will read back an empty instance_id
			// from the DB (which is the pre-instance-id behaviour).
			fmt.Fprintf(os.Stderr,
				"warning: could not set instance_id for %q: %v\n",
				opts.SessionName, err)
			opts.InstanceID = ""
		}
	}

	// Step 3: Allocate a port from the configured range. Fails fast if the
	// allocation fails — a session with no port cannot start opencode.
	port, err := d.AllocatePort(opts.SessionName)
	if err != nil {
		return fmt.Errorf("spawn session: allocate port: %w", err)
	}

	// Step 4 & 5: Create the tmux session and start the sidecar. Both
	// layouts share the same responsibilities — only the window shape and
	// the ownership of the sidecar-start call differ.
	switch opts.Layout {
	case LayoutFull:
		return spawnFullLayout(d, opts, port)
	case LayoutAgentOnly:
		return spawnAgentOnlyLayout(opts, port)
	default:
		return fmt.Errorf("spawn session: unsupported layout %d", opts.Layout)
	}
}

// spawnFullLayout delegates to Create, which handles the 3-window spawn-path
// layout: edit (nvim), agent (opencode), and term. Create also fires the
// tmux-session-start hook (which idempotently re-seeds root_agent_name) and
// starts the sidecar from inside setupFullLayout.
func spawnFullLayout(d *db.DB, opts SpawnOpts, port int) error {
	createOpts := Opts{
		Prompt:           opts.Prompt,
		Agent:            opts.AgentRole,
		ConfigContent:    opts.ConfigContent,
		SessionName:      opts.SessionName,
		Port:             port,
		ContainerMode:    opts.ContainerMode,
		IsolationMode:    opts.IsolationMode,
		PluginHostPath:   opts.PluginHostPath,
		InstanceID:       opts.InstanceID,
		AgentEnvVars:     opts.AgentEnvVars,
		Layout:           LayoutFull,
		ForceFresh:       opts.ForceFresh,
		Headless:         opts.Headless,
		DB:               d,
	}
	return Create(opts.SessionName, opts.Worktree, createOpts)
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
	if mode == "" && opts.ContainerMode {
		mode = "podman"
	}

	// Start the sidecar BEFORE creating the agent window so that the
	// readiness file (in podman mode) exists by the time the pane polls for
	// it. For bwrap / host modes the sidecar still handles SSE, state, and
	// host-API, and starting it up-front keeps the ordering consistent.
	sidecarOpts := StartSidecarOpts{
		Port:             port,
		ContainerMode:    opts.ContainerMode,
		IsolationMode:    mode,
		AgentRole:        opts.AgentRole,
		Worktree:         opts.Worktree,
		PluginHostPath:   opts.PluginHostPath,
		InitialPrompt:    opts.Prompt,
		ConfigContent:    opts.ConfigContent,
		InstanceID:       opts.InstanceID,
		WorktreeReadOnly: opts.WorktreeReadOnly,
	}
	if err := StartSidecarWithOpts(opts.SessionName, sidecarOpts); err != nil {
		// Non-fatal: log and continue, matching the LayoutFull behaviour
		// inside setupFullLayout. The session still gets created.
		fmt.Fprintf(os.Stderr,
			"warning: could not start sidecar for %q: %v\n",
			opts.SessionName, err)
	}

	// Build the agent command. BuildOpencodeCmd produces the right shape
	// for the resolved isolation mode (podman attach / prism agent-run /
	// direct opencode). For podman mode, wrap it in the readiness-wait
	// script so the pane blocks until the sidecar has health-checked the
	// container.
	buildOpts := Opts{
		Prompt:         opts.Prompt,
		Agent:          opts.AgentRole,
		ConfigContent:  opts.ConfigContent,
		SessionName:    opts.SessionName,
		Port:           port,
		ContainerMode:  opts.ContainerMode,
		IsolationMode:  mode,
		PluginHostPath: opts.PluginHostPath,
		// AgentEnvVars intentionally omitted: review sessions don't inject
		// profile env vars in host mode today.
	}
	agentCmd := BuildOpencodeCmd(buildOpts)
	if mode == "podman" && port != 0 {
		readyPath, pathErr := SidecarReadyPath(opts.SessionName)
		if pathErr != nil {
			fmt.Fprintf(os.Stderr,
				"warning: could not determine ready path for %q, skipping readiness wait: %v\n",
				opts.SessionName, pathErr)
		} else {
			_ = os.Remove(readyPath)
			agentCmd = buildReadinessWaitCmd(readyPath, agentCmd)
		}
	}

	// Create the tmux session (window 0 = bare shell) and the agent window
	// (window 1).
	if err := tmux.NewSessionDetached(opts.SessionName, opts.Worktree); err != nil {
		return fmt.Errorf("spawn session: new tmux session %q: %w", opts.SessionName, err)
	}
	if err := tmux.NewWindow(opts.SessionName, 1, "agent", opts.Worktree, agentCmd, nil); err != nil {
		return fmt.Errorf("spawn session: new agent window for %q: %w", opts.SessionName, err)
	}
	_ = tmux.SelectWindow(opts.SessionName, 1)
	return nil
}
