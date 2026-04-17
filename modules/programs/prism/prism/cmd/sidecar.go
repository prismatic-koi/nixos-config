package cmd

// prism sidecar — long-running SSE consumer and (optionally) container manager
// that drives agent state transitions.
//
// Flags:
//
//	--session <name>          prism session name (e.g. "nixos-config@main")
//	--opencode-url <url>      base URL of the opencode HTTP server
//	                          (e.g. "http://localhost:14000")
//	--container               enable container mode: create and manage a podman
//	                          container running opencode serve before connecting
//	--agent-role <role>       "worker" or "coordinator" (used in container mode
//	                          to select the correct GitHub token)
//	--port <n>                allocated host port (required in container mode)
//	--plugin-path <path>      host path to the prism-hooks.ts plugin; mounted
//	                          read-only into the container (container mode only)
//	--config-content <json>   JSON blob for the container's opencode.json;
//	                          written to a temp file and mounted into the
//	                          container (container mode only)
//
// The sidecar connects to <opencode-url>/event and maps opencode SSE events to
// agent state transitions, writing them to prism.db. It handles idle debounce,
// permission tracking, and dashboard sentinel updates — replacing the equivalent
// logic in opencode/plugins/prism-hooks.ts.
//
// In container mode, the sidecar creates a podman container running
// "opencode --port 4096 --hostname 0.0.0.0" (combined TUI + HTTP mode), waits
// until the HTTP endpoint is healthy, then writes a ready signal so that the
// tmux pane can run "podman attach" to bridge the PTY (RFC #691, Phase 1a).
//
// Clean shutdown: SIGINT and SIGTERM write "interrupted" state and (in container
// mode) stop/remove the container before exiting.

import (
	"context"
	"encoding/json"
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
	opencode "github.com/prismatic-koi/prism/internal/harness/opencode"
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

In container mode (--container), the sidecar also creates and manages the
podman container running opencode in combined TUI + HTTP mode, health-checks
it until ready, then writes a readiness signal so the tmux pane can run
podman attach to bridge the container PTY (RFC #691, Phase 1a).`,
	RunE: runSidecar,
}

func init() {
	sidecarCmd.Flags().String("session", "", "Prism session name (e.g. nixos-config@main)")
	sidecarCmd.Flags().String("opencode-url", "", "Base URL of the opencode HTTP server")
	sidecarCmd.Flags().Bool("container", false, "Enable container mode (create/manage podman container)")
	sidecarCmd.Flags().String("agent-role", "worker", "Agent role: worker or coordinator (used in container mode)")
	sidecarCmd.Flags().Int("port", 0, "Allocated host port (required in container mode)")
	sidecarCmd.Flags().String("plugin-path", "", "Host path to prism-hooks.ts plugin (container mode only)")
	sidecarCmd.Flags().String("initial-prompt", "", "Initial prompt to deliver to the agent after container readiness (container mode only)")
	sidecarCmd.Flags().String("config-content", "", "JSON blob for container opencode.json; written to temp file and mounted (container mode only)")
	sidecarCmd.Flags().String("instance-id", "", "UUID instance identifier for this session incarnation (for container labels and bus message scoping)")
	sidecarCmd.Flags().Bool("worktree-readonly", false, "Mount the worktree read-only inside the container (used for review agents)")
	_ = sidecarCmd.MarkFlagRequired("session")
	_ = sidecarCmd.MarkFlagRequired("opencode-url")
	rootCmd.AddCommand(sidecarCmd)
}

func runSidecar(cmd *cobra.Command, args []string) error {
	sessionName, _ := cmd.Flags().GetString("session")
	opencodeURL, _ := cmd.Flags().GetString("opencode-url")
	containerMode, _ := cmd.Flags().GetBool("container")
	agentRole, _ := cmd.Flags().GetString("agent-role")
	port, _ := cmd.Flags().GetInt("port")
	pluginPath, _ := cmd.Flags().GetString("plugin-path")
	initialPrompt, _ := cmd.Flags().GetString("initial-prompt")
	configContent, _ := cmd.Flags().GetString("config-content")
	instanceID, _ := cmd.Flags().GetString("instance-id")
	worktreeReadOnly, _ := cmd.Flags().GetBool("worktree-readonly")

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

	// Resolve the agent model once; used in both the container config and the
	// harness adapter construction below.
	agentModel := opencodeAgentModel(agentRole)

	// Build container config if container mode is enabled.
	var ctrCfg *container.Config
	if containerMode {
		if port == 0 {
			return fmt.Errorf("sidecar: --port is required in container mode")
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
		}
	}

	// Build the host-API socket path for container mode (A-1).
	var hostAPISockPath string
	if containerMode {
		sockPath, err := prismSession.SidecarHostAPIPath(sessionName)
		if err != nil {
			return fmt.Errorf("sidecar: resolve host-API socket path: %w", err)
		}
		hostAPISockPath = sockPath
		// Wire the socket path into the container config so the container
		// gets the socket mounted and PRISM_HOST_API injected (A-2).
		ctrCfg.HostAPISockPath = sockPath
	}

	// Build the OnReady callback for container mode (AC-18, AC-19).
	// Written to the config before creating the sidecar.
	var onReady func()
	if containerMode {
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

	// Construct the opencode harness adapter and inject it via Config.Harness.
	// The adapter encapsulates all opencode-specific behaviour: session
	// creation, prompt delivery, SSE subscription, event type extraction, and
	// event mapping. The sidecar calls through the harness.Harness interface
	// and has no direct dependency on the opencode package (#710).
	//
	// In container mode, use NewContainerMode so that:
	//   - CreateSession uses GET /session to retrieve the existing session ID
	//     (opencode already created a session when the TUI started)
	//   - DeliverInitialPrompt is a no-op (prompt was sent via --prompt CLI flag)
	var h *opencode.Adapter
	if containerMode {
		h = opencode.NewContainerMode(opencodeURL, nil, agentRole, agentModel)
	} else {
		h = opencode.New(opencodeURL, nil, agentRole, agentModel)
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
	if containerMode {
		go func() {
			_ = runClipboardClean(nil, nil)
		}()
	}

	fmt.Fprintf(os.Stderr, "[prism sidecar] starting: session=%s url=%s container=%v\n",
		sessionName, opencodeURL, containerMode)

	if err := sc.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("sidecar: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[prism sidecar] shutting down\n")
	return nil
}

// opencodeAgentModel reads the model configured for the given agent role from
// the opencode config file (~/.config/opencode/opencode.json). Returns empty
// string if the config cannot be read or the agent has no explicit model.
func opencodeAgentModel(agentRole string) string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	data, err := os.ReadFile(filepath.Join(configHome, "opencode", "opencode.json"))
	if err != nil {
		return ""
	}

	// Minimal parse — just enough to extract agent.<role>.model.
	// Using encoding/json directly to avoid pulling in a full config package.
	var cfg struct {
		Agent map[string]struct {
			Model string `json:"model"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	if a, ok := cfg.Agent[agentRole]; ok {
		return a.Model
	}
	return ""
}
