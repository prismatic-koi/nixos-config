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
//
// The sidecar connects to <opencode-url>/event and maps opencode SSE events to
// agent state transitions, writing them to prism.db. It handles idle debounce,
// permission tracking, and dashboard sentinel updates — replacing the equivalent
// logic in opencode/plugins/prism-hooks.ts.
//
// In container mode, the sidecar creates a podman container running
// "opencode serve --port 4096", waits until the HTTP endpoint is healthy, then
// writes a ready signal so that the tmux pane can run "opencode attach".
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

	"github.com/prismatic-koi/prism/internal/container"
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
podman container running opencode serve, health-checks it until ready, then
writes a readiness signal so the tmux pane can run opencode attach.`,
	RunE: runSidecar,
}

func init() {
	sidecarCmd.Flags().String("session", "", "Prism session name (e.g. nixos-config@main)")
	sidecarCmd.Flags().String("opencode-url", "", "Base URL of the opencode HTTP server")
	sidecarCmd.Flags().Bool("container", false, "Enable container mode (create/manage podman container)")
	sidecarCmd.Flags().String("agent-role", "worker", "Agent role: worker or coordinator (used in container mode)")
	sidecarCmd.Flags().Int("port", 0, "Allocated host port (required in container mode)")
	sidecarCmd.Flags().String("plugin-path", "", "Host path to prism-hooks.ts plugin (container mode only)")
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

	// Build container config if container mode is enabled.
	var ctrCfg *container.Config
	if containerMode {
		if port == 0 {
			return fmt.Errorf("sidecar: --port is required in container mode")
		}
		ctrCfg = &container.Config{
			SessionName:    sessionName,
			Worktree:       worktree,
			AllocatedPort:  port,
			AgentRole:      agentRole,
			PluginHostPath: pluginPath,
		}
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

	cfg := sidecar.Config{
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    worktree,
		OpencodeURL: opencodeURL,
		DB:          d,
		Clock:       sidecar.RealClock(),
		Container:   ctrCfg,
		OnReady:     onReady,
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
		// OnReady is guarded by ctx.Err() in sidecar.Run() to prevent it
		// from firing after SIGTERM even though cancel() comes last.
		sc.Shutdown()
		cancel()
	}()

	fmt.Fprintf(os.Stderr, "[prism sidecar] starting: session=%s url=%s container=%v\n",
		sessionName, opencodeURL, containerMode)

	if err := sc.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("sidecar: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[prism sidecar] shutting down\n")
	return nil
}
