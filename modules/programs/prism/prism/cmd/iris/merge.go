// merge.go — `iris merge <pr>` subcommand (D-10 parity).
//
// Enqueues a PR onto the iris merge queue (the pending_merges table —
// schema is shared with prism per §10.4 of the design doc). The actual
// transition through `watching` to a terminal state is driven by the
// daemon's merge-queue watcher (internal/iris/mergequeue.go). This
// subcommand only enqueues; querying status is a separate concern and
// will be added when iris grows a `merges` listing.
//
// As with all iris CLI surfaces, no prism binary is invoked.

package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

var (
	mergeSessionName string
	mergeTitle       string
)

var mergeCmd = &cobra.Command{
	Use:   "merge <pr>",
	Short: "Enqueue a PR for the iris merge queue",
	Long: `Enqueue a PR onto the iris pending_merges queue. The PR is inserted with
status "watching" and queue_position set to the current Unix millisecond
timestamp (preserves enqueue order). The daemon's merge-queue watcher,
running inside the coordinator session, then drives the row through to
a terminal state ("merged", "failed", "cancelled", or "abandoned").

The command is idempotent: re-enqueueing an already-watching PR returns
the existing row unchanged.

The --session flag is required when not running inside an iris coordinator
session. In a future change the daemon's session-aware dispatch will
populate it automatically.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pr, err := strconv.Atoi(args[0])
		if err != nil || pr <= 0 {
			return fmt.Errorf("iris merge: invalid PR number %q — must be a positive integer", args[0])
		}
		return runMerge(pr)
	},
}

func init() {
	mergeCmd.Flags().StringVar(&mergeSessionName, "session", "", "Coordinator session name to attach this PR to (required outside a daemon session)")
	mergeCmd.Flags().StringVar(&mergeTitle, "title", "", "PR title (optional, used for display in future `iris merges` listings)")
	rootCmd.AddCommand(mergeCmd)
}

func runMerge(pr int) error {
	if mergeSessionName == "" {
		return fmt.Errorf("iris merge: --session is required (no daemon-session auto-detection yet)")
	}
	p := iris.ResolvePaths()
	database, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris merge: open db: %w", err)
	}
	defer database.Close()

	var titlePtr *string
	if mergeTitle != "" {
		titlePtr = &mergeTitle
	}
	row, err := iris.EnqueueMerge(database, pr, mergeSessionName, "", titlePtr)
	if err != nil {
		return fmt.Errorf("iris merge: %w", err)
	}
	fmt.Printf("iris merge: PR #%d enqueued (status=%s, queue_position=%d)\n",
		row.PR, row.Status, row.QueuePosition)
	return nil
}
