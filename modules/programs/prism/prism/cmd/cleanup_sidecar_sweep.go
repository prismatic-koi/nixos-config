package cmd

import (
	"os"
	"time"

	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/review"
)

// sweepEndedSessionsAfterCleanup runs the ended-session sweeps at the end
// of a `prism cleanup` or `prism close` path. It catches a sidecar that an
// earlier path left behind, for example a review round whose monitor
// stopped before its release (#3160), and a per-session TMPDIR that no live
// session owns (#3170).
//
// It runs on the host only. Inside a sandbox the cleanup is a proxy, and
// the host side runs the sweep. Failure is non-fatal: the sweep fails
// closed, and a cleanup must not fail because of it.
func sweepEndedSessionsAfterCleanup() {
	if os.Getenv("PRISM_HOST_API") != "" {
		return
	}
	d, err := openDB()
	if err != nil {
		proglog.Warnf("[prism] warning: ended-session sweep: open db: %v\n", err)
		return
	}
	defer d.Close()
	if _, err := review.SweepEndedSessions(d, time.Now()); err != nil {
		proglog.Warnf("[prism] warning: ended-session sweep: %v\n", err)
	}
}
