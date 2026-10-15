package cmd

import (
	"os"
	"time"

	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/review"
)

// sweepEndedSidecarsAfterCleanup runs the ended-session sidecar sweep at
// the end of a `prism cleanup` or `prism close` path (#3160). It catches a
// sidecar that an earlier path left behind, for example a review round
// whose monitor stopped before its release.
//
// It runs on the host only. Inside a sandbox the cleanup is a proxy, and
// the host side runs the sweep. Failure is non-fatal: the sweep fails
// closed, and a cleanup must not fail because of it.
func sweepEndedSidecarsAfterCleanup() {
	if os.Getenv("PRISM_HOST_API") != "" {
		return
	}
	d, err := openDB()
	if err != nil {
		proglog.Warnf("[prism] warning: ended-session sidecar sweep: open db: %v\n", err)
		return
	}
	defer d.Close()
	if _, err := review.SweepEndedSidecars(d, time.Now()); err != nil {
		proglog.Warnf("[prism] warning: ended-session sidecar sweep: %v\n", err)
	}
}
