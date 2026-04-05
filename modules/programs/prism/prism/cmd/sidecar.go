package cmd

// prism sidecar — long-running SSE consumer that drives agent state transitions
//
// Flags:
//
//	--session <name>       prism session name (e.g. "nixos-config@main")
//	--opencode-url <url>   base URL of the opencode HTTP server (e.g. "http://localhost:14000")
//
// The sidecar connects to <opencode-url>/event and maps opencode SSE events to
// agent state transitions, writing them to prism.db. It handles idle debounce,
// permission tracking, and dashboard sentinel updates — replacing the equivalent
// logic in opencode/plugins/prism-hooks.ts.
//
// Clean shutdown: SIGINT and SIGTERM write "interrupted" state before exiting.

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/sidecar"
)

var sidecarCmd = &cobra.Command{
	Use:   "sidecar",
	Short: "SSE consumer sidecar for opencode event processing",
	Long: `Connects to the opencode SSE event stream and maps events to agent
state transitions in prism.db. Designed to be started alongside opencode
by prism spawn.

The sidecar handles: state machine transitions, idle debounce (2s),
permission tracking, event logging, and dashboard sentinel updates.`,
	RunE: runSidecar,
}

func init() {
	sidecarCmd.Flags().String("session", "", "Prism session name (e.g. nixos-config@main)")
	sidecarCmd.Flags().String("opencode-url", "", "Base URL of the opencode HTTP server")
	_ = sidecarCmd.MarkFlagRequired("session")
	_ = sidecarCmd.MarkFlagRequired("opencode-url")
	rootCmd.AddCommand(sidecarCmd)
}

func runSidecar(cmd *cobra.Command, args []string) error {
	sessionName, _ := cmd.Flags().GetString("session")
	opencodeURL, _ := cmd.Flags().GetString("opencode-url")

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

	cfg := sidecar.Config{
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    worktree,
		OpencodeURL: opencodeURL,
		DB:          d,
		Clock:       sidecar.RealClock(),
	}
	sc := sidecar.New(cfg)

	// Set up signal handling for clean shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		sc.Shutdown()
		cancel()
	}()

	// Resolve worktree to absolute path for sentinel derivation.
	if abs, err := filepath.Abs(worktree); err == nil {
		worktree = abs
	}

	fmt.Fprintf(os.Stderr, "[prism sidecar] starting: session=%s url=%s\n", sessionName, opencodeURL)

	if err := sc.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("sidecar: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[prism sidecar] shutting down\n")
	return nil
}
