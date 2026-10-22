package session

import (
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
)

// SweepStaleSessionTmpDirs removes each per-session TMPDIR
// (/var/tmp/prism-<hash>) that no session owns (#3170), and returns the
// paths that it removed. It reads the live rows with the lookup of
// SweepEndedSidecars and fails closed in the same way: when it cannot read
// the rows, it removes no dir. container.SweepSessionTmpDirs holds the
// checks on each dir.
//
// A row keeps two dirs: the dir of its instance ID, and the dir of its
// container name. The dispatcher keys the TMPDIR on the container name when
// the row has no instance ID. A live row keeps its dirs. An ended row keeps
// a dir only when the dir is newer than its ended_at: a respawned agent-only
// session (an investigator) runs with the ended_at of its old row, which is
// the same rule that SweepEndedSidecars applies to a PID file.
//
// It reads the ended rows before the live rows. A row that becomes live
// between the two reads is then in both lists, and its dir stays.
func SweepStaleSessionTmpDirs(d *db.DB) ([]string, error) {
	return container.SweepSessionTmpDirs(func() ([]container.SessionTmpDirOwner, error) {
		if d == nil {
			return nil, errNilSweepDB
		}
		ended, err := d.AllEndedStatus()
		if err != nil {
			return nil, err
		}
		live, err := liveSessions(d)
		if err != nil {
			return nil, err
		}
		owners := make([]container.SessionTmpDirOwner, 0, 2*(len(ended)+len(live)))
		for _, st := range append(ended, live...) {
			if st.InstanceID != nil && *st.InstanceID != "" {
				owners = append(owners, container.SessionTmpDirOwner{Key: *st.InstanceID, EndedAt: st.EndedAt})
			}
			owners = append(owners, container.SessionTmpDirOwner{Key: container.NameForSession(st.SessionName), EndedAt: st.EndedAt})
		}
		return owners, nil
	})
}
