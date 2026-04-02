package cmd

// prism restore — recreate tmux sessions from prism.db.
//
// Reads agent_status rows where ended_at IS NULL and recreates any sessions
// that are no longer present in the running tmux server. Sessions that already
// exist are skipped silently — safe to call more than once.
//
// Replaces the old sessions.json file-based approach (retired in Stage 6).

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
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

	if len(statuses) == 0 {
		return nil
	}

	for _, s := range statuses {
		if dryRun {
			fmt.Printf("would restore: %s (worktree=%s)\n", s.SessionName, s.Worktree)
			continue
		}
		if err := restoreSession(s); err != nil {
			fmt.Fprintf(os.Stderr, "restore %q: %v\n", s.SessionName, err)
		}
	}
	return nil
}

// restoreSession recreates a single session. Already-existing sessions are
// skipped silently.
func restoreSession(s db.Status) error {
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
		if s.Worktree == "" {
			// No directory info — create a minimal bare session.
			return tmux.NewSessionDetached(s.SessionName, "")
		}
		opencodeSession := ""
		if s.OpencodeSID != nil {
			opencodeSession = *s.OpencodeSID
		}
		bareRoot := git.BareRoot(s.Worktree)
		return ensureAndSwitchSession(s.Worktree, bareRoot, sessionOpts{
			headless:        true,
			opencodeSession: opencodeSession,
		})
	}
}
