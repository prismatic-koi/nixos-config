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

	notifyText := fmt.Sprintf("review agent %s failed to start: %v", s.cfg.SessionName, startupErr)

	// All sessions use the pi harness — route through the host-API Unix socket.
	if err := promptdelivery.DeliverToSession(parentSession, parentStatus, notifyText, buildNotifyPromptBody, "", "followUp"); err != nil {
		s.logger().Printf("sidecar: notifyParentWorker: FAILED — parent=%s reason=%v", parentSession, err)
	} else {
		s.logger().Printf("sidecar: notifyParentWorker: delivered to parent=%s via host-API socket", parentSession)
	}
}

// notifyCoordinator sends a "finished" notification to the coordinator session
// for this repo. It is called asynchronously (via go) after writing
// StateFinished, so s.mu must NOT be held when this method runs.
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
func (s *Sidecar) notifyCoordinator() {
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

	notifyText := fmt.Sprintf("Agent %s has finished its current task", s.cfg.SessionName)

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
	if err := promptdelivery.DeliverToSession(coordinatorName, coordStatus, notifyText, buildNotifyPromptBody, "", "followUp"); err != nil {
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
