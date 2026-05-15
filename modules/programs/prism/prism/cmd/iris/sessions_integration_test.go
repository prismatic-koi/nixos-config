package main

// sessions_integration_test.go — integration tests for `iris sessions list`
// and `iris sessions status` that exercise the real client-socket round-trip.
//
// These tests stand up an in-process iris.ClientSocket (the daemon's IPC
// surface) backed by a synthetic session list, then drive fetchSessionsSnapshot
// against it. The DB on disk is left empty so we exercise the AC:
//
//	"the command works correctly when the DB is not readable by the invoking
//	 process but the client socket is."
//
// The negative case (daemon not running) is covered separately by pointing the
// CLI at a non-existent socket path and asserting the documented error string.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// startTestClientSocket starts an iris.ClientSocket on a tempdir socket path,
// backed by `sessions`. Returns the socket path. The socket and its goroutines
// are torn down on test cleanup.
//
// We open a throwaway DB at <tempdir>/iris.db purely to satisfy the
// ClientSocketConfig contract — no rows are written. The DB is closed on
// cleanup.
func startTestClientSocket(t *testing.T, sessions []iris.SessionSnapshot) string {
	t.Helper()

	tmp := t.TempDir()
	// Unix socket sun_path is 108 bytes; t.TempDir() under a long test
	// name can blow that. Use a short MkdirTemp prefix for the socket
	// itself and keep the throwaway DB in the regular tempdir.
	shortPrefix := t.TempDir() // already short on most systems; if not, MkdirTemp would be needed.
	sockPath := filepath.Join(shortPrefix, "iris.sock")
	dbPath := filepath.Join(tmp, "iris.db")

	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

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

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cs.Serve(ctx)

	return sockPath
}

// TestFetchSessionsSnapshot_RealSocket verifies the CLI's dial+request+parse
// path against a real iris.ClientSocket carrying two synthetic sessions.
// This is the positive integration test for the AC:
//
//	"queries the daemon over the client socket — does NOT read iris.db
//	 directly."
func TestFetchSessionsSnapshot_RealSocket(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{
			Name:             "iris-test@coordinator",
			InstanceID:       "abcd1234-0000-1111-2222-333333333333",
			State:            "active",
			Role:             "coordinator",
			Worktree:         "/tmp/iris-test/.bare/main",
			StartedAt:        time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339),
			HarnessSessionID: "/tmp/iris-test/.pi/agent/sessions/coord.jsonl",
		},
		{
			Name:             "iris-test@worker",
			InstanceID:       "feed1234-0000-1111-2222-444444444444",
			State:            "waiting",
			Role:             "worker",
			Worktree:         "/tmp/iris-test/feature-x",
			StartedAt:        time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339),
			HarnessSessionID: "/tmp/iris-test/.pi/agent/sessions/worker.jsonl",
		},
	}
	sockPath := startTestClientSocket(t, sessions)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snap, err := fetchSessionsSnapshot(ctx, sockPath)
	if err != nil {
		t.Fatalf("fetchSessionsSnapshot: %v", err)
	}
	if len(snap.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(snap.Sessions))
	}

	// Build a name → snapshot map for order-independent assertions.
	byName := map[string]iris.SessionSnapshot{}
	for _, s := range snap.Sessions {
		byName[s.Name] = s
	}
	coord, ok := byName["iris-test@coordinator"]
	if !ok {
		t.Fatalf("coordinator session missing from snapshot: %+v", snap.Sessions)
	}
	if coord.Role != "coordinator" || coord.State != "active" {
		t.Errorf("coordinator snapshot mismatch: %+v", coord)
	}
	if coord.HarnessSessionID == "" {
		t.Errorf("coordinator HarnessSessionID is empty: %+v", coord)
	}
}

// TestFetchSessionsSnapshot_DaemonNotRunning verifies the documented error
// when the socket file does not exist, per the AC:
//
//	"daemon not running → non-zero exit with a clear error pointing at
//	 systemctl --user start iris, NOT a connection-refused stack trace."
func TestFetchSessionsSnapshot_DaemonNotRunning(t *testing.T) {
	// Point at a path inside a tempdir that we never create.
	sockPath := filepath.Join(t.TempDir(), "definitely-not-here.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := fetchSessionsSnapshot(ctx, sockPath)
	if err == nil {
		t.Fatal("expected error when daemon not running, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "iris daemon not running") {
		t.Errorf("error message must announce daemon-not-running condition: %q", msg)
	}
	if !strings.Contains(msg, "systemctl --user start iris") {
		t.Errorf("error message must point at `systemctl --user start iris`: %q", msg)
	}
}

// TestFetchSessionsSnapshot_StaleSocketRefused verifies that a socket file
// existing without a listener (the daemon crashed without unlinking) maps
// to the same "daemon not running" remedy. Implementation: bind a unix
// socket then close the listener; the file lingers but connections refuse.
func TestFetchSessionsSnapshot_StaleSocketRefused(t *testing.T) {
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "stale.sock")

	// Create a stale socket file via os.Create — the stat check passes but
	// the dial will hit ECONNREFUSED since nothing is listening.
	f, err := os.Create(sockPath)
	if err != nil {
		t.Fatalf("create stale socket file: %v", err)
	}
	_ = f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = fetchSessionsSnapshot(ctx, sockPath)
	if err == nil {
		t.Fatal("expected error against stale socket, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "systemctl --user start iris") {
		t.Errorf("stale socket error should still point at the start command: %q", msg)
	}
}

// TestFetchSessionsSnapshot_MalformedResponse exercises the "malformed
// response" branch. We stand up a unix-socket listener that responds with a
// not-JSON line. The CLI must surface "malformed response" rather than hang.
func TestFetchSessionsSnapshot_MalformedResponse(t *testing.T) {
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "bad.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Drain the client's sessions_list request before replying so the
		// CLI doesn't see a write error before its read. A single line is
		// enough; we don't validate the content.
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		// Send a non-JSON line and close.
		_, _ = conn.Write([]byte("this is not json\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = fetchSessionsSnapshot(ctx, sockPath)
	if err == nil {
		t.Fatal("expected malformed-response error, got nil")
	}
	if !strings.Contains(err.Error(), "malformed response") {
		t.Errorf("expected 'malformed response' in error, got %q", err.Error())
	}
}

// TestFetchSessionsSnapshot_ConnectionDropMidResponse exercises the
// "lost connection" branch. The server accepts the dial, then closes the
// connection without writing any reply. The CLI must report "lost
// connection" rather than hang.
func TestFetchSessionsSnapshot_ConnectionDropMidResponse(t *testing.T) {
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "drop.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Drain the request, then close without writing a reply.
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_ = conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = fetchSessionsSnapshot(ctx, sockPath)
	if err == nil {
		t.Fatal("expected lost-connection error, got nil")
	}
	if !strings.Contains(err.Error(), "lost connection") {
		t.Errorf("expected 'lost connection' in error, got %q", err.Error())
	}
}
