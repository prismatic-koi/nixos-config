package cmd

// prism restore — recreate tmux sessions from prism.db.
//
// Reads agent_status rows where ended_at IS NULL and recreates any sessions
// that are no longer present in the running tmux server. Sessions that already
// exist are skipped silently — safe to call more than once.

import (
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// loadRestoreConfig returns the active prism configuration for restore.
// It is a package-level variable so tests can override it to exercise
// container-mode and host-mode restore paths without depending on the
// process-wide config.Load() singleton cache.
var loadRestoreConfig = func() config.Config { return config.Load() }

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

	statuses, err := d.AllActiveStatus()
	if err != nil {
		return fmt.Errorf("prism restore: query sessions: %w", err)
	}

	for _, s := range statuses {
		if dryRun {
			fmt.Printf("would restore: %s (worktree=%s)\n", s.SessionName, s.Worktree)
			continue
		}
		if err := restoreSession(d, s); err != nil {
			fmt.Fprintf(os.Stderr, "restore %q: %v\n", s.SessionName, err)
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
// s.SessionName is the authoritative tmux session name — it is never
// re-derived from the worktree path. This ensures both bare and non-bare
// sessions (e.g. obsidian) are restored correctly even if the session name
// would not match what sessionNameFor() would compute from the worktree.
func restoreSession(d *db.DB, s db.Status) error {
	if tmux.HasSession(s.SessionName) {
		return nil
	}

	switch s.SessionName {
	case "prism-dashboard":
		// Defensive guard — prism-dashboard is an internal meta-session
		// explicitly skipped by name in cmd/event.go:tmux-session-start and
		// will never appear in agent_status under normal operation. This case
		// is retained as a safety net.
		return nil

	case "scratchpad":
		// Defensive guard — scratchpad is an internal meta-session explicitly
		// skipped by name in cmd/event.go:tmux-session-start and will never
		// appear in agent_status under normal operation. This case is retained
		// as a safety net.
		return ensureAndSwitch("[scratchpad]", "", session.Opts{Headless: true})

	default:
		return restoreProjectSession(d, s)
	}
}

// restoreProjectSession recreates a project session using the shared
// session.Create(LayoutFull) code path, ensuring the same three-window layout
// (edit / agent / term) that normal session creation produces. The session
// name comes from the DB row (s.SessionName) and is never re-derived from
// the filesystem — this ensures non-bare sessions (e.g. obsidian) and sessions
// whose name diverges from the worktree path are restored correctly.
//
// agent_status seeding is handled directly via the open DB handle
// (SkipStatusSeed=true) rather than forking a subprocess, because
// os.Executable() does not reliably resolve the real prism binary in all
// contexts (e.g. test binaries).
func restoreProjectSession(d *db.DB, s db.Status) error {
	// If the worktree directory doesn't exist or is inaccessible, mark the
	// session as ended in the DB so it doesn't appear as a zombie in the
	// dashboard.
	directory := s.Worktree
	if directory == "" {
		fmt.Fprintf(os.Stderr, "restore %q: no worktree recorded — marking ended\n", s.SessionName)
		return d.SetEnded(s.SessionName)
	}
	if _, err := os.Stat(directory); err != nil {
		fmt.Fprintf(os.Stderr, "restore %q: worktree %q not accessible (%v) — marking ended\n",
			s.SessionName, directory, err)
		return d.SetEnded(s.SessionName)
	}

	// Build opts for the full three-window layout. SkipStatusSeed prevents
	// setupFullLayout from forking "prism event tmux-session-start" — we
	// manage agent_status directly below via the open DB handle.
	//
	// ContainerMode is derived from both the global cfg and the per-session
	// s.HostMode flag: a session that was explicitly spawned with --host-mode
	// must be restored in host mode regardless of the current cfg setting.
	// When ContainerMode is enabled, PluginHostPath is also copied from cfg so
	// the sidecar can bind-mount the plugin into the container.
	cfg := loadRestoreConfig()
	containerMode := cfg.ContainerMode && !s.HostMode
	opencodeSession := ""
	if s.OpencodeSID != nil {
		opencodeSession = *s.OpencodeSID
	}
	opts := session.Opts{
		Headless:        true,
		OpencodeSession: opencodeSession,
		Agent:           session.DefaultAgent(directory, ""),
		SessionName:     s.SessionName,
		Layout:          session.LayoutFull,
		SkipStatusSeed:  true,
		ContainerMode:   containerMode,
	}
	if containerMode {
		opts.PluginHostPath = cfg.SidecarPluginPath

		// In container mode, inject the role-specific opencode.json blob as
		// OPENCODE_CONFIG_CONTENT. This mirrors the pattern in spawn.go.
		// Profile load errors are non-fatal for restore: log and skip
		// config injection so the session is still recreated without it,
		// rather than aborting the entire restore run.
		pf, pfErr := config.LoadProfiles()
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
	}

	// Re-check for a race immediately before DB writes: if the session has
	// appeared since restoreSession's outer HasSession check, skip
	// RefreshWorktree and AllocatePort to avoid corrupting the live session's
	// agent_status row. This is as late as we can usefully check — the
	// remaining window (between here and tmux new-session inside Create) is
	// unavoidable and is handled by Create's own inner guard.
	if tmux.HasSession(s.SessionName) {
		return nil
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

	if err := session.Create(s.SessionName, directory, opts); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	// session.Create contains its own inner HasSession guard as a final safety
	// net for the narrow race window between the check above and new-session.

	fmt.Printf("session %q restored\n", s.SessionName)
	return nil
}
