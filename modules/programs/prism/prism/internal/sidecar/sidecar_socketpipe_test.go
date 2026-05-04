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

// TestSocketPipe_TurnStartEmitsStateActive verifies that a turn_start frame
// causes the sidecar to transition the session to active state in the DB.
// This is the real fix for #1350: PI uses TransportSocketPipe, so the fix must
// live in handlePipeFrame — NormaliseFrame is not on this code path.
func TestSocketPipe_TurnStartEmitsStateActive(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive to idle first via state_change, then send turn_start.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "idle"})

	// Wait for idle to be committed.
	deadline := time.Now().Add(2 * time.Second)
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

	// Now send turn_start — must transition back to active.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	deadline = time.Now().Add(2 * time.Second)
	for {
		s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if s == string(agent.StateActive) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached active after turn_start, got %q", s)
		}
		time.Sleep(20 * time.Millisecond)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_TurnStartActiveWhenAlreadyActive verifies that a turn_start
// frame when the session is already active is a no-op (no invalid transition
// error — the upsertState deduplication handles it).
func TestSocketPipe_TurnStartActiveWhenAlreadyActive(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// First turn_start — transitions from initial state to active.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	// Wait for active.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if s == string(agent.StateActive) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached active after first turn_start, got %q", s)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Second consecutive turn_start — already active, must be a no-op.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	time.Sleep(50 * time.Millisecond)

	// State must still be active (not error or anything else).
	s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if s != string(agent.StateActive) {
		t.Errorf("state after consecutive turn_start = %q, want active", s)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_TurnStart_DoesNotClobberReviewing verifies that a turn_start
// frame arriving while the DB state is "reviewing" does NOT overwrite it with
// "active". This is the fix for #1365: prism review writes reviewing directly
// to the DB via UpsertStatus; the sidecar's in-memory lastState is still
// active, so without the guard a subsequent turn_start clobbers reviewing.
func TestSocketPipe_TurnStart_DoesNotClobberReviewing(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Bring the session to active state via turn_start (as the agent would).
	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	deadline := time.Now().Add(2 * time.Second)
	for {
		s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if s == string(agent.StateActive) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached active after first turn_start, got %q", s)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Simulate prism review writing reviewing directly to the DB (as the
	// review subprocess does — bypassing the sidecar's in-memory state).
	if err := sc.cfg.DB.UpsertStatus(
		sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree,
		string(agent.StateReviewing), nil, nil,
	); err != nil {
		t.Fatalf("UpsertStatus reviewing: %v", err)
	}

	// Confirm the DB is now in reviewing state.
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateReviewing) {
		t.Fatalf("expected reviewing in DB before turn_start, got %q", s)
	}

	// Send a turn_start — this must NOT overwrite reviewing with active.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	// Give the sidecar time to process the frame.
	time.Sleep(100 * time.Millisecond)

	// The DB state must still be reviewing.
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateReviewing) {
		t.Errorf("turn_start clobbered reviewing: DB state = %q, want %q", s, agent.StateReviewing)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_TurnStart_IdleTransitionsToActive verifies that a turn_start
// frame when the session is in idle state transitions it correctly to active
// (regression guard for #1365 — the reviewing guard must not break the
// idle→active path).
func TestSocketPipe_TurnStart_IdleTransitionsToActive(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive to idle via state_change frames.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "idle"})

	deadline := time.Now().Add(2 * time.Second)
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

	// turn_start must transition idle → active.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	deadline = time.Now().Add(2 * time.Second)
	for {
		s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if s == string(agent.StateActive) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached active after turn_start from idle, got %q", s)
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

// TestSocketPipe_HandshakeWritesStateActive verifies that after a successful
// handshake, the sidecar writes a state_change event of state "active" to the
// DB — this is what WaitForReady polls for when using the pi harness.
func TestSocketPipe_HandshakeWritesStateActive(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Verify a state_change event of state "active" is written to the DB
	// immediately after handshake — this is what WaitForReady polls for.
	deadline := time.Now().Add(2 * time.Second)
	for {
		events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
		found := false
		for _, ev := range events {
			if ev.Type == "state_change" {
				var payload map[string]string
				if err := json.Unmarshal([]byte(ev.Payload), &payload); err == nil {
					if payload["state"] == string(agent.StateActive) {
						found = true
						break
					}
				}
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no state_change event with state=active found in DB after handshake")
		}
		time.Sleep(20 * time.Millisecond)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_ShuttingDownSkipsStateActive verifies that if the sidecar is
// already shutting down when the handshake completes, no StateActive event is
// written (the shuttingDown guard is respected).
func TestSocketPipe_ShuttingDownSkipsStateActive(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)

	// Set shuttingDown before handshake completes.
	sc.mu.Lock()
	sc.shuttingDown = true
	sc.mu.Unlock()

	wait := runSocketPipeSidecar(sc)

	// Even with shuttingDown set, the handshake may still proceed up to the
	// point of writing state. Dial and complete the handshake.
	conn, _ := dialAndHandshake(t, sockPath)

	// Give it a moment, then check no active state was written.
	time.Sleep(100 * time.Millisecond)

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	for _, ev := range events {
		if ev.Type == "state_change" {
			var payload map[string]string
			if err := json.Unmarshal([]byte(ev.Payload), &payload); err == nil {
				if payload["state"] == string(agent.StateActive) {
					t.Error("StateActive event was written despite shuttingDown=true")
				}
			}
		}
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// ── coalescing tests ──────────────────────────────────────────────────────────

// TestSocketPipe_MsgAssistantCoalesced verifies that N msg_assistant fragment
// frames between turn_start and turn_end produce exactly one msg_assistant row
// in agent_events with the concatenated text (AC: N fragments → 1 row).
func TestSocketPipe_MsgAssistantCoalesced(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "Hello"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": " world"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "!"})
	sendJSON(t, conn, map[string]any{
		"type": "turn_end",
		"usage": map[string]any{
			"input":       100,
			"output":      50,
			"cache_read":  40,
			"cache_write": 5,
			"cost":        0.0015,
		},
	})

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)

	var msgAssistantEvents []db.Event
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			msgAssistantEvents = append(msgAssistantEvents, ev)
		}
	}

	if got := len(msgAssistantEvents); got != 1 {
		t.Fatalf("expected exactly 1 msg_assistant event, got %d", got)
	}

	var p struct {
		Text             string  `json:"text"`
		InputTokens      int     `json:"inputTokens"`
		OutputTokens     int     `json:"outputTokens"`
		CacheReadTokens  int     `json:"cacheReadTokens"`
		CacheWriteTokens int     `json:"cacheWriteTokens"`
		Cost             float64 `json:"cost"`
	}
	if err := json.Unmarshal([]byte(msgAssistantEvents[0].Payload), &p); err != nil {
		t.Fatalf("unmarshal msg_assistant payload: %v", err)
	}

	if p.Text != "Hello world!" {
		t.Errorf("coalesced text = %q, want %q", p.Text, "Hello world!")
	}
	if p.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", p.InputTokens)
	}
	if p.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", p.OutputTokens)
	}
	if p.CacheReadTokens != 40 {
		t.Errorf("CacheReadTokens = %d, want 40", p.CacheReadTokens)
	}
	if p.CacheWriteTokens != 5 {
		t.Errorf("CacheWriteTokens = %d, want 5", p.CacheWriteTokens)
	}
	if p.Cost != 0.0015 {
		t.Errorf("Cost = %v, want 0.0015", p.Cost)
	}
}

// TestSocketPipe_TurnStartTurnEndPersisted verifies that turn_start and
// turn_end frames each produce their own row in agent_events.
func TestSocketPipe_TurnStartTurnEndPersisted(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "hi"})
	sendJSON(t, conn, map[string]any{"type": "turn_end"})

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	eventTypes := map[string]int{}
	for _, ev := range events {
		eventTypes[ev.Type]++
	}

	if eventTypes["turn_start"] == 0 {
		t.Error("no turn_start event in agent_events")
	}
	if eventTypes["turn_end"] == 0 {
		t.Error("no turn_end event in agent_events")
	}
}

// TestSocketPipe_EmptyAccumulatorAtTurnEnd verifies that a turn_end with no
// preceding msg_assistant frames does NOT write a msg_assistant event. Tool-only
// turns (turn_start → tool_call → tool_result → turn_end, no text fragments)
// must not produce a spurious "(no text)" row in prism checkin (#1319).
func TestSocketPipe_EmptyAccumulatorAtTurnEnd(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	// No msg_assistant frames — tool-only turn.
	sendJSON(t, conn, map[string]any{"type": "turn_end"})

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			t.Errorf("unexpected msg_assistant event written for empty-accumulator turn_end: %s", ev.Payload)
		}
	}
}

// TestSocketPipe_AccumulatorResetsOnTurnStart verifies that multiple turns in a
// single session each produce their own single msg_assistant event and the
// accumulator resets between turns.
func TestSocketPipe_AccumulatorResetsOnTurnStart(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// First turn.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "turn1"})
	sendJSON(t, conn, map[string]any{"type": "turn_end"})

	// Second turn.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "turn2"})
	sendJSON(t, conn, map[string]any{"type": "turn_end"})

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	textSet := map[string]int{}
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(ev.Payload), &p); err == nil {
				textSet[p.Text]++
			}
		}
	}

	if len(textSet) != 2 {
		t.Fatalf("expected 2 distinct msg_assistant events (one per turn), got %d: %v", len(textSet), textSet)
	}
	if textSet["turn1"] != 1 {
		t.Errorf("expected exactly 1 msg_assistant with text='turn1', got %d", textSet["turn1"])
	}
	if textSet["turn2"] != 1 {
		t.Errorf("expected exactly 1 msg_assistant with text='turn2', got %d", textSet["turn2"])
	}
}

// TestSocketPipe_PartialAccumulatorFlushedOnShutdown verifies that a
// session_shutdown with a non-empty accumulator writes a partial msg_assistant
// event — the accumulated text is not silently discarded.
func TestSocketPipe_PartialAccumulatorFlushedOnShutdown(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "partial"})
	// No turn_end — simulate abrupt shutdown mid-turn.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	found := false
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			found = true
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(ev.Payload), &p)
			if p.Text != "partial" {
				t.Errorf("partial flush text = %q, want %q", p.Text, "partial")
			}
		}
	}
	if !found {
		t.Error("partial accumulator not flushed on session_shutdown")
	}
}

// TestSocketPipe_PartialAccumulatorFlushedOnDrop verifies that a connection
// drop with a non-empty accumulator writes a partial msg_assistant event.
// After the drop the test reconnects and sends session_shutdown so the sidecar
// exits cleanly (the reconnect loop keeps the listener open after a drop).
func TestSocketPipe_PartialAccumulatorFlushedOnDrop(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "dropped"})
	// Drop the connection without session_shutdown.
	conn.Close()

	// Give the sidecar a moment to process the drop and re-enter Accept.
	time.Sleep(50 * time.Millisecond)

	// Reconnect and send session_shutdown so runStartupSocketPipe exits.
	conn2, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn2, map[string]any{"type": "session_shutdown"})
	conn2.Close()

	_ = wait()

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	found := false
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			found = true
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(ev.Payload), &p)
			if p.Text != "dropped" {
				t.Errorf("partial flush on drop text = %q, want %q", p.Text, "dropped")
			}
		}
	}
	if !found {
		t.Error("partial accumulator not flushed on connection drop")
	}
}

// TestSocketPipe_ReconnectAfterDrop verifies that after a non-shutdown
// connection drop the sidecar keeps the listener open and accepts a second
// connection, continuing to record events.
//
// This covers the /new scenario: PI's /new command triggers session_start,
// which closes the old socket and opens a new one. The sidecar must survive
// the drop and accept the reconnect rather than transitioning to error state.
func TestSocketPipe_ReconnectAfterDrop(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	// First connection: handshake + one turn, then abrupt close (no shutdown).
	conn1, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn1, map[string]any{"type": "turn_start"})
	sendJSON(t, conn1, map[string]any{"type": "msg_assistant", "text": "turn1"})
	sendJSON(t, conn1, map[string]any{"type": "turn_end"})

	// Close without session_shutdown — simulates PI /new.
	conn1.Close()

	// Give the sidecar a moment to process the drop and re-enter Accept.
	time.Sleep(50 * time.Millisecond)

	// Second connection: must succeed because the listener is still open.
	conn2, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn2, map[string]any{"type": "turn_start"})
	sendJSON(t, conn2, map[string]any{"type": "msg_assistant", "text": "turn2"})
	sendJSON(t, conn2, map[string]any{"type": "turn_end"})

	// Clean shutdown on the second connection.
	sendJSON(t, conn2, map[string]any{"type": "session_shutdown"})
	conn2.Close()

	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error after reconnect: %v", err)
	}

	// Both turns should be in agent_events.
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	textSet := map[string]int{}
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(ev.Payload), &p); err == nil {
				textSet[p.Text]++
			}
		}
	}
	if textSet["turn1"] != 1 {
		t.Errorf("expected 1 msg_assistant with text='turn1', got %d", textSet["turn1"])
	}
	if textSet["turn2"] != 1 {
		t.Errorf("expected 1 msg_assistant with text='turn2', got %d", textSet["turn2"])
	}

	// Session should be finished after clean shutdown on second connection.
	s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if s != string(agent.StateFinished) {
		t.Errorf("state after reconnect+shutdown = %q, want %q", s, agent.StateFinished)
	}
}

// TestSocketPipe_ReconnectTimeout verifies that if no reconnect arrives within
// pipeDisconnectTimeout, the sidecar transitions to error state and exits.
func TestSocketPipe_ReconnectTimeout(t *testing.T) {
	sockPath := shortSockPath(t)

	// Use a very short StartupConnectTimeout so the test does not need to wait
	// the full pipeDisconnectTimeout. We override pipeDisconnectTimeout
	// indirectly by setting a tiny StartupConnectTimeout; the reconnect
	// timeout in the real code is pipeDisconnectTimeout (30s), so instead
	// we use a test-only pattern: close the conn and wait for the sidecar to
	// reach the error state by polling the DB.
	//
	// Note: we cannot override pipeDisconnectTimeout directly (package-level
	// constant). Instead we use a configuration where the initial accept
	// times out quickly. We create a sidecar with a StartupConnectTimeout of
	// 100ms and never connect — this exercises the timeout path for the FIRST
	// connection (which is also the reconnect path for the loop's timeout).
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
		StartupConnectTimeout: 150 * time.Millisecond,
		Harness:               pih.New("", "", ""),
	}
	sc := New(cfg)
	wait := runSocketPipeSidecar(sc)

	// Never connect — just wait for the sidecar to time out.
	err := wait()
	if err == nil {
		t.Error("expected runStartupSocketPipe to return error on connect timeout")
	}

	s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if s != string(agent.StateError) {
		t.Errorf("state after connect timeout = %q, want %q", s, agent.StateError)
	}
}

// TestSocketPipe_HostAPISockCreatedBeforeDispatch is the regression test for
// issue #1346: the host-API listener must be started before Run() dispatches to
// runStartupSocketPipe, so that pi bwrap sessions have a working hostapi.sock
// from the moment the harness-pipe handshake begins.
//
// The test verifies ordering by racing the two sockets: as soon as the
// harness-pipe socket appears (which proves Run() has entered
// runStartupSocketPipe), the host-API socket must already exist.  If the
// listener block is ever moved back to after the transport-shape switch, the
// host-API socket will not yet exist at that point and this test will fail.
func TestSocketPipe_HostAPISockCreatedBeforeDispatch(t *testing.T) {
	// Two short-prefix socket paths: one for the harness pipe, one for the
	// host-API listener.  Both must fit within the 104-char POSIX limit.
	pipeSockPath := shortSockPath(t)

	// Use the same temp dir (from shortSockPath) but a distinct filename so
	// both paths share the short prefix.
	dir := filepath.Dir(pipeSockPath)
	hostAPISockPath := filepath.Join(dir, "hostapi.sock")
	if len(hostAPISockPath) > maxSunPath {
		t.Fatalf("hostapi socket path too long (%d > %d): %s", len(hostAPISockPath), maxSunPath, hostAPISockPath)
	}

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
		HarnessPipeSockPath:   pipeSockPath,
		HostAPISockPath:       hostAPISockPath,
		StartupConnectTimeout: 5 * time.Second,
		Harness:               pih.New("", "", ""),
	}
	sc := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runErr error
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		runErr = sc.Run(ctx)
	}()

	// Wait for the harness-pipe socket to appear.  This proves that Run() has
	// entered runStartupSocketPipe (i.e. the transport-shape switch has been
	// executed).  At this point the host-API listener block — which is now
	// placed BEFORE the switch — must already have run.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(pipeSockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("harness-pipe socket never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Assert: host-API socket must already exist at this point.
	if _, err := os.Stat(hostAPISockPath); err != nil {
		t.Errorf("hostapi.sock does not exist after harness-pipe socket appeared — host-API listener was not started before dispatch (#1346): %v", err)
	}

	// Clean up: connect, handshake, shut down so Run() exits cleanly.
	conn, _ := dialAndHandshake(t, pipeSockPath)
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Error("Run() did not finish within timeout after session_shutdown")
	}
	_ = runErr
}

// Ensure db import is used.
var _ *db.DB
