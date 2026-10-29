package cmd

// The spawn budget line of `prism spawn` (issue #3177).
//
// The lookup runs host-side only. A sandboxed caller reaches runSpawn
// through the host-API /spawn handler, which runs `prism spawn` on the host
// and returns the budget line in its response. See internal/spawnbudget.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
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

// overBudgetHelp is the help text of --over-budget.
const overBudgetHelp = "Reason to start a worker when the 7-day worker spawn budget is at 100% of the limit or more. " +
	"Without it, prism refuses the spawn. Prism logs each override. Has no effect under the limit."

// overBudgetFlag returns the trimmed --over-budget reason. It returns an
// error when the flag is set with an empty reason, before any side effect.
func overBudgetFlag(cmd *cobra.Command) (string, error) {
	f := cmd.Flags().Lookup("over-budget")
	if f == nil {
		return "", nil
	}
	reason := strings.TrimSpace(f.Value.String())
	if f.Changed && reason == "" {
		return "", errors.New("prism spawn: --over-budget needs a reason. Example: --over-budget \"urgent fix\"")
	}
	return reason, nil
}

// enforceWorkerSpawnBudget looks up the spawn budget, writes the budget line
// to stderr when role is worker, and refuses the spawn when the count is at
// or over the limit and --over-budget is not set (issue #3178). A set reason
// starts the worker and logs the override. For every other role it does
// nothing: review agents, investigators, and coordinators do not count
// against the budget. An unknown count never refuses a spawn.
func enforceWorkerSpawnBudget(cmd *cobra.Command, role string, cfg config.Config, bareRoot, branch string) error {
	reason, err := overBudgetFlag(cmd)
	if err != nil {
		return err
	}
	r := printWorkerSpawnBudget(cmd.ErrOrStderr(), role, cfg)
	if r == nil || r.Level() != spawnbudget.LevelOver {
		return nil
	}
	if reason == "" {
		return fmt.Errorf("prism spawn: refused. %d of %d worker spawns in the last 7 days: the budget is used up. "+
			"To start this worker anyway, pass --over-budget <reason>", r.Count, r.Limit)
	}
	recordSpawnOverride(cmd.ErrOrStderr(), bareRoot, branch, reason)
	return nil
}

// recordSpawnOverride logs one override on stderr and writes the
// spawn.over_budget_override event that the exporter counts. A failed event
// write is a warning, not a refusal: the override is already granted.
func recordSpawnOverride(w io.Writer, bareRoot, branch, reason string) {
	repo := strings.TrimSuffix(filepath.Base(bareRoot), ".git")
	name, nameErr := session.NameFor(filepath.Join(bareRoot, branch), bareRoot)
	if nameErr != nil {
		name = repo + "@" + branch
	}
	now := time.Now()
	fmt.Fprintf(w, "spawn budget override: date=%s session=%s repo=%s reason=%q\n",
		now.UTC().Format(time.RFC3339), name, repo, reason)
	payload, _ := json.Marshal(map[string]string{
		"date": now.UTC().Format(time.RFC3339), "session_name": name, "repo": repo, "reason": reason,
	})
	d, err := openDB()
	if err == nil {
		defer d.Close()
		err = d.WriteEvent(db.Event{
			ID:          uuid.New().String(),
			SessionName: name,
			Repo:        repo,
			Worktree:    filepath.Join(bareRoot, branch),
			Type:        spawnbudget.EventSpawnOverride,
			Payload:     string(payload),
			CreatedAt:   now,
		})
	}
	if err != nil {
		fmt.Fprintf(w, "warning: could not record the spawn override event: %v\n", err)
	}
}

// printWorkerSpawnBudget looks up the spawn budget and writes the budget
// line to w when role is worker. For every other role it does nothing. It
// returns the result, or nil when it printed nothing.
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
