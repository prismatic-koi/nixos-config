package iris_test

// client_socket_kill_test.go — integration tests for the session_kill frame
// (issue #1674). These exercise the client-socket wire path that returns
// session_killed / error frames to clients, including the no-such-session
// and already-terminal idempotent paths.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// newKillTestClientSocket builds a ClientSocket whose KillSession callback
// records every invocation and returns a configurable state/error pair.
// This isolates the wire path (frame parsing + ack frame) from the daemon
// state machine; full end-to-end killing is covered by the Supervisor.Kill
// tests in supervisor_kill_test.go.
type killRecord struct {
	name    string
	timeout time.Duration
}

func newKillTestClientSocket(t *testing.T, sessions []iris.SessionSnapshot, killState string, killErr error) (*iris.ClientSocket, *[]killRecord) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	sockPath := filepath.Join(tmp, "iris.sock")
	records := &[]killRecord{}
	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot {
			return sessions
		},
		KillSession: func(_ context.Context, name string, timeout time.Duration) (string, error) {
			*records = append(*records, killRecord{name: name, timeout: timeout})
			return killState, killErr
		},
	})
	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	go cs.Serve(ctx)

	return cs, records
}

// TestSessionKill_HappyPath verifies that a session_kill frame for a live
// session triggers KillSession, returns a session_killed ack carrying the
// terminal state, and forwards the 5-second timeout.
func TestSessionKill_HappyPath(t *testing.T) {
	cs, records := newKillTestClientSocket(t,
		[]iris.SessionSnapshot{{Name: "alive@test", InstanceID: "iid-1", State: "active"}},
		"finished", nil,
	)

	conn, r := dialClientSocket(t, cs.SockPath())
	sendClientFrame(t, conn, map[string]any{
		"type": "session_kill",
		"name": "alive@test",
	})
	frame, ok := readClientFrameWithTimeout(t, conn, r, 5*time.Second)
	if !ok {
		t.Fatal("no ack frame received")
	}
	if frame["type"] != "session_killed" {
		t.Fatalf("type = %v, want session_killed (frame=%v)", frame["type"], frame)
	}
	if frame["name"] != "alive@test" {
		t.Errorf("name = %v, want alive@test", frame["name"])
	}
	if frame["state"] != "finished" {
		t.Errorf("state = %v, want finished", frame["state"])
	}

	// KillSession invoked exactly once with the canonical 5s timeout.
	if len(*records) != 1 {
		t.Fatalf("KillSession called %d times, want 1", len(*records))
	}
	rec := (*records)[0]
	if rec.name != "alive@test" {
		t.Errorf("recorded name = %q", rec.name)
	}
	if rec.timeout != 5*time.Second {
		t.Errorf("recorded timeout = %s, want 5s (the canonical session_kill grace period)", rec.timeout)
	}
}

// TestSessionKill_NoSuchSession verifies that killing a session that is not
// in the active set produces an error frame (request_type=session_kill)
// without invoking KillSession. The connection stays open so the client can
// continue using it.
func TestSessionKill_NoSuchSession(t *testing.T) {
	cs, records := newKillTestClientSocket(t,
		[]iris.SessionSnapshot{}, // no sessions
		"finished", nil,
	)

	conn, r := dialClientSocket(t, cs.SockPath())
	sendClientFrame(t, conn, map[string]any{
		"type": "session_kill",
		"name": "missing@test",
	})
	frame, ok := readClientFrameWithTimeout(t, conn, r, 5*time.Second)
	if !ok {
		t.Fatal("no error frame received")
	}
	if frame["type"] != "error" {
		t.Fatalf("type = %v, want error (frame=%v)", frame["type"], frame)
	}
	if frame["request_type"] != "session_kill" {
		t.Errorf("request_type = %v, want session_kill", frame["request_type"])
	}
	if len(*records) != 0 {
		t.Errorf("KillSession should not be invoked on missing session, got %d records", len(*records))
	}

	// Connection should still be open — send a ping and verify pong arrives.
	sendClientFrame(t, conn, map[string]any{"type": "ping"})
	pong, ok := readClientFrameWithTimeout(t, conn, r, 5*time.Second)
	if !ok || pong["type"] != "pong" {
		t.Errorf("ping after error did not return pong (got %v, ok=%v)", pong, ok)
	}
}

// TestSessionKill_AlreadyTerminal_Idempotent verifies that calling
// session_kill on a session whose state is already terminal returns a
// session_killed ack with state="already_terminal" — the daemon-side
// killFn maps that case before invoking the supervisor.
func TestSessionKill_AlreadyTerminal_Idempotent(t *testing.T) {
	cs, records := newKillTestClientSocket(t,
		[]iris.SessionSnapshot{{Name: "done@test", InstanceID: "iid-d", State: "finished"}},
		"already_terminal", nil,
	)

	conn, r := dialClientSocket(t, cs.SockPath())
	sendClientFrame(t, conn, map[string]any{
		"type": "session_kill",
		"name": "done@test",
	})
	frame, ok := readClientFrameWithTimeout(t, conn, r, 5*time.Second)
	if !ok {
		t.Fatal("no ack frame received")
	}
	if frame["type"] != "session_killed" {
		t.Fatalf("type = %v, want session_killed (frame=%v)", frame["type"], frame)
	}
	if frame["state"] != "already_terminal" {
		t.Errorf("state = %v, want already_terminal", frame["state"])
	}
	if len(*records) != 1 {
		t.Errorf("KillSession should still be called for idempotency check, got %d", len(*records))
	}
}

// TestSessionKill_NoKillFunctionWired verifies that a daemon constructed
// without a KillSession callback rejects session_kill with a clear error
// rather than silently succeeding or panicking.
func TestSessionKill_NoKillFunctionWired(t *testing.T) {
	cs, _ := newTestClientSocket(t,
		[]iris.SessionSnapshot{{Name: "alive@test", InstanceID: "iid-1", State: "active"}},
	)
	// newTestClientSocket does not wire KillSession.

	conn, r := dialClientSocket(t, cs.SockPath())
	sendClientFrame(t, conn, map[string]any{
		"type": "session_kill",
		"name": "alive@test",
	})
	frame, ok := readClientFrameWithTimeout(t, conn, r, 5*time.Second)
	if !ok {
		t.Fatal("no ack frame received")
	}
	if frame["type"] != "error" {
		t.Fatalf("type = %v, want error (frame=%v)", frame["type"], frame)
	}
}

// TestSessionKill_InvalidFrame verifies that a session_kill frame missing
// the required "name" field returns an error frame.
func TestSessionKill_InvalidFrame(t *testing.T) {
	cs, _ := newKillTestClientSocket(t,
		[]iris.SessionSnapshot{},
		"finished", nil,
	)

	conn, r := dialClientSocket(t, cs.SockPath())
	sendClientFrame(t, conn, map[string]any{
		"type": "session_kill",
		// name omitted
	})
	frame, ok := readClientFrameWithTimeout(t, conn, r, 5*time.Second)
	if !ok {
		t.Fatal("no ack frame received")
	}
	if frame["type"] != "error" {
		t.Errorf("type = %v, want error", frame["type"])
	}
}
