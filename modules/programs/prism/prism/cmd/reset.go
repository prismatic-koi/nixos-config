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
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

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

	// ── Step 2: Remove prism- podman containers ───────────────────────────────
	fmt.Println("Removing prism- podman containers...")
	if err := resetRemovePodmanContainers(); err != nil {
		fmt.Fprintf(os.Stderr, "[prism reset] podman cleanup: %v (continuing)\n", err)
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

// resetRemovePodmanContainers lists all podman containers whose names start
// with "prism-" and removes them. If podman is not available or returns no
// containers, the function is a no-op.
func resetRemovePodmanContainers() error {
	podmanBin, err := exec.LookPath("podman")
	if err != nil {
		// podman not installed — nothing to do.
		fmt.Println("  podman not found — skipping container cleanup.")
		return nil
	}

	// Fetch "name\tid" for every container (including stopped ones).
	//
	// We avoid --filter name=prism- because podman's name filter is a
	// substring match, not a prefix match — it would incorrectly include
	// containers like "not-prism-foo". Instead we do the prefix check
	// client-side after fetching all names.
	//
	// We avoid --format {{.Names}} alone because podman emits comma-separated
	// values for containers with multiple aliases (e.g. "prism-foo,prism-foo-alias"),
	// which breaks `podman rm -f` when passed as a single argument. Using
	// {{.ID}} gives a stable, unique identifier; we only use {{.Names}} here
	// to decide which containers to target.
	out, err := exec.Command(podmanBin, "ps", "-a", "--format", "{{.Names}}\t{{.ID}}").Output()
	if err != nil {
		// If podman fails (e.g. no running machine), treat as no containers.
		fmt.Printf("  podman ps failed: %v — skipping container cleanup.\n", err)
		return nil
	}

	var prismContainers []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		// parts[0] may be "name" or "name1,name2,..."; check the first name only.
		firstName := strings.SplitN(parts[0], ",", 2)[0]
		if strings.HasPrefix(firstName, "prism-") {
			prismContainers = append(prismContainers, strings.TrimSpace(parts[1]))
		}
	}

	if len(prismContainers) == 0 {
		fmt.Println("  no prism- containers found.")
		return nil
	}

	fmt.Printf("  removing %d container(s)\n", len(prismContainers))
	args := append([]string{"rm", "-f"}, prismContainers...)
	rmOut, err := exec.Command(podmanBin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman rm -f: %w\n%s", err, strings.TrimSpace(string(rmOut)))
	}
	fmt.Println("  containers removed.")
	return nil
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

		// Remove stale readiness and session-ID files so a re-launched session
		// for the same name starts fresh rather than picking up stale state.
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
