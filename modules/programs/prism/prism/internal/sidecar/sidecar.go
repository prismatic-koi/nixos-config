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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/opencode"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/mergequeue"
	"github.com/prismatic-koi/prism/internal/payload"
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
	// ModelsByRole is the per-role model override map (C.2). When non-nil
	// it takes precedence over AgentModel for any role present in the map.
	// The map is passed directly to the harness adapter at construction time.
	ModelsByRole map[string]string
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
	// IsolationMode is the effective isolation mode for this session (e.g.
	// "podman", "bwrap", "sandbox-exec", or "host"). It is used to derive
	// capability flags via container.CapabilitiesFor so that Run can branch on
	// typed caps rather than raw mode strings or the Container!=nil proxy.
	IsolationMode config.IsolationMode
	// Container, when non-nil, enables container mode: the sidecar creates and
	// manages a podman container running opencode in combined TUI + HTTP mode
	// instead of relying on a directly-launched opencode process.
	Container *container.Config
	// HostAPISockPath, when non-empty, is the path at which the sidecar starts a
	// Unix socket HTTP server exposing host-side tmux operations to agents running
	// inside the sandbox (container or bwrap). The listener is started regardless of
	// whether Container is set — bwrap sessions (where Container is nil) also need it.
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
	// transitioning to StateError via writeStartupError. Only applies when
	// NeedsStartupConnectTimeout is true (currently bwrap mode): podman mode
	// uses WaitHealthy/CreateSession for the same protection. Defaults to
	// DefaultStartupConnectTimeout (5m) when zero. Set to a small value in
	// tests to exercise the timeout path without real wall-clock waits.
	StartupConnectTimeout time.Duration
	// PipeReconnectTimeout is the window after an unexpected connection drop
	// during which the sidecar will accept a new extension connection. Defaults
	// to pipeDisconnectTimeout (30s) when zero. Set to a small value in tests
	// to avoid real wall-clock waits on the reconnect path.
	PipeReconnectTimeout time.Duration
	// HarnessBinaryPath is the path to the harness binary used by
	// runStartupStdio for TransportStdioPipe harnesses. The binary is launched
	// inside a bwrap sandbox (when bwrap is available); the sidecar reads its
	// stdout as a JSONL event stream.
	// When empty, runStartupStdio returns an error (the path is required for
	// stdio-pipe harnesses).
	HarnessBinaryPath string
	// BwrapPath overrides the bwrap binary path used by runStartupStdio.
	// When empty, "bwrap" is resolved via exec.LookPath. Set to a non-existent
	// path in tests that want to exercise the no-bwrap fallback, or to a custom
	// bwrap wrapper for test isolation.
	BwrapPath string
	// HarnessPipeSockPath is the Unix socket path at which the sidecar binds
	// the PI harness pipe for TransportSocketPipe sessions. When non-empty and
	// HarnessName resolves to TransportSocketPipe, runStartupSocketPipe binds
	// this path. Must be empty on Darwin (use HarnessPipeTCPPort instead).
	// Set by cmd/sidecar.go from session.SidecarHarnessPipePath.
	HarnessPipeSockPath string
	// HarnessPipeTCPPort is the OS-allocated TCP port for the harness pipe
	// listener on Darwin (where Unix socket bind-mounts inside sandbox-exec
	// are not reliable). The sidecar binds 0.0.0.0:<port> and exposes it to
	// the sandboxed extension as tcp://host.containers.internal:<port> via
	// PRISM_HARNESS_PIPE. Zero means no TCP listener (Linux path).
	HarnessPipeTCPPort int
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
	// Set in Run() when HostAPISockPath is non-empty (regardless of isolation mode).
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

	// harnessPipeListener is the Unix socket (Linux) or TCP (Darwin) listener
	// for the PI harness pipe. Set in runStartupSocketPipe, closed in Shutdown.
	// Protected by mu.
	harnessPipeListener net.Listener
	// harnessPipeConn is the accepted connection from the PI extension.
	// Set after the first Accept() in runStartupSocketPipe. Protected by mu.
	harnessPipeConn net.Conn
	// harnessPipeOutCh is the channel carrying outbound frames to the extension.
	// runStartupSocketPipe drains it in a writer goroutine. Protected by mu
	// on set (before the goroutine starts); after that only the writer goroutine
	// reads from it; callers enqueue via enqueueHarnessPipeFrame.
	harnessPipeOutCh chan []byte

	// pipeAccum accumulates msg_assistant text fragments between turn_start and
	// turn_end for the socket-pipe transport. On turn_end, the accumulated text
	// is flushed as a single msg_assistant event with token/cost fields from the
	// turn_end usage object. Reset to nil on each turn_start.
	// Protected by s.mu.
	pipeAccum *string

	// reviewingInFlight is set to true when the /review handler successfully
	// writes the reviewing state to the DB, and cleared when the monitor
	// delivers the review-complete prompt via /prompt (same-session,
	// TransportSocketPipe path). All reviewing guards in handlePipeFrame and
	// events.go read this field instead of calling currentDBState(), eliminating
	// the read-after-write race where currentDBState() could return active after
	// the pre-emptive reviewing write. Protected by s.mu.
	reviewingInFlight bool
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

	// Start the host-API Unix socket server (AC-1, AC-9).
	// This runs for ALL harness types — any session with HostAPISockPath set
	// needs the Unix socket so the sandboxed agent can proxy prism CLI calls
	// back to the host. Previously this block was nested inside the
	// Container!=nil branch (and then after the transport-shape switch), which
	// meant bwrap sessions never got a socket: socket-pipe (pi) and
	// stdio-pipe harnesses return early from the switch, so the block was
	// unreachable for them.
	//
	// This block MUST run before the transport-shape switch so that pi
	// (socket-pipe) and stdio-pipe sessions get the listener before
	// runStartupSocketPipe / runStartupStdio is called and returns.
	//
	// The TCP server for Darwin container sessions is started AFTER the switch
	// (below), because runStartupHTTP must run first to bind the TCP listener
	// port (s.hostAPITCPListener is set inside runStartupHTTP on Darwin). Only
	// HTTP-port harnesses reach the post-switch TCP server block because
	// socket-pipe and stdio-pipe return early from the switch.
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
	}

	// B.3 Stage 1: transport-shape gate. Consult the harness registry to
	// determine the wire-level shape and dispatch to the appropriate startup
	// helper. The back half of Run (merge-queue watcher, SSE loop, shutdown)
	// is shape-agnostic and is unchanged.
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
		case harness.TransportSocketPipe:
			return s.runStartupSocketPipe(ctx)
		default:
			return fmt.Errorf("sidecar: unsupported transport shape %q for harness %q", shape, s.cfg.HarnessName)
		}
	} else {
		// HarnessName is empty: preserve legacy behaviour (HTTP-port startup).
		if err := s.runStartupHTTP(ctx); err != nil {
			return err
		}
	}

	// Start the host-API TCP server — Darwin only, for HTTP-port (opencode
	// container) sessions. The TCP listener is bound inside runStartupHTTP
	// (before container creation, so the port is known at container launch
	// time). We start the server goroutine here, after runStartupHTTP returns,
	// because s.hostAPITCPListener is not set until runStartupHTTP runs.
	// Socket-pipe and stdio-pipe cases return early above and never reach here,
	// so this block only executes for HTTP-port sessions — exactly as before.
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

	// Mint or load instance_id. The sidecar is the single owner of session
	// identity. When --instance-id was passed at spawn time, s.cfg.InstanceID
	// is already set (fast path). Otherwise, query the DB: if the row has a
	// non-NULL instance_id, load it; if not, mint a fresh UUID and write it.
	// This runs before the merge-queue watcher so the watcher always has an
	// identity to key on.
	if s.cfg.InstanceID == "" && s.cfg.DB != nil {
		status, _ := s.cfg.DB.CurrentStatus(s.cfg.SessionName)
		if status != nil && status.InstanceID != nil && *status.InstanceID != "" {
			s.cfg.InstanceID = *status.InstanceID
			log.Printf("sidecar: instance_id loaded from DB (%s)", s.cfg.InstanceID)
		} else {
			minted := uuid.New().String()
			// Defensively ensure the row exists before writing instance_id.
			_ = s.cfg.DB.UpsertStatus(s.cfg.SessionName, s.cfg.Repo, s.cfg.Worktree, "idle", nil, nil)
			if err := s.cfg.DB.SetInstanceID(s.cfg.SessionName, minted); err != nil {
				log.Printf("sidecar: warning: could not write minted instance_id: %v", err)
			} else {
				s.cfg.InstanceID = minted
				log.Printf("sidecar: instance_id minted (%s)", s.cfg.InstanceID)
			}
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

	// Startup-connect timeout: applies only to modes where NeedsStartupConnectTimeout
	// is true (currently bwrap).
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
	// Podman mode already has WaitHealthy/CreateSession covering startup
	// failures, so NeedsStartupConnectTimeout is false there.
	if container.CapabilitiesFor(s.cfg.IsolationMode).NeedsStartupConnectTimeout {
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
		if err := s.cfg.DB.UpdateSessionEnded(s.cfg.InstanceID, "finished"); err != nil {
			log.Printf("sidecar: UpdateSessionEnded: %v", err)
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

	// Close the harness pipe listener and connection (socket-pipe transport).
	// Best-effort graceful shutdown: send an abort frame so the PI extension
	// can flush state to disk, write a final session_end event, and exit
	// cleanly. The frame is only delivered when the connection is still live;
	// in the typical cleanup path the tmux pane (and therefore PI) has
	// already been killed before the sidecar receives SIGTERM, so this write
	// is a no-op there. Either way the connection is then closed below.
	s.mu.Lock()
	pipeLn := s.harnessPipeListener
	pipeConn := s.harnessPipeConn
	pipeOutCh := s.harnessPipeOutCh
	s.harnessPipeListener = nil
	s.harnessPipeConn = nil
	s.mu.Unlock()
	if pipeOutCh != nil {
		abortFrame := []byte(`{"type":"abort"}` + "\n")
		select {
		case pipeOutCh <- abortFrame:
			// Give the writer goroutine a brief moment to flush the frame
			// onto the wire before the connection is torn down. Bounded so
			// shutdown never blocks longer than a couple of seconds even if
			// the extension is unresponsive.
			time.Sleep(100 * time.Millisecond)
		default:
			// Outbound queue full or no longer accepting — fall through to
			// the connection close. handlePipeFrame's connection-dropped
			// handler will mark the session error if PI exits unexpectedly.
		}
	}
	if pipeConn != nil {
		_ = pipeConn.Close()
	}
	if pipeLn != nil {
		_ = pipeLn.Close()
		if s.cfg.HarnessPipeSockPath != "" {
			_ = os.Remove(s.cfg.HarnessPipeSockPath)
		}
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
// When OwnsContainerLifecycle is false (bwrap / host mode), this function is a
// no-op: the harness is already running and the SSE loop connects directly. The
// container startup sequence (mgr.Create → WaitHealthy → CreateSession →
// OnReady → DeliverInitialPrompt) is only performed in podman container mode.
func (s *Sidecar) runStartupHTTP(ctx context.Context) error {
	// Container mode: create and health-check the container before connecting.
	if container.CapabilitiesFor(s.cfg.IsolationMode).OwnsContainerLifecycle {
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

// runStartupStdio launches a TransportStdioPipe harness binary inside a bwrap
// sandbox, reads its stdout as a JSONL event stream, and writes each frame as
// an agent event.
//
// # Sandbox
//
// The harness is launched via bwrap (bubblewrap). A minimal sandbox is
// constructed: the Nix store, system paths, and the harness binary's directory
// are bind-mounted read-only; PATH and HOME are propagated. If bwrap is not
// available (resolved via Config.BwrapPath or exec.LookPath("bwrap")), the
// harness is launched directly as a child process — this fallback exists
// primarily for non-Linux environments and test contexts where bwrap is absent.
//
// # Wire format
//
// The harness binary writes one JSON object per line to stdout. Each line is a
// "frame" with at minimum a "type" field:
//
//   - {"type":"state_change","state":"<state>"} — records an agent state
//     transition; valid values mirror agent.AgentState ("active", "finished",
//     "error", …).
//   - {"type":"msg_assistant","text":"<text>"} — records an assistant message
//     fragment.
//
// Any other frame type is written as-is to agent_events with the raw JSON as
// the payload, allowing forward compatibility.
//
// # Process lifecycle
//
// The binary is launched via exec.CommandContext so that ctx cancellation sends
// SIGKILL to the child. The sidecar reads frames until EOF (process exited).
// If the process exits 0 and the last state written was "finished", the session
// is considered cleanly terminated and runStartupStdio returns nil. Any other
// outcome (non-zero exit, process exits before any frame is read, or the last
// frame was not state=finished) is treated as a startup error.
//
// # Open questions (deferred to later stages)
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
func (s *Sidecar) runStartupStdio(ctx context.Context) error {
	if s.cfg.HarnessBinaryPath == "" {
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

	cmd, usedBwrap := s.buildStdioHarnessCmd(ctx)
	if usedBwrap {
		log.Printf("sidecar: runStartupStdio: launching harness binary %q under bwrap", s.cfg.HarnessBinaryPath)
	} else {
		log.Printf("sidecar: runStartupStdio: launching harness binary %q (no bwrap)", s.cfg.HarnessBinaryPath)
	}

	// Inherit stderr so harness log output reaches the sidecar's log.
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		startupErr := fmt.Errorf("sidecar: runStartupStdio: stdout pipe: %w", err)
		s.writeStartupError(startupErr)
		return startupErr
	}

	// If the harness implements StdinReceiver, wire up a stdin pipe so that
	// DeliverInitialPrompt / DeliverPrompt can write to the running process.
	if sr, ok := s.harness.(harness.StdinReceiver); ok {
		stdinPipe, stdinErr := cmd.StdinPipe()
		if stdinErr != nil {
			startupErr := fmt.Errorf("sidecar: runStartupStdio: stdin pipe: %w", stdinErr)
			s.writeStartupError(startupErr)
			return startupErr
		}
		sr.SetStdinPipe(stdinPipe)
	}

	if err := cmd.Start(); err != nil {
		startupErr := fmt.Errorf("sidecar: runStartupStdio: start %q: %w", s.cfg.HarnessBinaryPath, err)
		s.writeStartupError(startupErr)
		return startupErr
	}

	// Check whether the harness implements FrameNormaliser (B5.TR Translate
	// strategy). When it does, NormaliseFrame is called for each raw JSONL line
	// to produce opencode-shaped payloads before writing to agent_events. When
	// the harness does not implement FrameNormaliser, the legacy fallback path
	// below is used (which writes raw frames for state_change and msg_assistant,
	// and raw JSON bytes for all other event types).
	normaliser, hasNormaliser := s.harness.(harness.FrameNormaliser)

	// Read JSONL frames from the harness's stdout. Each line is a JSON object.
	var framesRead int
	var lastState string
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Parse only the "type" field to determine routing; the full raw line
		// is stored as the payload for every event type.
		var frame struct {
			Type  string `json:"type"`
			State string `json:"state"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			log.Printf("sidecar: runStartupStdio: parse frame: %v (raw: %s)", err, truncateBytes(line, 200))
			continue
		}

		// Only count parseable frames; corrupt lines are logged and skipped.
		framesRead++

		log.Printf("sidecar: runStartupStdio: frame type=%q", frame.Type)

		if hasNormaliser {
			// B5.TR path: the harness adapter normalises PI frames to
			// opencode-shaped payloads at write time. The normaliser is
			// responsible for logging unknown event types at info level
			// (not silently dropping them) per the edge-case AC.
			eventType, normPayload, shouldWrite := normaliser.NormaliseFrame(line)
			if !shouldWrite {
				// turn_start / turn_end / msg_assistant may not produce a
				// normPayload but still need to drive the accumulator — check
				// the raw frame type.
				switch frame.Type {
				case "turn_start":
					empty := ""
					s.mu.Lock()
					s.pipeAccum = &empty
					s.writeEvent(frame.Type, json.RawMessage(line), nil)
					s.mu.Unlock()
				case "msg_assistant":
					// Buffer the text fragment from the raw line.
					s.mu.Lock()
					if s.pipeAccum == nil {
						empty := ""
						s.pipeAccum = &empty
					}
					*s.pipeAccum += frame.Text
					s.mu.Unlock()
				case "turn_end":
					s.mu.Lock()
					stdioFlushPipeAccum(s, line)
					s.writeEvent(frame.Type, json.RawMessage(line), nil)
					s.mu.Unlock()
				}
				continue
			}
			if eventType == "state_change" {
				// State-change events must also drive the in-memory state
				// machine and the circuit-breaker, not just the DB row.
				var sc struct {
					State string `json:"state"`
				}
				if err := json.Unmarshal(line, &sc); err == nil && sc.State != "" {
					st := agent.AgentState(sc.State)
					s.mu.Lock()
					s.upsertState(st, nil, nil)
					s.writeEvent(eventType, normPayload, nil)
					s.mu.Unlock()
					lastState = sc.State
					continue
				}
			}
			if eventType == "msg_assistant" {
				// Accumulate text instead of writing immediately.
				var p struct {
					Text string `json:"text"`
				}
				// normPayload may be a payload.MsgAssistant struct; marshal and
				// unmarshal to extract the text field generically.
				if b, err := json.Marshal(normPayload); err == nil {
					_ = json.Unmarshal(b, &p)
				}
				s.mu.Lock()
				if s.pipeAccum == nil {
					empty := ""
					s.pipeAccum = &empty
				}
				*s.pipeAccum += p.Text
				s.mu.Unlock()
				continue
			}
			s.mu.Lock()
			s.writeEvent(eventType, normPayload, nil)
			s.mu.Unlock()
			continue
		}

		// Legacy fallback path (harness does not implement FrameNormaliser).
		switch frame.Type {
		case "turn_start":
			empty := ""
			s.mu.Lock()
			s.pipeAccum = &empty
			s.writeEvent(frame.Type, json.RawMessage(line), nil)
			s.mu.Unlock()

		case "msg_assistant":
			s.mu.Lock()
			if s.pipeAccum == nil {
				empty := ""
				s.pipeAccum = &empty
			}
			*s.pipeAccum += frame.Text
			s.mu.Unlock()

		case "turn_end":
			s.mu.Lock()
			stdioFlushPipeAccum(s, line)
			s.writeEvent(frame.Type, json.RawMessage(line), nil)
			s.mu.Unlock()

		case "state_change":
			st := agent.AgentState(frame.State)
			s.mu.Lock()
			s.upsertState(st, nil, nil)
			s.writeStateChange(st)
			s.mu.Unlock()
			lastState = frame.State

		default:
			// Forward-compatible: write the raw frame as an event of the named type.
			s.mu.Lock()
			s.writeEvent(frame.Type, json.RawMessage(line), nil)
			s.mu.Unlock()
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("sidecar: runStartupStdio: scanner error: %v", err)
	}

	// Flush any partial accumulator before writing error state (mid-turn exit).
	s.mu.Lock()
	s.flushPipeAccum()
	s.mu.Unlock()

	// Wait for the child process to exit. cmd.Wait() also closes the stdout
	// pipe, which is fine because we have already read all frames above.
	waitErr := cmd.Wait()

	if framesRead == 0 {
		// Harness exited before writing any parseable frames — startup failure.
		startupErr := fmt.Errorf("sidecar: runStartupStdio: harness %q exited before writing any frames", s.cfg.HarnessBinaryPath)
		log.Printf("sidecar: runStartupStdio: %v", startupErr)
		s.writeStartupError(startupErr)
		return startupErr
	}

	if waitErr != nil {
		startupErr := fmt.Errorf("sidecar: runStartupStdio: harness process exited with error: %w", waitErr)
		log.Printf("sidecar: runStartupStdio: %v", startupErr)
		s.writeStartupError(startupErr)
		return startupErr
	}

	if lastState != string(agent.StateFinished) {
		startupErr := fmt.Errorf("sidecar: runStartupStdio: harness exited without writing state=finished (last state: %q)", lastState)
		log.Printf("sidecar: runStartupStdio: %v", startupErr)
		s.writeStartupError(startupErr)
		return startupErr
	}

	log.Printf("sidecar: runStartupStdio: harness finished cleanly (%d frames)", framesRead)
	return nil
}

// piWireProtocolVersion is the protocol version this sidecar implementation
// supports. Matches the value in P2.WIRE (#1208) §4.
const piWireProtocolVersion = 1

// pipeDisconnectTimeout is the window the sidecar waits after an unexpected
// connection drop before marking the session as error. See P2.WIRE §7.2.
const pipeDisconnectTimeout = 30 * time.Second

// runStartupSocketPipe binds the per-session Unix socket (Linux) or TCP
// listener (Darwin), accepts the PI extension's connection, performs the
// P2.WIRE hello/hello_ack handshake, then enters the bidirectional frame loop.
//
// # Readiness signal
//
// The hello frame from the extension serves as the readiness signal that would
// otherwise be provided by WaitHealthy / CreateSession in the HTTP path. Once
// hello is received and hello_ack is sent, OnReady is called (if set) and the
// sidecar enters the frame loop.
//
// # Duplex loop
//
// A reader goroutine consumes inbound frames; a writer goroutine drains the
// harnessPipeOutCh channel. Both goroutines run concurrently after the
// handshake. The function blocks until the reader goroutine finishes (i.e.
// until the connection is closed or session_shutdown is received).
//
// # Reconnect loop
//
// The listener stays open across connection drops. On an unexpected disconnect
// (no preceding session_shutdown), the sidecar flushes the accumulator, logs
// the drop, and waits up to pipeDisconnectTimeout for a new connection. If a
// new connection arrives within the window, the handshake is replayed and the
// frame loop resumes. If no connection arrives before the timeout (or ctx is
// cancelled), the session transitions to error state and the function returns.
//
// A clean session_shutdown exits the loop immediately without waiting for
// reconnect.
//
// # Inbound frame handling
//
//   - state_change → existing state machine via upsertState + writeStateChange
//   - tool_call, tool_result, msg_assistant, turn_start, turn_end → agent_events
//   - provider_error, auto_retry_start, auto_retry_end → agent_events (raw JSON)
//   - session_shutdown → marks session finished, closes connection
//   - unknown types → logged and written to agent_events for forward-compat
//
// # Error handling
//
//   - Protocol version mismatch: error frame sent to extension, session → error
//   - Unexpected disconnect: flush accumulator, wait for reconnect; error after timeout
//   - Malformed JSONL frame: logged and skipped, connection not torn down
func (s *Sidecar) runStartupSocketPipe(ctx context.Context) error {
	// --- Bind the listener -----------------------------------------------
	var ln net.Listener
	var listenErr error

	startupTimeout := s.cfg.StartupConnectTimeout
	if startupTimeout == 0 {
		startupTimeout = DefaultStartupConnectTimeout
	}

	reconnectTimeout := s.cfg.PipeReconnectTimeout
	if reconnectTimeout == 0 {
		reconnectTimeout = pipeDisconnectTimeout
	}

	if s.cfg.HarnessPipeTCPPort != 0 {
		// Darwin: TCP fallback.
		ln, listenErr = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", s.cfg.HarnessPipeTCPPort))
		if listenErr != nil {
			err := fmt.Errorf("sidecar: runStartupSocketPipe: TCP listen :%d: %w", s.cfg.HarnessPipeTCPPort, listenErr)
			s.writeStartupError(err)
			return err
		}
		log.Printf("sidecar: runStartupSocketPipe: listening on TCP :%d", s.cfg.HarnessPipeTCPPort)
	} else if s.cfg.HarnessPipeSockPath != "" {
		// Linux: Unix socket.
		if err := os.MkdirAll(filepath.Dir(s.cfg.HarnessPipeSockPath), 0o700); err != nil {
			err2 := fmt.Errorf("sidecar: runStartupSocketPipe: mkdir socket dir: %w", err)
			s.writeStartupError(err2)
			return err2
		}
		_ = os.Remove(s.cfg.HarnessPipeSockPath)
		ln, listenErr = net.Listen("unix", s.cfg.HarnessPipeSockPath)
		if listenErr != nil {
			err := fmt.Errorf("sidecar: runStartupSocketPipe: unix listen %q: %w", s.cfg.HarnessPipeSockPath, listenErr)
			s.writeStartupError(err)
			return err
		}
		log.Printf("sidecar: runStartupSocketPipe: listening on unix %s", s.cfg.HarnessPipeSockPath)
	} else {
		err := fmt.Errorf("sidecar: runStartupSocketPipe: neither HarnessPipeSockPath nor HarnessPipeTCPPort configured")
		s.writeStartupError(err)
		return err
	}

	s.mu.Lock()
	s.harnessPipeListener = ln
	s.mu.Unlock()

	// closePipeListener closes the listener and removes the socket file on a
	// clean exit or timeout. Called exactly once at the end of the function.
	closePipeListener := func() {
		_ = ln.Close()
		if s.cfg.HarnessPipeSockPath != "" {
			_ = os.Remove(s.cfg.HarnessPipeSockPath)
		}
	}
	defer closePipeListener()

	// acceptWithTimeout waits up to the given duration for a new connection.
	// Returns (conn, true) on success or (nil, false) on timeout/ctx cancel.
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	acceptWithTimeout := func(timeout time.Duration) (net.Conn, bool) {
		ch := make(chan acceptResult, 1)
		go func() {
			c, e := ln.Accept()
			ch <- acceptResult{c, e}
		}()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, false
		case <-timer.C:
			return nil, false
		case ar := <-ch:
			if ar.err != nil {
				return nil, false
			}
			return ar.conn, true
		}
	}

	// Set up the shared outbound channel. It is created once and shared across
	// reconnections. The writer goroutine runs for the lifetime of the session,
	// draining frames to whichever conn is currently live.
	outCh := make(chan []byte, 64)
	s.mu.Lock()
	s.harnessPipeOutCh = outCh
	s.mu.Unlock()

	// connMu serialises conn replacement during reconnect so the writer
	// goroutine always writes to the latest live connection.
	var connMu sync.Mutex
	var activeConn net.Conn

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for frame := range outCh {
			s.archiveOutboundFrame(frame)
			connMu.Lock()
			c := activeConn
			connMu.Unlock()
			if c == nil {
				continue
			}
			if _, err := c.Write(frame); err != nil {
				log.Printf("sidecar: runStartupSocketPipe: write outbound frame: %v", err)
				// Don't return — the reader loop will detect the drop and
				// reconnect. The writer stays alive for the next connection.
			}
		}
	}()

	// onReadyFired tracks whether OnReady has been called. It must be called
	// at most once (on the first successful handshake).
	onReadyFired := false

	// --- Main reconnect loop ----------------------------------------------
	// acceptTimeout governs how long we wait for the FIRST connection.
	// After a non-shutdown drop it switches to pipeDisconnectTimeout.
	acceptTimeout := startupTimeout

	const maxLineBytes = 16 * 1024 * 1024

	for {
		// --- Wait for the extension to connect ----------------------------
		conn, ok := acceptWithTimeout(acceptTimeout)
		if !ok {
			s.mu.Lock()
			if ctx.Err() != nil {
				// Context cancelled (SIGTERM path) — Shutdown() handles state.
				s.mu.Unlock()
			} else {
				// Timeout waiting for (re)connection.
				s.flushPipeAccum()
				s.upsertState(agent.StateError, nil, nil)
				s.writeEvent("state_change", map[string]string{"state": string(agent.StateError), "note": "reconnect timeout"}, nil)
				s.lastState = agent.StateError
				s.mu.Unlock()
				err := fmt.Errorf("sidecar: runStartupSocketPipe: timed out waiting for extension to (re)connect (timeout=%s)", acceptTimeout)
				log.Printf("%v", err)
				// Drain and stop the writer goroutine.
				s.mu.Lock()
				s.harnessPipeOutCh = nil
				s.mu.Unlock()
				close(outCh)
				<-writerDone
				return err
			}
			s.mu.Lock()
			s.harnessPipeOutCh = nil
			s.mu.Unlock()
			close(outCh)
			<-writerDone
			return ctx.Err()
		}

		log.Printf("sidecar: runStartupSocketPipe: extension connected from %s", conn.RemoteAddr())
		connMu.Lock()
		activeConn = conn
		connMu.Unlock()
		s.mu.Lock()
		s.harnessPipeConn = conn
		s.mu.Unlock()

		// --- Protocol version handshake -----------------------------------
		reader := bufio.NewReader(conn)

		_ = conn.SetReadDeadline(time.Now().Add(startupTimeout))
		helloLine, err := reader.ReadBytes('\n')
		_ = conn.SetReadDeadline(time.Time{})
		if err != nil {
			err2 := fmt.Errorf("sidecar: runStartupSocketPipe: reading hello: %w", err)
			log.Printf("%v", err2)
			_ = conn.Close()
			connMu.Lock()
			activeConn = nil
			connMu.Unlock()
			// Treat a hello read failure like a premature disconnect: wait for
			// reconnect within reconnectTimeout.
			acceptTimeout = reconnectTimeout
			continue
		}
		if n := len(helloLine); n > 0 && helloLine[n-1] == '\n' {
			s.archiveInboundFrame(helloLine[:n-1])
		} else {
			s.archiveInboundFrame(helloLine)
		}

		var helloFrame struct {
			Type            string `json:"type"`
			ProtocolVersion int    `json:"protocol_version"`
			Harness         string `json:"harness"`
			HarnessVersion  string `json:"harness_version"`
		}
		if err := json.Unmarshal(helloLine, &helloFrame); err != nil {
			s.sendPipeError(conn, "protocol_violation", "malformed hello frame: "+err.Error())
			startupErr := fmt.Errorf("sidecar: runStartupSocketPipe: malformed hello frame: %w", err)
			s.writeStartupError(startupErr)
			_ = conn.Close()
			connMu.Lock()
			activeConn = nil
			connMu.Unlock()
			s.mu.Lock()
			s.harnessPipeOutCh = nil
			s.mu.Unlock()
			close(outCh)
			<-writerDone
			return startupErr
		}
		if helloFrame.Type != "hello" {
			s.sendPipeError(conn, "pre_handshake_frame", fmt.Sprintf("expected hello, got %q", helloFrame.Type))
			startupErr := fmt.Errorf("sidecar: runStartupSocketPipe: expected hello frame, got %q", helloFrame.Type)
			s.writeStartupError(startupErr)
			_ = conn.Close()
			connMu.Lock()
			activeConn = nil
			connMu.Unlock()
			s.mu.Lock()
			s.harnessPipeOutCh = nil
			s.mu.Unlock()
			close(outCh)
			<-writerDone
			return startupErr
		}
		if helloFrame.ProtocolVersion != piWireProtocolVersion {
			code := "protocol_version_unsupported"
			if helloFrame.ProtocolVersion < piWireProtocolVersion {
				code = "protocol_version_too_old"
			}
			msg := fmt.Sprintf("protocol_version %d is not supported (sidecar supports %d)", helloFrame.ProtocolVersion, piWireProtocolVersion)
			log.Printf("sidecar: runStartupSocketPipe: %s: %s", code, msg)
			s.sendPipeError(conn, code, msg)
			startupErr := fmt.Errorf("sidecar: runStartupSocketPipe: protocol version mismatch: extension=%d sidecar=%d", helloFrame.ProtocolVersion, piWireProtocolVersion)
			s.writeStartupError(startupErr)
			_ = conn.Close()
			connMu.Lock()
			activeConn = nil
			connMu.Unlock()
			s.mu.Lock()
			s.harnessPipeOutCh = nil
			s.mu.Unlock()
			close(outCh)
			<-writerDone
			return startupErr
		}

		// Send hello_ack.
		ackFrame := struct {
			Type            string `json:"type"`
			ProtocolVersion int    `json:"protocol_version"`
			SessionName     string `json:"session_name"`
			SessionRole     string `json:"session_role"`
		}{
			Type:            "hello_ack",
			ProtocolVersion: piWireProtocolVersion,
			SessionName:     s.cfg.SessionName,
			SessionRole:     s.cfg.AgentRole,
		}
		ackBytes, _ := json.Marshal(ackFrame)
		ackBytes = append(ackBytes, '\n')
		s.archiveOutboundFrame(ackBytes)
		if _, err := conn.Write(ackBytes); err != nil {
			startupErr := fmt.Errorf("sidecar: runStartupSocketPipe: write hello_ack: %w", err)
			log.Printf("%v", startupErr)
			_ = conn.Close()
			connMu.Lock()
			activeConn = nil
			connMu.Unlock()
			acceptTimeout = reconnectTimeout
			continue
		}

		log.Printf("sidecar: runStartupSocketPipe: handshake complete (harness=%q version=%q)", helloFrame.Harness, helloFrame.HarnessVersion)

		// Signal readiness on the first successful handshake only.
		if !onReadyFired {
			s.mu.Lock()
			if !s.shuttingDown {
				s.writeStateChange(agent.StateActive)
			}
			if s.cfg.OnReady != nil && !s.shuttingDown {
				go s.cfg.OnReady()
			}
			s.mu.Unlock()
			onReadyFired = true
		}

		// --- Inbound frame loop -------------------------------------------
		cleanShutdown := false
		for {
			line, readErr := reader.ReadBytes('\n')
			if len(line) > 0 {
				if len(line) > 1 && line[len(line)-2] == '\r' {
					line = append(line[:len(line)-2], '\n')
				}
				frameBytes := line[:len(line)-1]
				_ = maxLineBytes
				s.archiveInboundFrame(frameBytes)
				if s.handlePipeFrame(frameBytes) {
					cleanShutdown = true
					break
				}
			}
			if readErr != nil {
				log.Printf("sidecar: runStartupSocketPipe: connection dropped: %v", readErr)
				s.mu.Lock()
				s.flushPipeAccum()
				s.mu.Unlock()
				break
			}
		}

		_ = conn.Close()
		connMu.Lock()
		activeConn = nil
		connMu.Unlock()

		if cleanShutdown {
			// session_shutdown received — clean exit, no reconnect.
			break
		}

		// Non-shutdown disconnect: wait for PI to reconnect within
		// reconnectTimeout (e.g. after /new triggers session_start).
		log.Printf("sidecar: runStartupSocketPipe: waiting up to %s for reconnect", reconnectTimeout)
		acceptTimeout = reconnectTimeout
	}

	// Nil out the outbound channel under the lock before closing it, so that
	// any concurrent enqueueHarnessPipeFrame call sees nil (returns false)
	// rather than sending to a closed channel and panicking.
	s.mu.Lock()
	s.harnessPipeOutCh = nil
	s.mu.Unlock()

	// Close outCh to drain the writer goroutine.
	close(outCh)
	<-writerDone

	return nil
}

// handlePipeFrame processes a single inbound frame from the PI extension.
// Called by runStartupSocketPipe's reader loop with the raw JSON bytes
// (without the trailing newline). Returns true if the frame triggered a
// clean session shutdown (session_shutdown frame received).
//
// Implements P2.WIRE §5 frame catalogue. Unknown frame types are persisted
// as raw JSON for forward compatibility (P2.WIRE §8.2).
//
// msg_assistant coalescing: rather than writing one agent_events row per
// streaming fragment, the sidecar accumulates text between turn_start and
// turn_end, then writes a single msg_assistant row on turn_end (or on
// session_shutdown / connection drop) with the concatenated text and token/
// cost fields from the turn_end usage object. This produces one row per
// assistant turn instead of ~50 fragmented rows.
func (s *Sidecar) handlePipeFrame(line []byte) (cleanShutdown bool) {
	if len(line) == 0 {
		return false
	}
	var frame struct {
		Type  string `json:"type"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(line, &frame); err != nil {
		// P2.WIRE §7.3: log and skip.
		log.Printf("sidecar: runStartupSocketPipe: parse frame error: %v (raw: %s)", err, truncateBytes(line, 200))
		return false
	}
	if frame.Type == "" {
		log.Printf("sidecar: runStartupSocketPipe: frame missing type field (raw: %s)", truncateBytes(line, 200))
		return false
	}

	log.Printf("sidecar: runStartupSocketPipe: inbound frame type=%q", frame.Type)

	s.mu.Lock()
	defer s.mu.Unlock()

	switch frame.Type {
	case "state_change":
		st := agent.AgentState(frame.State)
		s.upsertState(st, nil, nil)
		s.writeStateChange(st)
		s.lastState = st

	case "turn_start":
		// Transition the session to active on every new turn. The sidecar's
		// upsertState deduplicates — if already active the write is a no-op.
		// This is the real fix for the idle→active path: NormaliseFrame is not
		// on this code path (PI uses TransportSocketPipe, not TransportStdioPipe).
		//
		// Guard: if reviewingInFlight is set, skip the active write. The /review
		// handler sets this flag atomically in-memory when the pre-emptive
		// reviewing DB write succeeds, closing the race where currentDBState()
		// could return active after the write (#1372). Using the in-memory flag
		// instead of currentDBState() avoids the SQLite read-after-write race.
		if !s.reviewingInFlight {
			s.upsertState(agent.StateActive, nil, nil)
			s.writeStateChange(agent.StateActive)
			s.lastState = agent.StateActive
		} else {
			log.Printf("sidecar: turn_start suppressed (cause=reviewing — awaiting review-complete prompt)")
		}
		// Reset the accumulator for the new turn, then persist the frame.
		empty := ""
		s.pipeAccum = &empty
		s.writeEvent(frame.Type, json.RawMessage(line), nil)

	case "msg_assistant":
		// Buffer the text fragment; do not write a row yet.
		var f struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(line, &f); err == nil {
			if s.pipeAccum == nil {
				// Fragments received before turn_start — initialise accumulator.
				empty := ""
				s.pipeAccum = &empty
			}
			*s.pipeAccum += f.Text
		}

	case "turn_end":
		// Flush the accumulator as a single msg_assistant event with token/cost
		// fields from the usage object, then persist the turn_end frame.
		var f struct {
			Usage struct {
				Input      int     `json:"input"`
				Output     int     `json:"output"`
				CacheRead  int     `json:"cache_read"`
				CacheWrite int     `json:"cache_write"`
				Cost       float64 `json:"cost"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(line, &f)

		text := ""
		if s.pipeAccum != nil {
			text = *s.pipeAccum
		}
		s.pipeAccum = nil

		// Only write a msg_assistant row when there is actual text content.
		// Tool-only turns emit turn_start/turn_end with zero msg_assistant
		// fragments, which would produce a spurious "(no text)" row in checkin.
		if text != "" {
			p := payload.MsgAssistant{
				Text:             text,
				InputTokens:      f.Usage.Input,
				OutputTokens:     f.Usage.Output,
				CacheReadTokens:  f.Usage.CacheRead,
				CacheWriteTokens: f.Usage.CacheWrite,
				Cost:             f.Usage.Cost,
			}
			s.writeEvent("msg_assistant", p, nil)
		}
		s.writeEvent(frame.Type, json.RawMessage(line), nil)

	case "tool_call", "tool_result",
		"provider_error", "auto_retry_start", "auto_retry_end":
		// Write raw JSON as event payload — the wire schema maps directly to
		// the existing agent_events payload format (P2.WIRE §8.1).
		s.writeEvent(frame.Type, json.RawMessage(line), nil)

	case "session_shutdown":
		// Flush any partial accumulator before marking finished.
		s.flushPipeAccum()
		// Clean shutdown: mark finished and signal the reader loop.
		s.upsertState(agent.StateFinished, nil, nil)
		s.writeStateChange(agent.StateFinished)
		s.lastState = agent.StateFinished
		return true

	default:
		// Forward-compatible: persist unknown frames as raw JSON.
		s.writeEvent(frame.Type, json.RawMessage(line), nil)
	}
	return false
}

// flushPipeAccum writes a msg_assistant event for any text accumulated in
// pipeAccum and resets the accumulator to nil. Called on session_shutdown and
// connection drop so that partial turns are not silently discarded.
// Must be called with s.mu held.
func (s *Sidecar) flushPipeAccum() {
	if s.pipeAccum == nil {
		return
	}
	p := payload.MsgAssistant{Text: *s.pipeAccum}
	s.writeEvent("msg_assistant", p, nil)
	s.pipeAccum = nil
}

// stdioFlushPipeAccum flushes the msg_assistant accumulator for the stdio
// (TransportStdioPipe) path on turn_end. Unlike flushPipeAccum it:
//   - Reads token/cost fields from the turn_end line (if present).
//   - Suppresses the write when the accumulator is nil or empty (zero
//     msg_assistant fragments in the turn → no spurious row).
//
// Must be called with s.mu held.
func stdioFlushPipeAccum(s *Sidecar, turnEndLine []byte) {
	if s.pipeAccum == nil || *s.pipeAccum == "" {
		s.pipeAccum = nil
		return
	}
	text := *s.pipeAccum
	s.pipeAccum = nil

	var f struct {
		Usage struct {
			Input      int     `json:"input"`
			Output     int     `json:"output"`
			CacheRead  int     `json:"cache_read"`
			CacheWrite int     `json:"cache_write"`
			Cost       float64 `json:"cost"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(turnEndLine, &f)

	p := payload.MsgAssistant{
		Text:             text,
		InputTokens:      f.Usage.Input,
		OutputTokens:     f.Usage.Output,
		CacheReadTokens:  f.Usage.CacheRead,
		CacheWriteTokens: f.Usage.CacheWrite,
		Cost:             f.Usage.Cost,
	}
	s.writeEvent("msg_assistant", p, nil)
}

// sendPipeError writes a protocol-level error frame to the connection and
// logs it. Used during the handshake before the normal frame loop starts.
func (s *Sidecar) sendPipeError(conn net.Conn, code, message string) {
	errFrame := struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}{
		Type:    "error",
		Code:    code,
		Message: message,
	}
	b, _ := json.Marshal(errFrame)
	b = append(b, '\n')
	// Archive the outbound error (P5.LOGS / #1218) so handshake failures show
	// up in `prism logs --harness-events`.
	s.archiveOutboundFrame(b)
	_, _ = conn.Write(b)
	_ = conn.Close()
}

// enqueueHarnessPipeFrame enqueues a JSONL frame for delivery to the PI
// extension via the outbound writer goroutine. The frame must already be
// terminated with '\n'. Returns false if no active pipe connection is open.
func (s *Sidecar) enqueueHarnessPipeFrame(frame []byte) bool {
	s.mu.Lock()
	ch := s.harnessPipeOutCh
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- frame:
		return true
	default:
		log.Printf("sidecar: enqueueHarnessPipeFrame: outbound queue full, dropping frame")
		return false
	}
}

// DeliverPrompt enqueues a prompt frame to the PI extension for
// TransportSocketPipe sessions. For other transport shapes (HTTP-port,
// stdio-pipe), this method is a no-op and the caller must use the
// harness-specific delivery path (e.g. harness.DeliverPrompt or stdin write).
//
// deliverAs controls the prompt delivery mode:
//   - "steer"    — mid-turn steering message
//   - "followUp" — follow-up after the current turn completes
//   - "nextTurn" — schedule for the next turn
//
// Empty deliverAs defaults to "nextTurn".
func (s *Sidecar) DeliverPrompt(text, deliverAs string) bool {
	if deliverAs == "" {
		deliverAs = "nextTurn"
	}
	frame := struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		DeliverAs string `json:"deliver_as"`
	}{
		Type:      "prompt",
		Text:      text,
		DeliverAs: deliverAs,
	}
	b, err := json.Marshal(frame)
	if err != nil {
		log.Printf("sidecar: DeliverPrompt: marshal: %v", err)
		return false
	}
	b = append(b, '\n')
	return s.enqueueHarnessPipeFrame(b)
}

// SetModel enqueues a set_model control frame to the PI extension.
// Stub for P3.LIVE — the sidecar does not yet act on this internally;
// it only forwards the frame.
func (s *Sidecar) SetModel(provider, model, thinking string) {
	frame := struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Thinking string `json:"thinking,omitempty"`
	}{
		Type:     "set_model",
		Provider: provider,
		Model:    model,
		Thinking: thinking,
	}
	b, _ := json.Marshal(frame)
	b = append(b, '\n')
	s.enqueueHarnessPipeFrame(b) //nolint:errcheck
}

// RegisterProvider enqueues a register_provider frame to the PI extension.
// Stub for P3.LIVE.
func (s *Sidecar) RegisterProvider(name string, cfg map[string]any) {
	frame := struct {
		Type   string         `json:"type"`
		Name   string         `json:"name"`
		Config map[string]any `json:"config"`
	}{
		Type:   "register_provider",
		Name:   name,
		Config: cfg,
	}
	b, _ := json.Marshal(frame)
	b = append(b, '\n')
	s.enqueueHarnessPipeFrame(b) //nolint:errcheck
}

// SetActiveTools enqueues a set_active_tools frame to the PI extension.
// Stub for P3.LIVE.
func (s *Sidecar) SetActiveTools(tools []string) {
	frame := struct {
		Type  string   `json:"type"`
		Tools []string `json:"tools"`
	}{
		Type:  "set_active_tools",
		Tools: tools,
	}
	b, _ := json.Marshal(frame)
	b = append(b, '\n')
	s.enqueueHarnessPipeFrame(b) //nolint:errcheck
}

// Abort enqueues an abort frame to the PI extension.
// Stub for P3.LIVE.
func (s *Sidecar) Abort() {
	frame := struct {
		Type string `json:"type"`
	}{Type: "abort"}
	b, _ := json.Marshal(frame)
	b = append(b, '\n')
	s.enqueueHarnessPipeFrame(b) //nolint:errcheck
}

// buildStdioHarnessCmd constructs the exec.Cmd used to launch the stdio
// harness binary. When bwrap is available (resolved via Config.BwrapPath or
// exec.LookPath("bwrap")), the command is wrapped in a minimal bwrap sandbox.
// The returned bool is true when bwrap is used.
//
// Sandbox design: the sandbox is intentionally minimal for the stdio transport
// shape. Unlike the opencode bwrap sandbox (which mounts a full NixOS tree for
// a long-lived interactive session), the stdio harness is a short-lived,
// non-interactive process whose only output is the JSONL stream on stdout.
// The minimal sandbox provides:
//   - Private PID and UTS namespaces (--unshare-pid, --unshare-uts)
//   - /proc, /dev, /tmp (standard process environment)
//   - /nix, /etc, /bin, /run/current-system (NixOS binary resolution)
//   - Read-only bind-mount of the harness binary's own directory so the
//     binary is reachable at the same path inside the sandbox
//   - PATH and HOME environment variables forwarded from the caller
//
// This is sufficient for the fake test harness and for most real stdio
// harnesses that are simple binaries. Future stages (Stage 3/4) may extend
// the sandbox to match the full bwrap profile in bwrap.go when a real harness
// (PI) requires additional mounts (config dirs, SSH keys, etc.).
func (s *Sidecar) buildStdioHarnessCmd(ctx context.Context) (*exec.Cmd, bool) {
	bwrapBin := s.cfg.BwrapPath
	if bwrapBin == "" {
		var err error
		bwrapBin, err = exec.LookPath("bwrap")
		if err != nil {
			// bwrap not available: fall back to direct exec.
			return exec.CommandContext(ctx, s.cfg.HarnessBinaryPath), false
		}
	}

	// Resolve the harness binary to its real path so bwrap can bind-mount
	// the containing directory.
	realBin, err := filepath.EvalSymlinks(s.cfg.HarnessBinaryPath)
	if err != nil {
		realBin = s.cfg.HarnessBinaryPath
	}
	binDir := filepath.Dir(realBin)

	// Build minimal bwrap args.
	bwrapArgs := []string{
		"--clearenv",
		"--unshare-pid",
		"--unshare-uts",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--die-with-parent",
	}

	// System binary roots — required for NixOS binary resolution.
	for _, sysRoot := range []string{"/nix", "/etc", "/bin", "/run/current-system"} {
		if _, statErr := os.Stat(sysRoot); statErr == nil {
			bwrapArgs = append(bwrapArgs, "--ro-bind", sysRoot, sysRoot)
		}
	}

	// Harness binary directory — bind read-only so the binary is reachable at
	// its original path inside the sandbox.
	bwrapArgs = append(bwrapArgs, "--ro-bind", binDir, binDir)

	// Forward PATH and HOME so the harness can resolve its own dependencies.
	if pathVal := os.Getenv("PATH"); pathVal != "" {
		bwrapArgs = append(bwrapArgs, "--setenv", "PATH", pathVal)
	}
	if homeVal := os.Getenv("HOME"); homeVal != "" {
		bwrapArgs = append(bwrapArgs, "--setenv", "HOME", homeVal)
	}

	// Forward PRISM_FAKE_STDIO_HARNESS so the fake test harness knows its mode.
	// In production this env var is unset; for real harnesses this is a no-op.
	if fakeMode := os.Getenv("PRISM_FAKE_STDIO_HARNESS"); fakeMode != "" {
		bwrapArgs = append(bwrapArgs, "--setenv", "PRISM_FAKE_STDIO_HARNESS", fakeMode)
	}

	// Terminate bwrap args and specify the harness binary to run.
	bwrapArgs = append(bwrapArgs, "--", realBin)

	return exec.CommandContext(ctx, bwrapBin, bwrapArgs...), true
}

// truncateBytes returns up to maxLen bytes of b as a string.
func truncateBytes(b []byte, maxLen int) string {
	if len(b) <= maxLen {
		return string(b)
	}
	return string(b[:maxLen])
}
