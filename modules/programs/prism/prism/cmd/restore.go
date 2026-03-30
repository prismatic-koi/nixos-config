package cmd

// prism restore — recreate tmux sessions from the snapshot written by prism save.
//
// Intended to be called once when a new tmux server starts (via the
// server-started hook in tmux.conf).  Sessions that already exist are skipped
// silently so it is safe to call more than once.
//
// For each saved session:
//   - scratchpad        → plain session in $HOME, term window
//   - prism-dashboard   → skipped (self-creates on first C-w / prefix+D)
//   - project@worktree  → full three-window session (edit / agent / term)
//     with opencode launched in the agent window, headless (no client switch)
//   - any other session → recreated as a bare session in Dir (best-effort)

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/tmux"
)

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Recreate tmux sessions from the last saved snapshot",
	RunE:  runRestore,
}

func init() {
	restoreCmd.Flags().Bool("dry-run", false, "Print what would be restored without creating sessions")
	rootCmd.AddCommand(restoreCmd)
}

func runRestore(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	saved, err := loadSessions()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No snapshot yet — nothing to restore.
			return nil
		}
		return fmt.Errorf("load sessions: %w", err)
	}

	if len(saved) == 0 {
		return nil
	}

	for _, s := range saved {
		if dryRun {
			fmt.Printf("would restore: %s (dir=%s bare=%s)\n", s.Name, s.Dir, s.BareRoot)
			continue
		}
		if err := restoreSession(s); err != nil {
			// Log but continue — a single failure should not abort the rest.
			fmt.Fprintf(os.Stderr, "restore %q: %v\n", s.Name, err)
		}
	}
	return nil
}

// loadSessions reads the snapshot file and returns the saved session list.
func loadSessions() ([]SavedSession, error) {
	data, err := os.ReadFile(saveStatePath())
	if err != nil {
		return nil, err
	}
	var sessions []SavedSession
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, fmt.Errorf("parse snapshot: %w", err)
	}
	return sessions, nil
}

// restoreSession recreates a single session.  Already-existing sessions are
// skipped silently.
func restoreSession(s SavedSession) error {
	if tmux.HasSession(s.Name) {
		return nil
	}

	switch s.Name {
	case "prism-dashboard":
		// Let the tmux binding create it on demand; skip here.
		return nil

	case "scratchpad":
		return ensureAndSwitchSession("[scratchpad]", "", sessionOpts{headless: true})

	default:
		if s.Dir == "" {
			// No directory info — create a minimal bare session.
			return tmux.NewSessionDetached(s.Name, "")
		}
		// Recreate the full worktree session (edit/agent/term windows).
		// headless=true so no client is switched.
		return ensureAndSwitchSession(s.Dir, s.BareRoot, sessionOpts{headless: true})
	}
}
