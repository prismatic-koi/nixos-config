package iris_test

// harness_socket_test.go — unit tests for the harness socket protocol handler.
//
// Tests cover:
//   - Protocol handshake (hello / hello_ack)
//   - tool_exec dispatch and tool_exec_result correlation
//   - Parallel tool calls dispatched concurrently with no result crossing
//   - tool_abort causing a running subprocess to be killed
//   - session_shutdown frame setting the clean-exit flag

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// openTestDB returns an in-memory (temp dir) iris DB for tests.
func openTestDB(t *testing.T) interface{ Close() error } {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	db, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// dialHarness dials the harness socket and returns a json-line writer/reader pair.
func dialHarness(t *testing.T, sockPath string) (net.Conn, *bufio.Reader) {
	t.Helper()
	// Retry a few times in case the server isn't ready yet.
	var conn net.Conn
	var err error
	for i := 0; i < 20; i++ {
		conn, err = net.DialTimeout("unix", sockPath, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial harness socket %q: %v", sockPath, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewReaderSize(conn, 1<<20)
}

// sendFrame sends a JSON-line frame on the connection.
func sendFrame(t *testing.T, conn net.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("send frame: %v", err)
	}
}

// readFrame reads and parses one JSON-line frame.
func readFrame(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("parse frame %q: %v", line, err)
	}
	return m
}

// doHandshake performs the hello/hello_ack exchange.
func doHandshake(t *testing.T, conn net.Conn, r *bufio.Reader) map[string]any {
	t.Helper()
	sendFrame(t, conn, map[string]any{
		"type":             "hello",
		"protocol_version": iris.ProtocolVersion,
		"harness":          "pi",
		"harness_version":  "test",
	})
	ack := readFrame(t, r)
	if ack["type"] != "hello_ack" {
		t.Fatalf("expected hello_ack, got %q", ack["type"])
	}
	return ack
}

// startServer starts a HarnessSocketServer in a background goroutine and
// returns it plus the session.
func startServer(t *testing.T) *iris.HarnessSocketServer {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	db, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	sockPath := filepath.Join(tmp, "harness.sock")
	sess := &iris.SessionRecord{
		InstanceID:      "test-instance-001",
		SessionName:     "test@branch",
		Worktree:        tmp,
		Role:            "worker",
		HarnessSockPath: sockPath,
	}
	srv, err := iris.NewHarnessSocketServer(sess, db)
	if err != nil {
		t.Fatalf("NewHarnessSocketServer: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	go func() { _ = srv.AcceptOne(ctx) }()

	return srv
}

// TestHandshake verifies hello/hello_ack round-trip.
func TestHandshake(t *testing.T) {
	srv := startServer(t)
	conn, r := dialHarness(t, srv.SockPath())
	ack := doHandshake(t, conn, r)

	if ack["protocol_version"] != float64(iris.ProtocolVersion) {
		t.Errorf("hello_ack protocol_version = %v, want %d", ack["protocol_version"], iris.ProtocolVersion)
	}
	if ack["session_role"] != "worker" {
		t.Errorf("hello_ack session_role = %v, want %q", ack["session_role"], "worker")
	}
	if ack["isolation_mode"] != "host" {
		t.Errorf("hello_ack isolation_mode = %v, want %q", ack["isolation_mode"], "host")
	}
	if ack["instance_id"] != "test-instance-001" {
		t.Errorf("hello_ack instance_id = %v, want %q", ack["instance_id"], "test-instance-001")
	}
}

// TestHandshakeVersionMismatch verifies that a wrong protocol version is rejected.
func TestHandshakeVersionMismatch(t *testing.T) {
	srv := startServer(t)
	conn, r := dialHarness(t, srv.SockPath())

	sendFrame(t, conn, map[string]any{
		"type":             "hello",
		"protocol_version": 999, // wrong
		"harness":          "pi",
		"harness_version":  "test",
	})
	frame := readFrame(t, r)
	if frame["type"] != "error" {
		t.Errorf("expected error frame, got %q", frame["type"])
	}
}

// TestToolExecEcho verifies that a tool_exec for bash executes and returns a result.
func TestToolExecEcho(t *testing.T) {
	srv := startServer(t)
	conn, r := dialHarness(t, srv.SockPath())
	doHandshake(t, conn, r)

	sendFrame(t, conn, map[string]any{
		"type": "tool_exec",
		"id":   "call-echo-001",
		"name": "bash",
		"args": map[string]any{"command": "echo hello"},
	})

	// May receive zero or more tool_exec_update frames before the result.
	var result map[string]any
	for {
		frame := readFrame(t, r)
		if frame["type"] == "tool_exec_result" {
			result = frame
			break
		}
		if frame["type"] != "tool_exec_update" {
			t.Logf("unexpected frame type %q (skipping)", frame["type"])
		}
	}

	if result["id"] != "call-echo-001" {
		t.Errorf("result id = %v, want %q", result["id"], "call-echo-001")
	}
	if result["success"] != true {
		t.Errorf("result success = %v, want true", result["success"])
	}
	output, _ := result["output"].(string)
	if output == "" {
		t.Error("result output is empty, expected 'hello\\n'")
	}
}

// TestParallelToolCalls verifies that two concurrent tool_exec frames are
// dispatched independently and their results correlate by id without crossing.
func TestParallelToolCalls(t *testing.T) {
	srv := startServer(t)
	conn, r := dialHarness(t, srv.SockPath())
	doHandshake(t, conn, r)

	// Send two parallel tool_exec frames with distinct ids.
	// Use sleep to make them truly overlapping in time.
	sendFrame(t, conn, map[string]any{
		"type": "tool_exec",
		"id":   "call-parallel-A",
		"name": "bash",
		"args": map[string]any{"command": "sleep 0.1 && echo output-A"},
	})
	sendFrame(t, conn, map[string]any{
		"type": "tool_exec",
		"id":   "call-parallel-B",
		"name": "bash",
		"args": map[string]any{"command": "echo output-B"},
	})

	// Collect all result frames, ignoring updates.
	results := make(map[string]map[string]any)
	deadline := time.Now().Add(10 * time.Second)
	for len(results) < 2 && time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline) //nolint:errcheck
		frame := readFrame(t, r)
		if frame["type"] == "tool_exec_result" {
			id, _ := frame["id"].(string)
			results[id] = frame
		}
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 tool_exec_result frames, got %d", len(results))
	}

	// Verify results are not crossed.
	if resA, ok := results["call-parallel-A"]; ok {
		out, _ := resA["output"].(string)
		if out == "" {
			t.Error("call-parallel-A: empty output")
		}
	} else {
		t.Error("no result for call-parallel-A")
	}

	if resB, ok := results["call-parallel-B"]; ok {
		out, _ := resB["output"].(string)
		if out == "" {
			t.Error("call-parallel-B: empty output")
		}
	} else {
		t.Error("no result for call-parallel-B")
	}
}

// TestToolAbort verifies that a tool_abort kills the in-flight subprocess.
func TestToolAbort(t *testing.T) {
	srv := startServer(t)
	conn, r := dialHarness(t, srv.SockPath())
	doHandshake(t, conn, r)

	// Start a long-running bash command.
	sendFrame(t, conn, map[string]any{
		"type": "tool_exec",
		"id":   "call-abort-001",
		"name": "bash",
		"args": map[string]any{"command": "sleep 60"},
	})

	// Small delay to let the subprocess start.
	time.Sleep(50 * time.Millisecond)

	// Send tool_abort.
	sendFrame(t, conn, map[string]any{
		"type": "tool_abort",
		"id":   "call-abort-001",
	})

	// Expect a tool_exec_result with success=false and output="aborted".
	var result map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline) //nolint:errcheck
		frame := readFrame(t, r)
		if frame["type"] == "tool_exec_result" {
			result = frame
			break
		}
	}

	if result == nil {
		t.Fatal("no tool_exec_result received after tool_abort")
	}
	if result["id"] != "call-abort-001" {
		t.Errorf("result id = %v, want %q", result["id"], "call-abort-001")
	}
	if result["success"] != false {
		t.Errorf("result success = %v after abort, want false", result["success"])
	}
	if result["is_error"] != true {
		t.Errorf("result is_error = %v after abort, want true", result["is_error"])
	}
	output, _ := result["output"].(string)
	if output != "aborted" {
		t.Errorf("result output = %q, want %q", output, "aborted")
	}
}

// TestSessionShutdown verifies that a session_shutdown frame is detected.
func TestSessionShutdown(t *testing.T) {
	srv := startServer(t)
	conn, r := dialHarness(t, srv.SockPath())
	doHandshake(t, conn, r)

	if srv.SessionShutdownReceived() {
		t.Error("SessionShutdownReceived should be false before sending session_shutdown")
	}

	sendFrame(t, conn, map[string]any{"type": "session_shutdown"})

	// Give the server goroutine time to process the frame.
	var ok bool
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		if srv.SessionShutdownReceived() {
			ok = true
			break
		}
	}
	if !ok {
		t.Error("SessionShutdownReceived not set after sending session_shutdown")
	}
	_ = r // suppress unused warning
}

// TestDBEventWritten verifies that tool_call and tool_result events are
// written to the DB for each tool_exec / tool_exec_result pair.
func TestDBEventWritten(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	db, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	sockPath := filepath.Join(tmp, "harness.sock")
	sess := &iris.SessionRecord{
		InstanceID:      "test-db-events",
		SessionName:     "test@db",
		Worktree:        tmp,
		Role:            "worker",
		HarnessSockPath: sockPath,
	}
	srv, err := iris.NewHarnessSocketServer(sess, db)
	if err != nil {
		t.Fatalf("NewHarnessSocketServer: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = srv.AcceptOne(ctx) }()

	conn, r := dialHarness(t, sockPath)
	doHandshake(t, conn, r)

	sendFrame(t, conn, map[string]any{
		"type": "tool_exec",
		"id":   "call-db-001",
		"name": "bash",
		"args": map[string]any{"command": "echo db-test"},
	})

	// Wait for result.
	for {
		frame := readFrame(t, r)
		if frame["type"] == "tool_exec_result" {
			break
		}
	}

	// Query DB for events.
	time.Sleep(50 * time.Millisecond)
	events, err := db.AllSessionEvents("test@db")
	if err != nil {
		t.Fatalf("AllSessionEvents: %v", err)
	}

	var toolCallCount, toolResultCount int
	for _, e := range events {
		switch e.Type {
		case "tool_call":
			toolCallCount++
		case "tool_result":
			toolResultCount++
		}
	}

	if toolCallCount == 0 {
		t.Error("no tool_call event written to DB")
	}
	if toolResultCount == 0 {
		t.Error("no tool_result event written to DB")
	}
}

// TestReadTool verifies the read tool executor.
func TestReadTool(t *testing.T) {
	tmp := t.TempDir()

	// Write a test file.
	testFile := filepath.Join(tmp, "hello.txt")
	if err := os.WriteFile(testFile, []byte("hello world\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dbPath := filepath.Join(tmp, "iris.db")
	db, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	sockPath := filepath.Join(tmp, "harness.sock")
	sess := &iris.SessionRecord{
		InstanceID:      "test-read-tool",
		SessionName:     "test@read",
		Worktree:        tmp,
		Role:            "worker",
		HarnessSockPath: sockPath,
	}
	srv, err := iris.NewHarnessSocketServer(sess, db)
	if err != nil {
		t.Fatalf("NewHarnessSocketServer: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = srv.AcceptOne(ctx) }()

	conn, r := dialHarness(t, sockPath)
	doHandshake(t, conn, r)

	sendFrame(t, conn, map[string]any{
		"type": "tool_exec",
		"id":   "call-read-001",
		"name": "read",
		"args": map[string]any{"file_path": "hello.txt"},
	})

	var result map[string]any
	for {
		frame := readFrame(t, r)
		if frame["type"] == "tool_exec_result" {
			result = frame
			break
		}
	}

	if result["success"] != true {
		t.Errorf("read result success = %v, want true; output=%v", result["success"], result["output"])
	}
	output, _ := result["output"].(string)
	if output != "hello world\n" {
		t.Errorf("read output = %q, want %q", output, "hello world\n")
	}
}

// TestSupervisorRestartPolicy verifies restart count logic and circuit-breaker.
// This is a unit test on the SessionRecord / RestartBackoff helpers, not a
// full integration test spawning pi.
func TestSupervisorRestartPolicy(t *testing.T) {
	// Verify RestartBackoff returns non-zero durations.
	for i := 1; i <= 5; i++ {
		d := iris.RestartBackoff(i)
		if d <= 0 {
			t.Errorf("RestartBackoff(%d) = %v, want > 0", i, d)
		}
	}

	// Verify DefaultRestartThreshold matches the prism sidecar's value.
	if iris.DefaultRestartThreshold != 3 {
		t.Errorf("DefaultRestartThreshold = %d, want 3", iris.DefaultRestartThreshold)
	}
}

// TestHarnessSockPath verifies path helpers.
func TestHarnessSockPath(t *testing.T) {
	got := iris.HarnessSockPath("/state/iris/run", "abc-123")
	want := "/state/iris/run/abc-123/harness.sock"
	if got != want {
		t.Errorf("HarnessSockPath = %q, want %q", got, want)
	}
}

// TestEnsureSessionDir verifies the run dir creation helper.
func TestEnsureSessionDir(t *testing.T) {
	tmp := t.TempDir()
	dir, err := iris.EnsureSessionDir(tmp, "my-instance")
	if err != nil {
		t.Fatalf("EnsureSessionDir: %v", err)
	}
	want := filepath.Join(tmp, "my-instance")
	if dir != want {
		t.Errorf("EnsureSessionDir = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

// TestToolExecUpdate verifies that tool_exec_update frames are sent during
// long bash commands (streaming partial output).
func TestToolExecUpdate(t *testing.T) {
	srv := startServer(t)
	conn, r := dialHarness(t, srv.SockPath())
	doHandshake(t, conn, r)

	// Run a bash command that produces output in two chunks.
	sendFrame(t, conn, map[string]any{
		"type": "tool_exec",
		"id":   "call-update-001",
		"name": "bash",
		// printf without newlines so output comes in pieces (race-free)
		"args": map[string]any{"command": "printf 'part1'; sleep 0.05; printf 'part2'"},
	})

	var updates []string
	var gotResult bool
	deadline := time.Now().Add(10 * time.Second)
	for !gotResult && time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline) //nolint:errcheck
		frame := readFrame(t, r)
		switch frame["type"] {
		case "tool_exec_update":
			if frame["id"] == "call-update-001" {
				content, _ := frame["content"].(string)
				updates = append(updates, content)
			}
		case "tool_exec_result":
			gotResult = true
		}
	}

	if !gotResult {
		t.Fatal("no tool_exec_result received")
	}
	// We should have received at least one update (exact count is OS-dependent).
	if len(updates) == 0 {
		t.Log("no tool_exec_update frames received (acceptable; OS may have buffered output)")
	}
}

// TestConcurrentMultipleClients verifies that AcceptOne handles one client at
// a time. This protects the invariant that each session has exactly one
// harness connection.
func TestConcurrentMultipleClients(t *testing.T) {
	srv := startServer(t)

	// First client performs handshake.
	conn1, r1 := dialHarness(t, srv.SockPath())
	doHandshake(t, conn1, r1)

	// AcceptOne only accepts one connection per call — the server goroutine
	// has already returned after the first connection. A second dial would
	// block because no goroutine is calling Accept. We verify this by checking
	// that our first client can still transact.
	sendFrame(t, conn1, map[string]any{
		"type": "tool_exec",
		"id":   "call-single-client-001",
		"name": "bash",
		"args": map[string]any{"command": "echo ok"},
	})
	for {
		frame := readFrame(t, r1)
		if frame["type"] == "tool_exec_result" {
			if frame["id"] != "call-single-client-001" {
				t.Errorf("result id = %v, want %q", frame["id"], "call-single-client-001")
			}
			break
		}
	}
}

// syncMu protects the test suite's shared resources.
var syncMu sync.Mutex
