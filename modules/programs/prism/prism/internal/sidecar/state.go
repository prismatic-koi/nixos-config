package sidecar

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// ── helpers (must be called with s.mu held) ─────────────────────────────────

func (s *Sidecar) cancelIdleTimer() {
	if s.idleTimer != nil {
		s.logger().Printf("sidecar: idle debounce cancelled")
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func (s *Sidecar) cancelRecoveryTimer() {
	if s.recoveryTimer != nil {
		s.recoveryTimer.Stop()
		s.recoveryTimer = nil
	}
}

// cancelActivityTimer cancels the inactivity watchdog (#1709). Must be called
// with s.mu held.
func (s *Sidecar) cancelActivityTimer() {
	if s.activityTimer != nil {
		s.activityTimer.Stop()
		s.activityTimer = nil
	}
}

// touchActivity resets the inactivity watchdog (#1709). Called on every
// inbound frame from the PI extension (handlePipeFrame) and the SSE harness
// (HandleEvent) so that any sign of life from the agent restarts the
// countdown. A no-op when cfg.ActivityTimeout is zero (watchdog disabled)
// or when the session has already been shut down. Must be called with s.mu
// held.
//
// When the timer fires (no inbound frame for the full window), the closure
// invokes handleActivityTimeout, which force-transitions the session to
// StateError with note="inactivity timeout". The transition is
// state-idempotent: it is a no-op if the session has already reached a
// terminal state through a normal path.
func (s *Sidecar) touchActivity() {
	if s.cfg.ActivityTimeout <= 0 {
		return
	}
	if s.shuttingDown {
		return
	}
	if s.activityTimer != nil {
		s.activityTimer.Stop()
		s.activityTimer = nil
	}
	timeout := s.cfg.ActivityTimeout
	s.activityTimer = s.cfg.Clock.AfterFunc(timeout, func() {
		s.handleActivityTimeout(timeout)
	})
}

// recordInboundFrame notes the receipt of an inbound frame from the agent
// harness (handlePipeFrame on the PI socket-pipe path, HandleEvent on the
// SSE path) and resets the inactivity watchdog. The frame counter and
// timestamps feed the watchdog's stall-vs-no-start classification (#2239):
// when the watchdog fires with zero recorded frames the agent never started;
// with one or more frames it stalled mid-run. Must be called with s.mu held.
func (s *Sidecar) recordInboundFrame() {
	now := s.cfg.Clock.Now()
	if s.inboundFrameCount == 0 {
		s.firstInboundFrameAt = now
	}
	s.inboundFrameCount++
	s.lastInboundFrameAt = now
	s.touchActivity()
}

// handleActivityTimeout is the inactivity-watchdog fire callback (#1709). It
// runs on the Clock's timer goroutine without s.mu held. Acquires s.mu,
// checks that the session is still in a non-terminal state, writes a
// state_change{error} event with note="inactivity timeout", and notifies the
// parent worker via the existing review-agent startup-failure delivery path
// so a stalled review agent surfaces a real signal to its worker rather than
// hanging silently.
//
// Failure-class labelling (#2239): the inbound-frame stats recorded by
// recordInboundFrame distinguish two very different failure classes that the
// watchdog previously collapsed into one "failed to start" label:
//
//   - never-started (inboundFrameCount == 0): no inbound frame was ever
//     received — spawn/handshake/auth failure. A startup_error event is
//     written so the review monitor reports it via the existing no-start
//     path, and the parent notification retains the "failed to start"
//     wording.
//   - mid-run stall (inboundFrameCount > 0): frames flowed, then stopped —
//     stream starvation, provider limit, payload wedge. A stall_error event
//     is written with "stalled mid-run after <elapsed> (<n> frames received,
//     last at <t>)" so the monitor and the parent notification carry the
//     stall label instead of the misleading no-start one.
//
// Both classes remain non-counting infrastructure rounds for the review
// cycle counter (#1995 contract): the session terminates in state "error",
// which is never verdict-producing.
func (s *Sidecar) handleActivityTimeout(timeout interface{}) {
	s.mu.Lock()
	s.activityTimer = nil

	if s.shuttingDown {
		s.mu.Unlock()
		return
	}

	// Re-check current state under the lock. If the session has already
	// reached a terminal state via a normal path (state_change{finished},
	// session_shutdown, etc.), the watchdog is a no-op.
	current := s.currentDBState()
	if current == agent.StateFinished || current == agent.StateError ||
		current == agent.StateInterrupted {
		s.logger().Printf("sidecar: inactivity watchdog fired but session already terminal (state=%s) — no-op", current)
		s.mu.Unlock()
		return
	}

	// Classify the failure before mutating state (#2239).
	frames := s.inboundFrameCount
	firstFrameAt := s.firstInboundFrameAt
	lastFrameAt := s.lastInboundFrameAt

	s.logger().Printf("sidecar: inactivity watchdog fired after %v — transition -> error (cause=inactivity_timeout, inbound_frames=%d)", timeout, frames)
	s.cancelIdleTimer()
	s.cancelRecoveryTimer()
	s.upsertState(agent.StateError, nil, nil)
	s.writeStateChange(agent.StateError)
	s.writeEvent("state_change", map[string]string{
		"state": string(agent.StateError),
		"note":  fmt.Sprintf("inactivity timeout after %v", timeout),
	}, nil)

	// Write the failure-class event so the review monitor's report carries
	// the distinguishing label (#2239). failureText doubles as the
	// parent-notification text below (notifyParentWorkerOnReviewFailure
	// prefixes it with "review agent <name> ").
	var failureText string
	if frames > 0 {
		elapsed := lastFrameAt.Sub(firstFrameAt).Round(time.Second)
		failureText = fmt.Sprintf(
			"stalled mid-run after %s (%d frame(s) received, last at %s): inactivity timeout: no inbound frame for %v",
			elapsed, frames, lastFrameAt.Format(time.RFC3339), timeout)
		s.writeEvent("stall_error", map[string]string{"reason": failureText}, nil)
	} else {
		failureText = fmt.Sprintf(
			"failed to start (no frames received): inactivity timeout: no inbound frame for %v", timeout)
		// The event reason omits the "failed to start" prefix because the
		// monitor's no-start rendering already supplies it
		// ("ERROR: agent failed to start (no-start): <reason>").
		s.writeEvent("startup_error", map[string]string{"reason": fmt.Sprintf(
			"inactivity timeout: no inbound frame for %v (no frames received)", timeout)}, nil)
	}

	s.lastState = agent.StateError
	s.lastErrorAt = s.cfg.Clock.Now()
	s.mu.Unlock()

	// Notify the parent worker on the same channel used for startup
	// failures so review agents that stall mid-turn surface a real signal
	// rather than disappearing into the GroupCompleted timeout. The helper
	// no-ops for non-review-agent session names, so workers and
	// coordinators that opt into ActivityTimeout do not generate spurious
	// parent notifications.
	//
	// Use goNotify (not a raw `go`) so the goroutine is tracked by notifyWG
	// and tests can drain in-flight notifications via WaitNotifies() without
	// sleeping or polling (#1842).
	s.goNotify(func() {
		s.notifyParentWorkerOnReviewFailure(failureText)
	})
}

func (s *Sidecar) currentDBState() agent.AgentState {
	st, err := s.cfg.DB.CurrentStatus(s.cfg.SessionName)
	if err != nil || st == nil {
		return ""
	}
	return agent.AgentState(st.State)
}

// upsertState writes the session's current state (and optional title /
// harness session ID) to the DB. Must be called with s.mu held.
//
// Writes (s.mu-protected fields): s.lastTitle (when title is non-nil and
// non-empty). All other persistence happens via s.cfg.DB and is not
// s.mu-protected struct state.
func (s *Sidecar) upsertState(state agent.AgentState, title *string, harnessSessionID *string) {
	// Track the most recently seen title for dashboard push events.
	if title != nil && *title != "" {
		s.lastTitle = *title
	}
	// Determine the effective agent role to write to root_agent_name:
	//   1. cfg.AgentRole non-empty (container mode): use it directly — the role
	//      is known at startup and authoritative (#555, #557).
	//   2. cfg.AgentRole empty AND s.rootAgent non-empty (host-mode sessions
	//      after SSE inference): use s.rootAgent so that root_agent_name is
	//      written (or self-corrected from a stale "worker" value) on every
	//      state transition after the first user message is seen (#776).
	//   3. cfg.AgentRole empty AND s.rootAgent empty (host-mode session before
	//      any SSE inference, or legacy session): fall back to UpsertStatus so
	//      that root_agent_name is left NULL rather than set to an empty string.
	effectiveRole := s.cfg.AgentRole
	if effectiveRole == "" {
		effectiveRole = s.rootAgent
	}
	if effectiveRole != "" {
		var agentModel *string
		if s.cfg.AgentModel != "" {
			m := s.cfg.AgentModel
			agentModel = &m
		}
		if err := s.cfg.DB.UpsertStatusWithRootAgent(
			s.cfg.SessionName,
			s.cfg.Repo,
			s.cfg.Worktree,
			string(state),
			title,
			harnessSessionID,
			&effectiveRole,
			agentModel,
		); err != nil {
			s.logger().Printf("sidecar: UpsertStatusWithRootAgent failed: %v", err)
		}
		return
	}
	if err := s.cfg.DB.UpsertStatus(
		s.cfg.SessionName,
		s.cfg.Repo,
		s.cfg.Worktree,
		string(state),
		title,
		harnessSessionID,
	); err != nil {
		s.logger().Printf("sidecar: UpsertStatus failed: %v", err)
	}
}

func (s *Sidecar) writeStateChange(state agent.AgentState) {
	s.writeStateChangeWithSID(state, nil)
}

func (s *Sidecar) writeStateChangeWithSID(state agent.AgentState, harnessSessionID *string) {
	if state == s.lastState {
		s.logger().Printf("sidecar: state dedup: %s (no change)", state)
		return
	}
	s.logger().Printf("sidecar: state: %s -> %s", s.lastState, state)
	s.writeEvent("state_change", map[string]string{"state": string(state)}, harnessSessionID)
	s.lastState = state
	// Push to the persistent dashboard socket (fire-and-forget, non-blocking).
	// Also touch the sentinel for the popup dashboard which still polls it.
	//
	// Both calls are routed through s.cfg.DashboardSink so test setups (via
	// sidecartest.NewIsolated) can install a no-op sink and avoid touching
	// $XDG_STATE_HOME-derived paths. Production sessions get the default
	// productionDashboardSink which preserves the historical fire-and-forget
	// goroutine for PushEvent and the inline TouchSentinel. See issue #1851.
	sessionName := s.cfg.SessionName
	title := s.lastTitle
	stateStr := string(state)
	s.cfg.DashboardSink.PushEvent(sessionName, stateStr, title)
	s.cfg.DashboardSink.TouchSentinel()
}

func (s *Sidecar) writeEvent(eventType string, payload any, harnessSessionID *string) {
	data, err := json.Marshal(payload)
	if err != nil {
		s.logger().Printf("sidecar: marshal event payload: %v", err)
		return
	}

	sid := harnessSessionID
	if sid == nil && s.harnessSessionID != "" {
		sid = &s.harnessSessionID
	}

	var instanceIDPtr *string
	if s.cfg.InstanceID != "" {
		iid := s.cfg.InstanceID
		instanceIDPtr = &iid
	}

	e := db.Event{
		ID:               uuid.New().String(),
		SessionName:      s.cfg.SessionName,
		Repo:             s.cfg.Repo,
		Worktree:         s.cfg.Worktree,
		HarnessSessionID: sid,
		InstanceID:       instanceIDPtr,
		Type:             eventType,
		Payload:          string(data),
		CreatedAt:        s.cfg.Clock.Now(),
	}
	if err := s.cfg.DB.WriteEvent(e); err != nil {
		s.logger().Printf("sidecar: WriteEvent(%s) failed: %v", eventType, err)
	}
}

// writeStartupError writes StateError to the DB and, when this session is a
// review agent, sends a notification to the parent worker session. It must be
// called WITHOUT s.mu held — it acquires s.mu itself to call upsertState and
// writeStateChange (which require the lock), then releases it before launching
// the notification goroutine. upsertState's own doc says "called with s.mu held"
// — that contract is satisfied here because writeStartupError holds the lock for
// the entire upsertState + writeStateChange block.
//
// This is the Gap 1 + Gap 2 fix for startup failures (WaitHealthy timeout,
// CreateSession failure). Calling it ensures:
//   - The DB row transitions to "error" immediately in the sidecar, not via the
//     fragile pane-died tmux hook.
//   - The parent worker is notified when a review-agent container fails to start.
//
// The parent-worker notification is launched on a background goroutine because
// it may perform a network/socket call (promptdelivery.DeliverToSession) and
// must not block the caller's startup-error path.
func (s *Sidecar) writeStartupError(startupErr error) {
	s.writeStartupErrorImpl(startupErr, true /* asyncNotify */)
}

// writeStartupErrorSync is the synchronous-notify variant of writeStartupError.
// It performs the same DB writes and then runs notifyParentWorkerOnStartupFailure
// inline (no `go`) so that, when this function returns, all log writes from the
// startup-error path have been committed to s.cfg.Logger.
//
// This variant exists to satisfy the write-ordering invariant required by the
// bwrap startup-connect timeout path (#1690): the `[timing] harness listening:
// ... (timed out)` log marker must be the last log line written by the timeout
// goroutine, so that a reader observing the marker is guaranteed not to race
// with any further concurrent writes from this path. With asynchronous notify,
// notifyParentWorkerOnStartupFailure could still be writing to the logger after
// Run() returned, producing a data race on test loggers (and an unobservable
// log ordering in production).
//
// Must be called WITHOUT s.mu held; same contract as writeStartupError.
func (s *Sidecar) writeStartupErrorSync(startupErr error) {
	s.writeStartupErrorImpl(startupErr, false /* asyncNotify */)
}

func (s *Sidecar) writeStartupErrorImpl(startupErr error, asyncNotify bool) {
	s.mu.Lock()
	s.logger().Printf("sidecar: startup failure — writing error state: %v", startupErr)
	s.upsertState(agent.StateError, nil, nil)
	s.writeStateChange(agent.StateError)
	// Write a startup_error event recording the failure reason so the review
	// monitor can distinguish a no-start failure from a mid-run crash when
	// formatting the review-complete prompt (#1222).
	s.writeEvent("startup_error", map[string]string{"reason": startupErr.Error()}, nil)
	s.mu.Unlock()

	// Gap 2 fix: notify the parent worker when this is a review-agent session.
	// Normal finish notifications for review agents remain suppressed in
	// notifyCoordinator — this is an exception only for the startup-failure path.
	if asyncNotify {
		s.goNotify(func() { s.notifyParentWorkerOnStartupFailure(startupErr) })
	} else {
		s.notifyParentWorkerOnStartupFailure(startupErr)
	}
}


