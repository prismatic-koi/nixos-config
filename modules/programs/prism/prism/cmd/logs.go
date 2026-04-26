package cmd

// prism logs <session> — stream the sidecar log file for a named session.
//
// Usage:
//
//	prism logs <session>                    print the full sidecar log to stdout
//	prism logs <session> --tail N           print only the last N lines
//	prism logs <session> --follow           stream new lines as they arrive
//	prism logs <session> -f                 alias for --follow
//	prism logs <session> --agent-run        print the agent-run log (bwrap stdout/stderr)
//	prism logs <session> --agent-run --tail N   last N lines of agent-run log
//	prism logs <session> --agent-run -f     follow agent-run log
//
// The output is the raw log file and can be piped to grep:
//
//	prism logs nixos-config@main | grep '[timing]'
//	prism logs nixos-config@feat --agent-run | grep 'error'
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

Use --agent-run to read the bwrap harness log instead (captures bwrap and
opencode stdout/stderr for the lifetime of the session):

  prism logs nixos-config@feat --agent-run
  prism logs nixos-config@feat --agent-run --follow

Use --startup to read the spawn-time breadcrumb log (covers the window
between "session created in DB" and "opencode reachable", written by
session.SpawnSession before tmux send-keys):

  prism logs nixos-config@feat --startup

Works identically from the host or inside a coordinator container — in
container mode the log is fetched via the host API Unix socket (PRISM_HOST_API).`,
	Args:         cobra.ExactArgs(1),
	RunE:         runLogs,
	SilenceUsage: true,
}

func init() {
	logsCmd.Flags().Int("tail", 0, "Number of lines to show from the end of the log; 0 prints nothing (omit flag to show all)")
	logsCmd.Flags().BoolP("follow", "f", false, "Stream new lines as they are written; exits when session ends or Ctrl-C")
	logsCmd.Flags().Bool("agent-run", false, "Read the agent-run log (bwrap harness stdout/stderr) instead of the sidecar log")
	logsCmd.Flags().Bool("startup", false, "Read the agent-startup log (spawn-time breadcrumbs written by session.SpawnSession)")
	rootCmd.AddCommand(logsCmd)
}

func runLogs(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	tailN, _ := cmd.Flags().GetInt("tail")
	follow, _ := cmd.Flags().GetBool("follow")
	tailSet := cmd.Flags().Changed("tail")
	agentRun, _ := cmd.Flags().GetBool("agent-run")
	startup, _ := cmd.Flags().GetBool("startup")

	if tailSet && follow {
		return fmt.Errorf("--tail and --follow are mutually exclusive")
	}

	if agentRun && startup {
		return fmt.Errorf("--agent-run and --startup are mutually exclusive")
	}

	if tailSet && tailN < 0 {
		return fmt.Errorf("--tail must be a non-negative integer")
	}

	// Container proxy path: delegate to the host-API sidecar.
	// (--startup is host-only because the agent-startup log is written by
	// the host-side SpawnSession; container coordinators have no use for it.)
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" && !startup {
		return proxyLogsFromHostAPI(apiURL, sessionName, tailN, tailSet, follow, agentRun, os.Stdout)
	}

	// Host path: resolve the appropriate log file.
	var logPath string
	var err error
	switch {
	case startup:
		logPath, err = session.AgentStartupLogPath(sessionName)
		if err != nil {
			return fmt.Errorf("resolve agent-startup log path: %w", err)
		}
	case agentRun:
		logPath, err = session.AgentRunLogPath(sessionName)
		if err != nil {
			return fmt.Errorf("resolve agent-run log path: %w", err)
		}
	default:
		logPath, err = session.SidecarLogPath(sessionName)
		if err != nil {
			return fmt.Errorf("resolve log path: %w", err)
		}
	}

	if _, statErr := os.Stat(logPath); os.IsNotExist(statErr) {
		switch {
		case startup:
			return fmt.Errorf("no agent-startup log file found for session %q", sessionName)
		case agentRun:
			return fmt.Errorf("no agent-run log file found for session %q", sessionName)
		default:
			// For the default sidecar-log path, if a startup log exists, hint
			// at it — the operator was probably looking at the wrong file
			// because the agent silently failed to come up before the sidecar
			// produced any meaningful events (the #1051 failure mode).
			if session.AgentStartupLogExists(sessionName) {
				return fmt.Errorf("no sidecar log file found for session %q\nhint: an agent-startup log exists for this session — try `prism logs %s --startup` to see spawn-time breadcrumbs",
					sessionName, sessionName)
			}
			return fmt.Errorf("no log file found for session %q", sessionName)
		}
	}

	if follow {
		return runLogsFollow(sessionName, logPath)
	}

	if tailSet {
		return runLogsTail(logPath, tailN)
	}

	if err := runLogsFull(logPath); err != nil {
		return err
	}

	// AC-4: when the operator is reading the default (sidecar) log and that
	// log contains nothing beyond SSE-retry noise — the #1051 failure
	// signature — surface a one-line hint pointing at the agent-startup log
	// where spawn-time breadcrumbs live. The hint is silenced when the
	// sidecar log shows real activity (server.connected, first event, etc.)
	// or when no startup log exists for this session.
	if !startup && !agentRun && sidecarLogIsStuckOnSSERetries(logPath) && session.AgentStartupLogExists(sessionName) {
		fmt.Fprintf(os.Stderr,
			"\nhint: this sidecar log contains only SSE-retry noise — opencode never bound its port.\n"+
				"      Spawn-time breadcrumbs (port, isolation mode, sidecar PID) are in the agent-startup log:\n"+
				"        prism logs %s --startup\n"+
				"      bwrap stderr (if it ran) is in the agent-run log:\n"+
				"        prism logs %s --agent-run\n",
			sessionName, sessionName)
	}
	return nil
}

// sidecarLogIsStuckOnSSERetries returns true when the named sidecar log file
// contains nothing beyond the [prism sidecar] starting line and a series of
// "sse: …" retry messages — i.e. the sidecar started, opencode never bound
// its port, and we have no further evidence of progress. Used by runLogs to
// surface a startup-log hint to operators reading the wrong file.
//
// The implementation is deliberately conservative: it returns false on any
// I/O error, on any line that does not begin with "[prism sidecar]" or "sse:"
// (or is blank / "clipboard …"), and on logs above 64 KiB (large logs almost
// always contain real events; the failure mode this targets produces tiny
// logs of a dozen lines or so). False negatives are fine — the hint is
// optional. False positives would print a noisy hint on healthy sessions.
func sidecarLogIsStuckOnSSERetries(logPath string) bool {
	const maxBytes = 64 * 1024
	info, statErr := os.Stat(logPath)
	if statErr != nil || info.Size() == 0 || info.Size() > maxBytes {
		return false
	}
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		return false
	}
	hasRealEvent := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Accept the lines we expect on the failure path:
		//   "[prism sidecar] starting: …"
		//   "clipboard clean: …"
		//   "<timestamp> sse: …"
		if strings.HasPrefix(trimmed, "[prism sidecar]") {
			continue
		}
		if strings.HasPrefix(trimmed, "clipboard ") {
			continue
		}
		// "2026/04/26 10:22:52 sse: …" — match the "sse:" token after
		// timestamp.
		if strings.Contains(trimmed, " sse: ") || strings.HasPrefix(trimmed, "sse: ") {
			continue
		}
		// Any other line indicates the sidecar produced real output — bail.
		hasRealEvent = true
		break
	}
	return !hasRealEvent
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
