// cleanup.go — `iris cleanup <session>` subcommand (D-10 parity).
//
// Removes the artefacts of an iris session: archive JSONL, per-session
// run dir + tmpdir, worktree + branch (optional), DB row end-state.
// This is the iris analogue of `prism cleanup` and never invokes any
// prism code path — see internal/iris/cleanup.go for the implementation
// and contract.
//
// Issue #1674: cleanup now invokes session_kill against the running daemon
// before archiving so that the kill-then-archive flow is atomic from the
// user's perspective and no zombie pi process is left running. When the
// daemon is not reachable, cleanup proceeds with the DB-only steps and
// reports skipped=true on the kill summary line.

package main

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

var (
	cleanupRemoveWorktree bool
	cleanupPIAgentDir     string
	cleanupSkipKill       bool
)

// cleanupKillDialTimeout bounds the dial against the daemon socket; cleanup
// must not hang when the daemon is down (the DB-only path is still useful).
const cleanupKillDialTimeout = 2 * time.Second

// cleanupKillAckTimeout bounds how long cleanup waits for the daemon's
// session_killed (or error) frame. The daemon's own kill ladder is
// SIGTERM+5s+SIGKILL+2s = 7s upper bound, so 15s gives the daemon plenty of
// slack and still surfaces hangs as a clear error.
const cleanupKillAckTimeout = 15 * time.Second

var cleanupCmd = &cobra.Command{
	Use:   "cleanup <session>",
	Short: "Clean up an iris session (archive JSONL, remove run dir, end DB row)",
	Long: `Clean up the named iris session. Cleanup is best-effort: each step
runs independently, and partial failures are surfaced in the summary
output rather than aborting the command.

Steps:

  1. Archive the pi JSONL into ~/code/archives/iris/<session>/<instance>/raw/session.jsonl
  2. Mark the sessions row ended (end_state="finished") if not already terminal.
  3. Remove the per-session run dir at ~/.local/state/iris/run/<instance>/.
  4. Optionally remove the worktree directory and local git branch
     (use --remove-worktree).

The coordinator's main worktree is always protected — cleanup refuses to
remove a worktree whose basename is "main" under a prism .bare layout.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCleanup(args[0])
	},
}

func init() {
	cleanupCmd.Flags().BoolVar(&cleanupRemoveWorktree, "remove-worktree", false, "Remove the worktree directory and local git branch (coordinator worktree is always protected)")
	cleanupCmd.Flags().StringVar(&cleanupPIAgentDir, "pi-agent-dir", "", "Override the pi agent dir (default: ~/.pi/agent/)")
	cleanupCmd.Flags().BoolVar(&cleanupSkipKill, "skip-kill", false, "Skip the session_kill step (DB-only cleanup; pi child is left untouched)")
	rootCmd.AddCommand(cleanupCmd)
}

func runCleanup(sessionName string) error {
	p := iris.ResolvePaths()

	// Step 0 (issue #1674): kill the running pi child via the daemon before
	// archiving. Skipped explicitly with --skip-kill or implicitly when the
	// daemon socket is not reachable (the kill is a best-effort prelude to
	// the DB-only cleanup steps below; a dead daemon already means no live
	// pi child to kill).
	killSummary := "skipped (--skip-kill)"
	if !cleanupSkipKill {
		killSummary = killSessionViaDaemon(p.Sock, sessionName)
	}

	database, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris cleanup: open db: %w", err)
	}
	defer database.Close()

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:       database,
		RunDir:         p.RunDir,
		LogDir:         p.LogDir,
		ArchiveRoot:    p.ArchiveRoot,
		PIAgentDir:     cleanupPIAgentDir,
		RemoveWorktree: cleanupRemoveWorktree,
	}, sessionName)
	if err != nil {
		return fmt.Errorf("iris cleanup: %w", err)
	}

	fmt.Printf("iris cleanup: %s\n", sessionName)
	fmt.Printf("  kill:           %s\n", killSummary)
	if res.ArchivePath != "" {
		fmt.Printf("  archive:        %s\n", res.ArchivePath)
	} else {
		fmt.Printf("  archive:        (skipped — no pi JSONL found)\n")
	}
	fmt.Printf("  run dir:        removed=%v\n", res.RunDirRemoved)
	fmt.Printf("  log file:       removed=%v\n", res.LogFileRemoved)
	fmt.Printf("  session row:    ended=%v\n", res.SessionRowRemoved)
	fmt.Printf("  worktree:       removed=%v\n", res.WorktreeRemoved)
	fmt.Printf("  branch:         removed=%v\n", res.BranchRemoved)
	if len(res.Errors) > 0 {
		fmt.Println("  errors:")
		for _, e := range res.Errors {
			fmt.Printf("    - %v\n", e)
		}
	}
	return nil
}

// killSessionViaDaemon dials the iris daemon socket and sends a session_kill
// frame. Returns a one-line summary string suitable for printing in the
// cleanup output. All failure modes — daemon down, no such session,
// ack timeout — are non-fatal: the DB-only cleanup steps still run.
func killSessionViaDaemon(sockPath, sessionName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupKillAckTimeout+cleanupKillDialTimeout)
	defer cancel()

	d := net.Dialer{Timeout: cleanupKillDialTimeout}
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return fmt.Sprintf("skipped (daemon not running at %s)", sockPath)
	}
	defer conn.Close()

	frame := iris.ClientSessionKillFrame{
		Type: iris.ClientFrameSessionKill,
		Name: sessionName,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Sprintf("skipped (marshal kill frame: %v)", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return fmt.Sprintf("skipped (write kill frame: %v)", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(cleanupKillAckTimeout)); err != nil {
		return fmt.Sprintf("skipped (set read deadline: %v)", err)
	}
	r := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return "skipped (daemon closed connection before ack)"
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				return fmt.Sprintf("skipped (timed out after %s)", cleanupKillAckTimeout)
			}
			return fmt.Sprintf("skipped (read ack: %v)", err)
		}
		var generic struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &generic); err != nil {
			// Skip malformed frames; keep reading for the ack.
			fmt.Fprintf(os.Stderr, "[iris] cleanup: ignoring malformed frame from daemon: %v\n", err)
			continue
		}
		switch generic.Type {
		case iris.DaemonFrameSessionKilled:
			var f iris.DaemonSessionKilledFrame
			if err := json.Unmarshal(line, &f); err != nil {
				return fmt.Sprintf("acknowledged (decode: %v)", err)
			}
			return fmt.Sprintf("killed (state=%s)", f.State)
		case iris.DaemonFrameError:
			var f iris.DaemonErrorFrame
			if err := json.Unmarshal(line, &f); err != nil {
				return fmt.Sprintf("skipped (decode error frame: %v)", err)
			}
			return fmt.Sprintf("skipped (%s)", f.Message)
		default:
			// Other frames (session_event, session_state, etc.) may arrive
			// before the ack — the daemon is a multiplexed surface. Skip.
			continue
		}
	}
}
