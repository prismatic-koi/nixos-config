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

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

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
		// Defensive guard — prism-dashboard is a non-project session and is
		// never written to agent_status by prism event tmux-session-start (which
		// exits silently when no .bare marker is found). This case cannot fire in
		// practice under normal operation.
		return nil

	case "scratchpad":
		// Defensive guard — same reasoning as prism-dashboard above: scratchpad
		// has no .bare ancestor and will never appear in agent_status.
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

	// Guard against a race: the session may have been created externally between
	// the HasSession check in restoreSession and here. If it now exists, skip
	// all DB writes (RefreshWorktree, AllocatePort) to avoid corrupting the
	// live session's agent_status row.
	if tmux.HasSession(s.SessionName) {
		return nil
	}

	// Build opts for the full three-window layout. SkipStatusSeed prevents
	// setupFullLayout from forking "prism event tmux-session-start" — we
	// manage agent_status directly below via the open DB handle.
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

			// Allocate a port now that the row is fresh. AllocatePort writes
			// to the agent_status row, so it must run after RefreshWorktree.
			port, err := d.AllocatePort(s.SessionName)
			if err != nil {
				// Non-fatal: log and continue without a port.
				fmt.Fprintf(os.Stderr, "restore %q: port allocation: %v\n", s.SessionName, err)
			} else {
				opts.Port = port
			}
		}
	}
	// When s.Repo == "", RefreshWorktree and AllocatePort are skipped. The
	// session is still created with the full layout; it just won't have an
	// opencode serve port allocated.

	if err := session.Create(s.SessionName, directory, opts); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	// session.Create is a no-op if the session already exists (the race guard
	// above makes this unlikely, but the inner guard in Create is a safety net).

	fmt.Printf("session %q restored\n", s.SessionName)
	return nil
}
