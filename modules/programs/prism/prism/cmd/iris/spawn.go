package main

// spawn.go — `iris spawn` subcommand.
//
// `iris spawn` dials the running iris daemon's client IPC socket
// (~/.local/state/iris/iris.sock) and asks it to spawn a pi session. The
// session's lifetime is owned by the daemon, not by the shell that ran
// `iris spawn`: this command exits immediately after the daemon
// acknowledges the spawn with a session_spawned frame.
//
// This routing was introduced for issue #1668. Before #1668, `iris spawn`
// ran an in-process supervisor and the resulting session was invisible to
// any other daemon-aware client (TUI, future `iris sessions list`, future
// C-f picker). The in-process path has been removed entirely: the daemon
// is the single source of truth for iris session lifecycle.
//
// # Behaviour
//
//	- If the daemon is not running (or its socket cannot be dialled),
//	  `iris spawn` exits non-zero with a clear error pointing at
//	  `systemctl --user start iris`. It does NOT silently fall back to an
//	  in-process supervisor — that would re-introduce the original bug.
//	- On a successful spawn, stdout includes both the session UUID
//	  (instance_id) and the harness socket path, preserving backward
//	  compatibility with any scripted callers that parsed the previous
//	  in-process command's output.
//	- If the daemon dies after the spawn frame is sent but before the
//	  ack arrives, `iris spawn` exits non-zero with a clear error rather
//	  than hanging.

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

// spawnDialTimeout bounds how long `iris spawn` will wait when dialling the
// daemon socket. The daemon is local so a real connection should complete
// in milliseconds; anything longer is almost certainly "daemon not running".
const spawnDialTimeout = 2 * time.Second

// spawnAckTimeout bounds how long `iris spawn` will wait for a
// session_spawned (or error) frame after sending the spawn request. The
// daemon's spawn work (process fork + harness socket bind) typically takes
// well under a second; a generous timeout still surfaces hangs as a clear
// error per AC ("daemon dies between the spawn frame send and the spawn_ack
// receipt … exits non-zero rather than hanging indefinitely").
const spawnAckTimeout = 30 * time.Second

var (
	spawnWorktree string
	spawnRole     string
)

// spawnCmd is the `iris spawn` subcommand. It dials the daemon client socket
// and asks the daemon to spawn the session.
var spawnCmd = &cobra.Command{
	Use:   "spawn",
	Short: "Spawn a pi session via the running iris daemon",
	Long: `iris spawn dials the iris daemon's client IPC socket and asks it to
spawn a pi session. The daemon owns the session's lifetime: closing the
terminal that ran 'iris spawn' does NOT kill the session.

The daemon must already be running. If it is not, this command exits
non-zero with a clear error — it will not start a local in-process
supervisor as a fallback (see issue #1668 for the history).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSpawn(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(spawnCmd)
	spawnCmd.Flags().StringVar(&spawnWorktree, "worktree", "", "Absolute path to the git worktree (required)")
	spawnCmd.Flags().StringVar(&spawnRole, "role", "worker", "Agent role (worker, coordinator, etc.)")
	_ = spawnCmd.MarkFlagRequired("worktree")
}

// runSpawn implements the spawn flow:
//
//  1. Resolve the daemon socket path.
//  2. Dial it (fail loud if the daemon is not running).
//  3. Send a session_spawn frame.
//  4. Read frames until session_spawned or error or EOF/timeout.
//  5. Print session UUID + harness socket path, then exit 0.
func runSpawn(ctx context.Context) error {
	p := iris.ResolvePaths()
	return runSpawnAt(ctx, p.Sock, p.RunDir, spawnWorktree, spawnRole, os.Stdout)
}

// runSpawnAt is the testable core of runSpawn. sockPath and runDir are passed
// explicitly so the integration test can point them at a t.TempDir(). Stdout
// is captured into out so the test can assert on what the user sees.
func runSpawnAt(ctx context.Context, sockPath, runDir, worktree, role string, out io.Writer) error {
	if worktree == "" {
		return errors.New("iris spawn: --worktree is required")
	}

	fmt.Fprintf(out, "[iris] spawning session via daemon (worktree=%s, role=%s)\n", worktree, role)

	conn, err := dialDaemon(ctx, sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := sendSpawnFrame(conn, worktree, role); err != nil {
		return fmt.Errorf("iris spawn: send spawn frame: %w", err)
	}

	name, instanceID, err := readSpawnAck(ctx, conn)
	if err != nil {
		return err
	}

	harnessSock := iris.HarnessSockPath(runDir, instanceID)
	fmt.Fprintf(out, "[iris] session %s spawned (instance_id=%s, socket=%s)\n",
		name, instanceID, harnessSock)
	return nil
}

// dialDaemon attempts to dial the daemon client socket. If the dial fails
// (most commonly because the daemon is not running), it returns a clear
// user-facing error that names the start command rather than the raw
// syscall message.
func dialDaemon(ctx context.Context, sockPath string) (net.Conn, error) {
	d := net.Dialer{Timeout: spawnDialTimeout}
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, daemonNotRunningError(sockPath, err)
	}
	return conn, nil
}

// daemonNotRunningError formats the canonical "daemon not running" message.
// It is the SAME error regardless of the underlying syscall failure mode
// (ENOENT vs ECONNREFUSED) so that scripts can match on a single message.
func daemonNotRunningError(sockPath string, cause error) error {
	return fmt.Errorf(
		"iris daemon not running (could not dial %s: %v); start it with: systemctl --user start iris",
		sockPath, cause,
	)
}

// sendSpawnFrame writes a ClientSessionSpawnFrame to the daemon connection.
// The frame format is the D-6 wire protocol (internal/iris/client_protocol.go);
// this function MUST NOT add fields not defined there.
func sendSpawnFrame(conn net.Conn, worktree, role string) error {
	frame := iris.ClientSessionSpawnFrame{
		Type:     iris.ClientFrameSessionSpawn,
		Worktree: worktree,
		Role:     role,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return err
	}
	return nil
}

// readSpawnAck reads JSON-line frames from conn until it sees either:
//
//   - session_spawned     — success; returns (name, instance_id, nil)
//   - error               — daemon-side failure; returns ("", "", error)
//   - EOF                 — daemon died mid-spawn; returns clear error
//   - context cancelled   — caller gave up; returns ctx.Err()
//   - read deadline       — no frame within spawnAckTimeout; returns clear error
//
// Unknown frame types are logged and skipped (forward-compatibility — matches
// the daemon's own behaviour for unknown frames).
func readSpawnAck(ctx context.Context, conn net.Conn) (string, string, error) {
	// Honour context cancellation by closing the conn from a goroutine.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()

	if err := conn.SetReadDeadline(time.Now().Add(spawnAckTimeout)); err != nil {
		return "", "", fmt.Errorf("iris spawn: set read deadline: %w", err)
	}

	r := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return "", "", ctx.Err()
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return "", "", errors.New("iris spawn: daemon closed connection before sending session_spawned ack (daemon may have died mid-spawn)")
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				return "", "", fmt.Errorf("iris spawn: timed out after %s waiting for session_spawned ack", spawnAckTimeout)
			}
			return "", "", fmt.Errorf("iris spawn: read ack: %w", err)
		}

		// Peek at the type only — we don't unmarshal the whole frame until we
		// know which type to decode it as.
		var generic struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &generic); err != nil {
			// Malformed frame — keep reading; the daemon may follow up with
			// a real ack. Don't treat as fatal.
			fmt.Fprintf(os.Stderr, "[iris] warning: ignoring malformed frame from daemon: %v\n", err)
			continue
		}

		switch generic.Type {
		case iris.DaemonFrameSessionSpawned:
			var f iris.DaemonSessionSpawnedFrame
			if err := json.Unmarshal(line, &f); err != nil {
				return "", "", fmt.Errorf("iris spawn: decode session_spawned: %w", err)
			}
			return f.Name, f.InstanceID, nil
		case iris.DaemonFrameError:
			var f iris.DaemonErrorFrame
			if err := json.Unmarshal(line, &f); err != nil {
				return "", "", fmt.Errorf("iris spawn: decode error frame: %w", err)
			}
			return "", "", fmt.Errorf("iris spawn: daemon rejected spawn: %s", f.Message)
		default:
			// Unknown frame — log and keep reading. The daemon emits no
			// other frames before the ack today, but future versions might
			// add log/progress frames. Forward-compat per §4 of the design doc.
			fmt.Fprintf(os.Stderr, "[iris] note: skipping pre-ack frame of type %q\n", generic.Type)
		}
	}
}
