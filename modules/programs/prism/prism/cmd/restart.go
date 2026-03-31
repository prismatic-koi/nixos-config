package cmd

import (
	"fmt"
	"os"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/tmux"
)

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
		fmt.Fprintf(os.Stderr, "prism restart: failed to save state: %v\n", err)
	}

	// 2. Kill tmux server
	_, _ = tmux.Run("kill-server")

	// 3. Re-exec
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}

	// Re-exec with "launch"
	return syscall.Exec(executable, []string{executable, "launch"}, os.Environ())
}
