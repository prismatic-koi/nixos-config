package cmd

// prism reset — clean shutdown and optional restart of all prism sessions.
//
// Steps (each attempted independently — a failure in one does not abort others):
//
//  1. Stop all sidecar processes found by PID file in
//     ~/.local/state/prism/run/, and remove the ready file of each stopped
//     sidecar. A session whose sidecar does not stop is kept: no later step
//     touches its tmux session, its row, its port, its resume pointer, its
//     transcript, or its instance directories, because the sidecar can
//     still use them.
//  2. Kill the tmux server (equivalent to `tmux kill-server`). If a session
//     is kept, kill every other tmux session instead.
//  3. Run each registered isolator's Reset sweep (all no-op stubs today).
//  4. Mark all non-ended rows in agent_status as ended (sets ended_at = now;
//     state is intentionally left at its last known value — ended_at IS NULL
//     is the canonical "active session" filter throughout the codebase) AND
//     clear the per-session pi conversation resume pointer
//     (agent_status.harness_session_id) on every row, so the next switch into
//     a previously-active project starts pi with a fresh conversation.
//     The (worktree, harness_session_id) pairs and the instance IDs are
//     snapshotted after step 1 and before step 2, so step 4b can scope its
//     transcript removal to exactly the sessions being reset, and step 4c
//     can remove their instance directories. Step 4b deletes exactly the pi
//     transcript JSONLs belonging to the snapshotted resume pointers from
//     the shared host pi sessions root.
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
	"slices"
	"strings"
	"sync"
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
  1. Stop all sidecar processes (reads PID files from
     ~/.local/state/prism/run/). A session whose sidecar does not stop
     keeps its tmux session and its row, and the later steps skip it.
  2. Kill the tmux server (all sessions and panes). If a session is kept,
     kill every other tmux session instead.
  3. Run each registered isolator's Reset sweep.
  4. Mark all non-ended rows in agent_status as ended, clear the pi
     conversation resume pointer (harness_session_id) on every row, and
     remove the pi transcript JSONLs belonging to the reset sessions from
     the shared pi sessions root.
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

	// ── Step 1: Stop sidecar processes ───────────────────────────────────────
	// Each sidecar stops before its tmux session and its row go (#3192).
	fmt.Println("Stopping sidecar processes...")
	kept, err := resetStopSidecars()
	if err != nil {
		proglog.Warnf("[prism reset] sidecar cleanup: %v (continuing)\n", err)
	}

	// Snapshot the resume pointers and the instance IDs before step 2. See
	// resetSnapshotSessions for why the order matters.
	resumePointers, instanceIDs, err := resetSnapshotSessions(kept...)
	if err != nil {
		proglog.Warnf("[prism reset] %v (transcript and instance-dir removal will be skipped)\n", err)
	}

	// ── Step 2: Kill tmux ────────────────────────────────────────────────────
	resetKillTmuxStep(kept)

	// ── Step 3: Reset every registered isolator ──────────────────────────────
	// The per-mode reset logic lives in each Isolator's Reset method.
	// `prism reset` iterates over the registered modes and dispatches to each.
	// Today every isolator's Reset is a no-op stub.
	fmt.Println("Running per-isolator reset sweeps...")
	if err := resetIsolators(); err != nil {
		proglog.Warnf("[prism reset] isolator cleanup: %v (continuing)\n", err)
	}

	// ── Step 4: Mark all agent_status rows as ended ───────────────────────────
	fmt.Println("Marking all sessions as ended in DB...")
	if err := resetMarkDBEnded(kept...); err != nil {
		proglog.Warnf("[prism reset] DB cleanup: %v (continuing)\n", err)
	}

	// ── Step 4b: Remove pi-agent transcript JSONLs ────────────────────────────
	// Reset means forget: drop the on-disk pi session JSONLs for the
	// snapshotted resume pointers so that even if a stale harness_session_id
	// somehow survived (e.g. an external writer), piResolveResumeSession
	// returns false and pi starts a fresh chat.
	fmt.Println("Removing pi-agent transcript JSONLs...")
	if err := resetClearPiTranscripts(resumePointers); err != nil {
		proglog.Warnf("[prism reset] transcript cleanup: %v (continuing)\n", err)
	}

	// ── Step 4c: Remove per-session instance directories ─────────────────────
	// The work dir, the per-session TMPDIR, and the podman-proxy audit dir are
	// keyed by instance ID, not by session name, and no other reset step
	// touches them. Every session snapshotted above (regardless of pi resume
	// linkage) gets these trees removed here, closing the gap where `prism
	// reset` left them behind for every session (issue #2960). The removals
	// are non-fatal and idempotent.
	// The prism-container audit dir goes only after a sweep of the
	// incarnation succeeds, because no later cleanup finds its service
	// containers and images without the dir.
	fmt.Println("Removing per-session instance directories...")
	for _, instanceID := range instanceIDs {
		removeSessionInstanceDirs(instanceID)
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
// Failures are logged at the call site (Step 3 in runReset); this function
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
// linkage — captured by resetSnapshotSessions BEFORE
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

// resetSnapshotSessions snapshots, from every agent_status row, the
// (worktree, harness_session_id) pairs that carry a resume pointer and the
// instance IDs. The FS half of the reset (resetClearPiTranscripts) deletes
// exactly those transcript JSONLs, and step 4c removes the instance
// directories of those instance IDs. The snapshot deliberately covers ended
// rows too — ClearAllResumePointers wipes the column on every row, and the
// FS half must match that scope.
//
// runReset takes the snapshot after the sidecar stops and before the tmux
// kill. Each killed tmux session fires the global session-closed hook,
// which clears instance_id of its row. A snapshot after the kill can then
// miss the instance ID, and the instance directories of that session stay
// on disk (#2960).
//
// The rows named in keep are left out, so that the later steps do not
// remove their transcript or their instance directories.
//
// The returned slices are valid even when err is non-nil — each reset step
// is best-effort and attempted independently. A snapshot failure degrades
// the FS half to a no-op; the DB-side clears (the load-bearing half) still
// run.
func resetSnapshotSessions(keep ...string) ([]piResumePointer, []string, error) {
	d, err := openDB()
	if err != nil {
		return nil, nil, fmt.Errorf("open DB: %w", err)
	}
	defer d.Close()

	// AllStatusesWithPrefix("") returns every agent_status row, active and
	// ended.
	//
	// instanceIDs is snapshotted from the same rows, independent of the
	// resume-pointer filter below: a session's instance-ID-keyed directory
	// trees (work dir, podman-proxy audit dir) exist regardless of whether
	// the row carries a pi resume pointer.
	statuses, err := d.AllStatusesWithPrefix("")
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot sessions: %w", err)
	}
	var pointers []piResumePointer
	var instanceIDs []string
	for _, s := range statuses {
		if slices.Contains(keep, s.SessionName) {
			continue
		}
		if s.InstanceID != nil && *s.InstanceID != "" {
			instanceIDs = append(instanceIDs, *s.InstanceID)
		}
		if s.HarnessSessionID == nil || *s.HarnessSessionID == "" || s.Worktree == "" {
			continue
		}
		pointers = append(pointers, piResumePointer{
			sessionName:      s.SessionName,
			worktree:         s.Worktree,
			harnessSessionID: *s.HarnessSessionID,
		})
	}
	return pointers, instanceIDs, nil
}

// resetMarkDBEnded opens the prism DB, calls MarkAllEnded to update all
// non-ended agent_status rows, then calls ClearAllResumePointers to wipe the
// per-session pi conversation resume pointer (harness_session_id) on every
// row. The two operations are independent (different columns) but conceptually
// paired by `prism reset`: end every session, then forget the resume pointer.
// resetSnapshotSessions runs first, so that the FS half of the reset knows
// which transcripts belong to the cleared pointers.
//
// The rows named in keep are left as they are: they stay open and keep
// their resume pointer.
func resetMarkDBEnded(keep ...string) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("open DB: %w", err)
	}
	defer d.Close()

	n, err := d.MarkAllEnded(keep...)
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("  no active sessions in DB.")
	} else {
		fmt.Printf("  marked %d session(s) as ended.\n", n)
	}

	// Wipe per-row resume pointers (harness_session_id). This is the DB-side
	// half of the resume-pointer clear; the FS-side half lives in
	// resetClearPiTranscripts.
	cleared, err := d.ClearAllResumePointers(keep...)
	if err != nil {
		return err
	}
	if cleared > 0 {
		fmt.Printf("  cleared pi resume pointer on %d row(s).\n", cleared)
	}
	return nil
}

// resetStopSidecar is the sidecar stop of prism reset. Tests replace it, so
// that a test signals no process.
var resetStopSidecar = func(sessionName string) bool {
	return prismSession.EnsureSidecarStopped(sessionName, prismSession.SidecarStopTimeout)
}

// resetStopSidecars scans ~/.local/state/prism/run/ for *-sidecar.pid files
// and stops the sidecar of each one. The stops run at the same time, so
// that several slow stops cost one stop timeout. For each sidecar that
// stopped, it removes the *-sidecar.ready file, so that a re-launched
// session for the same name does not find stale readiness state.
//
// It returns the sessions whose sidecar did not stop. Their PID file and
// ready file stay, so that a later stop finds the process again.
//
// The directory may be absent (fresh install or already cleaned up) — that
// is treated as a no-op, not an error.
func resetStopSidecars() ([]string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	runDir := filepath.Join(stateHome, "prism", "run")

	entries, err := os.ReadDir(runDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("  no sidecar run dir found — skipping.")
			return nil, nil
		}
		return nil, fmt.Errorf("read run dir %s: %w", runDir, err)
	}

	var sessions []string
	for _, entry := range entries {
		if name, ok := strings.CutSuffix(entry.Name(), "-sidecar.pid"); ok {
			sessions = append(sessions, name)
		}
	}
	if len(sessions) == 0 {
		fmt.Println("  no sidecar PID files found.")
		return nil, nil
	}

	stopped := make([]bool, len(sessions))
	var wg sync.WaitGroup
	for i, name := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stopped[i] = resetStopSidecar(name)
		}()
	}
	wg.Wait()

	var kept []string
	for i, name := range sessions {
		if !stopped[i] {
			proglog.Warnf("[prism reset] warning: sidecar of %q did not stop — keeping its tmux session and row (continuing)\n", name)
			kept = append(kept, name)
			continue
		}
		if readyPath, _ := prismSession.SidecarReadyPath(name); readyPath != "" {
			_ = os.Remove(readyPath)
		}
	}
	fmt.Printf("  stopped %d sidecar(s).\n", len(sessions)-len(kept))
	if len(kept) > 0 {
		fmt.Printf("  %d sidecar(s) did not stop: %s.\n", len(kept), strings.Join(kept, ", "))
	}
	return kept, nil
}

// resetKillTmuxStep is step 2 of runReset. Tests replace it, so that a test
// can act as the session-closed hook of each killed tmux session.
var resetKillTmuxStep = resetKillTmux

// resetKillTmux kills the tmux server. When kept names a session, the
// server stays, and every tmux session that kept does not name is killed.
// Each kill uses the exact-match target "=<name>": a plain target can also
// match a session by name prefix, and the kill must not reach a kept
// session.
func resetKillTmux(kept []string) {
	if len(kept) == 0 {
		fmt.Println("Killing tmux server...")
		if _, err := tmux.Run("kill-server"); err != nil {
			// No server running is not an error — continue.
			proglog.Warnf("[prism reset] tmux kill-server: %v (continuing)\n", err)
		} else {
			fmt.Println("  tmux server killed.")
		}
		return
	}

	fmt.Println("Killing tmux sessions...")
	out, err := tmux.Run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		// No server running is not an error — continue.
		proglog.Warnf("[prism reset] tmux list-sessions: %v (continuing)\n", err)
		return
	}
	killed := 0
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name == "" || slices.Contains(kept, name) {
			continue
		}
		if _, err := tmux.Run("kill-session", "-t", "="+name); err != nil {
			proglog.Warnf("[prism reset] tmux kill-session %q: %v (continuing)\n", name, err)
			continue
		}
		killed++
	}
	fmt.Printf("  killed %d tmux session(s). The tmux server stays for the %d session(s) whose sidecar did not stop.\n", killed, len(kept))
}

// resetClearPiTranscripts deletes the on-disk pi transcript JSONLs belonging
// to the sessions being reset. This is the filesystem-side half of the
// resume-pointer clear (the DB-side half is ClearAllResumePointers, called
// from resetMarkDBEnded). It removes transcripts from the shared host pi
// sessions root where pi writes transcripts in EVERY isolation mode:
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
// see archiveThenSeverPiResume), reset deletes WITHOUT archiving: reset is
// the destructive "forget everything" surface (Reset means forget); cleanup
// is the lifecycle path that preserves history.
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
