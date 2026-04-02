package cmd

// prism restore — recreate tmux sessions from prism.db.
//
// Reads agent_status rows where ended_at IS NULL and recreates any sessions
// that are no longer present in the running tmux server. Sessions that already
// exist are skipped silently — safe to call more than once.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
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
		return ensureAndSwitchSession("[scratchpad]", "", sessionOpts{headless: true})

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

	// Window 0: edit — open nvim, mirroring ensureAndSwitchSession behaviour.
	_ = tmux.RenameWindow(s.SessionName+":0", "edit")

	nvimCmd := "nvim"
	if des, err := os.ReadDir(directory); err == nil {
		var files []string
		for _, de := range des {
			if !de.IsDir() {
				files = append(files, filepath.Join(directory, de.Name()))
			}
		}
		switch {
		case len(files) == 1:
			nvimCmd = "nvim '" + files[0] + "'"
		case strings.Contains(directory, "obsidian"):
			nvimCmd = "nvim +'Obsidian today'"
		default:
			readme := filepath.Join(directory, "README.md")
			if _, err := os.Stat(readme); err == nil {
				nvimCmd = "nvim '" + readme + "'"
			}
		}
	}
	_ = tmux.SendKeys(s.SessionName+":0", nvimCmd)

	// Window 1: agent — launch opencode resuming the stored session ID.
	_ = tmux.NewWindow(s.SessionName, 1, "agent", directory)
	opencodeSession := ""
	if s.OpencodeSID != nil {
		opencodeSession = *s.OpencodeSID
	}
	opts := sessionOpts{
		headless:        true,
		opencodeSession: opencodeSession,
		agent:           defaultAgent(directory, ""),
	}
	_ = tmux.SendKeys(s.SessionName+":1", buildOpencodeCmd(opts))

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
