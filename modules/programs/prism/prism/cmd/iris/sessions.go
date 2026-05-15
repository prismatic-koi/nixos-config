package main

// sessions.go — `iris sessions` subcommand group.
//
// Canonical commands:
//
//	iris sessions list           active session table (human-readable)
//	iris sessions list --json    JSON array of session-status objects
//	iris sessions status         single-line state counts
//	iris sessions status --json  JSON object keyed by state with integer counts
//
// Both commands dial the iris daemon client socket (~/.local/state/iris/iris.sock
// by default; override with --socket) and read the existing sessions_snapshot
// frame (D-6). Neither command reads iris.db directly — the daemon is the
// single source of truth for live session state.
//
// Out of scope for this command group (see #1669):
//   - --all (no cross-repo model in iris)
//   - --tmux-format on status (#1672)
//   - sessions watch (use the TUI)
//   - backward-compat aliases (iris is new; no legacy surface)
//   - state/role/since filter flags

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

// sessionsCmd is the parent of `iris sessions list` and `iris sessions status`.
var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Query and inspect iris daemon-tracked sessions",
	Long: `Subcommand group for querying the iris daemon's session list.

Both 'list' and 'status' dial the iris daemon client socket and read the
existing sessions_snapshot frame — they do not read the iris DB directly.

The daemon must be running ('iris daemon') for these commands to work; if it
is not, they exit non-zero with a clear "is the iris daemon running?" error.`,
}

// sessionsListCmd implements `iris sessions list`.
var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List daemon-tracked sessions",
	Long: `Print a table of sessions the iris daemon is currently tracking.

Columns: SESSION (12-char UUID prefix), STATE, ROLE, WORKTREE (basename), UPTIME.
The full UUID and full worktree path are available via --json.

When no sessions are present, only the header row is printed (exit 0). This
keeps the output trivially grep-able.`,
	RunE: runSessionsList,
	// Errors here are operational (daemon down, lost connection) — not
	// usage errors — so suppress cobra's usage dump on failure. main.go
	// already prints the error message to stderr; SilenceErrors stops cobra
	// from also printing it (avoiding duplicate output).
	SilenceUsage:  true,
	SilenceErrors: true,
}

// sessionsStatusCmd implements `iris sessions status`.
var sessionsStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print iris session counts by state",
	Long: `Print a one-line summary of daemon-tracked sessions grouped by state.

Without flags: a single human-readable line, e.g.
    active: 2  waiting: 0  finished: 1  error: 0

With --json: a JSON object keyed by state with integer values, e.g.
    {"active":2,"waiting":0,"idle":0,"finished":1,"error":0}`,
	RunE:          runSessionsStatus,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	sessionsListCmd.Flags().Bool("json", false, "Emit a JSON array of session objects instead of the human-readable table")
	sessionsListCmd.Flags().String("socket", "", "Path to the iris daemon client socket (default: ~/.local/state/iris/iris.sock)")

	sessionsStatusCmd.Flags().Bool("json", false, "Emit a JSON object keyed by state with integer counts")
	sessionsStatusCmd.Flags().String("socket", "", "Path to the iris daemon client socket (default: ~/.local/state/iris/iris.sock)")

	sessionsCmd.AddCommand(sessionsListCmd)
	sessionsCmd.AddCommand(sessionsStatusCmd)
	rootCmd.AddCommand(sessionsCmd)
}

// daemonDialTimeout caps the time we spend trying to connect to the daemon
// socket. The daemon is local so a healthy dial completes in microseconds; a
// multi-second hang almost certainly means the daemon is not running and the
// socket file is a stale leftover, or the daemon is wedged. Either way the
// CLI should fail fast with a clear error rather than block indefinitely.
const daemonDialTimeout = 2 * time.Second

// daemonReadTimeout caps the time we wait for the sessions_snapshot response.
// The daemon answers from in-memory state, so a healthy round trip is sub-ms.
// A multi-second wait indicates the daemon is wedged or the connection has
// dropped silently — fail fast with a clear error.
const daemonReadTimeout = 5 * time.Second

// resolveSocketPath returns --socket if set, otherwise the canonical iris
// daemon socket path.
func resolveSocketPath(cmd *cobra.Command) string {
	if sock, _ := cmd.Flags().GetString("socket"); sock != "" {
		return sock
	}
	return iris.ResolvePaths().Sock
}

// fetchSessionsSnapshot dials the iris daemon, sends a sessions_list frame,
// and returns the decoded sessions_snapshot.
//
// Error categories (all wrapped with a fixed prefix for predictable grepping):
//
//   - "iris daemon not running" — socket file missing or connection refused.
//     The user should run `systemctl --user start iris` (or the equivalent
//     on Darwin).
//   - "iris daemon: lost connection to daemon" — the dial succeeded but the
//     read returned EOF / unexpected EOF / connection reset before a complete
//     frame arrived.
//   - "iris daemon: malformed response" — a line arrived but did not parse as
//     a sessions_snapshot frame. Defensive: should not happen against a
//     healthy daemon.
//   - "iris daemon: unexpected error response: <msg>" — the daemon sent an
//     error frame in response to sessions_list.
func fetchSessionsSnapshot(ctx context.Context, sockPath string) (*iris.DaemonSessionsSnapshotFrame, error) {
	// Phase 1: dial. We distinguish "socket not present" from "daemon
	// refused us" so the operator sees the most useful error.
	if _, err := os.Stat(sockPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("iris daemon not running: socket %s does not exist; start it with `systemctl --user start iris` (or `launchctl kickstart -k gui/$UID/iris` on Darwin)", sockPath)
		}
		// Other stat errors (permission etc.) are unusual but should
		// surface verbatim.
		return nil, fmt.Errorf("iris daemon: stat socket %s: %w", sockPath, err)
	}

	dialer := net.Dialer{Timeout: daemonDialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", sockPath)
	if err != nil {
		// At this point the socket file exists but the dial failed. Common
		// causes: ECONNREFUSED (stale socket; daemon crashed without
		// unlinking), ENOTSOCK (regular file at that path — someone created
		// it manually), or permission denied. All map to the same operator
		// remedy: start the daemon.
		return nil, fmt.Errorf("iris daemon not running: cannot connect to %s (%v); start it with `systemctl --user start iris` (or `launchctl kickstart -k gui/$UID/iris` on Darwin)", sockPath, err)
	}
	defer conn.Close()

	// Phase 2: send sessions_list.
	req := iris.ClientSessionsListFrame{Type: iris.ClientFrameSessionsList}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		// Marshalling a fixed struct cannot fail in practice; keep a
		// defensive error path so misuse doesn't panic.
		return nil, fmt.Errorf("iris daemon: marshal request: %w", err)
	}
	reqBytes = append(reqBytes, '\n')

	_ = conn.SetWriteDeadline(time.Now().Add(daemonReadTimeout))
	if _, err := conn.Write(reqBytes); err != nil {
		return nil, fmt.Errorf("iris daemon: lost connection to daemon during request: %w", err)
	}

	// Phase 3: read frames until we get a sessions_snapshot. The daemon
	// may send unrelated frames (none in normal flow, but be defensive).
	_ = conn.SetReadDeadline(time.Now().Add(daemonReadTimeout))
	r := bufio.NewReaderSize(conn, 4*1024*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("iris daemon: lost connection to daemon before response: %w", err)
			}
			// Includes net.Error timeouts.
			return nil, fmt.Errorf("iris daemon: lost connection to daemon: %w", err)
		}
		// Decode the type discriminator first so we can route a stray
		// error frame distinctly from a malformed payload.
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return nil, fmt.Errorf("iris daemon: malformed response from daemon: %w", err)
		}
		switch head.Type {
		case iris.DaemonFrameSessionsSnapshot:
			var snap iris.DaemonSessionsSnapshotFrame
			if err := json.Unmarshal(line, &snap); err != nil {
				return nil, fmt.Errorf("iris daemon: malformed response from daemon: %w", err)
			}
			return &snap, nil
		case iris.DaemonFrameError:
			var ef iris.DaemonErrorFrame
			if err := json.Unmarshal(line, &ef); err != nil {
				return nil, fmt.Errorf("iris daemon: malformed response from daemon: %w", err)
			}
			return nil, fmt.Errorf("iris daemon: unexpected error response: %s", ef.Message)
		default:
			// Unknown frame: ignore and keep reading. The protocol comment
			// in client_protocol.go states both sides must tolerate unknown
			// types. Reset the read deadline so a flood of unknowns can't
			// be used to slow us down past the original budget — we just
			// extend per-frame.
			continue
		}
	}
}
