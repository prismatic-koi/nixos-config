package cmd

// prism reset — clean shutdown and optional restart of all prism sessions.
//
// Steps (each attempted independently — a failure in one does not abort others):
//
//  1. Kill the tmux server (equivalent to `tmux kill-server`).
//  2. Run each registered isolator's Reset sweep (all no-op stubs today).
//  3. Mark all non-ended rows in agent_status as ended (sets ended_at = now;
//     state is intentionally left at its last known value — ended_at IS NULL
//     is the canonical "active session" filter throughout the codebase) AND
//     clear the per-session pi conversation resume pointer
//     (agent_status.harness_session_id) on every row, so the next switch into
//     a previously-active project starts pi with a fresh conversation
//     (issue #1947). The (worktree, harness_session_id) pairs are
//     snapshotted BEFORE the clear so step 4b can scope its transcript
//     removal to exactly the sessions being reset (issue #2220).
//  4. Kill all sidecar processes and remove stale run files (PID, ready)
//     from ~/.local/state/prism/run/, and delete exactly the pi transcript
//     JSONLs belonging to the snapshotted resume pointers from the shared
//     host pi sessions root (issues #1947, #2220).
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
  2. Run each registered isolator's Reset sweep.
  3. Mark all non-ended rows in agent_status as ended and clear the pi
     conversation resume pointer (harness_session_id) on every row.
  4. Terminate all sidecar processes (reads PID files from
     ~/.local/state/prism/run/) and remove the pi transcript JSONLs
     belonging to the reset sessions from the shared pi sessions root.
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
	// modes and dispatches to each. Today every isolator's Reset is a no-op
	// stub — orphan-agent-run reaping is a future implementation.
	fmt.Println("Running per-isolator reset sweeps...")
	if err := resetIsolators(); err != nil {
		proglog.Warnf("[prism reset] isolator cleanup: %v (continuing)\n", err)
	}

	// ── Step 3: Mark all agent_status rows as ended ───────────────────────────
	// Also snapshots the pi resume pointers (worktree + harness_session_id)
	// before clearing them, so step 4b can scope its transcript removal.
	fmt.Println("Marking all sessions as ended in DB...")
	resumePointers, err := resetMarkDBEnded()
	if err != nil {
		proglog.Warnf("[prism reset] DB cleanup: %v (continuing)\n", err)
	}

	// ── Step 4: Kill sidecar processes ────────────────────────────────────────
	fmt.Println("Terminating sidecar processes...")
	if err := resetKillSidecars(); err != nil {
		proglog.Warnf("[prism reset] sidecar cleanup: %v (continuing)\n", err)
	}

	// ── Step 4b: Remove pi-agent transcript JSONLs ────────────────────────────
	// Reset means forget: drop the on-disk pi session JSONLs for the
	// snapshotted resume pointers so that even if a stale harness_session_id
	// somehow survived (e.g. an external writer), piResolveResumeSession
	// returns false and pi starts a fresh chat. See issues #1947 and #2220.
	fmt.Println("Removing pi-agent transcript JSONLs...")
	if err := resetClearPiTranscripts(resumePointers); err != nil {
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
// modes still get a chance to run). Today every Reset body is a no-op stub.
//
// Failures are logged at the call site (Step 2 in runReset); this function
// returns the error verbatim so the caller can decide whether to continue.
func resetIsolators() error {
	// Use a generous per-mode timeout so a slow sweep in one isolator does
	// not starve the others.
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

// piResumePointer is the snapshot of one agent_status row's pi resume
// linkage — captured by resetMarkDBEnded immediately BEFORE
// ClearAllResumePointers nulls the harness_session_id column. The FS half of
// the reset (resetClearPiTranscripts) consumes the snapshot to delete exactly
// the transcript JSONLs belonging to the sessions being reset, mirroring
// cleanup's capture-before-clear pattern in severPiResumeLinkage
// (cmd/cleanup.go).
type piResumePointer struct {
	sessionName      string
	worktree         string
	harnessSessionID string
}

// resetMarkDBEnded opens the prism DB, calls MarkAllEnded to update all
// non-ended agent_status rows, then calls ClearAllResumePointers to wipe the
// per-session pi conversation resume pointer (harness_session_id) on every
// row. The two operations are independent (different columns) but conceptually
// paired by `prism reset`: end every session, then forget the resume pointer
// (issue #1947).
//
// Before the clear, the (worktree, harness_session_id) pairs of every row
// carrying a resume pointer are snapshotted and returned so the FS half of
// the reset (resetClearPiTranscripts) can delete exactly those transcript
// JSONLs (issue #2220). The snapshot deliberately covers ended rows too —
// ClearAllResumePointers wipes the column on every row, and the FS half must
// match that scope. A snapshot failure is logged and degrades the FS half to
// a no-op; the DB-side clears (the load-bearing half) still run.
//
// The returned slice is valid even when err is non-nil, so the caller can
// still hand whatever was captured to resetClearPiTranscripts — each reset
// step is best-effort and attempted independently.
func resetMarkDBEnded() ([]piResumePointer, error) {
	d, err := openDB()
	if err != nil {
		return nil, fmt.Errorf("open DB: %w", err)
	}
	defer d.Close()

	// Snapshot resume pointers BEFORE ClearAllResumePointers nulls the
	// column. AllStatusesWithPrefix("") returns every agent_status row,
	// active and ended.
	var pointers []piResumePointer
	statuses, err := d.AllStatusesWithPrefix("")
	if err != nil {
		proglog.Warnf("[prism reset] snapshot resume pointers: %v (transcript removal will be skipped)\n", err)
	}
	for _, s := range statuses {
		if s.HarnessSessionID == nil || *s.HarnessSessionID == "" || s.Worktree == "" {
			continue
		}
		pointers = append(pointers, piResumePointer{
			sessionName:      s.SessionName,
			worktree:         s.Worktree,
			harnessSessionID: *s.HarnessSessionID,
		})
	}

	n, err := d.MarkAllEnded()
	if err != nil {
		return pointers, err
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
		return pointers, err
	}
	if cleared > 0 {
		fmt.Printf("  cleared pi resume pointer on %d row(s).\n", cleared)
	}
	return pointers, nil
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

// resetClearPiTranscripts deletes the on-disk pi transcript JSONLs belonging
// to the sessions being reset. This is the filesystem-side half of the issue
// #1947 fix (the DB-side half is ClearAllResumePointers, called from
// resetMarkDBEnded), re-scoped by issue #2220 to the shared host pi sessions
// root where pi actually writes transcripts in EVERY isolation mode
// (post #2186/#2210):
//
//	$PI_CODING_AGENT_DIR/sessions/--<encoded-cwd>--/<ts>_<uuid>.jsonl
//	(or ~/.pi/agent/sessions/... when the env var is unset)
//
// That root is shared across all repos, sessions, and non-prism pi
// invocations on the host, so it must NEVER be swept wholesale. Instead the
// removal is keyed off the DB rows being reset: for each snapshotted
// (worktree, harness_session_id) pair, exactly the *_<id>.jsonl files in that
// worktree's encoded-cwd directory are deleted — the same per-file
// granularity as cleanup's severPiResumeLinkage, via the same
// container.RemovePiResumeJSONL(Count) mechanism. Conversations the DB does
// not know about (other repos' sessions, non-prism pi chats, sessions whose
// linkage was already severed) are untouched.
//
// Unlike `prism cleanup` (which archives the transcript before severing —
// see archiveThenSeverPiResume, issue #2219), reset deletes WITHOUT
// archiving: reset is the destructive "forget everything" surface
// (issue #1947: "Reset means forget"); cleanup is the lifecycle path that
// preserves history.
//
// The pre-#2220 implementation instead swept two dead layouts — the
// pre-#1985 bwrap run-dir layout ($XDG_STATE_HOME/prism/run/<hash>/pi-agent/
// sessions/) and the sandbox-exec staging HOME ($XDG_STATE_HOME/prism/
// sessions/<instanceID>/home/.pi/agent/sessions/, which pi never wrote to) —
// and was therefore a silent no-op in every current isolation mode.
//
// Best-effort: a per-pointer removal failure is logged and the remaining
// pointers are still processed; the first error is returned so the caller
// can emit its aggregate warning without aborting the reset pipeline.
func resetClearPiTranscripts(pointers []piResumePointer) error {
	if len(pointers) == 0 {
		fmt.Println("  no pi resume pointers recorded — nothing to remove.")
		return nil
	}
	removed := 0
	var firstErr error
	for _, p := range pointers {
		n, err := container.RemovePiResumeJSONLCount(container.Config{
			SessionName:      p.sessionName,
			Worktree:         p.worktree,
			HarnessSessionID: p.harnessSessionID,
		})
		removed += n
		if err != nil {
			proglog.Warnf("[prism reset] remove pi transcript for %s: %v (continuing)\n", p.sessionName, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if removed == 0 {
		fmt.Println("  no matching pi transcript JSONLs found.")
	} else {
		fmt.Printf("  removed %d pi transcript JSONL(s) for %d resume pointer(s).\n", removed, len(pointers))
	}
	return firstErr
}
