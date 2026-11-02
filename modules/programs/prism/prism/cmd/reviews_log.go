package cmd

// prism reviews log — print the review monitor log of one review round
// (issue #3277).
//
// `prism reviews log` reaches the log by one of two routes:
//
//   - Proxy route (PRISM_HOST_API set — a sandboxed caller): the host-API
//     handler GET /groups/monitor-log (internal/sidecar/host_api_review_log.go)
//     applies the access rule. The host takes the caller from the session of
//     its own sidecar.
//   - Direct route (PRISM_HOST_API unset — a `host`-isolation caller):
//     fetchReviewLogsLocal below applies the same rule. The caller is
//     PRISM_SESSION_NAME, else the current tmux session.
//
// Both routes call authz.AuthorizeReviewLog. When you change one route,
// change the other in the same edit.

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/authz"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
)

var reviewsLogCmd = &cobra.Command{
	Use:   "log <round>",
	Short: "Print the review monitor log of one review round",
	Long: `Print the log of the review monitor of one review round.

The review monitor waits for the review agents of a round, and delivers the
review-complete prompt. Its log shows what it saw and did. Use the log to
debug a round when the monitor did not deliver its result, or delivered a
wrong result.

With no --session, the command reads a round of your own session.

Access:
  - Each session can read the logs of its own review rounds.
  - A coordinator can also read the logs of the review rounds of a session
    in its own repo, with --session <session>.
  - prism refuses all other reads.

  prism reviews log 2
  prism reviews log 1 --session nixos-config@feature`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewsLog,
}

func init() {
	reviewsLogCmd.Flags().String("session", "", "Read a review round of this session (coordinator only for another session; default: your own session)")
	reviewsCmd.AddCommand(reviewsLogCmd)
}

func runReviewsLog(cmd *cobra.Command, args []string) error {
	round, err := strconv.Atoi(args[0])
	if err != nil || round <= 0 {
		return fmt.Errorf("prism reviews log: invalid round %q: the round must be a positive integer", args[0])
	}
	target, _ := cmd.Flags().GetString("session")
	if target != "" {
		if err := authz.ValidateReviewLogTarget(target); err != nil {
			return fmt.Errorf("prism reviews log: %w", err)
		}
	}

	var logs []review.MonitorLog
	if apiURL := sandboxenv.HostAPISocket(); apiURL != "" {
		logs, err = fetchReviewLogsProxy(apiURL, target, round)
	} else {
		logs, err = fetchReviewLogsLocal(review.LookupParentSession(), target, round)
	}
	if err != nil {
		return fmt.Errorf("prism reviews log: %w", err)
	}
	renderReviewLogs(cmd.OutOrStdout(), logs)
	return nil
}

// fetchReviewLogsProxy reads the logs through the host API. The request
// carries no caller: the host uses the session of its own sidecar.
func fetchReviewLogsProxy(apiURL, target string, round int) ([]review.MonitorLog, error) {
	params := map[string]string{"round": strconv.Itoa(round)}
	if target != "" {
		params["session"] = target
	}
	var resp struct {
		Logs []review.MonitorLog `json:"logs"`
	}
	if err := proxyGetFromHostAPI(apiURL, "/groups/monitor-log", params, &resp); err != nil {
		return nil, err
	}
	return resp.Logs, nil
}

// fetchReviewLogsLocal is the direct route of a `host`-mode session. It
// refuses an unresolvable caller before it opens the DB, as
// authorizeDirectCheckinFor does.
func fetchReviewLogsLocal(caller, target string, round int) ([]review.MonitorLog, error) {
	if caller == "" {
		return nil, fmt.Errorf("cannot determine caller session — run from inside a prism tmux session or set PRISM_SESSION_NAME")
	}
	if target == "" {
		target = caller
	}

	d, err := openDB()
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	decision := authz.AuthorizeReviewLog(authz.ReviewLogRequest{
		Caller:              caller,
		CallerIsCoordinator: callerHasCoordinatorRole(caller, d),
		Target:              target,
		DB:                  d,
		Logger:              log.Default(),
	})
	if !decision.Allow {
		return nil, errors.New(decision.Message)
	}
	return review.ReadMonitorLogs(d, target, round)
}

// renderReviewLogs writes the logs to w. When a round has more than one
// group, a header line names the group before each log.
func renderReviewLogs(w io.Writer, logs []review.MonitorLog) {
	for _, l := range logs {
		if len(logs) > 1 {
			fmt.Fprintf(w, "== group %s (PR #%s, round %d) ==\n", l.GroupID, l.PRNumber, l.Round)
		}
		fmt.Fprint(w, l.Content)
	}
}
