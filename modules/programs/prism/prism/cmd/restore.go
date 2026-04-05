package cmd

// prism restore — recreate tmux sessions from prism.db.
//
// Reads agent_status rows where ended_at IS NULL and recreates any sessions
// that are no longer present in the running tmux server. Sessions that already
// exist are skipped silently — safe to call more than once.

import (
	"fmt"
	"os"
	"strings"
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

// restoreProjectSession builds the standard three-window layout for a project
// session directly, without routing through ensureAndSwitchSession. This
// avoids the name re-derivation that caused non-bare sessions (e.g. obsidian)
// to silently create the wrong session or skip creation entirely.
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

	// Create the session with window 0 rooted at the worktree directory.
	if err := tmux.NewSessionDetached(s.SessionName, directory); err != nil {
		return fmt.Errorf("new-session: %w", err)
	}

	// Window 0: edit — open nvim, mirroring the full layout behaviour.
	_ = tmux.RenameWindow(s.SessionName+":0", "edit")
	_ = tmux.SendKeys(s.SessionName+":0", session.NvimCmd(directory))

	// Refresh agent_status with the verified worktree path, idle state, and a
	// current last_seen so that post-restore operations find a fresh row.
	//
	// RefreshWorktree is used instead of UpsertStatus because UpsertStatus only
	// writes repo/worktree on the initial INSERT (ON CONFLICT does not update
	// them). If the row was previously corrupted by the session-created hook
	// race (issue #380), UpsertStatus would silently leave the stale path.
	// RefreshWorktree corrects repo and worktree unconditionally.
	//
	// We write directly via the open DB handle rather than exec-ing a
	// subprocess to avoid depending on os.Executable() returning the real
	// prism binary (which it does not in test binaries).
	if s.Repo != "" {
		if err := d.RefreshWorktree(s.SessionName, s.Repo, directory); err != nil {
			// Non-fatal: the session is created; a stale DB row is preferable
			// to aborting the rest of the restore.
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

	// Window 1: agent — launch opencode resuming the stored session ID.
	_ = tmux.NewWindow(s.SessionName, 1, "agent", directory)
	opencodeSession := ""
	if s.OpencodeSID != nil {
		opencodeSession = *s.OpencodeSID
	}
	opts := session.Opts{
		Headless:        true,
		OpencodeSession: opencodeSession,
		Agent:           session.DefaultAgent(directory, ""),
		SessionName:     s.SessionName,
	}

	// Allocate a port for the restored session. The agent_status row was
	// refreshed above, so AllocatePort can write to it.
	port, err := d.AllocatePort(s.SessionName)
	if err != nil {
		// Non-fatal: log and continue without a port.
		fmt.Fprintf(os.Stderr, "restore %q: port allocation: %v\n", s.SessionName, err)
	} else {
		opts.Port = port
	}

	_ = tmux.SendKeys(s.SessionName+":1", session.BuildOpencodeCmd(opts))

	// Window 2: term.
	_ = tmux.NewWindow(s.SessionName, 2, "term", directory)

	// Focus: obsidian → edit (0), else → agent (1).
	focusIdx := 1
	if strings.Contains(directory, "obsidian") {
		focusIdx = 0
	}
	_ = tmux.SelectWindow(s.SessionName, focusIdx)

	fmt.Printf("session %q restored\n", s.SessionName)
	return nil
}
