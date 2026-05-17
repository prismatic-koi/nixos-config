package iris

// harness_socket.go — per-session harness socket listener for the iris daemon.
//
// Each pi child process connects to a Unix domain socket at
// ~/.local/state/iris/run/<instance_id>/harness.sock. The extension dials
// this socket on session_start, performs the hello/hello_ack handshake, then
// enters a bidirectional dispatch loop:
//
//   extension → daemon: tool_exec, tool_abort (and existing observation frames)
//   daemon → extension: tool_exec_result, tool_exec_update (and prompt/model/etc)
//
// The harness socket handler is agnostic about whether the extension is the
// prism extension or any other extension — it only cares about the wire
// protocol. In D-3 all tool subprocesses run unsandboxed on the host.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// HarnessSocketServer manages a per-session harness socket. One instance is
// created per session by the Supervisor when it spawns a pi child.
// EventPublisher receives notifications when a new event is written to the DB.
// It is set by the daemon to the ClientSocket.Publish method so that every
// harness event is fanned out to all subscribed clients in real time (D-6).
type EventPublisher interface {
	Publish(EventPublication)
}

type HarnessSocketServer struct {
	sess    *SessionRecord
	database *db.DB
	sockPath string
	listener net.Listener

	// mu guards in-flight tool calls.
	mu       sync.Mutex
	// inFlight maps tool call ID → channel that the caller is waiting on
	// for its ToolExecResultFrame.
	inFlight map[string]chan ToolExecResultFrame
	// abortChans maps tool call ID → channel signalled by tool_abort.
	abortChans map[string]chan struct{}

	// writer is the current connected extension's writer (nil when no
	// connection is active).
	writer *jsonlWriter

	// publisher receives Publish calls for every event written to the DB.
	// Nil when no client socket is configured (e.g. in harness-only tests).
	publisher EventPublisher

	// sessionStatusHandler is invoked when a session_status frame delivers a
	// non-empty session_id, immediately after the DB write. Wired by the
	// Supervisor so it can update its in-memory SessionRecord.PiSessionPath
	// under the supervisor's lock (issue #1682). Nil in harness-only tests.
	sessionStatusHandler func(sessionID string)

	// stateChangeHandler is invoked when a state_change frame arrives from
	// the extension, immediately after the agent_events row is written.
	// Wired by the Supervisor so it can drive the in-memory session state
	// machine, update sessions.iris_state, and notify subscribers via
	// PublishState. Per PR #1657 the event row is written *before* the
	// status row — callers must preserve that ordering (issue #1701). Nil
	// in harness-only tests; in that case the frame is logged only.
	stateChangeHandler func(state string)

	// sessionShutdownReceived is set to true when the session_shutdown
	// frame arrives — used by the supervisor to detect clean exits.
	sessionShutdownReceived bool
	shutdownMu              sync.Mutex

	// wg tracks goroutines spawned by this server that may write to the DB
	// or the session run directory: every `go h.dispatchToolExec(...)` and
	// the AcceptOne accept loop register an Add(1) before they start and a
	// Done() before they return. Wait() blocks until all of them complete
	// and is the canonical drain point for test cleanup ordering
	// (issue #1705) — callers that own shared state (tempdir, DB) must
	// invoke Wait before tearing that state down, otherwise a late write
	// from dispatchToolExec → writeEvent races against tempdir removal and
	// DB close, surfacing as 'unlinkat ... directory not empty' or
	// 'sql: database is closed'.
	wg sync.WaitGroup
}

// SockPath returns the path of the harness socket.
func (h *HarnessSocketServer) SockPath() string { return h.sockPath }

// NewHarnessSocketServer creates a HarnessSocketServer for the given session.
// It does not start listening yet — call Listen() followed by Accept() in a
// goroutine.
func NewHarnessSocketServer(sess *SessionRecord, database *db.DB) (*HarnessSocketServer, error) {
	return &HarnessSocketServer{
		sess:       sess,
		database:   database,
		sockPath:   sess.HarnessSockPath,
		inFlight:   make(map[string]chan ToolExecResultFrame),
		abortChans: make(map[string]chan struct{}),
	}, nil
}

// SetPublisher wires a ClientSocket (or any EventPublisher) to this harness
// server. After this call, every event written to the DB is also published
// to the client fan-out. Call before starting AcceptOne().
func (h *HarnessSocketServer) SetPublisher(p EventPublisher) {
	h.mu.Lock()
	h.publisher = p
	h.mu.Unlock()
}

// SetSessionStatusHandler wires a callback invoked when the session_status
// frame arrives with a non-empty session_id. The callback runs synchronously
// after the DB write so the in-memory SessionRecord and the DB row stay in
// lock-step. Wired by the Supervisor; nil in harness-only tests.
func (h *HarnessSocketServer) SetSessionStatusHandler(fn func(sessionID string)) {
	h.mu.Lock()
	h.sessionStatusHandler = fn
	h.mu.Unlock()
}

// SetStateChangeHandler wires a callback invoked when a state_change frame
// arrives from the extension. The callback runs synchronously after the
// agent_events row is written (PR #1657 ordering: event row first, then
// the status row inside the supervisor's setState). The callback is
// expected to drive setState under the supervisor's lock so PublishState
// fires for every transition (issue #1701).
func (h *HarnessSocketServer) SetStateChangeHandler(fn func(state string)) {
	h.mu.Lock()
	h.stateChangeHandler = fn
	h.mu.Unlock()
}

// Listen binds the Unix domain socket and begins accepting connections.
// The parent directory must already exist.
func (h *HarnessSocketServer) Listen() error {
	// Remove any stale socket file from a previous run.
	_ = os.Remove(h.sockPath)

	ln, err := net.Listen("unix", h.sockPath)
	if err != nil {
		return fmt.Errorf("iris: harness socket listen %q: %w", h.sockPath, err)
	}
	if err := os.Chmod(h.sockPath, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("iris: harness socket chmod %q: %w", h.sockPath, err)
	}
	h.listener = ln
	log.Printf("[iris] harness socket listening at %s (session %s)", h.sockPath, h.sess.InstanceID)
	return nil
}

// AcceptOne accepts a single connection from the pi extension and runs the
// handshake + dispatch loop. It returns when the connection closes or ctx is
// cancelled. Designed to be called in its own goroutine.
//
// When ctx is cancelled, AcceptOne unblocks the Accept call by setting a
// past deadline on the listener rather than closing it. This preserves the
// socket file on disk so the next pi spawn attempt can connect to the same
// socket. The listener is only closed (and the socket file removed) by an
// explicit call to Close().
func (h *HarnessSocketServer) AcceptOne(ctx context.Context) error {
	// Track this call in the WaitGroup so test scaffolding can Wait() for
	// the AcceptOne → handleConn path to finish before tearing down the
	// per-session tempdir / DB (issue #1705). Production callers Wait()
	// implicitly via the daemon's normal shutdown sequence; the WaitGroup
	// is otherwise free of cost.
	h.wg.Add(1)
	defer h.wg.Done()

	// Ensure any prior deadline is cleared before we start waiting.
	if dl, ok := h.listener.(interface{ SetDeadline(time.Time) error }); ok {
		_ = dl.SetDeadline(time.Time{})
	}

	// Set a deadline on the Accept so we can respect context cancellation.
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan acceptResult, 1)
	go func() {
		conn, err := h.listener.Accept()
		ch <- acceptResult{conn, err}
	}()

	select {
	case <-ctx.Done():
		// Interrupt the Accept goroutine by setting a past deadline. This
		// unblocks Accept without closing the listener or removing the socket
		// file — the socket must survive so the next pi spawn can connect.
		if dl, ok := h.listener.(interface{ SetDeadline(time.Time) error }); ok {
			_ = dl.SetDeadline(time.Now())
		}
		<-ch // wait for the Accept goroutine to unblock and exit
		return ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return fmt.Errorf("iris: harness socket accept: %w", res.err)
		}
		return h.handleConn(ctx, res.conn)
	}
}

// Wait blocks until every goroutine spawned by this HarnessSocketServer
// has returned: the AcceptOne / handleConn loop and every in-flight
// dispatchToolExec. It is the canonical drain point for test cleanup
// (issue #1705) — callers that own the per-session tempdir or the DB
// must Wait() before closing those resources, otherwise a late write
// from a dispatch goroutine races against teardown.
//
// Wait does NOT cancel anything itself. Callers must first cancel the
// context passed to AcceptOne (and any other context driving dispatch)
// so the goroutines actually exit; Wait only blocks until they do.
//
// Production callers in the iris daemon do not need to call Wait — the
// daemon's process lifetime owns these goroutines and they exit when
// the daemon does. Wait exists for test scaffolding.
func (h *HarnessSocketServer) Wait() {
	h.wg.Wait()
}

// Close closes the listener and removes the socket file.
func (h *HarnessSocketServer) Close() {
	if h.listener != nil {
		h.listener.Close()
	}
	_ = os.Remove(h.sockPath)
}

// SessionShutdownReceived reports whether the extension sent a session_shutdown
// frame before the connection closed.
func (h *HarnessSocketServer) SessionShutdownReceived() bool {
	h.shutdownMu.Lock()
	defer h.shutdownMu.Unlock()
	return h.sessionShutdownReceived
}

// SendFrame sends a frame to the connected extension. Returns an error if no
// connection is active.
func (h *HarnessSocketServer) SendFrame(frame any) error {
	h.mu.Lock()
	w := h.writer
	h.mu.Unlock()
	if w == nil {
		return fmt.Errorf("iris: no active extension connection on session %s", h.sess.InstanceID)
	}
	return w.write(frame)
}

// handleConn manages a single extension connection: handshake, then dispatch.
func (h *HarnessSocketServer) handleConn(ctx context.Context, conn net.Conn) error {
	defer conn.Close()

	// Use a bufio.Reader with a large buffer (16 MiB) per the wire spec §3
	// requirement that JSONL implementations must not impose a fixed line-size
	// limit — tool outputs can be very large.
	reader := bufio.NewReaderSize(conn, 16*1024*1024)
	w := &jsonlWriter{conn: conn}

	// --- Handshake ---

	// Expect the first frame to be hello.
	line, err := readLine(reader)
	if err != nil {
		return fmt.Errorf("iris: harness handshake: read hello: %w", err)
	}
	var hello HelloFrame
	if err := json.Unmarshal(line, &hello); err != nil || hello.Type != "hello" {
		errFrame := map[string]any{
			"type":    "error",
			"code":    "protocol_violation",
			"message": "expected hello as first frame",
		}
		_ = w.write(errFrame)
		return fmt.Errorf("iris: harness handshake: expected hello, got %q", hello.Type)
	}
	if hello.ProtocolVersion != ProtocolVersion {
		errFrame := map[string]any{
			"type":    "error",
			"code":    "protocol_version_unsupported",
			"message": fmt.Sprintf("iris supports protocol_version=%d; extension sent %d", ProtocolVersion, hello.ProtocolVersion),
		}
		_ = w.write(errFrame)
		return fmt.Errorf("iris: harness handshake: protocol version mismatch (extension=%d, iris=%d)", hello.ProtocolVersion, ProtocolVersion)
	}

	ack := HelloAckFrame{
		Type:            "hello_ack",
		ProtocolVersion: ProtocolVersion,
		SessionName:     h.sess.SessionName,
		SessionRole:     h.sess.Role,
		IsolationMode:   "host", // D-3: no sandbox
		InstanceID:      h.sess.InstanceID,
	}
	if err := w.write(ack); err != nil {
		return fmt.Errorf("iris: harness handshake: write hello_ack: %w", err)
	}

	// Register the writer so SendFrame() works.
	h.mu.Lock()
	h.writer = w
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.writer = nil
		h.mu.Unlock()
	}()

	log.Printf("[iris] harness connection established (session %s, harness=%s %s)",
		h.sess.InstanceID, hello.Harness, hello.HarnessVersion)

	// --- Dispatch loop ---
	for {
		line, err := readLine(reader)
		if err != nil {
			// EOF or connection closed — normal termination.
			return nil
		}

		var generic GenericFrame
		if err := json.Unmarshal(line, &generic); err != nil {
			log.Printf("[iris] harness: parse error on line (session %s): %v", h.sess.InstanceID, err)
			continue
		}

		switch generic.Type {
		case FrameTypeToolExec:
			var frame ToolExecFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				log.Printf("[iris] harness: bad tool_exec frame: %v", err)
				continue
			}
			// Track the dispatch in h.wg so Wait() drains in-flight tool
			// executions before test cleanup tears down the tempdir / DB
			// (issue #1705).
			h.wg.Add(1)
			go func(frame ToolExecFrame) {
				defer h.wg.Done()
				h.dispatchToolExec(ctx, w, frame)
			}(frame)

		case FrameTypeToolAbort:
			var frame ToolAbortFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				log.Printf("[iris] harness: bad tool_abort frame: %v", err)
				continue
			}
			h.handleToolAbort(frame.ID)

		case "session_shutdown":
			h.shutdownMu.Lock()
			h.sessionShutdownReceived = true
			h.shutdownMu.Unlock()
			log.Printf("[iris] harness: session_shutdown received (session %s)", h.sess.InstanceID)
			// Close connection cleanly; the supervisor will detect clean exit.
			return nil

		case "session_status":
			// Extract session_id (pi's JSONL session UUID) and persist it in
			// sessions.harness_session_id so the D-9 restore path can locate
			// the JSONL file at daemon restart. After the DB write, invoke
			// the session-status handler (wired by the Supervisor) so the
			// in-memory SessionRecord.PiSessionPath is updated too — without
			// this, daemonState.activeSessions() would report an empty
			// harness_session_id for every live session (issue #1682).
			var statusFrame map[string]any
			if err := json.Unmarshal(line, &statusFrame); err == nil {
				if sessionID, ok := statusFrame["session_id"].(string); ok && sessionID != "" {
					if err := h.database.IrisUpdateHarnessSessionID(h.sess.InstanceID, sessionID); err != nil {
						log.Printf("[iris] harness: failed to persist session_id %q for instance %s: %v",
							sessionID, h.sess.InstanceID, err)
					}
					// Ordering invariant (PR #1657): the agent_events row for
					// session_status is written by writeObservationEvent below;
					// the DB harness_session_id row is set above. The in-memory
					// mutation here mirrors the DB write — same ordering, same
					// place.
					h.mu.Lock()
					handler := h.sessionStatusHandler
					h.mu.Unlock()
					if handler != nil {
						handler(sessionID)
					}
				}
			}
			// Write observation event to DB.
			h.writeObservationEvent("session_status", line)

		case "tool_call":
			// Observation frame — write to DB for logging.
			h.writeObservationEvent("tool_call", line)

		case "tool_result":
			// Observation frame — write to DB for logging.
			h.writeObservationEvent("tool_result", line)

		case "tool_progress":
			// Mid-tool heartbeat (#1761). PI emits this on a fixed cadence
			// while a tool call is in flight so long-running shells don't
			// silence the wire long enough to trip downstream inactivity
			// timers. Deliberately not written to agent_events: it would
			// otherwise surface in narrative output (default branch of
			// narrative.RenderRow) as a duplicate artefact. The receipt of
			// the frame is itself the activity signal — no further work
			// needed here.
			continue

		case "state_change":
			// State transition from the extension (e.g. waiting, active,
			// finished, interrupted). Write the agent_events row FIRST per
			// PR #1657 ordering, then dispatch to the supervisor so the
			// sessions.iris_state row, the in-memory SessionRecord, and the
			// PublishState fan-out all happen under the supervisor's lock
			// (issue #1701).
			h.writeObservationEvent("state_change", line)
			var stateFrame struct {
				State string `json:"state"`
			}
			if err := json.Unmarshal(line, &stateFrame); err != nil || stateFrame.State == "" {
				if err != nil {
					log.Printf("[iris] harness: bad state_change frame: %v", err)
				} else {
					log.Printf("[iris] harness: state_change frame missing state field")
				}
				continue
			}
			h.mu.Lock()
			handler := h.stateChangeHandler
			h.mu.Unlock()
			if handler != nil {
				handler(stateFrame.State)
			}

		default:
			// Unknown or other observation frames — write to DB generically.
			h.writeObservationEvent(generic.Type, line)
		}
	}
}

// dispatchToolExec executes a tool in a subprocess (host-mode, no sandbox for
// D-3) and sends back tool_exec_result. Each call runs in its own goroutine.
func (h *HarnessSocketServer) dispatchToolExec(ctx context.Context, w *jsonlWriter, frame ToolExecFrame) {
	// Register the abort channel before starting execution so tool_abort
	// frames that arrive while the subprocess is running can be delivered.
	abortCh := make(chan struct{}, 1)
	h.mu.Lock()
	h.abortChans[frame.ID] = abortCh
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.abortChans, frame.ID)
		h.mu.Unlock()
	}()

	// Build the dispatcher first so we can ask the broker which credentials
	// are about to be injected — that list is recorded on the tool_call event
	// (D-7 audit logging) before dispatch runs.
	//
	// Derive tmpDir from the harness socket path:
	//   sessionDir = filepath.Dir(sess.HarnessSockPath)
	//   tmpDir     = filepath.Join(sessionDir, "tmp")
	// This is the host-side backing directory for the in-sandbox /tmp mount
	// (design doc §10.1: ~/.local/state/iris/run/<instance_id>/tmp/).
	sessionDir := filepath.Dir(h.sess.HarnessSockPath)
	tmpDir := filepath.Join(sessionDir, "tmp")
	dispatcher := &toolDispatcher{
		worktree:   h.sess.Worktree,
		tmpDir:     tmpDir,
		role:       h.sess.Role,
		bareRoot:   h.sess.BareRoot,
		broker:     NewCredentialBroker(),
		writer:     w,
		abortCh:    abortCh,
		toolExecID: frame.ID,
	}

	// D-7: record which credentials are about to be injected. Names only,
	// never values. nil/empty → no credentials (still a valid audit record).
	credentialsInjected := dispatcher.CredentialNamesForTool(frame.Name)
	if credentialsInjected == nil {
		// Use an empty (non-nil) slice so the JSON field is [], not null —
		// downstream consumers can distinguish "no credentials injected" from
		// "field missing on a pre-D-7 event".
		credentialsInjected = []string{}
	}

	// Write tool_call event to DB (AC: every tool_exec → tool_call event).
	payloadBytes, _ := json.Marshal(map[string]any{
		"id":                   frame.ID,
		"name":                 frame.Name,
		"args":                 frame.Args,
		"credentials_injected": credentialsInjected,
		"agent_role":           h.sess.Role,
	})
	h.writeEvent("tool_call", payloadBytes)

	// Execute the tool.
	result := dispatcher.dispatch(ctx, frame)

	// Send the result frame.
	resultFrame := ToolExecResultFrame{
		Type:    FrameTypeToolExecResult,
		ID:      frame.ID,
		Success: result.Success,
		IsError: result.IsError,
		Output:  result.Output,
		Details: result.Details,
	}
	if err := w.write(resultFrame); err != nil {
		log.Printf("[iris] harness: failed to write tool_exec_result (id=%s): %v", frame.ID, err)
	}

	// Write tool_result event to DB (AC: every tool_exec_result → tool_result event).
	resultPayloadBytes, _ := json.Marshal(map[string]any{
		"id":      frame.ID,
		"name":    frame.Name,
		"success": result.Success,
		"output":  truncate(result.Output, 8192),
	})
	h.writeEvent("tool_result", resultPayloadBytes)
}

// handleToolAbort signals the abort channel for the named in-flight tool call.
func (h *HarnessSocketServer) handleToolAbort(id string) {
	h.mu.Lock()
	ch, ok := h.abortChans[id]
	h.mu.Unlock()
	if ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// writeEvent writes an event row to the iris DB, setting instance_id so the
// D-9 orphan-detection query can correlate events by session instance.
// The sessions row is inserted by NewSupervisor / newRestoreSupervisor before
// the harness socket opens, satisfying the FK constraint.
// After writing, the event is published to the client fan-out (D-6).
func (h *HarnessSocketServer) writeEvent(eventType string, payload []byte) {
	h.writeEventWithInstanceID(eventType, payload, h.sess.InstanceID)
}

// writeObservationEvent writes a generic observation event (forwarded as-is)
// to the iris DB, associated with this session's instance_id.
func (h *HarnessSocketServer) writeObservationEvent(eventType string, rawLine []byte) {
	h.writeEventWithInstanceID(eventType, rawLine, h.sess.InstanceID)
}

// writeEventWithInstanceID is the shared write helper. iid may be empty when
// no sessions row exists (stored NULL). After writing to the DB it publishes
// to the client fan-out (D-6).
func (h *HarnessSocketServer) writeEventWithInstanceID(eventType string, payload []byte, iid string) {
	var iidPtr *string
	if iid != "" {
		iidPtr = &iid
	}
	event := db.Event{
		ID:          uuid.New().String(),
		SessionName: h.sess.SessionName,
		Repo:        "", // iris does not carry repo in the harness socket context
		Worktree:    h.sess.Worktree,
		Type:        eventType,
		Payload:     string(payload),
		CreatedAt:   time.Now(),
		InstanceID:  iidPtr,
	}
	rowID, err := h.database.WriteEventReturningRowID(event)
	if err != nil {
		log.Printf("[iris] harness: failed to write %s event: %v", eventType, err)
		return
	}
	h.publishEvent(eventType, string(payload), rowID)
}

// publishEvent forwards a just-written event to the client fan-out (if a
// publisher is configured). It is a no-op when publisher is nil.
func (h *HarnessSocketServer) publishEvent(eventType, payload string, rowID int64) {
	h.mu.Lock()
	pub := h.publisher
	h.mu.Unlock()
	if pub == nil {
		return
	}
	pub.Publish(EventPublication{
		SessionName: h.sess.SessionName,
		RowID:       rowID,
		EventType:   eventType,
		Payload:     payload,
	})
}

// --- Helper: jsonlWriter ---

// jsonlWriter wraps a net.Conn and provides synchronised JSON-line writes.
type jsonlWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (w *jsonlWriter) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.conn.Write(data)
	return err
}

// --- Helper: readLine ---

// readLine reads one '\n'-terminated line from the reader, stripping the
// trailing newline and any '\r' prefix. Returns (nil, io.EOF) when the
// connection is closed cleanly.
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	// Strip trailing \r\n or \n.
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, nil
}

// truncate truncates s to maxBytes, appending "…[truncated]" if truncation
// occurred. Follows the convention in pi-wire-protocol.md §5.3 / §5.4.
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "…[truncated]"
}

// HarnessSockPath returns the expected harness socket path for a session,
// given the iris run directory and the session instance_id.
func HarnessSockPath(runDir, instanceID string) string {
	sessionDir := filepath.Join(runDir, instanceID)
	return filepath.Join(sessionDir, "harness.sock")
}

// EnsureSessionDir creates the per-session run directory if it does not exist.
func EnsureSessionDir(runDir, instanceID string) (string, error) {
	dir := filepath.Join(runDir, instanceID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("iris: create session dir %q: %w", dir, err)
	}
	return dir, nil
}
