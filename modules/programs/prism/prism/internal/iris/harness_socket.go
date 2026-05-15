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

	// sessionShutdownReceived is set to true when the session_shutdown
	// frame arrives — used by the supervisor to detect clean exits.
	sessionShutdownReceived bool
	shutdownMu              sync.Mutex
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
func (h *HarnessSocketServer) AcceptOne(ctx context.Context) error {
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
		h.listener.Close()
		return ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return fmt.Errorf("iris: harness socket accept: %w", res.err)
		}
		return h.handleConn(ctx, res.conn)
	}
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
			go h.dispatchToolExec(ctx, w, frame)

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
			// Extract pi_session_path if present — used for restart continuation.
			var statusFrame map[string]any
			if err := json.Unmarshal(line, &statusFrame); err == nil {
				if sessionID, ok := statusFrame["session_id"].(string); ok && sessionID != "" {
					// Reconstruct pi session path from session_id and worktree.
					// Per pi-rpc-interface.md Q5, path is:
					//   ~/.pi/agent/sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl
					// We store the session_id here; the full path requires listing
					// the pi sessions dir. For D-3 we store the session_id for D-9.
					_ = sessionID // stored for D-9 orphan detection
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

	// Write tool_call event to DB (AC: every tool_exec → tool_call event).
	payloadBytes, _ := json.Marshal(map[string]any{
		"id":   frame.ID,
		"name": frame.Name,
		"args": frame.Args,
	})
	h.writeEvent("tool_call", payloadBytes)

	// Execute the tool.
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
		writer:     w,
		abortCh:    abortCh,
		toolExecID: frame.ID,
	}
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

// writeEvent writes an event row to the iris DB.
// InstanceID is set only when a sessions row exists for this instance — the
// FK constraint on agent_events.instance_id requires the parent row to exist
// before we can reference it.
func (h *HarnessSocketServer) writeEvent(eventType string, payload []byte) {
	event := db.Event{
		ID:          uuid.New().String(),
		SessionName: h.sess.SessionName,
		Repo:        "", // iris does not have a repo field in D-3
		Worktree:    h.sess.Worktree,
		Type:        eventType,
		Payload:     string(payload),
		CreatedAt:   time.Now(),
		// InstanceID is deliberately left nil here; the Supervisor inserts the
		// sessions row and the FK constraint requires the parent to exist first.
		// The event is still recorded and queryable by session_name.
	}
	if err := h.database.WriteEvent(event); err != nil {
		log.Printf("[iris] harness: failed to write %s event: %v", eventType, err)
	}
}

// writeObservationEvent writes a generic observation event (forwarded as-is)
// to the iris DB. InstanceID is left nil for the same FK reason as writeEvent.
func (h *HarnessSocketServer) writeObservationEvent(eventType string, rawLine []byte) {
	event := db.Event{
		ID:          uuid.New().String(),
		SessionName: h.sess.SessionName,
		Repo:        "",
		Worktree:    h.sess.Worktree,
		Type:        eventType,
		Payload:     string(rawLine),
		CreatedAt:   time.Now(),
	}
	if err := h.database.WriteEvent(event); err != nil {
		log.Printf("[iris] harness: failed to write %s observation event: %v", eventType, err)
	}
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
