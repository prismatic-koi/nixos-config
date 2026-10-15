package cmd

// The spawn budget line of `prism spawn` (issue #3177).
//
// The lookup runs host-side only. A sandboxed caller reaches runSpawn
// through the host-API /spawn handler, which runs `prism spawn` on the host
// and returns the budget line in its response. See internal/spawnbudget.

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/spawnbudget"
)

// spawnBudgetLookup runs one budget lookup. Tests replace it.
var spawnBudgetLookup = lookupSpawnBudget

func lookupSpawnBudget(cfg config.Config) spawnbudget.Result {
	return spawnbudget.Lookup(context.Background(), spawnbudget.Options{
		PrometheusURL: cfg.SpawnBudgetPrometheusURL,
		Limit:         cfg.SpawnBudgetLimit,
		// The database opens inside the local count, so a slow open is also
		// under the lookup timeout.
		Local: func(ctx context.Context, since time.Time) (int, error) {
			d, err := openDB()
			if err != nil {
				return 0, err
			}
			defer d.Close()
			return spawnbudget.LocalCounter(d)(ctx, since)
		},
	})
}

// plannedSpawnRole is the role that a spawn on branch gets: agentFlag when
// set, otherwise coordinator on main and worker on every other branch. It
// gives the same role as session.DefaultAgent, which needs the worktree
// path and so cannot run before the worktree exists.
func plannedSpawnRole(agentFlag, branch string) string {
	switch {
	case agentFlag != "":
		return agentFlag
	case branch == "main":
		return "coordinator"
	default:
		return "worker"
	}
}

// printWorkerSpawnBudget looks up the spawn budget and writes the budget
// line to w when role is worker. For every other role it does nothing:
// review agents, investigators, and coordinators do not count against the
// budget. It returns the result, or nil when it printed nothing.
func printWorkerSpawnBudget(w io.Writer, role string, cfg config.Config) *spawnbudget.Result {
	if role != spawnbudget.WorkerRole {
		return nil
	}
	r := spawnBudgetLookup(cfg)
	fmt.Fprintln(w, r.Line())
	return &r
}

// printProxiedSpawnBudget writes the budget line that the host-API /spawn
// response carries. The host-side spawn prints the line only for a worker,
// so an empty line means a spawn of another role, or a sidecar that runs a
// prism binary without the budget relay.
func printProxiedSpawnBudget(w io.Writer, line string) {
	if line != "" {
		fmt.Fprintln(w, line)
	}
}
