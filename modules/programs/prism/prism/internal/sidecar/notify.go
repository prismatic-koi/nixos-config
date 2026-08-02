package sidecar

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
)

// reviewAgentParentSession derives the parent worker session name from a
// review-agent session name. Review-agent sessions follow the naming convention:
//
//	<parent>~review-<N>-<role>   e.g. nixos-config@feature~review-2-review-goal
//
// The parent session name is the prefix before "~review".
// Returns ("", false) when the session name does not contain "~review".
func reviewAgentParentSession(sessionName string) (string, bool) {
	idx := strings.Index(sessionName, "~review")
	if idx < 0 {
		return "", false
	}
	return sessionName[:idx], true
}

// investigateAgentInvokerSession derives the invoker session name from an
// investigate-agent session name. Investigate-agent sessions follow the naming
// convention:
//
//	<invoker>~investigate-<slug>   e.g. nixos-config@main~investigate-abc123
//
// The invoker session name is the prefix before "~investigate".
// Returns ("", false) when the session name does not contain "~investigate".
func investigateAgentInvokerSession(sessionName string) (string, bool) {
	idx := strings.Index(sessionName, "~investigate")
	if idx < 0 {
		return "", false
	}
	return sessionName[:idx], true
}

// notifyInvestigatorCompletion delivers a single terminal notification from
// an investigate-agent session to its invoker when the investigation reaches a
// terminal state (finished, interrupted, or error).
//
// It is called asynchronously (via go) from the state-transition points in
// events.go, so s.mu must NOT be held.
//
// The finalText parameter is the text of the last completed turn. When the
// state is not agent.StateFinished (i.e. interrupted or error), an explicit
// failure notice is prepended regardless of whether finalText is present.
//
// Delivery uses promptdelivery.DeliverToSession with deliverAs="followUp" so
// that the notification queues behind any in-flight invoker turn.
//
// The notification body contains:
//   - A sender header: "From investigator session: <name>"
//   - The final assistant text (or a failure notice if state != finished).
//   - A steering-channel hint: "Reply with: prism prompt <name> --prompt '...'"
//
// When the invoker session has ended, the notification is dropped silently.
// When delivery fails, the failure is logged; the investigator keeps running.
func (s *Sidecar) notifyInvestigatorCompletion(state agent.AgentState, finalText string) {
	invokerSession, isInvestigate := investigateAgentInvokerSession(s.cfg.SessionName)
	if !isInvestigate {
		// Not an investigate-agent session — nothing to do.
		return
	}

	// Build the body text.
	var bodyText string
	trimmed := strings.TrimSpace(finalText)
	switch state {
	case agent.StateFinished:
		if trimmed == "" {
			s.logger().Printf("sidecar: notifyInvestigatorCompletion: no final text — delivering empty-completion notice (investigator=%s invoker=%s)",
				s.cfg.SessionName, invokerSession)
			bodyText = fmt.Sprintf(
				"From investigator session: %s\n\nInvestigation complete (no final text recorded).\n\nReply with: prism prompt %s --prompt '...'",
				s.cfg.SessionName, s.cfg.SessionName,
			)
		} else {
			bodyText = fmt.Sprintf(
				"From investigator session: %s\n\n%s\n\nReply with: prism prompt %s --prompt '...'",
				s.cfg.SessionName, trimmed, s.cfg.SessionName,
			)
		}
	default:
		// Interrupted or error: always notify so the invoker learns of the failure.
		if trimmed != "" {
			bodyText = fmt.Sprintf(
				"From investigator session: %s\n\nInvestigation ended with state %q.\n\nLast output:\n%s\n\nReply with: prism prompt %s --prompt '...'",
				s.cfg.SessionName, state, trimmed, s.cfg.SessionName,
			)
		} else {
			bodyText = fmt.Sprintf(
				"From investigator session: %s\n\nInvestigation ended with state %q (no output recorded).\n\nReply with: prism prompt %s --prompt '...'",
				s.cfg.SessionName, state, s.cfg.SessionName,
			)
		}
	}

	// Look up the invoker session.
	invokerStatus, err := s.cfg.DB.CurrentStatus(invokerSession)
	if err != nil {
		s.logger().Printf("sidecar: notifyInvestigatorCompletion: DB lookup for invoker %q: %v — skipping (investigator=%s)",
			invokerSession, err, s.cfg.SessionName)
		return
	}
	if invokerStatus == nil {
		s.logger().Printf("sidecar: notifyInvestigatorCompletion: invoker session %q not found in DB — skipping (investigator=%s)",
			invokerSession, s.cfg.SessionName)
		return
	}
	if invokerStatus.EndedAt != nil {
		s.logger().Printf("sidecar: notifyInvestigatorCompletion: invoker session %q has ended — dropping notification (investigator=%s reason=invoker_ended)",
			invokerSession, s.cfg.SessionName)
		return
	}

	if err := promptdelivery.DeliverToSession(invokerSession, invokerStatus, bodyText, buildNotifyPromptBody, "", "followUp"); err != nil {
		s.logger().Printf("sidecar: notifyInvestigatorCompletion: FAILED — investigator=%s invoker=%s reason=%v",
			s.cfg.SessionName, invokerSession, err)
		return
	}
	s.logger().Printf("sidecar: notifyInvestigatorCompletion: delivered state=%s to invoker=%s (investigator=%s)",
		state, invokerSession, s.cfg.SessionName)
}

// notifyParentWorkerOnStartupFailure sends a notification to the parent worker
// when a review-agent container fails to start. It is called asynchronously via
//
// If the parent worker session cannot be found or has ended, the failure is
// logged and the sidecar exits cleanly (the notification failure is not fatal).
//
// Normal finish notifications for review agents (on the success path) remain
// suppressed via the isReviewAgentSession guard in notifyCoordinator. This
// function is an exception only for the startup-failure path.
func (s *Sidecar) notifyParentWorkerOnStartupFailure(startupErr error) {
	s.notifyParentWorkerOnReviewFailure(fmt.Sprintf("failed to start: %v", startupErr))
}

// notifyParentWorkerOnReviewFailure is the shared delivery path for
// review-agent failure notifications to the parent worker. failureText is
// the failure description beginning with the failure-class verb — e.g.
// "failed to start: …" (startup failures, watchdog fire with no frames
// received) or "stalled mid-run after …" (watchdog fire after one or more
// inbound frames, #2239). The delivered text is
// "review agent <session> <failureText>".
//
// Same contract as notifyParentWorkerOnStartupFailure historically had: only
// applies to review-agent session names (silent no-op otherwise), and
// delivery failures are logged, never fatal.
func (s *Sidecar) notifyParentWorkerOnReviewFailure(failureText string) {
	// Only apply to review-agent sessions.
	parentSession, isReview := reviewAgentParentSession(s.cfg.SessionName)
	if !isReview {
		return
	}

	// Look up the parent worker in the DB.
	parentStatus, err := s.cfg.DB.CurrentStatus(parentSession)
	if err != nil {
		s.logger().Printf("sidecar: notifyParentWorker: DB lookup for parent %q: %v — skipping notification", parentSession, err)
		return
	}
	if parentStatus == nil {
		s.logger().Printf("sidecar: notifyParentWorker: parent session %q not found in DB — skipping notification", parentSession)
		return
	}
	if parentStatus.EndedAt != nil {
		s.logger().Printf("sidecar: notifyParentWorker: parent session %q has ended — skipping notification", parentSession)
		return
	}

	notifyText := fmt.Sprintf("review agent %s %s", s.cfg.SessionName, failureText)

	// All sessions use the pi harness — route through the host-API Unix socket.
	if err := promptdelivery.DeliverToSession(parentSession, parentStatus, notifyText, buildNotifyPromptBody, "", "followUp"); err != nil {
		s.logger().Printf("sidecar: notifyParentWorker: FAILED — parent=%s reason=%v", parentSession, err)
	} else {
		s.logger().Printf("sidecar: notifyParentWorker: delivered to parent=%s via host-API socket", parentSession)
	}
}

// followUpsByteCap bounds the follow-ups body appended to a worker's
// coordinator notification (issue #2528). The section is opt-in and
// worker-authored, but the cap is a defensive backstop so a worker cannot
// deliver an unbounded body regardless of what it writes between the
// markers. When the extracted section exceeds the cap it is truncated and
// the notification says so explicitly, with a prism checkin pointer to the
// untruncated turn.
const followUpsByteCap = 4096

// followUpsOpenTag delimits the worker follow-ups section (issue #2528). A
// worker opts in to a body-bearing coordinator notification by wrapping
// findings in this tag pair during its handoff turn:
//
//	<follow_ups>
//	...content...
//	</follow_ups>
//
// This mirrors the existing <summary>/<blocking_issues> convention review
// agents use (internal/review/results.go's extractTag) so the two
// structured-output conventions stay consistent. Matching is case-insensitive
// and the first well-formed pair wins; an absent or unterminated tag is
// treated as "no follow-ups section" — the notification falls back to the
// generic finish/error wording unchanged, and delivery is never blocked by a
// malformed marker.
const followUpsOpenTag = "follow_ups"

// extractTag returns the trimmed content between the first well-formed
// <tag>...</tag> pair in s (case-insensitive), or ("", false) when no such
// pair exists. This is a sidecar-local copy of the same primitive
// internal/review/results.go uses for <summary>/<blocking_issues> extraction
// — kept local rather than exported across packages to avoid coupling the
// worker follow-ups convention (issue #2528) to the review package's
// internals.
func extractTag(s, tag string) (string, bool) {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	lower := strings.ToLower(s)
	lopen := strings.ToLower(open)
	lclose := strings.ToLower(close)

	start := strings.Index(lower, lopen)
	if start < 0 {
		return "", false
	}
	inner := start + len(open)
	end := strings.Index(lower[inner:], lclose)
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(s[inner : inner+end]), true
}

// extractFollowUps pulls the worker-authored follow-ups section out of
// finalText, if present. It returns the trimmed section content, whether it
// was truncated to followUpsByteCap, and whether a well-formed section was
// found at all. A missing or unterminated tag, or a section that is empty or
// whitespace-only once trimmed, reports found=false — callers must treat
// that identically to "no section" (issue #2528 AC: empty/whitespace-only
// sections are treated the same as absent ones).
func extractFollowUps(finalText string) (content string, truncated bool, found bool) {
	raw, ok := extractTag(finalText, followUpsOpenTag)
	if !ok {
		return "", false, false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, false
	}
	if len(raw) > followUpsByteCap {
		return truncateBytes([]byte(raw), followUpsByteCap), true, true
	}
	return raw, false, true
}

// buildWorkerNotifyText composes the coordinator-facing notification body for
// a worker terminal-state transition. baseText is the existing generic
// wording ("Agent %s has finished/errored its current task"); finalText is
// the text of the worker's last completed turn, from which an opt-in
// follow-ups section is extracted (see extractFollowUps).
//
// When no follow-ups section is present, baseText is returned unchanged — no
// behaviour change from before this issue. When a section is present, the
// notification carries: baseText, the follow-ups content (truncated and
// flagged if it exceeded followUpsByteCap), the source session name (so a
// coordinator with several workers in flight can route it), and a
// `prism checkin <session>` pointer to the full turn.
func buildWorkerNotifyText(baseText, sessionName, finalText string) string {
	content, truncated, found := extractFollowUps(finalText)
	if !found {
		return baseText
	}

	var b strings.Builder
	b.WriteString(baseText)
	b.WriteString(fmt.Sprintf("\n\nFollow-ups from %s:\n%s", sessionName, content))
	if truncated {
		b.WriteString(fmt.Sprintf("\n\n[truncated to %d bytes]", followUpsByteCap))
	}
	b.WriteString(fmt.Sprintf("\n\nFor the full turn: prism checkin %s", sessionName))
	return b.String()
}

// notifyCoordinator sends a "finished" notification to the coordinator session
// for this repo. It is called asynchronously (via go) after writing
// StateFinished, so s.mu must NOT be held when this method runs.
//
// finalText is the text of the worker's last completed turn (the same value
// events.go captures into finalText := s.lastInvestigatorText immediately
// before calling this method). When finalText contains a well-formed
// <follow_ups> section, its content is appended to the notification body via
// buildWorkerNotifyText (issue #2528). When finalText is empty or carries no
// such section, the notification is the unchanged generic "has finished"
// string — no behaviour change from before this issue.
//
// The coordinator is discovered by looking up "<repo>@main" in the DB.
// Notification is delivered via the coordinator's host-API Unix socket
// (pi harness path). On confirmed delivery, an audit row is written via
// WriteBusMessageDelivered. On failure, a row is written via
// WriteBusMessageFailed and a structured error is logged.
//
// If no coordinator exists, it has ended, or this session IS the coordinator,
// the call is a silent no-op.
//
// Review-agent sessions (session names containing "~review") never notify:
// their finish events are internal progress signals consumed by the parent
// worker's pollAgents DB loop. Propagating them to the coordinator would be
// noise — 5 notifications per review round, none of which the coordinator
// needs to act on.
//
// Delivery is a single attempt via promptdelivery.DeliverToSession; there is
// no retry loop or backoff in this function.
func (s *Sidecar) notifyCoordinator(finalText string) {
	baseText := fmt.Sprintf("Agent %s has finished its current task", s.cfg.SessionName)
	s.notifyCoordinatorWithText(buildWorkerNotifyText(baseText, s.cfg.SessionName, finalText))
}

// notifyCoordinatorError sends an "errored" notification to the coordinator
// session for this repo. The wording is the verbatim error-terminal-state
// counterpart of notifyCoordinator's "has finished" wording (skill table:
// Worker terminal-state notifications). Like notifyCoordinator, finalText's
// optional <follow_ups> section (issue #2528) is appended via
// buildWorkerNotifyText when present.
//
// Called asynchronously (via goNotify) after writing StateError on the
// zero-output-exit path (issue #2081). All suppression guards (self,
// review-agent, investigate-agent, escalated, muted) and the audit-row
// behaviour are shared with notifyCoordinator via notifyCoordinatorWithText.
func (s *Sidecar) notifyCoordinatorError(finalText string) {
	baseText := fmt.Sprintf("Agent %s has errored its current task", s.cfg.SessionName)
	s.notifyCoordinatorWithText(buildWorkerNotifyText(baseText, s.cfg.SessionName, finalText))
}

// notifyCoordinatorWithText is the shared implementation used by
// notifyCoordinator ("has finished" wording) and notifyCoordinatorError
// ("has errored" wording). The two terminal-state variants differ only in
// the notifyText they pass in; everything else — recipient discovery,
// suppression guards, audit-row writes, and the notifyCoordinatorDeliverFn
// test seam — is identical and lives here so the two paths cannot drift.
func (s *Sidecar) notifyCoordinatorWithText(notifyText string) {
	// Self-notification guard: if this session IS the coordinator, skip.
	// DB-backed: check root_agent_name == "coordinator" for self.
	// Fallback to name heuristic for pre-migration rows.
	if isCoordinatorSession(s.cfg.SessionName, s.cfg.DB, s.logger()) {
		return
	}

	// Review-agent guard: review-agent sessions are internal to the worker's
	// prism review invocation. Their finish events are discovered by the
	// worker's pollAgents DB poll and must not be forwarded to the coordinator
	// as noise notifications.
	if isReviewAgentSession(s.cfg.SessionName, s.cfg.DB, s.logger()) {
		s.logger().Printf("sidecar: notifyCoordinator: suppressed for review-agent session %s", s.cfg.SessionName)
		return
	}

	// Investigate-agent guard: investigate-agent sessions deliver per-turn
	// body-bearing notifications to their invoker (via notifyInvestigatorTurnEnd)
	// and must not also emit a bare "has finished" notification to the coordinator.
	if _, isInvestigate := investigateAgentInvokerSession(s.cfg.SessionName); isInvestigate {
		s.logger().Printf("sidecar: notifyCoordinator: suppressed for investigate-agent session %s (bare_finish suppressed)", s.cfg.SessionName)
		return
	}

	// Escalated guard: while a session is in the escalated state the worker
	// has already informed the coordinator via `prism escalate` and the
	// session.escalated bus event. A subsequent `has finished` notification
	// would be a duplicate, false signal (the worker is paused awaiting
	// guidance, not done). The state clears back to active on any incoming
	// turn_start, after which a normal finish will notify as usual.
	selfStatus, selfStatusErr := s.cfg.DB.CurrentStatus(s.cfg.SessionName)
	if selfStatusErr == nil && selfStatus != nil && selfStatus.State == string(agent.StateEscalated) {
		s.logger().Printf("sidecar: notifyCoordinator: suppressed (cause=escalated — session.escalated already informed coordinator)")
		return
	}

	// Muted guard: the operator has explicitly silenced this session's
	// outbound coordinator notifications via `prism mute`. The flag is
	// orthogonal to the agent state machine: lifecycle transitions and DB
	// writes continue normally; only the bus-notification to the coordinator
	// is suppressed. Missed notifications are dropped, not queued — if the
	// session is unmuted after a finish, the coordinator does not receive a
	// retroactive ping.
	if selfStatusErr == nil && selfStatus != nil && selfStatus.Muted {
		s.logger().Printf("sidecar: notifyCoordinator: suppressed (cause=muted)")
		return
	}

	// DB-backed coordinator lookup: find the active coordinator for this repo.
	coordStatus, err := s.cfg.DB.CoordinatorForRepo(s.cfg.Repo)
	if err != nil {
		s.logger().Printf("sidecar: notifyCoordinator: DB lookup coordinator for repo %q: %v — falling back to name-based lookup", s.cfg.Repo, err)
	}
	if coordStatus == nil {
		// No coordinator with root_agent_name='coordinator' found — fall back to
		// the name-based convention for pre-migration rows.
		fallbackName := s.cfg.Repo + "@main"
		var fallbackStatus *db.Status
		fallbackStatus, err = s.cfg.DB.CurrentStatus(fallbackName)
		if err != nil {
			s.logger().Printf("sidecar: notifyCoordinator: fallback look up coordinator: %v", err)
			return
		}
		if fallbackStatus != nil {
			// Name-based fallback succeeded: a pre-migration coordinator row was
			// found via the @main name convention. Log deprecation only here,
			// not when no coordinator is running at all (which is a normal state).
			s.logger().Printf("[deprecation] sidecar: notifyCoordinator: no DB-backed coordinator found for %q — falling back to name convention %q (pre-migration row)", s.cfg.Repo, fallbackName)
		}
		coordStatus = fallbackStatus
	}
	if coordStatus == nil {
		// No coordinator session at all — silent skip.
		return
	}
	if coordStatus.EndedAt != nil {
		// Coordinator has ended — silent skip.
		return
	}

	coordinatorName := coordStatus.SessionName

	// Capture the coordinator's current instance_id so the message is scoped
	// to the correct incarnation of the coordinator. If the coordinator has no
	// instance_id (e.g. legacy row), ToInstanceID remains nil and the message
	// is delivered to any coordinator instance (backward-compatible).
	var coordInstanceID *string
	if coordStatus.InstanceID != nil {
		coordInstanceID = coordStatus.InstanceID
	}

	msg := db.BusMessage{
		ID:           uuid.New().String(),
		FromSession:  s.cfg.SessionName,
		ToSession:    coordinatorName,
		ToInstanceID: coordInstanceID,
		Repo:         s.cfg.Repo,
		Text:         notifyText,
		Urgency:      "normal",
		SentAt:       time.Now(),
	}

	// All sessions use the pi harness — deliver via host-API Unix socket.
	// Use "followUp" so the coordinator receives the notification after its
	// current turn completes. Finish notifications are post-turn signals.
	//
	// notifyCoordinatorDeliverFn is the narrow test seam (issue #1856) that
	// lets tests force a delivery failure so the WriteBusMessageFailed audit
	// path can be asserted on. In production the field is nil and the real
	// promptdelivery.DeliverToSession is used. See the seam field's
	// declaration on the Sidecar struct for the full rationale.
	deliverFn := s.notifyCoordinatorDeliverFn
	if deliverFn == nil {
		deliverFn = promptdelivery.DeliverToSession
	}
	if err := deliverFn(coordinatorName, coordStatus, notifyText, buildNotifyPromptBody, "", "followUp"); err != nil {
		s.logger().Printf("sidecar: notifyCoordinator: FAILED — coordinator=%s reason=%v", coordinatorName, err)
		if writeErr := s.cfg.DB.WriteBusMessageFailed(msg); writeErr != nil {
			s.logger().Printf("sidecar: notifyCoordinator: write failed audit: %v", writeErr)
		}
		return
	}
	if err := s.cfg.DB.WriteBusMessageDelivered(msg); err != nil {
		s.logger().Printf("sidecar: notifyCoordinator: write delivered audit: %v", err)
	}
	s.logger().Printf("sidecar: notifyCoordinator: delivered to coordinator=%s via host-API socket", coordinatorName)
}

// buildNotifyPromptBody constructs the request body for the coordinator
// notification prompt_async call. When root_model_id is known, it is included
// so the session continues using its root model. Falls back to model_id for
// sessions created before the root fields migration.
//
// The "agent" field is included when root_agent_name is non-nil and non-empty.
// This re-asserts the correct agent on notification delivery, preventing
// the agent from defaulting to its last-active (wrong) agent in host mode.
//
// Background: issue #848 showed that setting "agent" let an incoming
// notification switch a subagent's context to the notifier's agent. That
// concern does not apply here: the status passed in is the *receiving*
// session's own status, not the sender's. Re-asserting root_agent_name on
// delivery is safe and correct — it keeps the coordinator pinned to the right
// agent persona regardless of what the agent last processed.
func buildNotifyPromptBody(text string, status *db.Status) map[string]any {
	body := map[string]any{
		"parts": []map[string]string{
			{"type": "text", "text": text},
		},
	}

	// Re-assert the receiving session's root agent so the agent does not default
	// to its internally-tracked last-active agent (which may differ in host
	// mode). Only set when root_agent_name is known and non-empty.
	if status.RootAgentName != nil && *status.RootAgentName != "" {
		body["agent"] = *status.RootAgentName
	}

	modelID := status.RootModelID
	if modelID == nil {
		modelID = status.ModelID
	}

	if modelID != nil {
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

// goNotify launches fn on a new goroutine while tracking it in s.notifyWG.
// All call sites that previously used `go s.notify*` should use this helper
// instead so that tests can drain in-flight notify goroutines via
// WaitNotifies before reading test-observable state (logs, DB rows). The
// fire-and-forget production semantics are preserved — callers do not
// observe completion.
//
// This is the canonical site of the fix for the test-race class that
// includes #1713 (testTimer.Fire vs state-machine write) and #1716
// (captureLog strings.Builder Write/Read race). See those issues for the
// race-class context.
func (s *Sidecar) goNotify(fn func()) {
	s.notifyWG.Add(1)
	go func() {
		defer s.notifyWG.Done()
		fn()
	}()
}

// WaitNotifies blocks until every goroutine launched via goNotify has
// returned. It is safe to call from any goroutine and is a no-op when no
// notify goroutines are in flight.
//
// Intended use is in tests that exercise sidecar event handling and read
// log output afterwards: call WaitNotifies between the synchronous event
// dispatch (HandleEvent / testTimer.Fire) and the log-read (captureLog's
// getLogs()) so that any notify goroutine writing to the captured logger
// has completed before the test inspects the buffer.
func (s *Sidecar) WaitNotifies() {
	s.notifyWG.Wait()
}
