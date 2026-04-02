package cmd

// prism save — retired in Stage 6.
//
// Pruning is now handled by prism restore (at login) and
// prism event tmux-session-start (on each new tmux session).
// Session state is persisted in prism.db, not sessions.json.

import (
	"github.com/spf13/cobra"
)

var saveCmd = &cobra.Command{
	Use:   "save",
	Short: "Snapshot current tmux sessions to disk for later restore (retired, no-op)",
	RunE:  runSave,
}

func init() {
	rootCmd.AddCommand(saveCmd)
}

func runSave(_ *cobra.Command, _ []string) error {
	// Retired in Stage 6. Pruning is handled by prism restore and prism event tmux-session-start.
	return nil
}
