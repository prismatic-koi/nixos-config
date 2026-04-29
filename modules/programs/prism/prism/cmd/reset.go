package cmd

// prism reset — clean shutdown and optional restart of all prism sessions.
//
// Steps (each attempted independently — a failure in one does not abort others):
//
//  1. Kill the tmux server (equivalent to `tmux kill-server`).
//  2. Stop and remove all podman containers whose names match the "prism-" prefix.
//  3. Mark all non-ended rows in agent_status as ended (sets ended_at = now;
//     state is intentionally left at its last known value — ended_at IS NULL
//     is the canonical "active session" filter throughout the codebase).
//  4. Kill all sidecar processes and remove stale run files (PID, ready)
//     from ~/.local/state/prism/run/.
//  5. (Unless --no-launch) invoke `prism launch` to restart the server.
//
// Flags:
//
//	--yes       Skip the confirmation prompt (for scripting).
//	--no-launch Complete cleanup and exit without calling `prism launch`.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/container"
	prismSession "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Kill all prism sessions and optionally relaunch",
	Long: `Performs a clean shutdown of all prism sessions and infrastructure.

Steps (each attempted independently):
  1. Kill the tmux server (all sessions and panes).
  2. Stop and remove all podman containers with the "prism-" name prefix.
  3. Mark all non-ended rows in agent_status as ended.
  4. Terminate all sidecar processes (reads PID files from ~/.local/state/prism/run/).
  5. Invoke prism launch to restart (skipped with --no-launch).`,
	Args: cobra.NoArgs,
	RunE: runReset,
}

func init() {
	resetCmd.Flags().Bool("yes", false, "Skip the confirmation prompt")
	resetCmd.Flags().Bool("no-launch", false, "Exit after cleanup without calling prism launch")
	rootCmd.AddCommand(resetCmd)
}

func runReset(cmd *cobra.Command, _ []string) error {
	yesFlag, _ := cmd.Flags().GetBool("yes")
	noLaunch, _ := cmd.Flags().GetBool("no-launch")

	// Confirmation prompt (unless --yes).
	if !yesFlag {
		fmt.Print("This will kill all prism sessions. Continue? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		// ReadString returns (data, io.EOF) when it hits EOF before finding '\n'
		// (e.g. `echo -n y | prism reset`). Treat that as a valid answer only
		// when there is content; abort only when stdin was closed with nothing.
		if err != nil && (err != io.EOF || strings.TrimSpace(line) == "") {
			fmt.Fprintln(os.Stderr, "stdin closed — aborting (use --yes to skip prompt)")
			return nil
		}
		answer := strings.TrimSpace(strings.ToLower(line))
		if answer != "y" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	// ── Step 1: Kill tmux server ──────────────────────────────────────────────
	fmt.Println("Killing tmux server...")
	if _, err := tmux.Run("kill-server"); err != nil {
		// No server running is not an error — continue.
		fmt.Fprintf(os.Stderr, "[prism reset] tmux kill-server: %v (continuing)\n", err)
	} else {
		fmt.Println("  tmux server killed.")
	}

	// ── Step 2: Reset every registered isolator ──────────────────────────────
	// Post A1.L3 (issue #1140): the per-mode reset logic moved into each
	// Isolator's Reset method. `prism reset` iterates over the registered
	// modes and dispatches to each. Today only podman has a non-stub Reset
	// body (the prism-* container sweep); bwrap, sandbox-exec, and host
	// return nil — orphan-agent-run reaping is a future implementation.
	fmt.Println("Removing prism- podman containers...")
	if err := resetIsolators(); err != nil {
		fmt.Fprintf(os.Stderr, "[prism reset] isolator cleanup: %v (continuing)\n", err)
	}

	// ── Step 3: Mark all agent_status rows as ended ───────────────────────────
	fmt.Println("Marking all sessions as ended in DB...")
	if err := resetMarkDBEnded(); err != nil {
		fmt.Fprintf(os.Stderr, "[prism reset] DB cleanup: %v (continuing)\n", err)
	}

	// ── Step 4: Kill sidecar processes ────────────────────────────────────────
	fmt.Println("Terminating sidecar processes...")
	if err := resetKillSidecars(); err != nil {
		fmt.Fprintf(os.Stderr, "[prism reset] sidecar cleanup: %v (continuing)\n", err)
	}

	fmt.Println("Reset complete.")

	// ── Step 5: Launch ────────────────────────────────────────────────────────
	if noLaunch {
		return nil
	}

	fmt.Println("Relaunching prism...")
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}
	launchCmd := exec.Command(self, "launch")
	launchCmd.Stdout = os.Stdout
	launchCmd.Stderr = os.Stderr
	launchCmd.Stdin = os.Stdin
	return launchCmd.Run()
}

// resetIsolators iterates over every registered isolation mode and calls
// Reset on each one. Returns the first non-nil error encountered (other
// modes still get a chance to run). Today only podmanIsolator.Reset has a
// non-stub body — the prism-* container sweep (was: resetRemovePodmanContainers).
//
// Failures are logged at the call site (Step 2 in runReset); this function
// returns the error verbatim so the caller can decide whether to continue.
func resetIsolators() error {
	// Use a generous per-mode timeout so a slow `podman ps` does not starve
	// the rm step. The podman implementation issues two podman calls
	// (`ps -a` then `rm -f <ids...>`); 60 s covers both with margin.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var firstErr error
	for _, mode := range container.Names() {
		iso, err := container.For(mode, container.ConstructorOpts{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[prism reset] %s: %v\n", mode, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := iso.Reset(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "[prism reset] %s: %v\n", mode, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// resetMarkDBEnded opens the prism DB and calls MarkAllEnded to update all
// non-ended agent_status rows.
func resetMarkDBEnded() error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("open DB: %w", err)
	}
	defer d.Close()

	n, err := d.MarkAllEnded()
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("  no active sessions in DB.")
	} else {
		fmt.Printf("  marked %d session(s) as ended.\n", n)
	}
	return nil
}

// resetKillSidecars scans ~/.local/state/prism/run/ for *-sidecar.pid files,
// sends SIGTERM to each recorded process via session.KillSidecar, and then
// removes stale *-sidecar.ready files for the same sessions.
//
// Removing the .ready files prevents a re-launched session from finding stale
// readiness state from a prior run and misbehaving on startup.
//
// The directory may be absent (fresh install or already cleaned up) — that
// is treated as a no-op, not an error.
func resetKillSidecars() error {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home dir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	runDir := filepath.Join(stateHome, "prism", "run")

	entries, err := os.ReadDir(runDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("  no sidecar run dir found — skipping.")
			return nil
		}
		return fmt.Errorf("read run dir %s: %w", runDir, err)
	}

	killed := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, "-sidecar.pid") {
			continue
		}
		// Derive session name by stripping the "-sidecar.pid" suffix.
		sessionName := strings.TrimSuffix(name, "-sidecar.pid")

		// Send SIGTERM and remove the PID file.
		prismSession.KillSidecar(sessionName)

		// Remove stale readiness file so a re-launched session for the same
		// name starts fresh rather than picking up stale state.
		readyPath, _ := prismSession.SidecarReadyPath(sessionName)
		if readyPath != "" {
			_ = os.Remove(readyPath)
		}

		killed++
	}

	if killed == 0 {
		fmt.Println("  no sidecar PID files found.")
	} else {
		fmt.Printf("  sent termination signal to %d sidecar(s).\n", killed)
	}
	return nil
}
