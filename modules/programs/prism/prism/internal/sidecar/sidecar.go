// Package sidecar implements the core logic for the prism sidecar process.
//
// The sidecar subscribes to the agent runtime's event stream (via a
// harness.Harness adapter) and translates events into agent state transitions,
// DB writes, and dashboard sentinel touches — replicating the logic in
// opencode/plugins/prism-hooks.ts.
//
// All runtime-specific logic (session creation, prompt delivery, SSE
// subscription, event type mapping, message extraction, container command,
// health check, config mount path) is delegated to the Harness interface.
// The concrete implementation used in production is internal/harness/opencode.
// The Harness value is injected at construction time via Config.Harness
// (Phase 0a of the multi-harness migration, RFC #691).
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	session "github.com/prismatic-koi/prism/internal/session"
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
	// Harness is the agent runtime adapter used for all runtime-specific
	// operations: SSE subscription, session creation, prompt delivery, event
	// type extraction, and event mapping. When non-nil it is used directly;
	// when nil, New() panics — callers must always provide a Harness.
	Harness harness.Harness
	// AgentRole is the top-level agent role for this session (e.g. "worker" or
	// "coordinator"), derived from the --agent-role CLI flag. When non-empty, it
	// is used to pre-set rootAgent at initialisation time so that subagent user
	// messages (which have a non-empty agent field) do not accidentally overwrite
	// rootAgent with a subagent name (#555). It is also written to
	// root_agent_name in the DB so that `prism prompt` can target the correct
	// agent for follow-up messages (#557).
	AgentRole string
	// AgentModel is the model identifier for the agent role (e.g.
	// "anthropic/claude-sonnet-4-6"), read from the opencode config at startup.
	// When non-empty it is seeded into root_model_id in the DB so that
	// buildPromptBody can include the model in the prompt_async body (#557).
	AgentModel string
	// InstanceID is the UUID assigned to this session incarnation by the
	// tmux-session-start event handler. When non-empty it is written as
	// to_instance_id on bus messages addressed to the coordinator, so that the
	// coordinator only receives messages intended for its current instance.
	InstanceID string
	// HTTPClient is the HTTP client used for coordinator notification delivery.
	// If nil, defaultNotifyHTTPClient is used.
	HTTPClient *http.Client
	// Container, when non-nil, enables container mode: the sidecar creates and
	// manages a podman container running opencode in combined TUI + HTTP mode
	// instead of relying on a directly-launched opencode process.
	Container *container.Config
	// HostAPISockPath, when non-empty and Container is non-nil, is the path at which
	// the sidecar starts a Unix socket HTTP server exposing host-side tmux operations
	// to agents running inside the container.
	HostAPISockPath string
	// HostAPITCPPort is the OS-allocated TCP port used on Darwin for the host-API
	// listener. It is set by Run() after binding the listener and recorded here for
	// reference. Zero means no TCP listener was started (Linux path).
	HostAPITCPPort int
	// OnReady is called (once, synchronously) after the container is healthy
	// and before the SSE loop starts. Used in container mode to write the
	// readiness signal file that unblocks the tmux pane running "podman attach".
	// No-op when nil.
	OnReady func()
	// InitialPrompt, when non-empty, is passed to the container via
	// opencode --prompt <text> at startup. The opencode process creates its
	// session with the prompt already in flight so the conversation is visible
	// in the TUI from the start. The sidecar then calls CreateSession (GET
	// /session) to discover the session ID for subsequent prism prompt delivery.
	// DeliverInitialPrompt is a no-op in container mode (RFC #691, Phase 1a).
	InitialPrompt string
	// PrismBinaryPath, when non-empty, overrides the path to the prism binary
	// used by the host-API handler to delegate operations (/spawn, /cleanup,
	// /prompt). Used in tests to inject a stub binary.
	PrismBinaryPath string
}

// Sidecar is the core event processor. It consumes events from the agent
// runtime's event stream (via the Harness interface) and drives state
// transitions in the prism DB.
type Sidecar struct {
	cfg     Config
	harness harness.Harness // agent runtime adapter; injected via Config.Harness

	mu              sync.Mutex
	lastState       agent.AgentState
	idleTimer       Timer
	recoveryTimer   Timer
	manualDenial    bool
	compacting      bool
	opencodeSID     string
	writtenMessages map[string]bool // dedup message.updated writes
	textByMessage   map[string]string
	// msgCreatedAtMs tracks the time.created timestamp (ms since epoch) for
	// in-flight assistant messages. Used to compute TTFT when the first text
	// part arrives. Keyed by message ID; entries are deleted when the message
	// is written (same lifecycle as textByMessage). Messages abandoned mid-turn
	// (e.g. opencode interrupted) are not cleaned up; this matches the existing
	// textByMessage behaviour and is acceptable for short-lived sidecar processes.
	msgCreatedAtMs map[string]float64
	// ttftByMessage tracks the computed TTFT (ms) for each assistant message,
	// set when the first text part with a time.start timestamp arrives.
	// Zero means "not yet seen" or "unavailable". Keyed by message ID.
	// Same lifecycle / leak characteristics as msgCreatedAtMs above.
	ttftByMessage map[string]int64
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
	// hostAPITCPListener is the TCP listener for the host-API HTTP server on Darwin.
	// Started in Run() before container creation so the allocated port is known before
	// the container is launched. Protected by mu.
	hostAPITCPListener net.Listener
	// hostAPITCPSrv is the HTTP server for the host-API TCP listener (Darwin only).
	// Stored so Shutdown() can drain in-flight requests gracefully. Protected by mu.
	hostAPITCPSrv *http.Server
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
	// lastTitle is the most recently upserted session title. Used when
	// pushing state-change events to the dashboard socket so the dashboard
	// can update the title display immediately without a DB round-trip.
	// Protected by s.mu.
	lastTitle string
}

// New creates a Sidecar with the given configuration.
// cfg.Harness must be non-nil; New panics if it is nil.
func New(cfg Config) *Sidecar {
	if cfg.Harness == nil {
		panic("sidecar.New: cfg.Harness must not be nil")
	}
	if cfg.Clock == nil {
		cfg.Clock = RealClock()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = defaultNotifyHTTPClient
	}
	s := &Sidecar{
		cfg:             cfg,
		harness:         cfg.Harness,
		writtenMessages: make(map[string]bool),
		textByMessage:   make(map[string]string),
		msgCreatedAtMs:  make(map[string]float64),
		ttftByMessage:   make(map[string]int64),
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
		sessionStart := time.Now()

		// On Darwin, start a TCP listener on 0.0.0.0:0 BEFORE container creation
		// so the OS-allocated port is known when the container env is configured.
		// virtiofs returns ENOTSUP on connect() for Unix sockets mounted into the
		// container VM, so TCP is used instead (#661). The listener must bind on
		// 0.0.0.0 (not 127.0.0.1) so that gvproxy's bridge interface
		// (192.168.127.254) can reach it from inside the container VM — loopback
		// is not reachable from the VM network. No --publish flag is needed:
		// the container reaches the sidecar via host.containers.internal:<port>,
		// which routes through the gvproxy bridge directly to this listener.
		//
		// Failure to bind is FATAL. A silent fallback to Unix-socket-only mode would
		// reproduce the ENOTSUP bug (#661) — the container would start without a
		// working host-API channel. Returning an error here aborts container startup
		// with a clear message rather than creating a silently broken session.
		if runtime.GOOS == "darwin" && s.cfg.HostAPISockPath != "" {
			tcpLn, tcpErr := net.Listen("tcp", "0.0.0.0:0")
			if tcpErr != nil {
				return fmt.Errorf("sidecar: host-API TCP listener: %w", tcpErr)
			}
			port := tcpLn.Addr().(*net.TCPAddr).Port
			log.Printf("sidecar: host-API TCP listener bound on 0.0.0.0:%d", port)
			s.cfg.HostAPITCPPort = port
			s.cfg.Container.HostAPITCPPort = port
			s.mu.Lock()
			s.hostAPITCPListener = tcpLn
			s.mu.Unlock()
		}

		// closeTCPListenerOnEarlyReturn closes the TCP listener (Darwin only) when
		// Run() returns an error before the SSE loop — i.e. before Shutdown() has been
		// (or will be) called by the signal handler. We use a flag rather than a
		// defer-always so that the normal success path leaves the listener open for the
		// running HTTP server (which is closed by Shutdown() later).
		tcpListenerClosed := false
		closeTCPListenerOnError := func() {
			if tcpListenerClosed {
				return
			}
			s.mu.Lock()
			ln := s.hostAPITCPListener
			s.mu.Unlock()
			if ln != nil {
				_ = ln.Close()
				tcpListenerClosed = true
			}
		}

		mgr := container.New(*s.cfg.Container)
		s.mu.Lock()
		s.container = &containerMgr{mgr: mgr}
		s.mu.Unlock()

		log.Printf("[timing] pre-Create: %s", time.Since(sessionStart).Round(time.Millisecond))
		log.Printf("sidecar: creating container %q", mgr.Name())
		t0 := time.Now()
		if err := mgr.Create(ctx); err != nil {
			closeTCPListenerOnError()
			return fmt.Errorf("sidecar: container create: %w", err)
		}
		log.Printf("[timing] Create: %s", time.Since(t0).Round(time.Millisecond))

		log.Printf("sidecar: waiting for container %q to become healthy", mgr.Name())
		t0 = time.Now()
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
				closeTCPListenerOnError()
			}
			return fmt.Errorf("sidecar: container health check: %w", err)
		}
		log.Printf("[timing] WaitHealthy: %s", time.Since(t0).Round(time.Millisecond))
		log.Printf("sidecar: container %q is healthy", mgr.Name())

		// Signal readiness after the container is healthy (AC-7, AC-19).
		// Guard with shuttingDown to prevent OnReady from firing after SIGTERM,
		// even if WaitHealthy returned a genuine 200 during podman stop's grace
		// period. Shutdown() sets shuttingDown=true before starting podman stop,
		// so this check is reliable regardless of ctx cancellation timing.
		s.mu.Lock()
		isShuttingDown := s.shuttingDown
		s.mu.Unlock()
		// Discover the session ID and deliver the initial prompt (#487).
		// In container mode (opencode --prompt "text"), the prompt was already
		// delivered via the CLI flag — opencode starts the session and begins
		// processing immediately. The sidecar still needs the session ID for
		// subsequent prism prompt follow-up delivery.
		//
		// 1. GET /session  — retrieve the session opencode already created
		//    (via --prompt on CLI). In non-container mode, POST /session creates
		//    a new session. The harness adapter handles both cases.
		// 2. Call OnReady  — unblocks the TUI pane, which runs "podman attach".
		// 3. DeliverInitialPrompt — no-op in container mode (prompt already sent
		//    via CLI). This entire block is inside `if s.cfg.Container != nil`
		//    so it only runs in container mode; host-mode sessions never reach here.
		if !isShuttingDown && s.cfg.InitialPrompt != "" {
			_, createErr := s.harness.CreateSession(ctx)
			if createErr != nil {
				log.Printf("sidecar: deliverInitialPrompt: create session: %v", createErr)
			}
			log.Printf("[timing] ready: %s from start", time.Since(sessionStart).Round(time.Millisecond))
			if !isShuttingDown && s.cfg.OnReady != nil {
				s.cfg.OnReady()
			}
			if createErr == nil {
				initialPrompt := s.cfg.InitialPrompt
				go func() {
					if err := s.harness.DeliverInitialPrompt(ctx, initialPrompt, s.cfg.AgentRole); err != nil {
						log.Printf("sidecar: deliverInitialPrompt: %v", err)
					}
					log.Printf("[timing] prompt delivered: %s from start", time.Since(sessionStart).Round(time.Millisecond))
				}()
			}
		} else if !isShuttingDown {
			log.Printf("[timing] ready: %s from start", time.Since(sessionStart).Round(time.Millisecond))
			if s.cfg.OnReady != nil {
				s.cfg.OnReady()
			}
		}
	}

	// Start the host-API servers (AC-1, AC-9).
	// This runs for BOTH podman and bwrap isolation modes — any session with
	// HostAPISockPath set needs the Unix socket so the sandboxed agent can
	// proxy prism CLI calls back to the host. Previously this block was nested
	// inside the Container!=nil branch, which meant bwrap coordinators (where
	// Container is nil) never got a socket.
	//
	// Guard with !shuttingDown: if SIGTERM arrived before we reach here,
	// Shutdown() will have already run and we must not create listeners that
	// would never be closed.
	s.mu.Lock()
	alreadyShuttingDown := s.shuttingDown
	s.mu.Unlock()
	if !alreadyShuttingDown && s.cfg.HostAPISockPath != "" {
		// Unix socket server — always started when HostAPISockPath is set,
		// regardless of platform. On Darwin the container uses TCP
		// (HostAPITCPPort), but the Unix socket is still available for
		// host-side tooling.
		_ = os.Remove(s.cfg.HostAPISockPath)
		ln, listenErr := net.Listen("unix", s.cfg.HostAPISockPath)
		if listenErr != nil {
			log.Printf("sidecar: host-API server: listen unix: %v", listenErr)
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
					log.Printf("sidecar: host-API server (unix): %v", err)
				}
			}()
		}

		// TCP server — Darwin only, when the TCP listener was bound before
		// container creation. Uses a separate http.Server with the same handler
		// so both transports serve identical API endpoints.
		s.mu.Lock()
		tcpLn := s.hostAPITCPListener
		s.mu.Unlock()
		if tcpLn != nil {
			tcpSrv := &http.Server{Handler: s.hostAPIHandler()}
			s.mu.Lock()
			s.hostAPITCPSrv = tcpSrv
			s.mu.Unlock()
			go func() {
				if err := tcpSrv.Serve(tcpLn); err != nil &&
					!errors.Is(err, http.ErrServerClosed) &&
					!errors.Is(err, net.ErrClosed) {
					log.Printf("sidecar: host-API server (tcp): %v", err)
				}
			}()
			log.Printf("sidecar: host-API TCP server serving on 0.0.0.0:%d", s.cfg.HostAPITCPPort)
		}
	}

	ch, err := s.harness.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("sidecar: connect to SSE stream: %w", err)
	}

	// opencode_sid gap detection: warn if opencode_sid stays NULL for more than
	// 30 seconds after session start. A missing opencode_sid means events from
	// this session are invisible to forensics tools (checkin, stats) because
	// they cannot be correlated to an opencode session.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		s.mu.Lock()
		sid := s.opencodeSID
		s.mu.Unlock()
		if sid == "" {
			log.Printf("[warning] opencode_sid not received after 30s — session may be invisible to forensics")
		}
	}()

	for evt := range ch {
		s.HandleEvent(evt)
	}
	return ctx.Err()
}

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
	tcpLn := s.hostAPITCPListener
	tcpSrv := s.hostAPITCPSrv
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
	// Close the TCP host-API listener/server (Darwin only). Idempotent when
	// the listener was never started (both are nil on Linux or when container
	// mode is inactive).
	if tcpSrv != nil {
		shutCtx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		_ = tcpSrv.Shutdown(shutCtx2)
	} else if tcpLn != nil {
		_ = tcpLn.Close()
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

	// Always persist the new opencode_sid unconditionally so that if the user
	// creates a new opencode session mid-conversation (e.g. via /continue or
	// TUI restart), the DB stays current. upsertState uses COALESCE for
	// opencode_sid (only overwriting when the incoming value is non-nil), which
	// is insufficient here — we need an unconditional update so that a fresh
	// session ID always replaces a stale one. (#694)
	if info.ID != "" {
		if err := s.cfg.DB.UpdateOpencodeSID(s.cfg.SessionName, info.ID); err != nil {
			log.Printf("sidecar: handleSessionCreated: UpdateOpencodeSID failed: %v", err)
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

func (s *Sidecar) handleSessionError(evt harness.HarnessEvent) {
	var payload struct {
		Properties struct {
			Error *struct {
				Name string `json:"name"`
			} `json:"error"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(evt.Data, &payload); err != nil {
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
						"tool":        "bash",
						"command":     cmd,
						"sessionName": s.cfg.SessionName,
						"opencodeSID": s.opencodeSID,
						"messageId":   part.MessageID,
					}, nil)
					log.Printf("sidecar: audit: high-impact command recorded: %s", truncate(cmd, 120))
				}
			}
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

// pushDashboardEvent connects to the persistent dashboard Unix socket and sends
// a JSON push event with the session name, new state, and title. It is
// fire-and-forget: any error (socket absent, connection refused, write failure)
// is silently discarded. Must NOT be called with s.mu held (it is called via
// goroutine from writeStateChangeWithSID).
func pushDashboardEvent(sessionName, state, title string) {
	sockPath := dashboardSocketPath()
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		// Socket absent or stale — silently ignore.
		return
	}
	defer conn.Close()

	// Set a short write deadline so we never block the caller.
	_ = conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))

	data, err := json.Marshal(map[string]string{
		"session": sessionName,
		"state":   state,
		"title":   title,
	})
	if err != nil {
		return
	}
	// Append newline so the dashboard's bufio.Scanner can read the line.
	data = append(data, '\n')
	_, _ = conn.Write(data)
}

// dashboardSocketPath returns the path to the persistent dashboard Unix socket.
// Mirrors dashboard.DashSocketPath() but avoids a package import cycle.
func dashboardSocketPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "bus", "dashboard.sock")
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

// isReviewAgentSession returns true when the session name belongs to a
// review-agent spawned by `prism review`. Review-agent sessions are named
// <parent>~review-<N>-<role> (e.g. "nixos-config@feature~review-2-review-goal")
// and are identifiable by the presence of "~review" in the session name.
//
// These sessions are short-lived internal helpers; their finish events are
// consumed by the parent worker's pollAgents DB loop and must not propagate
// further up the chain as coordinator notifications.
func isReviewAgentSession(sessionName string) bool {
	return strings.Contains(sessionName, "~review")
}

// notifyCoordinator sends a "finished" notification to the coordinator session
// for this repo. It is called asynchronously (via go) after writing
// StateFinished, so s.mu must NOT be held when this method runs.
//
// The coordinator is discovered by looking up "<repo>@main" in the DB. If the
// coordinator has an opencode_port, the notification is delivered via HTTP POST
// to /session/<sid>/prompt_async after pre-validating that the stored SID is
// present in the live session list. On confirmed delivery, an audit row is
// written via WriteBusMessageDelivered. On exhausted retries, a row is written
// via WriteBusMessageFailed and a structured error is logged.
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
// Retry policy: up to 3 POST attempts with exponential backoff (500ms, 1s).
// SID validation (GET /session) is performed before each attempt. If GET /session
// returns an empty list, delivery fails immediately (no retry). If GET /session
// fails with a network or non-200 error, the retry policy applies.
func (s *Sidecar) notifyCoordinator() {
	// Self-notification guard: if this session is the coordinator, skip.
	coordinatorName := s.cfg.Repo + "@main"
	if s.cfg.SessionName == coordinatorName {
		return
	}

	// Review-agent guard: review-agent sessions are internal to the worker's
	// prism review invocation. Their finish events are discovered by the
	// worker's pollAgents DB poll and must not be forwarded to the coordinator
	// as noise notifications.
	if isReviewAgentSession(s.cfg.SessionName) {
		log.Printf("sidecar: notifyCoordinator: suppressed for review-agent session %s", s.cfg.SessionName)
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

	// Require port for HTTP delivery.
	if coordStatus.OpencodePort == nil {
		log.Printf("sidecar: notifyCoordinator: coordinator has no opencode port — cannot deliver notification")
		return
	}
	port := *coordStatus.OpencodePort

	// storedSID is the SID currently recorded in the DB. May be stale if the
	// coordinator created a new opencode session after the last DB write.
	storedSID := ""
	if coordStatus.OpencodeSID != nil {
		storedSID = *coordStatus.OpencodeSID
	}

	const maxAttempts = 3
	// backoff[i] is the sleep duration before attempt i+2 (i.e. before the
	// 2nd and 3rd attempts). With 3 attempts, only 2 sleeps are needed.
	backoff := []time.Duration{500 * time.Millisecond, 1 * time.Second}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(backoff[attempt-2])
		}

		// Pre-delivery SID validation: call GET /session to confirm the stored
		// SID is present in the coordinator's live session list. If not, pick
		// the most recently active session and update the DB so future
		// deliveries use the fresh SID.
		targetSID, validationErr := validateOrRefreshCoordinatorSID(
			port, storedSID, coordinatorName, s.cfg.DB, s.cfg.HTTPClient,
		)
		if validationErr != nil {
			lastErr = fmt.Errorf("attempt %d: SID validation failed: %w", attempt, validationErr)
			log.Printf("sidecar: notifyCoordinator: %v (coordinator=%s)", lastErr, coordinatorName)
			// An empty session list is not a transient condition — retrying
			// will not help. Break immediately to avoid unnecessary backoff.
			if errors.Is(validationErr, errEmptySessionList) {
				break
			}
			continue
		}
		// Keep storedSID current for next iteration if it was refreshed.
		storedSID = targetSID

		log.Printf("sidecar: notifyCoordinator: attempt %d/%d POST to coordinator=%s sid=%s",
			attempt, maxAttempts, coordinatorName, targetSID)

		httpErr := deliverNotificationViaHTTP(port, targetSID, notifyText, coordStatus, s.cfg.HTTPClient)
		if httpErr != nil {
			lastErr = fmt.Errorf("attempt %d: HTTP delivery failed: %w", attempt, httpErr)
			log.Printf("sidecar: notifyCoordinator: %v (coordinator=%s sid=%s)", lastErr, coordinatorName, targetSID)
			continue
		}

		// Confirmed delivery: SID validated + POST returned 200.
		if err := s.cfg.DB.WriteBusMessageDelivered(msg); err != nil {
			log.Printf("sidecar: notifyCoordinator: write delivered audit: %v", err)
		}
		log.Printf("sidecar: notifyCoordinator: delivered to coordinator=%s sid=%s", coordinatorName, targetSID)
		return
	}

	// All retries exhausted — write failed_at and log structured error.
	if err := s.cfg.DB.WriteBusMessageFailed(msg); err != nil {
		log.Printf("sidecar: notifyCoordinator: write failed audit: %v", err)
	}
	log.Printf("sidecar: notifyCoordinator: FAILED after %d attempts — coordinator=%s sid=%s reason=%v",
		maxAttempts, coordinatorName, storedSID, lastErr)
}

// errEmptySessionList is a sentinel error returned by validateOrRefreshCoordinatorSID
// when GET /session returns an empty array. The caller treats this as a
// non-retriable condition and breaks out of the retry loop immediately.
var errEmptySessionList = errors.New("GET /session: empty session list — coordinator has no active opencode sessions")

// opencodeSessionEntry is a single entry from the opencode GET /session response.
type opencodeSessionEntry struct {
	ID   string `json:"id"`
	Time *struct {
		Updated *float64 `json:"updated"`
	} `json:"time"`
}

// validateOrRefreshCoordinatorSID calls GET /session on the coordinator's
// opencode port to retrieve the live session list, checks whether storedSID is
// present, and returns the SID to use for delivery.
//
//   - If storedSID is present in the list, it is returned as-is.
//   - If storedSID is absent, the most recently updated session ID is returned
//     and agent_status is updated with the fresh SID.
//   - If GET /session fails, an error is returned.
//   - If GET /session returns an empty list, errEmptySessionList is returned
//     (sentinel — caller should not retry).
func validateOrRefreshCoordinatorSID(port int, storedSID string, coordinatorName string, database *db.DB, httpClient *http.Client) (string, error) {
	url := fmt.Sprintf("http://localhost:%d/session", port)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("create GET /session request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET /session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET /session: http status %d", resp.StatusCode)
	}

	var sessions []opencodeSessionEntry
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return "", fmt.Errorf("decode GET /session response: %w", err)
	}

	if len(sessions) == 0 {
		// Non-retriable: an empty session list is a definitive condition, not a
		// transient failure. The caller will break immediately on this error.
		return "", errEmptySessionList
	}

	// Check if stored SID is present.
	for _, s := range sessions {
		if s.ID == storedSID {
			// Stored SID confirmed present — use it.
			return storedSID, nil
		}
	}

	// Stored SID is absent (stale). Pick the most recently updated session.
	bestSID := sessions[0].ID
	var bestUpdated float64
	for _, s := range sessions {
		if s.Time != nil && s.Time.Updated != nil && *s.Time.Updated > bestUpdated {
			bestUpdated = *s.Time.Updated
			bestSID = s.ID
		}
	}

	log.Printf("sidecar: notifyCoordinator: stored SID %q not found in coordinator session list — using most recent SID %q", storedSID, bestSID)

	// Persist the refreshed SID so future deliveries use it.
	if err := database.UpdateOpencodeSID(coordinatorName, bestSID); err != nil {
		log.Printf("sidecar: notifyCoordinator: UpdateOpencodeSID failed: %v", err)
	}

	return bestSID, nil
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
// notification prompt_async call. When root_model_id is known, it is included
// so the session continues using its root model. Falls back to model_id for
// sessions created before the root fields migration.
//
// The receiving session's active-turn agent context is deliberately NOT
// overridden: the notification is processed by whichever agent the session is
// configured to run. Setting an "agent" field here would let an incoming
// notification switch a subagent's context to the notifier's agent — a real
// bug that caused subagent context-switch incidents. See issue #848.
func buildNotifyPromptBody(text string, status *db.Status) map[string]any {
	body := map[string]any{
		"parts": []map[string]string{
			{"type": "text", "text": text},
		},
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

// ── audit helpers ────────────────────────────────────────────────────────────

// highImpactPrefixes lists the command prefixes that trigger an audit event.
// Each entry is lowercased and compared against the trimmed, lowercased command.
var highImpactPrefixes = []string{
	"gh pr merge",
	"gh pr create",
	"gh issue close",
	"git push",
	"prism spawn",
	"prism cleanup",
	"prism prompt",
}

// isHighImpactCommand reports whether cmd matches any high-impact prefix.
// Matching is case-insensitive and ignores leading whitespace.
//
// Limitation: only the first (trimmed) line of the command is considered.
// Multi-line shell scripts where a high-impact command appears after an earlier
// line (e.g. "set -e\ngh pr merge 42") will not be matched. This is an
// accepted trade-off: simple prefix matching is sufficient for the forensic
// use-case and avoids false positives from subcommand arguments that happen to
// start with a matched prefix.
func isHighImpactCommand(cmd string) bool {
	lower := strings.ToLower(strings.TrimSpace(cmd))
	for _, prefix := range highImpactPrefixes {
		if lower == prefix || strings.HasPrefix(lower, prefix+" ") || strings.HasPrefix(lower, prefix+"\t") {
			return true
		}
	}
	return false
}

// extractBashCommand extracts the "command" field from the bash tool's input.
// The input is the raw value of part.State.Input, which is a map[string]any
// after JSON unmarshalling by the SSE parser. Returns an empty string when the
// input is not a map or does not contain a "command" key with a string value.
func extractBashCommand(input any) string {
	if input == nil {
		return ""
	}
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	cmd, ok := m["command"].(string)
	if !ok {
		return ""
	}
	return cmd
}

// ── host-API handler ─────────────────────────────────────────────────────────

// extractMessageIDFromPayload returns the "messageId" field from a raw event
// payload JSON string. Returns an empty string when the field is absent or the
// JSON cannot be parsed. Used by the /checkin handler's turn-centric logic.
func extractMessageIDFromPayload(raw string) string {
	var p struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ""
	}
	return p.MessageID
}

// repoFromSession extracts the repo prefix from a session name (e.g.
// "nixos-config" from "nixos-config@main"). Returns an error when the session
// name contains no "@" — this indicates a misconfigured or non-worktree session.
func repoFromSession(sessionName string) (string, error) {
	idx := strings.Index(sessionName, "@")
	if idx < 0 {
		return "", fmt.Errorf("session name %q contains no '@' — cannot derive repo", sessionName)
	}
	return sessionName[:idx], nil
}

// isCoordinator returns true when the session name ends with "@main", which is
// the convention for coordinator sessions in the prism model.
func isCoordinator(sessionName string) bool {
	return strings.HasSuffix(sessionName, "@main")
}

// isHostAPITerminalState returns true when the agent state is a terminal state
// for the purpose of the host-API /logs follow handler.
func isHostAPITerminalState(state agent.AgentState) bool {
	return state == agent.StateFinished ||
		state == agent.StateInterrupted ||
		state == agent.StateDeleted ||
		state == agent.StateError
}

// hostAPIServeLogsTail writes the last n lines of the log file to w.
// When n == 0, the response body is empty.
func hostAPIServeLogsTail(w http.ResponseWriter, logPath string, n int) {
	if n == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	f, err := os.Open(logPath)
	if err != nil {
		http.Error(w, `{"error":"cannot open log"}`, http.StatusInternalServerError)
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, `{"error":"cannot read log"}`, http.StatusInternalServerError)
		return
	}

	content := string(data)
	lines := strings.Split(content, "\n")
	// Remove trailing empty entry produced when the file ends with a newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if n < len(lines) {
		lines = lines[len(lines)-n:]
	}

	w.WriteHeader(http.StatusOK)
	for _, line := range lines {
		_, _ = fmt.Fprintln(w, line)
	}
}

// hostAPIServeLogsFollow streams the log file to w, keeping the connection
// open until the session reaches a terminal state and 5 seconds of silence
// elapse, or the client disconnects.
func hostAPIServeLogsFollow(w http.ResponseWriter, r *http.Request, targetSession, logPath string, s *Sidecar) {
	f, err := os.Open(logPath)
	if err != nil {
		http.Error(w, `{"error":"cannot open log"}`, http.StatusInternalServerError)
		return
	}
	defer f.Close()

	flusher, canFlush := w.(http.Flusher)

	// isTerminal checks the DB for a terminal agent state.
	isTerminal := func() bool {
		st, dbErr := s.cfg.DB.CurrentStatus(targetSession)
		if dbErr != nil || st == nil {
			return false
		}
		return isHostAPITerminalState(agent.AgentState(st.State))
	}

	// If the session is already in a terminal state, send the full existing
	// log and return immediately.
	if isTerminal() {
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
		if canFlush {
			flusher.Flush()
		}
		return
	}

	// Stream the existing content first, then poll for new lines.
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
	if canFlush {
		flusher.Flush()
	}

	ctx := r.Context()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var (
		terminalDetected bool
		silenceDeadline  time.Time
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, readErr := io.Copy(w, f)
			if readErr != nil {
				return
			}
			if n > 0 && canFlush {
				flusher.Flush()
			}

			if terminalDetected {
				// Reset the silence deadline each time new content arrives.
				if n > 0 {
					silenceDeadline = time.Now().Add(5 * time.Second)
				} else if time.Now().After(silenceDeadline) {
					// 5 s of silence after terminal state: close the connection.
					return
				}
			} else {
				if isTerminal() {
					terminalDetected = true
					silenceDeadline = time.Now().Add(5 * time.Second)
				}
			}
		}
	}
}

// hostAPIHandler returns an http.Handler that exposes host-side tmux operations
// to agents running inside the container via a Unix socket. Routes:
//
//	POST /spawn        — spawn a new worktree session (coordinator only)
//	POST /review       — run review agents against a PR (workers and coordinators)
//	POST /cleanup      — clean up an existing session (coordinator only)
//	POST /switch       — switch the tmux client to a session
//	GET  /list-sessions — list active sessions (role-scoped)
//	GET  /checkin      — return conversation history for a session (coordinator only)
//	POST /prompt       — deliver a prompt to a target session (role-scoped)
//
// Role-based permissions are enforced based on s.cfg.AgentRole and
// s.cfg.SessionName. Workers have restricted access; coordinators have broader
// access. All denied requests return HTTP 403 with a structured JSON error.
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

	// requireGet writes a 405 and returns false if the method is not GET.
	// Returns true when the method is GET (caller should proceed).
	requireGet := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return false
		}
		return true
	}

	// requireCoordinator checks that the calling sidecar's AgentRole is
	// "coordinator". Returns false and writes HTTP 403 if not.
	requireCoordinator := func(w http.ResponseWriter, operation string) bool {
		if s.cfg.AgentRole != "coordinator" {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("workers cannot perform %s", operation))
			return false
		}
		return true
	}

	// prismBinary returns the path to the prism binary (this process).
	// Uses os.Executable() — consistent with StartSidecarWithOpts — to get
	// the absolute path at binary launch time, avoiding CWD-relative resolution.
	// When Config.PrismBinaryPath is set (e.g. in tests), it is used instead.
	prismBinary := func() string {
		if s.cfg.PrismBinaryPath != "" {
			return s.cfg.PrismBinaryPath
		}
		self, err := os.Executable()
		if err != nil {
			return os.Args[0]
		}
		return self
	}

	// GET /list-sessions
	// Query param: all=true (optional, coordinator only)
	// Response: JSON array of session status objects
	mux.HandleFunc("/list-sessions", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}

		showAll := r.URL.Query().Get("all") == "true"

		// Workers cannot use all=true.
		if showAll && s.cfg.AgentRole != "coordinator" {
			writeError(w, http.StatusForbidden, "workers cannot list sessions across all repos (all=true requires coordinator role)")
			return
		}

		var (
			sessions []db.Status
			err      error
		)
		if showAll {
			sessions, err = s.cfg.DB.AllActiveStatus()
		} else {
			// Scope to own repo by default.
			ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
			if repoErr != nil {
				writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
				return
			}
			sessions, err = s.cfg.DB.AllActiveStatusForRepo(ownRepo)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
			return
		}

		// Return empty array rather than null when no sessions found.
		if sessions == nil {
			sessions = []db.Status{}
		}
		writeJSON(w, http.StatusOK, sessions)
	})

	// GET /checkin
	// Query params: session (required), last (default 10), types (optional),
	//               from (optional cursor), before (optional cursor)
	// Permission: coordinator only; own repo sessions or cross-repo @main sessions.
	mux.HandleFunc("/checkin", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		if !requireCoordinator(w, "checkin") {
			return
		}

		q := r.URL.Query()
		targetSession := q.Get("session")
		if targetSession == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}

		// Permission check: coordinator can access own-repo sessions and
		// any cross-repo coordinator (@main) session.
		ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
		if repoErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
			return
		}
		targetRepo, targetRepoErr := repoFromSession(targetSession)
		if targetRepoErr != nil {
			writeError(w, http.StatusBadRequest, "invalid target session name: "+targetRepoErr.Error())
			return
		}
		crossRepo := targetRepo != ownRepo
		if crossRepo && !isCoordinator(targetSession) {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("cross-repo checkin can only target coordinators (<repo>@main), got %q", targetSession))
			return
		}

		// Parse limit (default 10).
		limit := 10
		if lastStr := q.Get("last"); lastStr != "" {
			if n, parseErr := strconv.Atoi(lastStr); parseErr == nil && n > 0 {
				limit = n
			}
		}

		// Parse optional cursor params.
		var afterPtr, beforePtr *string
		if fromStr := q.Get("from"); fromStr != "" {
			afterPtr = &fromStr
		}
		if beforeStr := q.Get("before"); beforeStr != "" {
			beforePtr = &beforeStr
		}

		// Parse optional types filter.
		var types []string
		if typesStr := q.Get("types"); typesStr != "" {
			for _, t := range strings.Split(typesStr, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					types = append(types, t)
				}
			}
		}

		// Fetch session state.
		status, statusErr := s.cfg.DB.CurrentStatus(targetSession)
		var state string
		if statusErr == nil && status != nil {
			state = status.State
		}

		var events []db.Event
		if len(types) > 0 {
			// Explicit --types: return raw events with the type filter, same as
			// the runCheckinSessionRaw path in the CLI.
			var err error
			events, err = s.cfg.DB.QueryEvents(targetSession, limit, beforePtr, afterPtr, types)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
				return
			}
		} else {
			// Default (no --types): replicate the assistant-turn-centric logic from
			// runCheckinSession / renderCheckinTurns so that --last N means N
			// assistant turns, not N raw events.

			// Primary query: fetch last N msg_assistant events.
			assistantEvents, err := s.cfg.DB.QueryEvents(targetSession, limit, beforePtr, afterPtr, []string{"msg_assistant"})
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error: "+err.Error())
				return
			}

			if len(assistantEvents) > 0 {
				// Collect all messageIds from the assistant events.
				messageIDs := make([]string, 0, len(assistantEvents))
				for _, e := range assistantEvents {
					msgID := extractMessageIDFromPayload(e.Payload)
					if msgID != "" {
						messageIDs = append(messageIDs, msgID)
					}
				}

				// Secondary query: fetch tool calls, results, permission events, and
				// thinking events that share a messageId with one of the assistant events.
				childTypes := []string{"tool_call", "tool_result", "permission_ask", "permission_denied", "thinking"}
				childEvents, _ := s.cfg.DB.QueryEventsByMessageIDs(targetSession, messageIDs, childTypes)

				// Determine the time window for msg_user events.
				earliest := assistantEvents[0].CreatedAt
				latest := assistantEvents[len(assistantEvents)-1].CreatedAt
				for _, ae := range assistantEvents {
					if ae.CreatedAt.Before(earliest) {
						earliest = ae.CreatedAt
					}
					if ae.CreatedAt.After(latest) {
						latest = ae.CreatedAt
					}
				}

				// Fetch msg_user events and filter to the time window.
				allUserEvents, _ := s.cfg.DB.QueryEvents(targetSession, 0, nil, nil, []string{"msg_user"})
				var userEvents []db.Event
				for _, ue := range allUserEvents {
					if !ue.CreatedAt.Before(earliest) && !ue.CreatedAt.After(latest) {
						userEvents = append(userEvents, ue)
					}
				}

				// Merge all into a single sorted timeline (insertion sort, ASC).
				merged := make([]db.Event, 0, len(assistantEvents)+len(childEvents)+len(userEvents))
				merged = append(merged, assistantEvents...)
				merged = append(merged, childEvents...)
				merged = append(merged, userEvents...)
				for i := 1; i < len(merged); i++ {
					for j := i; j > 0 && merged[j].CreatedAt.Before(merged[j-1].CreatedAt); j-- {
						merged[j], merged[j-1] = merged[j-1], merged[j]
					}
				}
				events = merged
			}
			// If no assistant events exist, events stays nil → returned as [].
		}

		// Ensure empty arrays rather than null.
		if events == nil {
			events = []db.Event{}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"session": targetSession,
			"state":   state,
			"events":  events,
		})
	})

	// GET /logs
	// Query params: session (required), tail (optional int ≥ 0), follow (optional bool)
	// Permission: coordinator only; own-repo sessions or cross-repo @main sessions.
	// Returns 404 with JSON error when the log file does not exist.
	// When follow=true, streams new lines and closes after the session reaches a
	// terminal state and 5 s of silence elapse.
	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		if !requireGet(w, r) {
			return
		}
		if !requireCoordinator(w, "logs") {
			return
		}

		q := r.URL.Query()
		targetSession := q.Get("session")
		if targetSession == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}

		// Validate session name format before semantic permission checks.
		// Valid session names are "repo@branch" — slashes and dot-segments are
		// not valid and would escape the logs directory via filepath.Join.
		if strings.Contains(targetSession, "/") || strings.Contains(targetSession, "..") {
			writeError(w, http.StatusBadRequest, "invalid session name: must not contain '/' or '..'")
			return
		}

		// Permission check: own-repo sessions or cross-repo @main sessions.
		ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
		if repoErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
			return
		}
		targetRepo, targetRepoErr := repoFromSession(targetSession)
		if targetRepoErr != nil {
			writeError(w, http.StatusBadRequest, "invalid target session name: "+targetRepoErr.Error())
			return
		}
		if targetRepo != ownRepo && !isCoordinator(targetSession) {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("cross-repo logs can only target coordinators (<repo>@main), got %q", targetSession))
			return
		}

		// Resolve log file path.
		logPath, pathErr := session.SidecarLogPath(targetSession)
		if pathErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot resolve log path: "+pathErr.Error())
			return
		}
		if _, statErr := os.Stat(logPath); os.IsNotExist(statErr) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no log file for session %s", targetSession))
			return
		}

		// Parse optional tail param (non-negative integer).
		tailN := 0
		tailSet := false
		if tailStr := q.Get("tail"); tailStr != "" {
			if n, parseErr := strconv.Atoi(tailStr); parseErr == nil && n >= 0 {
				tailN = n
				tailSet = true
			}
		}

		follow := q.Get("follow") == "true"

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		if follow {
			hostAPIServeLogsFollow(w, r, targetSession, logPath, s)
			return
		}

		if tailSet {
			hostAPIServeLogsTail(w, logPath, tailN)
			return
		}

		// Full log: stream the whole file.
		f, openErr := os.Open(logPath)
		if openErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot open log: "+openErr.Error())
			return
		}
		defer f.Close()
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
	})

	// POST /prompt
	// Request:  {"session":"<target>", "prompt":"<text>"}
	// Permission: worker → own coordinator (@main) only;
	//             coordinator → own repo any session, cross-repo coordinator only.
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}

		var req struct {
			Session string `json:"session"`
			Prompt  string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.Session == "" {
			writeError(w, http.StatusBadRequest, "session is required")
			return
		}
		if req.Prompt == "" {
			writeError(w, http.StatusBadRequest, "prompt is required")
			return
		}

		ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
		if repoErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
			return
		}

		targetRepo, targetRepoErr := repoFromSession(req.Session)
		if targetRepoErr != nil {
			writeError(w, http.StatusBadRequest, "invalid target session name: "+targetRepoErr.Error())
			return
		}
		crossRepo := targetRepo != ownRepo

		if s.cfg.AgentRole == "coordinator" {
			// Coordinator: own repo any session allowed; cross-repo only @main.
			if crossRepo && !isCoordinator(req.Session) {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("cross-repo prompts can only target coordinators (<repo>@main), got %q", req.Session))
				return
			}
		} else {
			// Worker: only own coordinator (@main) allowed.
			ownCoordinator := ownRepo + "@main"
			if req.Session != ownCoordinator {
				writeError(w, http.StatusForbidden,
					fmt.Sprintf("workers can only prompt their own coordinator (%s), got %q", ownCoordinator, req.Session))
				return
			}
		}

		// Deliver via prism prompt on the host.
		args := []string{"prompt", req.Session, "--prompt", req.Prompt}
		log.Printf("sidecar: host-API /prompt: prism prompt %s <omitted>", req.Session)
		cmd := exec.Command(prismBinary(), args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("sidecar: host-API /prompt: %v: %s", err, out)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("prompt delivery failed: %v", err))
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{})
	})

	// POST /spawn
	// Request:  {"branch":"my-feature","prompt":"...","agent":"worker","profile":"gemini-hybrid","host_mode":false,"harness":"opencode"}
	// The "repo" field is accepted but ignored — the sidecar always substitutes
	// its own repo (derived from its session name) so that a client sending a
	// mount-path name (e.g. "prism-git") still spawns into the correct repo
	// (e.g. "nixos-config"). See issue #616.
	// Response: {"session_name":"nixos-config@my-feature"} | {"error":"..."}
	mux.HandleFunc("/spawn", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		if !requireCoordinator(w, "spawn") {
			return
		}
		var req struct {
			Repo     string `json:"repo"` // accepted but ignored — ownRepo is always used
			Branch   string `json:"branch"`
			Prompt   string `json:"prompt"`
			Agent    string `json:"agent"`
			Profile  string `json:"profile"`
			Model    string `json:"model"`
			Variant  string `json:"variant"`
			HostMode bool   `json:"host_mode"`
			Harness  string `json:"harness"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.Branch == "" {
			writeError(w, http.StatusBadRequest, "branch is required")
			return
		}
		// Validate harness before spawning. Default empty string to "opencode"
		// for backwards compatibility with clients that don't send the field.
		if req.Harness == "" {
			req.Harness = "opencode"
		}
		if req.Harness != "opencode" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown harness %q: only 'opencode' is supported in this version of prism", req.Harness))
			return
		}

		// Always derive the repo from the sidecar's own session name.
		// This means a client that sends the wrong repo (e.g. a container
		// mount-path name instead of the actual repo name) is silently
		// corrected. The own-repo restriction is enforced implicitly: the
		// sidecar can only spawn into its own repo.
		ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
		if repoErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
			return
		}

		args := []string{"spawn", "--branch", req.Branch}
		if req.Prompt != "" {
			args = append(args, "--prompt", req.Prompt)
		}
		if req.Agent != "" {
			args = append(args, "--agent", req.Agent)
		}
		if req.Profile != "" {
			args = append(args, "--profile", req.Profile)
		}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.Variant != "" {
			args = append(args, "--variant", req.Variant)
		}
		if req.HostMode {
			args = append(args, "--host-mode")
		}
		args = append(args, "--harness", req.Harness)
		args = append(args, "--repo", ownRepo)

		// Log without the prompt value — it may contain sensitive context.
		logArgs := []string{"spawn", "--branch", req.Branch}
		if req.Prompt != "" {
			logArgs = append(logArgs, "--prompt", "<omitted>")
		}
		if req.Agent != "" {
			logArgs = append(logArgs, "--agent", req.Agent)
		}
		if req.Profile != "" {
			logArgs = append(logArgs, "--profile", req.Profile)
		}
		if req.Model != "" {
			logArgs = append(logArgs, "--model", req.Model)
		}
		if req.Variant != "" {
			logArgs = append(logArgs, "--variant", req.Variant)
		}
		if req.HostMode {
			logArgs = append(logArgs, "--host-mode")
		}
		logArgs = append(logArgs, "--harness", req.Harness)
		logArgs = append(logArgs, "--repo", ownRepo)
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
			// Fallback: derive from ownRepo@branch (branch already sanitised by spawn).
			sessionName = ownRepo + "@" + req.Branch
		}
		writeJSON(w, http.StatusOK, map[string]string{"session_name": sessionName})
	})

	// POST /review
	// Request:  {"pr_number":"123","agents":["review-code","review-goal"],"timeout":"10m"}
	// pr_number must be a numeric string (e.g. "123"). Non-numeric values are rejected.
	// agents is optional (empty = full set resolved by prism review on host).
	// timeout is optional (default: 10m).
	// Response: {"output":"...","passed":true} | {"error":"..."}
	//
	// This endpoint is called by workers and coordinators running inside
	// containers that cannot reach tmux directly. The sidecar runs on the host
	// where tmux is available, so it delegates to `prism review` on the host.
	// Both worker and coordinator role sidecars are permitted: workers call
	// `prism review` as part of their own PR workflow.
	//
	// PRISM_SESSION_NAME is injected into the subprocess environment so that
	// `review.LookupParentSession()` can determine the parent session name
	// (the sidecar daemon process does not run inside tmux, so the fallback
	// tmux.CurrentSession() call would fail).
	mux.HandleFunc("/review", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		var req struct {
			PRNumber string   `json:"pr_number"`
			Agents   []string `json:"agents"`
			Timeout  string   `json:"timeout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.PRNumber == "" {
			writeError(w, http.StatusBadRequest, "pr_number is required")
			return
		}
		// Validate pr_number is numeric to prevent flag injection into the
		// subprocess (e.g. "--keep" being interpreted as a cobra flag).
		for _, c := range req.PRNumber {
			if c < '0' || c > '9' {
				writeError(w, http.StatusBadRequest, "pr_number must be a numeric string (e.g. \"123\")")
				return
			}
		}

		// Validate each agent name against the known set to prevent flag
		// injection via the --only argument.
		for _, name := range req.Agents {
			if !isKnownReviewAgent(name) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown agent name %q — must be one of: %s",
					name, strings.Join(knownReviewAgentNames(), ", ")))
				return
			}
		}

		args := []string{"review", req.PRNumber}
		if len(req.Agents) > 0 {
			args = append(args, "--only", strings.Join(req.Agents, ","))
		}
		if req.Timeout != "" {
			args = append(args, "--timeout", req.Timeout)
		}

		log.Printf("sidecar: host-API /review: prism %s", strings.Join(args, " "))

		// Parse the timeout to determine an appropriate exec deadline.
		// Default to 10 minutes per agent; add 2 minutes of overhead.
		// We must not cut off the prism review process before its own timeout fires.
		execTimeout := 12 * time.Minute
		if req.Timeout != "" {
			if d, parseErr := time.ParseDuration(req.Timeout); parseErr == nil {
				// Add 2 minutes overhead so the HTTP request outlives the review timeout.
				execTimeout = d + 2*time.Minute
			}
		}

		// Use a context with deadline so hung review processes don't block forever.
		ctx, cancel := context.WithTimeout(r.Context(), execTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, prismBinary(), args...)

		// Build the subprocess environment:
		// PRISM_SESSION_NAME: so review.LookupParentSession() resolves the
		// parent session correctly. The sidecar daemon is not inside tmux,
		// so the fallback tmux.CurrentSession() would fail without this.
		env := append(os.Environ(), "PRISM_SESSION_NAME="+s.cfg.SessionName)
		cmd.Env = env

		// Capture stdout (formatted results); stderr is logged server-side only.
		out, err := cmd.Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				// Log stderr for diagnostics but do not include it in the HTTP
				// response to avoid leaking internal paths or credentials to
				// the container caller.
				if len(exitErr.Stderr) > 0 {
					log.Printf("sidecar: host-API /review: stderr: %s", strings.TrimSpace(string(exitErr.Stderr)))
				}
			}
		}

		// prism review exits non-zero when one or more agents fail (exit 1).
		// This is expected and not an infrastructure error — return the output
		// with passed=false. Only treat it as a hard error if there is no output.
		passed := err == nil
		output := strings.TrimRight(string(out), "\n")

		if output == "" && err != nil {
			// No output produced — infrastructure failure. Log error server-side
			// and return a generic message to avoid leaking details to the caller.
			log.Printf("sidecar: host-API /review: review process failed: %v", err)
			writeError(w, http.StatusInternalServerError, "review process failed — check host logs for details")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"output": output,
			"passed": passed,
		})
	})

	// POST /cleanup
	// Request:  {"session":"nixos-config@my-feature","yes":true}
	// Response: {} | {"error":"..."}
	mux.HandleFunc("/cleanup", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		if !requireCoordinator(w, "cleanup") {
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

		// Own-repo restriction: coordinators may only clean up sessions in their own repo.
		ownRepo, repoErr := repoFromSession(s.cfg.SessionName)
		if repoErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot derive repo from session name: "+repoErr.Error())
			return
		}
		targetRepo, targetRepoErr := repoFromSession(req.Session)
		if targetRepoErr != nil {
			writeError(w, http.StatusBadRequest, "invalid target session name: "+targetRepoErr.Error())
			return
		}
		if targetRepo != ownRepo {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("coordinators can only clean up sessions in their own repo (%s), got %q", ownRepo, req.Session))
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

// reviewAgentAllowlist is the set of valid review agent names accepted by the
// /review host-API endpoint. These match the names in review.Agents().
// New agents must be added here when introduced.
// Keeping this inline avoids importing the review package from the sidecar.
var reviewAgentAllowlist = map[string]bool{
	"review-goal":     true,
	"review-code":     true,
	"review-security": true,
	"review-qa":       true,
	"review-context":  true,
}

// isKnownReviewAgent returns true if name is a recognised review agent.
func isKnownReviewAgent(name string) bool {
	return reviewAgentAllowlist[name]
}

// knownReviewAgentNames returns the sorted list of known review agent names
// for use in error messages.
func knownReviewAgentNames() []string {
	names := make([]string, 0, len(reviewAgentAllowlist))
	for name := range reviewAgentAllowlist {
		names = append(names, name)
	}
	// Sort for stable error messages.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[i] > names[j] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
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
