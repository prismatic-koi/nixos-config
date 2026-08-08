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

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
)

// Event types written by persistReviewOutcome (#2703). Each round that
// reaches a real pass/fail verdict writes exactly one of these as a durable
// agent_events row, so the exporter's tail cursor can count
// prism_review_verdicts_total{verdict} without ever reading the free-form
// review report text. The verdict lives in the TYPE, not the payload — the
// exporter must never read agent_events.payload (#2699 section 5), and
// folding the verdict into the type is the same trick eventtypes.go already
// uses for the closed label set.
const (
	// EventReviewVerdictPass is written when every review agent in the round
	// passed.
	EventReviewVerdictPass = "review.verdict_pass"
	// EventReviewVerdictFail is written when at least one review agent in
	// the round did not pass.
	EventReviewVerdictFail = "review.verdict_fail"
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
	// ReapAfterDelivery makes MonitorFunc wait out the reap grace period after
	// it delivers the review-complete prompt, then release this round's agent
	// sessions (issue #2649). RunAsync sets it for every production round.
	//
	// It is opt-in rather than always-on so MonitorFunc stays a fast, pure
	// function for the tests that drive it directly: a test that does not ask
	// for the reap does not pay the wait and sees no teardown.
	ReapAfterDelivery bool
	// ReapGrace overrides ReapGracePeriod for this round. Zero uses the
	// default. Read only when ReapAfterDelivery is true.
	ReapGrace time.Duration
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

	// Read the rows db.GroupResults drops — members whose ended_at is set
	// (#2573). Without them a reaped session is indistinguishable from a
	// session that never existed, and the report cannot say why the agent
	// produced no verdict.
	endedRows := endedGroupMembers(d, opts.GroupID)

	// Read the close cause each lifecycle path recorded for those rows
	// (#2613). Without it the report can only see the state string, which is
	// why it used to name a force-terminate and a readiness-gate failure as
	// two possibilities for the same row.
	endedCauses := endedMemberCauses(d, endedRows)

	// Build AgentResult slice, handling "missing" sessions (row reaped or
	// deleted mid-review).
	results := buildMonitorResults(opts.Agents, opts.AgentSessions, groupData, endedRows, endedCauses)

	// Classify the round once. RoundStatus is the single source of truth for
	// both the report wording and the cycle-counter gate below (#2573).
	status := ClassifyRoundWithCauses(opts.Agents, opts.AgentSessions, groupData, endedRows, endedCauses)

	// Format the delivery message. FormatResultsForRound keeps the summary
	// footer's retry hint consistent with the re-run advice below (#2573).
	output, allPassed := FormatResultsForRound(results, opts.PRNumber, opts.Round, 0, status)
	deliveryText := buildDeliveryMessage(opts.PRNumber, opts.Round, output, allPassed, status)

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
	//       EXPECTED member emitted a parseable `<verdict>` tag (#1995,
	//       #2573). When the cycle is NOT verdict-producing, the relevant
	//       branch in buildDeliveryMessage ("infrastructure failure",
	//       "Round incomplete", or "ran but produced no parseable verdict")
	//       already tells the worker to re-run — it is not yet at the
	//       limit, AND
	//   (c) the total count of verdict-producing cycles for this parent
	//       (including this one) is ≥ REVIEW_CYCLE_THRESHOLD.
	//
	// The footer is naturally rate-limited — it appears once per actual
	// completed verdict-producing cycle, embedded in the prompt the worker is
	// already going to act on. This dissolves the per-turn-spam and the
	// bash-substring false-match defects at the source (#1512).
	if !allPassed {
		if status.CountsAsCycle() {
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

	// Write the authoritative end-of-life signal for this review group
	// (#2259). Once delivered_at is set, GroupCompleted short-circuits to
	// true and ActiveReviewGroupForParent skips this group, so any
	// subsequent mutation of agent_status (e.g. the per-process sidecar-
	// restart anti-pattern in cmd/sidecar.go) cannot flip the parent
	// worker back into "round N already in progress" refusals.
	//
	// Failure is non-fatal: the prompt has already been accepted by
	// `prism prompt` at this point and the recovery watcher's grace timer
	// will not re-fire because the worker sidecar's /prompt handler
	// clears reviewingInFlight. A missing delivered_at write leaves the
	// system in the pre-fix state (vulnerable to agent_status clobbers)
	// but does not lose the verdict.
	if setErr := d.SetGroupDeliveredAt(opts.GroupID); setErr != nil {
		proglog.Warnf("[prism monitor-review] warning: SetGroupDeliveredAt(%s): %v\n", opts.GroupID, setErr)
	} else {
		proglog.Infof("[prism monitor-review] group %s delivered_at recorded\n", opts.GroupID)
	}

	proglog.Infof("[prism monitor-review] results delivered to %s\n", opts.WorkerSession)

	// Release this round's agent sessions once the grace period elapses
	// (#2649). This runs LAST, and only after delivered_at is written: the
	// reap predicate reads that column, so a failed SetGroupDeliveredAt above
	// leaves the round un-reapable rather than reaping it early. The wait
	// blocks this process, which has no work left to do — see
	// ReapGroupAfterGrace for why the monitor hosts the wait.
	if opts.ReapAfterDelivery {
		ReapGroupAfterGrace(d, opts.GroupID, opts.ReapGrace)
	}
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

	// Durable event for the exporter's tail cursor (#2703). Best-effort:
	// telemetry must never break the review-outcome path it rides alongside.
	instanceID := sess.InstanceID
	eventType := EventReviewVerdictFail
	if allPassed {
		eventType = EventReviewVerdictPass
	}
	if err := d.WriteEvent(db.Event{
		ID:          uuid.New().String(),
		SessionName: workerSession,
		Repo:        sess.Repo,
		Worktree:    sess.Worktree,
		InstanceID:  &instanceID,
		Type:        eventType,
		Payload:     "{}",
		CreatedAt:   time.Now(),
	}); err != nil {
		proglog.Warnf("[prism monitor-review] warning: write %s event (iid=%s): %v\n", eventType, sess.InstanceID, err)
	}
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
		// Record WHY this row is about to close, before it closes (#2613).
		// The monitor is the only path that force-terminates a member of a
		// live round, and without this record the resulting row is
		// indistinguishable in the report from a readiness-gate cleanup.
		if err := d.RecordSessionReap(sess, db.ReapCauseMonitorTimeout,
			fmt.Sprintf("still in state %q when the monitor safety deadline fired (per-agent timeout %v)", status.State, perAgentTimeout)); err != nil {
			proglog.Warnf("[prism monitor-review] warning: RecordSessionReap(%s): %v\n", sess, err)
		}
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
// handling missing sessions (row reaped or deleted mid-review) as a special
// case. Missing agents are counted as an error result.
//
// endedRows carries the group's agent_status rows whose ended_at is set —
// exactly the rows db.GroupResults drops. It lets the missing-session branch
// state WHY the member vanished (#2573). endedCauses carries the close cause
// each lifecycle path recorded for those rows (#2613). Pass nil for either
// when no DB handle is available; the branch then reports the absence without
// a cause.
func buildMonitorResults(agents []Agent, agentSessions []string, groupData map[string]db.GroupMemberResult, endedRows map[string]db.Status, endedCauses map[string]db.SessionEndCause) []AgentResult {
	results := make([]AgentResult, len(agents))
	for i, ag := range agents {
		agentSession := ""
		if i < len(agentSessions) {
			agentSession = agentSessions[i]
		}

		mr, ok := groupData[agentSession]
		if !ok || agentSession == "" {
			// Session was reaped mid-review (ended_at set), deleted, or never
			// registered — count as missing and name the recorded cause.
			class, reason := classifyAbsentMember(agentSession, endedRows, endedCauses)
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: agent produced no verdict — %s: %s", class, reason),
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
//
// status is the round classification produced by ClassifyRound — the single
// source of truth for which agents produced a verdict and why the others did
// not (#2573). The header branch and the cycle-counter gate in MonitorFunc
// read the same classification, so the report and the counter cannot drift.
func buildDeliveryMessage(prNumber string, round int, formattedResults string, allPassed bool, status RoundStatus) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Review complete: PR #%s (round %d)\n\n", prNumber, round))

	// No-start failures (#1222): the sidecar wrote a startup_error event
	// (container never bound its port, or the inactivity watchdog fired with
	// zero inbound frames). Mid-run stalls (#2239): the watchdog fired after
	// one or more inbound frames — the agent ran, then went silent. Both are
	// infrastructure failures, but they keep separate headers because the
	// operational response differs: a never-started agent suggests config or
	// infra and is worth an immediate retry, while repeated stalls under
	// concurrent load suggest rate/subscription limits where blind retries
	// burn rounds.
	noStart := status.MissingOfClass(NoVerdictNoStart)
	stalled := status.MissingOfClass(NoVerdictStalled)

	switch {
	case allPassed && status.Complete():
		sb.WriteString("**All 5 review agents passed.** You may proceed with announcing completion.\n\n")
	case status.Complete():
		// Every agent produced a verdict and at least one is FAIL — an
		// ordinary failed review round. This is the only non-passing branch
		// that consumes a review cycle.
		sb.WriteString("**One or more review agents failed.** Fix the blocking issues and re-run `prism review`.\n\n")
	case len(noStart) > 0 && len(noStart) == status.Expected:
		// Every agent failed to start — pure infrastructure failure with no
		// code-quality signal at all.
		sb.WriteString("**All review agents failed to start (infrastructure failure).** ")
		sb.WriteString("This is NOT a code-quality verdict — no agents ran. ")
		sb.WriteString("Re-run `prism review` to retry; do not treat this as FAIL.\n\n")
	case len(stalled) > 0 && len(stalled) == status.Expected:
		// Every agent stalled mid-run — infrastructure failure, but distinct
		// from no-start: the agents DID run and then went silent (#2239).
		sb.WriteString("**All review agents stalled mid-run (infrastructure failure).** ")
		sb.WriteString("This is NOT a code-quality verdict — the agents ran but stopped producing frames before completing. ")
		sb.WriteString("Re-run `prism review` to retry; do not treat this as FAIL. ")
		sb.WriteString("Repeated mid-run stalls under concurrent load may indicate provider rate/subscription limits — escalate rather than burning rounds on blind re-runs if this recurs.\n\n")
	case !status.HasInfrastructureFailure():
		// One or more agents ran to `finished` but produced no parseable
		// `<verdict>` tag (#1995). The signal is incomplete — not a
		// code-quality FAIL — so the worker should re-run rather than
		// treat this as a normal failed review.
		sb.WriteString("**One or more review agents ran but produced no parseable verdict.** ")
		sb.WriteString("This is NOT a code-quality FAIL — the agents reached `finished` state without emitting a `<verdict>PASS</verdict>` / `<verdict>FAIL</verdict>` tag (e.g. truncated mid-analysis or ended on a tool-only turn). ")
		sb.WriteString("Re-run `prism review` to retry; this round does NOT count toward the 3-cycle limit. ")
		sb.WriteString("If any other agent surfaced blocking issues, address those before re-running.\n\n")
	default:
		// The round is incomplete and at least one absence is an
		// infrastructure fault: a no-start, a mid-run stall, a mid-run crash,
		// a session reaped mid-round, or a member still non-terminal at the
		// monitor's safety timeout (#2573). Report the shortfall first — the
		// verdicts that did arrive are NOT the result of the round.
		sb.WriteString(fmt.Sprintf("**Round incomplete: %d of %d review agents produced a verdict.** ",
			status.Verdicts, status.Expected))
		sb.WriteString(fmt.Sprintf("Agents with no verdict: %s — infrastructure failure. ",
			status.ClassSummary()))
		switch {
		case status.HasFailVerdict():
			// A FAIL means the worker must change code, and that change makes
			// every verdict in this round stale — so the whole set has to run
			// again. See the targeted-rerun condition (#2530 / #2557).
			sb.WriteString("The missing dimensions were never examined; a missing verdict is NOT a pass and is NOT a code-quality verdict. ")
			sb.WriteString(fmt.Sprintf("Fix the blocking issues from the agents that ran, then re-run the FULL set (`%s`) — your fix invalidates the verdicts this round produced. ",
				status.FullRerunCommand(prNumber)))
		case status.Verdicts > 0:
			sb.WriteString("The agents that ran all passed, but the round is not a pass: the missing dimensions were never examined. ")
			sb.WriteString("Re-run the agents named below. ")
		default:
			sb.WriteString("This is NOT a code-quality verdict — no agent produced a verdict. ")
			sb.WriteString("Re-run `prism review` to retry; do not treat this as FAIL. ")
		}
		sb.WriteString("This round does NOT count toward the 3-cycle limit.\n\n")
	}

	sb.WriteString("### Results\n\n")
	sb.WriteString(formattedResults)

	// Name every agent that produced no verdict, with the reason recorded for
	// it, and the targeted re-run command (#2573).
	sb.WriteString(buildNoVerdictSection(status, prNumber))

	return sb.String()
}

// buildNoVerdictSection renders the per-agent "no verdict" roll-call appended
// to the delivery message (#2573). It returns "" for a complete round, so a
// round in which all agents produced verdicts is reported exactly as before.
func buildNoVerdictSection(status RoundStatus, prNumber string) string {
	if len(status.Missing) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\n### Agents with no verdict (%d of %d)\n\n",
		len(status.Missing), status.Expected))
	sb.WriteString("These review dimensions were NOT examined this round. Read a missing verdict as unreviewed, never as a pass.\n\n")
	for _, m := range status.Missing {
		name := m.Agent
		if name == "" {
			name = "(unnamed agent)"
		}
		if m.Session != "" {
			sb.WriteString(fmt.Sprintf("- **%s** (`%s`) — %s: %s\n", name, m.Session, m.Class, m.Reason))
		} else {
			sb.WriteString(fmt.Sprintf("- **%s** — %s: %s\n", name, m.Class, m.Reason))
		}
	}
	sb.WriteString(buildRerunAdvice(status, prNumber))
	sb.WriteString("\nThis round does NOT count toward the 3-cycle limit.\n")
	return sb.String()
}

// buildRerunAdvice renders the re-run instruction for an incomplete round.
//
// The targeted-rerun condition (#2530, widened by #2557) is prose-only: a
// worker may re-run a subset of the agents only when the inter-cycle diff is
// exactly formatter output, comments, or documentation. The report cannot see
// that diff, so it must not print a bare `--only` command as if the condition
// always held — a worker that fixed a FAIL and then re-ran one agent would
// carry four verdicts produced against the pre-fix commit.
//
// Two cases:
//
//   - An agent that ran returned FAIL — a code change is coming, so the
//     targeted command is invalid. Print the full-set command only.
//   - No agent returned FAIL — print the targeted command, with the
//     push-nothing-else caveat and the full-set fallback.
func buildRerunAdvice(status RoundStatus, prNumber string) string {
	targeted := status.TargetedRerunCommand(prNumber)
	if targeted == "" {
		return ""
	}
	full := status.FullRerunCommand(prNumber)

	var sb strings.Builder
	if !status.TargetedRerunAllowed() {
		sb.WriteString("\nAn agent that ran returned FAIL, so a targeted `--only` re-run is NOT valid here: ")
		sb.WriteString("the verdicts above were produced against the pre-fix commit. ")
		sb.WriteString("Fix the blocking issues, push, then re-run the full set:\n\n")
		sb.WriteString("    " + full + "\n")
		return sb.String()
	}

	sb.WriteString("\nRe-run only the agents above, provided you push nothing else first:\n\n")
	sb.WriteString("    " + targeted + "\n")
	sb.WriteString("\nIf you push any change other than formatter output, comments, or documentation ")
	sb.WriteString("before re-running, the verdicts above are stale — re-run the full set instead:\n\n")
	sb.WriteString("    " + full + "\n")
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
	// Skip groups whose review-complete prompt has already been delivered
	// (#2259): once session_groups.delivered_at is set, the group is
	// authoritatively complete regardless of any subsequent member-row
	// mutation. This closes the wedge-at-idle failure class where a per-
	// process sidecar restart (cmd/sidecar.go) re-seeded an agent_status
	// row back to a non-terminal state after delivery, leaving the parent
	// worker permanently refused with "round N already in progress".
	delivered, err := d.DeliveredGroupIDsForParent(parentSession)
	if err != nil {
		return "", fmt.Errorf("active review group: DeliveredGroupIDsForParent: %w", err)
	}

	members, err := d.GroupMembersForParent(parentSession)
	if err != nil {
		return "", fmt.Errorf("active review group: GroupMembersForParent: %w", err)
	}
	if len(members) == 0 {
		return "", nil
	}

	// Group sessions by group_id.
	// A group is "active" when at least one member is not in a terminal state
	// AND its session_groups.delivered_at is NULL.
	groupStates := make(map[string]bool) // group_id → hasActiveMembers
	for _, m := range members {
		if m.GroupID == nil {
			continue
		}
		gid := *m.GroupID
		if _, isDelivered := delivered[gid]; isDelivered {
			// Already delivered — not active. Do not enter into groupStates
			// at all so the final scan cannot accidentally classify it as
			// active on a subsequent member iteration.
			continue
		}
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

// cycleProducedVerdicts reports whether every EXPECTED member of a group
// produced a parseable `<verdict>` tag — the predicate that decides whether a
// round counts toward the LOOP-LIMIT threshold.
//
// Contract (#1995, extended by #2573): a cycle is verdict-producing iff there
// is at least one expected member AND every expected member is finished with a
// non-empty assistant message that AssessPassed classifies as VerdictPass or
// VerdictFail. A member that is in a non-finished state, has no LastMessage,
// had a startup_error or stall_error, or is ABSENT from groupData altogether
// (its row was reaped or deleted) makes the cycle non-counting — the worker is
// expected to re-run.
//
// expectedSessions is the authoritative member list. Reading only the
// groupData keys was the #2573 defect: db.GroupResults omits reaped rows, so a
// four-verdict round looked like a full set and consumed a cycle.
//
// Both the in-flight gate (MonitorFunc, via RoundStatus.CountsAsCycle) and the
// historical count (CompletedReviewCyclesForParent) resolve to ClassifyRound,
// so the contract is defined exactly once.
func cycleProducedVerdicts(expectedSessions []string, groupData map[string]db.GroupMemberResult, endedRows map[string]db.Status) bool {
	agents := make([]Agent, 0, len(expectedSessions))
	for _, sess := range expectedSessions {
		agents = append(agents, Agent{Name: agentNameFromSession(sess)})
	}
	return ClassifyRound(agents, expectedSessions, groupData, endedRows).CountsAsCycle()
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
//  1. cycle count (int)
//  2. PR label (string, e.g. "PR #42")
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
//  1. Every member of the group is in a terminal state (so the group is no
//     longer running), AND
//  2. Every member finished with a parseable `<verdict>PASS</verdict>` /
//     `<verdict>FAIL</verdict>` tag — i.e. the group produced a full set
//     of real per-agent verdicts (#1995). The per-member check is shared
//     with the in-flight predicate via ClassifyRound so the two callsites
//     cannot drift.
//
// This is the single source of truth for cycle counting in the LOOP-LIMIT
// firing logic. Condition 2 excludes pure-infrastructure failures (every
// member never bound its port), ran-but-no-parseable-verdict rounds (any
// member terminated in `finished` state without emitting a `<verdict>` tag —
// the #1993 shape), and rounds where a member's row was reaped mid-review (the
// #2573 shape) — mirroring the documented contract in
// `modules/programs/prism/skills/prism/SKILL.md`:
//
//	"Count re-run cycles from the first round that had a full set of agent
//	 results; do not count infrastructure-failure rounds toward your 3-cycle
//	 limit."
//
// What enforces those exclusions changed in #2649. This function reads
// db.GroupResultsAll, so a closed row is no longer dropped from groupData and
// no longer excluded by its ABSENCE. Each of the three is now excluded by the
// per-member predicate itself, on the facts the row carries: a non-terminal
// state, an empty LastMessage, a recorded startup_error or stall_error, or a
// terminal row whose message holds no parseable `<verdict>` tag. A member
// reaped mid-review still fails that predicate — it was reaped precisely
// because it had not produced a verdict — so the #2573 outcome is unchanged.
//
// One outcome DID change, deliberately: the #2594 sub-case. A member that
// closed AFTER reaching `finished` with a parseable verdict used to drop out
// of the narrow read and make its round non-counting. It is now present, and
// its round counts. That is the correct answer — the round did produce a full
// set of verdicts — and it is load-bearing after #2649, because the automatic
// release closes every member of every delivered round. Without the wide read
// this count returned zero for every past round and the LOOP-LIMIT footer
// never fired.
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
		// We read each member's last assistant message, startup_error, and
		// state, and require AssessPassed to return VerdictPass or VerdictFail
		// for every member (#1995). A group where any member terminated
		// without a parseable verdict — empty LastMessage, startup_error, or
		// mid-analysis truncation — is NOT counted: the worker is expected to
		// re-run.
		//
		// The read is GroupResultsAll, not GroupResults (#2649, #2584).
		// The `ended_at IS NULL` filter on GroupResults is correct on the live
		// aggregation path — it is what makes the #1495 cleanup escape hatch
		// work — and wrong here. Review agents are now released automatically
		// 15 minutes after their round is delivered, so by the time a later
		// round asks "how many cycles came before me", every earlier round's
		// rows are closed. Through the narrow read this loop counted zero every
		// time, and the LOOP-LIMIT footer — the thing that tells a worker to
		// stop and escalate at three cycles — never fired.
		//
		// Widening the read does not loosen the predicate. A row closed while
		// still non-terminal, or closed before it emitted a verdict, is now
		// visible with its real state and its empty LastMessage, and
		// ClassifyRound rejects it for that reason instead of for its absence.
		// The #1495 abandoned-agent row reads as state "interrupted", which is
		// not verdict-producing, so that round still does not count. The one
		// behaviour that changes is the #2594 sub-case — a row that closed
		// AFTER delivering a full verdict now counts, which is what it always
		// should have done.
		//
		// The expected member list comes from gMembers (agent_status rows,
		// including closed ones), NOT from the groupData keys. That is the
		// #2573 fix: counting the keys that came back cannot detect a member
		// that never came back.
		//
		// Rationale: the lenient predicate ("at least one finished member
		// with non-empty LastMessage") tripped the LOOP-LIMIT at cycle 3 in
		// PR #1992 even though only 3 of 5 agents had actually emitted
		// verdicts; the other two finished with no parseable `<verdict>` tag.
		// See `docs/diagnoses/review-agent-no-verdict-1993.md` for the full
		// trace; #1995 tightened both predicates to share this contract.
		// The HISTORICAL read (#2649). GroupResults drops rows whose ended_at
		// is set, which is correct on the live aggregation path and wrong
		// here: review agents are released automatically 15 minutes after
		// their round is delivered, so by the time a later round asks "how
		// many cycles came before me", every earlier round's rows are closed.
		// Read through GroupResults, this loop counted zero every time and the
		// LOOP-LIMIT footer never fired. The release deletes no row and no
		// event, so the wider read still sees the full history.
		groupData, gErr := d.GroupResultsAll(gid)
		if gErr != nil {
			// Be defensive: a bad row should not silently underreport cycle
			// count. Log and skip the group so we do not fire LOOP-LIMIT
			// based on partial data, but do not return the error — callers
			// (the monitor) treat a missing footer as a benign degradation.
			proglog.Warnf("[prism review] warning: GroupResultsAll(%s) failed during cycle counting: %v\n", gid, gErr)
			continue
		}
		if len(groupData) == 0 {
			continue
		}
		expected := make([]string, 0, len(gMembers))
		for _, m := range gMembers {
			expected = append(expected, m.SessionName)
		}
		if cycleProducedVerdicts(expected, groupData, endedRowsFrom(gMembers)) {
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
// for use in external test packages. It passes no ended-row detail, which is
// the degraded shape a caller without a DB handle sees (#2573).
func BuildMonitorResultsForTest(agents []Agent, agentSessions []string, groupData map[string]db.GroupMemberResult) []AgentResult {
	return buildMonitorResults(agents, agentSessions, groupData, nil, nil)
}

// BuildMonitorResultsWithEndedForTest is the #2573 variant: it also supplies
// the group's closed (ended_at set) agent_status rows so tests can assert the
// reaped-session reason text. It records no close cause, which is the shape a
// pre-#2613 row has.
func BuildMonitorResultsWithEndedForTest(agents []Agent, agentSessions []string, groupData map[string]db.GroupMemberResult, endedRows map[string]db.Status) []AgentResult {
	return buildMonitorResults(agents, agentSessions, groupData, endedRows, nil)
}

// BuildMonitorResultsWithCausesForTest is the #2613 variant: it also supplies
// the close cause recorded for each closed row, so tests can assert that the
// report names one cause rather than a disjunction.
func BuildMonitorResultsWithCausesForTest(agents []Agent, agentSessions []string, groupData map[string]db.GroupMemberResult, endedRows map[string]db.Status, endedCauses map[string]db.SessionEndCause) []AgentResult {
	return buildMonitorResults(agents, agentSessions, groupData, endedRows, endedCauses)
}

// EndedMemberCausesForTest is an exported wrapper around endedMemberCauses so
// tests can exercise the DB read that feeds the classifier (#2613).
func EndedMemberCausesForTest(d *db.DB, endedRows map[string]db.Status) map[string]db.SessionEndCause {
	return endedMemberCauses(d, endedRows)
}
