package cmd

import (
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// cleanupKillTmuxSession is the tmux kill of the cleanup paths. Tests
// replace it, so that a test can act as the session-closed hook right after
// the kill.
var cleanupKillTmuxSession = tmux.KillSession

// cleanupSnapshot holds the values of the agent_status row of a session that
// the DB block of a cleanup path uses.
//
// A cleanup path takes the snapshot before the tmux kill. The kill fires the
// global session-closed hook, and `prism event tmux-session-end` then clears
// instance_id of the row. A read after the kill can get "", and the cleanup
// then skips the archive of the transcript and the removal of the instance
// directories (#3213).
type cleanupSnapshot struct {
	instanceID    string
	isolationMode string
}

// takeCleanupSnapshot reads the cleanupSnapshot of session. If the DB does
// not open, it returns empty values, and the cleanup skips the steps that
// need an instance ID, as for a row that has none.
func takeCleanupSnapshot(session string) cleanupSnapshot {
	d, err := openDB()
	if err != nil {
		proglog.Warnf("[prism] warning: cleanup: snapshot of %q: open db: %v\n", session, err)
		return cleanupSnapshot{}
	}
	defer d.Close()
	return cleanupSnapshot{
		instanceID:    instanceIDFromStatus(d, session),
		isolationMode: isolationModeFromDB(d, session),
	}
}

// sessionsRowOpen reports whether the sessions row of instanceID exists and
// has no ended_at. A cleanup path reads it before its own SetEnded, and
// calls UpdateSessionEnded only for an open row. The sidecar stop or the
// session-closed hook can end the row first, and the cleanup must not end it
// a second time.
func sessionsRowOpen(d *db.DB, instanceID string) bool {
	if instanceID == "" {
		return false
	}
	s, err := d.SessionByInstanceID(instanceID)
	return err == nil && s != nil && s.EndedAt == nil
}
