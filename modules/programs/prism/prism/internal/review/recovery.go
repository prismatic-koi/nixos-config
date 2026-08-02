package review

// recovery.go — review-completion recovery path for the worker-sidecar
// watchdog (#1709 reopen).
//
// Background. The `prism review` happy path spawns a detached `prism
// monitor-review` subprocess that polls db.GroupCompleted, aggregates
// db.GroupResults, and delivers the review-complete prompt to the worker
// session via `prism prompt`. The subprocess is the sole owner of the
// all-terminal → deliver transition; there is no other watcher anywhere in
// the system today.
//
// If that subprocess dies between spawn and delivery (OOM, kernel kill,
// PRISM upgrade mid-review, host reboot, or a silent failure in
// StartMonitorProcess), the agent rows reach `finished` in the DB but no
// process is watching that signal — the worker stays in `reviewing`
// forever and the only recovery is a manual `prism cleanup`.
//
// DeliverGroupResults provides the in-process delivery primitive used by
// the worker-sidecar recovery watcher (internal/sidecar/review_recovery.go).
// It mirrors the back half of MonitorFunc — aggregation, formatting,
// pre-delivery DB state-flip, and delivery — but takes its inputs from the
// DB rather than a MonitorOpts file, so it can run after the original
// monitor subprocess is long dead. The delivery_id is supplied by the
// caller so the host-API /prompt handler's dedup set (#1685) drops the
// second delivery if both the original monitor and the recovery watcher
// happen to deliver at the same instant.

import (
	"fmt"
	"strings"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
)

// RecoveryDeliveryResult captures what DeliverGroupResults did, for logging
// and test assertions. AllPassed mirrors the FormatResults return value.
type RecoveryDeliveryResult struct {
	AllPassed   bool
	Delivered   bool
	Replayed    bool
	FallbackTo  string // non-empty when delivery failed and a fallback file was written
	MessageText string // the prompt body that was (or would have been) delivered
}

// DeliverGroupResults aggregates the group's results, formats the
// review-complete prompt, flips the worker session's state from `reviewing`
// to `active` if needed, and delivers the prompt via the host-API socket
// with delivery_id-based idempotency.
//
// It is the recovery analogue of the back half of MonitorFunc. Unlike
// MonitorFunc it does NOT poll — the caller (the worker-sidecar recovery
// watcher) has already established that GroupCompleted returns true.
//
// PRNumber and round are read from session_groups (via db.GetGroup) so the
// caller does not need to thread them through. When either is missing
// (legacy groups registered before migration v30→v31), the message header
// degrades to "Review complete: PR # (round 0)" — actionable but obviously
// reconstructed.
//
// The deliveryID is forwarded to the host-API /prompt handler. Pass a
// stable UUID for the group so that repeated invocations of this function
// (e.g. the watcher firing twice across a daemon restart) dedup at the
// receiving sidecar (#1685). The recommended construction is
// "review-recovery:" + groupID — derived deterministically from the group
// so any number of recovery attempts collapse to one delivered prompt.
func DeliverGroupResults(d *db.DB, groupID, deliveryID string) (*RecoveryDeliveryResult, error) {
	if d == nil {
		return nil, fmt.Errorf("review recovery: db is required")
	}
	if groupID == "" {
		return nil, fmt.Errorf("review recovery: groupID is required")
	}

	info, err := d.GetGroup(groupID)
	if err != nil {
		return nil, fmt.Errorf("review recovery: GetGroup(%s): %w", groupID, err)
	}
	if info == nil {
		return nil, fmt.Errorf("review recovery: group %s not found", groupID)
	}
	if info.ParentSession == "" {
		return nil, fmt.Errorf("review recovery: group %s has no parent_session", groupID)
	}

	members, err := d.GroupMembersForGroup(groupID)
	if err != nil {
		return nil, fmt.Errorf("review recovery: GroupMembersForGroup(%s): %w", groupID, err)
	}

	// Reconstruct the (Agents, AgentSessions) pair from the member rows.
	// The agent name is the suffix after the last "-" in the session name
	// (member sessions are <parent>~review-<N>-<agent>); fall back to the
	// session name itself when the suffix cannot be parsed.
	agents := make([]Agent, 0, len(members))
	agentSessions := make([]string, 0, len(members))
	for _, m := range members {
		agentSessions = append(agentSessions, m.SessionName)
		agents = append(agents, Agent{Name: agentNameFromSession(m.SessionName)})
	}

	groupData, err := d.GroupResults(groupID)
	if err != nil {
		// Non-fatal: emit a degraded delivery rather than refusing to deliver.
		// MonitorFunc takes the same stance on this branch.
		proglog.Warnf("[prism review recovery] warning: GroupResults(%s): %v — using empty data\n", groupID, err)
		groupData = map[string]db.GroupMemberResult{}
	}

	// members comes from GroupMembersForGroup, which — unlike GroupResults —
	// includes rows whose ended_at is set. endedRows carries exactly those
	// rows so a member reaped mid-review is reported with its recorded cause
	// rather than as an unexplained absence (#2573).
	endedRows := endedRowsFrom(members)

	results := buildMonitorResults(agents, agentSessions, groupData, endedRows)
	status := ClassifyRound(agents, agentSessions, groupData, endedRows)
	output, allPassed := FormatResults(results, info.PRNumber, info.Round, 0)
	deliveryText := buildDeliveryMessage(info.PRNumber, info.Round, output, allPassed, status)

	// LOOP-LIMIT footer (#1512) — mirror the MonitorFunc gating exactly so a
	// recovery-path delivery is indistinguishable from a happy-path delivery.
	if !allPassed && status.CountsAsCycle() {
		prior, ccErr := CompletedReviewCyclesForParent(d, info.ParentSession, groupID)
		if ccErr != nil {
			proglog.Warnf("[prism review recovery] warning: cycle count failed: %v — footer suppressed\n", ccErr)
		} else if prior+1 >= REVIEW_CYCLE_THRESHOLD {
			deliveryText += buildLoopLimitFooter(prior+1, info.PRNumber)
		}
	}

	// Flip the worker from `reviewing` to `active` before delivery so the
	// busy event triggered by the prompt arriving lifts the suppression
	// guard cleanly (see the analogous block in MonitorFunc).
	if workerStatus, stErr := d.CurrentStatus(info.ParentSession); stErr == nil && workerStatus != nil {
		if workerStatus.State == string(agent.StateReviewing) {
			if upErr := d.UpsertStatus(info.ParentSession, workerStatus.Repo, workerStatus.Worktree,
				string(agent.StateActive), nil, nil); upErr != nil {
				proglog.Warnf("[prism review recovery] warning: could not clear reviewing\u2192active before delivery: %v\n", upErr)
			} else {
				proglog.Errorf("[prism review recovery] worker state reviewing\u2192active (pre-delivery, group=%s)\n", groupID)
			}
		}
	} else if stErr != nil {
		proglog.Warnf("[prism review recovery] warning: CurrentStatus(%s): %v\n", info.ParentSession, stErr)
	}

	res := &RecoveryDeliveryResult{
		AllPassed:   allPassed,
		MessageText: deliveryText,
	}

	// Look up the worker status fresh for promptdelivery.DeliverToSession.
	workerStatus, err := d.CurrentStatus(info.ParentSession)
	if err != nil {
		return res, fmt.Errorf("review recovery: CurrentStatus(%s): %w", info.ParentSession, err)
	}
	if workerStatus == nil {
		return res, fmt.Errorf("review recovery: parent session %q not found in DB", info.ParentSession)
	}
	if workerStatus.EndedAt != nil {
		// Worker is gone — write the fallback file so the verdict is not
		// silently dropped, but skip delivery (it would error anyway).
		fallback, fbErr := WriteFallbackResult(info.PRNumber, info.Round, deliveryText)
		if fbErr != nil {
			return res, fmt.Errorf("review recovery: parent session ended AND fallback write failed: %w", fbErr)
		}
		res.FallbackTo = fallback
		return res, fmt.Errorf("review recovery: parent session %q has ended \u2014 wrote fallback to %s", info.ParentSession, fallback)
	}

	if delErr := promptdelivery.DeliverToSessionWithID(
		info.ParentSession, workerStatus, deliveryText,
		buildPromptBodyForMonitor, "review-complete", "", deliveryID,
	); delErr != nil {
		// Delivery failed once; mirror MonitorFunc by writing the fallback.
		// We do NOT retry here — the caller (the watcher) will fire again on
		// its next tick if the worker is still stuck in `reviewing`, and the
		// delivery_id dedup ensures a successful delivery sticks exactly once.
		fallback, fbErr := WriteFallbackResult(info.PRNumber, info.Round, deliveryText)
		if fbErr == nil {
			res.FallbackTo = fallback
		}
		return res, fmt.Errorf("review recovery: deliver to %q: %w", info.ParentSession, delErr)
	}

	// Write the authoritative end-of-life signal for this review group
	// (#2259). Once delivered_at is set, GroupCompleted short-circuits to
	// true and ActiveReviewGroupForParent skips this group, so any
	// subsequent mutation of agent_status (e.g. the per-process sidecar-
	// restart anti-pattern in cmd/sidecar.go) cannot flip the parent
	// worker back into "round N already in progress" refusals on the next
	// `prism review`.
	//
	// Failure is non-fatal: the prompt has already been accepted by the
	// host-API /prompt handler at this point. A missing delivered_at write
	// leaves the system in the pre-fix state (vulnerable to agent_status
	// clobbers) but does not lose the verdict.
	if setErr := d.SetGroupDeliveredAt(groupID); setErr != nil {
		proglog.Warnf("[prism review recovery] warning: SetGroupDeliveredAt(%s): %v\n", groupID, setErr)
	} else {
		proglog.Infof("[prism review recovery] group %s delivered_at recorded\n", groupID)
	}

	res.Delivered = true
	return res, nil
}

// agentNameFromSession extracts the agent name suffix from a review-agent
// session name of the form "<parent>~review-<N>-<agent>". Returns the full
// session name unchanged when the pattern is not present (degraded mode for
// unexpected name shapes).
func agentNameFromSession(sessionName string) string {
	tilde := strings.Index(sessionName, "~review-")
	if tilde < 0 {
		return sessionName
	}
	suffix := sessionName[tilde+len("~review-"):]
	dash := strings.Index(suffix, "-")
	if dash < 0 || dash == len(suffix)-1 {
		return sessionName
	}
	return suffix[dash+1:]
}

// RecoveryDeliveryID returns the deterministic delivery_id used for the
// recovery-path /prompt request for groupID. Exported so the watcher and
// integration tests use the same value.
func RecoveryDeliveryID(groupID string) string {
	return "review-recovery:" + groupID
}
