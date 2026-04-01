package cmd

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// waitForTmuxServerDead polls until tmux list-sessions fails (server gone) or
// the deadline is exceeded. Returns true if the server died within the timeout.
func waitForTmuxServerDead(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := tmux.Run("list-sessions")
		if err != nil {
			// Server is gone.
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Save sessions, kill tmux, and re-launch",
	RunE:  runRestart,
}

func init() {
	rootCmd.AddCommand(restartCmd)
}

func runRestart(_ *cobra.Command, _ []string) error {
	// 1. Save state
	if err := runSave(nil, nil); err != nil {
		return fmt.Errorf("failed to save state: %w", err)
	}

	// 2. Kill tmux server
	_, _ = tmux.Run("kill-server")

	// Wait for the server to actually die before restoring. kill-server is
	// asynchronous, so we poll until list-sessions fails rather than sleeping
	// a fixed amount.
	if !waitForTmuxServerDead(3 * time.Second) {
		return fmt.Errorf("tmux server did not stop within timeout")
	}

	// 3. Restore sessions
	if err := Restore(false); err != nil {
		return fmt.Errorf("failed to restore sessions: %w", err)
	}

	// 4. Re-exec
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}

	// Re-exec with "launch"
	return syscall.Exec(executable, []string{executable, "launch"}, os.Environ())
}
