package cmd

// prism restore — recreate tmux sessions from prism.db.
//
// Reads agent_status rows where ended_at IS NULL and recreates any sessions
// that are no longer present in the running tmux server. Sessions that already
// exist are skipped silently — safe to call more than once.
//
// Stagger: a configurable delay (default 500ms) is inserted between successive
// session creates to flatten the podman startup burst on machines with many
// sessions. The delay only applies to sessions that are actually being created;
// it is skipped for already-running sessions and for sessions being marked
// ended due to missing worktrees. It is also fully skipped in dry-run mode.
//
// Circuit breaker: before restoring a session, restore checks its recent
// sidecar exit history. If the last N sidecar lifecycles all ended
// non-successfully (state != "finished") with no intervening success, restore
// skips re-spawning that session and prints a clear message pointing the user
// at `prism restart` or `prism cleanup`. The session's agent_status row is
// NOT marked ended — it stays active in the dashboard so the user can
// intervene. N defaults to 3 and is tunable via
// cfg.SidecarCircuitBreakerThreshold.

import (
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/dashboard"
	"github.com/prismatic-koi/prism/internal/db"
	opencode "github.com/prismatic-koi/prism/internal/harness/opencode"
	"github.com/prismatic-koi/prism/internal/session"
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

	// restoreOutcomeCircuitOpen means the circuit breaker tripped: the session
	// has too many consecutive sidecar failures. No session was created and the
	// agent_status row is left active. A stagger delay is NOT applied (we did
	// no real work).
	restoreOutcomeCircuitOpen
)

// loadRestoreConfig returns the active prism configuration for restore.
// It is a package-level variable so tests can override it to exercise
// container-mode and host-mode restore paths without depending on the
// process-wide config.Load() singleton cache.
var loadRestoreConfig = func() config.Config { return config.Load() }

// loadRestoreProfiles loads profiles.json for the restore code path.
// It is a package-level variable so tests can inject a fake profiles file
// without depending on XDG_CONFIG_HOME or the filesystem.
var loadRestoreProfiles = func() (*config.ProfilesFile, error) { return config.LoadProfiles() }

// onRestoreSessionCreate is called just before session.Create in
// restoreProjectSession. In production it is nil (no-op). Tests may set it to
// capture the session.Opts and assert that ConfigContent is populated correctly.
// The hook is invoked with a snapshot of opts before Create is called.
var onRestoreSessionCreate func(opts session.Opts)

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Recreate tmux sessions from prism.db",
	RunE:  runRestore,
}

func init() {
	restoreCmd.Flags().Bool("dry-run", false, "Print what would be restored without creating sessions")
	rootCmd.AddCommand(restoreCmd)
}

func runRestore(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	return Restore(dryRun)
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

	// Remove any stale dashboard socket left behind by a crashed dashboard.
	// Non-fatal: a stale socket only affects real-time push delivery.
	dashboard.RemoveStaleSocket()

	statuses, err := d.AllActiveStatus()
	if err != nil {
		return fmt.Errorf("prism restore: query sessions: %w", err)
	}

	// Load config once to read the stagger delay and circuit-breaker threshold.
	// loadRestoreConfig is a package-level var so tests can override it.
	cfg := loadRestoreConfig()
	staggerDelay := cfg.RestoreStaggerDelay()
	threshold := cfg.CircuitBreakerThreshold()

	// pendingStagger is set to true after each successful session.Create. It is
	// consumed (a sleep is applied) immediately before the NEXT actual create,
	// not before skipped sessions or circuit-breaker-tripped sessions. This
	// ensures the delay only fires between real podman/sidecar starts.
	//
	// Using a pointer so that restoreProjectSession can consume it without an
	// extra return value: the function sleeps before session.Create if
	// *pendingStagger is true, then resets it to false.
	pendingStagger := false

	for _, s := range statuses {
		if dryRun {
			// In dry-run mode, show what would happen but never sleep or write.
			// Check the circuit breaker state so the user sees accurate output.
			if threshold > 0 {
				failures, cbErr := d.ConsecutiveSidecarFailures(s.SessionName, threshold)
				if cbErr != nil {
					fmt.Fprintf(os.Stderr, "restore dry-run %q: circuit breaker query: %v — treating as 0 failures\n", s.SessionName, cbErr)
					failures = 0
				}
				if failures >= threshold {
					fmt.Printf("would skip (circuit breaker): %s — %d consecutive sidecar failure(s); run `prism restart %s` or `prism cleanup` to unblock\n",
						s.SessionName, failures, s.SessionName)
					continue
				}
			}
			fmt.Printf("would restore: %s (worktree=%s)\n", s.SessionName, s.Worktree)
			continue
		}

		outcome, restErr := restoreSession(d, s, threshold, &pendingStagger, staggerDelay)
		if restErr != nil {
			fmt.Fprintf(os.Stderr, "restore %q: %v\n", s.SessionName, restErr)
		}

		// A created session sets pendingStagger so that the NEXT actual create
		// is preceded by a sleep. Skipped sessions and circuit-open sessions do
		// not consume or reset pendingStagger.
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

	return nil
}

// restoreSession recreates a single session. Already-existing sessions are
// skipped silently. Sessions with missing/inaccessible worktrees are marked as
// ended in the DB rather than left as zombies.
//
// threshold is the circuit-breaker consecutive-failure threshold. A value of 0
// disables the circuit breaker for this call (all sessions are restored).
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
func restoreSession(d *db.DB, s db.Status, threshold int, pendingStagger *bool, staggerDelay time.Duration) (restoreOutcome, error) {
	if tmux.HasSession(s.SessionName) {
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

	return restoreProjectSession(d, s, threshold, pendingStagger, staggerDelay)
}

// restoreProjectSession recreates a project session using the shared
// session.Create(LayoutFull) code path, ensuring the same three-window layout
// (edit / agent / term) that normal session creation produces. The session
// name comes from the DB row (s.SessionName) and is never re-derived from
// the filesystem — this ensures non-bare sessions (e.g. obsidian) and sessions
// whose name diverges from the worktree path are restored correctly.
//
// threshold is the circuit-breaker consecutive-failure threshold. A value of 0
// disables the circuit breaker for this call.
//
// pendingStagger and staggerDelay implement the inter-create stagger. If
// *pendingStagger is true when session.Create is about to be called, this
// function sleeps for staggerDelay and resets *pendingStagger to false first.
//
// agent_status seeding is handled directly via the open DB handle
// (SkipStatusSeed=true) rather than forking a subprocess, because
// os.Executable() does not reliably resolve the real prism binary in all
// contexts (e.g. test binaries).
func restoreProjectSession(d *db.DB, s db.Status, threshold int, pendingStagger *bool, staggerDelay time.Duration) (restoreOutcome, error) {
	// If the worktree directory doesn't exist or is inaccessible, mark the
	// session as ended in the DB so it doesn't appear as a zombie in the
	// dashboard.
	directory := s.Worktree
	if directory == "" {
		fmt.Fprintf(os.Stderr, "restore %q: no worktree recorded — marking ended\n", s.SessionName)
		return restoreOutcomeSkipped, d.SetEnded(s.SessionName)
	}
	if _, err := os.Stat(directory); err != nil {
		fmt.Fprintf(os.Stderr, "restore %q: worktree %q not accessible (%v) — marking ended\n",
			s.SessionName, directory, err)
		return restoreOutcomeSkipped, d.SetEnded(s.SessionName)
	}

	// Circuit breaker: skip sessions whose sidecar has failed too many times
	// in a row without a successful run in between. The agent_status row is
	// intentionally left active (no SetEnded call) so the session remains
	// visible in `prism list-sessions` and the dashboard, and the user can
	// intervene via `prism restart` or `prism cleanup`.
	if threshold > 0 {
		failures, cbErr := d.ConsecutiveSidecarFailures(s.SessionName, threshold)
		if cbErr != nil {
			// Non-fatal: a broken history table must not block recovery.
			// Log and fall through to attempt the restore normally.
			fmt.Fprintf(os.Stderr, "restore %q: circuit breaker query failed: %v — proceeding with restore\n",
				s.SessionName, cbErr)
		} else if failures >= threshold {
			fmt.Printf("skipped (circuit breaker): %s — %d consecutive sidecar failure(s); run `prism restart %s` to try again, or `prism cleanup` to remove it\n",
				s.SessionName, failures, s.SessionName)
			return restoreOutcomeCircuitOpen, nil
		}
	}

	// Build opts for the full three-window layout. SkipStatusSeed prevents
	// setupFullLayout from forking "prism event tmux-session-start" — we
	// manage agent_status directly below via the open DB handle.
	//
	// IsolationMode is read from the DB row (s.IsolationMode) when recorded.
	// When absent (pre-v10 rows), fall back to the back-compat derivation:
	// HostMode=true → "host", else use the global cfg's effective mode.
	// This ensures sessions spawned before the isolation_mode column was added
	// are restored with the same behaviour they were created with.
	cfg := loadRestoreConfig()

	var isoMode config.IsolationMode
	if s.IsolationMode != "" {
		isoMode = config.IsolationMode(s.IsolationMode)
	} else if s.HostMode {
		isoMode = config.IsolationHost
	} else {
		// Pre-v10 row without isolation_mode and without host_mode:
		// fall back to the global config's effective isolation mode.
		// This restores pre-v10 sessions with the same mode the machine
		// is currently configured for.
		isoMode = cfg.EffectiveIsolationMode()
	}

	containerMode := isoMode == config.IsolationPodman
	opencodeSession := ""
	if s.HarnessSessionID != nil {
		opencodeSession = *s.HarnessSessionID
	}
	restoreHarness := opencode.New("", nil, "", "")
	opts := session.Opts{
		Headless:         true,
		OpencodeSession:  opencodeSession,
		Agent:            session.DefaultAgent(directory, ""),
		SessionName:      s.SessionName,
		Layout:           session.LayoutFull,
		SkipStatusSeed:   true,
		ContainerMode:    containerMode,
		IsolationMode:    string(isoMode),
		ConfigEnvVarName: restoreHarness.ConfigEnvVar(),
		RuntimeEnvVars:   restoreHarness.RuntimeEnv(),
	}
	if containerMode {
		opts.PluginHostPath = cfg.SidecarPluginPath
	}

	// In sandboxed mode (podman or bwrap), inject the role-specific config
	// blob as the harness config env var. This mirrors the pattern in spawn.go.
	// Profile load errors are non-fatal for restore: log and skip config
	// injection so the session is still recreated without it, rather than
	// aborting the entire restore run.
	//
	// Host-mode sessions skip this entirely because they run opencode directly
	// with the host's real ~/.config/opencode/opencode.json via xdg.configFile.
	sandboxed := isoMode == config.IsolationPodman || isoMode == config.IsolationBwrap
	if sandboxed {
		pf, pfErr := loadRestoreProfiles()
		if pfErr != nil {
			fmt.Fprintf(os.Stderr, "restore %q: load profiles: %v — skipping config injection\n", s.SessionName, pfErr)
		} else {
			effectiveRole := session.DefaultAgent(directory, "")
			roleConfig, roleErr := config.ContainerConfigForRole(pf, effectiveRole)
			if roleErr != nil {
				fmt.Fprintf(os.Stderr, "restore %q: container config for role %q: %v — skipping config injection\n", s.SessionName, effectiveRole, roleErr)
			} else if roleConfig != "" {
				opts.ConfigContent = roleConfig
			} else if effectiveRole == "worker" || effectiveRole == "coordinator" {
				fmt.Fprintf(os.Stderr, "[prism restore] warning: no container role config for %q in profiles.json — rebuild the system config to generate it\n", effectiveRole)
			}
		}

		// For bwrap sessions, write the opencode.json config file to disk now
		// so it is present before the agent pane opens. prism agent-run
		// reconstructs a container.Manager from DB state (which does not carry
		// ConfigContent), so the file must be written here at restore time via
		// the deterministic temp path. The bwrap.go mount-emission block checks
		// file existence (os.Stat) rather than cfg.ConfigContent, so it picks
		// this up correctly.
		//
		// Podman mode does NOT need this write — the sidecar's Create() path
		// already writes the file before the container starts.
		//
		// IMPORTANT: the path key used here must match the one used by Manager
		// internally. Manager.name = container.NameForSession(s.SessionName),
		// and Manager.opencodeConfigFilePath() calls OpencodeConfigFilePath(m.name).
		// So we must pass the container name (not the raw tmux session name) to
		// WriteOpencodeConfig. This mirrors the pattern in spawn.go.
		if isoMode == config.IsolationBwrap && opts.ConfigContent != "" {
			containerName := container.NameForSession(s.SessionName)
			if err := container.WriteOpencodeConfig(containerName, opts.ConfigContent); err != nil {
				// Non-fatal: log and continue with restore. The session will
				// still be re-spawned; it just won't have the opencode.json
				// mounted. This matches the general "restore is best-effort"
				// posture (profile load errors are also non-fatal above).
				fmt.Fprintf(os.Stderr, "restore %q: write opencode config: %v — session will spawn without opencode.json mounted\n", s.SessionName, err)
			}
		}
	}

	// Re-check for a race immediately before DB writes: if the session has
	// appeared since restoreSession's outer HasSession check, skip
	// RefreshWorktree and AllocatePort to avoid corrupting the live session's
	// agent_status row. This is as late as we can usefully check — the
	// remaining window (between here and tmux new-session inside Create) is
	// unavoidable and is handled by Create's own inner guard.
	if tmux.HasSession(s.SessionName) {
		return restoreOutcomeSkipped, nil
	}

	// Kill any orphaned sidecar left over from a previous lifecycle (e.g. a
	// reboot or crash without clean shutdown). KillSidecar is a no-op when
	// no PID file is present or the process is already gone, so it is safe
	// to call unconditionally for both host-mode and container-mode sessions.
	// Clearing the PID file here ensures StartSidecarWithOpts can write a
	// fresh one below.
	session.KillSidecar(s.SessionName)

	// For container-mode sessions, also remove any stale container left over
	// from the previous lifecycle. Without this, `podman create` inside the
	// new sidecar would fail with "container name already in use".
	// removeContainerIfExists is idempotent and logs non-fatal errors
	// internally, so it is safe to call even when no container exists.
	// Host-mode sessions never have a container, so this step is skipped.
	if containerMode {
		removeContainerIfExists(s.SessionName)
	}

	// Refresh agent_status and allocate a port before calling session.Create,
	// so that opts.Port is set when BuildOpencodeCmd fires inside
	// setupFullLayout.
	//
	// RefreshWorktree is used instead of UpsertStatus because UpsertStatus only
	// writes repo/worktree on the initial INSERT (ON CONFLICT does not update
	// them). If the row was previously corrupted by the session-created hook
	// race (issue #380), UpsertStatus would silently leave the stale path.
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

		// Allocate a port unconditionally (regardless of whether
		// RefreshWorktree succeeded). AllocatePort only needs the
		// agent_status row to exist, which it does since we are restoring
		// a previously-known session.
		port, err := d.AllocatePort(s.SessionName)
		if err != nil {
			// Non-fatal: log and continue without a port.
			fmt.Fprintf(os.Stderr, "restore %q: port allocation: %v\n", s.SessionName, err)
		} else {
			opts.Port = port
		}
	}
	// When s.Repo == "", RefreshWorktree and AllocatePort are skipped. The
	// session is still created with the full layout; it just won't have an
	// opencode serve port allocated.

	// Invoke the test hook (nil in production) with a snapshot of opts
	// so tests can assert ConfigContent and other fields without intercepting
	// session.Create.
	if onRestoreSessionCreate != nil {
		onRestoreSessionCreate(opts)
	}

	// Apply stagger delay: if a previous session was just created, sleep
	// before starting this one. *pendingStagger is set by the Restore() loop
	// after each successful create; we consume it here (reset to false) so
	// that the delay fires exactly once per create pair.
	// This is the point in the code path where we are committed to calling
	// session.Create, so all cheap checks (worktree, HasSession, circuit
	// breaker) have already been handled above without consuming the stagger.
	if pendingStagger != nil && *pendingStagger && staggerDelay > 0 {
		time.Sleep(staggerDelay)
		*pendingStagger = false
	}

	if err := session.Create(s.SessionName, directory, opts); err != nil {
		return restoreOutcomeSkipped, fmt.Errorf("create session: %w", err)
	}
	// session.Create contains its own inner HasSession guard as a final safety
	// net for the narrow race window between the check above and new-session.

	fmt.Printf("session %q restored\n", s.SessionName)
	return restoreOutcomeCreated, nil
}
