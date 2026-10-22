package session

import (
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
)

// SweepStaleSessionTmpDirs removes each per-session TMPDIR
// (/var/tmp/prism-<hash>) that no live row owns (#3170), and returns the
// paths that it removed. It reads the live set with the lookup of
// SweepEndedSidecars and fails closed in the same way: when it cannot read
// the live rows, it removes no dir. container.SweepSessionTmpDirs holds the
// checks on each dir.
//
// A live row keeps two dirs: the dir of its instance ID, and the dir of its
// container name. The dispatcher keys the TMPDIR on the container name when
// the row has no instance ID.
func SweepStaleSessionTmpDirs(d *db.DB) ([]string, error) {
	return container.SweepSessionTmpDirs(func() ([]string, error) {
		live, err := liveSessions(d)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, 2*len(live))
		for _, st := range live {
			if st.InstanceID != nil && *st.InstanceID != "" {
				ids = append(ids, *st.InstanceID)
			}
			ids = append(ids, container.NameForSession(st.SessionName))
		}
		return ids, nil
	})
}
