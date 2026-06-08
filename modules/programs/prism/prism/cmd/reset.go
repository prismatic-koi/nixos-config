package cmd

// prism reset — clean shutdown and optional restart of all prism sessions.
//
// Steps (each attempted independently — a failure in one does not abort others):
//
//  1. Kill the tmux server (equivalent to `tmux kill-server`).
//  2. Stop and remove all podman containers whose names match the "prism-" prefix.
//  3. Mark all non-ended rows in agent_status as ended (sets ended_at = now;
//     state is intentionally left at its last known value — ended_at IS NULL
//     is the canonical "active session" filter throughout the codebase) AND
//     clear the per-session pi conversation resume pointer
//     (agent_status.harness_session_id) on every row, so the next switch into
//     a previously-active project starts pi with a fresh conversation
//     (issue #1947).
//  4. Kill all sidecar processes and remove stale run files (PID, ready)
//     from ~/.local/state/prism/run/, and remove the per-session pi-agent
//     transcript JSONL subtree (issue #1947).
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
	"github.com/prismatic-koi/prism/internal/proglog"
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
  3. Mark all non-ended rows in agent_status as ended and clear the pi
     conversation resume pointer (harness_session_id) on every row.
  4. Terminate all sidecar processes (reads PID files from
     ~/.local/state/prism/run/) and remove pi-agent transcript JSONLs.
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
		proglog.Warnf("[prism reset] tmux kill-server: %v (continuing)\n", err)
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
		proglog.Warnf("[prism reset] isolator cleanup: %v (continuing)\n", err)
	}

	// ── Step 3: Mark all agent_status rows as ended ───────────────────────────
	fmt.Println("Marking all sessions as ended in DB...")
	if err := resetMarkDBEnded(); err != nil {
		proglog.Warnf("[prism reset] DB cleanup: %v (continuing)\n", err)
	}

	// ── Step 4: Kill sidecar processes ────────────────────────────────────────
	fmt.Println("Terminating sidecar processes...")
	if err := resetKillSidecars(); err != nil {
		proglog.Warnf("[prism reset] sidecar cleanup: %v (continuing)\n", err)
	}

	// ── Step 4b: Remove pi-agent transcript JSONLs ────────────────────────────
	// Reset means forget: drop the on-disk pi session JSONLs so that even if
	// a stale harness_session_id somehow survived (e.g. an external writer),
	// piResolveResumeSession returns false and pi starts a fresh chat.
	// See issue #1947.
	fmt.Println("Removing pi-agent transcript JSONLs...")
	if err := resetClearPiTranscripts(); err != nil {
		proglog.Warnf("[prism reset] transcript cleanup: %v (continuing)\n", err)
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
			proglog.Errorf("[prism reset] %s: %v\n", mode, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := iso.Reset(ctx); err != nil {
			proglog.Errorf("[prism reset] %s: %v\n", mode, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// resetMarkDBEnded opens the prism DB, calls MarkAllEnded to update all
// non-ended agent_status rows, then calls ClearAllResumePointers to wipe the
// per-session pi conversation resume pointer (harness_session_id) on every
// row. The two operations are independent (different columns) but conceptually
// paired by `prism reset`: end every session, then forget the resume pointer
// (issue #1947).
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

	// Wipe per-row resume pointers (harness_session_id). This is the DB-side
	// half of the issue #1947 fix; the FS-side half lives in
	// resetClearPiTranscripts.
	cleared, err := d.ClearAllResumePointers()
	if err != nil {
		return err
	}
	if cleared > 0 {
		fmt.Printf("  cleared pi resume pointer on %d row(s).\n", cleared)
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

// resetClearPiTranscripts removes the per-session pi-agent transcript JSONL
// subtree under each per-session run directory, and the sandbox-exec staging
// HOME equivalent. This is the filesystem-side half of the issue #1947 fix
// (the DB-side half is ClearAllResumePointers, called from resetMarkDBEnded).
//
// Layouts walked (all defensively skipped when missing):
//
//   bwrap:
//     $XDG_STATE_HOME/prism/run/<sessionDirHash>/pi-agent/sessions/
//
//   sandbox-exec (Darwin):
//     $XDG_STATE_HOME/prism/sessions/<instanceID>/home/.pi/agent/sessions/
//
//   host:
//     $PI_CODING_AGENT_DIR/sessions/ (when set) or ~/.pi/agent/sessions/
//     — NOT touched here. Host mode is already safe because
//     `prism switch` leaves opts.HarnessSessionID empty (see
//     internal/session/session.go ~line 181-182), so the host-mode
//     buildDirectAgentCmd never appends --session even if a transcript
//     remains on disk. Wiping the host PI sessions root would also touch
//     state belonging to non-prism pi invocations — strictly out of scope.
//     The host-mode resolution path now honours PI_CODING_AGENT_DIR (see
//     internal/harness/pi/archive.go::piSessionsRoot) but is unchanged here:
//     reset still skips host mode entirely.
//
// In every layout only the inner `.../sessions/` subtree is removed; the
// enclosing per-session directory (`<sessionDirHash>` or `<instanceID>/home`)
// is preserved because other state may live alongside the transcripts.
//
// All path joins are anchored at $XDG_STATE_HOME/prism/{run,sessions}/, so
// the walk never traverses outside the prism state root.
func resetClearPiTranscripts() error {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home dir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	prismRoot := filepath.Join(stateHome, "prism")

	removed := 0
	// Bwrap layout: prism/run/<sessionDirHash>/pi-agent/sessions/
	removed += removePiSessionsSubtree(filepath.Join(prismRoot, "run"), "pi-agent", "sessions")

	// Sandbox-exec layout: prism/sessions/<instanceID>/home/.pi/agent/sessions/
	removed += removePiSessionsSubtree(filepath.Join(prismRoot, "sessions"), "home", ".pi", "agent", "sessions")

	if removed == 0 {
		fmt.Println("  no pi-agent transcript subtrees found.")
	} else {
		fmt.Printf("  removed %d pi-agent transcript subtree(s).\n", removed)
	}
	return nil
}

// removePiSessionsSubtree iterates over the immediate children of parent (each
// expected to be a per-session directory like `<sessionDirHash>` or
// `<instanceID>`) and removes the `subPath...` subtree under each child. The
// child directory itself is preserved; only the inner subtree goes.
//
// Missing parent / missing subtree / non-dir child are silent no-ops — reset
// is best-effort and must never abort the rest of the pipeline.
//
// Returns the number of child directories under which a subtree was actually
// removed (i.e. the subtree existed before the call).
func removePiSessionsSubtree(parent string, subPath ...string) int {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return 0 // parent missing — nothing to do
	}
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		child := filepath.Join(parent, entry.Name())
		target := filepath.Join(append([]string{child}, subPath...)...)
		// Confirm the target is inside the parent root — belt-and-braces
		// against a malformed entry name like "..". filepath.Join cleans
		// the path so any traversal would surface here.
		rel, err := filepath.Rel(parent, target)
		if err != nil || rel == "" || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		if _, err := os.Stat(target); err != nil {
			continue // subtree absent under this child
		}
		if err := os.RemoveAll(target); err == nil {
			removed++
		}
	}
	return removed
}
