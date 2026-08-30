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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// derefStr renders a *string for a test failure message: the pointed-to value,
// or "<nil>" when the pointer is nil.
func derefStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

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
	// Redirect XDG_STATE_HOME so sidecar writes (touchDashboardSentinel,
	// socket paths, and more) never escape to the developer's real prism state.
	// Also activate the host-API socket guard.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
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
		// PipeReconnectTimeout is set to a short value so tests that close the
		// connection without a session_shutdown do not block for 30s.
		PipeReconnectTimeout: 200 * time.Millisecond,
		Harness:              pih.New("", "", ""),
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
		"protocol_version": 2,
		"harness":          "pi",
		"harness_version":  "0.1.0-test",
	}
	sendJSON(t, conn, hello)

	// Read hello_ack and post-handshake reviewing_state through a single
	// shared buffered reader to avoid the multi-bufio data-loss footgun
	// (if both frames arrive in one syscall, a fresh bufio per readJSON call
	// would discard the second frame). The post-handshake reviewing_state
	// frame initialises the extension's pendingReviewCall guard from the
	// authoritative sidecar-side flag.
	rd := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{}) //nolint:errcheck

	ackLine, ackErr := rd.ReadBytes('\n')
	if ackErr != nil {
		t.Fatalf("hello_ack read: %v", ackErr)
	}
	var ack map[string]any
	if err := json.Unmarshal(ackLine, &ack); err != nil {
		t.Fatalf("hello_ack unmarshal: %v", err)
	}
	if ack["type"] != "hello_ack" {
		t.Fatalf("expected hello_ack, got %q", ack["type"])
	}

	rsLine, rsErr := rd.ReadBytes('\n')
	if rsErr != nil {
		t.Fatalf("reviewing_state read: %v", rsErr)
	}
	var rs map[string]any
	if err := json.Unmarshal(rsLine, &rs); err != nil {
		t.Fatalf("reviewing_state unmarshal: %v", err)
	}
	if rs["type"] != "reviewing_state" {
		t.Fatalf("expected reviewing_state after hello_ack, got %q", rs["type"])
	}

	// Push any extra buffered bytes back onto the connection so the caller's
	// own reader sees the next frame intact. bufio.Reader.Buffered() returns
	// data we read into the bufio buffer but did not consume; if non-zero we
	// must surface it to the caller. The simplest correct path is to refuse
	// to leak buffered bytes: assert they are zero. Test-side writers feed
	// frames synchronously after the handshake, so in practice there is
	// nothing left in the bufio buffer beyond the two frames we read.
	if rd.Buffered() != 0 {
		t.Fatalf("dialAndHandshake: %d unconsumed bytes in bufio buffer after handshake — caller would lose data", rd.Buffered())
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
		mu   sync.Mutex
		err  error
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
	if got := ack["protocol_version"]; got != float64(2) {
		t.Errorf("hello_ack protocol_version = %v, want 2", got)
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
//
// Note: state_change{finished} is handled by the finished-debounce path
// (handleSessionFinished). The sidecar does not persist "finished" immediately;
// instead it starts a 2s debounce that writes StateFinished after it fires.
// This test verifies the active transition and that state_change{finished}
// starts the debounce without immediately overwriting the DB state to "finished".
func TestSocketPipe_StateChange(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
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

	// Send finished — this starts the debounce but does NOT write "finished" to the DB
	// immediately. The sidecar records a state_change event in agent_events and starts
	// the 2s debounce timer.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// A debounce timer must have been created. Wait deterministically on the
	// AfterFunc registration event rather than sleeping a fixed window: under
	// -race on a loaded runner the sidecar goroutine can be descheduled past a
	// fixed sleep, so LastTimer() would observe nil and the test would flake
	// (issue #2902).
	if timer := clk.WaitForTimerCount(1, 5*time.Second); timer == nil {
		t.Error("no finished debounce timer created after state_change{finished}")
	}

	// The DB state must NOT be finished yet (debounce has not fired).
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s == string(agent.StateFinished) {
		t.Errorf("DB state should not be 'finished' before debounce fires, got %q", s)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_TurnStartEmitsStateActive verifies that a turn_start frame
// causes the sidecar to transition the session to active state in the DB.
// PI uses TransportSocketPipe, so the fix must
// live in handlePipeFrame — NormaliseFrame is not on this code path.
//
// Note: state_change{finished} starts the 2s finished debounce. turn_start
// cancels the debounce and writes StateActive directly.
func TestSocketPipe_TurnStartEmitsStateActive(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive to active first, then trigger the finished debounce.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Wait deterministically for the finished debounce timer registration.
	idleTimer := clk.WaitForTimerCount(1, 5*time.Second)
	if idleTimer == nil {
		t.Fatal("no finished debounce timer created after state_change{finished}")
	}

	// Now send turn_start — must cancel the debounce and keep the session active.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	// Wait until the finished debounce timer is stopped (cancelled by turn_start).
	// The sidecar processes frames sequentially under s.mu: cancelIdleTimer is
	// called before any DB write, so once the timer is stopped we know turn_start
	// was processed.
	if !idleTimer.WaitStopped(5 * time.Second) {
		t.Fatal("finished debounce timer was not stopped by turn_start within timeout")
	}

	// State must be active (not finished).
	st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if st != string(agent.StateActive) {
		t.Errorf("state after turn_start = %q, want active", st)
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
// "active". prism review writes reviewing directly
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

	// Simulate the /review handler: write reviewing to the DB and set the
	// in-memory reviewingInFlight flag (which is now what handlePipeFrame's
	// turn_start guard checks instead of calling currentDBState).
	if err := sc.cfg.DB.UpsertStatus(
		sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree,
		string(agent.StateReviewing), nil, nil,
	); err != nil {
		t.Fatalf("UpsertStatus reviewing: %v", err)
	}
	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.mu.Unlock()

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
// frame when the session is in the idle-debounce window transitions it correctly
// to active (regression guard — the reviewing guard must not break
// the idle→active path).
//
// Note: state_change{finished} starts the debounce (not writing "finished"
// to DB immediately); we wait for the debounce timer, then send turn_start
// and verify the session reaches active with the debounce cancelled.
func TestSocketPipe_TurnStart_IdleTransitionsToActive(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive to active then trigger finished debounce via state_change frames.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Wait deterministically for the debounce timer registration.
	if clk.WaitForTimerCount(1, 5*time.Second) == nil {
		t.Fatal("no finished debounce timer created after state_change{finished}")
	}

	// turn_start must cancel the debounce and transition to active.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	deadline := time.Now().Add(2 * time.Second)
	for {
		s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if s == string(agent.StateActive) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached active after turn_start from idle-debounce, got %q", s)
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

// TestSocketPipe_ProtocolVersionTooOld verifies that protocol_version < 2
// (current supported version) yields a "too old" error code.
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
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
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
// from the host CLI at all (the spawn edge case).
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

// TestSocketPipe_ModelStampedOnMsgAssistant verifies that a `model` field on
// the turn_end frame is recorded as $.model on the coalesced msg_assistant row
// and refreshes agent_status.model_id / root_model_id. The format is
// providerID/modelID, matching the keys in
// internal/pricing.
func TestSocketPipe_ModelStampedOnMsgAssistant(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "hi"})
	sendJSON(t, conn, map[string]any{
		"type":  "turn_end",
		"model": "anthropic/claude-sonnet-4-6",
		"usage": map[string]any{"input": 2, "output": 293, "cost": 0.15},
	})

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	var found bool
	for _, ev := range events {
		if ev.Type != "msg_assistant" {
			continue
		}
		found = true
		var p struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
			t.Fatalf("unmarshal msg_assistant payload: %v", err)
		}
		if p.Model != "anthropic/claude-sonnet-4-6" {
			t.Errorf("$.model = %q, want %q", p.Model, "anthropic/claude-sonnet-4-6")
		}
	}
	if !found {
		t.Fatal("expected a msg_assistant event")
	}

	st, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("expected a status row")
	}
	if st.ModelID == nil || *st.ModelID != "anthropic/claude-sonnet-4-6" {
		t.Errorf("model_id = %v, want %q", derefStr(st.ModelID), "anthropic/claude-sonnet-4-6")
	}
	if st.RootModelID == nil || *st.RootModelID != "anthropic/claude-sonnet-4-6" {
		t.Errorf("root_model_id = %v, want %q", derefStr(st.RootModelID), "anthropic/claude-sonnet-4-6")
	}
}

// TestSocketPipe_SubagentModelDoesNotOverwriteRoot verifies that a turn_end
// frame from a subagent (a differing, non-empty `agent` field) records its own
// model on that turn's msg_assistant row but does NOT overwrite the root
// agent's recorded model in agent_status. This
// mirrors handleMessageUpdated's subagent-suppression semantic on the SSE path.
func TestSocketPipe_SubagentModelDoesNotOverwriteRoot(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	// The session's root agent is "worker" (set by newSocketPipeSidecar).
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Root-agent turn establishes the root model.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "root"})
	sendJSON(t, conn, map[string]any{
		"type":  "turn_end",
		"agent": "worker",
		"model": "anthropic/claude-opus-4-6",
	})

	// Subagent turn runs a different model — must not overwrite root's.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "sub"})
	sendJSON(t, conn, map[string]any{
		"type":  "turn_end",
		"agent": "subagent-x",
		"model": "anthropic/claude-haiku-4-5",
	})

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	// agent_status must still hold the root agent's model.
	st, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("expected a status row")
	}
	if st.RootModelID == nil || *st.RootModelID != "anthropic/claude-opus-4-6" {
		t.Errorf("root_model_id = %v, want root model %q (subagent must not overwrite)", derefStr(st.RootModelID), "anthropic/claude-opus-4-6")
	}
	if st.ModelID == nil || *st.ModelID != "anthropic/claude-opus-4-6" {
		t.Errorf("model_id = %v, want root model %q (subagent must not overwrite)", derefStr(st.ModelID), "anthropic/claude-opus-4-6")
	}

	// But each turn's own msg_assistant row records the model it ran on.
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	modelsByText := map[string]string{}
	for _, ev := range events {
		if ev.Type != "msg_assistant" {
			continue
		}
		var p struct {
			Text  string `json:"text"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
			t.Fatalf("unmarshal msg_assistant payload: %v", err)
		}
		modelsByText[p.Text] = p.Model
	}
	if modelsByText["root"] != "anthropic/claude-opus-4-6" {
		t.Errorf("root turn $.model = %q, want %q", modelsByText["root"], "anthropic/claude-opus-4-6")
	}
	if modelsByText["sub"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("subagent turn $.model = %q, want %q (per-turn accuracy)", modelsByText["sub"], "anthropic/claude-haiku-4-5")
	}
}

// TestSocketPipe_NoModelOnTurnEndWritesEmptyModel verifies the edge case: a
// turn_end frame that carries no `model` field still writes its msg_assistant
// row (with $.model empty) and leaves the agent_status model columns unchanged,
// without error.
func TestSocketPipe_NoModelOnTurnEndWritesEmptyModel(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "hi"})
	// turn_end with usage but NO model field.
	sendJSON(t, conn, map[string]any{
		"type":  "turn_end",
		"usage": map[string]any{"input": 2, "output": 10},
	})

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	var found bool
	for _, ev := range events {
		if ev.Type != "msg_assistant" {
			continue
		}
		found = true
		var p struct {
			Text  string `json:"text"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
			t.Fatalf("unmarshal msg_assistant payload: %v", err)
		}
		if p.Text != "hi" {
			t.Errorf("text = %q, want %q", p.Text, "hi")
		}
		if p.Model != "" {
			t.Errorf("$.model = %q, want empty for a frame with no model", p.Model)
		}
	}
	if !found {
		t.Fatal("expected a msg_assistant event even when the frame carries no model")
	}

	st, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st != nil && st.RootModelID != nil && *st.RootModelID != "" {
		t.Errorf("root_model_id = %q, want unchanged/empty when no model on the wire", *st.RootModelID)
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
// must not produce a spurious "(no text)" row in prism checkin.
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
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
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
// The host-API listener must be started before Run() dispatches to
// runStartupSocketPipe, so that pi bwrap sessions have a working hostapi.sock
// from the moment the harness-pipe handshake begins.
//
// The test verifies ordering by racing the two sockets: as soon as the
// harness-pipe socket appears (which proves Run() has entered
// runStartupSocketPipe), the host-API socket must already exist.  If the
// listener block is ever moved back to after the transport-shape switch, the
// host-API socket will not yet exist at that point and this test will fail.
func TestSocketPipe_HostAPISockCreatedBeforeDispatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
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
	// entered runStartupSocketPipe (that is, the transport-shape switch has been
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

// newSocketPipeSidecarWithClock creates a Sidecar for socket-pipe testing and
// also returns the testClock so callers can drive timers manually.
// PipeReconnectTimeout is set to a short value so tests that close the
// connection without a session_shutdown do not block for 30s.
func newSocketPipeSidecarWithClock(t *testing.T, sockPath string) (*Sidecar, *testClock) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
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
		PipeReconnectTimeout:  200 * time.Millisecond,
		Harness:               pih.New("", "", ""),
	}
	return New(cfg), clk
}

// countBusMessages returns the number of bus_messages rows addressed to the
// given session (both delivered and undelivered).
func countBusMessages(t *testing.T, d *db.DB, toSession string) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE to_session = ?", toSession).Scan(&n); err != nil {
		t.Fatalf("count bus_messages: %v", err)
	}
	return n
}

// ── state-gap tests ──────────────────────────────────────────────────────────

// TestSocketPipe_IdleDebounce_WritesFinishedAfterDebounce verifies that
// state_change{finished} starts the 2s finished debounce and, when it fires,
// writes StateFinished and calls notifyCoordinator (protocol v2).
//
// Synchronisation: rather than sleeping for a fixed 50ms before
// calling clk.LastTimer(), this test blocks on clk.WaitForTimerCount(1, ...)
// which observes the actual AfterFunc registration event. Under load the old
// 50ms sleep was not always enough for the sidecar goroutine to reach
// AfterFunc, so LastTimer() returned nil and the test failed deterministically
// in the Nix sandbox even though the production code was correct.
func TestSocketPipe_IdleDebounce_WritesFinishedAfterDebounce(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive to active, then send state_change{finished}.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Wait deterministically for the finished debounce timer to be registered.
	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer was created")
	}

	// State should NOT be finished yet (debounce has not fired).
	s := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if s == string(agent.StateFinished) {
		t.Error("state reached finished before finished debounce fired")
	}

	timer.Fire()

	// State must now be finished.
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if st == string(agent.StateFinished) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached finished after finished debounce, got %q", st)
		}
		time.Sleep(20 * time.Millisecond)
	}

	conn.Close()
	_ = wait()
}

// TestSocketPipe_IdleDebounce_CancelledByTurnStart verifies that a turn_start
// arriving while the finished debounce timer is running cancels the timer and
// transitions the session to StateActive, not StateFinished.
//
// Synchronisation: waits for timer registration via
// clk.WaitForTimerCount and for cancellation via timer.WaitStopped — no
// wall-clock sleeps. The DB-state assertion uses waitForState so a slow handler
// commit does not race with the assertion.
func TestSocketPipe_IdleDebounce_CancelledByTurnStart(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Wait for the finished debounce timer to be registered.
	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer was created after state_change{finished}")
	}

	// Send turn_start — must cancel the debounce.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	if !timer.WaitStopped(5 * time.Second) {
		t.Fatal("finished debounce timer was not stopped by turn_start within 5s")
	}

	// Firing the (now-cancelled) timer must not write StateFinished.
	timer.Fire()

	// State must be active after the turn_start (waitForState polls the DB to
	// avoid racing the handler's commit).
	st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateActive), 2*time.Second)
	if st == string(agent.StateFinished) {
		t.Errorf("state became finished after cancelled debounce, want active")
	}
	if st != string(agent.StateActive) {
		t.Errorf("state = %q after turn_start, want active", st)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestSocketPipe_MultipleIdle_OnlyOneTimer verifies that multiple consecutive
// state_change{finished} frames result in exactly one debounce timer running
// at a time — earlier timers are cancelled before a new one starts.
//
// Synchronisation: each timer is awaited via WaitForTimerCount
// (registration) and WaitStopped (cancellation), so the test does not depend
// on a fixed sleep being long enough for the sidecar goroutine to run.
func TestSocketPipe_MultipleIdle_OnlyOneTimer(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	// First finished signal: wait for timer #1 to be registered.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})
	firstTimer := clk.WaitForTimerCount(1, 5*time.Second)
	if firstTimer == nil {
		t.Fatal("no timer after first state_change{finished}")
	}

	// Second finished signal: wait for timer #2 — the first must already be stopped.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})
	secondTimer := clk.WaitForTimerCount(2, 5*time.Second)
	if secondTimer == nil {
		t.Fatal("no timer after second state_change{finished}")
	}
	if firstTimer == secondTimer {
		t.Fatal("same timer object — second state_change{finished} did not create a new timer")
	}
	if !firstTimer.WaitStopped(5 * time.Second) {
		t.Error("first finished debounce timer was not stopped when second state_change{finished} arrived")
	}

	// Firing the second timer must write StateFinished exactly once.
	secondTimer.Fire()

	// Also firing the first (already-stopped) timer must be a no-op.
	firstTimer.Fire()

	st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateFinished), 2*time.Second)
	if st != string(agent.StateFinished) {
		t.Errorf("state after second debounce = %q, want finished", st)
	}

	conn.Close()
	_ = wait()
}

// TestSocketPipe_ErrorState_CancelsTimers verifies that state_change{error}
// cancels any in-flight finished-debounce or recovery timer and records
// lastErrorAt, so a stale debounce cannot overwrite StateError.
//
// Synchronisation: WaitForTimerCount + WaitStopped + waitForState
// replace the previous fixed sleeps so the assertions wait on the actual
// events (timer registered, timer cancelled, DB state committed) rather than
// on wall-clock elapsed time.
func TestSocketPipe_ErrorState_CancelsTimers(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Start finished debounce and wait for the timer to be registered.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})
	idleTimer := clk.WaitForTimerCount(1, 5*time.Second)
	if idleTimer == nil {
		t.Fatal("no finished debounce timer after state_change{finished}")
	}

	// Send state_change{error} — must cancel the finished debounce timer.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "error"})
	if !idleTimer.WaitStopped(5 * time.Second) {
		t.Error("finished debounce timer was not stopped by state_change{error}")
	}

	// Firing the stale timer must not overwrite StateError.
	idleTimer.Fire()

	st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateError), 2*time.Second)
	if st != string(agent.StateError) {
		t.Errorf("state after stale debounce fire = %q, want error", st)
	}

	// lastErrorAt must be set (non-zero). Wait for the handler to commit the
	// field under s.mu — the state-change observation above guarantees the
	// frame has been processed, but lastErrorAt is set in the same critical
	// section so a poll under the lock is sufficient and race-free.
	if !waitForCondition(t, func() bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return !sc.lastErrorAt.IsZero()
	}, 2*time.Second, "lastErrorAt set after state_change{error}") {
		t.Error("lastErrorAt was not set after state_change{error}")
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestSocketPipe_TurnStart_ClearsEndedOnErrorResume verifies that a turn_start
// arriving after a state_change{error} clears ended_at in the DB so the session
// reappears in AllActiveStatus.
func TestSocketPipe_TurnStart_ClearsEndedOnErrorResume(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, _ := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive to error state.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "error"})
	time.Sleep(50 * time.Millisecond)

	// Manually set ended_at so the session would be invisible to AllActiveStatus.
	if err := sc.cfg.DB.SetEnded(sc.cfg.SessionName); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	// Verify session is invisible before the resume.
	active, err := sc.cfg.DB.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	for _, st := range active {
		if st.SessionName == sc.cfg.SessionName {
			t.Error("session visible in AllActiveStatus before resume (ended_at should exclude it)")
			break
		}
	}

	// Simulate resume via turn_start.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if st == string(agent.StateActive) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached active after resume, got %q", st)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Session must now be visible in AllActiveStatus.
	active, err = sc.cfg.DB.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	found := false
	for _, st := range active {
		if st.SessionName == sc.cfg.SessionName {
			found = true
			break
		}
	}
	if !found {
		t.Error("session not visible in AllActiveStatus after error resume (ended_at was not cleared)")
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestSocketPipe_TurnStart_ClearsEndedOnInterruptedResume verifies that a
// turn_start after a state_change{interrupted} also clears ended_at.
func TestSocketPipe_TurnStart_ClearsEndedOnInterruptedResume(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, _ := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive to interrupted via state_change.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "interrupted"})
	time.Sleep(50 * time.Millisecond)

	// Set ended_at manually (normally set by Shutdown).
	if err := sc.cfg.DB.SetEnded(sc.cfg.SessionName); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	// Resume via turn_start.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
		if st == string(agent.StateActive) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never reached active, got %q", st)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Session must be visible in AllActiveStatus.
	active, err := sc.cfg.DB.AllActiveStatus()
	if err != nil {
		t.Fatalf("AllActiveStatus: %v", err)
	}
	found := false
	for _, st := range active {
		if st.SessionName == sc.cfg.SessionName {
			found = true
			break
		}
	}
	if !found {
		t.Error("session not visible in AllActiveStatus after interrupted resume")
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestSocketPipe_WaitingState_CancelsIdleTimer verifies that state_change{waiting}
// cancels any in-flight finished-debounce timer so the session does not spuriously
// transition to StateFinished while waiting for user input.
//
// Synchronisation: WaitForTimerCount + WaitStopped + waitForState
// replace fixed sleeps; the test now reacts to the actual sidecar transitions.
func TestSocketPipe_WaitingState_CancelsIdleTimer(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Start finished debounce and wait for the timer.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})
	idleTimer := clk.WaitForTimerCount(1, 5*time.Second)
	if idleTimer == nil {
		t.Fatal("no finished debounce timer after state_change{finished}")
	}

	// Send state_change{waiting} — must cancel the finished debounce timer.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "waiting"})
	if !idleTimer.WaitStopped(5 * time.Second) {
		t.Error("finished debounce timer was not stopped by state_change{waiting}")
	}

	// Firing the stale timer must not write StateFinished.
	idleTimer.Fire()

	st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateWaiting), 2*time.Second)
	if st == string(agent.StateFinished) {
		t.Errorf("state became finished after cancelled finished debounce timer during waiting, want waiting")
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestSocketPipe_AutoRetryStart_CancelsIdleTimer verifies that an auto_retry_start
// frame cancels any in-flight finished-debounce timer so the session does not
// spuriously finish during the retry window.
//
// Synchronisation: WaitForTimerCount + WaitStopped replace fixed
// sleeps. The trailing state assertion is bounded by waitForCondition so a
// stray race cannot let StateFinished slip in unnoticed.
func TestSocketPipe_AutoRetryStart_CancelsIdleTimer(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Start finished debounce and wait for the timer.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})
	idleTimer := clk.WaitForTimerCount(1, 5*time.Second)
	if idleTimer == nil {
		t.Fatal("no finished debounce timer after state_change{finished}")
	}

	// Send auto_retry_start — must cancel the finished debounce timer.
	sendJSON(t, conn, map[string]any{"type": "auto_retry_start", "attempt": 1})
	if !idleTimer.WaitStopped(5 * time.Second) {
		t.Error("finished debounce timer was not stopped by auto_retry_start")
	}

	// Firing the stale timer must not write StateFinished. The state should
	// remain active (we transitioned via turn_start above and auto_retry_start
	// does not change the DB state).
	idleTimer.Fire()

	// Assert the state is NOT finished. We wait briefly to give any erroneous
	// transition a chance to surface, then assert. waitForState returns when
	// either the target is reached or the deadline passes — we explicitly want
	// the deadline path here, so we use a short window.
	st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateFinished), 200*time.Millisecond)
	if st == string(agent.StateFinished) {
		t.Errorf("state became finished after cancelled finished debounce timer during retry, want active")
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestSocketPipe_TurnEnd_UpdatesLastAssistantAgent verifies that a turn_end
// frame updates lastAssistantAgent so that handleSessionFinished()'s subagent-
// suppression logic works correctly for PI sessions.
//
// Synchronisation: turn_end has no observable timer or DB-state
// effect by itself, so the test polls sc.lastAssistantAgent under sc.mu via
// waitForCondition rather than racing a 50ms sleep.
func TestSocketPipe_TurnEnd_UpdatesLastAssistantAgent(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, _ := newSocketPipeSidecarWithClock(t, sockPath)
	// Set rootAgent to "worker" (same as AgentRole, set in New via cfg.AgentRole).
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Send a turn_start / turn_end for a subagent.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "sub output"})
	sendJSON(t, conn, map[string]any{
		"type":  "turn_end",
		"agent": "subagent",
	})

	// lastAssistantAgent must be "subagent" (not cleared because it != rootAgent).
	if !waitForCondition(t, func() bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return sc.lastAssistantAgent == "subagent"
	}, 5*time.Second, "lastAssistantAgent==\"subagent\" after subagent turn_end") {
		sc.mu.Lock()
		laa := sc.lastAssistantAgent
		sc.mu.Unlock()
		t.Errorf("lastAssistantAgent = %q after subagent turn_end, want %q", laa, "subagent")
	}

	// Now send a root-agent turn_end.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "root output"})
	sendJSON(t, conn, map[string]any{
		"type":  "turn_end",
		"agent": sc.rootAgent,
	})

	// lastAssistantAgent must be cleared after root-agent turn_end.
	if !waitForCondition(t, func() bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return sc.lastAssistantAgent == ""
	}, 5*time.Second, "lastAssistantAgent cleared after root turn_end") {
		sc.mu.Lock()
		laa := sc.lastAssistantAgent
		sc.mu.Unlock()
		t.Errorf("lastAssistantAgent = %q after root turn_end, want empty (cleared)", laa)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestSocketPipe_SessionShutdown_CancelsTimers verifies that session_shutdown
// cancels any in-flight finished-debounce/recovery timer before writing
// StateFinished so the coordinator receives exactly one notification.
//
// Synchronisation: WaitForTimerCount replaces the leading sleep;
// the post-shutdown assertion that no extra finished event was emitted now
// uses a brief polling window to give a (would-be-buggy) timer fire a chance
// to surface, rather than a fixed sleep that may be too short under load.
func TestSocketPipe_SessionShutdown_CancelsTimers(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Start finished debounce and wait for the timer.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})
	idleTimer := clk.WaitForTimerCount(1, 5*time.Second)
	if idleTimer == nil {
		t.Fatal("no finished debounce timer after state_change{finished}")
	}

	// Send session_shutdown immediately (before debounce fires).
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()

	// The idle timer must be stopped.
	if !idleTimer.WaitStopped(5 * time.Second) {
		t.Error("idle timer was not stopped by session_shutdown")
	}

	// State must be finished (written by session_shutdown, not by debounce).
	st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if st != string(agent.StateFinished) {
		t.Errorf("state after session_shutdown = %q, want finished", st)
	}

	// Firing the (now-stopped) timer must not produce a second StateFinished write.
	// We verify indirectly: the timer was stopped so Fire() is a no-op. Use a
	// short polling window after Fire() so any erroneous extra write would have
	// time to surface; the loop returns on first observed change or at deadline.
	beforeCount := countStateChangeEvents(t, sc.cfg.DB, sc.cfg.SessionName, "finished")
	idleTimer.Fire()
	deadline := time.Now().Add(200 * time.Millisecond)
	afterCount := beforeCount
	for time.Now().Before(deadline) {
		afterCount = countStateChangeEvents(t, sc.cfg.DB, sc.cfg.SessionName, "finished")
		if afterCount != beforeCount {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if afterCount != beforeCount {
		t.Errorf("firing stopped idle timer after session_shutdown wrote %d extra finished event(s)",
			afterCount-beforeCount)
	}
}

// countStateChangeEvents returns the number of state_change events with the
// given state value recorded in agent_events for the session.
func countStateChangeEvents(t *testing.T, d *db.DB, session, state string) int {
	t.Helper()
	events := getEvents(t, d, session)
	n := 0
	for _, ev := range events {
		if ev.Type == "state_change" {
			var p map[string]string
			if err := json.Unmarshal([]byte(ev.Payload), &p); err == nil {
				if p["state"] == state {
					n++
				}
			}
		}
	}
	return n
}

// TestSocketPipe_ErrorResumeDebounce_LastErrorAtRecorded verifies that
// state_change{error} records lastErrorAt so the ErrorResumeDebounce guard in
// handleSessionUpdated has a reference time.
//
// The PI path does not go through handleSessionUpdated; this test validates
// the lastErrorAt prerequisite: that a turn_start arriving within the debounce
// window DOES still proceed normally (since the PI path uses turn_start, not
// session.updated), but that lastErrorAt is set after an error state.
// Synchronisation: the test waits for the DB to reflect the
// state change, then polls sc.lastErrorAt under sc.mu via waitForCondition.
// This replaces a fixed 50ms sleep that flaked under contended scheduling.
func TestSocketPipe_ErrorResumeDebounce_LastErrorAtRecorded(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "error"})

	// Wait for the handler to commit StateError and lastErrorAt under s.mu.
	if !waitForCondition(t, func() bool {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return !sc.lastErrorAt.IsZero()
	}, 5*time.Second, "lastErrorAt set after state_change{error}") {
		t.Fatal("lastErrorAt was not set after state_change{error}")
	}

	sc.mu.Lock()
	lastErrorAt := sc.lastErrorAt
	sc.mu.Unlock()
	expectedTime := clk.Now()
	if !lastErrorAt.Equal(expectedTime) {
		t.Errorf("lastErrorAt = %v, want %v (clock.Now())", lastErrorAt, expectedTime)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestSocketPipe_TurnStart_DoesNotClobberReviewing_PreservesExistingBehaviour
// is an alias for TestSocketPipe_TurnStart_DoesNotClobberReviewing that
// confirms the reviewing guard holds. That test already covers this; this
// marker makes the requirement explicit.
func TestSocketPipe_ReviewingGuardPreservedAfterGapFixes(t *testing.T) {
	// This is covered by TestSocketPipe_TurnStart_DoesNotClobberReviewing.
	// We verify here that the reviewing guard AND the cancelIdleTimer call
	// in turn_start do not interact badly: an idle debounce is cancelled
	// but reviewingInFlight still suppresses the active write.
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Drive to active, start finished debounce.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Wait deterministically for the finished debounce timer registration
	// rather than sleeping a fixed window; a fixed sleep could observe nil
	// under scheduler load and make the later cancellation check vacuous
	// (issue #2902).
	idleTimer := clk.WaitForTimerCount(1, 5*time.Second)
	if idleTimer == nil {
		t.Fatal("no finished debounce timer created after state_change{finished}")
	}

	// Simulate /review: write reviewing to DB and set reviewingInFlight.
	if err := sc.cfg.DB.UpsertStatus(
		sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree,
		string(agent.StateReviewing), nil, nil,
	); err != nil {
		t.Fatalf("UpsertStatus reviewing: %v", err)
	}
	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.mu.Unlock()

	// Send turn_start — finished debounce timer cancelled but active write suppressed.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})

	// Finished debounce timer must be cancelled. Wait deterministically for the
	// cancellation rather than sleeping a fixed window (issue #2902).
	if !idleTimer.WaitStopped(5 * time.Second) {
		t.Error("finished debounce timer was not cancelled by turn_start while reviewing")
	}

	// DB state must still be reviewing (active write suppressed).
	st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if st != string(agent.StateReviewing) {
		t.Errorf("state = %q after turn_start while reviewing, want reviewing", st)
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestSocketPipe_ReviewAgent_ReachesFinishedViaTurnEnd verifies that a review-agent
// session (AgentRole = "review-goal") reaches StateFinished via the unified
// turn_end → state_change{finished} path, without requiring the agent_end hook
// or role-specific branching.
//
// Synchronisation: WaitForTimerCount replaces the previous 50ms
// sleep so the test waits on the actual debounce-timer registration event.
func TestSocketPipe_ReviewAgent_ReachesFinishedViaTurnEnd(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	sc.rootAgent = "review-goal"
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Simulate a review-agent session: one turn, then state_change{finished}.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "active"})
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "review verdict"})
	sendJSON(t, conn, map[string]any{"type": "turn_end"})
	// Extension emits state_change{finished} directly (protocol v2).
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Wait deterministically for the finished-debounce timer to be registered.
	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer created after state_change{finished} from review agent")
	}

	// State must NOT be finished yet (debounce has not fired).
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s == string(agent.StateFinished) {
		t.Error("state became finished before debounce fired")
	}

	// Fire the debounce timer — state must become finished.
	timer.Fire()

	if st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateFinished), 2*time.Second); st != string(agent.StateFinished) {
		t.Fatalf("review-agent session never reached finished after debounce, got %q", st)
	}

	conn.Close()
	_ = wait()
}

// TestSocketPipe_ProtocolVersion1_TooOld verifies that protocol_version=1 is
// rejected as too old now that the sidecar requires protocol v2.
func TestSocketPipe_ProtocolVersion1_TooOld(t *testing.T) {
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
		"protocol_version": 1, // v1 is now too old (sidecar requires v2)
		"harness":          "pi",
		"harness_version":  "0.1.0-test",
	})

	errFrame := readJSON(t, conn)
	code, _ := errFrame["code"].(string)
	if code != "protocol_version_too_old" {
		t.Errorf("error code = %q, want protocol_version_too_old (v1 < v2)", code)
	}
	conn.Close()
	_ = wait()
}

// ── session_shutdown regression tests ────────────────────────────────────────────────
//
// These tests guard against a regression: the PI extension's
// `session_shutdown` hook must not send a `{type:"session_shutdown"}` wire
// frame before closing the connection. The
// sidecar treats that frame as terminal, removes pipe.sock, and breaks the
// reconnect loop — causing ECONNRESET when PI fires `session_start`.
//
// The fix: only call writer.close() from the hook (no wire frame). The sidecar
// reads EOF, keeps the listener open, and accepts the re-dial.

// TestSocketPipe_SessionShutdownHook_NoWireFrame_New verifies that when the
// extension closes the connection without sending a session_shutdown wire frame
// (the fixed session_shutdown hook path for /new), the sidecar keeps pipe.sock
// and accepts a second connection — no ECONNRESET.
func TestSocketPipe_SessionShutdownHook_NoWireFrame_New(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	// First connection — simulates the active PI session before /new.
	conn1, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn1, map[string]any{"type": "turn_start"})
	sendJSON(t, conn1, map[string]any{"type": "msg_assistant", "text": "before-new"})
	sendJSON(t, conn1, map[string]any{"type": "turn_end"})

	// Simulate the fixed session_shutdown hook: close without wire frame.
	// This is exactly what writer.close() does — it calls socket.end(), which
	// sends a TCP FIN. The sidecar reads EOF and waits for reconnect.
	conn1.Close()

	// Give the sidecar a moment to process the drop and re-enter Accept.
	time.Sleep(50 * time.Millisecond)

	// The socket file must still exist (sidecar must NOT have removed it).
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("pipe.sock was removed after session_shutdown hook close (regression #1440): %v", err)
	}

	// Second connection — simulates PI session_start with reason "new".
	// Must succeed without ECONNRESET.
	conn2, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn2, map[string]any{"type": "turn_start"})
	sendJSON(t, conn2, map[string]any{"type": "msg_assistant", "text": "after-new"})
	sendJSON(t, conn2, map[string]any{"type": "turn_end"})
	sendJSON(t, conn2, map[string]any{"type": "session_shutdown"})
	conn2.Close()

	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error after /new reconnect: %v", err)
	}

	// Both turns must be recorded.
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	textSet := map[string]bool{}
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(ev.Payload), &p); err == nil {
				textSet[p.Text] = true
			}
		}
	}
	if !textSet["before-new"] {
		t.Error("msg_assistant 'before-new' not found in agent_events")
	}
	if !textSet["after-new"] {
		t.Error("msg_assistant 'after-new' not found in agent_events")
	}

	// Session must be finished.
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateFinished) {
		t.Errorf("state after /new reconnect+shutdown = %q, want finished", s)
	}
}

// TestSocketPipe_SessionShutdownHook_NoWireFrame_Resume is the same as _New
// but named for the /resume case (same wire behaviour, different PI reason).
func TestSocketPipe_SessionShutdownHook_NoWireFrame_Resume(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn1, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn1, map[string]any{"type": "turn_start"})
	sendJSON(t, conn1, map[string]any{"type": "msg_assistant", "text": "before-resume"})
	sendJSON(t, conn1, map[string]any{"type": "turn_end"})
	conn1.Close() // fixed hook: no wire frame, only FIN

	time.Sleep(50 * time.Millisecond)

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("pipe.sock was removed after session_shutdown hook close (/resume, regression #1440): %v", err)
	}

	conn2, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn2, map[string]any{"type": "turn_start"})
	sendJSON(t, conn2, map[string]any{"type": "msg_assistant", "text": "after-resume"})
	sendJSON(t, conn2, map[string]any{"type": "turn_end"})
	sendJSON(t, conn2, map[string]any{"type": "session_shutdown"})
	conn2.Close()

	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error after /resume reconnect: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	textSet := map[string]bool{}
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(ev.Payload), &p); err == nil {
				textSet[p.Text] = true
			}
		}
	}
	if !textSet["before-resume"] {
		t.Error("msg_assistant 'before-resume' not found in agent_events")
	}
	if !textSet["after-resume"] {
		t.Error("msg_assistant 'after-resume' not found in agent_events")
	}

	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateFinished) {
		t.Errorf("state after /resume reconnect+shutdown = %q, want finished", s)
	}
}

// TestSocketPipe_SessionShutdownHook_NoWireFrame_Fork is the same as _New
// but named for the /fork case.
func TestSocketPipe_SessionShutdownHook_NoWireFrame_Fork(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn1, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn1, map[string]any{"type": "turn_start"})
	sendJSON(t, conn1, map[string]any{"type": "msg_assistant", "text": "before-fork"})
	sendJSON(t, conn1, map[string]any{"type": "turn_end"})
	conn1.Close() // fixed hook: no wire frame, only FIN

	time.Sleep(50 * time.Millisecond)

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("pipe.sock was removed after session_shutdown hook close (/fork, regression #1440): %v", err)
	}

	conn2, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn2, map[string]any{"type": "turn_start"})
	sendJSON(t, conn2, map[string]any{"type": "msg_assistant", "text": "after-fork"})
	sendJSON(t, conn2, map[string]any{"type": "turn_end"})
	sendJSON(t, conn2, map[string]any{"type": "session_shutdown"})
	conn2.Close()

	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error after /fork reconnect: %v", err)
	}

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	textSet := map[string]bool{}
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(ev.Payload), &p); err == nil {
				textSet[p.Text] = true
			}
		}
	}
	if !textSet["before-fork"] {
		t.Error("msg_assistant 'before-fork' not found in agent_events")
	}
	if !textSet["after-fork"] {
		t.Error("msg_assistant 'after-fork' not found in agent_events")
	}

	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateFinished) {
		t.Errorf("state after /fork reconnect+shutdown = %q, want finished", s)
	}
}

// TestSocketPipe_SessionShutdownHook_SockFilePresent verifies that when the
// session_shutdown hook closes the connection without a wire frame, the unix
// socket file at HarnessPipeSockPath is NOT removed between the first
// connection's close and the second connection's accept. This is the key
// invariant that prevents ECONNRESET on the session_start re-dial.
func TestSocketPipe_SessionShutdownHook_SockFilePresent(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	// Wait for socket file to appear.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket file never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	conn1, _ := dialAndHandshake(t, sockPath)
	// Simulate session_shutdown hook: close without wire frame.
	conn1.Close()

	// Give the sidecar a moment to process the drop.
	time.Sleep(50 * time.Millisecond)

	// The socket file must still be present — the sidecar must not have
	// removed it (which would only happen after a genuine session_shutdown
	// wire frame breaks the reconnect loop).
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("pipe.sock removed after session_shutdown hook close (regression #1440): %v", err)
	}

	// Second connection must be accepted without error.
	conn2, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn2, map[string]any{"type": "session_shutdown"})
	conn2.Close()

	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_GenuineSessionShutdown_StillDrivesFinished verifies that a
// genuine {type:"session_shutdown"} wire frame (e.g. from process exit) still
// drives the session to StateFinished and breaks the reconnect loop. This
// guards against any accidental change to handlePipeFrame semantics
// (the sidecar must still handle the genuine frame correctly).
func TestSocketPipe_GenuineSessionShutdown_StillDrivesFinished(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Send a genuine session_shutdown wire frame (as if from process exit).
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()

	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error on genuine session_shutdown: %v", err)
	}

	// State must be finished — the reconnect loop must have broken.
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateFinished) {
		t.Errorf("state after genuine session_shutdown = %q, want finished", s)
	}

	// The socket file must have been removed (closePipeListener cleanup).
	if _, err := os.Stat(sockPath); err == nil {
		t.Error("pipe.sock still present after genuine session_shutdown — listener should have closed and removed it")
	}
}

// TestSocketPipe_SessionShutdownHook_NextFrameDispatchedNormally verifies that
// after the extension reconnects following a session_shutdown hook close, the
// next inbound frame from the sidecar is parsed and dispatched normally with
// no leftover state from the previous connection.
func TestSocketPipe_SessionShutdownHook_NextFrameDispatchedNormally(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	// First connection — session_shutdown hook close.
	conn1, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn1, map[string]any{"type": "turn_start"})
	sendJSON(t, conn1, map[string]any{"type": "msg_assistant", "text": "first-turn"})
	sendJSON(t, conn1, map[string]any{"type": "turn_end"})
	conn1.Close() // no wire frame — fixed hook

	time.Sleep(50 * time.Millisecond)

	// Second connection — session_start reconnect.
	conn2, _ := dialAndHandshake(t, sockPath)

	// The sidecar must deliver a prompt on the second connection without any
	// leftover state from the first connection. Enqueue a prompt from the
	// sidecar side and verify the extension receives it normally.
	rd := bufio.NewReader(conn2)
	const promptText = "post-reconnect prompt"
	go func() {
		time.Sleep(50 * time.Millisecond)
		sc.DeliverPrompt(promptText, "nextTurn")
	}()

	_ = conn2.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := rd.ReadBytes('\n')
	_ = conn2.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatalf("read prompt frame after reconnect: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(line, &frame); err != nil {
		t.Fatalf("unmarshal prompt frame: %v", err)
	}
	if frame["type"] != "prompt" {
		t.Errorf("expected prompt frame after reconnect, got type %v", frame["type"])
	}
	if frame["text"] != promptText {
		t.Errorf("prompt text = %v, want %q", frame["text"], promptText)
	}

	// Clean shutdown on second connection.
	sendJSON(t, conn2, map[string]any{"type": "session_shutdown"})
	conn2.Close()

	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error after reconnect: %v", err)
	}
}

// TestSocketPipe_SessionStatus_PopulatesHarnessSessionID verifies that a
// session_status frame carrying a non-empty session_id causes the sidecar to
// call UpdateHarnessSessionID and record the PI session ID in the DB.
//
// This is the regression test for the harness_session_id write: without this
// handling session_status falls through to the default case and the
// harness_session_id column is never populated for PI sessions, causing
// prism cleanup to log
// "raw/session.jsonl not found" and produce an empty archive.
func TestSocketPipe_SessionStatus_PopulatesHarnessSessionID(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Send a session_status frame with a real PI session ID.
	const piSessionID = "pi-ses-test-abc-123"
	sendJSON(t, conn, map[string]any{
		"type":          "session_status",
		"role":          "worker",
		"branch":        "main",
		"review_cycles": 0,
		"pr_number":     "",
		"session_id":    piSessionID,
	})

	// Poll DB until harness_session_id is populated.
	deadline := time.Now().Add(2 * time.Second)
	var gotSID string
	for {
		s, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
		if err != nil {
			t.Fatalf("CurrentStatus: %v", err)
		}
		if s != nil && s.HarnessSessionID != nil && *s.HarnessSessionID == piSessionID {
			gotSID = *s.HarnessSessionID
			break
		}
		if time.Now().After(deadline) {
			var sid string
			if s != nil && s.HarnessSessionID != nil {
				sid = *s.HarnessSessionID
			}
			t.Fatalf("harness_session_id never set to %q (got %q)", piSessionID, sid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gotSID != piSessionID {
		t.Errorf("harness_session_id = %q, want %q", gotSID, piSessionID)
	}

	// The session_status frame should also be persisted as a raw event.
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	var found bool
	for _, e := range events {
		if e.Type == "session_status" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected session_status event in agent_events, not found")
	}

	// Clean shutdown.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// TestSocketPipe_SessionStatus_EmptySessionID_NoOp verifies that a
// session_status frame with an empty session_id does NOT call
// UpdateHarnessSessionID (which would overwrite a real ID with an empty string).
func TestSocketPipe_SessionStatus_EmptySessionID_NoOp(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Send a session_status frame with no session_id (post-handshake case).
	sendJSON(t, conn, map[string]any{
		"type":          "session_status",
		"role":          "worker",
		"branch":        "main",
		"review_cycles": 0,
		"pr_number":     "",
		"session_id":    "",
	})

	time.Sleep(100 * time.Millisecond)

	// harness_session_id must remain NULL (not set to empty string).
	s, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s != nil && s.HarnessSessionID != nil && *s.HarnessSessionID != "" {
		t.Errorf("harness_session_id = %q, want NULL (empty session_id should be a no-op)", *s.HarnessSessionID)
	}

	// Clean shutdown.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// ── harness_session_id write-ordering invariant test ─────────────────────────────────────────────────
//
// These tests assert the write-ordering invariant introduced by the fix for
// Whenever agent_status.harness_session_id is set, the
// corresponding session_status event must already exist in agent_events.
// This invariant is what makes TestSocketPipe_SessionStatus_PopulatesHarnessSessionID
// race-free: the polling target (agent_status) is always the LAST write.

// TestSocketPipe_SessionStatus_EventBeforeStatus asserts the write-ordering
// invariant for the session_status handler: the
// agent_events row for the session_status frame is committed BEFORE
// agent_status.harness_session_id is set. We verify this by polling
// agent_status until harness_session_id is set, then immediately asserting
// — without any sleep — that agent_events already contains the row.
//
// This test is the invariant proof: if the write ordering regresses, this test
// will fail (not just flake) because there is no sleep between the poll
// resolving and the events assertion.
func TestSocketPipe_SessionStatus_EventBeforeStatus(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	const piSessionID = "pi-ses-invariant-test-456"
	sendJSON(t, conn, map[string]any{
		"type":          "session_status",
		"role":          "worker",
		"branch":        "main",
		"review_cycles": 0,
		"pr_number":     "",
		"session_id":    piSessionID,
	})

	// Poll agent_status until harness_session_id is set. Once we see it, the
	// agent_events row MUST already be present — no additional sleep or retry.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
		if err != nil {
			t.Fatalf("CurrentStatus: %v", err)
		}
		if s != nil && s.HarnessSessionID != nil && *s.HarnessSessionID == piSessionID {
			// harness_session_id is now visible. Assert the invariant: the
			// session_status event must already be in agent_events.
			events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
			var found bool
			for _, e := range events {
				if e.Type == "session_status" {
					found = true
					break
				}
			}
			if !found {
				t.Error("invariant violated: harness_session_id is set in agent_status but session_status event is missing from agent_events")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("harness_session_id never set — timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Clean shutdown.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}
}

// ── reviewing-window TOCTOU regression tests ────────────────────────────────────────────────
//
// These tests guard against the race where the finished-debounce in
// handleSessionFinished suppresses only when BOTH reviewingInFlight==true AND
// the DB state is StateReviewing. Intermediate state_change frames can drive
// the DB back to StateActive while the in-memory flag is still true, causing
// the AND to fail and the session to prematurely transition to StateFinished.
//
// The fix: rely on the in-memory flag alone.

// TestSocketPipe_StateChange_DoesNotClobberReviewing verifies that a
// state_change{finished} frame while reviewingInFlight==true does NOT
// transition the session to finished, even when the DB state has drifted back
// to active (the old AND guard fails here). This is the core
// regression test for the reviewing-window TOCTOU.
//
// Synchronisation: WaitForTimerCount waits for the debounce timer, then we
// fire it manually and assert the session stays not-finished.
func TestSocketPipe_StateChange_DoesNotClobberReviewing(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Bring the session to active.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	if st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateActive), 2*time.Second); st != string(agent.StateActive) {
		t.Fatalf("state never reached active after turn_start, got %q", st)
	}

	// Simulate /review: write reviewing to the DB and set reviewingInFlight.
	if err := sc.cfg.DB.UpsertStatus(
		sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree,
		string(agent.StateReviewing), nil, nil,
	); err != nil {
		t.Fatalf("UpsertStatus reviewing: %v", err)
	}
	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.mu.Unlock()

	// Simulate DB state drifting back to active (the bug scenario):
	// a state_change frame from a prior code path overwrote reviewing with active.
	// We replicate this directly without going through the protocol.
	if err := sc.cfg.DB.UpsertStatus(
		sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree,
		string(agent.StateActive), nil, nil,
	); err != nil {
		t.Fatalf("UpsertStatus active (simulating drift): %v", err)
	}

	// Confirm DB is now active (not reviewing) — this is the buggy state.
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateActive) {
		t.Fatalf("expected DB to be active (drift simulation), got %q", s)
	}

	// Send state_change{finished} — this starts the finished debounce.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Wait for the debounce timer to be registered.
	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer created after state_change{finished}")
	}

	// Fire the debounce timer — with the fix the suppression should trigger.
	timer.Fire()

	// Give the debounce closure time to run.
	time.Sleep(100 * time.Millisecond)

	// The session must NOT have transitioned to finished (reviewingInFlight is
	// still true, so the debounce should be suppressed).
	st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if st == string(agent.StateFinished) {
		t.Errorf("session transitioned to finished while reviewingInFlight=true and DB state=active — #1652 regression")
	}

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestSocketPipe_FailedRetryReview_StateChangeDoesNotFinish reproduces the
// exact timeline:
//
//  1. Worker starts, reaches active.
//  2. /review is called (1st attempt) — writes reviewing to DB, sets flag.
//  3. /review subprocess fails — reviewingInFlight stays true (HTTP 500 path).
//  4. /review is called again (2nd attempt, --rebase) — flag is already true.
//  5. Worker does fix-cycle work: state_change frames drive DB back to active.
//  6. Worker emits state_change{finished} — debounce fires — session must NOT
//     transition to finished.
//
// Synchronisation: WaitForTimerCount + manual Fire + sleep.
func TestSocketPipe_FailedRetryReview_StateChangeDoesNotFinish(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newSocketPipeSidecarWithClock(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Step 1: worker reaches active.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	if st := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateActive), 2*time.Second); st != string(agent.StateActive) {
		t.Fatalf("state never reached active after turn_start, got %q", st)
	}

	// Step 2: first /review attempt — writes reviewing + sets flag.
	if err := sc.cfg.DB.UpsertStatus(
		sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree,
		string(agent.StateReviewing), nil, nil,
	); err != nil {
		t.Fatalf("UpsertStatus reviewing (1st attempt): %v", err)
	}
	sc.mu.Lock()
	sc.reviewingInFlight = true
	sc.mu.Unlock()

	// Step 3: /review subprocess fails. In the real code the HTTP handler
	// returns 500 and does NOT clear reviewingInFlight (it stays true). The DB
	// state stays "reviewing" at this point.

	// Step 4: second /review attempt (--rebase). reviewingInFlight is already
	// true; the handler skips the DB write and re-launches the subprocess.
	// (Nothing to do here — the flag is already set.)

	// Step 5: worker does fix-cycle work. state_change{active} frames via the
	// default branch of handlePipeFrame drive DB back to active.
	// We simulate this directly to avoid relying on internal protocol details.
	if err := sc.cfg.DB.UpsertStatus(
		sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree,
		string(agent.StateActive), nil, nil,
	); err != nil {
		t.Fatalf("UpsertStatus active (fix-cycle drift): %v", err)
	}

	// Confirm DB has drifted back to active while flag is still set.
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateActive) {
		t.Fatalf("expected DB=active after fix-cycle drift, got %q", s)
	}
	sc.mu.Lock()
	stillInFlight := sc.reviewingInFlight
	sc.mu.Unlock()
	if !stillInFlight {
		t.Fatal("reviewingInFlight should still be true after failed-then-retried /review")
	}

	// Step 6: worker finishes its fix-cycle turn → state_change{finished}.
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Wait for the debounce timer.
	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer created after state_change{finished}")
	}

	// Fire the debounce — with the fix it must be suppressed.
	timer.Fire()
	time.Sleep(100 * time.Millisecond)

	// Session must NOT be finished — review is still in flight.
	st := getState(t, sc.cfg.DB, sc.cfg.SessionName)
	if st == string(agent.StateFinished) {
		t.Errorf("session prematurely finished during failed-then-retried /review with DB state=active — #1652 regression")
	}

	// Cleanup: clear the flag and shut down.
	sc.mu.Lock()
	sc.reviewingInFlight = false
	sc.mu.Unlock()

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// Ensure db import is used.
var _ *db.DB

// ── control-plane backpressure tests ─────────────────────────────
//
// These tests verify that when the outbound channel (harnessPipeOutCh, buffered
// to 64) is full, each control-plane HTTP handler returns a non-200 response
// instead of silently accepting 200 OK.
//
// The full-channel state is simulated by directly setting harnessPipeOutCh to a
// pre-filled channel (all 64 slots occupied). This is the package-internal
// equivalent of "block the writer goroutine with a hung mock connection" because
// the HTTP-level invariant under test is identical regardless of how the channel
// became full.

// fillOutboundChannel pre-fills s.harnessPipeOutCh to capacity with dummy frames
// so that the next enqueueHarnessPipeFrame call returns false. The sidecar's
// pipe-loop need not be running — we set the channel directly so isPipeConnected
// returns true while the channel is already at capacity.
func fillOutboundChannel(s *Sidecar) {
	ch := make(chan []byte, 64)
	dummy := []byte(`{"type":"noop"}` + "\n")
	for i := 0; i < 64; i++ {
		ch <- dummy
	}
	s.mu.Lock()
	s.harnessPipeOutCh = ch
	s.mu.Unlock()
}

// newQueueFullSidecar creates a sidecar whose outbound channel is pre-filled
// to capacity. The pipe loop is NOT running; the test operates purely through
// the HTTP handler surface.
func newQueueFullSidecar(t *testing.T) *Sidecar {
	t.Helper()
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	sc.cfg.AgentRole = "worker"
	fillOutboundChannel(sc)
	return sc
}

// TestControlPlane_QueueFull_SetModel verifies that /set-model returns 503
// when the outbound channel is full and the failure is logged.
func TestControlPlane_QueueFull_SetModel(t *testing.T) {
	sc := newQueueFullSidecar(t)
	getLogs := captureLog(sc)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/set-model", map[string]any{
		"session":  sc.cfg.SessionName,
		"provider": "anthropic",
		"model":    "claude-3-7-sonnet",
		"thinking": "",
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// Failure must be logged.
	if logs := getLogs(); !strings.Contains(logs, "queue full") {
		t.Errorf("expected queue-full log, got: %s", logs)
	}
}

// TestControlPlane_HappyPath_SetModel verifies that /set-model returns 200 OK
// when the outbound channel has space (regression guard for the queue-full change).
func TestControlPlane_HappyPath_SetModel(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/set-model", map[string]any{
		"session":  sc.cfg.SessionName,
		"provider": "anthropic",
		"model":    "claude-3-7-sonnet",
		"thinking": "",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// The extension should receive the frame.
	frame := readFrameWithDeadline(t, rd, conn)
	if got := frame["type"]; got != "set_model" {
		t.Errorf("frame type = %v, want set_model", got)
	}
	// Clean shutdown.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestControlPlane_QueueFull_RegisterProvider verifies that /register-provider
// returns 503 when the outbound channel is full and the failure is logged.
func TestControlPlane_QueueFull_RegisterProvider(t *testing.T) {
	sc := newQueueFullSidecar(t)
	getLogs := captureLog(sc)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/register-provider", map[string]any{
		"session": sc.cfg.SessionName,
		"name":    "my-provider",
		"config":  map[string]any{"apiKey": "test"},
		"scope":   "session",
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// Failure must be logged.
	if logs := getLogs(); !strings.Contains(logs, "queue full") {
		t.Errorf("expected queue-full log, got: %s", logs)
	}
}

// TestControlPlane_HappyPath_RegisterProvider verifies that /register-provider
// returns 200 OK when the outbound channel has space.
func TestControlPlane_HappyPath_RegisterProvider(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	sc.cfg.AgentRole = "worker"
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/register-provider", map[string]any{
		"session": sc.cfg.SessionName,
		"name":    "my-provider",
		"config":  map[string]any{"apiKey": "test"},
		"scope":   "session",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// The extension should receive the frame.
	frame := readFrameWithDeadline(t, rd, conn)
	if got := frame["type"]; got != "register_provider" {
		t.Errorf("frame type = %v, want register_provider", got)
	}
	// Clean shutdown.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestControlPlane_QueueFull_SetActiveTools verifies that /set-active-tools
// returns 503 when the outbound channel is full and the failure is logged.
func TestControlPlane_QueueFull_SetActiveTools(t *testing.T) {
	sc := newQueueFullSidecar(t)
	getLogs := captureLog(sc)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/set-active-tools", map[string]any{
		"session": sc.cfg.SessionName,
		"tools":   []string{"bash", "read"},
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// Failure must be logged.
	if logs := getLogs(); !strings.Contains(logs, "queue full") {
		t.Errorf("expected queue-full log, got: %s", logs)
	}
}

// TestControlPlane_HappyPath_SetActiveTools verifies that /set-active-tools
// returns 200 OK when the outbound channel has space.
func TestControlPlane_HappyPath_SetActiveTools(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	sc.cfg.AgentRole = "worker"
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/set-active-tools", map[string]any{
		"session": sc.cfg.SessionName,
		"tools":   []string{"bash", "read"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// The extension should receive the frame.
	frame := readFrameWithDeadline(t, rd, conn)
	if got := frame["type"]; got != "set_active_tools" {
		t.Errorf("frame type = %v, want set_active_tools", got)
	}
	// Clean shutdown.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestControlPlane_QueueFull_Abort verifies that /abort returns 503 when the
// outbound channel is full and the failure is logged.
func TestControlPlane_QueueFull_Abort(t *testing.T) {
	sc := newQueueFullSidecar(t)
	getLogs := captureLog(sc)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/abort", map[string]any{
		"session": sc.cfg.SessionName,
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// Failure must be logged.
	if logs := getLogs(); !strings.Contains(logs, "queue full") {
		t.Errorf("expected queue-full log, got: %s", logs)
	}
}

// TestControlPlane_HappyPath_Abort verifies that /abort returns 200 OK when
// the outbound channel has space.
func TestControlPlane_HappyPath_Abort(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	sc.cfg.HarnessName = "pi"
	sc.cfg.AgentRole = "worker"
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)
	rd := bufio.NewReader(conn)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/abort", map[string]any{
		"session": sc.cfg.SessionName,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// The extension should receive the frame.
	frame := readFrameWithDeadline(t, rd, conn)
	if got := frame["type"]; got != "abort" {
		t.Errorf("frame type = %v, want abort", got)
	}
	// Clean shutdown.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	_ = wait()
}

// TestControlPlane_QueueFull_ApplyProfile verifies that /apply-profile returns
// 503 when the outbound channel of a target session is full.
func TestControlPlane_QueueFull_ApplyProfile(t *testing.T) {
	sc := newQueueFullSidecar(t)
	// apply-profile is coordinator-only for scope=session targeting own session
	// when the caller is a worker — override to coordinator so the handler proceeds.
	sc.cfg.AgentRole = "coordinator"
	getLogs := captureLog(sc)

	// Insert a DB row for the session so resolveRoleForSession can look up the role.
	_ = sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, "active", nil, nil)
	_ = sc.cfg.DB.SetHarness(sc.cfg.SessionName, "pi")

	// Stub the profile loader so the test doesn't need a real profiles file.
	origLoader := hostAPILoadProfiles
	hostAPILoadProfiles = func() (*config.ProfilesFile, error) {
		return &config.ProfilesFile{
			Default: "base",
			Profiles: map[string]config.ProfileEntry{
				"base": {
					"worker":      {Provider: "anthropic", Model: "claude-sonnet", Thinking: "none"},
					"coordinator": {Provider: "anthropic", Model: "claude-sonnet", Thinking: "none"},
				},
			},
		}, nil
	}
	defer func() { hostAPILoadProfiles = origLoader }()

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/apply-profile", map[string]any{
		"profile": "base",
		"scope":   "session",
		"session": sc.cfg.SessionName,
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// Failure must be logged.
	if logs := getLogs(); !strings.Contains(logs, "queue full") {
		t.Errorf("expected queue-full log, got: %s", logs)
	}
}

// TestControlPlane_QueueFull_RegisterProviderDirect verifies that
// /register-provider-direct returns 503 when the outbound channel is full.
func TestControlPlane_QueueFull_RegisterProviderDirect(t *testing.T) {
	sc := newQueueFullSidecar(t)
	getLogs := captureLog(sc)

	handler := sc.hostAPIHandler()
	rr := postJSON(t, handler, "/register-provider-direct", map[string]any{
		"session": sc.cfg.SessionName,
		"name":    "my-provider",
		"config":  map[string]any{"apiKey": "test"},
	})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	// Failure must be logged.
	if logs := getLogs(); !strings.Contains(logs, "queue full") {
		t.Errorf("expected queue-full log, got: %s", logs)
	}
}

// ── readLineLimited / maxLineBytes cap tests ──────────────────────────────────

// testMaxLineBytes is the per-test per-frame byte cap used by the _LineCap_
// tests below. 256 bytes is large enough to accommodate the hello frame used
// by dialAndHandshake (~82 bytes) while still being far below the 16 MiB
// default, keeping test allocations tiny and execution fast.
const testMaxLineBytes = 256

// newSocketPipeSidecarWithLineCap creates a Sidecar configured for socket-pipe
// testing with Config.PipeMaxLineBytes set to cap. This avoids any global
// state mutation and is safe for parallel tests.
func newSocketPipeSidecarWithLineCap(t *testing.T, sockPath string, cap int) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
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
		PipeReconnectTimeout:  200 * time.Millisecond,
		PipeMaxLineBytes:      cap,
		Harness:               pih.New("", "", ""),
	}
	return New(cfg)
}

// TestSocketPipe_LineCap_OversizedFrameClosesConnection verifies that a frame
// of PipeMaxLineBytes+1 bytes (no newline) causes the sidecar to close the
// connection and emit a clearly-tagged log line.
func TestSocketPipe_LineCap_OversizedFrameClosesConnection(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecarWithLineCap(t, sockPath, testMaxLineBytes)
	getLogs := captureLog(sc)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Send a blob of testMaxLineBytes+1 bytes with no newline — exceeds the
	// cap and must trigger a protocol-violation close.
	oversized := make([]byte, testMaxLineBytes+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	if _, err := conn.Write(oversized); err != nil {
		t.Fatalf("write oversized frame: %v", err)
	}

	// The sidecar must close the connection. Detect this by waiting for a
	// read on our side to return an error (EOF / connection reset).
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	readBuf := make([]byte, 1)
	_, readErr := conn.Read(readBuf)
	if readErr == nil {
		t.Error("expected connection to be closed by sidecar after oversized frame, but Read returned nil error")
	}
	conn.Close()

	// After connection closed, reconnect and send session_shutdown so sidecar exits.
	conn2, _ := dialAndHandshake(t, sockPath)
	sendJSON(t, conn2, map[string]any{"type": "session_shutdown"})
	conn2.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	// The violation log line must have been emitted.
	logs := getLogs()
	if !strings.Contains(logs, "frame exceeded maxLineBytes") {
		t.Errorf("expected 'frame exceeded maxLineBytes' in logs, got: %s", logs)
	}
	if !strings.Contains(logs, "closing connection") {
		t.Errorf("expected 'closing connection' in logs, got: %s", logs)
	}
}

// TestSocketPipe_LineCap_BoundaryFrameAccepted verifies that a frame of
// exactly PipeMaxLineBytes bytes terminated by a newline is accepted and
// processed normally (the cap is a strict greater-than comparison).
func TestSocketPipe_LineCap_BoundaryFrameAccepted(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecarWithLineCap(t, sockPath, testMaxLineBytes)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Build a valid JSON frame of exactly testMaxLineBytes bytes (including
	// the trailing newline). We use {"type":"unknown_cap_test","p":"<pad>"}
	// and pad to fill exactly testMaxLineBytes bytes.
	const frameType = "unknown_cap_test"
	// Skeleton (no padding): {"type":"unknown_cap_test","p":""}\n
	skeleton := `{"type":"` + frameType + `","p":"` + `"}` + "\n"
	padLen := testMaxLineBytes - len(skeleton)
	if padLen < 0 {
		t.Fatalf("skeleton frame (%d bytes) already exceeds testMaxLineBytes (%d)", len(skeleton), testMaxLineBytes)
	}
	padding := strings.Repeat("a", padLen)
	frame := `{"type":"` + frameType + `","p":"` + padding + `"}` + "\n"
	if len(frame) != testMaxLineBytes {
		t.Fatalf("frame length = %d, want %d", len(frame), testMaxLineBytes)
	}

	if _, err := conn.Write([]byte(frame)); err != nil {
		t.Fatalf("write boundary frame: %v", err)
	}

	// Send session_shutdown and wait for clean exit.
	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error on boundary frame: %v", err)
	}

	// The boundary frame must have been persisted as an event (unknown types
	// are stored for forward-compatibility).
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	found := false
	for _, ev := range events {
		if ev.Type == frameType {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("boundary frame of type %q not found in agent_events", frameType)
	}
}

// TestSocketPipe_LineCap_HelloOversizedClosesConnection verifies that the
// hello-handshake reader also enforces the cap: an oversized hello frame
// causes the connection to be closed and a violation log line is emitted.
func TestSocketPipe_LineCap_HelloOversizedClosesConnection(t *testing.T) {
	// Use a cap that is smaller than the hello frame (~82 bytes) so that the
	// very first read on the connection triggers the cap-exceeded path.
	const helloCap = 64

	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecarWithLineCap(t, sockPath, helloCap)
	getLogs := captureLog(sc)
	wait := runSocketPipeSidecar(sc)

	// Wait for the socket to appear (sidecar may still be binding).
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

	// Send an oversized hello (no newline) — exceeds the cap before a valid
	// hello frame can be parsed.
	oversized := make([]byte, helloCap+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	if _, err := conn.Write(oversized); err != nil {
		t.Fatalf("write oversized hello: %v", err)
	}

	// The sidecar must close the connection.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	readBuf := make([]byte, 1)
	_, readErr := conn.Read(readBuf)
	if readErr == nil {
		t.Error("expected connection to be closed by sidecar after oversized hello, but Read returned nil error")
	}
	conn.Close()

	// After connection closed, reconnect the sidecar with helloCap=0 (fall back
	// to the default 16 MiB cap) for the cleanup handshake is not needed —
	// the sidecar will wait for a new connection within PipeReconnectTimeout
	// (200 ms). The test just needs to wait for runStartupSocketPipe to exit
	// (it will time out waiting for reconnect).
	_ = wait()

	// The violation log line must have been emitted.
	logs := getLogs()
	if !strings.Contains(logs, "frame exceeded maxLineBytes") {
		t.Errorf("expected 'frame exceeded maxLineBytes' in logs, got: %s", logs)
	}
	if !strings.Contains(logs, "closing connection") {
		t.Errorf("expected 'closing connection' in logs, got: %s", logs)
	}
}

// ── zero-output exit classification ───────────────────────────

// newZeroOutputWorkerSidecar wires a worker-named ("testrepo@feature")
// socket-pipe sidecar against a freshly-seeded coordinator row at
// "testrepo@main" so that notifyCoordinator / notifyCoordinatorError can
// progress past the self / discovery / suppression guards. It returns the
// sidecar plus its testClock; the caller is responsible for installing the
// notifyCoordinatorDeliverFn test seam and for running runStartupSocketPipe.
//
// Mirrors newSocketPipeSidecarWithClock's isolation contract (XDG_STATE_HOME
// redirected, PRISM_TEST_MODE_RESTRICT_HOSTAPI set) and uses session names
// that cannot collide with a live host coordinator.
func newZeroOutputWorkerSidecar(t *testing.T, sockPath string) (*Sidecar, *testClock) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	d := openTestDB(t)
	clk := newTestClock()
	cfg := Config{
		SessionName:           "testrepo@feature",
		Repo:                  "testrepo",
		Worktree:              t.TempDir(),
		DB:                    d,
		Clock:                 clk,
		AgentRole:             "worker",
		HarnessName:           "pi",
		HarnessPipeSockPath:   sockPath,
		StartupConnectTimeout: 5 * time.Second,
		PipeReconnectTimeout:  200 * time.Millisecond,
		Harness:               pih.New("", "", ""),
	}
	sc := New(cfg)

	// Seed an active coordinator row at testrepo@main so CoordinatorForRepo
	// discovers it via the DB-backed path. root_agent_name='coordinator' is
	// required for isCoordinatorSession's DB lookup; harness='' forces the
	// HTTP fallback in promptdelivery (we shortcut that anyway via the
	// notifyCoordinatorDeliverFn seam).
	coordName := "testrepo@main"
	coordAgent := "coordinator"
	coordModel := "anthropic/claude-sonnet-4-5"
	coordSID := "coord-sid-zero-output"
	if err := d.UpsertStatusWithAgent(coordName, "testrepo", "/tmp/test-coord", "active", nil, &coordSID, &coordAgent, &coordModel); err != nil {
		t.Fatalf("seed coordinator: UpsertStatusWithAgent: %v", err)
	}
	if err := d.QueryRow(
		"UPDATE agent_status SET root_agent_name = 'coordinator', harness = '' WHERE session_name = ? RETURNING session_name",
		coordName,
	).Scan(new(string)); err != nil {
		t.Fatalf("seed coordinator: set root_agent_name: %v", err)
	}

	return sc, clk
}

// seedWorkerIdle writes an idle agent_status row for the worker before the
// sidecar runs, reproducing the production sequence where tmux-session-start
// has set state=idle prior to the PI socket-pipe handshake. The persisted
// agent_status.state stays "idle" through handshake (which only writes a
// state_change *event*, not an upsert) until a turn_start frame arrives. The
// zero-output bug is reproduced when no turn_start ever arrives.
func seedWorkerIdle(t *testing.T, sc *Sidecar) {
	t.Helper()
	if err := sc.cfg.DB.UpsertStatus(sc.cfg.SessionName, sc.cfg.Repo, sc.cfg.Worktree, string(agent.StateIdle), nil, nil); err != nil {
		t.Fatalf("seed worker idle row: %v", err)
	}
}

// TestSocketPipe_ZeroOutputExit_ClassifiesAsError reproduces the zero-output
// exit: a worker connects, performs the handshake, receives a turn_end +
// state_change{finished} pair without ever emitting a turn_start (and
// therefore without producing any assistant output), and disconnects. Without
// the classification, this writes StateFinished and notifies the coordinator
// with the "has finished" wording — leading the coordinator to chase a PR
// that did not exist.
//
// After the fix, the finished-debounce callback consults the persisted state:
// when it is still StateIdle (no turn_start ever fired), the sidecar honours
// the state machine's idle→finished rejection and routes to StateError with
// the "has errored" wording instead.
func TestSocketPipe_ZeroOutputExit_ClassifiesAsError(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newZeroOutputWorkerSidecar(t, sockPath)

	// Reproduce the production pre-handshake state: tmux-session-start has
	// written state=idle to agent_status. The handshake's writeStateChange
	// writes an event but does NOT upsert agent_status, so this row remains
	// the authoritative state when the finished debounce fires.
	seedWorkerIdle(t, sc)

	// Capture the notification text via the deliverFn seam so we can assert
	// on the exact wording without performing real network delivery.
	var (
		deliverMu     sync.Mutex
		capturedText  string
		capturedToCnt int
	)
	sc.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		deliverMu.Lock()
		defer deliverMu.Unlock()
		capturedToCnt++
		capturedText = text
		return nil
	}

	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sockPath)

	// Reproduce the issue's frame sequence verbatim: turn_end with the root
	// agent name, immediately followed by state_change{finished}. Crucially
	// there is NO turn_start — the worker exits without producing output.
	sendJSON(t, conn, map[string]any{"type": "turn_end", "agent": "worker"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	// Wait deterministically for the finished debounce timer to be registered.
	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer was created after state_change{finished}")
	}

	// Pre-fire state must still be idle (handshake does not upsert).
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateIdle) {
		t.Fatalf("pre-fire state = %q, want %q (handshake must not upsert agent_status)", s, agent.StateIdle)
	}

	timer.Fire()

	// State must land in error (NOT finished). Poll because the debounce
	// goroutine commits asynchronously.
	got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateError), 2*time.Second)
	if got != string(agent.StateError) {
		t.Errorf("state after zero-output debounce = %q, want %q", got, agent.StateError)
	}
	if got == string(agent.StateFinished) {
		t.Error("zero-output exit was misclassified as finished — issue #2081 regression")
	}

	// Drain notify goroutines so the deliverFn seam captures have committed.
	sc.WaitNotifies()

	deliverMu.Lock()
	defer deliverMu.Unlock()
	if capturedToCnt != 1 {
		t.Fatalf("deliverFn invocations = %d, want 1", capturedToCnt)
	}
	wantText := "Agent testrepo@feature has errored its current task"
	if capturedText != wantText {
		t.Errorf("coordinator notification text = %q, want %q", capturedText, wantText)
	}
	if strings.Contains(capturedText, "has finished") {
		t.Errorf("coordinator notification still uses 'has finished' wording: %q", capturedText)
	}

	conn.Close()
	_ = wait()
}

// TestSocketPipe_NormalExitWithContent_StillClassifiesFinished is the
// negative counterpart to TestSocketPipe_ZeroOutputExit_ClassifiesAsError.
// A worker that emits a normal turn_start → msg_assistant
// → turn_end → state_change{finished} sequence must still land in StateFinished
// with the "has finished" coordinator notification. The fix must not be
// over-broad and silently relabel normal exits as errors.
func TestSocketPipe_NormalExitWithContent_StillClassifiesFinished(t *testing.T) {
	sockPath := shortSockPath(t)
	sc, clk := newZeroOutputWorkerSidecar(t, sockPath)

	// Same pre-handshake state seeding as the zero-output test: tmux has
	// written state=idle. The turn_start frame should transition the
	// persisted state to active, so the finished debounce sees a valid
	// active→finished transition.
	seedWorkerIdle(t, sc)

	var (
		deliverMu    sync.Mutex
		capturedText string
		capturedCnt  int
	)
	sc.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		deliverMu.Lock()
		defer deliverMu.Unlock()
		capturedCnt++
		capturedText = text
		return nil
	}

	wait := runSocketPipeSidecar(sc)
	conn, _ := dialAndHandshake(t, sockPath)

	// Normal frame sequence: turn_start moves agent_status to active, a
	// msg_assistant carries actual text content, turn_end closes the turn,
	// state_change{finished} starts the debounce.
	sendJSON(t, conn, map[string]any{"type": "turn_start"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "text": "hello from the worker"})
	sendJSON(t, conn, map[string]any{"type": "turn_end", "agent": "worker"})
	sendJSON(t, conn, map[string]any{"type": "state_change", "state": "finished"})

	timer := clk.WaitForTimerCount(1, 5*time.Second)
	if timer == nil {
		t.Fatal("no finished debounce timer was created after state_change{finished}")
	}

	// Pre-fire state must be active (turn_start transitioned idle→active).
	if s := getState(t, sc.cfg.DB, sc.cfg.SessionName); s != string(agent.StateActive) {
		t.Fatalf("pre-fire state = %q, want %q (turn_start must upsert to active)", s, agent.StateActive)
	}

	timer.Fire()

	got := waitForState(t, sc.cfg.DB, sc.cfg.SessionName, string(agent.StateFinished), 2*time.Second)
	if got != string(agent.StateFinished) {
		t.Errorf("state after normal-exit debounce = %q, want %q", got, agent.StateFinished)
	}
	if got == string(agent.StateError) {
		t.Error("normal-exit worker was misclassified as error — zero-output fix is over-broad")
	}

	sc.WaitNotifies()

	deliverMu.Lock()
	defer deliverMu.Unlock()
	if capturedCnt != 1 {
		t.Fatalf("deliverFn invocations = %d, want 1", capturedCnt)
	}
	wantText := "Agent testrepo@feature has finished its current task"
	if capturedText != wantText {
		t.Errorf("coordinator notification text = %q, want %q", capturedText, wantText)
	}
	if strings.Contains(capturedText, "has errored") {
		t.Errorf("coordinator notification incorrectly uses 'has errored' wording for a normal exit: %q", capturedText)
	}

	conn.Close()
	_ = wait()
}
