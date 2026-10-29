package cmd

// prism restore — recreate tmux sessions from prism.db.
//
// Reads agent_status rows where ended_at IS NULL and recreates any sessions
// that are no longer present in the running tmux server. Sessions that already
// exist are skipped silently — safe to call more than once. A row whose
// session name is outside sessionname.SessionNameSafeSet is reported on
// stderr, counted as failed, and not restored.
//
// Stagger: a configurable delay (default 500ms) is inserted between successive
// session creates to flatten the sidecar startup burst on machines with many
// sessions. The delay only applies to sessions that are actually being created;
// it is skipped for already-running sessions and for sessions being marked
// ended due to missing worktrees. It is also fully skipped in dry-run mode.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sessionname"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// restoreOutcome describes what restoreSession did for a given session.
type restoreOutcome int

const (
	// restoreOutcomeSkipped means the session was already running or was
	// marked ended (missing worktree). No sidecar was started; no stagger
	// delay should be applied after this call.
	restoreOutcomeSkipped restoreOutcome = iota

	// restoreOutcomeCreated means the session was successfully created.
	// A stagger delay should be applied after this call (if enabled).
	restoreOutcomeCreated
)

// loadRestoreConfig returns the active prism configuration for restore.
// It is a package-level variable so tests can override it to exercise
// container-mode and host-mode restore paths without depending on the
// process-wide config.Load() singleton cache.
var loadRestoreConfig = func() config.Config { return config.Load() }

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Recreate tmux sessions from prism.db",
	Long: `Recreate tmux sessions from prism.db.

With no flag, restore recreates each live session that has no tmux session.
It does not touch a session whose tmux session exists.

With --session <name>, restore restarts that one session, also when its tmux
session exists: it closes the tmux session, stops the sidecar, and starts the
sidecar and the agent again, as after prism restart. The conversation of the
agent continues. Use it to recover a dead agent pane: a bare
'tmux respawn-pane' restarts the agent and not its sidecar. Run it in a host
shell, not in the session that it restarts.`,
	RunE: runRestore,
}

func init() {
	restoreCmd.Flags().Bool("dry-run", false, "Print what would be restored without creating sessions")
	restoreCmd.Flags().String("session", "", "Restart this one session: its tmux session, its sidecar, and its agent")
	rootCmd.AddCommand(restoreCmd)
}

func runRestore(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if sessionName, _ := cmd.Flags().GetString("session"); sessionName != "" {
		return RestoreOneSession(sessionName, dryRun)
	}
	return Restore(dryRun)
}

// RestoreOneSession restarts the live session sessionName: the tmux session,
// the sidecar, and the agent (#3345). It is the recovery for a dead agent
// pane. A respawn of the pane alone can start an agent with no working
// sidecar.
func RestoreOneSession(sessionName string, dryRun bool) error {
	// A sandbox has no tmux server of the host and no prism.db.
	if os.Getenv("PRISM_HOST_API") != "" {
		return errors.New("prism restore --session: run this command in a host shell, not in a sandbox")
	}
	if err := sessionname.ValidateName(sessionName); err != nil {
		return fmt.Errorf("prism restore --session: %w", err)
	}
	// The restart closes the tmux session, and with it the shell of a
	// caller in that session, before the new session exists.
	if pane := os.Getenv("TMUX_PANE"); pane != "" && os.Getenv("TMUX") != "" {
		if own, err := tmux.Run("display-message", "-p", "-t", pane, "#{session_name}"); err == nil && own == sessionName {
			return fmt.Errorf("prism restore --session: this shell is in session %q, and the restart closes it. Run the command from another session or from a shell outside tmux", sessionName)
		}
	}
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("prism restore --session: cannot open DB: %w", err)
	}
	defer d.Close()
	return restoreOneSession(d, sessionName, dryRun)
}

// restoreOneSession is RestoreOneSession after the checks of the caller.
func restoreOneSession(d *db.DB, sessionName string, dryRun bool) error {
	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		return fmt.Errorf("prism restore --session: read session %q: %w", sessionName, err)
	}
	if st == nil || st.EndedAt != nil {
		return fmt.Errorf("prism restore --session: prism.db has no live session %q. Start it with prism switch or prism spawn", sessionName)
	}
	if session.IsMetaSession(sessionName) {
		return fmt.Errorf("prism restore --session: %q is not a project session", sessionName)
	}
	if dryRun {
		fmt.Printf("would restart: %s (worktree=%s)\n", sessionName, st.Worktree)
		return nil
	}

	// Close the tmux session first, as prism restart closes the tmux server
	// first. tmux.KillSession renames the session before the kill, so the
	// session-closed hook does not end the row. restoreSession then stops
	// the old sidecar, keeps the merge queue and the incarnation, and starts
	// the new sidecar and the agent.
	if session.HasExactSession(sessionName) {
		if err := tmux.KillSession(sessionName); err != nil {
			return fmt.Errorf("prism restore --session: close tmux session %q: %w", sessionName, err)
		}
	}
	pendingStagger := false
	outcome, err := restoreSession(d, *st, &pendingStagger, 0)
	if err != nil {
		return fmt.Errorf("prism restore --session %q: %w", sessionName, err)
	}
	if outcome != restoreOutcomeCreated {
		return fmt.Errorf("prism restore --session: session %q was not restarted (see the message above)", sessionName)
	}
	return nil
}

func Restore(dryRun bool) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("prism restore: cannot open DB: %w", err)
	}
	defer d.Close()

	// Prune old events/messages once at restore time.
	if err := d.Prune(90 * 24 * time.Hour); err != nil {
		fmt.Fprintf(os.Stderr, "prism restore: prune: %v\n", err)
		// Non-fatal — continue with restore.
	}
	// Shorter retention for the raw harness wire archive.
	if err := d.PruneHarnessFrames(7 * 24 * time.Hour); err != nil {
		fmt.Fprintf(os.Stderr, "prism restore: prune harness_frames: %v\n", err)
	}

	// Remove any stale dashboard socket left behind by a crashed dashboard.
	// Non-fatal: a stale socket only affects real-time push delivery.
	dashboard.RemoveStaleSocket()

	statuses, err := d.AllActiveStatus()
	if err != nil {
		return fmt.Errorf("prism restore: query sessions: %w", err)
	}

	// Load config once to read the stagger delay.
	// loadRestoreConfig is a package-level var so tests can override it.
	cfg := loadRestoreConfig()
	staggerDelay := cfg.RestoreStaggerDelay()

	// pendingStagger is set to true after each successful session.Create. It is
	// consumed (a sleep is applied) immediately before the NEXT actual create,
	// not before skipped sessions. This ensures the delay only fires between
	// real sidecar starts.
	//
	// Using a pointer so that restoreProjectSession can consume it without an
	// extra return value: the function sleeps before session.Create if
	// *pendingStagger is true, then resets it to false.
	pendingStagger := false

	// Aggregate counts for the final summary line.
	// restored:  outcome == restoreOutcomeCreated
	// skipped:   outcome == restoreOutcomeSkipped
	// failed:    restErr != nil (per-session error from restoreSession)
	var restored, skipped, failed int

	for _, s := range statuses {
		// tmux.nix puts the session name into shell commands. A row whose
		// name is outside the safe set predates the check, so report it and
		// keep it as it is. The user decides with `prism cleanup` on the host.
		if err := sessionname.ValidateName(s.SessionName); err != nil {
			fmt.Fprintf(os.Stderr, "restore %q: not restored: %v\n", s.SessionName, err)
			failed++
			continue
		}
		if dryRun {
			// In dry-run mode, show what would happen but never sleep or write.
			fmt.Printf("would restore: %s (worktree=%s)\n", s.SessionName, s.Worktree)
			restored++
			continue
		}

		outcome, restErr := restoreSession(d, s, &pendingStagger, staggerDelay)
		if restErr != nil {
			fmt.Fprintf(os.Stderr, "restore %q: %v\n", s.SessionName, restErr)
			failed++
		} else {
			switch outcome {
			case restoreOutcomeCreated:
				restored++
			case restoreOutcomeSkipped:
				skipped++
			}
		}

		// A created session sets pendingStagger so that the NEXT actual create
		// is preceded by a sleep. Skipped sessions do not consume or reset
		// pendingStagger.
		if outcome == restoreOutcomeCreated {
			pendingStagger = true
		}
	}

	// Ensure the persistent dashboard session exists after restoring project
	// sessions. This is skipped in dry-run mode since no sessions are created.
	if !dryRun {
		if err := ensureDashSession(); err != nil {
			// Non-fatal: log and continue. The user can open the dashboard
			// manually via prefix+D or prism dashboard.
			fmt.Fprintf(os.Stderr, "prism restore: ensure dashboard session: %v\n", err)
		}
	}

	// Final aggregate summary line. Emitted on stdout after
	// all per-session lines so callers can grep for the totals reliably. Also
	// emitted in dry-run mode so dry-run output is non-empty when there are
	// no sessions.
	fmt.Printf("restore complete: %d restored, %d skipped, %d failed\n", restored, skipped, failed)

	return nil
}

// restoreSession recreates a single session. Already-existing sessions are
// skipped silently. Sessions with missing/inaccessible worktrees are marked as
// ended in the DB rather than left as zombies.
//
// pendingStagger points to a bool that is set by the caller after each
// successful create. If *pendingStagger is true when session.Create is about
// to be called, restoreProjectSession sleeps for staggerDelay and resets it
// to false first. This ensures the delay only fires between real creates, not
// before already-running or worktree-missing sessions.
//
// s.SessionName is the authoritative tmux session name — it is never
// re-derived from the worktree path. This ensures both bare and non-bare
// sessions (e.g. obsidian) are restored correctly even if the session name
// would not match what sessionNameFor() would compute from the worktree.
func restoreSession(d *db.DB, s db.Status, pendingStagger *bool, staggerDelay time.Duration) (restoreOutcome, error) {
	if session.HasExactSession(s.SessionName) {
		return restoreOutcomeSkipped, nil
	}

	// Defensive guard — meta-sessions (scratchpad, prism-dashboard) are
	// explicitly skipped by IsMetaSession in cmd/event.go:tmux-session-start
	// and will never appear in agent_status under normal operation. This
	// block is retained as a safety net. The scratchpad has special restore
	// behaviour (re-create via ensureAndSwitch); all other meta-sessions are
	// simply skipped.
	if session.IsMetaSession(s.SessionName) {
		if s.SessionName == session.ScratchpadSession {
			return restoreOutcomeCreated, ensureAndSwitch("[scratchpad]", "", session.Opts{Headless: true})
		}
		return restoreOutcomeSkipped, nil
	}

	return restoreProjectSession(d, s, pendingStagger, staggerDelay)
}

// restoreProjectSession recreates a project session using the shared
// session.Create(LayoutFull) code path, ensuring the same three-window layout
// (edit / agent / term) that normal session creation produces. The session
// name comes from the DB row (s.SessionName) and is never re-derived from
// the filesystem — this ensures non-bare sessions (e.g. obsidian) and sessions
// whose name diverges from the worktree path are restored correctly.
//
// pendingStagger and staggerDelay implement the inter-create stagger. If
// *pendingStagger is true when session.Create is about to be called, this
// function sleeps for staggerDelay and resets *pendingStagger to false first.
//
// agent_status seeding is handled directly via the open DB handle
// (SkipStatusSeed=true) rather than forking a subprocess, because
// os.Executable() does not reliably resolve the real prism binary in all
// contexts (e.g. test binaries).
func restoreProjectSession(d *db.DB, s db.Status, pendingStagger *bool, staggerDelay time.Duration) (restoreOutcome, error) {
	// If the worktree directory doesn't exist or is inaccessible, mark the
	// session as ended in the DB so it doesn't appear as a zombie in the
	// dashboard.
	directory := s.Worktree
	if directory == "" {
		fmt.Fprintf(os.Stderr, "restore %q: no worktree recorded — marking ended\n", s.SessionName)
		return restoreOutcomeSkipped, setEndedStoppingReviewChildren(d, s.SessionName)
	}
	if _, err := os.Stat(directory); err != nil {
		fmt.Fprintf(os.Stderr, "restore %q: worktree %q not accessible (%v) — marking ended\n",
			s.SessionName, directory, err)
		return restoreOutcomeSkipped, setEndedStoppingReviewChildren(d, s.SessionName)
	}

	// Build opts for the full three-window layout. SkipStatusSeed prevents
	// setupFullLayout from forking "prism event tmux-session-start" — we
	// manage agent_status directly below via the open DB handle.
	//
	// IsolationMode is read directly from the DB row (s.IsolationMode).
	// All rows have isolation_mode set (guaranteed by v22→v23 backfill).
	cfg := loadRestoreConfig()

	var isoMode config.IsolationMode
	if s.IsolationMode != "" {
		isoMode = config.IsolationMode(s.IsolationMode)
	} else {
		// Defensive fallback for rows that somehow still have an empty
		// isolation_mode (should not occur post-v25 migration). Resolve using
		// global config defaults.
		var resolveErr error
		isoMode, resolveErr = container.Resolve(container.ResolveInput{
			ConfigDefault: cfg.DefaultIsolationMode,
		})
		if resolveErr != nil {
			isoMode = config.IsolationHost
		}
	}

	// Apply per-path isolation override: if the stored worktree path matches
	// a configured override, use the override mode regardless of what was
	// recorded in the DB row. This ensures sessions for paths like
	// ~/documents/obsidian are always restored with the correct isolation mode
	// (e.g. "host") even if they were originally recorded with a different mode.
	if override := cfg.IsolationOverrideForPath(directory); override != "" {
		proglog.Infof("[prism restore] using isolation override %q for path %q\n", override, directory)
		isoMode = override
	}

	// Look up the isolation capabilities for this mode. All per-mode branching
	// below reads from isoCaps rather than comparing against raw mode constants.
	isoCaps := container.CapabilitiesFor(isoMode)

	// Resolve the harness name from the DB row. Fall back to "pi" for
	// pre-registry rows where the harness column is NULL.
	restoreHarnessName := "pi"
	if s.Harness != nil && *s.Harness != "" {
		restoreHarnessName = *s.Harness
	}
	// If the persisted harness is not registered (e.g. a future harness that
	// was later uninstalled), fall back to the pi adapter so that
	// restore does not hard-fail on every row.
	restoreHarness, restoreHarnessErr := harness.New(restoreHarnessName, "", nil, "", "")
	if restoreHarnessErr != nil {
		restoreHarness, _ = harness.New("pi", "", nil, "", "")
	}
	opts := session.Opts{
		Headless:       true,
		Agent:          session.DefaultAgentForSession(s.SessionName, directory, "", d),
		SessionName:    s.SessionName,
		Layout:         session.LayoutFull,
		SkipStatusSeed: true,
		IsolationMode:  string(isoMode),
		RuntimeEnvVars: restoreHarness.RuntimeEnv(),
		HarnessName:    restoreHarnessName,
		// PIExtensionDir for host-mode pi launches.
		PIExtensionDir: cfg.PIExtensionDir,
	}
	// Host mode has no agent-run process to re-read the profile, so carry the
	// spawn-time profile into the launch command.
	if s.InstanceID == nil {
		// No instance row: fall through to the active profile.
	} else if si, siErr := d.SpawnInputsByInstanceID(*s.InstanceID); siErr == nil && si != nil && si.ProfileName != nil {
		opts.ProfileName = *si.ProfileName
	}
	// Propagate the persisted harness session ID so the sandbox launcher can
	// ask pi to resume the prior conversation. Empty pointer
	// or empty string both mean "start fresh" — PIInvocation treats an empty
	// HarnessSessionID as a silent no-op.
	if s.HarnessSessionID != nil && *s.HarnessSessionID != "" {
		opts.HarnessSessionID = restoreResumeSessionID(s.SessionName, directory, *s.HarnessSessionID)
	}
	if isoCaps.IsContainer {
		opts.PluginHostPath = cfg.SidecarPluginPath
	}
	// For socket-pipe harnesses (e.g. "pi") in host isolation mode,
	// pre-compute the Unix socket path so agentPaneEnvVars can inject
	// PRISM_HARNESS_PIPE into the tmux pane. bwrap and sandbox-exec set
	// PRISM_HARNESS_PIPE via their own paths; only inject here for host mode.
	// Mirrors the same block in spawn.go and switch.go.
	if hShape, hShapeOK := harness.ShapeOf(restoreHarnessName); hShapeOK && hShape == harness.TransportSocketPipe && string(isoMode) == "host" {
		if pipePath, pipeErr := session.SidecarHarnessPipePath(s.SessionName); pipeErr == nil {
			opts.HarnessPipeSockPath = pipePath
		} else {
			proglog.Warnf("[prism restore] warning: could not resolve harness pipe path for %q: %v\n", s.SessionName, pipeErr)
		}
	}

	// Re-check for a race immediately before DB writes: if the session has
	// appeared since restoreSession's outer HasSession check, skip
	// RefreshWorktree and AllocatePort to avoid corrupting the live session's
	// agent_status row. This is as late as we can usefully check — the
	// remaining window (between here and tmux new-session inside Create) is
	// unavoidable and is handled by Create's own inner guard.
	if session.HasExactSession(s.SessionName) {
		return restoreOutcomeSkipped, nil
	}

	// Stop any sidecar left over from a previous lifecycle (e.g. `prism
	// restart` kills tmux but not the sidecars). StopSidecarReport is a no-op
	// when no PID file is present or the process is already gone, so it is
	// safe to call unconditionally for both host-mode and container-mode
	// sessions. When it proves the old sidecar gone, it removes the PID file,
	// so StartSidecarWithOpts can write a fresh one below. restoredInstanceID
	// needs to know whether the old sidecar stopped.
	//
	// The Shutdown of a coordinator sidecar abandons the merge queue, so read
	// the queue first (#3166).
	queued, queueErr := d.WatchingMerges(s.SessionName)
	if queueErr != nil {
		fmt.Fprintf(os.Stderr, "restore %q: read merge queue: %v\n", s.SessionName, queueErr)
	}
	oldSidecarPID, oldSidecarStopped, oldSidecarGone := restoreStopSidecar(s.SessionName, restoreSidecarStopTimeout)
	// A stop before restore, as at a host shutdown, abandoned its rows
	// before the read above (#3175). Find them before restoredInstanceID
	// reopens the incarnation.
	if s.InstanceID != nil {
		var err error
		if queued, err = withMergesAbandonedByStop(d, s.SessionName, *s.InstanceID, queued); err != nil {
			fmt.Fprintf(os.Stderr, "restore %q: read merge queue of the last stop: %v\n", s.SessionName, err)
		}
	}
	// A process that can be the old sidecar can still watch the merge queue.
	// A new sidecar next to it starts a second watcher, and the duplicate-start
	// check of the new sidecar refuses only when a socket of the old sidecar
	// answers. So do not restore the session, and change no row of it
	// (#3303).
	if !oldSidecarGone {
		return restoreOutcomeSkipped, oldSidecarRunsError(s.SessionName, oldSidecarPID, queued)
	}

	// For container-mode sessions (none in current code — isoCaps.IsContainer
	// is always false), also remove any stale container left over
	// from the previous lifecycle.
	// removeContainerIfExists is idempotent and logs non-fatal errors
	// internally, so it is safe to call even when no container exists.
	// Host-mode sessions never have a container, so this step is skipped.
	if isoCaps.IsContainer {
		removeContainerIfExists(s.SessionName)
	}

	// Refresh agent_status and allocate a port before calling session.Create,
	// so that opts.Port is set when BuildOpencodeCmd fires inside
	// setupFullLayout.
	//
	// RefreshWorktree is used instead of UpsertStatus because UpsertStatus only
	// writes repo/worktree on the initial INSERT (ON CONFLICT does not update
	// them). If the row was previously corrupted by the session-created hook
	// race, UpsertStatus would silently leave the stale path.
	// RefreshWorktree corrects repo and worktree unconditionally.
	if s.Repo != "" {
		if err := d.RefreshWorktree(s.SessionName, s.Repo, directory); err != nil {
			// Non-fatal: a stale DB row is preferable to aborting the restore.
			fmt.Fprintf(os.Stderr, "restore %q: refresh agent_status: %v\n", s.SessionName, err)
		} else {
			e := db.Event{
				ID:          uuid.New().String(),
				SessionName: s.SessionName,
				Repo:        s.Repo,
				Worktree:    directory,
				Type:        "tmux_session_start",
				Payload:     `{}`,
				CreatedAt:   time.Now(),
			}
			if err := d.WriteEvent(e); err != nil {
				// Non-fatal.
				fmt.Fprintf(os.Stderr, "restore %q: write event: %v\n", s.SessionName, err)
			}
		}
	}

	// Allocate a port for every session, including root sessions with an
	// empty repo (e.g. "obsidian"): setupFullLayout skips the sidecar when
	// opts.Port is 0. AllocatePort only needs the agent_status row to exist;
	// a missing row or allocation error is logged and the restore continues.
	port, err := d.AllocatePort(s.SessionName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore %q: port allocation: %v\n", s.SessionName, err)
	} else {
		opts.Port = port
	}

	// Apply stagger delay: if a previous session was just created, sleep
	// before starting this one. *pendingStagger is set by the Restore() loop
	// after each successful create; we consume it here (reset to false) so
	// that the delay fires exactly once per create pair.
	// This is the point in the code path where we are committed to calling
	// session.Create, so all cheap checks (worktree, HasSession) have already
	// been handled above without consuming the stagger.
	if pendingStagger != nil && *pendingStagger && staggerDelay > 0 {
		time.Sleep(staggerDelay)
		*pendingStagger = false
	}

	if instanceID := restoredInstanceID(d, s, opts.Agent, oldSidecarStopped); instanceID != "" {
		opts.InstanceID = instanceID
	}
	resumeMergeQueue(d, "restore", s.SessionName, opts.InstanceID, queued)

	if err := session.Create(s.SessionName, directory, opts); err != nil {
		return restoreOutcomeSkipped, fmt.Errorf("create session: %w", err)
	}
	// session.Create contains its own inner HasSession guard as a final safety
	// net for the narrow race window between the check above and new-session.

	fmt.Printf("session %q restored\n", s.SessionName)
	return restoreOutcomeCreated, nil
}

// restoreResumeSessionID returns harnessSessionID when pi can resume it in
// worktree, and "" when it cannot.
//
// pi runs a resumed session in the cwd from the header of its session
// file, and a sandboxed session can write that header. If the header cwd
// is not worktree, restore prints the session name and the reason, and the
// session starts a fresh conversation in worktree (#3284). If the sessions
// dir of the worktree does not exist, restore prints nothing and returns
// the id: the launcher logs the missing file to the agent-run log, as
// before.
//
// For bwrap and sandbox-exec, agent-run reads the session id from the DB
// row, not from session.Opts. container.ResolvePIResumeSession runs the
// same check there, so those launchers refuse the same files.
func restoreResumeSessionID(sessionName, worktree, harnessSessionID string) string {
	cfg := container.Config{SessionName: sessionName, Worktree: worktree, HarnessSessionID: harnessSessionID}
	if err := container.CheckPIResumeCwd(cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return harnessSessionID
		}
		fmt.Fprintf(os.Stderr, "restore %q: not resuming pi session %s: %v\n", sessionName, harnessSessionID, err)
		return ""
	}
	return harnessSessionID
}

// restoreSidecarStopTimeout bounds the wait for the old sidecar of a
// restored session to stop after SIGTERM. Tests shorten it.
var restoreSidecarStopTimeout = killSidecarTimeout

// restoreStopSidecar stops the old sidecar of a restored session. Tests
// replace it.
var restoreStopSidecar = session.StopSidecarReport

// oldSidecarRunsError is the error of the restore of sessionName when the
// stop cannot prove that the old sidecar is gone. pid is the PID in its PID
// file. queued is the merge queue that a restore resumes.
func oldSidecarRunsError(sessionName string, pid int, queued []db.PendingMerge) error {
	var b strings.Builder
	fmt.Fprintf(&b, "the session is not restored, because process %d can be the old sidecar and still runs (see the warning above). "+
		"A new sidecar next to that process can start a second merge-queue watcher.", pid)
	if len(queued) > 0 {
		fmt.Fprintf(&b, " The merge queue is not resumed: %s.", formatMergePRs(queued))
	}
	fmt.Fprintf(&b, " Stop process %d, then run prism restore again.", pid)
	if pidPath, err := session.SidecarPIDPath(sessionName); err == nil {
		fmt.Fprintf(&b, " If the process is not a prism sidecar, remove its PID file first: %s", pidPath)
	}
	return errors.New(b.String())
}

// restoredInstanceID returns the instance ID that a session which restore
// recreates must run with. It returns "" when it cannot record one.
//
// A restore continues the incarnation that agent_status names. The instance
// ID keys the spawn_inputs profile pin, spawn_outcome, and the podman
// resource names, so all of them must survive a
// restart. The old sidecar stamps ended_at on its instance when it stops, so
// restore clears ended_at and end_state again. Otherwise the restored sidecar
// writes every event with the instance ID of an ended session (#3027).
//
// If the old sidecar did not stop inside the SIGTERM wait, the stop sent it
// SIGKILL, and restore starts a new incarnation instead. Restore calls
// restoredInstanceID only after the stop proved that the old sidecar is
// gone (#3303).
func restoredInstanceID(d *db.DB, s db.Status, agentRole string, oldSidecarStopped bool) string {
	if s.InstanceID == nil || *s.InstanceID == "" {
		return startRestoredIncarnation(d, s, agentRole)
	}
	instanceID := *s.InstanceID
	if !oldSidecarStopped {
		fmt.Fprintf(os.Stderr, "warning: restore %q: the old sidecar did not stop within %v; starting a new incarnation instead of reopening instance %s\n",
			s.SessionName, restoreSidecarStopTimeout, instanceID)
		return startRestoredIncarnation(d, s, agentRole)
	}
	prev, err := d.SessionByInstanceID(instanceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore %q: read incarnation %s: %v; starting a new incarnation\n", s.SessionName, instanceID, err)
		return startRestoredIncarnation(d, s, agentRole)
	}
	if prev == nil {
		// agent_status names an instance with no sessions row. Insert the
		// full row, so the events of the restored sidecar have a live parent.
		if err := d.InsertSession(restoredSessionRow(s, instanceID, agentRole, nil)); err != nil {
			fmt.Fprintf(os.Stderr, "restore %q: insert session: %v\n", s.SessionName, err)
		}
		return instanceID
	}
	if prev.EndedAt != nil {
		if err := d.ReopenSession(instanceID); err != nil {
			fmt.Fprintf(os.Stderr, "restore %q: reopen incarnation %s: %v; starting a new incarnation\n", s.SessionName, instanceID, err)
			return startRestoredIncarnation(d, s, agentRole)
		}
	}
	return instanceID
}

// startRestoredIncarnation starts a new incarnation for a session that
// restore recreates, and returns its instance ID. It returns "" when it
// cannot record the new instance ID in agent_status.
func startRestoredIncarnation(d *db.DB, s db.Status, agentRole string) string {
	instanceID := uuid.New().String()
	if err := d.SetInstanceID(s.SessionName, instanceID); err != nil {
		fmt.Fprintf(os.Stderr, "restore %q: set instance_id: %v\n", s.SessionName, err)
		return ""
	}

	var prev *db.Session
	if s.InstanceID != nil && *s.InstanceID != "" {
		p, err := d.SessionByInstanceID(*s.InstanceID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "restore %q: read previous incarnation: %v\n", s.SessionName, err)
		}
		prev = p
		// No other path ends a sessions row that agent_status no longer
		// names. If restore does not end it here, it stays open forever.
		if prev != nil && prev.EndedAt == nil {
			if err := d.UpdateSessionEnded(*s.InstanceID, "finished"); err != nil {
				fmt.Fprintf(os.Stderr, "restore %q: end previous incarnation: %v\n", s.SessionName, err)
			}
		}
	}

	// The sidecar inserts a row with fewer columns when none exists, so
	// insert the full row before the sidecar starts.
	if err := d.InsertSession(restoredSessionRow(s, instanceID, agentRole, prev)); err != nil {
		fmt.Fprintf(os.Stderr, "restore %q: insert session: %v\n", s.SessionName, err)
	}
	return instanceID
}

// restoredSessionRow builds the sessions row for instanceID from the
// agent_status row s. It copies parent_session, and agent_role when
// agentRole is empty, from prev when prev is non-nil.
func restoredSessionRow(s db.Status, instanceID, agentRole string, prev *db.Session) db.Session {
	row := db.Session{
		InstanceID:       instanceID,
		SessionName:      s.SessionName,
		RootAgentName:    s.RootAgentName,
		Repo:             s.Repo,
		Worktree:         s.Worktree,
		HarnessSessionID: s.HarnessSessionID,
		GroupID:          s.GroupID,
	}
	if s.Harness != nil {
		row.Harness = *s.Harness
	}
	if agentRole != "" {
		row.AgentRole = &agentRole
	}
	if prev != nil {
		row.ParentSession = prev.ParentSession
		if row.AgentRole == nil {
			row.AgentRole = prev.AgentRole
		}
	}
	return row
}
