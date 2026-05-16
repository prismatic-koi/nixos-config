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

	database, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris cleanup: open db: %w", err)
	}
	defer database.Close()

	// KillFn (issue #1699): the cleanup library invokes this once per
	// session being cleaned up (parent + each review-group child) BEFORE
	// the archive step, so kill-then-archive (#1692) holds at every
	// recursion level. --skip-kill propagates by leaving KillFn nil.
	var killFn func(string) string
	if !cleanupSkipKill {
		sockPath := p.Sock
		killFn = func(name string) string {
			return killSessionViaDaemon(sockPath, name)
		}
	}

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:       database,
		RunDir:         p.RunDir,
		LogDir:         p.LogDir,
		ArchiveRoot:    p.ArchiveRoot,
		PIAgentDir:     cleanupPIAgentDir,
		RemoveWorktree: cleanupRemoveWorktree,
		KillFn:         killFn,
	}, sessionName)
	if err != nil {
		return fmt.Errorf("iris cleanup: %w", err)
	}

	printCleanupResult(os.Stdout, res, cleanupSkipKill)
	return nil
}

// printCleanupResult renders a single cleanup result (parent + recursive
// children, if any) to out. Pulled out as a free function so it can be
// reused by future bulk-cleanup paths and unit-tested without going
// through the cobra command.
func printCleanupResult(out io.Writer, res *iris.CleanupResult, skipKill bool) {
	fmt.Fprintf(out, "iris cleanup: %s\n", res.SessionName)
	printCleanupBody(out, res, skipKill, "  ")
	if len(res.Children) > 0 {
		fmt.Fprintf(out, "  children:       %d cleaned up\n", len(res.Children))
		for _, child := range res.Children {
			fmt.Fprintf(out, "    - %s\n", child.SessionName)
			printCleanupBody(out, child, skipKill, "        ")
		}
	}
}

// printCleanupBody renders the per-session lines (kill, archive, run dir,
// etc.) at the given indent. Used for both the parent and each child.
func printCleanupBody(out io.Writer, res *iris.CleanupResult, skipKill bool, indent string) {
	kill := res.KillSummary
	if kill == "" {
		if skipKill {
			kill = "skipped (--skip-kill)"
		} else {
			kill = "skipped (no KillFn)"
		}
	}
	fmt.Fprintf(out, "%skill:           %s\n", indent, kill)
	if res.ArchivePath != "" {
		fmt.Fprintf(out, "%sarchive:        %s\n", indent, res.ArchivePath)
	} else {
		fmt.Fprintf(out, "%sarchive:        (skipped — no pi JSONL found)\n", indent)
	}
	fmt.Fprintf(out, "%srun dir:        removed=%v\n", indent, res.RunDirRemoved)
	fmt.Fprintf(out, "%slog file:       removed=%v\n", indent, res.LogFileRemoved)
	fmt.Fprintf(out, "%ssession row:    ended=%v\n", indent, res.SessionRowRemoved)
	fmt.Fprintf(out, "%sworktree:       removed=%v\n", indent, res.WorktreeRemoved)
	fmt.Fprintf(out, "%sbranch:         removed=%v\n", indent, res.BranchRemoved)
	if len(res.Errors) > 0 {
		fmt.Fprintf(out, "%serrors:\n", indent)
		for _, e := range res.Errors {
			fmt.Fprintf(out, "%s  - %v\n", indent, e)
		}
	}
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
