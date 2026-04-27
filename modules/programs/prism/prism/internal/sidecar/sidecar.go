// Package sidecar implements the core logic for the prism sidecar process.
//
// The sidecar subscribes to the agent runtime's event stream (via a
// harness.Harness adapter) and translates events into agent state transitions,
// DB writes, and dashboard sentinel touches — replicating the logic in
// opencode/plugins/prism-hooks.ts.
//
// Host-API socket path budget. The Unix socket the sidecar binds for the
// host-API server (Config.HostAPISockPath) must fit the kernel's sun_path
// limit: 108 bytes on Linux, 104 bytes on Darwin. We treat 104 as the budget
// for both platforms so the same code path works everywhere. The path is
// constructed by session.SidecarHostAPIPath and uses a 12-hex-char SHA-256
// prefix of the session name as the per-session directory — see #1050 for the
// path arithmetic and the regression tests in
// internal/session/sidecar_test.go (TestSidecarHostAPIPath_LengthInvariant_*).
// If a future change reverts that to a long-form session name, those tests
// will fail with a clear message before the bug ships.
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
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/opencode"
	"github.com/prismatic-koi/prism/internal/mergequeue"
)

// defaultNotifyHTTPClient is the HTTP client used for coordinator notification
// delivery when Config.HTTPClient is nil.
var defaultNotifyHTTPClient = &http.Client{Timeout: 10 * time.Second}

// IdleDebounce is the duration to wait after session.idle before committing
// the finished state. Cancelled if session.status busy fires in the window.
const IdleDebounce = 2 * time.Second

// DefaultStartupConnectTimeout is the default duration that bwrap-mode sidecars
// will wait for the first SSE event from the harness. If no event is received
// within this window, the session is transitioned to StateError via
// writeStartupError. This mirrors the WaitHealthy/CreateSession timeout
// mechanism added in #1011 for the podman path.
const DefaultStartupConnectTimeout = 5 * time.Minute

// ReconnectRecoveryDelay is the window the sidecar waits after a reconnect
// (detected via server.connected while in active state) before concluding
// that session.idle was missed and writing the finished state. Any arriving
// session.status busy or session.idle event resets normal flow and cancels
// this timer.
const ReconnectRecoveryDelay = 60 * time.Second

// ErrorResumeDebounce is the window after a non-MessageAbortedError session.error
// during which a session.updated event is treated as post-error churn and does NOT
// transition the session from error to active. After this window, a genuine
// user-initiated resume (session.updated) transitions normally.
//
// The opencode runtime emits a rapid burst of events in the same millisecond
// after an error (session.error → session.status → session.updated → session.idle),
// and the session.updated is internal housekeeping, not a user action. Without
// this guard, the false resume would erase the error state and the coordinator
// would see a spurious "finished" notification.
const ErrorResumeDebounce = 5 * time.Second

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
	// HarnessName is the registered harness name (e.g. "opencode"). When
	// non-empty, Run consults harness.ShapeOf(HarnessName) at the top of the
	// function to determine the transport shape and route to the appropriate
	// startup helper (runStartupHTTP or runStartupStdio). When empty, Run
	// defaults to TransportHTTPPort behaviour for back-compat.
	HarnessName string
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
	// StartupConnectTimeout is the duration the sidecar waits for the first
	// SSE event before concluding the harness never bound to its port and
	// transitioning to StateError via writeStartupError. Only applies to
	// bwrap mode (Container == nil): podman mode uses WaitHealthy/CreateSession
	// for the same protection. Defaults to DefaultStartupConnectTimeout (5m)
	// when zero. Set to a small value in tests to exercise the timeout path
	// without real wall-clock waits.
	StartupConnectTimeout time.Duration
}

// seenUnknownCap is the maximum number of unique unknown event types tracked
// in seenUnknown before the cap-reached message fires and tracking stops.
const seenUnknownCap = 50

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
	// lastErrorAt records the time when handleSessionError last wrote StateError
	// for a non-MessageAbortedError. Used by handleSessionUpdated to suppress
	// false resumes caused by the post-error session.updated churn that opencode
	// emits in the same millisecond as session.error. Protected by s.mu.
	lastErrorAt time.Time
	// spawnTime records when the sidecar Run() began; used to compute elapsed
	// time for the "first event received" log line (Gap 1b).
	// Set at the top of Run(), read under s.mu in HandleEvent.
	spawnTime time.Time
	// firstEventLogged is set to true after the "first event received" log
	// line has been emitted. Prevents duplicate lines on reconnect.
	// Protected by s.mu.
	firstEventLogged bool
	// seenUnknown tracks event types that have hit the default switch case
	// and been logged once. Capped at seenUnknownCap entries. Protected by s.mu.
	seenUnknown map[string]bool
	// seenUnknownCapReached is set to true when seenUnknown reaches seenUnknownCap.
	// Prevents repeated "cap reached" log lines. Protected by s.mu.
	seenUnknownCapReached bool

	// mergeWatcherCancel cancels the merge-watcher goroutine context. Set in
	// Run() when this is a coordinator session (AgentRole == "coordinator").
	// Protected by mu; nil when no watcher is running.
	mergeWatcherCancel context.CancelFunc
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
		seenUnknown:     make(map[string]bool),
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
	// Record spawn time for "first event received" log line (Gap 1b).
	s.mu.Lock()
	s.spawnTime = time.Now()
	s.mu.Unlock()

	// B.3 Stage 1: transport-shape gate. Consult the harness registry to
	// determine the wire-level shape and dispatch to the appropriate startup
	// helper. The back half of Run (host-API, merge-queue watcher, SSE loop,
	// shutdown) is shape-agnostic and is unchanged.
	//
	// When HarnessName is empty (back-compat: callers that pre-date this field)
	// the registry lookup is skipped and we fall through to runStartupHTTP,
	// preserving today's behaviour for all existing sessions.
	if s.cfg.HarnessName != "" {
		shape, ok := harness.ShapeOf(s.cfg.HarnessName)
		if !ok {
			return fmt.Errorf("sidecar: unknown harness %q (not registered)", s.cfg.HarnessName)
		}
		switch shape {
		case harness.TransportHTTPPort:
			if err := s.runStartupHTTP(ctx); err != nil {
				return err
			}
		case harness.TransportStdioPipe:
			return s.runStartupStdio(ctx)
		default:
			return fmt.Errorf("sidecar: unsupported transport shape %q for harness %q", shape, s.cfg.HarnessName)
		}
	} else {
		// HarnessName is empty: preserve legacy behaviour (HTTP-port startup).
		if err := s.runStartupHTTP(ctx); err != nil {
			return err
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
		// For bwrap mode, the per-session socket directory may not yet exist —
		// prepareVolumeDirs runs in agent-run (after the sidecar starts). Create it
		// here so net.Listen succeeds. For podman, prepareVolumeDirs already ran
		// inside mgr.Create(), so this is a no-op.
		if err := os.MkdirAll(filepath.Dir(s.cfg.HostAPISockPath), 0o700); err != nil {
			log.Printf("sidecar: host-API socket dir: %v", err)
		}
		_ = os.Remove(s.cfg.HostAPISockPath)
		ln, listenErr := net.Listen("unix", s.cfg.HostAPISockPath)
		if listenErr != nil {
			// Failure to bind is FATAL (#1050). Continuing without a socket
			// produces a partially-functional agent: bwrap exec proceeds with
			// PRISM_HOST_API set but nothing listening, so anything inside the
			// container that hits the host-API channel hangs or fails in a
			// downstream way that prevents opencode from ever binding its TCP
			// port. The agent then appears as "idle" with no title, the SSE
			// loop retries forever, and the user has no clear signal that
			// anything is wrong. Returning the error here surfaces it via the
			// sidecar log and exits with a non-zero status so the operator
			// sees the failure immediately.
			log.Printf("sidecar: host-API server: listen unix: %v", listenErr)
			return fmt.Errorf("sidecar: host-API socket bind failed for %q: %w", s.cfg.HostAPISockPath, listenErr)
		}
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

	// Start the merge-queue watcher for coordinator sessions. The watcher polls
	// the pending_merges head on a 45s ticker and drives PRs through the merge
	// lifecycle. It is started only when:
	//   - AgentRole is "coordinator" (explicit), OR
	//   - SessionName ends with "@main" (legacy heuristic).
	// The watcher context is stored so Shutdown() can cancel it before the
	// abandon-watching-rows SQL runs (preventing a race where the watcher fires
	// a terminal transition after AbandonWatchingMerges clears the rows).
	if s.cfg.AgentRole == "coordinator" || isCoordinatorSession(s.cfg.SessionName, s.cfg.DB) {
		if s.cfg.InstanceID != "" {
			watcherCtx, watcherCancel := context.WithCancel(ctx)
			s.mu.Lock()
			s.mergeWatcherCancel = watcherCancel
			s.mu.Unlock()
			watcher := mergequeue.New(s.cfg.DB, s.cfg.InstanceID, s.cfg.SessionName, s.cfg.HTTPClient)
			go watcher.Run(watcherCtx)
			log.Printf("sidecar: merge-queue watcher started (instance=%s)", s.cfg.InstanceID)
		} else {
			log.Printf("sidecar: merge-queue watcher NOT started — no instance_id")
		}
	}

	// Wrap ctx with a cancel so the startup-timeout goroutine (bwrap mode only)
	// can stop the SSE loop by cancelling the derived context. The outer ctx
	// cancellation still propagates normally.
	sseCtx, sseCancel := context.WithCancel(ctx)
	defer sseCancel()

	ch, err := s.harness.Subscribe(sseCtx)
	if err != nil {
		return fmt.Errorf("sidecar: connect to SSE stream: %w", err)
	}

	// opencode_sid gap detection: warn if opencode_sid stays NULL for more than
	// 30 seconds after session start. A missing opencode_sid means events from
	// this session are invisible to forensics tools (checkin, stats) because
	// they cannot be correlated to an opencode session.
	go func() {
		select {
		case <-sseCtx.Done():
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

	// Startup-connect timeout: bwrap mode only.
	//
	// In bwrap mode the harness is launched by agent-run from a tmux pane, not
	// by the sidecar. The sidecar's only signal of harness liveness is whether
	// SSE connects. If the harness never binds to its port the SSE retry loop
	// runs forever, leaving the session stuck at "idle".
	//
	// After startupConnectTimeout with no first event, transition the session
	// to StateError (same mechanism as #1011's WaitHealthy/CreateSession paths)
	// and cancel the SSE loop so the sidecar exits cleanly.
	//
	// Container != nil means podman mode, which already has WaitHealthy/
	// CreateSession covering startup failures — skip the timeout there.
	if s.cfg.Container == nil {
		startupTimeout := s.cfg.StartupConnectTimeout
		if startupTimeout == 0 {
			startupTimeout = DefaultStartupConnectTimeout
		}
		url := s.cfg.OpencodeURL
		go func() {
			select {
			case <-sseCtx.Done():
				return
			case <-time.After(startupTimeout):
			}

			// Check whether the first SSE event has been received. If it has,
			// the harness is alive and this is a post-connect reconnect scenario
			// — the existing reconnect loop handles it; do nothing.
			s.mu.Lock()
			firstEventReceived := s.firstEventLogged
			alreadyShuttingDown := s.shuttingDown
			s.mu.Unlock()

			if firstEventReceived || alreadyShuttingDown {
				// Harness connected successfully, or sidecar is already shutting
				// down (SIGTERM). No startup timeout action needed.
				return
			}

			// The harness never bound to its port within the timeout window.
			// Write StateError and cancel the SSE context to stop the retry loop.
			startupErr := fmt.Errorf("bwrap harness for %s never bound to %s within %v",
				s.cfg.SessionName, url, startupTimeout)
			log.Printf("sidecar: startup-connect timeout fired: %v", startupErr)
			// Emit a `[timing] opencode listening` line recording the timeout
			// duration so the grep-the-log workflow yields a coherent timeline
			// even on the failure path (#1052 AC: "When opencode never reaches
			// the listening state and the sidecar times out, the timing line
			// emitted records the timeout duration, not silence."). The
			// "(timed out)" suffix distinguishes failure from a real listening
			// marker without changing the leading prefix that grep targets.
			log.Printf("[timing] opencode listening: %s from start (timed out)",
				time.Since(s.spawnTime).Round(time.Millisecond))
			s.writeStartupError(startupErr)
			// writeStartupError notifies the parent worker for review-agent sessions.
			// For non-review-agent (worker) sessions, also notify the coordinator so
			// the coordinator learns the worker is dead — symmetric to the finished
			// notification path (notifyCoordinator is suppressed for review agents
			// and self-notifications internally). This satisfies the routing requirement
			// from #1022: "worker agents notify the coordinator."
			if !isReviewAgentSession(s.cfg.SessionName, s.cfg.DB) {
				go s.notifyCoordinator()
			}
			sseCancel()
		}()
	}

	for evt := range ch {
		s.HandleEvent(evt)
	}
	return ctx.Err()
}

// Shutdown writes the interrupted state if the session is not already in a
// terminal state, cancels any pending idle timer, and stops/removes the
// container (if running in container mode). Called on SIGINT/SIGTERM.
//
// For coordinator sessions, Shutdown also cancels the merge-queue watcher and
// transitions all watching pending_merges rows to 'abandoned' so that the next
// coordinator session starts clean.
func (s *Sidecar) Shutdown() {
	s.mu.Lock()
	// Mark shutdown before releasing the lock so that Run()'s OnReady guard
	// sees shuttingDown=true even if it races with Shutdown() (AC-16).
	s.shuttingDown = true
	ctr := s.container
	watcherCancel := s.mergeWatcherCancel
	s.mergeWatcherCancel = nil
	s.mu.Unlock()

	// Cancel the merge-queue watcher first (before the SQL below) to prevent
	// a race where the watcher fires a state transition after the abandon SQL.
	if watcherCancel != nil {
		watcherCancel()
	}

	// Abandon all watching merge rows for this coordinator incarnation.
	if s.cfg.InstanceID != "" {
		if err := s.cfg.DB.AbandonWatchingMerges(s.cfg.InstanceID); err != nil {
			log.Printf("sidecar: AbandonWatchingMerges: %v", err)
		} else {
			log.Printf("sidecar: watching merge rows abandoned (instance=%s)", s.cfg.InstanceID)
		}
	}

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
		// Also remove the per-session socket directory introduced by security
		// fix #960. os.Remove only succeeds when the directory is empty (which
		// it will be after the socket file is removed), so this is safe.
		_ = os.Remove(filepath.Dir(s.cfg.HostAPISockPath))
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
		s.lastState != agent.StateInterrupted &&
		s.lastState != agent.StateError {
		// Note: StateError is excluded because writeStartupError may have already
		// written it before Run() returned on the startup-failure path. Overwriting
		// error with interrupted here would clobber the correct terminal state.
		log.Printf("sidecar: transition -> interrupted (cause=sigterm)")
		s.upsertState(agent.StateInterrupted, nil, nil)
		s.writeStateChange(agent.StateInterrupted)
	}
}

// runStartupHTTP runs the HTTP-port startup sequence for TransportHTTPPort
// harnesses (e.g. opencode). It is called from Run when the harness transport
// shape is TransportHTTPPort (or when HarnessName is empty for back-compat).
//
// When Config.Container is nil (bwrap / host mode), this function is a no-op:
// the harness is already running and the SSE loop connects directly. The
// container startup sequence (mgr.Create → WaitHealthy → CreateSession →
// OnReady → DeliverInitialPrompt) is only performed in podman container mode.
func (s *Sidecar) runStartupHTTP(ctx context.Context) error {
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
			// but we check shuttingDown to distinguish a SIGTERM-cancelled WaitHealthy
			// (Shutdown already ran) from a genuine probe timeout (no Shutdown yet),
			// avoiding a double-Shutdown with spurious log lines.
			//
			// shuttingDown is the reliable SIGTERM gate: Shutdown() sets it before
			// calling ctr.mgr.Shutdown(), so even when the container exits early
			// (WaitHealthy returns via hasExited before cancel fires), shuttingDown
			// is already true. ctx.Err() is set only after Shutdown() returns, which
			// is too late.
			s.mu.Lock()
			alreadyShutdown := s.shuttingDown
			s.mu.Unlock()
			startupErr := fmt.Errorf("sidecar: container health check: %w", err)
			if !alreadyShutdown {
				// Genuine probe timeout (not SIGTERM): clean up the container
				// ourselves (Shutdown() has not run yet) and write StateError
				// directly so the DB row is in the correct terminal state.
				// On SIGTERM, Shutdown() handles cleanup and writes StateInterrupted.
				mgr.Shutdown()
				closeTCPListenerOnError()
				// Gap 1 fix: write StateError directly so the DB row transitions
				// to "error" (not relying on the fragile pane-died tmux hook).
				// This is skipped on SIGTERM to avoid racing with Shutdown().
				s.writeStartupError(startupErr)
			}
			return startupErr
		}
		healthyAt := time.Now()
		log.Printf("[timing] WaitHealthy: %s", healthyAt.Sub(t0).Round(time.Millisecond))
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
			log.Printf("[timing] CreateSession start: %s after WaitHealthy", time.Since(healthyAt).Round(time.Millisecond))
			_, createErr := s.harness.CreateSession(ctx)
			if createErr != nil {
				log.Printf("sidecar: deliverInitialPrompt: create session: %v", createErr)
				startupErr := fmt.Errorf("sidecar: create session: %w", createErr)
				// Use shuttingDown (not ctx.Err()) as the SIGTERM guard — same
				// rationale as the WaitHealthy path above. The outer isShuttingDown
				// snapshot predates CreateSession, so re-read under the lock here.
				s.mu.Lock()
				alreadyShutdown := s.shuttingDown
				s.mu.Unlock()
				if !alreadyShutdown {
					// Genuine CreateSession failure. Write StateError so the DB row
					// is in the correct terminal state, regardless of tmux hook timing.
					// On SIGTERM, Shutdown() handles the state write.
					s.writeStartupError(startupErr)
				}
				return startupErr
			}
			log.Printf("[timing] CreateSession done: %s after container healthy", time.Since(healthyAt).Round(time.Millisecond))
			log.Printf("[timing] ready: %s from start", time.Since(sessionStart).Round(time.Millisecond))
			if !isShuttingDown && s.cfg.OnReady != nil {
				s.cfg.OnReady()
			}
			initialPrompt := s.cfg.InitialPrompt
			go func() {
				if err := s.harness.DeliverInitialPrompt(ctx, initialPrompt, s.cfg.AgentRole); err != nil {
					log.Printf("sidecar: deliverInitialPrompt: %v", err)
				}
				log.Printf("[timing] prompt delivered: %s from start", time.Since(sessionStart).Round(time.Millisecond))
			}()
		} else if !isShuttingDown {
			log.Printf("[timing] ready: %s from start", time.Since(sessionStart).Round(time.Millisecond))
			if s.cfg.OnReady != nil {
				s.cfg.OnReady()
			}
		}
	}
	return nil
}

// runStartupStdio is the startup stub for TransportStdioPipe harnesses.
//
// This stage (B.3 Stage 1) ships the transport-shape gate seam only. Real
// stdio harness lifecycle integration (launching the child process, managing
// its stdin/stdout pipes, handling process exit) lands in later stages once
// the following open questions are resolved:
//
//   - TUI bridging: how does the user-facing PTY connect to the harness
//     process's output? Options include a capture pipe, a tmux pane that
//     attaches to the child PTY, or a dedicated log-tail view.
//   - fd separation: stdout is the JSON-Lines protocol channel; a separate
//     fd (stderr, or a dedicated pipe fd) must carry human-readable harness
//     log output so the two streams do not interfere.
//   - Readiness signal: HTTP harnesses use WaitHealthy (HTTP probe loop);
//     stdio harnesses need a different readiness signal — either a sentinel
//     line on stdout/stderr, a timeout heuristic, or a protocol-level
//     handshake message.
//   - Signal handling: SIGTERM must drain in-flight stdio messages before
//     closing the pipe and waiting for the child process to exit; the
//     current Shutdown() path is HTTP-centric.
//
// For now, this function writes a startup-error event and returns an error
// so the sidecar exits cleanly rather than hanging.
func (s *Sidecar) runStartupStdio(ctx context.Context) error {
	const note = "stdio harness lifecycle not yet implemented; refusing to start"
	startupErr := fmt.Errorf(note)
	s.mu.Lock()
	s.upsertState(agent.StateError, nil, nil)
	s.writeEvent("state_change", map[string]string{"state": string(agent.StateError), "note": note}, nil)
	s.lastState = agent.StateError
	s.mu.Unlock()
	log.Printf("sidecar: runStartupStdio: %v", startupErr)
	return startupErr
}
