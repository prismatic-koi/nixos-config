package main

// logs.go — `iris logs <session>` subcommand (issue #1675).
//
// The daemon writes a per-session log file at
// ~/.local/state/iris/logs/<session>.log capturing the supervisor's
// session-scoped log lines (spawn, state transitions, RPC events, restart
// decisions). `iris logs <session>` reads that file and writes to stdout.
//
// Flags:
//
//	--tail N        write only the last N lines (after-the-fact, like `tail -n N`)
//	--follow, -f    stream new lines as they arrive (like `tail -f`)
//
// When --follow is set, iris dials the daemon client socket and subscribes to
// session_state frames for the named session. When the session transitions to
// a terminal state ("finished" or "error"), a 5-second grace timer starts;
// any trailing log lines are emitted, then the command exits 0. The grace
// matches prism's behaviour so trailing supervisor lines (final state write,
// archive notice) reach the user.
//
// Reading the log file is a pure file operation — `iris logs` works even
// when the daemon is dead, provided the file exists on disk. This is
// intentional: diagnostics must work when the daemon is wedged. Terminal-
// state detection requires the daemon (the daemon is the source of truth for
// session lifecycle) — when the daemon is not running and --follow is set,
// the command streams the file and exits when the file stops growing for the
// grace window.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

// logsFollowGraceForTest is the time to keep streaming after the session
// reaches a terminal state (finished or error). Matches prism's
// `prism logs --follow` behaviour. Five seconds is enough to capture
// trailing supervisor lines (final state writes, archive notices, restart-
// decision logs) without making the CLI feel sluggish to exit.
//
// Declared as a var (not const) so the unit tests in logs_test.go can
// shorten it to keep the suite fast. Production code never mutates it.
var logsFollowGraceForTest = 5 * time.Second

// logsIdleGraceForTest is how long --follow waits for new lines when the
// daemon is not reachable and we therefore cannot subscribe to
// session_state. The command exits when no new bytes have arrived for this
// duration. Chosen to be conservative — if a session is genuinely idle for
// 30s, the user can re-invoke; the alternative (waiting forever) would hang
// scripts.
//
// Var rather than const for the same reason as logsFollowGraceForTest.
var logsIdleGraceForTest = 30 * time.Second

// logsPollInterval is the sleep between read attempts on the log file in
// --follow mode. Short enough to feel live, long enough that a slow disk
// doesn't dominate.
const logsPollInterval = 200 * time.Millisecond

// subscribeProbeTimeout is how long subscribeTerminalState waits for a
// first daemon frame before assuming the subscription is live. The daemon
// emits an error frame immediately for unknown sessions — a timeout means
// "subscription accepted, no events yet". 300ms is well above the loopback
// round-trip and well below any user-visible latency.
const subscribeProbeTimeout = 300 * time.Millisecond

var (
	logsTail   int
	logsFollow bool
)

var logsCmd = &cobra.Command{
	Use:   "logs <session>",
	Short: "Print or stream the daemon's per-session log file",
	Long: `Print the iris daemon's per-session log file for <session>.

The daemon writes one log file per session at ~/.local/state/iris/logs/<session>.log
capturing supervisor lifecycle lines (spawn, state transitions, RPC observation,
restart-policy decisions). This command reads that file.

Flags:

  --tail N        Print only the last N lines.
  --follow, -f    Stream new lines as they arrive. Exits ~5 seconds after the
                  session reaches a terminal state ("finished" or "error"),
                  so trailing supervisor lines are captured. When the daemon
                  is not reachable, --follow falls back to a 30-second idle
                  timeout (no new bytes → exit).

When no log file exists for the named session (e.g. the session just started
and the supervisor has not yet written a line), the command exits 0 with no
output rather than erroring — an empty log is not an error.

Diagnostics-first: reading the log is a pure file operation. The command
works even when the iris daemon is dead, provided the file exists on disk.
Terminal-state detection in --follow mode requires the daemon (the daemon is
the source of truth for session lifecycle); without it, --follow uses the
idle-timeout fallback.`,
	Args:          cobra.ExactArgs(1),
	RunE:          runLogs,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	logsCmd.Flags().IntVar(&logsTail, "tail", 0, "Print only the last N lines (0 = print all)")
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Stream new lines as they arrive; exits ~5s after session reaches terminal state")
	rootCmd.AddCommand(logsCmd)
}

// runLogs is the cobra RunE for `iris logs <session>`.
func runLogs(cmd *cobra.Command, args []string) error {
	sessionName := args[0]
	if sessionName == "" {
		return errors.New("iris logs: session name is required")
	}

	p := iris.ResolvePaths()
	logPath := p.SessionLogPath(sessionName)

	// AC: when the daemon is not running but the log file exists, the
	// command still works. We therefore do NOT require the daemon to be
	// reachable for the bare or --tail invocation.
	//
	// AC: when no log file exists, exit 0 with empty output (not an error).
	// We rely on the daemon writing the file as soon as the supervisor logs
	// its first line; until then, "session exists but has no log yet" is
	// indistinguishable from "session does not exist" at the file-system
	// level. The full session-existence check would require a DB or
	// daemon round-trip — out of scope here per the AC.
	//
	// Confirming session existence (and emitting a clear "no such session"
	// message when missing) is best-effort via the daemon when --follow is
	// requested: a follower without a daemon round-trip cannot know when to
	// exit, so we dial the daemon and surface "no such session" if it
	// answers negatively.

	if !logsFollow {
		return runLogsOneShot(cmd.OutOrStdout(), logPath)
	}
	return runLogsFollow(cmd.Context(), cmd.OutOrStdout(), p, sessionName, logPath)
}

// runLogsOneShot reads the entire log file (or the last --tail N lines) and
// writes to w. Missing-file is not an error.
func runLogsOneShot(w io.Writer, logPath string) error {
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// AC: no log file → exit 0 with empty output.
			return nil
		}
		return fmt.Errorf("iris logs: open %q: %w", logPath, err)
	}
	defer f.Close()

	if logsTail > 0 {
		return writeLastNLines(w, f, logsTail)
	}
	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("iris logs: read %q: %w", logPath, err)
	}
	return nil
}

// writeLastNLines reads from r and writes the last n lines to w. It scans
// the full input — the per-session log is bounded by session lifetime, so
// a streaming scan is acceptable. A ring-buffer keeps memory at O(n).
func writeLastNLines(w io.Writer, r io.Reader, n int) error {
	if n <= 0 {
		return nil
	}
	ring := make([]string, 0, n)
	scanner := bufio.NewScanner(r)
	// Allow long lines — supervisor RPC payloads can contain large JSON.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		if len(ring) < n {
			ring = append(ring, scanner.Text())
			continue
		}
		// Shift left by one and append. For modest n this is fine;
		// production tail counts are <= a few thousand.
		copy(ring, ring[1:])
		ring[n-1] = scanner.Text()
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("iris logs: scan: %w", err)
	}
	for _, line := range ring {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// runLogsFollow streams the log file, exiting ~5s after the session reaches a
// terminal state. It:
//
//  1. Opens the log file (tolerating not-yet-present — polls until it shows up).
//  2. Optionally seeks to the end if --tail was not given (matching `tail -f`
//     default; if --tail N was given, prints the last N lines first then
//     streams the tail).
//  3. Dials the daemon client socket and subscribes to session_state for the
//     named session. On terminal-state, starts a 5s grace timer; exits when
//     the timer fires.
//  4. Falls back to an idle timeout if the daemon is not reachable.
func runLogsFollow(ctx context.Context, w io.Writer, p iris.Paths, sessionName, logPath string) error {
	// Subscribe to session_state in a goroutine. The result is delivered via
	// a terminalCh that closes when the daemon reports a terminal state, or
	// is left nil when the daemon is not reachable (idle-timeout fallback).
	terminalCh := make(chan struct{})
	subErr := subscribeTerminalState(ctx, p.Sock, sessionName, terminalCh)
	if subErr != nil {
		// Daemon unreachable or session not found. The latter is an explicit
		// "no such session" surface; the former falls back to the idle
		// timeout. We can distinguish via the error message.
		if isSessionNotFound(subErr) {
			return fmt.Errorf("iris logs: %w", subErr)
		}
		// Daemon unreachable. Set terminalCh to nil so the streamer uses
		// the idle-timeout fallback.
		terminalCh = nil
	}

	// Print the last --tail N lines first if requested (matches prism behaviour).
	startOffset := int64(0)
	if logsTail > 0 {
		f, err := os.Open(logPath)
		if err == nil {
			_ = writeLastNLines(w, f, logsTail)
			off, _ := f.Seek(0, io.SeekEnd)
			startOffset = off
			f.Close()
		} else if errors.Is(err, os.ErrNotExist) {
			// File not present yet — fall through; streamLog will poll
			// for it and start from offset 0.
		} else {
			return fmt.Errorf("iris logs: open %q: %w", logPath, err)
		}
	} else {
		// Default --follow with no --tail: start from end of current file
		// to mimic `tail -f` (don't dump the whole history). Issue #1675
		// is silent on this choice; matching `tail -f` is the least
		// surprising option.
		if st, err := os.Stat(logPath); err == nil {
			startOffset = st.Size()
		}
	}

	return streamLog(ctx, w, logPath, startOffset, terminalCh)
}

// streamLog tails the file at logPath starting from startOffset and writes
// new bytes to w. It returns when either:
//
//   - terminalCh closes, then logsFollowGrace elapses (the daemon-driven exit
//     path), or
//   - no new bytes have arrived for logsIdleGrace (the daemon-unreachable
//     fallback; terminalCh is nil), or
//   - ctx is cancelled.
//
// Re-opens the file on rotation. Bounded poll interval keeps CPU near zero.
func streamLog(ctx context.Context, w io.Writer, logPath string, startOffset int64, terminalCh <-chan struct{}) error {
	var (
		f         *os.File
		offset    = startOffset
		lastBytes = time.Now()
		// graceDeadline is non-zero only after terminalCh has closed.
		graceDeadline time.Time
	)
	closeFile := func() {
		if f != nil {
			f.Close()
			f = nil
		}
	}
	defer closeFile()

	ticker := time.NewTicker(logsPollInterval)
	defer ticker.Stop()

	// Drain initial available bytes immediately (don't wait for the first tick).
	for {
		// Open the file lazily — it may not exist yet (no log lines written).
		if f == nil {
			ff, err := os.Open(logPath)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("iris logs: open %q: %w", logPath, err)
				}
				// File not present — keep waiting.
			} else {
				f = ff
				if _, err := f.Seek(offset, io.SeekStart); err != nil {
					closeFile()
					return fmt.Errorf("iris logs: seek %q: %w", logPath, err)
				}
			}
		}

		// Read whatever is available.
		if f != nil {
			n, err := io.Copy(w, f)
			if err != nil {
				closeFile()
				return fmt.Errorf("iris logs: read %q: %w", logPath, err)
			}
			if n > 0 {
				offset += n
				lastBytes = time.Now()
			}
		}

		// Decide whether to exit.
		if terminalCh != nil {
			select {
			case <-terminalCh:
				if graceDeadline.IsZero() {
					graceDeadline = time.Now().Add(logsFollowGraceForTest)
				}
			default:
			}
			if !graceDeadline.IsZero() && time.Now().After(graceDeadline) {
				return nil
			}
		} else {
			// Idle-timeout fallback when the daemon is unreachable.
			if time.Since(lastBytes) > logsIdleGraceForTest {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// isSessionNotFound reports whether err is the daemon's "session not found"
// error response. The daemon's session_subscribe handler emits an error
// frame with message containing "not found" when the session is unknown.
func isSessionNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "not found")
}

// subscribeTerminalState dials the daemon and subscribes to session_state
// frames for the named session. When a terminal-state frame ("finished" or
// "error") arrives, terminalCh is closed. The goroutine exits when the
// daemon connection drops, ctx is cancelled, or a terminal state is seen.
//
// Returns nil when the subscription is successfully established (the
// goroutine is running). Returns a non-nil error when:
//
//   - the daemon socket is not present or refuses the connection
//     (the caller falls back to the idle-timeout path), or
//   - the daemon answers session_subscribe with an error frame
//     (e.g. "session not found"; the caller surfaces this verbatim).
func subscribeTerminalState(ctx context.Context, sockPath, sessionName string, terminalCh chan<- struct{}) error {
	// Phase 1: dial.
	if _, err := os.Stat(sockPath); err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}
	dialer := net.Dialer{Timeout: daemonDialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return fmt.Errorf("dial daemon: %w", err)
	}

	// Phase 2: send session_subscribe.
	req := iris.ClientSessionSubscribeFrame{
		Type: iris.ClientFrameSessionSubscribe,
		Name: sessionName,
	}
	reqBytes, _ := json.Marshal(req)
	reqBytes = append(reqBytes, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(daemonReadTimeout))
	if _, err := conn.Write(reqBytes); err != nil {
		conn.Close()
		return fmt.Errorf("write session_subscribe: %w", err)
	}
	// Clear the write deadline; reads below have their own per-frame deadline.
	_ = conn.SetWriteDeadline(time.Time{})

	// Phase 3: poll the connection for a short window so we can detect
	// "session not found" synchronously (the daemon emits an error frame
	// immediately for unknown sessions). If the deadline expires without a
	// frame, the subscription is live (the daemon only sends frames when
	// events arrive). We then spawn the long-running goroutine and return
	// nil.
	r := bufio.NewReaderSize(conn, 1*1024*1024)
	_ = conn.SetReadDeadline(time.Now().Add(subscribeProbeTimeout))
	first, readErr := r.ReadBytes('\n')
	_ = conn.SetReadDeadline(time.Time{})

	var (
		sawFirst bool
		firstFrame []byte = first
	)
	if readErr == nil {
		sawFirst = true
	} else {
		// A net.Error timeout means no frame arrived in the probe window —
		// that's the expected "subscription is live" case. Any other error
		// (EOF, connection reset, etc.) is a real failure.
		ne, ok := readErr.(net.Error)
		if !ok || !ne.Timeout() {
			conn.Close()
			return fmt.Errorf("read first frame: %w", readErr)
		}
	}

	if sawFirst {
		var head struct {
			Type        string `json:"type"`
			Message     string `json:"message"`
			RequestType string `json:"request_type"`
		}
		if err := json.Unmarshal(firstFrame, &head); err != nil {
			conn.Close()
			return fmt.Errorf("malformed daemon response: %w", err)
		}
		if head.Type == iris.DaemonFrameError {
			conn.Close()
			// "session not found" or similar — surface verbatim.
			return errors.New(head.Message)
		}
		// A state frame arriving as the first frame is possible if there's
		// a race between subscribe and a transition; handle it now.
		if head.Type == iris.DaemonFrameSessionState {
			var sf iris.DaemonSessionStateFrame
			if err := json.Unmarshal(firstFrame, &sf); err == nil && isTerminalState(sf.State) {
				close(terminalCh)
				conn.Close()
				return nil
			}
		}
		// Otherwise: an event frame arrived. Subscription is live; fall
		// through to the goroutine which will continue draining.
	}

	// Subscription is live. Drain the remaining frames in a goroutine.
	go func() {
		defer conn.Close()
		// Close terminalCh exactly once.
		closed := false
		closeOnce := func() {
			if !closed {
				close(terminalCh)
				closed = true
			}
		}
		// On any unexpected exit (connection drop, EOF), we do NOT close
		// terminalCh — the streamer should keep tailing until ctx is
		// cancelled or the idle timeout fires. This mirrors the prism
		// behaviour where a dead daemon does not immediately terminate
		// the follower.
		defer func() {
			// If ctx was cancelled, propagate by closing terminalCh so the
			// streamer exits promptly. ctx.Err() is non-nil only after Done.
			if ctx.Err() != nil {
				closeOnce()
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var h struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(line, &h); err != nil {
				continue
			}
			if h.Type != iris.DaemonFrameSessionState {
				continue
			}
			var sf iris.DaemonSessionStateFrame
			if err := json.Unmarshal(line, &sf); err != nil {
				continue
			}
			if isTerminalState(sf.State) {
				closeOnce()
				return
			}
		}
	}()

	return nil
}

// isTerminalState returns true for state names that represent a session
// reaching the end of its lifetime ("finished" or "error"). The string set
// mirrors the SessionState constants in internal/iris.
func isTerminalState(state string) bool {
	return state == "finished" || state == "error"
}
