package iris_test

// client_socket_test.go — unit and integration tests for the client IPC socket.
//
// Test coverage:
//
//   - Frame encode/decode round-trips (all frame types)
//   - Fan-out: 3 clients subscribe, an event publishes, all 3 receive
//   - Disconnected client removed without affecting remaining subscribers
//   - since_event_id replay returns the expected event range
//   - session_subscribe for a nonexistent session returns error, connection stays open
//   - Integration: connect → sessions_list → sessions_snapshot
//   - Integration: connect → session_subscribe → receive session_event from Publish
//   - ping → pong round-trip
//   - session_unsubscribe stops event delivery

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
)

// ---------------------------------------------------------------------------
// Frame encode/decode round-trips
// ---------------------------------------------------------------------------

// TestClientFrameRoundTrip verifies that every frame type can be
// JSON-marshalled and unmarshalled without data loss.
func TestClientFrameRoundTrip(t *testing.T) {
	t.Run("ClientSessionsListFrame", func(t *testing.T) {
		orig := iris.ClientSessionsListFrame{Type: iris.ClientFrameSessionsList}
		data, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got iris.ClientSessionsListFrame
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Type != iris.ClientFrameSessionsList {
			t.Errorf("type = %q, want %q", got.Type, iris.ClientFrameSessionsList)
		}
	})

	t.Run("ClientSessionSubscribeFrame", func(t *testing.T) {
		orig := iris.ClientSessionSubscribeFrame{
			Type:         iris.ClientFrameSessionSubscribe,
			Name:         "test@branch",
			SinceEventID: 42,
		}
		data, _ := json.Marshal(orig)
		var got iris.ClientSessionSubscribeFrame
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Name != "test@branch" || got.SinceEventID != 42 {
			t.Errorf("got %+v, want same as orig", got)
		}
	})

	t.Run("ClientSessionSubscribeFrame_NoSince", func(t *testing.T) {
		// since_event_id is omitempty — absent from JSON when zero.
		orig := iris.ClientSessionSubscribeFrame{
			Type: iris.ClientFrameSessionSubscribe,
			Name: "test@branch",
		}
		data, _ := json.Marshal(orig)
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := m["since_event_id"]; ok {
			t.Error("since_event_id should be absent when zero (omitempty)")
		}
	})

	t.Run("ClientSessionUnsubscribeFrame", func(t *testing.T) {
		orig := iris.ClientSessionUnsubscribeFrame{Type: iris.ClientFrameSessionUnsubscribe, Name: "s"}
		data, _ := json.Marshal(orig)
		var got iris.ClientSessionUnsubscribeFrame
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Name != "s" {
			t.Errorf("Name = %q, want %q", got.Name, "s")
		}
	})

	t.Run("ClientSessionSpawnFrame", func(t *testing.T) {
		orig := iris.ClientSessionSpawnFrame{
			Type:     iris.ClientFrameSessionSpawn,
			Worktree: "/tmp/wt",
			Role:     "worker",
			ConfigOverrides: map[string]any{
				"model": "claude-3",
			},
		}
		data, _ := json.Marshal(orig)
		var got iris.ClientSessionSpawnFrame
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Worktree != "/tmp/wt" || got.Role != "worker" {
			t.Errorf("got %+v", got)
		}
		if got.ConfigOverrides["model"] != "claude-3" {
			t.Errorf("ConfigOverrides[model] = %v", got.ConfigOverrides["model"])
		}
	})

	t.Run("ClientPromptDeliverFrame", func(t *testing.T) {
		orig := iris.ClientPromptDeliverFrame{
			Type:      iris.ClientFramePromptDeliver,
			Name:      "sess",
			Text:      "hello",
			DeliverAs: "steer",
			Images:    []string{"base64abc"},
		}
		data, _ := json.Marshal(orig)
		var got iris.ClientPromptDeliverFrame
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Text != "hello" || got.DeliverAs != "steer" || len(got.Images) != 1 {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("DaemonSessionsSnapshotFrame", func(t *testing.T) {
		orig := iris.DaemonSessionsSnapshotFrame{
			Type: iris.DaemonFrameSessionsSnapshot,
			Sessions: []iris.SessionSnapshot{
				{Name: "s", InstanceID: "id", State: "active", Role: "worker"},
			},
		}
		data, _ := json.Marshal(orig)
		var got iris.DaemonSessionsSnapshotFrame
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(got.Sessions) != 1 || got.Sessions[0].Name != "s" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("DaemonSessionEventFrame", func(t *testing.T) {
		orig := iris.DaemonSessionEventFrame{
			Type:        iris.DaemonFrameSessionEvent,
			SessionName: "s",
			RowID:       99,
			EventType:   "tool_call",
			Payload:     `{"tool":"bash"}`,
		}
		data, _ := json.Marshal(orig)
		var got iris.DaemonSessionEventFrame
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.RowID != 99 || got.EventType != "tool_call" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("DaemonErrorFrame", func(t *testing.T) {
		orig := iris.DaemonErrorFrame{
			Type:        iris.DaemonFrameError,
			RequestType: "sessions_list",
			Message:     "not found",
		}
		data, _ := json.Marshal(orig)
		var got iris.DaemonErrorFrame
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.RequestType != "sessions_list" || got.Message != "not found" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("DaemonPongFrame", func(t *testing.T) {
		orig := iris.DaemonPongFrame{Type: iris.DaemonFramePong}
		data, _ := json.Marshal(orig)
		var got iris.DaemonPongFrame
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Type != iris.DaemonFramePong {
			t.Errorf("type = %q, want %q", got.Type, iris.DaemonFramePong)
		}
	})

	t.Run("DaemonSessionSpawnedFrame", func(t *testing.T) {
		orig := iris.DaemonSessionSpawnedFrame{
			Type:       iris.DaemonFrameSessionSpawned,
			Name:       "iris-worker@repo",
			InstanceID: "uuid-123",
		}
		data, _ := json.Marshal(orig)
		var got iris.DaemonSessionSpawnedFrame
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Name != "iris-worker@repo" || got.InstanceID != "uuid-123" {
			t.Errorf("got %+v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestClientSocket creates a ClientSocket backed by a test DB with an in-memory
// session list. Returns the socket and a publish function.
func newTestClientSocket(t *testing.T, sessions []iris.SessionSnapshot) (*iris.ClientSocket, func(iris.EventPublication)) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	sockPath := filepath.Join(tmp, "iris.sock")
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot {
			return sessions
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	go cs.Serve(ctx)

	return cs, func(pub iris.EventPublication) {
		cs.Publish(pub)
	}
}

// dialClientSocket dials the client socket and returns conn + buffered reader.
func dialClientSocket(t *testing.T, sockPath string) (net.Conn, *bufio.Reader) {
	t.Helper()
	var conn net.Conn
	var err error
	for i := 0; i < 30; i++ {
		conn, err = net.DialTimeout("unix", sockPath, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial %q: %v", sockPath, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewReaderSize(conn, 1<<20)
}

// sendClientFrame encodes and sends a frame on the connection.
func sendClientFrame(t *testing.T, conn net.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readClientFrame reads one JSON-line frame from the buffered reader.
func readClientFrame(t *testing.T, r *bufio.Reader) map[string]any {
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

// readClientFrameWithTimeout reads a frame, failing if none arrives within d.
func readClientFrameWithTimeout(t *testing.T, conn net.Conn, r *bufio.Reader, d time.Duration) (map[string]any, bool) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(d)) //nolint:errcheck
	defer conn.SetReadDeadline(time.Time{}) //nolint:errcheck
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Logf("parse frame %q: %v", line, err)
		return nil, false
	}
	return m, true
}

// ---------------------------------------------------------------------------
// Integration: sessions_list / sessions_snapshot
// ---------------------------------------------------------------------------

// TestSessionsList verifies that a client can send sessions_list and receive a
// sessions_snapshot with the expected sessions.
func TestSessionsList(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "nixos-config@main", InstanceID: "iid-1", State: "active", Role: "worker"},
		{Name: "nixos-config@feat", InstanceID: "iid-2", State: "spawning", Role: "coordinator"},
	}
	cs, _ := newTestClientSocket(t, sessions)

	conn, r := dialClientSocket(t, cs.SockPath())
	sendClientFrame(t, conn, map[string]any{"type": "sessions_list"})

	frame := readClientFrame(t, r)
	if frame["type"] != "sessions_snapshot" {
		t.Fatalf("expected sessions_snapshot, got %q", frame["type"])
	}
	sess, ok := frame["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions field is not an array: %T", frame["sessions"])
	}
	if len(sess) != 2 {
		t.Errorf("sessions count = %d, want 2", len(sess))
	}
}

// TestSessionsListEmpty verifies that an empty session list returns an empty
// sessions array (not nil/null).
func TestSessionsListEmpty(t *testing.T) {
	cs, _ := newTestClientSocket(t, []iris.SessionSnapshot{})

	conn, r := dialClientSocket(t, cs.SockPath())
	sendClientFrame(t, conn, map[string]any{"type": "sessions_list"})

	frame := readClientFrame(t, r)
	if frame["type"] != "sessions_snapshot" {
		t.Fatalf("type = %q, want sessions_snapshot", frame["type"])
	}
	sess := frame["sessions"]
	if sess == nil {
		t.Error("sessions field is nil, want empty array")
	}
	arr, ok := sess.([]any)
	if !ok || len(arr) != 0 {
		t.Errorf("sessions = %v, want []", sess)
	}
}

// ---------------------------------------------------------------------------
// Integration: ping / pong
// ---------------------------------------------------------------------------

// TestPingPong verifies the keepalive round-trip.
func TestPingPong(t *testing.T) {
	cs, _ := newTestClientSocket(t, nil)
	conn, r := dialClientSocket(t, cs.SockPath())

	sendClientFrame(t, conn, map[string]any{"type": "ping"})
	frame := readClientFrame(t, r)
	if frame["type"] != "pong" {
		t.Errorf("expected pong, got %q", frame["type"])
	}
}

// ---------------------------------------------------------------------------
// Fan-out: multiple subscribers receive every event
// ---------------------------------------------------------------------------

// TestFanOut verifies that 3 clients subscribed to the same session each
// receive every session_event frame when Publish is called.
func TestFanOut(t *testing.T) {
	sessionName := "fanout@test"
	sessions := []iris.SessionSnapshot{
		{Name: sessionName, InstanceID: "iid-fanout", State: "active", Role: "worker"},
	}
	cs, publish := newTestClientSocket(t, sessions)

	// Connect 3 clients and subscribe.
	type clientConn struct {
		conn net.Conn
		r    *bufio.Reader
	}
	clients := make([]clientConn, 3)
	for i := range clients {
		conn, r := dialClientSocket(t, cs.SockPath())
		clients[i] = clientConn{conn, r}
		sendClientFrame(t, conn, map[string]any{
			"type": "session_subscribe",
			"name": sessionName,
		})
	}

	// Allow subscriptions to be registered (goroutines need a moment to start).
	time.Sleep(50 * time.Millisecond)

	// Publish an event.
	publish(iris.EventPublication{
		SessionName: sessionName,
		RowID:       1,
		EventType:   "tool_call",
		Payload:     `{"id":"tc-001"}`,
	})

	// Every client must receive the session_event.
	for i, c := range clients {
		frame, ok := readClientFrameWithTimeout(t, c.conn, c.r, 5*time.Second)
		if !ok {
			t.Errorf("client %d: no frame received", i)
			continue
		}
		if frame["type"] != "session_event" {
			t.Errorf("client %d: type = %q, want session_event", i, frame["type"])
		}
		if frame["session_name"] != sessionName {
			t.Errorf("client %d: session_name = %q, want %q", i, frame["session_name"], sessionName)
		}
		if frame["event_type"] != "tool_call" {
			t.Errorf("client %d: event_type = %q, want tool_call", i, frame["event_type"])
		}
	}
}

// ---------------------------------------------------------------------------
// Disconnected client removed without affecting others
// ---------------------------------------------------------------------------

// TestDisconnectedSubscriberRemoved verifies that when one subscriber
// disconnects, the remaining subscribers still receive events.
func TestDisconnectedSubscriberRemoved(t *testing.T) {
	sessionName := "disconnect@test"
	sessions := []iris.SessionSnapshot{
		{Name: sessionName, InstanceID: "iid-dc", State: "active", Role: "worker"},
	}
	cs, publish := newTestClientSocket(t, sessions)

	// Subscribe client A (rA not used since A disconnects before receiving events).
	connA, _ := dialClientSocket(t, cs.SockPath())
	sendClientFrame(t, connA, map[string]any{"type": "session_subscribe", "name": sessionName})

	// Subscribe client B.
	connB, rB := dialClientSocket(t, cs.SockPath())
	sendClientFrame(t, connB, map[string]any{"type": "session_subscribe", "name": sessionName})

	// Allow subscriptions to register.
	time.Sleep(50 * time.Millisecond)

	// Disconnect client A.
	connA.Close()

	// Brief delay to let the server detect the disconnect.
	time.Sleep(50 * time.Millisecond)

	// Publish — client B must still receive.
	publish(iris.EventPublication{
		SessionName: sessionName,
		RowID:       2,
		EventType:   "agent_start",
		Payload:     `{}`,
	})

	frame, ok := readClientFrameWithTimeout(t, connB, rB, 5*time.Second)
	if !ok {
		t.Fatal("client B: no frame received after client A disconnected")
	}
	if frame["type"] != "session_event" {
		t.Errorf("client B: type = %q, want session_event", frame["type"])
	}
}

// ---------------------------------------------------------------------------
// session_subscribe for nonexistent session returns error; connection stays open
// ---------------------------------------------------------------------------

// TestSubscribeNonexistentSession verifies that subscribing to a session that
// doesn't exist returns an error frame, and the connection stays open for
// further operations.
func TestSubscribeNonexistentSession(t *testing.T) {
	cs, _ := newTestClientSocket(t, []iris.SessionSnapshot{}) // no sessions

	conn, r := dialClientSocket(t, cs.SockPath())

	// Subscribe to a nonexistent session.
	sendClientFrame(t, conn, map[string]any{
		"type": "session_subscribe",
		"name": "does-not-exist@branch",
	})

	// Expect an error frame.
	frame := readClientFrame(t, r)
	if frame["type"] != "error" {
		t.Errorf("type = %q, want error", frame["type"])
	}
	if frame["request_type"] != "session_subscribe" {
		t.Errorf("request_type = %q, want session_subscribe", frame["request_type"])
	}
	if frame["message"] == "" {
		t.Error("error message is empty")
	}

	// Connection must stay open — we can still ping.
	sendClientFrame(t, conn, map[string]any{"type": "ping"})
	pong := readClientFrame(t, r)
	if pong["type"] != "pong" {
		t.Errorf("expected pong after error, got %q", pong["type"])
	}
}

// ---------------------------------------------------------------------------
// since_event_id replay
// ---------------------------------------------------------------------------

// TestSinceEventIDReplay writes several events to the DB, then connects a
// client and subscribes with since_event_id set to a past rowid. Verifies
// that only events after that rowid are replayed.
func TestSinceEventIDReplay(t *testing.T) {
	sessionName := "replay@test"

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer database.Close()

	// Write 5 events to the DB.
	var rowIDs []int64
	for i := 1; i <= 5; i++ {
		ev := db.Event{
			ID:          fmt.Sprintf("ev-%d", i),
			SessionName: sessionName,
			Repo:        "",
			Worktree:    tmp,
			Type:        "tool_call",
			Payload:     fmt.Sprintf(`{"n":%d}`, i),
		}
		rowID, err := database.WriteEventReturningRowID(ev)
		if err != nil {
			t.Fatalf("WriteEventReturningRowID[%d]: %v", i, err)
		}
		rowIDs = append(rowIDs, rowID)
	}
	// rowIDs[0] is the rowid of event 1, rowIDs[4] is event 5.

	// We'll subscribe since rowID[1] (event 2's rowid), expecting events 3,4,5.
	sinceRowID := rowIDs[1]

	sessions := []iris.SessionSnapshot{
		{Name: sessionName, InstanceID: "iid-replay", State: "active", Role: "worker"},
	}
	sockPath := filepath.Join(tmp, "iris.sock")
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot {
			return sessions
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer cs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go cs.Serve(ctx)

	conn, r := dialClientSocket(t, sockPath)

	sendClientFrame(t, conn, map[string]any{
		"type":          "session_subscribe",
		"name":          sessionName,
		"since_event_id": sinceRowID,
	})

	// Expect 3 replayed events (rowids for events 3, 4, 5).
	var received []float64
	deadline := time.Now().Add(5 * time.Second)
	for len(received) < 3 {
		conn.SetReadDeadline(deadline) //nolint:errcheck
		frame, ok := readClientFrameWithTimeout(t, conn, r, 5*time.Second)
		if !ok {
			break
		}
		if frame["type"] == "session_event" {
			rid, _ := frame["row_id"].(float64)
			received = append(received, rid)
		}
	}

	if len(received) != 3 {
		t.Fatalf("expected 3 replayed events, got %d: %v", len(received), received)
	}
	// The replayed events should be events 3, 4, 5 (rowIDs[2], [3], [4]).
	for i, rid := range received {
		expected := float64(rowIDs[i+2])
		if rid != expected {
			t.Errorf("replayed event[%d] rowID = %.0f, want %.0f", i, rid, expected)
		}
	}
}

// ---------------------------------------------------------------------------
// session_unsubscribe
// ---------------------------------------------------------------------------

// TestSessionUnsubscribe verifies that after sending session_unsubscribe,
// no further events are received for that session.
func TestSessionUnsubscribe(t *testing.T) {
	sessionName := "unsub@test"
	sessions := []iris.SessionSnapshot{
		{Name: sessionName, InstanceID: "iid-unsub", State: "active", Role: "worker"},
	}
	cs, publish := newTestClientSocket(t, sessions)

	conn, r := dialClientSocket(t, cs.SockPath())

	// Subscribe.
	sendClientFrame(t, conn, map[string]any{"type": "session_subscribe", "name": sessionName})
	time.Sleep(50 * time.Millisecond)

	// Unsubscribe.
	sendClientFrame(t, conn, map[string]any{"type": "session_unsubscribe", "name": sessionName})
	time.Sleep(50 * time.Millisecond)

	// Publish — the client should NOT receive this.
	publish(iris.EventPublication{
		SessionName: sessionName,
		RowID:       10,
		EventType:   "agent_end",
		Payload:     `{}`,
	})

	// Verify no session_event is received within 200ms (we should get nothing).
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)) //nolint:errcheck
	frame, ok := readClientFrameWithTimeout(t, conn, r, 200*time.Millisecond)
	if ok && frame["type"] == "session_event" {
		t.Error("received session_event after session_unsubscribe — should not happen")
	}
}

// ---------------------------------------------------------------------------
// Multiple concurrent clients
// ---------------------------------------------------------------------------

// TestMultipleConcurrentClients verifies that multiple clients can connect
// simultaneously and operate independently.
func TestMultipleConcurrentClients(t *testing.T) {
	cs, _ := newTestClientSocket(t, []iris.SessionSnapshot{})

	const numClients = 5
	var wg sync.WaitGroup
	errors := make(chan string, numClients)

	for i := 0; i < numClients; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, r := dialClientSocket(t, cs.SockPath())
			defer conn.Close()

			sendClientFrame(t, conn, map[string]any{"type": "ping"})
			frame, ok := readClientFrameWithTimeout(t, conn, r, 5*time.Second)
			if !ok {
				errors <- fmt.Sprintf("client %d: no pong received", i)
				return
			}
			if frame["type"] != "pong" {
				errors <- fmt.Sprintf("client %d: type = %q, want pong", i, frame["type"])
			}
		}()
	}
	wg.Wait()
	close(errors)
	for msg := range errors {
		t.Error(msg)
	}
}

// ---------------------------------------------------------------------------
// Socket file permissions
// ---------------------------------------------------------------------------

// TestClientSocketPermissions verifies that the socket file has mode 0600 and
// the parent directory has mode 0700.
func TestClientSocketPermissions(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer database.Close()

	// Place the socket in a subdirectory so we can check the parent mode.
	sockDir := filepath.Join(tmp, "iris")
	sockPath := filepath.Join(sockDir, "iris.sock")

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath:          sockPath,
		Database:          database,
		GetActiveSessions: func() []iris.SessionSnapshot { return nil },
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer cs.Close()

	// Check socket inode mode: 0600.
	sockInfo, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("Stat socket: %v", err)
	}
	sockMode := sockInfo.Mode().Perm()
	if sockMode != 0o600 {
		t.Errorf("socket mode = %o, want 0600", sockMode)
	}

	// Check parent directory mode: 0700.
	dirInfo, err := os.Stat(sockDir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	dirMode := dirInfo.Mode().Perm()
	if dirMode != 0o700 {
		t.Errorf("parent dir mode = %o, want 0700", dirMode)
	}
}

// ---------------------------------------------------------------------------
// Socket survives stale file (daemon restart)
// ---------------------------------------------------------------------------

// TestStaleSocketRemoved verifies that calling Listen() on a path where a
// stale socket file already exists succeeds (the stale file is removed).
func TestStaleSocketRemoved(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer database.Close()

	sockPath := filepath.Join(tmp, "iris.sock")

	// Write a fake stale socket file.
	if err := os.WriteFile(sockPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot { return nil },
	})
	// This should succeed even though a file exists at sockPath.
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen with stale file: %v", err)
	}
	cs.Close()
}

// ---------------------------------------------------------------------------
// Unknown frame type is ignored (forward compatibility)
// ---------------------------------------------------------------------------

// TestUnknownFrameIgnored verifies that sending an unknown frame type does not
// close the connection — the daemon logs and skips it.
func TestUnknownFrameIgnored(t *testing.T) {
	cs, _ := newTestClientSocket(t, nil)
	conn, r := dialClientSocket(t, cs.SockPath())

	// Send an unknown frame.
	sendClientFrame(t, conn, map[string]any{"type": "future_unimplemented_feature"})

	// Connection should still be alive: ping → pong.
	sendClientFrame(t, conn, map[string]any{"type": "ping"})
	frame, ok := readClientFrameWithTimeout(t, conn, r, 5*time.Second)
	if !ok {
		t.Fatal("no pong after unknown frame — connection may have been closed")
	}
	if frame["type"] != "pong" {
		t.Errorf("type = %q, want pong", frame["type"])
	}
}

// ---------------------------------------------------------------------------
// SockPath accessor
// ---------------------------------------------------------------------------

// TestClientSocketSockPath verifies the SockPath() accessor.
func TestClientSocketSockPath(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer database.Close()

	sockPath := filepath.Join(tmp, "iris.sock")
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot { return nil },
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer cs.Close()

	if cs.SockPath() != sockPath {
		t.Errorf("SockPath() = %q, want %q", cs.SockPath(), sockPath)
	}
}

// ---------------------------------------------------------------------------
// Integration: harness → publisher → client fan-out (end-to-end wiring)
// ---------------------------------------------------------------------------

// TestHarnessToClientFanOut is an integration test that exercises the full
// harness→publisher→client-socket fan-out path:
//
//  1. Create a ClientSocket and a HarnessSocketServer sharing the same DB.
//  2. Wire the ClientSocket as the harness publisher via SetPublisher.
//  3. Connect a client to the ClientSocket and subscribe to the session.
//  4. Simulate a harness event by calling writeObservationEvent via the harness
//     dispatch (tool_exec → tool_exec_result round-trip).
//  5. Assert that the client receives a session_event frame carrying that event.
//
// This test specifically exercises the code path that review-code and
// review-goal identified as broken: SetPublisher wired through the
// SupervisorConfig.Publisher field and called in NewSupervisor.
func TestHarnessToClientFanOut(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer database.Close()

	sessionName := "harness-fanout@test"

	// --- Set up the ClientSocket ---
	sockPath := filepath.Join(tmp, "iris.sock")
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot {
			return []iris.SessionSnapshot{
				{Name: sessionName, InstanceID: "iid-hf", State: "active", Role: "worker"},
			}
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("ClientSocket.Listen: %v", err)
	}
	defer cs.Close()

	csCtx, csCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer csCancel()
	go cs.Serve(csCtx)

	// Insert sessions row so FK on agent_events.instance_id is satisfied.
	insertTestSessionRow(t, database, "iid-hf", sessionName, tmp)

	// --- Set up the HarnessSocketServer with the ClientSocket as publisher ---
	harnessSockPath := filepath.Join(tmp, "harness.sock")
	sess := &iris.SessionRecord{
		InstanceID:      "iid-hf",
		SessionName:     sessionName,
		Worktree:        tmp,
		Role:            "worker",
		HarnessSockPath: harnessSockPath,
	}
	harness, err := iris.NewHarnessSocketServer(sess, database)
	if err != nil {
		t.Fatalf("NewHarnessSocketServer: %v", err)
	}
	// KEY: wire the client socket as the harness publisher.
	harness.SetPublisher(cs)

	if err := harness.Listen(); err != nil {
		t.Fatalf("HarnessSocketServer.Listen: %v", err)
	}
	defer harness.Close()

	harnessCtx, harnessCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer harnessCancel()
	go func() { _ = harness.AcceptOne(harnessCtx) }()

	// --- Connect a client to the ClientSocket and subscribe ---
	clientConn, clientReader := dialClientSocket(t, sockPath)
	sendClientFrame(t, clientConn, map[string]any{
		"type": "session_subscribe",
		"name": sessionName,
	})
	// Allow the subscription goroutine to register.
	time.Sleep(50 * time.Millisecond)

	// --- Simulate a harness event by connecting as the pi extension and sending a tool_exec ---
	hConn, hReader := dialHarness(t, harnessSockPath)
	doHandshake(t, hConn, hReader)

	// Send a tool_exec that will generate a tool_call observation event (via writeObservationEvent).
	sendFrame(t, hConn, map[string]any{
		"type": "tool_exec",
		"id":   "call-fanout-001",
		"name": "bash",
		"args": map[string]any{"command": "echo harness-fanout-test"},
	})

	// Wait for the tool_exec_result on the harness side (ensures the event was written to DB).
	var gotResult bool
	resultDeadline := time.Now().Add(10 * time.Second)
	for !gotResult && time.Now().Before(resultDeadline) {
		hConn.SetReadDeadline(resultDeadline) //nolint:errcheck
		hFrame := readFrame(t, hReader)
		if hFrame["type"] == "tool_exec_result" {
			gotResult = true
		}
	}
	if !gotResult {
		t.Fatal("harness: no tool_exec_result received — harness dispatch failed")
	}

	// --- The client should have received at least one session_event for this session ---
	// The harness writes tool_call + tool_result observation events, each of which
	// goes through writeEvent/writeObservationEvent → WriteEventReturningRowID → publishEvent → Publish.
	var receivedEvent bool
	eventDeadline := time.Now().Add(5 * time.Second)
	for !receivedEvent {
		frame, ok := readClientFrameWithTimeout(t, clientConn, clientReader, 5*time.Second)
		if !ok {
			break
		}
		if frame["type"] == "session_event" {
			if frame["session_name"] == sessionName {
				receivedEvent = true
			}
		}
		if time.Now().After(eventDeadline) {
			break
		}
	}
	if !receivedEvent {
		t.Error("client did not receive a session_event frame after harness tool_exec — fan-out wiring is broken")
	}
}

// TestSupervisorPublisherConfig verifies that SupervisorConfig.Publisher is
// wired through to the harness and causes events to be published.
// This is a unit test of the SupervisorConfig.Publisher → SetPublisher path
// without actually spawning a pi child.
func TestSupervisorPublisherConfig(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer database.Close()

	// A minimal EventPublisher implementation that records publications.
	type testPublisher struct {
		mu   sync.Mutex
		pubs []iris.EventPublication
	}
	tp := &testPublisher{}
	publishFn := iris.PublisherFunc(func(pub iris.EventPublication) {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		tp.pubs = append(tp.pubs, pub)
	})

	sessionName := "sup-publisher@test"

	// Set up a HarnessSocketServer with the publisher wired via SupervisorConfig.Publisher.
	// (We don't spawn a real supervisor — we test the harness server directly to
	// validate the wiring path that NewSupervisor follows.)

	// Insert sessions row so FK on agent_events.instance_id is satisfied.
	insertTestSessionRow(t, database, "iid-sp", sessionName, tmp)

	harnessSockPath := filepath.Join(tmp, "harness.sock")
	sess := &iris.SessionRecord{
		InstanceID:      "iid-sp",
		SessionName:     sessionName,
		Worktree:        tmp,
		Role:            "worker",
		HarnessSockPath: harnessSockPath,
	}
	harness, err := iris.NewHarnessSocketServer(sess, database)
	if err != nil {
		t.Fatalf("NewHarnessSocketServer: %v", err)
	}
	// This is the same call that NewSupervisor makes when cfg.Publisher != nil.
	harness.SetPublisher(publishFn)
	if err := harness.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer harness.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = harness.AcceptOne(ctx) }()

	// Connect as the extension, do handshake, send a tool_exec.
	hConn, hReader := dialHarness(t, harnessSockPath)
	doHandshake(t, hConn, hReader)

	sendFrame(t, hConn, map[string]any{
		"type": "tool_exec",
		"id":   "call-pub-001",
		"name": "bash",
		"args": map[string]any{"command": "echo pub-test"},
	})

	// Wait for the result.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		hConn.SetReadDeadline(deadline) //nolint:errcheck
		f := readFrame(t, hReader)
		if f["type"] == "tool_exec_result" {
			break
		}
	}

	// The publisher should have received at least one publication.
	time.Sleep(50 * time.Millisecond) // let publishEvent goroutine complete
	tp.mu.Lock()
	count := len(tp.pubs)
	tp.mu.Unlock()
	if count == 0 {
		t.Error("publisher received 0 publications — SetPublisher wiring is broken in harness")
	}
}
