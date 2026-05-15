// Command iris is the daemon-mode successor to prism (codename iris, D-3/D-6).
//
// D-3 adds: spawn, harness-socket dispatch, tool override registration.
// D-6 adds: client IPC socket (iris.sock), session fan-out, subscribe/replay.
// D-8 adds: bubbletea TUI (iris tui) — session list + live event stream + prompt delivery.
// Issue #1668: `iris spawn` routes through the running daemon (no in-process supervisor).
// See docs/daemon-mode-design.md §3 and §4 for the architecture.
//
// Usage:
//
//	iris --version                              — print version string and exit 0
//	iris version                                — same, as a subcommand
//	iris daemon                                 — start the full daemon (client socket + sessions)
//	iris spawn --worktree <path> [--role <role>] — ask the running daemon to spawn a pi session
//	iris tui [--socket <path>]                  — open the bubbletea TUI
//	iris sessions list [--json]                 — list daemon-tracked sessions (human or JSON)
//	iris sessions status [--json]               — print session counts by state
//	iris prompt <session> --prompt <text>       — deliver a prompt to a running session
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/iris"
)

// irisVersion is the version string for the iris binary. It is set to the
// package version at compile time; in development builds it is "dev".
//
// This intentionally does NOT reuse the prism version string or any
// prism-internal ldflags variable — iris has its own identity.
const irisVersion = "0.1.0-d8"

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
	// spawnCmd, sessionsCmd and promptCmd are registered in their own files'
	// init() functions so each subcommand and its flags live together.
	rootCmd.AddCommand(daemonCmd)
	rootCmd.AddCommand(tuiCmd)
}

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
// open the DB, run the D-9 restore sequence, then enter the daemon loop.
func startup() error {
	p := iris.ResolvePaths()

	// Load config — absent file returns defaults, not an error.
	cfg, err := iris.LoadConfig(p.ConfigFile)
	if err != nil {
		return fmt.Errorf("iris: load config: %w", err)
	}

	// Open (or create) the iris DB.
	db, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris: open db: %w", err)
	}
	defer db.Close()

	// Ensure the run directory exists before attempting restore.
	if err := os.MkdirAll(p.RunDir, 0o700); err != nil {
		return fmt.Errorf("iris: create run dir: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// D-9: run the daemon-restart restore sequence. This detects orphaned
	// tool calls (writing synthetic tool_results), reconciles sessions that
	// were in spawning state (marking them error), and re-spawns sessions
	// that were active when the daemon died.
	extensionPath := cfg.PIExtensionPath
	restoreCfg := iris.RestoreConfig{
		Database: db,
		RunDir:   p.RunDir,
		SupervisorTemplate: iris.SupervisorConfig{
			PIBinaryPath:     cfg.PIBinaryPath,
			ExtensionPath:    extensionPath,
			RestartThreshold: cfg.RestartThreshold,
			RunDir:           p.RunDir,
			LogDir:           p.LogDir,
			Database:         db,
		},
	}
	restoreResult, err := iris.RunRestore(ctx, restoreCfg)
	if err != nil {
		// Non-fatal: log and continue.
		fmt.Fprintf(os.Stderr, "[iris] restore: %v\n", err)
	} else {
		fmt.Printf("[iris] restore complete: spawning_errors=%d orphans=%d restored=%d skipped=%d\n",
			restoreResult.SpawningMarkError,
			restoreResult.OrphansWritten,
			restoreResult.SessionsRestored,
			restoreResult.SessionsSkipped,
		)
	}

	fmt.Println("iris daemon initialised (D-9). Use 'iris daemon' to start the full daemon, or 'iris spawn --worktree <path>' to test a session.")
	<-ctx.Done()
	return nil
}

// daemonCmd starts the full iris daemon: client IPC socket + session manager.
// Clients connect to ~/.local/state/iris/iris.sock to list/subscribe/spawn
// sessions. This is the D-6 entry point.
var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Start the iris daemon (client socket + session manager)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDaemon()
	},
}

// daemonState holds the in-memory runtime state of the iris daemon.
type daemonState struct {
	mu          sync.Mutex
	supervisors map[string]*iris.Supervisor // session name → supervisor
}

func (ds *daemonState) addSupervisor(name string, sup *iris.Supervisor) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.supervisors[name] = sup
}

func (ds *daemonState) removeSupervisor(name string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	delete(ds.supervisors, name)
}

func (ds *daemonState) activeSessions() []iris.SessionSnapshot {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	out := make([]iris.SessionSnapshot, 0, len(ds.supervisors))
	for _, sup := range ds.supervisors {
		rec := sup.SessionRecord()
		out = append(out, iris.SessionSnapshot{
			Name:             rec.SessionName,
			InstanceID:       rec.InstanceID,
			State:            string(rec.State),
			Role:             rec.Role,
			Worktree:         rec.Worktree,
			StartedAt:        rec.StartedAt.UTC().Format(time.RFC3339),
			HarnessSessionID: rec.PiSessionPath,
		})
	}
	return out
}

// runDaemon initialises the iris daemon and blocks until the context is
// cancelled. It:
//
//  1. Resolves paths and loads config.
//  2. Opens the iris DB.
//  3. Creates and starts the client IPC socket.
//  4. Waits for SIGINT/SIGTERM.
func runDaemon() error {
	p := iris.ResolvePaths()

	cfg, err := iris.LoadConfig(p.ConfigFile)
	if err != nil {
		return fmt.Errorf("iris: load config: %w", err)
	}

	database, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris: open db: %w", err)
	}
	defer database.Close()

	// Ensure the run directory exists with 0700 (parent of per-session dirs).
	if err := os.MkdirAll(p.RunDir, 0o700); err != nil {
		return fmt.Errorf("iris: create run dir: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	state := &daemonState{
		supervisors: make(map[string]*iris.Supervisor),
	}

	// deliverFn is called by the client socket when a prompt_deliver frame arrives.
	// It sends the prompt via the supervisor's RPC channel.
	deliverFn := func(_ context.Context, name, text, deliverAs string, images []string) error {
		state.mu.Lock()
		sup, ok := state.supervisors[name]
		state.mu.Unlock()
		if !ok {
			return fmt.Errorf("session %q not found", name)
		}
		kind := deliverAs
		if kind == "" {
			kind = "prompt"
		}
		return sup.SendRPC(map[string]any{
			"type":    kind,
			"message": text,
			"images":  images,
		})
	}

	// Create the client IPC socket first so spawnFn can capture it and wire
	// each new harness as a publisher (D-6 fan-out). spawnFn is passed to
	// NewClientSocket as ClientSocketConfig.SpawnSession.
	clientSock := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath:          p.Sock,
		Database:          database,
		GetActiveSessions: state.activeSessions,
		// SpawnSession is assigned below after spawnFn is defined.
		DeliverPrompt: deliverFn,
	})

	// spawnFn is called by the client socket when a session_spawn frame arrives.
	// It must be defined after clientSock so it can capture clientSock and wire
	// the harness publisher (D-6 fan-out: harness events → client subscribers).
	spawnFn := func(spawnCtx context.Context, worktree, role string, configOverrides map[string]any) (*iris.Supervisor, error) {
		extPath := cfg.PIExtensionPath
		// Derive the bare repo root from the worktree for 4-PAT GITHUB_TOKEN
		// selection in the bash sandbox (D-5).
		bareRoot := git.BareRoot(worktree)
		// Apply the iris default-agent rule when role is empty. Explicit role
		// from session_spawn wins; on miss, basename=="main" under a .bare
		// parent → "coordinator", otherwise → "worker".
		resolvedRole := iris.ResolveAgent(worktree, role)
		if resolvedRole == "" {
			resolvedRole = "worker"
		}
		superCfg := iris.SupervisorConfig{
			SessionName:      iris.GenerateSessionName(worktree, resolvedRole),
			Worktree:         worktree,
			Role:             resolvedRole,
			BareRoot:         bareRoot,
			PIBinaryPath:     cfg.PIBinaryPath,
			ExtensionPath:    extPath,
			RestartThreshold: cfg.RestartThreshold,
			RunDir:           p.RunDir,
			LogDir:           p.LogDir,
			Database:         database,
			// Wire the client socket as the harness publisher so that every
			// harness event is fanned out to all subscribed clients in real time.
			Publisher: clientSock,
		}
		sup, err := iris.SpawnSession(ctx, superCfg)
		if err != nil {
			return nil, err
		}
		state.addSupervisor(superCfg.SessionName, sup)
		go func() {
			// When the session terminates, remove it from the live map.
			// SpawnSession starts the supervisor in a goroutine; we detect
			// termination by polling the session record state.
			for {
				rec := sup.SessionRecord()
				if rec.State == iris.StateFinished || rec.State == iris.StateError {
					state.removeSupervisor(superCfg.SessionName)
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-waitMillis(500):
				}
			}
		}()
		return sup, nil
	}
	// Wire the spawn function now that it is defined.
	clientSock.SetSpawnSession(spawnFn)
	if err := clientSock.Listen(); err != nil {
		return fmt.Errorf("iris: client socket: %w", err)
	}
	defer clientSock.Close()

	go clientSock.Serve(ctx)

	log.Printf("[iris] daemon running. Client socket: %s", p.Sock)
	fmt.Printf("[iris] daemon ready. Connect via: %s\n", p.Sock)

	<-ctx.Done()
	log.Println("[iris] daemon shutting down")
	return nil
}

// waitMillis returns a channel that closes after n milliseconds.
func waitMillis(n int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		time.Sleep(time.Duration(n) * time.Millisecond)
	}()
	return ch
}
