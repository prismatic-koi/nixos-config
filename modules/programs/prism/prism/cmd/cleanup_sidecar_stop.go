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
	cleanupStopSidecar = func(session string) bool {
		return prismSession.EnsureSidecarStopped(session, prismSession.SidecarStopTimeout)
	}
	cleanupReviewChildren = review.CleanupReviewSessionsForParent
)

// errSidecarNotStopped is in the error chain of a cleanup path that kept a
// session because its sidecar did not stop.
var errSidecarNotStopped = errors.New("sidecar did not stop")

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

// cleanupChildrenOfKeptSession runs when the sidecar of session did not
// stop. The tmux session, the row, the port, the worktree, and the branch
// of session stay, because the sidecar can still use them. The review
// children are cleaned up as usual, because each child has its own sidecar
// stop. It returns session and each child whose sidecar did not stop.
func cleanupChildrenOfKeptSession(session string) []string {
	proglog.Warnf("[prism] warning: sidecar of %q did not stop — keeping its tmux session, row, port, worktree, and branch\n", session)
	failed := []string{session}
	killOrphanReviewSidecars(session)
	d, err := openDB()
	if err != nil {
		proglog.Errorf("[prism] cleanup: open db: %v\n", err)
		return failed
	}
	defer d.Close()
	stopAndRemoveChildContainers(d, session)
	return append(failed, cleanupReviewChildren(d, session)...)
}

// runCleanupTUI runs the interactive cleanup of m. The TUI closes as soon
// as the cleanup is done, so it returns the error of a sidecar that did not
// stop: the command then names the session and exits non-zero.
func runCleanupTUI(m cleanupModel) error {
	final, err := tea.NewProgram(cleanupWrapper{cleanupModel: m}, tea.WithAltScreen()).Run()
	if err != nil {
		return err
	}
	if w, ok := final.(cleanupWrapper); ok && errors.Is(w.doneErr, errSidecarNotStopped) {
		return w.doneErr
	}
	return nil
}
