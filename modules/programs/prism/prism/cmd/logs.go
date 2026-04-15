package cmd

// prism logs <session> — stream the sidecar log file for a named session.
//
// Usage:
//
//	prism logs <session>             print the full sidecar log to stdout
//	prism logs <session> --tail N    print only the last N lines
//	prism logs <session> --follow    stream new lines as they arrive
//	prism logs <session> -f          alias for --follow
//
// The output is the raw sidecar log file and can be piped to grep:
//
//	prism logs nixos-config@main | grep '[timing]'
//
// Works identically from the host or inside a coordinator container:
// when PRISM_HOST_API is set, the log is fetched via GET /logs on the
// host-API Unix socket.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/session"
)

var logsCmd = &cobra.Command{
	Use:   "logs <session>",
	Short: "Show the sidecar log for a session",
	Long: `Show the sidecar log for a named prism session.

The output is the raw sidecar log ($XDG_STATE_HOME/prism/logs/<session>-sidecar.log)
and can be piped to grep for filtering:

  prism logs nixos-config@main | grep '[timing]'

Works identically from the host or inside a coordinator container — in
container mode the log is fetched via the host API Unix socket (PRISM_HOST_API).`,
	Args:         cobra.ExactArgs(1),
	RunE:         runLogs,
	SilenceUsage: true,
}

func init() {
	logsCmd.Flags().Int("tail", 0, "Number of lines to show from the end of the log; 0 prints nothing (omit flag to show all)")
	logsCmd.Flags().BoolP("follow", "f", false, "Stream new lines as they are written; exits when session ends or Ctrl-C")
	rootCmd.AddCommand(logsCmd)
}

func runLogs(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	tailN, _ := cmd.Flags().GetInt("tail")
	follow, _ := cmd.Flags().GetBool("follow")
	tailSet := cmd.Flags().Changed("tail")

	if tailSet && follow {
		return fmt.Errorf("--tail and --follow are mutually exclusive")
	}

	if tailSet && tailN < 0 {
		return fmt.Errorf("--tail must be a non-negative integer")
	}

	// Container proxy path: delegate to the host-API sidecar.
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxyLogsFromHostAPI(apiURL, sessionName, tailN, tailSet, follow, os.Stdout)
	}

	// Host path: resolve the sidecar log file.
	logPath, err := session.SidecarLogPath(sessionName)
	if err != nil {
		return fmt.Errorf("resolve log path: %w", err)
	}

	if _, statErr := os.Stat(logPath); os.IsNotExist(statErr) {
		return fmt.Errorf("no log file found for session %q", sessionName)
	}

	if follow {
		return runLogsFollow(sessionName, logPath)
	}

	if tailSet {
		return runLogsTail(logPath, tailN)
	}

	return runLogsFull(logPath)
}

// runLogsFull prints the full contents of the log file to stdout.
func runLogsFull(logPath string) error {
	f, err := os.Open(logPath)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()
	_, err = io.Copy(os.Stdout, f)
	return err
}

// runLogsTail prints the last n lines of the log file to stdout.
// If n == 0, nothing is printed.
func runLogsTail(logPath string, n int) error {
	if n == 0 {
		return nil
	}

	f, err := os.Open(logPath)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	lines, err := tailLinesFromReader(f, n)
	if err != nil {
		return fmt.Errorf("tail log: %w", err)
	}

	bw := bufio.NewWriter(os.Stdout)
	for _, line := range lines {
		fmt.Fprintln(bw, line)
	}
	return bw.Flush()
}

// runLogsFollow streams the log file to stdout, blocking until Ctrl-C or
// the session reaches a terminal state and 5 seconds of silence elapse.
// If the session is already in a terminal state, prints the full log and exits.
func runLogsFollow(sessionName, logPath string) error {
	f, err := os.Open(logPath)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	// Open the DB so we can check the session's state.
	d, dbErr := openDB()
	if dbErr == nil {
		defer d.Close()
		// If the session is already in a terminal state, print the full log and exit.
		if st, stErr := d.CurrentStatus(sessionName); stErr == nil && st != nil &&
			isLogsTerminalState(agent.AgentState(st.State)) {
			_, copyErr := io.Copy(os.Stdout, f)
			return copyErr
		}
	}

	// Stream existing content first.
	if _, err := io.Copy(os.Stdout, f); err != nil {
		return fmt.Errorf("read existing log: %w", err)
	}

	// Set up context for cancellation via Ctrl-C.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var (
		terminalDetected bool
		silenceDeadline  time.Time
	)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Read any new content written since the last tick.
			// io.Copy translates EOF to nil, so any non-nil error is real.
			n, readErr := io.Copy(os.Stdout, f)
			if readErr != nil {
				return fmt.Errorf("stream log: %w", readErr)
			}

			if terminalDetected {
				// After terminal state, wait for 5 seconds of silence.
				if n > 0 {
					// Got more content: reset the silence deadline.
					silenceDeadline = time.Now().Add(5 * time.Second)
				} else if time.Now().After(silenceDeadline) {
					// 5s of silence after terminal state: exit cleanly.
					return nil
				}
			} else if dbErr == nil {
				// Check if the session has reached a terminal state.
				if st, stErr := d.CurrentStatus(sessionName); stErr == nil && st != nil &&
					isLogsTerminalState(agent.AgentState(st.State)) {
					terminalDetected = true
					silenceDeadline = time.Now().Add(5 * time.Second)
				}
			}
		}
	}
}

// tailLinesFromReader reads all content from r and returns the last n lines.
// If the content has fewer than n lines, all lines are returned.
func tailLinesFromReader(r io.Reader, n int) ([]string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return nil, nil
	}

	content := string(data)
	lines := strings.Split(content, "\n")
	// Remove trailing empty entry produced when the file ends with a newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if n >= len(lines) {
		return lines, nil
	}
	return lines[len(lines)-n:], nil
}

// isLogsTerminalState returns true when the agent state is one of the terminal
// states after which no further log output is expected.
func isLogsTerminalState(state agent.AgentState) bool {
	return state == agent.StateFinished ||
		state == agent.StateInterrupted ||
		state == agent.StateDeleted ||
		state == agent.StateError
}
