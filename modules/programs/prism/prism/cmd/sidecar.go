package cmd

// prism sidecar — long-running SSE consumer and (optionally) container manager
// that drives agent state transitions.
//
// Flags:
//
//	--session <name>           prism session name (e.g. "nixos-config@main")
//	--opencode-url <url>       base URL of the opencode HTTP server
//	                           (e.g. "http://localhost:14000")
//	--isolation-mode <mode>    isolation mode: "podman", "bwrap", or "host"
//	                           (default: "host" for back-compat when absent)
//	--container                enable container mode: create and manage a podman
//	                           container running opencode serve before connecting.
//	                           Deprecated: use --isolation-mode=podman instead.
//	--agent-role <role>        "worker" or "coordinator" (used in container mode
//	                           to select the correct GitHub token; when empty,
//	                           the role is inferred from SSE events at runtime)
//	--port <n>                 allocated host port (required in container/bwrap mode)
//	--plugin-path <path>       host path to the prism-hooks.ts plugin; mounted
//	                           read-only into the container (container mode only)
//	--config-content <json>    JSON blob for the container's opencode.json;
//	                           written to a temp file and mounted into the
//	                           container (container mode only)
//
// The sidecar connects to <opencode-url>/event and maps opencode SSE events to
// agent state transitions, writing them to prism.db. It handles idle debounce,
// permission tracking, and dashboard sentinel updates — replacing the equivalent
// logic in opencode/plugins/prism-hooks.ts.
//
// In podman mode (--isolation-mode=podman or legacy --container), the sidecar
// creates a podman container running "opencode --port 4096 --hostname 0.0.0.0"
// (combined TUI + HTTP mode), waits until the HTTP endpoint is healthy, then
// writes a ready signal so that the tmux pane can run "podman attach" to bridge
// the PTY (RFC #691, Phase 1a).
//
// In bwrap mode (--isolation-mode=bwrap), the sidecar does NOT create a
// container. The bwrap sandbox is launched and owned by the tmux pane via
// "prism agent-run". The sidecar still sets up the host-API Unix socket,
// selects the NewContainerMode harness adapter, and runs the full SSE loop.
//
// In host mode (--isolation-mode=host or no --container flag), the sidecar
// connects to an already-running opencode process. No container or sandbox
// management is performed.
//
// Clean shutdown: SIGINT and SIGTERM write "interrupted" state and (in container
// mode) stop/remove the container before exiting.

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/opencode"
	prismSession "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar"
)

var sidecarCmd = &cobra.Command{
	Use:   "sidecar",
	Short: "SSE consumer sidecar for opencode event processing",
	Long: `Connects to the opencode SSE event stream and maps events to agent
state transitions in prism.db. Designed to be started alongside opencode
by prism spawn.

The sidecar handles: state machine transitions, idle debounce (2s),
permission tracking, event logging, and dashboard sentinel updates.

In podman mode (--isolation-mode=podman or legacy --container), the sidecar
also creates and manages the podman container running opencode in combined
TUI + HTTP mode, health-checks it until ready, then writes a readiness signal
so the tmux pane can run podman attach to bridge the container PTY (RFC #691).

In bwrap mode (--isolation-mode=bwrap), the sidecar sets up the host-API Unix
socket and runs the full SSE loop, but does NOT create a container or write a
readiness signal. The bwrap sandbox is owned by the tmux pane.`,
	RunE: runSidecar,
}

func init() {
	sidecarCmd.Flags().String("session", "", "Prism session name (e.g. nixos-config@main)")
	sidecarCmd.Flags().String("opencode-url", "", "Base URL of the opencode HTTP server")
	sidecarCmd.Flags().String("isolation-mode", "", "Isolation mode: podman, bwrap, sandbox-exec, or host (default: derived from --container flag)")
	sidecarCmd.Flags().Bool("container", false, "Enable container mode (create/manage podman container) — deprecated, use --isolation-mode=podman")
	sidecarCmd.Flags().String("agent-role", "", "Agent role: worker or coordinator (used in container mode; inferred from SSE events when empty)")
	sidecarCmd.Flags().Int("port", 0, "Allocated host port (required in container/bwrap mode)")
	sidecarCmd.Flags().String("plugin-path", "", "Host path to prism-hooks.ts plugin (container mode only)")
	sidecarCmd.Flags().String("initial-prompt", "", "Initial prompt to deliver to the agent after container readiness (container mode only)")
	sidecarCmd.Flags().String("config-content", "", "JSON blob for container opencode.json; written to temp file and mounted (container mode only)")
	sidecarCmd.Flags().String("instance-id", "", "UUID instance identifier for this session incarnation (for container labels and bus message scoping)")
	sidecarCmd.Flags().Bool("worktree-readonly", false, "Mount the worktree read-only inside the container (used for review agents)")
	sidecarCmd.Flags().String("harness", "opencode", "Agent harness to use (e.g. opencode)")
	_ = sidecarCmd.MarkFlagRequired("session")
	_ = sidecarCmd.MarkFlagRequired("opencode-url")
	rootCmd.AddCommand(sidecarCmd)
}

func runSidecar(cmd *cobra.Command, args []string) error {
	sessionName, _ := cmd.Flags().GetString("session")
	opencodeURL, _ := cmd.Flags().GetString("opencode-url")
	isolationModeFlag, _ := cmd.Flags().GetString("isolation-mode")
	containerFlag, _ := cmd.Flags().GetBool("container")
	agentRole, _ := cmd.Flags().GetString("agent-role")
	port, _ := cmd.Flags().GetInt("port")
	pluginPath, _ := cmd.Flags().GetString("plugin-path")
	initialPrompt, _ := cmd.Flags().GetString("initial-prompt")
	configContent, _ := cmd.Flags().GetString("config-content")
	instanceID, _ := cmd.Flags().GetString("instance-id")
	worktreeReadOnly, _ := cmd.Flags().GetBool("worktree-readonly")
	harnessName, _ := cmd.Flags().GetString("harness")
	if harnessName == "" {
		harnessName = "opencode"
	}
	if _, ok := harness.Lookup(harnessName); !ok {
		return fmt.Errorf("sidecar: unknown harness %q: valid harnesses: %s", harnessName, strings.Join(harness.Names(), ", "))
	}

	// Resolve the effective isolation mode. When --isolation-mode is set it
	// takes precedence. Otherwise fall back to --container for back-compat.
	var isolationMode config.IsolationMode
	if isolationModeFlag != "" && config.IsValidIsolationMode(isolationModeFlag) {
		isolationMode = config.IsolationMode(isolationModeFlag)
	} else if containerFlag {
		isolationMode = config.IsolationPodman
	} else {
		isolationMode = config.IsolationHost
	}

	// Derive the legacy bool for paths that still use it.
	podmanMode := isolationMode == config.IsolationPodman
	bwrapMode := isolationMode == config.IsolationBwrap
	sandboxExecMode := isolationMode == config.IsolationSandboxExec
	// needsHostAPI is true for podman, bwrap, and sandbox-exec (the agent runs
	// in a sandbox without direct access to host tmux, so the host-API socket
	// is required).
	needsHostAPI := podmanMode || bwrapMode || sandboxExecMode
	// useContainerHarness is true for podman, bwrap, and sandbox-exec
	// (opencode is pre-created with --prompt at launch in all three cases).
	useContainerHarness := podmanMode || bwrapMode || sandboxExecMode

	// Derive repo and worktree from session name and environment.
	// The session name format is "repo@branch". The worktree is expected
	// to be passed via environment (PRISM_WORKTREE) or derived from CWD.
	repo := sessionName
	if idx := strings.Index(sessionName, "@"); idx >= 0 {
		repo = sessionName[:idx]
	}

	worktree := os.Getenv("PRISM_WORKTREE")
	if worktree == "" {
		worktree, _ = os.Getwd()
	}
	// Resolve worktree to absolute path before passing it to the sidecar config
	// and the initial UpsertStatus call.
	if abs, err := filepath.Abs(worktree); err == nil {
		worktree = abs
	}

	// Open the prism database.
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("sidecar: open database: %w", err)
	}
	defer d.Close()

	// Ensure there is an initial agent_status row so the session is visible on
	// the dashboard immediately. Use UpsertStatus with state "idle" and write
	// repo/worktree — mirroring what tmux-session-start does.
	if err := d.UpsertStatus(sessionName, repo, worktree, "idle", nil, nil); err != nil {
		fmt.Fprintf(os.Stderr, "[prism sidecar] initial upsert: %v\n", err)
	}

	// Load prism runtime config — needed for git identity and SSH key names.
	prismCfg := config.Load()

	// Resolve the agent model once via the harness adapter. The adapter is
	// constructed transiently here for the EffectiveModel call; a fresh
	// adapter with the resolved model is constructed below for the sidecar.
	modelProbe, _ := harness.New(harnessName, "", nil, agentRole, "")
	agentModel := modelProbe.EffectiveModel(agentRole)

	// Load profiles to extract container resource caps and agent env vars.
	// Non-fatal if missing (e.g. running without the nix module) — resource
	// fields remain at their zero values and no resource flags are emitted.
	var containerResources config.ContainerResources
	var agentEnvVars map[string]string
	if podmanMode {
		if pf, pfErr := config.LoadProfiles(); pfErr == nil {
			containerResources = pf.ContainerResources
			agentEnvVars = pf.AgentEnvVars
		}
	}

	// Build container config — only for podman mode. bwrap mode does not
	// use a container.Config; the bwrap sandbox is owned by the tmux pane.
	var ctrCfg *container.Config
	if podmanMode {
		if port == 0 {
			return fmt.Errorf("sidecar: --port is required in podman container mode")
		}
		// Derive git bare-root and worktree private git dir so that git works
		// inside the container without following the absolute host path stored
		// in the worktree's .git file (fixes #485).
		bareRoot := git.BareRoot(worktree)
		var worktreeGitDir string
		if bareRoot != "" {
			worktreeGitDir = filepath.Join(bareRoot, ".bare", "worktrees", filepath.Base(worktree))
		}
		ctrCfg = &container.Config{
			SessionName:       sessionName,
			Worktree:          worktree,
			WorktreeReadOnly:  worktreeReadOnly,
			BareRoot:          bareRoot,
			WorktreeGitDir:    worktreeGitDir,
			AllocatedPort:     port,
			AgentRole:         agentRole,
			AgentModel:        agentModel,
			InstanceID:        instanceID,
			PluginHostPath:    pluginPath,
			ConfigContent:     configContent,
			InitialPrompt:     initialPrompt,
			GitUserName:       prismCfg.GitUserName,
			GitUserEmail:      prismCfg.GitUserEmail,
			SshAccessKeyName:  prismCfg.SshAccessKeyName,
			SshSigningKeyName: prismCfg.SshSigningKeyName,
			MemoryMax:         containerResources.MemoryMax,
			MemorySwapMax:     containerResources.MemorySwapMax,
			PidsLimit:         containerResources.PidsLimit,
			AgentEnvVars:      agentEnvVars,
		}
	}

	// Build the host-API socket path for podman, bwrap, and sandbox-exec modes.
	// The agent runs in a sandbox without direct access to host tmux in all
	// three cases, so the host-API Unix socket is required for proxying prism
	// CLI calls.
	var hostAPISockPath string
	if needsHostAPI {
		if port == 0 && (bwrapMode || sandboxExecMode) {
			return fmt.Errorf("sidecar: --port is required in bwrap/sandbox-exec mode")
		}
		sockPath, err := prismSession.SidecarHostAPIPath(sessionName)
		if err != nil {
			return fmt.Errorf("sidecar: resolve host-API socket path: %w", err)
		}
		hostAPISockPath = sockPath
		// Wire the socket path into the container config so the container
		// gets the socket mounted and PRISM_HOST_API injected (A-2).
		// In bwrap and sandbox-exec modes ctrCfg is nil — the sandbox args
		// are built by the isolator at prism agent-run time, not here.
		if ctrCfg != nil {
			ctrCfg.HostAPISockPath = sockPath
		}
	}

	// Build the OnReady callback — only for podman mode (AC-18, AC-19).
	// In bwrap and sandbox-exec modes the sidecar does NOT write the readiness
	// signal: "prism agent-run" in the tmux pane starts immediately without
	// waiting. In host mode there is no readiness file at all.
	var onReady func()
	if podmanMode {
		onReady = func() {
			readyPath, pathErr := prismSession.SidecarReadyPath(sessionName)
			if pathErr != nil {
				fmt.Fprintf(os.Stderr, "[prism sidecar] ready path: %v\n", pathErr)
				return
			}
			if err := os.MkdirAll(filepath.Dir(readyPath), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "[prism sidecar] ready dir: %v\n", err)
				return
			}
			f, err := os.Create(readyPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[prism sidecar] write ready signal: %v\n", err)
				return
			}
			_ = f.Close()
			fmt.Fprintf(os.Stderr, "[prism sidecar] ready signal written: %s\n", readyPath)
		}
	}

	// Construct the harness adapter and inject it via Config.Harness.
	// The adapter encapsulates all runtime-specific behaviour: session
	// creation, prompt delivery, SSE subscription, event type extraction, and
	// event mapping. The sidecar calls through the harness.Harness interface
	// and has no direct dependency on the adapter package (#710).
	//
	// In podman and bwrap mode, use NewContainer so that:
	//   - CreateSession uses GET /session to retrieve the existing session ID
	//     (opencode already created a session when the TUI started)
	//   - DeliverInitialPrompt is a no-op (prompt was sent via --prompt CLI flag)
	var h harness.Harness
	if useContainerHarness {
		h, err = harness.NewContainer(harnessName, opencodeURL, nil, agentRole, agentModel)
	} else {
		h, err = harness.New(harnessName, opencodeURL, nil, agentRole, agentModel)
	}
	if err != nil {
		return fmt.Errorf("sidecar: construct harness adapter: %w", err)
	}

	// Inject harness-specific runtime env vars into the container config.
	// This replaces the previously hard-coded OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS
	// in container.go and bwrap.go with values from the harness adapter.
	if ctrCfg != nil {
		ctrCfg.RuntimeEnv = h.RuntimeEnv()
	}

	cfg := sidecar.Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        worktree,
		OpencodeURL:     opencodeURL,
		DB:              d,
		Clock:           sidecar.RealClock(),
		AgentRole:       agentRole,
		AgentModel:      agentModel,
		HarnessName:     harnessName,
		InstanceID:      instanceID,
		Container:       ctrCfg,
		HostAPISockPath: hostAPISockPath,
		OnReady:         onReady,
		InitialPrompt:   initialPrompt,
		Harness:         h,
	}
	sc := sidecar.New(cfg)

	// Set up signal handling for clean shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		// Shutdown() runs before cancel() so that its DB writes (upsertState,
		// writeEvent) complete while the database connection is still open.
		// Defer d.Close() runs only after runSidecar returns, which happens
		// after Run() exits, which happens after cancel() fires here.
		// OnReady is gated on Sidecar.shuttingDown (set at the start of
		// Shutdown()) to prevent it from firing after SIGTERM even though
		// cancel() comes last.
		sc.Shutdown()
		cancel()
	}()

	// Run clipboard staging dir cleanup in the background at sidecar startup.
	// This implements the TTL sweep that prevents ~/.cache/prism/clipboard/ from
	// growing unboundedly across multiple paste operations. The sweep is
	// fire-and-forget; errors are non-fatal (logged by runClipboardClean itself).
	// Runs for podman and bwrap modes (both run the agent in a sandbox where
	// the clipboard staging dir is mounted).
	if needsHostAPI {
		go func() {
			_ = runClipboardClean(nil, nil)
		}()
	}

	fmt.Fprintf(os.Stderr, "[prism sidecar] starting: session=%s url=%s isolation=%s\n",
		sessionName, opencodeURL, isolationMode)

	if err := sc.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("sidecar: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[prism sidecar] shutting down\n")
	return nil
}


