package sidecar

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/harness"
)

// HandleEvent processes a single harness event. It is safe for concurrent use
// (protected by s.mu). Exported for testing.
func (s *Sidecar) HandleEvent(evt harness.HarnessEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Delegate opencode-specific event type extraction to the harness adapter.
	// opencode sends all events as plain `data:` lines with no `event:` field;
	// the real event type is embedded in the JSON `type` field of the payload.
	eventType := s.harness.ExtractEventType(evt)

	log.Printf("sidecar: event: %s", eventType)

	// Gap 1b: log the first event received from opencode (once per session).
	if !s.firstEventLogged {
		s.firstEventLogged = true
		elapsed := time.Since(s.spawnTime).Round(time.Millisecond)
		log.Printf("sidecar: first event received from opencode (%s after spawn)", elapsed)

		// Bwrap path `[timing]` markers (#1052). The podman path emits these
		// from sidecar.Run() around WaitHealthy / CreateSession; in bwrap
		// mode opencode is launched by the tmux pane (via prism agent-run)
		// and the sidecar's only signal of readiness is the first SSE event,
		// so the markers are emitted here.
		//
		//   - opencode listening: equivalent to the podman WaitHealthy ok
		//     marker — opencode's HTTP endpoint is reachable, since SSE has
		//     successfully connected and delivered an event.
		//   - ready: the sidecar is processing events. In bwrap there is no
		//     CreateSession step (opencode is started with --prompt by
		//     agent-run, so the session pre-exists), which means listening
		//     and ready coincide. Both lines are still emitted so the bwrap
		//     and podman timelines have the same shape for grepping.
		//   - prompt delivered: when InitialPrompt is non-empty, the prompt
		//     was supplied to opencode via --prompt at agent-run time. From
		//     the sidecar's POV "delivered" is observable when opencode
		//     starts emitting events — the prompt is in flight by then.
		//     Emitted at the same moment for symmetry with the podman line
		//     at sidecar.go:489.
		if !container.CapabilitiesFor(s.cfg.IsolationMode).OwnsContainerLifecycle {
			log.Printf("[timing] opencode listening: %s from start", elapsed)
			log.Printf("[timing] ready: %s from start", elapsed)
			if s.cfg.InitialPrompt != "" {
				log.Printf("[timing] prompt delivered: %s from start", elapsed)
			}
		}
	}

	switch eventType {
	case "server.connected":
		s.handleServerConnected()
	case "server.heartbeat":
		// Periodic keep-alive emitted by opencode when running in --prompt
		// (server-only) mode without an interactive TUI. In that mode
		// session.created is never emitted on the SSE stream (the session
		// pre-exists by the time the sidecar connects), so the heartbeat is
		// the only signal that opencode is alive and the port is up.
		// Write a state_change to agent_events so WaitForReady's DB poll
		// unblocks — but only once, and only if no state has been written yet
		// (i.e. lastState is still empty), to avoid stomping on a real state
		// transition that arrives on a subsequent heartbeat.
		s.handleServerHeartbeat()
	case "session.status":
		s.handleSessionStatus(evt)
	case "session.idle":
		s.handleSessionIdle()
	case "session.created":
		s.handleSessionCreated(evt)
	case "session.updated":
		s.handleSessionUpdated(evt)
	case "session.error":
		s.handleSessionError(evt)
	case "session.compacted":
		s.handleSessionCompacted()
	case "session.deleted":
		s.handleSessionDeleted(evt)
	case "permission.asked":
		s.handlePermissionAsked(evt)
	case "permission.replied":
		s.handlePermissionReplied(evt)
	case "question.asked":
		// The question tool asks the user something — treat like a permission wait.
		s.upsertState(agent.StateWaiting, nil, nil)
		s.writeStateChange(agent.StateWaiting)
	case "question.replied", "question.rejected":
		s.upsertState(agent.StateActive, nil, nil)
		s.writeStateChange(agent.StateActive)
	case "message.updated":
		s.handleMessageUpdated(evt)
	case "message.part.updated":
		s.handleMessagePartUpdated(evt)
	default:
		// Gap 6: log unknown event types once per unique type.
		if !s.seenUnknown[eventType] {
			if len(s.seenUnknown) >= seenUnknownCap {
				if !s.seenUnknownCapReached {
					s.seenUnknownCapReached = true
					log.Printf("sidecar: unknown-event log cap reached")
				}
			} else {
				s.seenUnknown[eventType] = true
				log.Printf("sidecar: event: %s (unhandled — opencode may have added a new event type)", eventType)
			}
		}
	}
}

// ── event handlers (must be called with s.mu held) ──────────────────────────

// handleServerHeartbeat is called when opencode emits server.heartbeat.
// In --prompt (server-only) mode the TUI is absent and session.created is
// never emitted, so the heartbeat is the only liveness signal the sidecar
// receives before the agent starts working. We treat the first heartbeat as
// an "active" readiness signal so WaitForReady's DB poll unblocks.
func (s *Sidecar) handleServerHeartbeat() {
	if s.lastState != "" {
		return // real state already written — don't overwrite
	}
	s.upsertState(agent.StateActive, nil, nil)
	s.writeStateChange(agent.StateActive)
}

// handleServerConnected is called when opencode sends the server.connected
// event on each new SSE connection. On the initial connection the sidecar has
// no prior state (lastState is empty) so this is a no-op. On reconnects the
// sidecar may have been in active state when the connection dropped, in which
// case session.idle might have been emitted during the gap. The recovery timer
// gives arriving events (session.status busy, session.idle) a 60-second window
// to arrive before concluding the session finished and writing that state.
func (s *Sidecar) handleServerConnected() {
	if s.lastState != agent.StateActive {
		return
	}

	// Reconnect while active — start a recovery timer in case session.idle was
	// missed during the gap.
	s.cancelRecoveryTimer()
	log.Printf("sidecar: reconnected while active, starting %v recovery timer", ReconnectRecoveryDelay)
	s.recoveryTimer = s.cfg.Clock.AfterFunc(ReconnectRecoveryDelay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.recoveryTimer = nil

		// Only proceed if we are still in active state (no event arrived to
		// update it in the recovery window).
		if s.lastState != agent.StateActive {
			return
		}
		// Suppress if the DB state is reviewing — the worker is awaiting review
		// results and must not be prematurely finished.
		if s.currentDBState() == agent.StateReviewing {
			log.Printf("sidecar: recovery timer suppressed (cause=reviewing — awaiting review-complete prompt)")
			return
		}
		log.Printf("sidecar: recovery timer fired, writing finished (session.idle likely missed on reconnect)")
		log.Printf("sidecar: transition -> finished (cause=recovery_timer)")
		s.upsertState(agent.StateFinished, nil, nil)
		s.writeStateChange(agent.StateFinished)
		go s.notifyCoordinator()
	})
}

func (s *Sidecar) handleSessionStatus(evt harness.HarnessEvent) {
	var payload struct {
		Properties struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err != nil {
		log.Printf("sidecar: session.status parse error: %v", err)
		return
	}

	switch payload.Properties.Status.Type {
	case "busy":
		s.cancelIdleTimer()
		s.cancelRecoveryTimer()
		s.manualDenial = false
		s.busyEpoch++
		// Suppress the active write if compacting (existing exception) or if the
		// DB state is reviewing — the worker is awaiting the review-complete
		// prompt and any incidental busy turn (e.g. an assistant summary fired
		// after `prism review` returns, before the monitor delivers results)
		// must not clobber `reviewing`. The monitor is responsible for writing
		// `active` to the DB just before delivering the review-complete prompt
		// so that the genuine reviewing→active transition still happens. See
		// internal/review/monitor.go and #1049.
		dbStateOnBusy := s.currentDBState()
		if dbStateOnBusy == agent.StateReviewing {
			log.Printf("sidecar: busy event suppressed (cause=reviewing — awaiting review-complete prompt)")
		} else if !s.compacting {
			s.upsertState(agent.StateActive, nil, nil)
			s.writeStateChange(agent.StateActive)
		}
	case "retry":
		s.cancelIdleTimer()
		s.cancelRecoveryTimer()
		// Record lastErrorAt so that handleSessionUpdated's error-resume debounce
		// also protects this path. session.status{retry} independently writes
		// StateError; without this, an immediate session.updated would bypass the
		// debounce guard and false-resume.
		s.lastErrorAt = s.cfg.Clock.Now()
		log.Printf("sidecar: transition -> error (cause=error_finish)")
		s.upsertState(agent.StateError, nil, nil)
		s.writeStateChange(agent.StateError)
	}
}

func (s *Sidecar) handleSessionIdle() {
	// Snapshot current DB state before the timer fires.
	s.cancelIdleTimer()
	s.cancelRecoveryTimer()

	// If the most recent assistant message was from a subagent (not the root
	// agent), suppress the finished debounce entirely. The parent agent is
	// likely about to resume — the next session.idle after the root agent
	// completes will start the timer normally.
	if s.lastAssistantAgent != "" && s.rootAgent != "" && s.lastAssistantAgent != s.rootAgent {
		log.Printf("sidecar: idle suppressed: lastAssistantAgent=%q is not rootAgent=%q", s.lastAssistantAgent, s.rootAgent)
		return
	}

	log.Printf("sidecar: idle debounce started (%v)", IdleDebounce)
	s.idleTimer = s.cfg.Clock.AfterFunc(IdleDebounce, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.idleTimer = nil

		log.Printf("sidecar: idle debounce fired -> finished")

		// If the user manually denied a permission, write interrupted not finished.
		if s.manualDenial {
			s.manualDenial = false
			log.Printf("sidecar: transition -> interrupted (cause=interrupted_by_denial)")
			s.upsertState(agent.StateInterrupted, nil, nil)
			s.writeStateChange(agent.StateInterrupted)
			return
		}

		// Check current DB state: if interrupted or error, don't overwrite.
		// If reviewing, suppress finished — the worker is awaiting review results;
		// it will transition to finished naturally after the review-complete prompt
		// is delivered and the worker resolves the results.
		currentState := s.currentDBState()
		if currentState == agent.StateInterrupted || currentState == agent.StateError {
			return
		}
		if currentState == agent.StateReviewing {
			log.Printf("sidecar: idle debounce suppressed (cause=reviewing — awaiting review-complete prompt)")
			return
		}

		log.Printf("sidecar: transition -> finished (cause=idle_debounce)")
		s.upsertState(agent.StateFinished, nil, nil)
		s.writeStateChange(agent.StateFinished)
		go s.notifyCoordinator()
	})
}

func (s *Sidecar) handleSessionCreated(evt harness.HarnessEvent) {
	var payload struct {
		Properties struct {
			Info struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"info"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err != nil {
		log.Printf("sidecar: session.created parse error: %v", err)
		return
	}

	info := payload.Properties.Info
	s.opencodeSID = info.ID

	// Always persist the new harness session ID unconditionally so that if the
	// user creates a new session mid-conversation (e.g. via /continue or TUI
	// restart), the DB stays current. upsertState uses COALESCE for
	// harness_session_id (only overwriting when the incoming value is non-nil),
	// which is insufficient here — we need an unconditional update so that a
	// fresh session ID always replaces a stale one. (#694)
	if info.ID != "" {
		if err := s.cfg.DB.UpdateHarnessSessionID(s.cfg.SessionName, info.ID); err != nil {
			log.Printf("sidecar: handleSessionCreated: UpdateHarnessSessionID failed: %v", err)
		}
	}

	title := strPtr(info.Title)
	sid := strPtr(info.ID)
	s.upsertState(agent.StateActive, title, sid)
	s.writeStateChange(agent.StateActive)
}

func (s *Sidecar) handleSessionUpdated(evt harness.HarnessEvent) {
	var payload struct {
		Properties struct {
			Info struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Time  *struct {
					Compacting *float64 `json:"compacting"`
				} `json:"time"`
			} `json:"info"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err != nil {
		log.Printf("sidecar: session.updated parse error: %v", err)
		return
	}

	info := payload.Properties.Info

	if info.Time != nil && info.Time.Compacting != nil {
		// Compaction started.
		s.compacting = true
		s.writeEvent("compaction", map[string]string{"note": "compaction started"}, strPtr(info.ID))
		sid := strPtr(info.ID)
		// Deliberately omit writeStateChange here: compacting is an internal detail
		// and does not warrant a dashboard refresh or state_change event.
		// lastState is intentionally not updated so a later writeStateChange
		// won't consider this a no-op.
		s.upsertState(agent.StateCompacting, nil, sid)
		return
	}

	// Normal session update (possibly a resume).
	s.opencodeSID = info.ID

	// Check if this is a resume from a terminal state.
	currentState := s.currentDBState()
	sid := strPtr(info.ID)
	title := strPtr(info.Title)

	switch currentState {
	case "":
		// No prior row — treat as session creation.
		s.upsertState(agent.StateActive, title, sid)
		s.writeStateChange(agent.StateActive)
	case agent.StateError:
		// Resume from error: only transition to active if we are outside the
		// post-error debounce window. Within ErrorResumeDebounce of the last
		// session.error, this event is treated as post-error churn that opencode
		// emits in the same millisecond burst (session.error → session.updated),
		// not as a genuine user-initiated resume. After the window, genuine
		// resumes (e.g. user presses Enter) transition normally.
		if !s.lastErrorAt.IsZero() && s.cfg.Clock.Now().Sub(s.lastErrorAt) < ErrorResumeDebounce {
			// Within debounce window: treat as churn. Update metadata only.
			log.Printf("sidecar: session.updated within error-resume debounce window (%v since error) — suppressing resume", s.cfg.Clock.Now().Sub(s.lastErrorAt))
			s.upsertState(currentState, title, sid)
			return
		}
		// Outside debounce window (or no error recorded): genuine resume.
		if err := s.cfg.DB.ClearEnded(s.cfg.SessionName); err != nil {
			log.Printf("sidecar: ClearEnded failed on resume: %v", err)
		}
		s.upsertState(agent.StateActive, title, sid)
		s.writeStateChange(agent.StateActive)
	case agent.StateInterrupted, agent.StateFinished:
		// Resume: transition back to active. Clear ended_at so the session
		// becomes visible again in AllActiveStatus / dashboard filters
		// (both query WHERE ended_at IS NULL).
		if err := s.cfg.DB.ClearEnded(s.cfg.SessionName); err != nil {
			log.Printf("sidecar: ClearEnded failed on resume: %v", err)
		}
		s.upsertState(agent.StateActive, title, sid)
		s.writeStateChange(agent.StateActive)
	default:
		// Session exists and is in a non-terminal state. Update metadata only.
		// We still call upsert to update title/opencode_sid/last_seen, but
		// pass the current state to avoid changing it.
		s.upsertState(currentState, title, sid)
	}
}

func (s *Sidecar) handleSessionError(evt harness.HarnessEvent) {
	var payload struct {
		Properties struct {
			Error *struct {
				Name    string `json:"name"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err != nil {
		log.Printf("sidecar: session.error parse error: %v", err)
		return
	}

	errorName := ""
	errorMessage := ""
	if payload.Properties.Error != nil {
		errorName = payload.Properties.Error.Name
		errorMessage = payload.Properties.Error.Message
	}

	// Gap 2: log the error name and truncated message.
	// MessageAbortedError is visually distinguishable — it is a known/expected
	// condition (user pressed Escape/Ctrl-C) whereas other errors are unexpected.
	truncatedMsg := truncate(errorMessage, 200)
	if errorName == "MessageAbortedError" {
		log.Printf("sidecar: session.error: name=%q message=%s [MessageAbortedError — user-initiated]", errorName, truncatedMsg)
	} else {
		log.Printf("sidecar: session.error: name=%q message=%s", errorName, truncatedMsg)
	}

	if errorName == "MessageAbortedError" {
		// User pressed Escape/Ctrl-C — record as interrupted.
		s.cancelIdleTimer()
		s.cancelRecoveryTimer()
		log.Printf("sidecar: transition -> interrupted (cause=interrupted_by_denial)")
		s.upsertState(agent.StateInterrupted, nil, nil)
		s.writeStateChange(agent.StateInterrupted)
	} else {
		s.cancelIdleTimer()
		s.cancelRecoveryTimer()
		// Record the error time so handleSessionUpdated can suppress the
		// post-error session.updated churn that opencode emits in the same
		// millisecond as session.error (see ErrorResumeDebounce).
		s.lastErrorAt = s.cfg.Clock.Now()
		log.Printf("sidecar: transition -> error (cause=error_finish)")
		s.upsertState(agent.StateError, nil, nil)
		s.writeStateChange(agent.StateError)
		s.writeEvent("error", map[string]string{"name": errorName}, nil)
	}
}

func (s *Sidecar) handleSessionCompacted() {
	s.compacting = false
	s.cancelIdleTimer()
	s.cancelRecoveryTimer()

	// If already in a terminal/exceptional state (interrupted, deleted), leave it.
	currentState := s.currentDBState()
	if currentState == agent.StateInterrupted || currentState == agent.StateDeleted {
		return
	}

	// Compaction complete — the session is resuming, not finishing.
	// Do NOT notify the coordinator; the task is still in progress.
	s.upsertState(agent.StateActive, nil, nil)
	s.writeStateChange(agent.StateActive)
	s.writeEvent("compaction", map[string]string{"note": "compaction complete"}, nil)
}

func (s *Sidecar) handleSessionDeleted(evt harness.HarnessEvent) {
	var payload struct {
		Properties struct {
			Info struct {
				ID string `json:"id"`
			} `json:"info"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err != nil {
		log.Printf("sidecar: session.deleted parse error: %v", err)
		return
	}

	// Update state to deleted and set ended_at.
	sid := strPtr(payload.Properties.Info.ID)
	s.upsertState(agent.StateDeleted, nil, sid)
	if err := s.cfg.DB.SetEnded(s.cfg.SessionName); err != nil {
		log.Printf("sidecar: SetEnded failed: %v", err)
	}
	s.writeStateChangeWithSID(agent.StateDeleted, sid)
}

func (s *Sidecar) handlePermissionAsked(evt harness.HarnessEvent) {
	s.upsertState(agent.StateWaiting, nil, nil)
	s.writeStateChange(agent.StateWaiting)

	// Write a permission_ask event with tool info from the payload.
	var payload struct {
		Properties struct {
			Permission string `json:"permission"`
			Patterns   any    `json:"patterns"`
			Tool       *struct {
				MessageID string `json:"messageID"`
			} `json:"tool"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err == nil {
		tool := payload.Properties.Permission
		if tool == "" {
			tool = "unknown"
		}
		msgID := ""
		if payload.Properties.Tool != nil {
			msgID = payload.Properties.Tool.MessageID
		}
		s.writeEvent("permission_ask", map[string]any{
			"tool":      tool,
			"patterns":  payload.Properties.Patterns,
			"messageId": msgID,
		}, nil)
	}
}

func (s *Sidecar) handlePermissionReplied(evt harness.HarnessEvent) {
	var payload struct {
		Properties struct {
			Reply string `json:"reply"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err != nil {
		log.Printf("sidecar: permission.replied parse error: %v", err)
		return
	}

	if payload.Properties.Reply == "reject" {
		s.manualDenial = true
		s.writeEvent("permission_denied", map[string]string{
			"tool": "unknown",
		}, nil)
	}

	s.upsertState(agent.StateActive, nil, nil)
	s.writeStateChange(agent.StateActive)
}

func (s *Sidecar) handleMessageUpdated(evt harness.HarnessEvent) {
	var payload struct {
		Properties struct {
			Info struct {
				ID    string `json:"id"`
				Role  string `json:"role"`
				Agent string `json:"agent"`
				Model *struct {
					ProviderID string `json:"providerID"`
					ModelID    string `json:"modelID"`
				} `json:"model"`
				ProviderID string `json:"providerID"`
				ModelID    string `json:"modelID"`
				Tokens     *struct {
					Input  int `json:"input"`
					Output int `json:"output"`
					Cache  *struct {
						Read  int `json:"read"`
						Write int `json:"write"`
					} `json:"cache"`
				} `json:"tokens"`
				Cost float64 `json:"cost"`
				Time *struct {
					Created   *float64 `json:"created"`
					Completed *float64 `json:"completed"`
				} `json:"time"`
			} `json:"info"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err != nil {
		log.Printf("sidecar: message.updated parse error: %v", err)
		return
	}

	info := payload.Properties.Info

	if info.Role == "user" {
		if s.writtenMessages[info.ID] {
			return
		}
		text := s.textByMessage[info.ID]
		if text == "" {
			return
		}

		agentName := info.Agent
		model := ""
		if info.Model != nil && info.Model.ProviderID != "" && info.Model.ModelID != "" {
			model = info.Model.ProviderID + "/" + info.Model.ModelID
		}

		// Record the root agent from the first user message seen. All
		// subsequent user messages from the same or different agents can
		// be used to detect subagent invocations.
		if s.rootAgent == "" && agentName != "" {
			s.rootAgent = agentName
		}

		// Refresh root_model_id immediately when the user sends a message
		// with a known model. This captures in-session model picker changes
		// before any assistant message is produced, so that worker prompts
		// delivered during the response window read a fresh model value.
		// Only write for root-agent messages (same gate as the assistant
		// path below) and only when model is non-empty (AC-1, AC-2, AC-3,
		// AC-4).
		isRootAgent := s.rootAgent == "" || agentName == "" || agentName == s.rootAgent
		if model != "" && isRootAgent {
			if err := s.cfg.DB.UpdateRootModelID(s.cfg.SessionName, model); err != nil {
				log.Printf("sidecar: UpdateRootModelID (user msg) failed: %v", err)
			}
		}

		s.writeEvent("msg_user", map[string]string{
			"messageId": info.ID,
			"text":      text,
			"agent":     agentName,
			"model":     model,
		}, nil)
		s.writtenMessages[info.ID] = true
		delete(s.textByMessage, info.ID)

	} else if info.Role == "assistant" {
		// Store time.created for TTFT computation as soon as we see any
		// assistant message event (whether or not time.completed is set).
		// This records the request-sent timestamp that we compare against the
		// first text-part arrival. Only store once (first seen wins) so that
		// subsequent message.updated events for the same message don't
		// overwrite the original created time.
		if info.Time != nil && info.Time.Created != nil {
			if _, alreadyStored := s.msgCreatedAtMs[info.ID]; !alreadyStored {
				s.msgCreatedAtMs[info.ID] = *info.Time.Created
			}
		}

		if info.Time == nil || info.Time.Completed == nil {
			// Message not yet complete — nothing more to do.
			return
		}

		if s.writtenMessages[info.ID] {
			return
		}
		text := s.textByMessage[info.ID]
		if text == "" {
			return
		}

		agentName := info.Agent
		model := ""
		if info.ProviderID != "" && info.ModelID != "" {
			model = info.ProviderID + "/" + info.ModelID
		}

		// Track which agent last produced an assistant message. When
		// session.idle fires, this is used to suppress the finished
		// debounce if a subagent (non-root) was most recently active —
		// the parent agent is likely about to resume.
		log.Printf("sidecar: lastAssistantAgent: %q -> %q (rootAgent=%q)", s.lastAssistantAgent, agentName, s.rootAgent)
		s.lastAssistantAgent = agentName
		// If the root agent just completed, clear the tracking so the
		// next session.idle can proceed normally to finished. Also start
		// the idle debounce timer immediately: opencode emits only one
		// session.idle per agent cycle (before the root agent writes its
		// final message), so a second session.idle may never arrive after
		// the root agent appends its handoff message. Starting the timer
		// here ensures the session always reaches finished even when no
		// second idle event is emitted (#538).
		if agentName != "" && agentName == s.rootAgent {
			// Gap 4: emit assistant turn summary before the internal state lines.
			isRoot := agentName == s.rootAgent
			totalTokens := 0
			if info.Tokens != nil {
				totalTokens = info.Tokens.Input + info.Tokens.Output
			}
			log.Printf("sidecar: assistant turn complete (agent=%s root=%t model=%s messageId=%s tokens=%d)",
				agentName, isRoot, model, info.ID, totalTokens)

			log.Printf("sidecar: lastAssistantAgent cleared (root agent completed)")
			s.lastAssistantAgent = ""

			// Start the idle debounce timer immediately. opencode emits only
			// one session.idle per agent cycle — the idle fires after the
			// subagent returns, before the root agent writes its final turn.
			// The root agent then appends its handoff message, but because the
			// session was already idle from opencode's perspective, no second
			// session.idle is emitted. Starting the timer here ensures the
			// session always reaches finished even when no second idle arrives
			// (#538).
			//
			// Capture the current busyEpoch. If session.status busy fires
			// after this point, cancelIdleTimer() stops the timer in
			// handleSessionStatus. The epoch guard in the closure is an
			// additional safety net for the race where Stop() returns false
			// (the goroutine already started running).
			epochAtStart := s.busyEpoch
			log.Printf("sidecar: root-agent message completed — starting idle debounce early (%v)", IdleDebounce)
			s.cancelIdleTimer()
			s.idleTimer = s.cfg.Clock.AfterFunc(IdleDebounce, func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.idleTimer = nil

				// If a new busy event arrived after this timer started, the
				// agent began a new turn — do not transition to finished.
				if s.busyEpoch != epochAtStart {
					log.Printf("sidecar: idle debounce (root-agent message path) suppressed — busy fired after timer start (epochAtStart=%d, busyEpoch=%d)", epochAtStart, s.busyEpoch)
					return
				}

				log.Printf("sidecar: idle debounce fired (root-agent message path) -> finished")

				if s.manualDenial {
					s.manualDenial = false
					log.Printf("sidecar: transition -> interrupted (cause=interrupted_by_denial)")
					s.upsertState(agent.StateInterrupted, nil, nil)
					s.writeStateChange(agent.StateInterrupted)
					return
				}

				currentState := s.currentDBState()
				if currentState == agent.StateInterrupted || currentState == agent.StateError {
					return
				}
				if currentState == agent.StateReviewing {
					log.Printf("sidecar: idle debounce (root-agent message path) suppressed (cause=reviewing — awaiting review-complete prompt)")
					return
				}

				log.Printf("sidecar: transition -> finished (cause=root_agent_idle_debounce)")
				s.upsertState(agent.StateFinished, nil, nil)
				s.writeStateChange(agent.StateFinished)
				go s.notifyCoordinator()
			})
		}

		// Refresh root_model_id with the current session's model so that
		// coordinator notifications always reflect the live model
		// configuration (AC-1, AC-2, AC-3). Only write when model is
		// non-empty to avoid overwriting an existing value with nothing
		// (AC-5). Only write for root-agent messages: if a root agent is
		// known and this message is from a subagent (different name), skip
		// the update so the root agent's model is not overwritten by a
		// subagent's model.
		isRootAgent := s.rootAgent == "" || agentName == "" || agentName == s.rootAgent
		if model != "" && isRootAgent {
			if err := s.cfg.DB.UpdateRootModelID(s.cfg.SessionName, model); err != nil {
				log.Printf("sidecar: UpdateRootModelID failed: %v", err)
			}
		}

		eventPayload := map[string]any{
			"messageId": info.ID,
			"text":      text,
			"agent":     agentName,
			"model":     model,
		}

		if info.Tokens != nil {
			if info.Tokens.Input > 0 {
				eventPayload["inputTokens"] = info.Tokens.Input
			}
			if info.Tokens.Output > 0 {
				eventPayload["outputTokens"] = info.Tokens.Output
			}
			if info.Tokens.Cache != nil {
				if info.Tokens.Cache.Read > 0 {
					eventPayload["cacheReadTokens"] = info.Tokens.Cache.Read
				}
				if info.Tokens.Cache.Write > 0 {
					eventPayload["cacheWriteTokens"] = info.Tokens.Cache.Write
				}
			}
		}

		if info.Cost > 0 {
			eventPayload["cost"] = info.Cost
		}

		if info.Time != nil && info.Time.Created != nil && info.Time.Completed != nil {
			dur := *info.Time.Completed - *info.Time.Created
			if dur > 0 {
				eventPayload["durationMs"] = int(dur)
			}
		}

		if ttft, ok := s.ttftByMessage[info.ID]; ok && ttft > 0 {
			eventPayload["ttftMs"] = ttft
		}

		s.writeEvent("msg_assistant", eventPayload, nil)
		s.writtenMessages[info.ID] = true
		delete(s.textByMessage, info.ID)
		delete(s.msgCreatedAtMs, info.ID)
		delete(s.ttftByMessage, info.ID)
	}
}

func (s *Sidecar) handleMessagePartUpdated(evt harness.HarnessEvent) {
	var payload struct {
		Properties struct {
			Part struct {
				Type      string `json:"type"`
				MessageID string `json:"messageID"`
				Text      string `json:"text"`
				Tool      string `json:"tool"`
				State     *struct {
					Status string `json:"status"`
					Input  any    `json:"input"`
					Output any    `json:"output"`
					Time   *struct {
						Start *float64 `json:"start"`
						End   *float64 `json:"end"`
					} `json:"time"`
				} `json:"state"`
				Time *struct {
					Start *float64 `json:"start"`
					End   *float64 `json:"end"`
				} `json:"time"`
			} `json:"part"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err != nil {
		log.Printf("sidecar: message.part.updated parse error: %v", err)
		return
	}

	part := payload.Properties.Part

	switch part.Type {
	case "text":
		if part.Text != "" {
			s.textByMessage[part.MessageID] = part.Text
		}
		// Capture TTFT from the first text part with a time.start timestamp.
		// Only record once per message (first text part wins).
		if _, alreadyRecorded := s.ttftByMessage[part.MessageID]; !alreadyRecorded {
			if part.Time != nil && part.Time.Start != nil {
				if createdAt, ok := s.msgCreatedAtMs[part.MessageID]; ok {
					ttft := *part.Time.Start - createdAt
					if ttft > 0 {
						s.ttftByMessage[part.MessageID] = int64(ttft)
					}
				}
			}
		}

	case "tool":
		if part.State != nil && part.State.Status == "completed" {
			args := marshalTruncated(part.State.Input, 500)
			result := truncate(fmt.Sprintf("%v", part.State.Output), 500)

			toolPayload := map[string]any{
				"tool":      part.Tool,
				"args":      args,
				"messageId": part.MessageID,
			}
			if part.State.Time != nil && part.State.Time.Start != nil && part.State.Time.End != nil {
				dur := *part.State.Time.End - *part.State.Time.Start
				if dur > 0 {
					toolPayload["durationMs"] = int(dur)
				}
			}
			s.writeEvent("tool_call", toolPayload, nil)
			s.writeEvent("tool_result", map[string]string{
				"tool":      part.Tool,
				"result":    result,
				"messageId": part.MessageID,
			}, nil)

			// Promote high-impact bash commands to the persistent audit log.
			if part.Tool == "bash" {
				if cmd := extractBashCommand(part.State.Input); isHighImpactCommand(cmd) {
					s.writeEvent("audit", map[string]any{
						"tool":             "bash",
						"command":          cmd,
						"sessionName":      s.cfg.SessionName,
						"harnessSessionID": s.opencodeSID,
						"messageId":        part.MessageID,
					}, nil)
					log.Printf("sidecar: audit: high-impact command recorded: %s", truncate(cmd, 120))
				}
			}
		} else if part.State != nil && part.State.Status == "error" {
			// Gap 5: log tool call failures and write a tool_error DB event.
			errStr := truncate(fmt.Sprintf("%v", part.State.Output), 200)
			log.Printf("sidecar: tool call failed (tool=%s messageId=%s err=%s)", part.Tool, part.MessageID, errStr)
			s.writeEvent("tool_error", map[string]string{
				"tool":      part.Tool,
				"err":       errStr,
				"messageId": part.MessageID,
			}, nil)
		}

	case "reasoning":
		if part.Time != nil && part.Time.End != nil {
			s.writeEvent("thinking", map[string]string{
				"text":      truncate(part.Text, 500),
				"messageId": part.MessageID,
			}, nil)
		}
	}
}
