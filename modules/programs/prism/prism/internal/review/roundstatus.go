package review

// roundstatus.go — the single classifier for "did every agent in this review
// round produce a verdict, and if not, why?" (#2573).
//
// Background. Before this file existed, three separate call sites answered
// overlapping parts of that question with their own ad-hoc scans of the
// db.GroupResults map:
//
//   - buildMonitorResults      → per-agent AgentResult.Output strings
//   - buildDeliveryMessage     → the header branch (no-start / stall / …)
//   - currentCycleProducedVerdicts + CompletedReviewCyclesForParent
//     → whether the round counts against the 3-cycle LOOP-LIMIT
//
// All three read `groupData` only, so an agent whose row was NOT in that map
// was invisible to two of them. db.GroupResults deliberately drops any
// agent_status row whose ended_at is set (the #1495 cleanup escape hatch),
// so a review agent whose session was closed mid-round simply vanished: the
// round reported "4 verdicts" and counted as a full cycle, and the lost
// dimension read as a pass. That is the #2573 silent failure.
//
// ClassifyRound is now the single source of truth. It walks the EXPECTED
// agent list (not the map that happened to come back), so an absent member is
// a first-class outcome with a class and a reason, exactly like a no-start or
// a mid-run stall.
//
// Cycle-counting contract. A round counts against the 3-cycle limit only when
// every expected agent produced a parseable verdict — RoundStatus.CountsAsCycle
// is Complete(). This extends the existing "rounds that do not count"
// machinery (#1512 / #1995 / #2239) rather than adding a parallel mechanism;
// the same predicate now backs both the in-flight monitor gate and the
// historical count in CompletedReviewCyclesForParent.

import (
	"fmt"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
)

// NoVerdictClass names the reason an expected review agent produced no
// verdict. The string value is the short human label used in reports.
type NoVerdictClass string

const (
	// NoVerdictNoStart — the sidecar wrote a startup_error event: the agent
	// never ran (#1222).
	NoVerdictNoStart NoVerdictClass = "failed to start"
	// NoVerdictStalled — the inactivity watchdog fired after one or more
	// inbound frames: the agent ran, then went silent (#2239).
	NoVerdictStalled NoVerdictClass = "stalled mid-run"
	// NoVerdictCrashed — the member reached state "error" with neither a
	// startup_error nor a stall_error event: a mid-run crash.
	NoVerdictCrashed NoVerdictClass = "ended in error state"
	// NoVerdictSessionEnded — the member's agent_status row exists but its
	// ended_at is set, so db.GroupResults excluded it. The session was
	// reaped mid-round; no verdict was recorded (#2573). Since #2613 this
	// class covers only the reaps for which no closing path recorded a
	// cause, plus the tmux-session-end hook; the two causes it used to
	// conflate now have classes of their own below.
	NoVerdictSessionEnded NoVerdictClass = "session ended mid-review"
	// NoVerdictNotReady — the agent was spawned but never signalled
	// readiness, so the review readiness gate closed its row before the
	// round began (#2613). Distinct from NoVerdictForceTerminated: nothing
	// terminated a running agent, because the agent never came up.
	NoVerdictNotReady NoVerdictClass = "failed its readiness gate"
	// NoVerdictForceTerminated — a prism lifecycle path closed the row while
	// the round was running: the monitor's safety-timeout sweep, a cleanup
	// cascade from the parent worker, or a direct cleanup command (#2613).
	// Distinct from NoVerdictNotReady: the agent was up and was stopped.
	NoVerdictForceTerminated NoVerdictClass = "force-terminated"
	// NoVerdictSessionUnknown — no agent_status row for the member could be
	// read at all: the row was deleted, or it was never registered against
	// the group.
	NoVerdictSessionUnknown NoVerdictClass = "session not found in group results"
	// NoVerdictNoOutput — the agent reached "finished" with no recorded
	// msg_assistant event (#1995).
	NoVerdictNoOutput NoVerdictClass = "finished with no output"
	// NoVerdictUnparseable — the agent reached "finished" and produced text
	// with no parseable <verdict> tag (#1995).
	NoVerdictUnparseable NoVerdictClass = "finished with no parseable verdict"
	// NoVerdictUnexpectedState — the group reported complete while this
	// member was still in a non-terminal state (monitor safety timeout).
	NoVerdictUnexpectedState NoVerdictClass = "ended in an unexpected state"
)

// Infrastructure reports whether the class is an infrastructure fault rather
// than a defect in the agent's own output.
//
// The distinction drives report wording, not cycle counting: EVERY class here
// leaves a review dimension unexamined, so no class consumes a review cycle.
// The two non-infrastructure classes (#1995) are the ones where the agent
// itself ran to completion and simply failed to emit a usable verdict.
func (c NoVerdictClass) Infrastructure() bool {
	switch c {
	case NoVerdictNoStart, NoVerdictStalled, NoVerdictCrashed,
		NoVerdictSessionEnded, NoVerdictSessionUnknown, NoVerdictUnexpectedState,
		NoVerdictNotReady, NoVerdictForceTerminated:
		return true
	default:
		return false
	}
}

// countLabel is the class label used in the report's count summary. It adds
// the disambiguating detail the operator needs to pick a response.
func (c NoVerdictClass) countLabel() string {
	switch c {
	case NoVerdictNoStart:
		return "failed to start (no frames received)"
	case NoVerdictNotReady:
		return "failed its readiness gate (no readiness signal)"
	case NoVerdictForceTerminated:
		return "force-terminated by a prism lifecycle path"
	default:
		return string(c)
	}
}

// MissingVerdict records one expected agent that produced no verdict.
type MissingVerdict struct {
	// Agent is the role name, e.g. "review-qa".
	Agent string
	// Session is the agent's prism session name.
	Session string
	// Class is the reason class.
	Class NoVerdictClass
	// Reason is the detail recorded for this agent: the startup_error text,
	// the stall reason, or the state/ended_at of a reaped row.
	Reason string
}

// RoundStatus is the classification of one review round.
type RoundStatus struct {
	// Expected is the number of agents the round spawned.
	Expected int
	// Verdicts is the number of agents that produced a parseable verdict.
	Verdicts int
	// Fails is the number of agents that produced a parseable FAIL verdict.
	// It gates the re-run advice: a FAIL means the worker must change code,
	// which makes every other verdict in the round stale (see
	// TargetedRerunAllowed).
	Fails int
	// Missing lists every expected agent that produced no verdict, in the
	// order the agents were spawned.
	Missing []MissingVerdict
}

// HasFailVerdict reports whether any agent that ran returned a FAIL verdict.
func (rs RoundStatus) HasFailVerdict() bool {
	return rs.Fails > 0
}

// Complete reports whether every expected agent produced a parseable verdict.
// A round with zero expected agents is never complete.
func (rs RoundStatus) Complete() bool {
	return rs.Expected > 0 && len(rs.Missing) == 0
}

// CountsAsCycle reports whether this round consumes one of the worker's three
// review cycles. Only a complete round does (#2573 AC-3 / AC-6).
func (rs RoundStatus) CountsAsCycle() bool {
	return rs.Complete()
}

// NonCountingLabel returns the short label naming why this round does not
// count toward the 3-cycle limit, mirroring the header branch
// buildDeliveryMessage selects for the same round (#2573). Returns "" for a
// complete round (CountsAsCycle() true) — there is nothing to label.
//
// This is the single source of truth for the non-counting label text so a
// historical consumer (`prism retro`'s review-cycle detail, issue #2584) and
// the live delivery message cannot drift apart.
func (rs RoundStatus) NonCountingLabel() string {
	if rs.Complete() {
		return ""
	}
	noStart := rs.MissingOfClass(NoVerdictNoStart)
	stalled := rs.MissingOfClass(NoVerdictStalled)
	switch {
	case len(noStart) > 0 && len(noStart) == rs.Expected:
		return "all agents failed to start (infrastructure failure)"
	case len(stalled) > 0 && len(stalled) == rs.Expected:
		return "all agents stalled mid-run (infrastructure failure)"
	case !rs.HasInfrastructureFailure():
		return "ran but produced no parseable verdict"
	default:
		return fmt.Sprintf("round incomplete: %d of %d review agents produced a verdict (%s)",
			rs.Verdicts, rs.Expected, rs.ClassSummary())
	}
}

// MissingOfClass returns the missing entries of one class, in spawn order.
func (rs RoundStatus) MissingOfClass(c NoVerdictClass) []MissingVerdict {
	var out []MissingVerdict
	for _, m := range rs.Missing {
		if m.Class == c {
			out = append(out, m)
		}
	}
	return out
}

// HasInfrastructureFailure reports whether any missing agent failed for an
// infrastructure reason.
func (rs RoundStatus) HasInfrastructureFailure() bool {
	for _, m := range rs.Missing {
		if m.Class.Infrastructure() {
			return true
		}
	}
	return false
}

// MissingAgentNames returns the role names of every agent with no verdict, in
// spawn order and de-duplicated.
func (rs RoundStatus) MissingAgentNames() []string {
	seen := make(map[string]struct{}, len(rs.Missing))
	out := make([]string, 0, len(rs.Missing))
	for _, m := range rs.Missing {
		if m.Agent == "" {
			continue
		}
		if _, dup := seen[m.Agent]; dup {
			continue
		}
		seen[m.Agent] = struct{}{}
		out = append(out, m.Agent)
	}
	return out
}

// TargetedRerunCommand returns the `prism review … --only …` command that
// re-runs exactly the agents that produced no verdict (#2573 AC-5). It
// returns "" when no agent is missing or no agent name could be recovered.
//
// Callers must not print this command unconditionally — see TargetedRerunAllowed for
// the gate.
func (rs RoundStatus) TargetedRerunCommand(prNumber string) string {
	names := rs.MissingAgentNames()
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("prism review %s --only %s", prLabelForCommand(prNumber), strings.Join(names, ","))
}

// FullRerunCommand returns the `prism review …` command that re-runs the whole
// agent set.
func (rs RoundStatus) FullRerunCommand(prNumber string) string {
	return fmt.Sprintf("prism review %s", prLabelForCommand(prNumber))
}

// prLabelForCommand renders the PR argument of a re-run command, degrading to
// a placeholder when the PR number is not known (the recovery path can
// reconstruct a group with no PR number).
func prLabelForCommand(prNumber string) string {
	if prNumber == "" {
		return "<pr>"
	}
	return prNumber
}

// TargetedRerunAllowed reports whether the report may advertise a targeted `--only`
// re-run of the agents that produced no verdict.
//
// The targeted-rerun condition (#2530, widened by #2557, stated in
// `modules/programs/prism/agents/worker.md`) is prose-only — no code enforces
// it. A worker may re-run a subset only when the inter-cycle diff is exactly
// formatter output, comments, or documentation, and touches no file cited in a
// FAIL finding. Any other change makes the verdicts from the agents that ran
// stale, because they were produced against the pre-fix commit.
//
// The report cannot see the inter-cycle diff, so it applies the one part of
// the condition it CAN evaluate: when an agent that ran returned FAIL, the
// worker has to change code before re-running, so a targeted re-run is not
// valid and the report must print the full-set command instead. When no agent
// returned FAIL, the report prints the targeted command with the
// push-nothing-else caveat attached.
func (rs RoundStatus) TargetedRerunAllowed() bool {
	return !rs.HasFailVerdict()
}

// ClassSummary renders the per-class counts, e.g.
// "1 failed to start (no frames received), 2 stalled mid-run". Classes appear
// in a stable order so the text does not churn between rounds.
func (rs RoundStatus) ClassSummary() string {
	order := []NoVerdictClass{
		NoVerdictNoStart,
		NoVerdictNotReady,
		NoVerdictStalled,
		NoVerdictCrashed,
		NoVerdictForceTerminated,
		NoVerdictSessionEnded,
		NoVerdictSessionUnknown,
		NoVerdictUnexpectedState,
		NoVerdictNoOutput,
		NoVerdictUnparseable,
	}
	var parts []string
	for _, c := range order {
		if n := len(rs.MissingOfClass(c)); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, c.countLabel()))
		}
	}
	return strings.Join(parts, ", ")
}

// ClassifyRound classifies a completed review round.
//
// agents and agentSessions are parallel slices describing the agents the round
// spawned — the EXPECTED set. groupData is the db.GroupResults map, which
// omits any member whose agent_status row has been closed (ended_at set) or
// deleted. endedRows supplies those omitted rows, keyed by session name, so
// the classifier can report why the member vanished; pass nil when the caller
// has no DB handle (the member is then classified as
// NoVerdictSessionUnknown).
//
// ClassifyRound reports a closed row without its recorded close cause. Use
// ClassifyRoundWithCauses wherever a DB handle is available — that is what
// splits a readiness-gate failure from a force-terminate (#2613). This
// signature is kept for callers that need only the cycle-counting predicate,
// which does not depend on the cause.
func ClassifyRound(agents []Agent, agentSessions []string, groupData map[string]db.GroupMemberResult, endedRows map[string]db.Status) RoundStatus {
	return ClassifyRoundWithCauses(agents, agentSessions, groupData, endedRows, nil)
}

// ClassifyRoundWithCauses is ClassifyRound plus the close causes recorded for
// the group's closed rows (db.SessionEndCauses). causes may be nil or partial;
// a member with no recorded cause degrades to the same output ClassifyRound
// produces.
func ClassifyRoundWithCauses(
	agents []Agent,
	agentSessions []string,
	groupData map[string]db.GroupMemberResult,
	endedRows map[string]db.Status,
	causes map[string]db.SessionEndCause,
) RoundStatus {
	rs := RoundStatus{Expected: len(agents)}
	for i, ag := range agents {
		session := ""
		if i < len(agentSessions) {
			session = agentSessions[i]
		}
		name := ag.Name
		if name == "" {
			name = agentNameFromSession(session)
		}

		mr, present := groupData[session]
		if session == "" || !present {
			class, reason := classifyAbsentMember(session, endedRows, causes)
			rs.Missing = append(rs.Missing, MissingVerdict{
				Agent:   name,
				Session: session,
				Class:   class,
				Reason:  reason,
			})
			continue
		}

		class, reason, kind := classifyMember(mr)
		if kind != VerdictNone {
			rs.Verdicts++
			if kind == VerdictFail {
				rs.Fails++
			}
			continue
		}
		rs.Missing = append(rs.Missing, MissingVerdict{
			Agent:   name,
			Session: session,
			Class:   class,
			Reason:  reason,
		})
	}
	return rs
}

// classifyMember classifies one member that IS present in the GroupResults
// map. It returns (class, reason, kind). A kind of VerdictNone means the
// member produced no verdict and the class and reason apply; VerdictPass or
// VerdictFail means it did, and the class and reason must be ignored.
//
// The branch order mirrors buildMonitorResults so the per-agent AgentResult
// and the round-level classification cannot disagree:
//
//  1. startup_error present → no-start, whatever the state says (#1222).
//  2. state "error" + stall_error → mid-run stall (#2239). The state check
//     keeps a stale stall_error on a member that later resumed and finished
//     (#1495) from relabelling a real verdict.
//  3. state "error" otherwise → mid-run crash.
//  4. state "finished" → verdict, no output, or unparseable output (#1995).
//  5. anything else → non-terminal at group completion.
func classifyMember(mr db.GroupMemberResult) (NoVerdictClass, string, VerdictKind) {
	if mr.StartupError != "" {
		return NoVerdictNoStart, mr.StartupError, VerdictNone
	}
	if mr.State == "error" {
		if mr.StallError != "" {
			return NoVerdictStalled, mr.StallError, VerdictNone
		}
		return NoVerdictCrashed, "agent did not complete cleanly (state: error)", VerdictNone
	}
	if mr.State == "finished" {
		if mr.LastMessage == "" {
			return NoVerdictNoOutput, "no msg_assistant event was recorded for the session", VerdictNone
		}
		text := ExtractAssistantText(mr.LastMessage)
		_, kind := AssessPassed(text)
		if kind == VerdictNone {
			return NoVerdictUnparseable, "the last assistant message carried no <verdict>PASS</verdict> / <verdict>FAIL</verdict> tag", VerdictNone
		}
		return "", "", kind
	}
	return NoVerdictUnexpectedState, fmt.Sprintf("the group completed while this member was still in state %q", mr.State), VerdictNone
}

// classifyAbsentMember explains why an expected member is absent from the
// GroupResults map (#2573).
//
// db.GroupResults reads `agent_status WHERE group_id = ? AND ended_at IS
// NULL`, so there are exactly two ways to be absent:
//
//   - the row exists but its ended_at is set — the session was REAPED
//     mid-round by one of the SetEnded paths (the tmux-session-end event
//     hook, the sidecar's harness session.deleted handler, the readiness-
//     timeout cleanup, `prism cleanup`, `prism reset`). endedRows carries
//     that row, so the report can name the state and the closing time.
//   - no row can be read at all — the row was deleted (DB prune), or it was
//     never registered against the group.
//
// The group itself never "loses" a member: group_id linkage is intact in both
// cases, which is why db.GroupCompleted still reports the round complete.
//
// causes carries the close cause each lifecycle path recorded for the row
// (#2613), plus the sidecar's own startup_error / stall_error events. The
// order below is causal, not alphabetical: an agent that failed on its own
// (no-start, stall) is reported by that failure, because the later close is a
// consequence of it, not the cause of the missing verdict.
func classifyAbsentMember(session string, endedRows map[string]db.Status, causes map[string]db.SessionEndCause) (NoVerdictClass, string) {
	if session == "" {
		return NoVerdictSessionUnknown, "no session name was recorded for this agent slot"
	}
	row, ok := endedRows[session]
	if !ok {
		return NoVerdictSessionUnknown,
			"no agent_status row could be read for the session (row deleted, or never registered against the group)"
	}
	when := "an unrecorded time"
	if row.EndedAt != nil {
		when = row.EndedAt.UTC().Format(time.RFC3339)
	}
	closedAt := fmt.Sprintf("the agent_status row was closed at %s in state %q", when, row.State)

	cause := causes[session]

	// 1. The sidecar recorded that the agent never ran (#1222). The close
	//    that followed is bookkeeping; the no-start is the cause.
	if cause.StartupError != "" {
		return NoVerdictNoStart, fmt.Sprintf("%s — %s", cause.StartupError, closedAt)
	}
	// 2. The sidecar recorded a mid-run stall (#2239). Same reasoning: the
	//    stall came first, and it is what the operator must act on. Before
	//    #2613 a stalled agent whose tmux session then died was reported as
	//    an unexplained reap, because this branch did not exist.
	if cause.StallError != "" {
		return NoVerdictStalled, fmt.Sprintf("%s — %s", cause.StallError, closedAt)
	}
	// 3. The state the row was left in already explains itself. "finished"
	//    means the agent completed and the row closed before the results were
	//    aggregated (#2594); "deleted" means the harness dropped the session.
	//    A close cause recorded on top of either says who closed the row, not
	//    why the verdict is missing, so the state wins.
	if hint := selfExplainingStateHint(row.State); hint != "" {
		return NoVerdictSessionEnded, fmt.Sprintf("%s — %s", closedAt, hint)
	}
	// 4. A lifecycle path recorded why it closed the row. Exactly one cause.
	if cause.Cause != "" {
		detail := ""
		if cause.Detail != "" {
			detail = fmt.Sprintf(" (%s)", cause.Detail)
		}
		return classForReapCause(cause.Cause), fmt.Sprintf("%s — %s%s",
			closedAt, cause.Cause.Description(), detail)
	}
	// 5. The tmux session-closed hook stamped ended_at. It rewrites no state,
	//    so the state column is whatever the agent last reached.
	if cause.TmuxSessionEnded {
		return NoVerdictSessionEnded, fmt.Sprintf(
			"%s — the tmux session closed and the session-end hook closed the row", closedAt)
	}
	// 6. Nothing was recorded. Say exactly that rather than guessing between
	//    paths — a guess reads as a finding and is not one.
	return NoVerdictSessionEnded, fmt.Sprintf("%s — %s", closedAt, endedRowHint(row.State))
}

// selfExplainingStateHint returns the explanation for a closed row whose state
// already accounts for the missing verdict, or "" when the state does not.
// These states are reported from the state alone, ahead of any recorded close
// cause: knowing that an operator ran `prism cleanup` on a row that had
// already reached "finished" does not explain the missing verdict.
func selfExplainingStateHint(state string) string {
	switch state {
	case "finished":
		return "the agent finished but its row was closed before results were aggregated"
	case "deleted":
		return "the harness reported session.deleted (the sidecar closes the row on that event)"
	default:
		return ""
	}
}

// classForReapCause maps a recorded close cause to the no-verdict class that
// describes it. The mapping is total: an unrecognised cause (a newer prism
// wrote it) degrades to NoVerdictForceTerminated, which is accurate for every
// path that closes a row deliberately.
func classForReapCause(c db.SessionReapCause) NoVerdictClass {
	switch c {
	case db.ReapCauseReadinessGate:
		return NoVerdictNotReady
	case db.ReapCauseSpawnFailure:
		return NoVerdictNoStart
	default:
		return NoVerdictForceTerminated
	}
}

// endedRowHint is the last-resort text for a closed row with NO recorded
// cause. Each branch names one possibility. Before #2613 the "error" branch
// named two — "the session was force-terminated, or its readiness gate
// failed" — which is why #2610 could not be diagnosed from the report. Those
// two paths now record a cause and are classified above, so this branch says
// only what is true: the row closed and nothing recorded why.
func endedRowHint(state string) string {
	if hint := selfExplainingStateHint(state); hint != "" {
		return hint
	}
	if state == "interrupted" {
		return "the session was interrupted and then closed"
	}
	return "no close cause was recorded for this row"
}

// endedGroupMembers returns the group's agent_status rows whose ended_at is
// set, keyed by session name. These are exactly the rows db.GroupResults
// drops. Errors are non-fatal: the caller degrades to the
// NoVerdictSessionUnknown class rather than losing the delivery.
func endedGroupMembers(d *db.DB, groupID string) map[string]db.Status {
	if d == nil || groupID == "" {
		return nil
	}
	members, err := d.GroupMembersForGroup(groupID)
	if err != nil {
		return nil
	}
	return endedRowsFrom(members)
}

// endedMemberCauses reads the recorded close cause for every closed row in
// endedRows (#2613). Errors are non-fatal: the caller degrades to the
// no-cause-recorded wording rather than losing the delivery.
func endedMemberCauses(d *db.DB, endedRows map[string]db.Status) map[string]db.SessionEndCause {
	if d == nil || len(endedRows) == 0 {
		return nil
	}
	names := make([]string, 0, len(endedRows))
	for name := range endedRows {
		names = append(names, name)
	}
	causes, err := d.SessionEndCauses(names)
	if err != nil {
		proglog.Warnf("[prism review] warning: SessionEndCauses: %v — reporting closed rows without a cause\n", err)
		return nil
	}
	return causes
}

// endedRowsFrom filters a member slice down to the rows whose ended_at is set.
func endedRowsFrom(members []db.Status) map[string]db.Status {
	out := make(map[string]db.Status, len(members))
	for _, m := range members {
		if m.EndedAt != nil {
			out[m.SessionName] = m
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
