// Package sidecar implements the core logic for the prism sidecar process.
//
// The sidecar connects to the opencode SSE event stream and translates events
// into agent state transitions, DB writes, and dashboard sentinel touches —
// replicating the logic in opencode/plugins/prism-hooks.ts.
//
// All timer and clock operations go through an abstracted Clock interface so
// that tests can control time deterministically.
package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sse"
)

// defaultNotifyHTTPClient is the HTTP client used for coordinator notification
// delivery when Config.HTTPClient is nil.
var defaultNotifyHTTPClient = &http.Client{Timeout: 10 * time.Second}

// IdleDebounce is the duration to wait after session.idle before committing
// the finished state. Cancelled if session.status busy fires in the window.
const IdleDebounce = 2 * time.Second

// Clock abstracts time and timer operations for testing.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer abstracts a stoppable timer for testing.
type Timer interface {
	Stop() bool
}

// realClock uses the standard library time functions.
type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// RealClock returns a Clock backed by the standard library.
func RealClock() Clock { return realClock{} }

// Config holds the static configuration for a sidecar instance.
type Config struct {
	SessionName string
	Repo        string
	Worktree    string
	OpencodeURL string
	DB          *db.DB
	Clock       Clock
	// HTTPClient is the HTTP client used for coordinator notification delivery.
	// If nil, defaultNotifyHTTPClient is used.
	HTTPClient *http.Client
}

// Sidecar is the core event processor. It consumes SSE events from the
// opencode stream and drives state transitions in the prism DB.
type Sidecar struct {
	cfg Config

	mu              sync.Mutex
	lastState       agent.AgentState
	idleTimer       Timer
	manualDenial    bool
	compacting      bool
	opencodeSID     string
	writtenMessages map[string]bool // dedup message.updated writes
	textByMessage   map[string]string
}

// New creates a Sidecar with the given configuration.
func New(cfg Config) *Sidecar {
	if cfg.Clock == nil {
		cfg.Clock = RealClock()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = defaultNotifyHTTPClient
	}
	return &Sidecar{
		cfg:             cfg,
		writtenMessages: make(map[string]bool),
		textByMessage:   make(map[string]string),
	}
}

// Run connects to the SSE stream and processes events until ctx is cancelled.
// It blocks until the event channel is closed (context cancellation or
// permanent connection failure).
func (s *Sidecar) Run(ctx context.Context) error {
	url := s.cfg.OpencodeURL + "/event"
	client := &sse.Client{
		InitialRetryDelay: 1 * time.Second,
		MaxRetryDelay:     30 * time.Second,
	}

	ch, err := client.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("sidecar: connect to SSE stream: %w", err)
	}

	for evt := range ch {
		s.HandleEvent(evt)
	}
	return ctx.Err()
}

// HandleEvent processes a single SSE event. It is safe for concurrent use
// (protected by s.mu). Exported for testing.
func (s *Sidecar) HandleEvent(evt sse.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch evt.Type {
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
	}
}

// Shutdown writes the interrupted state if the session is not already in a
// terminal state, and cancels any pending idle timer. Called on SIGINT/SIGTERM.
func (s *Sidecar) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cancelIdleTimer()

	if s.lastState != agent.StateFinished &&
		s.lastState != agent.StateDeleted &&
		s.lastState != agent.StateInterrupted {
		s.upsertState(agent.StateInterrupted, nil, nil)
		s.writeStateChange(agent.StateInterrupted)
	}
}

// ── event handlers (must be called with s.mu held) ──────────────────────────

func (s *Sidecar) handleSessionStatus(evt sse.Event) {
	var payload struct {
		Properties struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
		log.Printf("sidecar: session.status parse error: %v", err)
		return
	}

	switch payload.Properties.Status.Type {
	case "busy":
		s.cancelIdleTimer()
		s.manualDenial = false
		if !s.compacting {
			s.upsertState(agent.StateActive, nil, nil)
			s.writeStateChange(agent.StateActive)
		}
	case "retry":
		s.upsertState(agent.StateError, nil, nil)
		s.writeStateChange(agent.StateError)
	}
}

func (s *Sidecar) handleSessionIdle() {
	// Snapshot current DB state before the timer fires.
	s.cancelIdleTimer()

	s.idleTimer = s.cfg.Clock.AfterFunc(IdleDebounce, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.idleTimer = nil

		// If the user manually denied a permission, write interrupted not finished.
		if s.manualDenial {
			s.manualDenial = false
			s.upsertState(agent.StateInterrupted, nil, nil)
			s.writeStateChange(agent.StateInterrupted)
			return
		}

		// Check current DB state: if interrupted or error, don't overwrite.
		currentState := s.currentDBState()
		if currentState == agent.StateInterrupted || currentState == agent.StateError {
			return
		}

		s.upsertState(agent.StateFinished, nil, nil)
		s.writeStateChange(agent.StateFinished)
		go s.notifyCoordinator()
	})
}

func (s *Sidecar) handleSessionCreated(evt sse.Event) {
	var payload struct {
		Properties struct {
			Info struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"info"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
		log.Printf("sidecar: session.created parse error: %v", err)
		return
	}

	info := payload.Properties.Info
	s.opencodeSID = info.ID
	title := strPtr(info.Title)
	sid := strPtr(info.ID)
	s.upsertState(agent.StateActive, title, sid)
	s.writeStateChange(agent.StateActive)
}

func (s *Sidecar) handleSessionUpdated(evt sse.Event) {
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
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
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
	case agent.StateInterrupted, agent.StateError, agent.StateFinished:
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

func (s *Sidecar) handleSessionError(evt sse.Event) {
	var payload struct {
		Properties struct {
			Error *struct {
				Name string `json:"name"`
			} `json:"error"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
		log.Printf("sidecar: session.error parse error: %v", err)
		return
	}

	errorName := ""
	if payload.Properties.Error != nil {
		errorName = payload.Properties.Error.Name
	}

	if errorName == "MessageAbortedError" {
		// User pressed Escape/Ctrl-C — record as interrupted.
		s.cancelIdleTimer()
		s.upsertState(agent.StateInterrupted, nil, nil)
		s.writeStateChange(agent.StateInterrupted)
	} else {
		s.upsertState(agent.StateError, nil, nil)
		s.writeStateChange(agent.StateError)
		s.writeEvent("error", map[string]string{"name": errorName}, nil)
	}
}

func (s *Sidecar) handleSessionCompacted() {
	s.compacting = false
	s.cancelIdleTimer()

	// If already interrupted, leave it.
	currentState := s.currentDBState()
	if currentState == agent.StateInterrupted {
		return
	}

	s.upsertState(agent.StateFinished, nil, nil)
	s.writeStateChange(agent.StateFinished)
	s.writeEvent("compaction", map[string]string{"note": "compaction complete"}, nil)
	go s.notifyCoordinator()
}

func (s *Sidecar) handleSessionDeleted(evt sse.Event) {
	var payload struct {
		Properties struct {
			Info struct {
				ID string `json:"id"`
			} `json:"info"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
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

func (s *Sidecar) handlePermissionAsked(evt sse.Event) {
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
	if err := json.Unmarshal([]byte(evt.Data), &payload); err == nil {
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

func (s *Sidecar) handlePermissionReplied(evt sse.Event) {
	var payload struct {
		Properties struct {
			Reply string `json:"reply"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
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

func (s *Sidecar) handleMessageUpdated(evt sse.Event) {
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
				Time *struct {
					Created   *float64 `json:"created"`
					Completed *float64 `json:"completed"`
				} `json:"time"`
			} `json:"info"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
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
		if info.Model != nil {
			model = info.Model.ProviderID + "/" + info.Model.ModelID
		}

		s.writeEvent("msg_user", map[string]string{
			"messageId": info.ID,
			"text":      text,
			"agent":     agentName,
			"model":     model,
		}, nil)
		s.writtenMessages[info.ID] = true
		delete(s.textByMessage, info.ID)

	} else if info.Role == "assistant" && info.Time != nil && info.Time.Completed != nil {
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

		if info.Time != nil && info.Time.Created != nil && info.Time.Completed != nil {
			dur := *info.Time.Completed - *info.Time.Created
			if dur > 0 {
				eventPayload["durationMs"] = int(dur)
			}
		}

		s.writeEvent("msg_assistant", eventPayload, nil)
		s.writtenMessages[info.ID] = true
		delete(s.textByMessage, info.ID)
	}
}

func (s *Sidecar) handleMessagePartUpdated(evt sse.Event) {
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
					End *float64 `json:"end"`
				} `json:"time"`
			} `json:"part"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
		log.Printf("sidecar: message.part.updated parse error: %v", err)
		return
	}

	part := payload.Properties.Part

	switch part.Type {
	case "text":
		if part.Text != "" {
			s.textByMessage[part.MessageID] = part.Text
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

// ── helpers (must be called with s.mu held) ─────────────────────────────────

func (s *Sidecar) cancelIdleTimer() {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
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
		return // deduplicate
	}
	s.writeEvent("state_change", map[string]string{"state": string(state)}, opencodeSID)
	s.lastState = state
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

	e := db.Event{
		ID:          uuid.New().String(),
		SessionName: s.cfg.SessionName,
		Repo:        s.cfg.Repo,
		Worktree:    s.cfg.Worktree,
		OpencodeSID: sid,
		Type:        eventType,
		Payload:     string(data),
		CreatedAt:   s.cfg.Clock.Now(),
	}
	if err := s.cfg.DB.WriteEvent(e); err != nil {
		log.Printf("sidecar: WriteEvent(%s) failed: %v", eventType, err)
	}
}

// touchDashboardSentinel creates or updates the dashboard sentinel file's
// modification time, causing the dashboard's watcher to refresh.
func touchDashboardSentinel() {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	sentinel := filepath.Join(stateHome, "prism", "bus", ".dashboard.signal")
	_ = os.MkdirAll(filepath.Dir(sentinel), 0o755)
	now := time.Now()
	if err := os.Chtimes(sentinel, now, now); err != nil {
		f, err := os.OpenFile(sentinel, os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
		}
	}
}

// notifyCoordinator sends a "finished" notification to the coordinator session
// for this repo. It is called asynchronously (via go) after writing
// StateFinished, so s.mu must NOT be held when this method runs.
//
// The coordinator is discovered by looking up "<repo>@main" in the DB. If the
// coordinator has an opencode_port and opencode_sid, the notification is
// delivered via HTTP POST to /session/<sid>/prompt_async. On success, an audit
// row is written via WriteBusMessageDelivered. If the coordinator has no port
// or the HTTP call fails, the notification falls back to WriteBusMessage
// (undelivered, for plugin polling).
//
// If no coordinator exists, it has ended, or this session IS the coordinator,
// the call is a silent no-op.
func (s *Sidecar) notifyCoordinator() {
	// Self-notification guard: if this session is the coordinator, skip.
	coordinatorName := s.cfg.Repo + "@main"
	if s.cfg.SessionName == coordinatorName {
		return
	}

	// Look up coordinator status.
	coordStatus, err := s.cfg.DB.CurrentStatus(coordinatorName)
	if err != nil {
		log.Printf("sidecar: notifyCoordinator: look up coordinator: %v", err)
		return
	}
	if coordStatus == nil {
		// No coordinator session — silent skip.
		return
	}
	if coordStatus.EndedAt != nil {
		// Coordinator has ended — silent skip.
		return
	}

	notifyText := fmt.Sprintf("Agent %s has finished its current task", s.cfg.SessionName)

	msg := db.BusMessage{
		ID:          uuid.New().String(),
		FromSession: s.cfg.SessionName,
		ToSession:   coordinatorName,
		Repo:        s.cfg.Repo,
		Text:        notifyText,
		Urgency:     "normal",
		SentAt:      time.Now(),
	}

	// Try HTTP delivery if coordinator has port and session ID.
	if coordStatus.OpencodePort != nil && coordStatus.OpencodeSID != nil {
		httpErr := deliverNotificationViaHTTP(*coordStatus.OpencodePort, *coordStatus.OpencodeSID, notifyText, coordStatus, s.cfg.HTTPClient)
		if httpErr == nil {
			// HTTP succeeded — write audit trail with delivered_at set.
			if err := s.cfg.DB.WriteBusMessageDelivered(msg); err != nil {
				log.Printf("sidecar: notifyCoordinator: write audit bus message: %v", err)
			}
			return
		}
		// HTTP failed — log and fall through to bus_messages fallback.
		log.Printf("sidecar: notifyCoordinator: HTTP delivery failed, falling back to bus: %v", httpErr)
	}

	// Fallback: write to bus_messages for plugin-based delivery.
	if err := s.cfg.DB.WriteBusMessage(msg); err != nil {
		log.Printf("sidecar: notifyCoordinator: write bus message: %v", err)
	}
}

// deliverNotificationViaHTTP sends a notification prompt to the opencode HTTP API.
func deliverNotificationViaHTTP(port int, opencodeSID string, text string, status *db.Status, httpClient *http.Client) error {
	body := buildNotifyPromptBody(text, status)
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal prompt body: %w", err)
	}

	url := fmt.Sprintf("http://localhost:%d/session/%s/prompt_async", port, opencodeSID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}

	return nil
}

// buildNotifyPromptBody constructs the request body for the coordinator
// notification prompt_async call. When root_agent_name and root_model_id are
// known, they are included so the session continues using its root agent/model.
// Falls back to agent_name/model_id for sessions created before the root fields
// migration. Mirrors the buildPromptBody logic in cmd/prompt.go.
func buildNotifyPromptBody(text string, status *db.Status) map[string]any {
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

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

func marshalTruncated(v any, maxLen int) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return truncate(string(data), maxLen)
}
