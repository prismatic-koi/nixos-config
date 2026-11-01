package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/review"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// Seams for the sidecar stops of the cleanup paths. Tests replace them, so
// that a test signals no process.
var (
	cleanupStopSidecar = func(session string) error {
		return prismSession.EnsureSidecarStoppedErr(session, prismSession.SidecarStopTimeout)
	}
	cleanupReviewChildren = review.CleanupReviewSessionsForParent
	// stopSessionSandboxProcs ends each process in the sandbox of a session
	// (#3375). The cleanup paths, the stop before a conversion, and the
	// tmux-session-end hook call it after the tmux kill.
	stopSessionSandboxProcs = func(session string) error {
		return prismSession.StopSandboxProcesses(session, prismSession.SandboxProcessStopTimeout)
	}
)

// errSidecarNotStopped is in the error chain of a cleanup path that kept a
// session because its sidecar did not stop.
var errSidecarNotStopped = errors.New("sidecar did not stop")

// errSandboxProcessRuns is in the error chain of a cleanup path that kept
// the row of a session because a process in its sandbox still runs after
// the tmux kill.
var errSandboxProcessRuns = errors.New("a process in the sandbox of the session still runs")

// sandboxProcessKeptError returns the error of a cleanup path that stopped
// after the tmux kill of session, because stopErr reports a process in the
// sandbox of session that prism cannot end. The path keeps the row, the
// port, the review children, and the instance dirs, so that a later
// cleanup of the session ends the process and does the rest. stopErr names
// the process and the session.
func sandboxProcessKeptError(session string, stopErr error) error {
	proglog.Warnf("[prism] warning: a process in the sandbox of %q still runs after the tmux kill — keeping its row, port, and review children\n", session)
	return errors.Join(stopErr, fmt.Errorf("%w: prism kept the row, the port, and the review children of %s, so prism confirm-bare and prism convert refuse a project root that the session can write. Run the cleanup again when the process is gone", errSandboxProcessRuns, strconv.Quote(session)))
}

// sidecarsNotStoppedError returns nil when sessions is empty. Otherwise it
// returns an error that names each session, so that the command exits
// non-zero.
func sidecarsNotStoppedError(sessions []string) error {
	if len(sessions) == 0 {
		return nil
	}
	quoted := make([]string, len(sessions))
	for i, s := range sessions {
		quoted[i] = strconv.Quote(s)
	}
	return fmt.Errorf("%w for %s: prism kept the tmux session, the row, and the port of each one", errSidecarNotStopped, strings.Join(quoted, ", "))
}

// keptSessionError returns the error of a path that kept a session because
// its sidecar stop failed with stopErr. stopErr names the PID and the
// session. failed names each kept session (sidecarsNotStoppedError).
func keptSessionError(stopErr error, failed []string) error {
	return errors.Join(stopErr, sidecarsNotStoppedError(failed))
}

// cleanupChildrenOfKeptSession runs when the sidecar of session did not
// stop. The tmux session, the row, the port, the worktree, and the branch
// of session stay, because the sidecar can still use them. The review
// children are cleaned up as usual, because each child has its own sidecar
// stop. It returns session and each child whose sidecar did not stop.
func cleanupChildrenOfKeptSession(session string) []string {
	proglog.Warnf("[prism] warning: sidecar of %q did not stop — keeping its tmux session, row, port, worktree, and branch\n", session)
	failed := []string{session}
	d, err := openDB()
	if err != nil {
		proglog.Errorf("[prism] cleanup: open db: %v\n", err)
		killOrphanReviewSidecars(session, nil)
		return failed
	}
	defer d.Close()
	stopAndRemoveChildContainers(d, session)
	// The tmux session of session still runs, so the hand-over of the child
	// audit logs reads the instance ID of session from agent_status.
	keepChildren := cleanupReviewChildren(d, session, "")
	// After the child stop: see the header of cleanup_orphan_sidecars.go.
	killOrphanReviewSidecars(session, keepChildren)
	return append(failed, keepChildren...)
}

// runCleanupTUI runs the interactive cleanup of m. The TUI closes as soon
// as the cleanup is done, so it returns the error of a sidecar that did not
// stop, or of a process in the sandbox that still runs: the command then
// names the session and exits non-zero.
func runCleanupTUI(m cleanupModel) error {
	final, err := tea.NewProgram(cleanupWrapper{cleanupModel: m}, tea.WithAltScreen()).Run()
	if err != nil {
		return err
	}
	if w, ok := final.(cleanupWrapper); ok && (errors.Is(w.doneErr, errSidecarNotStopped) || errors.Is(w.doneErr, errSandboxProcessRuns)) {
		return w.doneErr
	}
	return nil
}
