// Package sidecar implements the core logic for the prism sidecar process.
//
// The sidecar connects to the opencode SSE event stream and translates events
// into agent state transitions, DB writes, and dashboard sentinel touches —
// replicating the logic in opencode/plugins/prism-hooks.ts.
//
// In container mode (Config.Container != nil), the sidecar also manages the
// podman container lifecycle: create, health-check, stop, remove.
//
// All timer and clock operations go through an abstracted Clock interface so
// that tests can control time deterministically.
package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	session "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sse"
)

// defaultNotifyHTTPClient is the HTTP client used for coordinator notification
// delivery when Config.HTTPClient is nil.
var defaultNotifyHTTPClient = &http.Client{Timeout: 10 * time.Second}

// IdleDebounce is the duration to wait after session.idle before committing
// the finished state. Cancelled if session.status busy fires in the window.
const IdleDebounce = 2 * time.Second

// ReconnectRecoveryDelay is the window the sidecar waits after a reconnect
// (detected via server.connected while in active state) before concluding
// that session.idle was missed and writing the finished state. Any arriving
// session.status busy or session.idle event resets normal flow and cancels
// this timer.
const ReconnectRecoveryDelay = 60 * time.Second

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
	// AgentRole is the top-level agent role for this session (e.g. "worker" or
	// "coordinator"), derived from the --agent-role CLI flag. When non-empty, it
	// is used to pre-set rootAgent at initialisation time so that subagent user
	// messages (which have a non-empty agent field) do not accidentally overwrite
	// rootAgent with a subagent name (#555).
	AgentRole string
	// HTTPClient is the HTTP client used for coordinator notification delivery.
	// If nil, defaultNotifyHTTPClient is used.
	HTTPClient *http.Client
	// Container, when non-nil, enables container mode: the sidecar creates and
	// manages a podman container running opencode serve instead of relying on a
	// directly-launched opencode process.
	Container *container.Config
	// HostAPISockPath, when non-empty and Container is non-nil, is the path at which
	// the sidecar starts a Unix socket HTTP server exposing host-side tmux operations
	// to agents running inside the container.
	HostAPISockPath string
	// OnReady is called (once, synchronously) after the container is healthy
	// and before the SSE loop starts. Used in container mode to write the
	// readiness signal file that unblocks the tmux pane running "opencode attach".
	// No-op when nil.
	OnReady func()
	// InitialPrompt, when non-empty, is delivered to the opencode server via
	// prompt_async after the first session.created event is received following
	// container readiness. This is the mechanism for prompt delivery in container
	// mode, where the TUI runs "opencode attach" and cannot accept --prompt (#487).
	InitialPrompt string
}

// Sidecar is the core event processor. It consumes SSE events from the
// opencode stream and drives state transitions in the prism DB.
type Sidecar struct {
	cfg Config

	mu              sync.Mutex
	lastState       agent.AgentState
	idleTimer       Timer
	recoveryTimer   Timer
	manualDenial    bool
	compacting      bool
	opencodeSID     string
	writtenMessages map[string]bool // dedup message.updated writes
	textByMessage   map[string]string
	// container is set when running in container mode.
	// Protected by mu.
	container *containerMgr
	// hostAPIListener is the Unix socket listener for the host-API HTTP server.
	// Set in Run() when container mode is active and HostAPISockPath is non-empty.
	// Protected by mu.
	hostAPIListener net.Listener
	// hostAPISrv is the HTTP server for the host-API Unix socket.
	// Stored so Shutdown() can drain in-flight requests gracefully.
	// Protected by mu.
	hostAPISrv *http.Server
	// shuttingDown is set to true at the start of Shutdown(). Used by Run()
	// to prevent OnReady from firing after SIGTERM even when the HTTP health
	// probe succeeds during podman stop's grace period. Protected by mu.
	shuttingDown bool
	// rootAgent is the name of the top-level agent for this session.
	// Pre-set from Config.AgentRole in New() when non-empty (#555); falls back
	// to inference from the first user message with a non-empty agent field when
	// AgentRole is empty (see handleMessageUpdated).
	rootAgent string
	// lastAssistantAgent is the agent name from the most recent completed
	// assistant message. Used to suppress spurious finished transitions when a
	// subagent (non-root) was the last to produce output: in that case the
	// parent agent is likely about to resume, so session.idle should not start
	// the finished debounce.
	lastAssistantAgent string
	// busyEpoch is incremented each time a session.status busy event is
	// received. It is used by the root-agent-message debounce path to detect
	// whether a new busy event arrived after the timer was started (which
	// would mean the agent started a new turn and the debounce should be
	// cancelled). The existing cancelIdleTimer() in handleSessionStatus
	// already stops the timer; busyEpoch provides an additional guard for
	// the timer closure so it can bail out even if Stop() loses the race.
	busyEpoch uint64
}

// New creates a Sidecar with the given configuration.
func New(cfg Config) *Sidecar {
	if cfg.Clock == nil {
		cfg.Clock = RealClock()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = defaultNotifyHTTPClient
	}
	s := &Sidecar{
		cfg:             cfg,
		writtenMessages: make(map[string]bool),
		textByMessage:   make(map[string]string),
	}
	// Pre-set rootAgent from the configured agent role so that subagent user
	// messages (which have a non-empty agent field in SSE events) do not
	// accidentally overwrite it. The existing user-message inference logic
	// (handleMessageUpdated) is preserved as a fallback for when AgentRole is
	// empty (#555).
	if cfg.AgentRole != "" {
		s.rootAgent = cfg.AgentRole
	}
	return s
}

// containerMgr holds the container.Manager when running in container mode.
// It is set in Run before the SSE loop starts, and used by Shutdown.
type containerMgr struct {
	mgr *container.Manager
}

// Run connects to the SSE stream and processes events until ctx is cancelled.
// It blocks until the event channel is closed (context cancellation or
// permanent connection failure).
//
// When Config.Container is non-nil (container mode), Run first creates and
// health-checks the podman container before subscribing to the SSE stream.
// The container is stopped and removed by Shutdown().
func (s *Sidecar) Run(ctx context.Context) error {
	// Container mode: create and health-check the container before connecting.
	if s.cfg.Container != nil {
		mgr := container.New(*s.cfg.Container)
		s.mu.Lock()
		s.container = &containerMgr{mgr: mgr}
		s.mu.Unlock()

		log.Printf("sidecar: creating container %q", mgr.Name())
		if err := mgr.Create(ctx); err != nil {
			return fmt.Errorf("sidecar: container create: %w", err)
		}

		log.Printf("sidecar: waiting for container %q to become healthy", mgr.Name())
		if err := mgr.WaitHealthy(ctx); err != nil {
			log.Printf("sidecar: health check failed: %v", err)
			// AC-14: genuine timeout — Shutdown() has not been called yet, so we
			// must stop/remove the container here.
			// When SIGTERM arrives, the signal goroutine calls Shutdown() (which
			// sets shuttingDown=true and stops the container) then cancel(). In
			// that case ctx may still be non-nil here (cancel fires after Shutdown),
			// but we check ctx.Err() to distinguish a context-cancelled WaitHealthy
			// (SIGTERM path where Shutdown already ran) from a genuine probe timeout
			// (no Shutdown yet), avoiding a double-Shutdown with spurious log lines.
			if ctx.Err() == nil {
				mgr.Shutdown()
			}
			return fmt.Errorf("sidecar: container health check: %w", err)
		}
		log.Printf("sidecar: container %q is healthy", mgr.Name())

		// Signal readiness after the container is healthy (AC-7, AC-19).
		// Guard with shuttingDown to prevent OnReady from firing after SIGTERM,
		// even if WaitHealthy returned a genuine 200 during podman stop's grace
		// period. Shutdown() sets shuttingDown=true before starting podman stop,
		// so this check is reliable regardless of ctx cancellation timing.
		s.mu.Lock()
		isShuttingDown := s.shuttingDown
		s.mu.Unlock()
		// Deliver the initial prompt (#487) before calling OnReady, so the
		// .sid file is on disk before the TUI readiness-wait script unblocks.
		// 1. POST /session  — create the session, capture its ID.
		// 2. Write the sid file so opencode attach -s <sid> opens the right session.
		// 3. Call OnReady  — unblocks the TUI pane, which runs opencode attach -s <sid>.
		// 4. POST /session/<sid>/prompt_async — deliver the prompt. The TUI is
		//    now attaching/subscribed so execution begins immediately.
		if !isShuttingDown && s.cfg.InitialPrompt != "" {
			sid, createErr := s.createOpencodeSession(s.cfg.OpencodeURL, s.cfg.HTTPClient)
			if createErr != nil {
				log.Printf("sidecar: deliverInitialPrompt: create session: %v", createErr)
			} else {
				if sidPath, err := session.SidecarSessionPath(s.cfg.SessionName); err == nil {
					_ = os.WriteFile(sidPath, []byte(sid), 0o644)
				}
			}
			if !isShuttingDown && s.cfg.OnReady != nil {
				s.cfg.OnReady()
			}
			if createErr == nil {
				go s.deliverInitialPrompt(s.cfg.OpencodeURL, sid, s.cfg.InitialPrompt, s.cfg.HTTPClient)
			}
		} else if !isShuttingDown && s.cfg.OnReady != nil {
			s.cfg.OnReady()
		}

		// Start the host-API Unix socket server (AC-1, AC-9).
		// Guard with !isShuttingDown: if SIGTERM arrived between WaitHealthy
		// and here, Shutdown() will have already run and we must not create a
		// new listener that would never be closed.
		if !isShuttingDown && s.cfg.HostAPISockPath != "" {
			// Remove stale socket file from a previous crashed session.
			_ = os.Remove(s.cfg.HostAPISockPath)
			ln, listenErr := net.Listen("unix", s.cfg.HostAPISockPath)
			if listenErr != nil {
				log.Printf("sidecar: host-API server: listen: %v", listenErr)
			} else {
				srv := &http.Server{Handler: s.hostAPIHandler()}
				s.mu.Lock()
				s.hostAPIListener = ln
				s.hostAPISrv = srv
				s.mu.Unlock()
				go func() {
					if err := srv.Serve(ln); err != nil &&
						!errors.Is(err, http.ErrServerClosed) &&
						!errors.Is(err, net.ErrClosed) {
						log.Printf("sidecar: host-API server: %v", err)
					}
				}()
			}
		} else if s.cfg.HostAPISockPath == "" {
			log.Printf("sidecar: host-API server: HostAPISockPath is empty — skipping (container mode active but no socket path configured)")
		}
	}

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

	// opencode sends all events as plain `data:` lines with no `event:` field.
	// The SSE spec defaults the event type to "message" when no `event:` line
	// is present. Extract the real event type from the JSON `type` field in the
	// data payload when the SSE-level type is "message" or empty.
	eventType := evt.Type
	if eventType == "" || eventType == "message" {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(evt.Data), &envelope); err == nil && envelope.Type != "" {
			eventType = envelope.Type
		}
	}

	log.Printf("sidecar: event: %s", eventType)

	switch eventType {
	case "server.connected":
		s.handleServerConnected()
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
// terminal state, cancels any pending idle timer, and stops/removes the
// container (if running in container mode). Called on SIGINT/SIGTERM.
func (s *Sidecar) Shutdown() {
	s.mu.Lock()
	// Mark shutdown before releasing the lock so that Run()'s OnReady guard
	// sees shuttingDown=true even if it races with Shutdown() (AC-16).
	s.shuttingDown = true
	ctr := s.container
	s.mu.Unlock()

	// Stop and remove the container before writing state — this ensures
	// cleanup happens even if SIGTERM arrives during health-check (AC-16).
	if ctr != nil {
		ctr.mgr.Shutdown()
	}

	// Close the host-API Unix socket listener and remove the socket file (AC-5).
	// Drain in-flight requests with a short deadline before closing.
	s.mu.Lock()
	ln := s.hostAPIListener
	srv := s.hostAPISrv
	s.mu.Unlock()
	if srv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	} else if ln != nil {
		_ = ln.Close()
	}
	if ln != nil {
		_ = os.Remove(s.cfg.HostAPISockPath)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cancelIdleTimer()
	s.cancelRecoveryTimer()

	if s.lastState != agent.StateFinished &&
		s.lastState != agent.StateDeleted &&
		s.lastState != agent.StateInterrupted {
		s.upsertState(agent.StateInterrupted, nil, nil)
		s.writeStateChange(agent.StateInterrupted)
	}
}

// ── event handlers (must be called with s.mu held) ──────────────────────────

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
		log.Printf("sidecar: recovery timer fired, writing finished (session.idle likely missed on reconnect)")
		s.upsertState(agent.StateFinished, nil, nil)
		s.writeStateChange(agent.StateFinished)
		go s.notifyCoordinator()
	})
}

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
		s.cancelRecoveryTimer()
		s.manualDenial = false
		s.busyEpoch++
		if !s.compacting {
			s.upsertState(agent.StateActive, nil, nil)
			s.writeStateChange(agent.StateActive)
		}
	case "retry":
		s.cancelRecoveryTimer()
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
		s.cancelRecoveryTimer()
		s.upsertState(agent.StateInterrupted, nil, nil)
		s.writeStateChange(agent.StateInterrupted)
	} else {
		s.cancelRecoveryTimer()
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

		// Record the root agent from the first user message seen. All
		// subsequent user messages from the same or different agents can
		// be used to detect subagent invocations.
		if s.rootAgent == "" && agentName != "" {
			s.rootAgent = agentName
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
					s.upsertState(agent.StateInterrupted, nil, nil)
					s.writeStateChange(agent.StateInterrupted)
					return
				}

				currentState := s.currentDBState()
				if currentState == agent.StateInterrupted || currentState == agent.StateError {
					return
				}

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

// createOpencodeSession creates a new session on the opencode server via
// POST /session and returns its ID. The directory defaults to /workspace
// (the container's working directory).
func (s *Sidecar) createOpencodeSession(opencodeURL string, httpClient *http.Client) (string, error) {
	body := map[string]string{"directory": "/workspace"}
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal session body: %w", err)
	}

	req, err := http.NewRequest("POST", opencodeURL+"/session", bytes.NewReader(jsonBytes))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("http status %d", resp.StatusCode)
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("empty session ID in response")
	}
	return result.ID, nil
}

// deliverInitialPrompt sends the initial spawn prompt to an existing opencode
// session via POST /session/<sid>/prompt_async (#487).
//
// The session must already exist (created by createOpencodeSession). The TUI
// will have received the sid and opened that session via opencode attach -s
// before this is called, so prompt_async fires into a subscribed session.
func (s *Sidecar) deliverInitialPrompt(opencodeURL, sid, prompt string, httpClient *http.Client) {
	agentRole := s.cfg.AgentRole
	if agentRole == "" {
		agentRole = "worker"
	}

	body := map[string]any{
		"parts": []map[string]string{
			{"type": "text", "text": prompt},
		},
		"agent": agentRole,
	}
	if s.cfg.Container != nil && s.cfg.Container.AgentModel != "" {
		slashIdx := strings.Index(s.cfg.Container.AgentModel, "/")
		if slashIdx >= 0 {
			body["model"] = map[string]string{
				"providerID": s.cfg.Container.AgentModel[:slashIdx],
				"modelID":    s.cfg.Container.AgentModel[slashIdx+1:],
			}
		}
	}

	jsonBytes, err := json.Marshal(body)
	if err != nil {
		log.Printf("sidecar: deliverInitialPrompt: marshal body: %v", err)
		return
	}

	url := fmt.Sprintf("%s/session/%s/prompt_async", opencodeURL, sid)
	log.Printf("sidecar: deliverInitialPrompt: POST %s (agent=%s)", url, agentRole)

	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		log.Printf("sidecar: deliverInitialPrompt: create request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("sidecar: deliverInitialPrompt: http request: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("sidecar: deliverInitialPrompt: http status %d", resp.StatusCode)
		return
	}
	log.Printf("sidecar: deliverInitialPrompt: completed for session %s", sid)
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
		// Read up to 200 bytes of the response body to include in the error so
		// that the root cause of non-2xx responses is self-diagnosing in logs.
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		bodySnippet := strings.TrimSpace(string(bodyBytes))
		if bodySnippet != "" {
			return fmt.Errorf("http status %d: %s", resp.StatusCode, bodySnippet)
		}
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

// ── host-API handler ─────────────────────────────────────────────────────────

// hostAPIHandler returns an http.Handler that exposes host-side tmux operations
// to agents running inside the container via a Unix socket. Routes:
//
//	POST /spawn   — spawn a new worktree session
//	POST /cleanup — clean up an existing session
//	POST /switch  — switch the tmux client to a session
//
// AC-11: returns HTTP 400 for malformed JSON, HTTP 405 for non-POST methods.
func (s *Sidecar) hostAPIHandler() http.Handler {
	mux := http.NewServeMux()

	// writeJSON writes a JSON response with the given status code.
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			http.Error(w, `{"error":"internal: marshal response"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(data)
	}

	// writeError writes a JSON error response.
	writeError := func(w http.ResponseWriter, status int, msg string) {
		writeJSON(w, status, map[string]string{"error": msg})
	}

	// requirePost writes a 405 and returns false if the method is not POST.
	// Returns true when the method is POST (caller should proceed).
	requirePost := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return false
		}
		return true
	}

	// prismBinary returns the path to the prism binary (this process).
	// Uses os.Executable() — consistent with StartSidecarWithOpts — to get
	// the absolute path at binary launch time, avoiding CWD-relative resolution.
	prismBinary := func() string {
		self, err := os.Executable()
		if err != nil {
			return os.Args[0]
		}
		return self
	}

	// POST /spawn
	// Request:  {"repo":"nixos-config","branch":"my-feature","prompt":"...","agent":"worker"}
	// Response: {"session_name":"nixos-config@my-feature"} | {"error":"..."}
	mux.HandleFunc("/spawn", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		var req struct {
			Repo   string `json:"repo"`
			Branch string `json:"branch"`
			Prompt string `json:"prompt"`
			Agent  string `json:"agent"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.Repo == "" {
			writeError(w, http.StatusBadRequest, "repo is required")
			return
		}
		if req.Branch == "" {
			writeError(w, http.StatusBadRequest, "branch is required")
			return
		}

		args := []string{"spawn", "--branch", req.Branch}
		if req.Prompt != "" {
			args = append(args, "--prompt", req.Prompt)
		}
		if req.Agent != "" {
			args = append(args, "--agent", req.Agent)
		}
		args = append(args, req.Repo)

		// Log without the prompt value — it may contain sensitive context.
		logArgs := []string{"spawn", "--branch", req.Branch}
		if req.Prompt != "" {
			logArgs = append(logArgs, "--prompt", "<omitted>")
		}
		if req.Agent != "" {
			logArgs = append(logArgs, "--agent", req.Agent)
		}
		logArgs = append(logArgs, req.Repo)
		log.Printf("sidecar: host-API /spawn: prism %s", strings.Join(logArgs, " "))
		cmd := exec.Command(prismBinary(), args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("sidecar: host-API /spawn: %v: %s", err, out)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("spawn failed: %v", err))
			return
		}

		// prism spawn headless prints: session "name" created
		// Parse the session name from the output.
		sessionName := parseSpawnSessionName(string(out))
		if sessionName == "" {
			// Fallback: derive from repo@branch (branch already sanitised by spawn).
			sessionName = req.Repo + "@" + req.Branch
		}
		writeJSON(w, http.StatusOK, map[string]string{"session_name": sessionName})
	})

	// POST /cleanup
	// Request:  {"session":"nixos-config@my-feature","yes":true}
	// Response: {} | {"error":"..."}
	mux.HandleFunc("/cleanup", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		var req struct {
			Session string `json:"session"`
			Yes     bool   `json:"yes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.Session == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}

		args := []string{"cleanup", "--session", req.Session}
		if req.Yes {
			args = append(args, "--yes")
		}

		log.Printf("sidecar: host-API /cleanup: prism %s", strings.Join(args, " "))
		cmd := exec.Command(prismBinary(), args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("sidecar: host-API /cleanup: %v: %s", err, out)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("cleanup failed: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{})
	})

	// POST /switch
	// Request:  {"session":"nixos-config@my-feature"}
	// Response: {} | {"error":"..."}
	mux.HandleFunc("/switch", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		var req struct {
			Session string `json:"session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.Session == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}

		// Resolve worktree path for the session from the DB, then use
		// prism switch --path <worktree> to switch the tmux client.
		worktreePath, err := s.worktreePathForSession(req.Session)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("resolve worktree: %v", err))
			return
		}

		args := []string{"switch", "--path", worktreePath}
		log.Printf("sidecar: host-API /switch: prism %s", strings.Join(args, " "))
		cmd := exec.Command(prismBinary(), args...)
		out, switchErr := cmd.CombinedOutput()
		if switchErr != nil {
			log.Printf("sidecar: host-API /switch: %v: %s", switchErr, out)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("switch failed: %v", switchErr))
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{})
	})

	return mux
}

// worktreePathForSession looks up the worktree path for a session from the DB.
// Used by the /switch host-API handler to resolve the path for prism switch --path.
func (s *Sidecar) worktreePathForSession(sessionName string) (string, error) {
	status, err := s.cfg.DB.CurrentStatus(sessionName)
	if err != nil {
		return "", fmt.Errorf("db lookup: %w", err)
	}
	if status == nil {
		return "", fmt.Errorf("session %q not found in DB", sessionName)
	}
	if status.Worktree == "" {
		return "", fmt.Errorf("session %q has no worktree path in DB", sessionName)
	}
	return status.Worktree, nil
}

// parseSpawnSessionName parses the session name from the output of `prism spawn`
// in headless mode, which prints: session "name" created
// Returns empty string if the output does not match the expected format.
func parseSpawnSessionName(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		// Match: session "name" created
		if !strings.HasPrefix(line, "session ") || !strings.HasSuffix(line, " created") {
			continue
		}
		// Strip prefix "session " and suffix " created".
		inner := strings.TrimPrefix(line, "session ")
		inner = strings.TrimSuffix(inner, " created")
		// inner should now be a quoted string like `"name"`.
		if len(inner) >= 2 && inner[0] == '"' && inner[len(inner)-1] == '"' {
			return inner[1 : len(inner)-1]
		}
	}
	return ""
}
