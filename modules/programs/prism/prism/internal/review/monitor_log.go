package review

// monitor_log.go — read and remove the monitor logs of the review rounds of
// a parent session (issue #3277).
//
// The log path comes from the session_groups row of the round: the group ID,
// the PR number, and the round number. No value that a caller sends becomes
// part of the path. The caller names a parent session and a round number,
// and both only select rows.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
)

// ErrNoMonitorLog is in the error chain of ReadMonitorLogs when the round
// has no monitor log.
var ErrNoMonitorLog = errors.New("not found")

// MonitorLog is the monitor log of one review group.
type MonitorLog struct {
	GroupID  string `json:"group_id"`
	PRNumber string `json:"pr"`
	Round    int    `json:"round"`
	Content  string `json:"content"`
}

// ReadMonitorLogs returns the monitor log of each group of round `round` of
// parentSession, in the order that the groups started. A round can have more
// than one group: NextRoundNumber counts the agent sessions of the parent,
// so a run that created no agent session leaves its round number free for
// the next run.
//
// The caller must decide the access first (authz.AuthorizeReviewLog). This
// function does not check who asks.
func ReadMonitorLogs(d *db.DB, parentSession string, round int) ([]MonitorLog, error) {
	if round <= 0 {
		return nil, fmt.Errorf("invalid round %d: the round must be a positive integer", round)
	}
	groups, err := d.GroupsForParent(parentSession)
	if err != nil {
		return nil, err
	}
	var logs []MonitorLog
	foundRound := false
	for _, g := range groups {
		if g.Round != round {
			continue
		}
		foundRound = true
		path, pathErr := monitorLogPathOf(g)
		if pathErr != nil {
			return nil, pathErr
		}
		content, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read monitor log of group %s: %w", g.GroupID, readErr)
		}
		logs = append(logs, MonitorLog{GroupID: g.GroupID, PRNumber: g.PRNumber, Round: g.Round, Content: string(content)})
	}
	if !foundRound {
		return nil, fmt.Errorf("%w: session %s has no review round %d", ErrNoMonitorLog, parentSession, round)
	}
	if len(logs) == 0 {
		return nil, fmt.Errorf("%w: review round %d of session %s has no monitor log", ErrNoMonitorLog, round, parentSession)
	}
	return logs, nil
}

// monitorLogPathOf returns the monitor log path of group g. It refuses a
// path outside MonitorStateDir. MonitorLogPath removes every character that
// is not a letter, a digit, or "-", so the check holds by construction. It
// stays as a second barrier in front of the file read.
func monitorLogPathOf(g db.GroupInfo) (string, error) {
	path := MonitorLogPath(g.GroupID, g.PRNumber, g.Round)
	if filepath.Dir(path) != MonitorStateDir() {
		return "", fmt.Errorf("monitor log path %q of group %s is outside %s", path, g.GroupID, MonitorStateDir())
	}
	return path, nil
}

// RemoveMonitorLogsForParent removes the monitor log of each review round of
// parentSession. Errors are logged, not returned: a file that stays on disk
// does not stop the cleanup.
//
// It also removes a log at the legacy path, which has no group ID in its
// name. It keeps that file when another parent session has a round with the
// same PR number and round number, because the file can then hold the log
// of the other session.
func RemoveMonitorLogsForParent(d *db.DB, parentSession string) {
	groups, err := d.GroupsForParent(parentSession)
	if err != nil {
		proglog.Warnf("[prism] warning: list review groups of %q: %v\n", parentSession, err)
		return
	}
	for _, g := range groups {
		if path, pathErr := monitorLogPathOf(g); pathErr != nil {
			proglog.Warnf("[prism] warning: %v\n", pathErr)
		} else {
			removeMonitorLogFile(path, g.GroupID)
		}
		// The SQL equality in OtherParentHasRound does not match a NULL
		// pr_number or round, so it cannot prove that such a file is not
		// shared.
		if g.Round <= 0 || g.PRNumber == "" {
			continue
		}
		shared, sharedErr := d.OtherParentHasRound(parentSession, g.PRNumber, g.Round)
		if sharedErr != nil {
			proglog.Warnf("[prism] warning: keep legacy monitor log of group %s: %v\n", g.GroupID, sharedErr)
			continue
		}
		if !shared {
			removeMonitorLogFile(legacyMonitorLogPath(g.PRNumber, g.Round), g.GroupID)
		}
	}
}

func removeMonitorLogFile(path, groupID string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		proglog.Warnf("[prism] warning: remove monitor log of group %s: %v\n", groupID, err)
	}
}
