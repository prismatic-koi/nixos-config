package cmd

// Keep the merge queue of a coordinator session across a stop of its
// sidecar (#3166, #3175).
//
// The Shutdown of a coordinator sidecar abandons each watching row of its
// instance, because the sidecar cannot tell a restart from an end. prism
// restore and the stale-sidecar path of prism switch restart the session,
// so they give the rows back to the new sidecar with db.ResumeMerges.
//
// Only one watcher may poll a row. The watcher polls the watching rows of
// its session name, whatever their instance ID. Thus the rows are resumed
// only after the stop proved that no process that can be the old sidecar
// runs. Without that proof, the rows do not change, and the caller prints
// why.

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/prismatic-koi/prism/internal/db"
)

// withMergesAbandonedByStop returns queued plus the rows of sessionName
// that the stop which ended the incarnation instanceID abandoned, with no
// duplicates, in queue order. That stop can come before the restart, as at
// a host shutdown, so the caller cannot read those rows as watching first.
//
// Call it only for a session that agent_status shows as active, and before
// the incarnation is reopened. A stop that ends the session also ends the
// agent_status row, and its rows must not come back.
func withMergesAbandonedByStop(d *db.DB, sessionName, instanceID string, queued []db.PendingMerge) ([]db.PendingMerge, error) {
	if instanceID == "" {
		return queued, nil
	}
	stopped, err := d.MergesAbandonedByStop(sessionName, instanceID)
	if err != nil {
		return queued, err
	}
	out := slices.Clone(queued)
	for _, m := range stopped {
		if !slices.ContainsFunc(out, func(q db.PendingMerge) bool { return q.Repo == m.Repo && q.PR == m.PR }) {
			out = append(out, m)
		}
	}
	slices.SortStableFunc(out, func(a, b db.PendingMerge) int { return cmp.Compare(a.QueuePosition, b.QueuePosition) })
	return out, nil
}

// resumeMergeQueue gives the merge-queue rows in queued back to the queue
// of sessionName, owned by instanceID, and prints the result. logPrefix
// starts each line on stderr. A row that the old watcher ended stays
// ended, so its outcome is not notified again.
//
// oldSidecarGone reports that the stop proved that no process that can be
// the old sidecar runs. When it is false, the rows do not change, because
// that process can still watch them.
func resumeMergeQueue(d *db.DB, logPrefix, sessionName, instanceID string, queued []db.PendingMerge, oldSidecarGone bool) {
	if len(queued) == 0 {
		return
	}
	if !oldSidecarGone {
		// The stop already wrote a warning that names the process.
		fmt.Fprintf(os.Stderr, "%s %q: the merge queue is not resumed, because the old sidecar can still run and watch it (see the warning above): %s\n",
			logPrefix, sessionName, formatMergePRs(queued))
		return
	}
	if instanceID == "" {
		fmt.Fprintf(os.Stderr, "%s %q: no instance ID, so the merge queue is not resumed: %s\n",
			logPrefix, sessionName, formatMergePRs(queued))
		return
	}
	resumed, err := d.ResumeMerges(sessionName, instanceID, queued)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %q: resume merge queue %s: %v\n", logPrefix, sessionName, formatMergePRs(queued), err)
		return
	}
	if len(resumed) > 0 {
		fmt.Printf("session %q: merge queue resumed: %s\n", sessionName, formatPRNumbers(resumed))
	}
}

func formatMergePRs(merges []db.PendingMerge) string {
	prs := make([]int, 0, len(merges))
	for _, m := range merges {
		prs = append(prs, m.PR)
	}
	return formatPRNumbers(prs)
}

func formatPRNumbers(prs []int) string {
	parts := make([]string, 0, len(prs))
	for _, pr := range prs {
		parts = append(parts, fmt.Sprintf("PR #%d", pr))
	}
	return strings.Join(parts, ", ")
}
