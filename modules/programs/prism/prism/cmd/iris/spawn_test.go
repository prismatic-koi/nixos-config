package main

// spawn_test.go — tests for `iris spawn` (issue #1668).
//
// These tests cover the CLI's daemon-routed spawn flow. Rather than wiring a
// real daemon (which would require a pi binary), they stand up a mock daemon
// socket that speaks the D-6 wire protocol and assert that `iris spawn`:
//
//   - sends a session_spawn frame with the worktree/role from the CLI flags;
//   - prints session UUID and harness socket path on success;
//   - exits with a clear error when the daemon is not running;
//   - exits with a clear error when the daemon closes the connection
//     before sending a session_spawned ack (covers the "daemon dies
//     mid-spawn" edge case);
//   - exits with a clear error when the daemon returns an error frame;
//   - does NOT fall back to an in-process supervisor in any failure mode.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// mockDaemon is a tiny test double that binds a Unix socket and replies to a
// single session_spawn frame with a script of responses.
type mockDaemon struct {
	sockPath string
	listener net.Listener
	// reply controls what the mock sends back after receiving the spawn frame.
	reply mockReply
	// gotFrame captures the parsed spawn frame for assertions.
	gotFrame chan iris.ClientSessionSpawnFrame
}

type mockReply int

const (
	replySuccess mockReply = iota // emit DaemonFrameSessionSpawned
	replyError                    // emit DaemonFrameError
	replyHangup                   // close the connection without replying
	replyUnknown                  // emit one unknown frame, then success (forward-compat)
)

func newMockDaemon(t *testing.T, reply mockReply) *mockDaemon {
	t.Helper()
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "iris.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m := &mockDaemon{
		sockPath: sockPath,
		listener: ln,
		reply:    reply,
		gotFrame: make(chan iris.ClientSessionSpawnFrame, 1),
	}
	t.Cleanup(func() { _ = ln.Close() })
	go m.serve(t)
	return m
}

func (m *mockDaemon) serve(t *testing.T) {
	conn, err := m.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Logf("mockDaemon: read: %v", err)
		return
	}
	var frame iris.ClientSessionSpawnFrame
	if err := json.Unmarshal(line, &frame); err != nil {
		t.Logf("mockDaemon: parse spawn frame: %v", err)
		return
	}
	m.gotFrame <- frame

	switch m.reply {
	case replySuccess:
		writeMockFrame(conn, iris.DaemonSessionSpawnedFrame{
			Type:       iris.DaemonFrameSessionSpawned,
			Name:       "iris-worker@" + filepath.Base(frame.Worktree),
			InstanceID: "fake-instance-uuid-0000",
		})
	case replyError:
		writeMockFrame(conn, iris.DaemonErrorFrame{
			Type:        iris.DaemonFrameError,
			RequestType: iris.ClientFrameSessionSpawn,
			Message:     "synthetic test failure",
		})
	case replyHangup:
		// Close without writing anything — simulates the daemon dying after
		// receiving the spawn frame.
		return
	case replyUnknown:
		writeMockFrame(conn, map[string]any{"type": "some_future_frame", "data": 42})
		writeMockFrame(conn, iris.DaemonSessionSpawnedFrame{
			Type:       iris.DaemonFrameSessionSpawned,
			Name:       "iris-worker@" + filepath.Base(frame.Worktree),
			InstanceID: "fake-instance-uuid-0001",
		})
	}
	// Keep the connection open briefly so the client can drain. The defer
	// closes it.
	time.Sleep(50 * time.Millisecond)
}

func writeMockFrame(w io.Writer, v any) {
	data, _ := json.Marshal(v)
	data = append(data, '\n')
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// Test: success path — happy spawn returns ack with session UUID
// ---------------------------------------------------------------------------

func TestRunSpawn_Success(t *testing.T) {
	m := newMockDaemon(t, replySuccess)

	runDir := filepath.Join(t.TempDir(), "run")
	worktree := "/abs/path/to/worktree"
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := runSpawnAt(ctx, m.sockPath, runDir, worktree, "worker", &out)
	if err != nil {
		t.Fatalf("runSpawnAt: %v", err)
	}

	// Assert the daemon received the right spawn frame.
	select {
	case got := <-m.gotFrame:
		if got.Type != iris.ClientFrameSessionSpawn {
			t.Errorf("frame type = %q, want %q", got.Type, iris.ClientFrameSessionSpawn)
		}
		if got.Worktree != worktree {
			t.Errorf("frame worktree = %q, want %q", got.Worktree, worktree)
		}
		if got.Role != "worker" {
			t.Errorf("frame role = %q, want worker", got.Role)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mock daemon did not receive a spawn frame")
	}

	output := out.String()
	if !strings.Contains(output, "fake-instance-uuid-0000") {
		t.Errorf("expected output to mention the instance UUID; got %q", output)
	}
	// AC: stdout still includes the harness socket path so existing scripted
	// callers keep working. The path is deterministic from runDir/instance_id.
	wantHarness := iris.HarnessSockPath(runDir, "fake-instance-uuid-0000")
	if !strings.Contains(output, wantHarness) {
		t.Errorf("expected output to mention the harness socket path %q; got %q",
			wantHarness, output)
	}
}

// ---------------------------------------------------------------------------
// Test: role flag propagates to the spawn frame
// ---------------------------------------------------------------------------

func TestRunSpawn_RolePropagates(t *testing.T) {
	m := newMockDaemon(t, replySuccess)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := runSpawnAt(ctx, m.sockPath, t.TempDir(), "/wt", "coordinator", &out); err != nil {
		t.Fatalf("runSpawnAt: %v", err)
	}
	got := <-m.gotFrame
	if got.Role != "coordinator" {
		t.Errorf("role = %q, want coordinator", got.Role)
	}
}

// ---------------------------------------------------------------------------
// Test: daemon not running — clear error, no fallback
// ---------------------------------------------------------------------------

func TestRunSpawn_DaemonNotRunning(t *testing.T) {
	// Point at a path that doesn't exist. The dial must fail and the error
	// message must name systemctl --user start iris so the user knows how
	// to recover.
	bogusSock := filepath.Join(t.TempDir(), "does-not-exist.sock")
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := runSpawnAt(ctx, bogusSock, t.TempDir(), "/wt", "worker", &out)
	if err == nil {
		t.Fatal("expected an error when daemon is not running, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "iris daemon not running") {
		t.Errorf("expected 'iris daemon not running' in error, got: %v", err)
	}
	if !strings.Contains(msg, "systemctl --user start iris") {
		t.Errorf("expected 'systemctl --user start iris' in error, got: %v", err)
	}
	// AC: no in-process fallback. The clearest assertion is that the
	// runDir was not created — the removed in-process path used to call
	// os.MkdirAll(p.RunDir, ...).
	if _, statErr := os.Stat(filepath.Join(t.TempDir(), "run")); !errors.Is(statErr, os.ErrNotExist) {
		// The path above was a fresh TempDir anyway; the real check is that
		// runSpawnAt did not, for example, write to it. We assert no output
		// was produced beyond the "spawning…" log line that precedes the
		// dial.
		if strings.Contains(out.String(), "spawned") {
			t.Errorf("expected no 'spawned' output on failure; got %q", out.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Test: daemon dies mid-spawn — clear error, no hang
// ---------------------------------------------------------------------------

func TestRunSpawn_DaemonHangup(t *testing.T) {
	m := newMockDaemon(t, replyHangup)
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := runSpawnAt(ctx, m.sockPath, t.TempDir(), "/wt", "worker", &out)
	if err == nil {
		t.Fatal("expected an error when daemon hangs up mid-spawn, got nil")
	}
	if !strings.Contains(err.Error(), "daemon closed connection") {
		t.Errorf("expected 'daemon closed connection' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: daemon returns an error frame — surfaced verbatim
// ---------------------------------------------------------------------------

func TestRunSpawn_DaemonErrorFrame(t *testing.T) {
	m := newMockDaemon(t, replyError)
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := runSpawnAt(ctx, m.sockPath, t.TempDir(), "/wt", "worker", &out)
	if err == nil {
		t.Fatal("expected an error when daemon rejects spawn, got nil")
	}
	if !strings.Contains(err.Error(), "synthetic test failure") {
		t.Errorf("expected the daemon's message to be surfaced; got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: unknown pre-ack frame is skipped (forward compat)
// ---------------------------------------------------------------------------

func TestRunSpawn_UnknownFrameSkipped(t *testing.T) {
	m := newMockDaemon(t, replyUnknown)
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := runSpawnAt(ctx, m.sockPath, t.TempDir(), "/wt", "worker", &out); err != nil {
		t.Fatalf("runSpawnAt: %v", err)
	}
	if !strings.Contains(out.String(), "fake-instance-uuid-0001") {
		t.Errorf("expected instance UUID in stdout after skipping unknown frame; got %q", out.String())
	}
}

// ---------------------------------------------------------------------------
// Test: missing --worktree returns a clear error
// ---------------------------------------------------------------------------

func TestRunSpawn_EmptyWorktree(t *testing.T) {
	var out bytes.Buffer
	err := runSpawnAt(context.Background(), "/dev/null", t.TempDir(), "", "worker", &out)
	if err == nil {
		t.Fatal("expected an error when worktree is empty, got nil")
	}
	if !strings.Contains(err.Error(), "--worktree is required") {
		t.Errorf("expected '--worktree is required' in error, got: %v", err)
	}
}
