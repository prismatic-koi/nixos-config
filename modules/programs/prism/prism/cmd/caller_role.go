package cmd

import (
	"github.com/prismatic-koi/prism/internal/authz"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
)

// callerHasCoordinatorRole reports whether callerSession has the coordinator
// role on the direct CLI route. Two arms admit it:
//
//   - session.IsCoordinatorSession: a coordinator.
//   - authz.IsNonWorktreeRootFromDB: prism started the session as the root
//     session of a non-worktree project (for example `obsidian`).
//
// The host-API route ORs the same two arms in requireCoordinator. It reads
// the second arm from the start config of the sidecar instead of the DB.
// Every direct-CLI coordinator guard must call this function. If a guard
// calls session.IsCoordinatorSession alone, a `host`-mode root session of a
// non-worktree project gets a different answer on the two routes.
func callerHasCoordinatorRole(callerSession string, d *db.DB) bool {
	return session.IsCoordinatorSession(callerSession, d) || authz.IsNonWorktreeRootFromDB(callerSession, d)
}
