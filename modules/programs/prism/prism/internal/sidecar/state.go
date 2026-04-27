package sidecar

import (
	"encoding/json"
	"log"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// ── helpers (must be called with s.mu held) ─────────────────────────────────

func (s *Sidecar) cancelIdleTimer() {
	if s.idleTimer != nil {
		log.Printf("sidecar: idle debounce cancelled")
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

func (s *Sidecar) currentDBState() agent.AgentState {
	st, err := s.cfg.DB.CurrentStatus(s.cfg.SessionName)
	if err != nil || st == nil {
		return ""
	}
	return agent.AgentState(st.State)
}

func (s *Sidecar) upsertState(state agent.AgentState, title *string, opencodeSID *string) {
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
			opencodeSID,
			&effectiveRole,
			agentModel,
		); err != nil {
			log.Printf("sidecar: UpsertStatusWithRootAgent failed: %v", err)
		}
		return
	}
	if err := s.cfg.DB.UpsertStatus(
		s.cfg.SessionName,
		s.cfg.Repo,
		s.cfg.Worktree,
		string(state),
		title,
		opencodeSID,
	); err != nil {
		log.Printf("sidecar: UpsertStatus failed: %v", err)
	}
}

func (s *Sidecar) writeStateChange(state agent.AgentState) {
	s.writeStateChangeWithSID(state, nil)
}

func (s *Sidecar) writeStateChangeWithSID(state agent.AgentState, opencodeSID *string) {
	if state == s.lastState {
		log.Printf("sidecar: state dedup: %s (no change)", state)
		return
	}
	log.Printf("sidecar: state: %s -> %s", s.lastState, state)
	s.writeEvent("state_change", map[string]string{"state": string(state)}, opencodeSID)
	s.lastState = state
	// Push to the persistent dashboard socket (fire-and-forget, non-blocking).
	// Also touch the sentinel for the popup dashboard which still polls it.
	sessionName := s.cfg.SessionName
	title := s.lastTitle
	stateStr := string(state)
	go pushDashboardEvent(sessionName, stateStr, title)
	touchDashboardSentinel()
}

func (s *Sidecar) writeEvent(eventType string, payload any, opencodeSID *string) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("sidecar: marshal event payload: %v", err)
		return
	}

	sid := opencodeSID
	if sid == nil && s.opencodeSID != "" {
		sid = &s.opencodeSID
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
		log.Printf("sidecar: WriteEvent(%s) failed: %v", eventType, err)
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
func (s *Sidecar) writeStartupError(startupErr error) {
	s.mu.Lock()
	log.Printf("sidecar: startup failure — writing error state: %v", startupErr)
	s.upsertState(agent.StateError, nil, nil)
	s.writeStateChange(agent.StateError)
	s.mu.Unlock()

	// Gap 2 fix: notify the parent worker when this is a review-agent session.
	// Normal finish notifications for review agents remain suppressed in
	// notifyCoordinator — this is an exception only for the startup-failure path.
	go s.notifyParentWorkerOnStartupFailure(startupErr)
}


