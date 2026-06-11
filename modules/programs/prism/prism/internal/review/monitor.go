package review

// monitor.go — group-completion monitor for async review.
//
// After `prism review` spawns the 5 review-agent sessions and registers a
// group, it starts a detached monitor subprocess (via cmd/monitor_review.go
// using `prism monitor-review`). The monitor polls db.GroupCompleted every 5 s;
// when the group is complete it aggregates results via db.GroupResults,
// formats them with FormatResults, and delivers to the worker via
// `prism prompt`. On delivery failure, it retries with bounded backoff then
// writes the fallback file.
//
// MonitorOpts is passed from the cmd layer; MonitorFunc is the entry-point
// called by the monitor-review subcommand.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
)

// MonitorOpts configures the group-completion monitor.
type MonitorOpts struct {
	// GroupID is the session_groups.group_id to monitor.
	GroupID string
	// WorkerSession is the session name to deliver results to.
	WorkerSession string
	// PRNumber is the PR number string (e.g. "864") — used in the delivery
	// message, retry messages, and fallback file naming.
	PRNumber string
	// Round is the 1-indexed review round number — used in the fallback file name.
	Round int
	// Agents is the ordered list of agents spawned in this round. Used to
	// reconstruct the result ordering when GroupResults is incomplete.
	Agents []Agent
	// AgentSessions maps each agent index to its session name — needed for
	// buildResults to correlate GroupResults data with agent ordering.
	AgentSessions []string
	// DBPath is the path to the prism database. If empty, the default is used.
	DBPath string
	// PollInterval controls how often the monitor checks GroupCompleted.
	// Zero uses the default (5 s).
	PollInterval time.Duration
	// MaxDeliveryRetries is the number of times to retry delivery before writing
	// the fallback file. Zero uses the default (5).
	MaxDeliveryRetries int
	// DeliveryRetryBackoff is the base delay between delivery retries.
	// Zero uses the default (30 s).
	DeliveryRetryBackoff time.Duration
	// Timeout is the maximum time to wait for the group to complete. Zero means
	// no timeout (monitor runs until the group is complete).
	Timeout time.Duration
}

// defaultPollInterval is how often the monitor polls GroupCompleted.
const defaultPollInterval = 5 * time.Second

// defaultMaxDeliveryRetries is how many times to attempt prompt delivery.
const defaultMaxDeliveryRetries = 5

// defaultDeliveryRetryBackoff is the initial backoff between delivery retries.
const defaultDeliveryRetryBackoff = 30 * time.Second

// MonitorFunc is the entry-point for the group-completion monitor.
// It is called by the `prism monitor-review` subcommand after startup.
// It blocks until the group is complete (or the timeout fires), delivers
// results to the worker, and returns. The caller (monitor-review) may
// os.Exit after this returns.
func MonitorFunc(opts MonitorOpts) error {
	if opts.GroupID == "" {
		return fmt.Errorf("monitor-review: group_id is required")
	}
	if opts.WorkerSession == "" {
		return fmt.Errorf("monitor-review: worker_session is required")
	}

	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	maxRetries := opts.MaxDeliveryRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxDeliveryRetries
	}
	retryBackoff := opts.DeliveryRetryBackoff
	if retryBackoff <= 0 {
		retryBackoff = defaultDeliveryRetryBackoff
	}

	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = defaultDBPath()
	}

	d, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("monitor-review: open db: %w", err)
	}
	defer d.Close()

	proglog.Infof("[prism monitor-review] watching group %s for PR #%s (worker: %s)\n",
		opts.GroupID, opts.PRNumber, opts.WorkerSession)

	// Set deadline if timeout is specified.
	var deadline time.Time
	if opts.Timeout > 0 {
		deadline = time.Now().Add(opts.Timeout)
	}

	// Poll loop: check GroupCompleted every pollInterval.
	timedOut := false
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			proglog.Infof("[prism monitor-review] timeout reached for group %s — delivering partial results\n", opts.GroupID)
			timedOut = true
			break
		}

		done, groupErr := d.GroupCompleted(opts.GroupID)
		if groupErr != nil {
			proglog.Warnf("[prism monitor-review] warning: GroupCompleted(%s): %v — retrying\n", opts.GroupID, groupErr)
		} else if done {
			proglog.Infof("[prism monitor-review] group %s complete — aggregating results\n", opts.GroupID)
			break
		}

		time.Sleep(pollInterval)
	}

	// If the outer safety timeout fired, force-terminate any remaining
	// non-terminal members (#1709). Without this, agent rows stay in
	// `active` indefinitely, GroupCompleted keeps returning false on the
	// next call, and a subsequent `prism review` for the same parent
	// refuses with "round N already in progress". The per-session sidecar
	// inactivity watchdog should normally cover this case, but the monitor
	// adds a belt-and-braces sweep for sessions whose sidecar died,
	// crashed, or was misconfigured with ActivityTimeout=0.
	if timedOut {
		forceTerminateStuckMembers(d, opts.AgentSessions, opts.Timeout)
	}

	// Aggregate results.
	groupData, grErr := d.GroupResults(opts.GroupID)
	if grErr != nil {
		proglog.Warnf("[prism monitor-review] warning: GroupResults(%s): %v — using empty data\n", opts.GroupID, grErr)
		groupData = map[string]db.GroupMemberResult{}
	}

	// Build AgentResult slice, handling "missing" sessions (row deleted mid-review).
	results := buildMonitorResults(opts.Agents, opts.AgentSessions, groupData)

	// Format the delivery message.
	output, allPassed := FormatResults(results, opts.PRNumber, opts.Round, 0)
	deliveryText := buildDeliveryMessage(opts.PRNumber, opts.Round, output, allPassed, groupData, opts.AgentSessions)

	// Issue #2110: persist the latest-round review verdict and counts on the
	// worker's spawn_outcome row. This is the single write site that the AC
	// requires — it lives at the same point that constructs the delivery
	// prompt, so the persisted values match exactly what the worker sees.
	//
	// Latest-round-wins semantics: each MonitorFunc call represents one
	// review round. The UPSERT in UpdateSpawnOutcomeReviewResult overwrites
	// the previous round's values rather than summing across rounds, so a
	// worker that runs round 1 (FAIL) then round 2 (PASS) ends with the
	// round-2 values — its actual ship state. The verb is a single
	// transaction so verdict/passCount/failCount never drift relative to
	// each other under a partial-failure scenario.
	//
	// Failure is non-fatal: a write error logs and continues. The prompt
	// delivery is the user-facing path; the column persistence is purely
	// for `prism stats compare` reporting, and a missing column renders as
	// the existing — placeholder.
	persistReviewOutcome(d, opts.WorkerSession, results, allPassed)

	// LOOP-LIMIT footer (#1512). Append the footer to the prompt body when
	//   (a) the cycle has not converged (¬allPassed),
	//   (b) THIS cycle is itself a verdict-producing cycle — i.e. every
	//       member emitted a parseable `<verdict>` tag (#1995). When the
	//       cycle is NOT verdict-producing, the relevant branch in
	//       buildDeliveryMessage ("infrastructure failure" or
	//       "ran but produced no parseable verdict") already tells the
	//       worker to re-run — it is not yet at the limit, AND
	//   (c) the total count of verdict-producing cycles for this parent
	//       (including this one) is ≥ REVIEW_CYCLE_THRESHOLD.
	//
	// The footer is naturally rate-limited — it appears once per actual
	// completed verdict-producing cycle, embedded in the prompt the worker is
	// already going to act on. This dissolves the per-turn-spam and the
	// bash-substring false-match defects at the source (#1512).
	if !allPassed {
		if currentCycleProducedVerdicts(groupData) {
			prior, ccErr := CompletedReviewCyclesForParent(d, opts.WorkerSession, opts.GroupID)
			if ccErr != nil {
				proglog.Warnf("[prism monitor-review] warning: cycle count failed: %v — footer suppressed\n", ccErr)
			} else if prior+1 >= REVIEW_CYCLE_THRESHOLD {
				deliveryText += buildLoopLimitFooter(prior+1, opts.PRNumber)
			}
		}
	}

	// Before delivery, clear the worker's `reviewing` state by writing `active`.
	// Background: RunAsync writes `reviewing` to the DB so that incidental busy
	// turns + idle debounces in the worker session do not produce a premature
	// "has finished" notification while the review is in flight (see #1036 and
	// #1049). The sidecar's busy-event handler treats `reviewing` as a sticky
	// state and refuses to write `active` over it. We must therefore flip the
	// DB state to `active` ourselves *just before* delivering the review-
	// complete prompt — that way, the busy event triggered by the prompt
	// arriving is processed against `active`, the subsequent idle debounce
	// fires normally, and the genuine end-of-review handoff is observed.
	//
	// This write only runs while the worker is in `reviewing`; if the state
	// has already moved on (worker crashed, manual override, etc.) we leave it
	// alone. Failures here are non-fatal: the prompt is still delivered, and
	// at worst the suppression remains in effect (no spurious notifications,
	// just no end-of-review notification — the worker can still observe the
	// prompt directly).
	if workerStatus, stErr := d.CurrentStatus(opts.WorkerSession); stErr == nil && workerStatus != nil {
		if workerStatus.State == string(agent.StateReviewing) {
			if err := d.UpsertStatus(opts.WorkerSession, workerStatus.Repo, workerStatus.Worktree,
				string(agent.StateActive), nil, nil); err != nil {
				proglog.Warnf("[prism monitor-review] warning: could not clear reviewing→active before delivery: %v\n", err)
			} else {
				proglog.Infof("[prism monitor-review] worker state reviewing→active (pre-delivery)\n")
			}
		}
	} else if stErr != nil {
		proglog.Warnf("[prism monitor-review] warning: could not look up worker session %q before delivery: %v\n", opts.WorkerSession, stErr)
	}

	// Deliver to worker via prism prompt with bounded retry.
	// Use the same deterministic delivery_id as the recovery path so that if
	// the worker-sidecar recovery watcher (internal/sidecar/review_recovery.go)
	// races us to deliver for the same group, the sidecar's /prompt dedup set
	// (#1685) drops whichever copy arrives second. See recovery.go and
	// RecoveryDeliveryID for the shared-dedup-ID contract.
	monitorDeliveryID := RecoveryDeliveryID(opts.GroupID)
	deliverErr := deliverWithRetry(opts.WorkerSession, deliveryText, maxRetries, retryBackoff, dbPath, monitorDeliveryID)
	if deliverErr != nil {
		// Delivery failed after all retries — write fallback file.
		fallbackPath := fmt.Sprintf("/tmp/prism-review-%s-round-%d-result.md", sanitisePRNumber(opts.PRNumber), opts.Round)
		proglog.Errorf("[prism monitor-review] delivery failed after %d retries — writing fallback to %s\n", maxRetries, fallbackPath)
		writeErr := os.WriteFile(fallbackPath, []byte(deliveryText), 0o644)
		if writeErr != nil {
			proglog.Errorf("[prism monitor-review] error: could not write fallback file %s: %v\n", fallbackPath, writeErr)
		} else {
			proglog.Infof("[prism monitor-review] fallback file written to %s\n", fallbackPath)
		}
		return fmt.Errorf("monitor-review: delivery failed and fallback written to %s", fallbackPath)
	}

	proglog.Infof("[prism monitor-review] results delivered to %s\n", opts.WorkerSession)
	return nil
}

// persistReviewOutcome records the latest-round verdict and pass/fail counts
// on the worker's spawn_outcome row (issue #2110). It is intentionally a thin
// glue function so that the write-site for the three columns surfaces as a
// single point in MonitorFunc — callers can grep for the issue number and
// see the entire write path. The instance_id is resolved via the most-recent
// sessions row for the worker session name; a missing row makes the call a
// silent no-op.
//
// Counts: passCount = number of agents whose AgentResult.Passed is true (i.e.
// LastMessage carried a parseable `<verdict>PASS</verdict>` marker);
// failCount = number of agents that did not pass (FAIL verdicts, error states,
// no-start failures, finished-without-verdict). Verdict: "pass" when every
// agent passed; "fail" when at least one did not. The lowercase casing
// matches the existing ComputeSpawnOutcome convention so the renderer's
// existing pass-through display does not need a casing-aware code path.
func persistReviewOutcome(d *db.DB, workerSession string, results []AgentResult, allPassed bool) {
	if d == nil || workerSession == "" {
		return
	}
	sess, err := d.MostRecentSessionForName(workerSession)
	if err != nil {
		proglog.Warnf("[prism monitor-review] warning: persist review outcome: lookup session %q: %v\n", workerSession, err)
		return
	}
	if sess == nil || sess.InstanceID == "" {
		proglog.Infof("[prism monitor-review] persist review outcome: no sessions row for %q — skipping\n", workerSession)
		return
	}
	var passCount, failCount int
	for _, r := range results {
		if r.Passed {
			passCount++
		} else {
			failCount++
		}
	}
	verdict := "fail"
	if allPassed {
		verdict = "pass"
	}
	if err := d.UpdateSpawnOutcomeReviewResult(sess.InstanceID, verdict, passCount, failCount); err != nil {
		proglog.Warnf("[prism monitor-review] warning: UpdateSpawnOutcomeReviewResult(iid=%s, verdict=%s): %v\n", sess.InstanceID, verdict, err)
		return
	}
	proglog.Infof("[prism monitor-review] persisted review verdict=%s pass=%d fail=%d on worker spawn_outcome (iid=%s)\n", verdict, passCount, failCount, sess.InstanceID)
}

// forceTerminateStuckMembers walks the given session names and transitions any
// row still in a non-terminal state to `error` so the monitor's safety-timeout
// path leaves the review group genuinely terminal (#1709).
//
// Without this sweep, a row stuck at state="active" past the safety deadline
// keeps GroupCompleted returning false, blocks ActiveReviewGroupForParent
// from clearing, and forces the worker into manual `prism cleanup` recovery.
// The per-session sidecar inactivity watchdog should normally rescue these
// rows long before the monitor's outer timeout fires; this is the belt-and-
// braces fallback for the case where the sidecar itself is unreachable
// (crashed, killed without cleanup, etc.).
//
// After setting state="error" we also call SetEnded so that ended_at is
// written atomically for both agent_status and sessions rows. This mirrors
// cleanupAgentSession (lifecycle.go:170-178), which calls UpsertStatus then
// SetEnded for the same reason — without SetEnded the row has state="error"
// but ended_at=NULL, and any query that filters on "ended_at IS NOT NULL"
// (e.g. AllActiveStatus, dashboard active-session listings) will still treat
// the row as live. Same table-drift class as #1870 (MarkAllEnded/SetEnded)
// and #1881 (cleanupHalfAliveSession).
//
// All failures are logged but non-fatal: a stuck DB row is recoverable via
// `prism cleanup`, but a hard error here would also block the partial
// review-results delivery the monitor has already promised the worker.
func forceTerminateStuckMembers(d *db.DB, agentSessions []string, perAgentTimeout time.Duration) {
	for _, sess := range agentSessions {
		if sess == "" {
			continue
		}
		status, err := d.CurrentStatus(sess)
		if err != nil {
			proglog.Warnf("[prism monitor-review] warning: CurrentStatus(%s) during force-terminate sweep: %v\n", sess, err)
			continue
		}
		if status == nil {
			continue
		}
		if status.EndedAt != nil {
			continue
		}
		if isTerminalAgentState(status.State) {
			continue
		}
		proglog.Warnf("[prism monitor-review] force-terminating stuck member %s (state=%q, per-agent timeout=%v)\n",
			sess, status.State, perAgentTimeout)
		if err := d.UpsertStatus(sess, status.Repo, status.Worktree, "error", nil, nil); err != nil {
			proglog.Warnf("[prism monitor-review] warning: UpsertStatus(%s, error): %v\n", sess, err)
		}
		// Set ended_at so the row is fully terminal for queries filtering on
		// ended_at IS NOT NULL (see comment above). Best-effort: failure logs
		// and continues — this is already a recovery path.
		if err := d.SetEnded(sess); err != nil {
			proglog.Warnf("[prism monitor-review] warning: SetEnded(%s) after force-terminate: %v\n", sess, err)
		}
	}
}

// buildMonitorResults constructs AgentResult entries from GroupResults data,
// handling missing sessions (row deleted mid-review) as a special case.
// Missing agents are counted as an error result with state "missing".
func buildMonitorResults(agents []Agent, agentSessions []string, groupData map[string]db.GroupMemberResult) []AgentResult {
	results := make([]AgentResult, len(agents))
	for i, ag := range agents {
		agentSession := ""
		if i < len(agentSessions) {
			agentSession = agentSessions[i]
		}

		mr, ok := groupData[agentSession]
		if !ok || agentSession == "" {
			// Session was deleted mid-review or never registered — count as missing.
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  "ERROR: agent session not found in group (possibly deleted mid-review)",
				IsError: true,
			}
			continue
		}

		switch mr.State {
		case "error":
			// Distinguish the failure classes within state "error" (#1222,
			// #2239):
			//
			//   - no-start: a startup_error event was written (by the
			//     sidecar's writeStartupError, or by the inactivity watchdog
			//     when it fired with zero inbound frames) — the agent never
			//     ran. StartupError is non-empty.
			//   - mid-run stall: a stall_error event was written by the
			//     inactivity watchdog after one or more inbound frames were
			//     received — the agent ran, then went silent. StallError is
			//     non-empty and includes elapsed time, frame count, and the
			//     last-frame timestamp.
			//   - anything else: a mid-run crash.
			//
			// Label each clearly so the coordinator treats the first two as
			// infrastructure failures rather than code-quality verdicts.
			//
			// Note: "interrupted" is intentionally NOT bucketed with "error" here
			// (#1495). An interrupted agent that was redirected via `prism prompt`
			// and subsequently crashes still lands here with state="error" — the
			// genuine-error path is unchanged. "interrupted" only reaches this
			// switch via the default branch below, which would only fire if the
			// MonitorFunc poll loop hit its overall safety timeout while an agent
			// was still in the interrupted state (i.e. the user neither redirected
			// nor cleaned up). Without that safety timeout, GroupCompleted keeps
			// returning false and the monitor keeps waiting.
			if mr.StartupError != "" {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  fmt.Sprintf("ERROR: agent failed to start (no-start): %s", mr.StartupError),
					IsError: true,
				}
			} else if mr.StallError != "" {
				// The StallError reason already begins with "stalled mid-run
				// after <elapsed> (<n> frame(s) received, last at <t>)".
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  fmt.Sprintf("ERROR: agent %s", mr.StallError),
					IsError: true,
				}
			} else {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  fmt.Sprintf("ERROR: agent did not complete cleanly (state: %s)", mr.State),
					IsError: true,
				}
			}
		case "finished":
			if mr.LastMessage == "" {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  "ERROR: no output produced",
					IsError: true,
				}
				continue
			}
			text := extractAssistantText(mr.LastMessage)
			passed, kind := AssessPassed(text)
			if !passed && kind == VerdictNone {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  "ERROR: no verdict found in agent output — review output:\n" + text,
					IsError: true,
				}
				continue
			}
			results[i] = AgentResult{
				Agent:  ag,
				Passed: passed,
				Output: text,
			}
		default:
			// Non-terminal state after group says complete — treat as timed out / missing.
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: agent in unexpected state %q (may have timed out)", mr.State),
				IsError: true,
			}
		}
	}
	return results
}

// buildDeliveryMessage constructs the prompt text delivered to the worker when
// the review group completes.
func buildDeliveryMessage(prNumber string, round int, formattedResults string, allPassed bool, groupData map[string]db.GroupMemberResult, agentSessions []string) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Review complete: PR #%s (round %d)\n\n", prNumber, round))

	// Count no-start failures: agents whose sidecar wrote a startup_error event
	// (container never bound its port, or the inactivity watchdog fired with
	// zero inbound frames). These are infrastructure failures, not
	// code-quality signals, and must be treated differently from FAIL verdicts.
	var noStartSessions []string
	for _, sess := range agentSessions {
		if mr, ok := groupData[sess]; ok && mr.StartupError != "" {
			noStartSessions = append(noStartSessions, sess)
		}
	}

	// Count mid-run stalls (#2239): agents whose inactivity watchdog fired
	// after one or more inbound frames were received — the agent ran, then
	// went silent (stream starvation, provider limit, payload wedge). Like
	// no-starts these are infrastructure failures, but they are labelled
	// distinctly because the operational response differs: a never-started
	// agent suggests config/infra and is worth an immediate retry, while
	// repeated stalls under concurrent load suggest rate/subscription limits
	// where blind retries burn rounds. Only counted when the member's
	// terminal state is "error" — a stale stall_error event on a member that
	// later recovered to "finished" (#1495-style resume) must not relabel a
	// real verdict.
	var stalledSessions []string
	for _, sess := range agentSessions {
		mr, ok := groupData[sess]
		if !ok {
			continue
		}
		if mr.State == "error" && mr.StartupError == "" && mr.StallError != "" {
			stalledSessions = append(stalledSessions, sess)
		}
	}
	infraFailureCount := len(noStartSessions) + len(stalledSessions)

	// Count ran-but-no-parseable-verdict failures (#1995): agents that
	// terminated in `finished` state with no startup_error, but whose
	// LastMessage either is empty or does not contain a parseable
	// `<verdict>PASS</verdict>` / `<verdict>FAIL</verdict>` tag. These are
	// distinct from no-start failures (the agent did run — the output is
	// just unparseable) AND from FAIL verdicts (no code-quality signal was
	// produced). The worker should re-run, not escalate, not treat as FAIL.
	var noVerdictSessions []string
	for _, sess := range agentSessions {
		mr, ok := groupData[sess]
		if !ok {
			continue
		}
		if mr.StartupError != "" {
			continue // already counted as no-start
		}
		if mr.State != "finished" {
			continue // a non-finished state is surfaced via the unexpected-state branch in buildMonitorResults
		}
		if memberProducedParseableVerdict(mr) {
			continue
		}
		noVerdictSessions = append(noVerdictSessions, sess)
	}

	switch {
	case allPassed:
		sb.WriteString("**All 5 review agents passed.** You may proceed with announcing completion.\n\n")
	case len(noStartSessions) > 0 && len(noStartSessions) == len(agentSessions):
		// Every agent failed to start — pure infrastructure failure with no
		// code-quality signal at all.
		sb.WriteString("**All review agents failed to start (infrastructure failure).** ")
		sb.WriteString("This is NOT a code-quality verdict — no agents ran. ")
		sb.WriteString("Re-run `prism review` to retry; do not treat this as FAIL.\n\n")
	case len(stalledSessions) > 0 && len(stalledSessions) == len(agentSessions):
		// Every agent stalled mid-run — infrastructure failure, but distinct
		// from no-start: the agents DID run and then went silent (#2239).
		sb.WriteString("**All review agents stalled mid-run (infrastructure failure).** ")
		sb.WriteString("This is NOT a code-quality verdict — the agents ran but stopped producing frames before completing. ")
		sb.WriteString("Re-run `prism review` to retry; do not treat this as FAIL. ")
		sb.WriteString("Repeated mid-run stalls under concurrent load may indicate provider rate/subscription limits — escalate rather than burning rounds on blind re-runs if this recurs.\n\n")
	case infraFailureCount > 0 && infraFailureCount == len(agentSessions):
		// Every agent infra-failed, in a mix of the two classes (#2239).
		sb.WriteString(fmt.Sprintf("**All review agents failed before completing (infrastructure failure): %d failed to start (no frames received), %d stalled mid-run.** ",
			len(noStartSessions), len(stalledSessions)))
		sb.WriteString("This is NOT a code-quality verdict — no agent produced a verdict. ")
		sb.WriteString("Re-run `prism review` to retry; do not treat this as FAIL.\n\n")
	case infraFailureCount > 0:
		// Mixed: some agents returned FAIL/PASS verdicts; others failed to
		// start and/or stalled mid-run. Surface both signals so the
		// coordinator knows to both fix code issues AND re-run for the agents
		// that produced no verdict.
		switch {
		case len(stalledSessions) == 0:
			sb.WriteString("**One or more review agents failed AND one or more failed to start.** ")
			sb.WriteString("Fix any blocking issues from the agents that ran, then re-run `prism review` ")
			sb.WriteString("to cover the agents that failed to start (infrastructure failure). ")
			sb.WriteString("Do not treat no-start errors as code-quality verdicts.\n\n")
		case len(noStartSessions) == 0:
			sb.WriteString("**One or more review agents failed AND one or more stalled mid-run.** ")
			sb.WriteString("Fix any blocking issues from the agents that ran, then re-run `prism review` ")
			sb.WriteString("to cover the agents that stalled mid-run (infrastructure failure). ")
			sb.WriteString("Do not treat mid-run stalls as code-quality verdicts.\n\n")
		default:
			sb.WriteString("**One or more review agents failed AND one or more failed to start or stalled mid-run.** ")
			sb.WriteString("Fix any blocking issues from the agents that ran, then re-run `prism review` ")
			sb.WriteString("to cover the agents that failed to start (no frames received) or stalled mid-run (infrastructure failure). ")
			sb.WriteString("Do not treat no-start errors or mid-run stalls as code-quality verdicts.\n\n")
		}
	case len(noVerdictSessions) > 0:
		// One or more agents ran to `finished` but produced no parseable
		// `<verdict>` tag (#1995). The signal is incomplete — not a
		// code-quality FAIL — so the worker should re-run rather than
		// treat this as a normal failed review.
		sb.WriteString("**One or more review agents ran but produced no parseable verdict.** ")
		sb.WriteString("This is NOT a code-quality FAIL — the agents reached `finished` state without emitting a `<verdict>PASS</verdict>` / `<verdict>FAIL</verdict>` tag (e.g. truncated mid-analysis or ended on a tool-only turn). ")
		sb.WriteString("Re-run `prism review` to retry; this round does NOT count toward the 3-cycle limit. ")
		sb.WriteString("If any other agent surfaced blocking issues, address those before re-running.\n\n")
	default:
		sb.WriteString("**One or more review agents failed.** Fix the blocking issues and re-run `prism review`.\n\n")
	}

	sb.WriteString("### Results\n\n")
	sb.WriteString(formattedResults)

	// Note any missing/deleted sessions.
	var missing []string
	for _, sess := range agentSessions {
		if _, ok := groupData[sess]; !ok {
			missing = append(missing, sess)
		}
	}
	if len(missing) > 0 {
		sb.WriteString(fmt.Sprintf("\n**Note:** %d agent session(s) were not found in the group (possibly deleted mid-review): %s\n",
			len(missing), strings.Join(missing, ", ")))
	}

	return sb.String()
}

// deliverWithRetry attempts to deliver text to workerSession via `prism prompt`,
// retrying up to maxRetries times with exponential backoff starting from baseBackoff.
// dbPath is passed through to deliverPrompt for DB access.
// deliveryID is forwarded to the host-API /prompt handler for dedup; all retry
// attempts use the same ID so the sidecar treats them as one delivery.
func deliverWithRetry(workerSession, text string, maxRetries int, baseBackoff time.Duration, dbPath, deliveryID string) error {
	var lastErr error
	backoff := baseBackoff

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			proglog.Warnf("[prism monitor-review] delivery attempt %d/%d failed (%v) — retrying in %s\n",
				attempt, maxRetries, lastErr, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
		}

		err := deliverPrompt(workerSession, text, dbPath, deliveryID)
		if err == nil {
			return nil
		}
		lastErr = err
	}

	return fmt.Errorf("delivery failed after %d attempts: %w", maxRetries+1, lastErr)
}

// deliverPrompt sends text to workerSession via the harness-aware delivery
// helper. For pi sessions (TransportSocketPipe), it routes through the
// session's host-API Unix socket. For HTTP-harness sessions, it uses the
// harness HTTP API (prompt_async).
// deliveryID is forwarded to the host-API /prompt handler; pass
// RecoveryDeliveryID(groupID) so the sidecar's dedup set (#1685) can
// collapse a concurrent recovery-watcher delivery to exactly one prompt.
func deliverPrompt(workerSession, text, dbPath, deliveryID string) error {
	if dbPath == "" {
		dbPath = defaultDBPath()
	}
	d, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	status, err := d.CurrentStatus(workerSession)
	if err != nil {
		return fmt.Errorf("check session status for %q: %w", workerSession, err)
	}
	if status == nil {
		return fmt.Errorf("session %q not found in DB — worker may have ended", workerSession)
	}
	if status.EndedAt != nil {
		return fmt.Errorf("session %q has ended — cannot deliver review results", workerSession)
	}

	return promptdelivery.DeliverToSessionWithID(workerSession, status, text, buildPromptBodyForMonitor, "review-complete", "", deliveryID)
}

// buildPromptBodyForMonitor constructs the HTTP request body for prompt_async.
// Mirrors cmd/prompt.go buildPromptBody but operates on db.Status directly
// (monitor lives in internal/review, not cmd).
func buildPromptBodyForMonitor(text string, status *db.Status) map[string]any {
	body := map[string]any{
		"parts": []map[string]string{
			{"type": "text", "text": text},
		},
	}

	agentName := status.RootAgentName
	if agentName == nil {
		agentName = status.AgentName
	}
	modelID := status.RootModelID
	if modelID == nil {
		modelID = status.ModelID
	}

	if agentName != nil && modelID != nil {
		body["agent"] = *agentName

		// Split model_id on the first "/" to get providerID and modelID.
		slashIdx := strings.Index(*modelID, "/")
		providerID := *modelID
		modelIDStr := ""
		if slashIdx >= 0 {
			providerID = (*modelID)[:slashIdx]
			modelIDStr = (*modelID)[slashIdx+1:]
		}
		body["model"] = map[string]string{
			"providerID": providerID,
			"modelID":    modelIDStr,
		}
	}

	return body
}

// StartMonitorProcess launches the group-completion monitor as a detached
// subprocess. The monitor runs `prism monitor-review` with the provided opts
// encoded as JSON via a temp file. The subprocess is detached (Setsid) so it
// survives the parent `prism review` process exiting.
//
// The prismBinary parameter is the path to the prism binary. Pass "" to use
// os.Executable() (the current binary) as the default.
func StartMonitorProcess(opts MonitorOpts, prismBinary string) error {
	if prismBinary == "" {
		var err error
		prismBinary, err = os.Executable()
		if err != nil {
			return fmt.Errorf("monitor: resolve prism binary: %w", err)
		}
	}

	// Serialise MonitorOpts to a temp file so we can pass it via flag.
	optsJSON, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("monitor: marshal opts: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "prism-monitor-opts-*.json")
	if err != nil {
		return fmt.Errorf("monitor: create temp opts file: %w", err)
	}
	defer tmpFile.Close()
	if _, err := tmpFile.Write(optsJSON); err != nil {
		return fmt.Errorf("monitor: write opts file: %w", err)
	}

	cmd := exec.Command(prismBinary, "monitor-review", "--opts-file", tmpFile.Name())
	// Detach the process: use setsid so it survives the parent process exiting.
	detachProcess(cmd)
	// Redirect stdout/stderr to log file for diagnostics.
	logPath := fmt.Sprintf("/tmp/prism-monitor-review-%s-round-%d.log", sanitisePRNumber(opts.PRNumber), opts.Round)
	logFile, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if logErr == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return fmt.Errorf("monitor: start monitor process: %w", err)
	}
	// Do not wait — the process is detached and runs independently.
	// logFile stays open in the child; Close in parent has no effect on child fd.
	if logFile != nil {
		logFile.Close()
	}
	return nil
}

// ActiveReviewGroupForParent returns the group_id of an in-progress (not yet
// completed) review group for parentSession, or "" when none exists.
// It is used to detect duplicate `prism review` invocations for the same
// parent session.
//
// A group member counts as terminal here when EITHER:
//
//   - its state is in {finished, error, deleted} — aligned with
//     db.terminalStates so a "deleted" sibling does not block the guard; OR
//   - its ended_at is non-NULL — the row has been closed by `prism cleanup`,
//     `prism reset`, or any other lifecycle path that calls SetEnded, even
//     if the state string is still "interrupted" or "active" (#1962
//     primary fix: closes the dead-sidecar gap where cleanup runs but the
//     state flip to "deleted" never happens because the sidecar is gone).
//
// An "interrupted" row whose ended_at is still NULL is intentionally NOT
// terminal here: the sidecar is alive and the user may still redirect the
// agent via `prism prompt`, eventually reaching finished/error (#1495).
// Only once cleanup (or any other SetEnded path) closes the row does the
// ended_at arm trip and the guard release.
func ActiveReviewGroupForParent(d *db.DB, parentSession string) (string, error) {
	members, err := d.GroupMembersForParent(parentSession)
	if err != nil {
		return "", fmt.Errorf("active review group: GroupMembersForParent: %w", err)
	}
	if len(members) == 0 {
		return "", nil
	}

	// Group sessions by group_id.
	// A group is "active" when at least one member is not in a terminal state.
	groupStates := make(map[string]bool) // group_id → hasActiveMembers
	for _, m := range members {
		if m.GroupID == nil {
			continue
		}
		gid := *m.GroupID
		if !isTerminalForGuard(m) {
			groupStates[gid] = true
		} else if _, exists := groupStates[gid]; !exists {
			groupStates[gid] = false
		}
	}

	for gid, active := range groupStates {
		if active {
			return gid, nil
		}
	}
	return "", nil
}

// isTerminalForGuard reports whether a group member should be treated as
// terminal for the purpose of the `prism review` in-progress guard
// (ActiveReviewGroupForParent). See the comment on ActiveReviewGroupForParent
// for the full rationale. The two arms are intentionally OR-ed:
//
//  1. state-based: {finished, error, deleted}
//  2. ended_at-based: ended_at IS NOT NULL
//
// Arm 2 is the "ended_at wins" check from #1962: any row whose session has
// been closed by `prism cleanup` (which calls SetEnded) is terminal here
// regardless of what the state string says, because the sidecar that would
// have updated the state to "deleted" is already gone. This is what makes
// the user's escape hatch — `prism cleanup --yes --session <agent>` —
// actually release the in-progress guard, even when the cleanup happened
// against a dead-sidecar row that still reads state="interrupted".
func isTerminalForGuard(m db.Status) bool {
	if m.EndedAt != nil {
		return true
	}
	switch m.State {
	case "finished", "error", "deleted":
		return true
	}
	return false
}

// currentCycleProducedVerdicts reports whether *every* member of the current
// group produced a parseable `<verdict>` tag. Used by the monitor to decide
// whether the in-flight cycle counts toward the LOOP-LIMIT threshold.
//
// Contract (#1995): a cycle is verdict-producing iff there is at least one
// member AND every member is finished with a non-empty assistant message that
// AssessPassed classifies as either VerdictPass or VerdictFail. If any member
// is in a non-finished state, missing a LastMessage, had a startup_error, or
// terminated without emitting a parseable `<verdict>` tag, the cycle is NOT
// counted — the worker is expected to re-run.
//
// This is the same predicate `CompletedReviewCyclesForParent` applies to
// historical groups; the two share the contract.
func currentCycleProducedVerdicts(groupData map[string]db.GroupMemberResult) bool {
	if len(groupData) == 0 {
		return false
	}
	for _, mr := range groupData {
		if !memberProducedParseableVerdict(mr) {
			return false
		}
	}
	return true
}

// memberProducedParseableVerdict reports whether a single group member's
// terminal state contains a parseable `<verdict>PASS</verdict>` or
// `<verdict>FAIL</verdict>` marker. Shared by both the in-flight predicate
// (currentCycleProducedVerdicts) and the historical predicate inside
// CompletedReviewCyclesForParent so the contract is defined exactly once.
func memberProducedParseableVerdict(mr db.GroupMemberResult) bool {
	if mr.State != "finished" || mr.LastMessage == "" || mr.StartupError != "" {
		return false
	}
	text := ExtractAssistantText(mr.LastMessage)
	_, kind := AssessPassed(text)
	return kind != VerdictNone
}

// REVIEW_CYCLE_THRESHOLD is the number of completed verdict-producing review
// cycles after which the review-complete prompt gets a LOOP-LIMIT footer
// appended. The threshold itself is out of scope for #1512; this constant is
// the single source of truth for the firing condition.
const REVIEW_CYCLE_THRESHOLD = 3

// loopLimitFooterTemplate is the LOOP-LIMIT footer appended to the
// review-complete prompt body when a review group completes the Nth
// non-converged verdict-producing cycle (N ≥ REVIEW_CYCLE_THRESHOLD) for the
// same PR. The template is exported for testing.
//
// Format args (in order):
//   1. cycle count (int)
//   2. PR label (string, e.g. "PR #42")
const loopLimitFooterTemplate = "\n---\n" +
	"⚠️ **REVIEW LOOP LIMIT.** You have run %d review cycles for %s without all agents passing. " +
	"Stop and escalate to the coordinator. Do NOT run another review cycle. " +
	"Summarise: (1) what was originally requested, (2) what each cycle found, " +
	"and (3) why the fixes are not converging.\n"

// buildLoopLimitFooter renders the LOOP-LIMIT footer for the given cycle
// count and PR number.
func buildLoopLimitFooter(cycles int, prNumber string) string {
	prLabel := "this PR"
	if prNumber != "" {
		prLabel = fmt.Sprintf("PR #%s", prNumber)
	}
	return fmt.Sprintf(loopLimitFooterTemplate, cycles, prLabel)
}

// CompletedReviewCyclesForParent returns the count of completed
// verdict-producing review cycles for parentSession, optionally excluding the
// group given by excludeGroupID. A cycle is "completed and verdict-producing"
// when:
//
//   1. Every member of the group is in a terminal state (so the group is no
//      longer running), AND
//   2. Every member finished with a parseable `<verdict>PASS</verdict>` /
//      `<verdict>FAIL</verdict>` tag — i.e. the group produced a full set
//      of real per-agent verdicts (#1995). The per-member check is shared
//      with the in-flight predicate via memberProducedParseableVerdict so
//      the two callsites cannot drift.
//
// This is the single source of truth for cycle counting in the LOOP-LIMIT
// firing logic. Pure-infrastructure failures (every member never bound its
// port) AND ran-but-no-parseable-verdict rounds (any member terminated in
// `finished` state without emitting a `<verdict>` tag — the #1993 shape)
// are both excluded by condition 2 — mirroring the documented contract in
// `modules/programs/prism/skills/prism/SKILL.md`:
//
//   "Count re-run cycles from the first round that had a full set of agent
//    results; do not count infrastructure-failure rounds toward your 3-cycle
//    limit."
//
// Pass excludeGroupID="" to count every group; pass the current group's id
// when computing "cycles before this one" so that a caller can ask
// "is the current cycle the 3rd?".
func CompletedReviewCyclesForParent(d *db.DB, parentSession, excludeGroupID string) (int, error) {
	members, err := d.GroupMembersForParent(parentSession)
	if err != nil {
		return 0, fmt.Errorf("completed review cycles: GroupMembersForParent: %w", err)
	}
	if len(members) == 0 {
		return 0, nil
	}

	// Bucket members by group_id.
	byGroup := make(map[string][]db.Status)
	for _, m := range members {
		if m.GroupID == nil {
			continue
		}
		gid := *m.GroupID
		if gid == excludeGroupID {
			continue
		}
		byGroup[gid] = append(byGroup[gid], m)
	}

	count := 0
	for gid, gMembers := range byGroup {
		// Condition 1: every member must be terminal.
		allTerminal := true
		for _, m := range gMembers {
			if !isTerminalState(m.State) && m.EndedAt == nil {
				allTerminal = false
				break
			}
		}
		if !allTerminal {
			continue
		}

		// Condition 2: every member produced a parseable `<verdict>` tag.
		// We use GroupResults to read each member's last assistant message,
		// startup_error, and state, and require AssessPassed to return
		// VerdictPass or VerdictFail for every member (#1995). A group where
		// any member terminated without a parseable verdict — empty
		// LastMessage, startup_error, or mid-analysis truncation — is NOT
		// counted: the worker is expected to re-run.
		//
		// Rationale: the lenient predicate ("at least one finished member
		// with non-empty LastMessage") tripped the LOOP-LIMIT at cycle 3 in
		// PR #1992 even though only 3 of 5 agents had actually emitted
		// verdicts; the other two finished with no parseable `<verdict>` tag.
		// See `docs/diagnoses/review-agent-no-verdict-1993.md` for the full
		// trace; #1995 tightened both predicates to share this contract.
		groupData, gErr := d.GroupResults(gid)
		if gErr != nil {
			// Be defensive: a bad row should not silently underreport cycle
			// count. Log and skip the group so we do not fire LOOP-LIMIT
			// based on partial data, but do not return the error — callers
			// (the monitor) treat a missing footer as a benign degradation.
			proglog.Warnf("[prism review] warning: GroupResults(%s) failed during cycle counting: %v\n", gid, gErr)
			continue
		}
		if len(groupData) == 0 {
			continue
		}
		producedVerdict := true
		for _, mr := range groupData {
			if !memberProducedParseableVerdict(mr) {
				producedVerdict = false
				break
			}
		}
		if producedVerdict {
			count++
		}
	}
	return count, nil
}

// ReviewRoundForGroup returns the round number for the given group_id by
// inspecting the session names of its members.
// Returns 0 when the round cannot be determined.
func ReviewRoundForGroup(members []db.Status) int {
	// Member session names have shape <parent>~review-<N>-<agent>.
	// Extract the first N we find.
	for _, m := range members {
		name := m.SessionName
		tilde := strings.Index(name, "~review-")
		if tilde < 0 {
			continue
		}
		suffix := name[tilde+len("~review-"):]
		dash := strings.Index(suffix, "-")
		if dash <= 0 {
			continue
		}
		n := 0
		for _, c := range suffix[:dash] {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int(c-'0')
		}
		if n > 0 {
			return n
		}
	}
	return 0
}

// WriteFallbackResult writes the review result to the standard fallback file
// when delivery fails. prNumber is sanitised to prevent path traversal.
// Exported for testing.
func WriteFallbackResult(prNumber string, round int, content string) (string, error) {
	path := fmt.Sprintf("/tmp/prism-review-%s-round-%d-result.md", sanitisePRNumber(prNumber), round)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write fallback result: %w", err)
	}
	return path, nil
}

// MonitorOptsFilePath is the temp-file path used to pass MonitorOpts to the
// monitor subprocess. It is read by the monitor-review subcommand.
// Exported for testability.
func LoadMonitorOptsFromFile(path string) (MonitorOpts, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return MonitorOpts{}, fmt.Errorf("read opts file %q: %w", path, err)
	}
	var opts MonitorOpts
	if err := json.Unmarshal(data, &opts); err != nil {
		return MonitorOpts{}, fmt.Errorf("parse opts file %q: %w", path, err)
	}
	return opts, nil
}

// fallbackFilePath returns the standard fallback result file path.
// prNumber is sanitised to prevent path traversal.
func fallbackFilePath(prNumber string, round int) string {
	return filepath.Join("/tmp", fmt.Sprintf("prism-review-%s-round-%d-result.md", sanitisePRNumber(prNumber), round))
}

// BuildMonitorResultsForTest is an exported wrapper around buildMonitorResults
// for use in external test packages.
func BuildMonitorResultsForTest(agents []Agent, agentSessions []string, groupData map[string]db.GroupMemberResult) []AgentResult {
	return buildMonitorResults(agents, agentSessions, groupData)
}
