package session

import (
	"errors"
	"strings"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// git.CloneWorktree and git.ConvertToBare refuse while a session in the
// target root, or above it, runs (#3381). internal/git cannot import this
// package, so this package gives it the check.
func init() {
	git.SetSessionRunsSource(SessionRunsInTmux)
}

// SessionRunsInTmux asks the tmux server of the host for the session. A
// sandbox has no access to that server, so a session cannot change the
// answer.
func SessionRunsInTmux(name string) (bool, error) {
	switch TmuxSessionPresence(name) {
	case TmuxPresent:
		return true, nil
	case TmuxGone:
		return false, nil
	default:
		return false, errors.New("tmux gave no clear answer")
	}
}

// TmuxPresence is the answer of TmuxSessionPresence.
type TmuxPresence int

const (
	// TmuxPresenceUnknown: tmux gave an answer that does not prove either
	// state, for example an exec failure. Do not act on it.
	TmuxPresenceUnknown TmuxPresence = iota
	// TmuxPresent: the tmux server lists the session.
	TmuxPresent
	// TmuxGone: the tmux server runs and does not list the session, or no
	// tmux server runs on the socket of this process.
	TmuxGone
)

// listTmuxSessions returns the output of `tmux list-sessions`. Tests
// replace it.
var listTmuxSessions = func() (string, error) {
	return tmux.Run("list-sessions", "-F", "#{session_name}")
}

// TmuxSessionPresence reports whether a tmux session with exactly the name
// sessionName exists on the tmux server of this process.
//
// It compares the names that list-sessions prints. Do not use a `-t`
// target here: tmux matches a target by prefix, and it reads a "." in a
// target as a pane separator, so a branch name with a dot does not resolve.
func TmuxSessionPresence(sessionName string) TmuxPresence {
	out, err := listTmuxSessions()
	if err != nil {
		return classifyTmuxListError(err.Error())
	}
	for _, line := range strings.Split(out, "\n") {
		if line == sessionName {
			return TmuxPresent
		}
	}
	return TmuxGone
}

// classifyTmuxListError maps the error text of a failed list-sessions to a
// presence. Only the texts that prove that no server runs on the socket
// give TmuxGone. A server exits when its last session closes, so no server
// means no session.
func classifyTmuxListError(msg string) TmuxPresence {
	switch {
	case strings.Contains(msg, "no server running"):
		return TmuxGone
	case strings.Contains(msg, "error connecting to") &&
		(strings.Contains(msg, "No such file or directory") || strings.Contains(msg, "Connection refused")):
		return TmuxGone
	default:
		return TmuxPresenceUnknown
	}
}

// HasExactSession reports whether a tmux session with exactly the name
// exists. It answers false when tmux cannot be queried.
func HasExactSession(sessionName string) bool {
	return TmuxSessionPresence(sessionName) == TmuxPresent
}
