// Command iris is the daemon-mode successor to prism (codename iris, D-3).
//
// D-3 adds: spawn, harness-socket dispatch, tool override registration.
// See docs/daemon-mode-design.md §3 for the architecture.
//
// Usage:
//
//	iris --version                              — print version string and exit 0
//	iris version                                — same, as a subcommand
//	iris spawn --worktree <path> [--role <role>] — spawn a pi session
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

// irisVersion is the version string for the iris binary. It is set to the
// package version at compile time; in development builds it is "dev".
//
// This intentionally does NOT reuse the prism version string or any
// prism-internal ldflags variable — iris has its own identity.
const irisVersion = "0.1.0-d3"

func main() {
	if err := rootCmd.Execute(); err != nil {
		var ec interface{ ExitCode() int }
		if errors.As(err, &ec) {
			if msg := err.Error(); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			os.Exit(ec.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:     "iris",
	Short:   "Iris — daemon-mode successor to prism (codename, D-2+)",
	Version: irisVersion,

	// The default run opens the DB and loads config so that a plain `iris`
	// invocation exercises the startup path. --version is handled by cobra
	// before RunE is called.
	RunE: func(cmd *cobra.Command, args []string) error {
		return startup()
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(spawnCmd)
	spawnCmd.Flags().StringVar(&spawnWorktree, "worktree", "", "Absolute path to the git worktree (required)")
	spawnCmd.Flags().StringVar(&spawnRole, "role", "worker", "Agent role (worker, coordinator, etc.)")
	spawnCmd.Flags().StringVar(&spawnExtension, "extension", "", "Path to the prism.ts extension file (overrides config)")
	_ = spawnCmd.MarkFlagRequired("worktree")
}

var (
	spawnWorktree  string
	spawnRole      string
	spawnExtension string
)

// versionCmd provides `iris version` as an explicit subcommand in addition to
// the --version flag (cobra wires --version automatically from rootCmd.Version).
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the iris version and exit",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println(irisVersion)
		return nil
	},
}

// startup runs the iris initialisation sequence: resolve paths, load config,
// and open the DB. This is the D-3 startup contract.
func startup() error {
	p := iris.ResolvePaths()

	// Load config — absent file returns defaults, not an error.
	_, err := iris.LoadConfig(p.ConfigFile)
	if err != nil {
		return fmt.Errorf("iris: load config: %w", err)
	}

	// Open (or create) the iris DB.
	db, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris: open db: %w", err)
	}
	defer db.Close()

	fmt.Println("iris daemon initialised (D-3). Use 'iris spawn --worktree <path>' to start a session.")
	return nil
}

// spawnCmd spawns a single pi session and blocks until it completes.
// This provides a CLI entry point for testing D-3 without a full daemon.
var spawnCmd = &cobra.Command{
	Use:   "spawn",
	Short: "Spawn a pi session and run until it completes",
	RunE: func(cmd *cobra.Command, args []string) error {
		return spawnSession()
	},
}

func spawnSession() error {
	p := iris.ResolvePaths()

	cfg, err := iris.LoadConfig(p.ConfigFile)
	if err != nil {
		return fmt.Errorf("iris: load config: %w", err)
	}

	db, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris: open db: %w", err)
	}
	defer db.Close()

	// Ensure the run directory exists.
	if err := os.MkdirAll(p.RunDir, 0o700); err != nil {
		return fmt.Errorf("iris: create run dir: %w", err)
	}

	extensionPath := spawnExtension
	if extensionPath == "" {
		extensionPath = cfg.PIExtensionPath
	}

	superCfg := iris.SupervisorConfig{
		SessionName:      fmt.Sprintf("iris@%s", spawnRole),
		Worktree:         spawnWorktree,
		Role:             spawnRole,
		PIBinaryPath:     cfg.PIBinaryPath,
		ExtensionPath:    extensionPath,
		RestartThreshold: cfg.RestartThreshold,
		RunDir:           p.RunDir,
		Database:         db,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("[iris] spawning session (worktree=%s, role=%s)\n", spawnWorktree, spawnRole)

	sup, err := iris.SpawnSession(ctx, superCfg)
	if err != nil {
		return fmt.Errorf("iris: spawn: %w", err)
	}

	fmt.Printf("[iris] session %s running (socket: %s)\n",
		sup.InstanceID(), sup.SessionRecord().HarnessSockPath)

	// Block until the session terminates (supervisor's Start goroutine exits)
	// or context is cancelled.
	<-ctx.Done()
	return nil
}
