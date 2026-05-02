package sidecar

// Integration tests for runStartupSocketPipe (P2.SIDECAR).
//
// These tests use a "fake extension" — a goroutine that dials the sidecar's
// Unix socket, exchanges JSONL frames, and validates that state transitions
// and event persistence work as specified in P2.WIRE.

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// maxSunPath is the POSIX limit for sockaddr_un.sun_path (104 on macOS, 108 on Linux).
// We use 104 as the conservative cross-platform ceiling.
const maxSunPath = 104

// shortSockPath creates a temp directory under os.TempDir() with a short
// prefix and returns a socket path whose total length is guaranteed to be
// within the 104-character POSIX limit for sockaddr_un.sun_path.
// The directory is automatically removed when the test ends.
func shortSockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatalf("shortSockPath MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "p.sock")
	if len(p) > maxSunPath {
		t.Fatalf("socket path too long (%d > %d): %s", len(p), maxSunPath, p)
	}
	return p
}

// newSocketPipeSidecar creates a Sidecar configured for socket-pipe testing.
// The caller must set cfg.HarnessPipeSockPath before calling runStartupSocketPipe.
func newSocketPipeSidecar(t *testing.T, sockPath string) *Sidecar {
	t.Helper()
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName:           "testrepo@main",
		Repo:                  "testrepo",
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             "worker",
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		Harness:               pih.New("", "", ""),
	}
	return New(cfg)
}

// dialAndHandshake dials the socket, waits for it to appear, sends hello, and
// returns the conn and the hello_ack frame. Fails the test on any error.
func dialAndHandshake(t *testing.T, sockPath string) (net.Conn, map[string]any) {
	t.Helper()

	// Wait for the socket file to exist (sidecar may still be binding).
	deadline := time.Now().Add(3 * time.Second)
	var conn net.Conn
	var err error
	for {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for socket %s: %v", sockPath, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Send hello.
	hello := map[string]any{
		"type":             "hello",
		"protocol_version": 1,
		"harness":          "pi",
		"harness_version":  "0.1.0-test",
	}
	sendJSON(t, conn, hello)

	// Read hello_ack.
	ack := readJSON(t, conn)
	if ack["type"] != "hello_ack" {
		t.Fatalf("expected hello_ack, got %q", ack["type"])
	}
	return conn, ack
}

// sendJSON marshals v as JSONL and writes it to conn.
func sendJSON(t *testing.T, conn net.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("sendJSON marshal: %v", err)
	}
	b = append(b, '\n')
	if _, err := conn.Write(b); err != nil {
		t.Fatalf("sendJSON write: %v", err)
	}
}

// readJSON reads one JSONL line from conn and returns it as a map.
func readJSON(t *testing.T, conn net.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{}) //nolint:errcheck
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("readJSON read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("readJSON unmarshal: %v", err)
	}
	return m
}

// runSocketPipeSidecar starts runStartupSocketPipe in a goroutine and returns
// a function that waits for it to finish and returns its error.
func runSocketPipeSidecar(sc *Sidecar) (wait func() error) {
	var (
		mu  sync.Mutex
		err error
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		e := sc.runStartupSocketPipe(context.Background())
		mu.Lock()
		err = e
		mu.Unlock()
	}()
	return func() error {
		<-done
		mu.Lock()
		defer mu.Unlock()
		return err
	}
}

// ── tests ────────────────────────────────────────────────────────────────────

// TestSocketPipe_Handshake_HelloAck verifies that after a valid hello the
// sidecar sends hello_ack with the correct fields.
func TestSocketPipe_Handshake_HelloAck(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)

	wait := runSocketPipeSidecar(sc)

	conn, ack := dialAndHandshake(t, sockPath)

	if got := ack["type"]; got != "hello_ack" {
		t.Errorf("hello_ack type = %v, want hello_ack", got)
	}
	if got := ack["protocol_version"]; got != float64(1) {
		t.Errorf("hello_ack protocol_version = %v, want 1", got)
	}
	if got := ack["session_name"]; got != "testrepo@main" {
		t.Errorf("hello_ack session_name = %v, want testrepo@main", got)
	}
	if got := ack["session_role"]; got != "worker" {
		t.Errorf("hello_ack session_role = %v, want worker", got)
	}

	// Send shutdown so the sidecar can exit.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_StateChange drives state_change frames and verifies DB state.
func TestSocketPipe_StateChange(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Send active.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})

	// Poll DB until state matches.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if s == string(agent.StateActive) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached active, got %q", s)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Send idle.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "idle"})
	deadline = time.Now().Add(2 * time.Second)
	for {
		s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if s == string(agent.StateIdle) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached idle, got %q", s)
		}
		time.Sleep(20 * time.Millisecond)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_EventFrames verifies that tool_call, tool_result, and
// msg_assistant frames are persisted to agent_events.
func TestSocketPipe_EventFrames(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{
		"type":    "tool_call",
		"tool_id": "bash",
		"input":   "ls",
	})
	sendJSON(t, conn, map[string]any{
		"type":    "tool_result",
		"tool_id": "bash",
		"output":  "file1\nfile2\n",
	})
	sendJSON(t, conn, map[string]any{
		"type":    "msg_assistant",
		"content": "Here are the files.",
	})

	// Shutdown and wait.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	eventTypes := map[string]bool{}
	for _, ev := range events {
		eventTypes[ev.Type] = true
	}
	for _, want := range []string{"tool_call", "tool_result", "msg_assistant"} {
		if !eventTypes[want] {
			t.Errorf("event type %q not found in agent_events; got types: %v", want, eventTypes)
		}
	}
}

// TestSocketPipe_SessionShutdown verifies that session_shutdown marks the
// session finished.
func TestSocketPipe_SessionShutdown(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()

	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if s != string(agent.StateFinished) {
		t.Errorf("state after shutdown = %q, want %q", s, agent.StateFinished)
	}
}

// TestSocketPipe_ProtocolVersionMismatch verifies that a hello with the wrong
// protocol version is rejected with a clear error frame, not a hang.
func TestSocketPipe_ProtocolVersionMismatch(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	deadline := time.Now().Add(3 * time.Second)
	var conn net.Conn
	var err error
	for {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for socket: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Send hello with wrong protocol version.
	sendJSON(t, conn, map[string]any{
		"type":             "hello",
		"protocol_version": 99,
		"harness":          "pi",
		"harness_version":  "0.1.0-test",
	})

	// Read the error frame.
	errFrame := readJSON(t, conn)
	if got := errFrame["type"]; got != "error" {
		t.Errorf("expected error frame, got type %v", got)
	}
	code, _ := errFrame["code"].(string)
	if code != "protocol_version_unsupported" {
		t.Errorf("error code = %q, want protocol_version_unsupported", code)
	}
	conn.Close()

	runErr := wait()
	if runErr == nil {
		t.Error("expected runStartupSocketPipe to return error on version mismatch")
	}
	if !strings.Contains(runErr.Error(), "protocol version") {
		t.Errorf("error message should mention protocol version, got: %v", runErr)
	}
}

// TestSocketPipe_ProtocolVersionTooOld verifies that protocol_version < 1
// yields a "too old" error code.
func TestSocketPipe_ProtocolVersionTooOld(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	deadline := time.Now().Add(3 * time.Second)
	var conn net.Conn
	var err error
	for {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for socket: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	sendJSON(t, conn, map[string]any{
		"type":             "hello",
		"protocol_version": 0,
		"harness":          "pi",
		"harness_version":  "0.1.0-test",
	})

	errFrame := readJSON(t, conn)
	code, _ := errFrame["code"].(string)
	if code != "protocol_version_too_old" {
		t.Errorf("error code = %q, want protocol_version_too_old", code)
	}
	conn.Close()
	_ = wait()
}

// TestSocketPipe_MalformedFrame verifies that a malformed JSONL frame is
// logged and skipped — the session continues, not fatal.
func TestSocketPipe_MalformedFrame(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Send a non-JSON line.
	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatalf("write malformed frame: %v", err)
	}

	// Session should continue — send shutdown.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()

	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe should not fatal on malformed frame, got: %v", err)
	}
}

// TestSocketPipe_PrematureDisconnect verifies that an unexpected disconnect
// marks the session as error state.
func TestSocketPipe_PrematureDisconnect(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Close without sending session_shutdown.
	conn.Close()

	if err := wait(); err != nil {
		// Error return is acceptable on unexpected disconnect.
		_ = err
	}

	s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if s != string(agent.StateError) && s != string(agent.StateFinished) {
		t.Errorf("state after premature disconnect = %q, want error or finished", s)
	}
}

// TestSocketPipe_DeliverPrompt verifies that Sidecar.DeliverPrompt enqueues
// a prompt frame that the extension can receive.
func TestSocketPipe_DeliverPrompt(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Use a large-buffer reader that doesn't get confused by the already-read
	// hello_ack (which was consumed by dialAndHandshake using its own reader).
	// We need a fresh reader bound to conn for the remaining frames.
	rd := bufio.NewReader(conn)

	// Deliver a prompt from the sidecar side.
	const promptText = "Hello from the coordinator!"
	go func() {
		// Small sleep to ensure the reader goroutine is ready.
		time.Sleep(50 * time.Millisecond)
		sc.DeliverPrompt(promptText, "nextTurn")
	}()

	// Read the prompt frame from the extension side.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := rd.ReadBytes('\n')
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatalf("read prompt frame: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(line, &frame); err != nil {
		t.Fatalf("unmarshal prompt frame: %v", err)
	}
	if got := frame["type"]; got != "prompt" {
		t.Errorf("frame type = %v, want prompt", got)
	}
	if got := frame["text"]; got != promptText {
		t.Errorf("frame text = %v, want %q", got, promptText)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_UnknownFramePersisted verifies that unknown frame types are
// persisted to agent_events for forward compatibility.
func TestSocketPipe_UnknownFramePersisted(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{
		"type":    "future_frame_type",
		"payload": "some data",
	})

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()

	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	for _, ev := range events {
		if ev.Type == "future_frame_type" {
			return // found — pass
		}
	}
	t.Error("unknown frame type not persisted to agent_events")
}

// TestSocketPipe_NeitherSocketNorTCP verifies that a sidecar with no socket
// path and no TCP port returns an error immediately.
func TestSocketPipe_NeitherSocketNorTCP(t *testing.T) {
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName: "testrepo@main",
		Repo:        "testrepo",
		Worktree:    t.TempDir(),
		DB:          d,
		Clock:       clk,
		AgentRole:   "worker",
		HarnessName: "pi",
		// HarnessPipeSockPath and HarnessPipeTCPPort intentionally omitted.
		Harness: pih.New("", "", ""),
	}
	sc := New(cfg)
	err := sc.runStartupSocketPipe(context.Background())
	if err == nil {
		t.Fatal("expected error when neither socket path nor TCP port is configured")
	}
	if !strings.Contains(err.Error(), "neither") {
		t.Errorf("error should mention 'neither', got: %v", err)
	}
}

// TestSocketPipe_TransportShapeRegistered verifies that the pi harness is
// registered with TransportSocketPipe shape.
func TestSocketPipe_TransportShapeRegistered(t *testing.T) {
	shape, ok := harness.ShapeOf("pi")
	if !ok {
		t.Fatal("harness 'pi' not registered")
	}
	if shape != harness.TransportSocketPipe {
		t.Errorf("pi harness shape = %v, want TransportSocketPipe", shape)
	}
}

// TestSocketPipe_SidecarDispatchesSocketPipe verifies that the sidecar
// dispatches to runStartupSocketPipe for the pi harness shape.
// We check this indirectly: Run() with a socket-pipe harness should bind
// the socket (creating the file) shortly after start.
func TestSocketPipe_SidecarDispatchesSocketPipe(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runErr error
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		runErr = sc.Run(ctx)
	}()

	// Wait for the socket file to appear — proves dispatch happened.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sockPath); err == nil {
			break // socket exists
		}
		if time.Now().After(deadline) {
			t.Fatal("socket file never appeared — sidecar may not have dispatched to runStartupSocketPipe")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Connect and do a minimal handshake + shutdown.
	conn, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Error("Run() did not finish within timeout after session_shutdown")
	}
	_ = runErr
}

// TestSocketPipe_HostAPIPromptSelfWorkerBypassesAuth verifies that a worker
// PI session can be self-prompted via the host-API /prompt endpoint, bypassing
// the worker→own-coordinator-only auth check that the HTTP-port path enforces.
//
// The bypass is necessary because the host-side `prism prompt <pi-session>`
// CLI dials the per-session host-API socket directly — the request targets
// the sidecar's own session, so there is no cross-session security boundary
// to enforce. Without the bypass, a worker PI session could not be prompted
// from the host CLI at all (P2.SPAWN edge-case AC, #1212).
func TestSocketPipe_HostAPIPromptSelfWorkerBypassesAuth(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	defer conn.Close()

	// Drive the host-API /prompt as if the host CLI dialled hostapi.sock and
	// targeted the sidecar's own session. The sidecar role is "worker" — under
	// the cross-session rules, a worker may only prompt its own coordinator;
	// the bypass is what permits a same-session target.
	rr := doHostAPI(t, sc, "POST", "/prompt",
		`{"session":"testrepo@main","prompt":"hello pi"}`)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (bypass should permit same-session worker self-prompt); body=%s",
			rr.Code, rr.Body.String())
	}

	// The sidecar must have enqueued a prompt frame to the extension.
	rd := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := rd.ReadBytes('\n')
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatalf("read prompt frame: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(line, &frame); err != nil {
		t.Fatalf("unmarshal prompt frame: %v", err)
	}
	if frame["type"] != "prompt" || frame["text"] != "hello pi" {
		t.Errorf("prompt frame = %v, want type=prompt text=hello pi", frame)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// Ensure db import is used.
var _ *db.DB
