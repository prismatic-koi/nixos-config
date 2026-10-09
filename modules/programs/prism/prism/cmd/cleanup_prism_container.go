package cmd

import (
	"context"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/proglog"
)

// sweepPrismContainersForSession stops every running `prism container`
// build, then removes every container and every built image, of each
// incarnation of session: the current one and every one that the sessions
// table holds. Ownership is the instance label (the build unit name for a
// running build), never the name.
//
// It issues podman commands only when one of those incarnations has a
// prism-container audit dir. Every request creates that dir before podman
// runs, so a session that never used `prism container` causes no podman
// command and no warning. A failure is a warning, never a cleanup error. A
// container that a failed sweep leaves behind still stops and removes
// itself: podman's own --timeout and --rm are in its argument vector.
func sweepPrismContainersForSession(session string) {
	d, err := openDB()
	if err != nil {
		return
	}
	defer d.Close()
	status, _ := d.CurrentStatus(session)
	ids := incarnationInstanceIDs(d, session, status)

	used := false
	for _, id := range ids {
		if container.PrismContainerAuditDirExists(id) {
			used = true
			break
		}
	}
	if !used {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), podmanSweepBudget)
	defer cancel()
	if err := prismcontainer.SweepSession(ctx, currentPrismContainerRunner(), currentPrismContainerBuildExecutor(), ids); err != nil {
		proglog.Warnf("[prism] warning: cleanup: prism-container sweep for %q failed (%v) — continuing cleanup\n", session, err)
	}
}

// gatherPrismContainerAuditLogs appends the prism-container audit log of
// every earlier incarnation of session to the log of the final incarnation,
// then removes the earlier audit dir.
//
// The archive copies the log of the final incarnation only. An earlier
// incarnation can end with no cleanup of its own (the `prism restore`
// fallback, or a new instance ID at session start). Without this step, its
// log is never archived and its dir is never removed. An earlier dir whose
// append fails stays on disk, and a warning names it.
func gatherPrismContainerAuditLogs(d *db.DB, session, finalID string) {
	for _, id := range incarnationInstanceIDs(d, session, nil) {
		if id == finalID || !container.PrismContainerAuditDirExists(id) {
			continue
		}
		if err := prismcontainer.AppendAuditLog(id, finalID); err != nil {
			proglog.Warnf("[prism] warning: archive: keep the prism-container audit log of incarnation %s of %q: append failed (%v)\n", id, session, err)
			continue
		}
		container.RemovePrismContainerAuditDir(id)
	}
}

// incarnationInstanceIDs returns the instance ID of the current incarnation
// of session and of every incarnation that the sessions table holds.
func incarnationInstanceIDs(d *db.DB, session string, status *db.Status) []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if status != nil && status.InstanceID != nil {
		add(*status.InstanceID)
	}
	sessions, err := d.SessionsByName(session)
	if err != nil {
		proglog.Warnf("[prism] warning: cleanup: list incarnations of %q failed (%v) — sweeping the current incarnation only\n", session, err)
	}
	for _, s := range sessions {
		add(s.InstanceID)
	}
	return ids
}
