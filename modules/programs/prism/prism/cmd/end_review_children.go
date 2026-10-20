package cmd

import (
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/review"
)

// stopReviewChildrenForEnd stops the sidecars of the open review children
// of a session before setEndedStoppingReviewChildren ends the session.
// Tests replace it, so that a test signals no process and closes no tmux
// session.
var stopReviewChildrenForEnd = review.StopReviewChildrenForEnd

// setEndedStoppingReviewChildren ends the row of session. db.SetEnded also
// ends every open `<session>~review-*` row, so the sidecar of each of those
// children stops first. A child whose sidecar does not stop keeps its row
// and its port (#3168).
func setEndedStoppingReviewChildren(d *db.DB, session string) error {
	_, err := endStoppingReviewChildren(d, session)
	return err
}

// endStoppingReviewChildren is setEndedStoppingReviewChildren, and also
// returns the children whose sidecar did not stop.
func endStoppingReviewChildren(d *db.DB, session string) ([]string, error) {
	keep := stopReviewChildrenForEnd(d, session)
	return keep, d.SetEndedExcept(session, keep)
}
