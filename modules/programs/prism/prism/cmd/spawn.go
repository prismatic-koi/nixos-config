package cmd

// prism spawn — create a new timestamped worktree from the current repo
// and switch to it immediately. Bound to prefix+a.
//
// Infers the bare repo root from the current tmux pane path.
// Branch name format: 20260327T1423 (zettelkasten-style timestamp).

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

var spawnCmd = &cobra.Command{
	Use:   "spawn",
	Short: "Create a new timestamped worktree from the current repo and switch to it",
	RunE: func(cmd *cobra.Command, args []string) error {
		// PRISM_SPAWN_PATH is set by the tmux binding via display-popup -e so
		// that the caller's pane path is available inside the popup (inside a
		// display-popup, tmux display-message returns the popup's own path).
		panePathRaw := os.Getenv("PRISM_SPAWN_PATH")
		if panePathRaw == "" {
			// Fallback: query tmux directly (works when not in a popup).
			var err error
			panePathRaw, err = tmux.CurrentPanePath()
			if err != nil || panePathRaw == "" {
				return fmt.Errorf("could not determine current pane path")
			}
		}

		// Walk up from the pane path to find a bare repo root.
		bareRoot := git.BareRoot(panePathRaw)
		if bareRoot == "" {
			// Also try the path itself (if we're already at the project root).
			if git.IsBareRepo(panePathRaw) {
				bareRoot = panePathRaw
			}
		}
		if bareRoot == "" {
			if git.IsInsideRegularRepo(panePathRaw) {
				return fmt.Errorf("this repo isn't using the worktree layout yet\nuse C-f to convert it first")
			}
			return fmt.Errorf("not inside a git repo")
		}

		// Zettelkasten timestamp: 20260327T1423
		branch := time.Now().Format("20060102T1504")

		// Create the worktree.
		worktreePath, err := git.CreateWorktree(bareRoot, branch)
		if err != nil {
			return fmt.Errorf("create worktree: %w", err)
		}

		// Create session and switch to it.
		return ensureAndSwitchSession(worktreePath, bareRoot)
	},
}

func init() {
	rootCmd.AddCommand(spawnCmd)
}
