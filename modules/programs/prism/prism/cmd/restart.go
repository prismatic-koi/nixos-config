package cmd

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/tmux"
)

// restartSummaryLine is the single success-side summary line emitted by
// `prism restart` immediately before the re-exec into `launch`. Defined as
// a package-level constant so tests can assert on the exact byte content
// without duplicating the string literal. Issue #1527 AC: the destructive
// command must not be silent on success.
const restartSummaryLine = "prism restart: tmux server killed, sessions restored, re-execing into launch"

// emitRestartSummary writes the success-side restart summary to w and flushes
// it. Extracted from runRestart so unit tests can exercise the print without
// performing the surrounding tmux-server kill / restore / re-exec sequence.
func emitRestartSummary(w io.Writer) {
	fmt.Fprintln(w, restartSummaryLine)
	if f, ok := w.(*os.File); ok {
		_ = f.Sync()
	}
}

// waitForTmuxServerDead polls until tmux info fails (server gone) or
// the deadline is exceeded. Returns true if the server died within the timeout.
func waitForTmuxServerDead(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// "tmux info" fails only when no server is reachable on the socket,
		// unlike "list-sessions" which also fails when the server is running
		// but has no sessions.
		_, err := tmux.Run("info")
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
	Short: "Kill tmux and re-launch",
	RunE:  runRestart,
}

func init() {
	rootCmd.AddCommand(restartCmd)
}

func runRestart(_ *cobra.Command, _ []string) error {
	// 1. Kill tmux server
	_, _ = tmux.Run("kill-server")

	// Wait for the server to actually die before restoring. kill-server is
	// asynchronous, so we poll until tmux info fails rather than sleeping
	// a fixed amount.
	if !waitForTmuxServerDead(3 * time.Second) {
		return fmt.Errorf("tmux server did not stop within timeout")
	}

	// 2. Restore sessions
	// Restore() bootstraps the server itself by creating the scratchpad session
	// as its first action, so no explicit bootstrap is needed here.
	if err := Restore(false); err != nil {
		return fmt.Errorf("failed to restore sessions: %w", err)
	}

	// 3. Re-exec
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}

	// Emit a single summary line on the success path before syscall.Exec so
	// the line survives the re-exec boundary (the new launch process inherits
	// our stdout fd but does not flush our buffered writes for us; fmt.Println
	// to os.Stdout is unbuffered on a tty/pipe, but Sync is called inside
	// emitRestartSummary as belt-and-braces for the unusual case where stdout
	// is a regular file). Per issue #1527: the destructive command must not
	// be silent on success.
	emitRestartSummary(os.Stdout)

	// Re-exec with "launch"
	return syscall.Exec(executable, []string{executable, "launch"}, os.Environ())
}
