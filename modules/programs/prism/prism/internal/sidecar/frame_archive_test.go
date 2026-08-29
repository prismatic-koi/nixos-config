package sidecar

// Integration tests for the harness frame archive.
//
// These tests piggyback on the socket-pipe test harness defined in
// sidecar_socketpipe_test.go: a fake PI extension dials the sidecar, performs
// the hello/hello_ack handshake, exchanges some frames, and shuts down. The
// assertions here are about the harness_frames table — every byte that
// crossed the socket should be persisted with the right direction and type.

import (
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// waitForHarnessFrameCount blocks (with a deadline) until the harness_frames
// row count for sessionName reaches at least want, or fails the test.
func waitForHarnessFrameCount(t *testing.T, d *db.DB, sessionName string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		n, err := d.CountHarnessFrames(sessionName)
		if err != nil {
			t.Fatalf("CountHarnessFrames: %v", err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("harness_frames count = %d after %s, want >= %d", n, timeout, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSocketPipe_ArchivesHandshakeFrames verifies that the hello and
// hello_ack frames are persisted via the frame archive.
func TestSocketPipe_ArchivesHandshakeFrames(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// After the handshake at least 2 frames (hello in, hello_ack out) must
	// be in the archive. Wait briefly because writes happen on a background
	// goroutine for the outbound path.
	waitForHarnessFrameCount(t, sc.cfg.DB, sc.cfg.SessionName, 2, 2*time.Second)

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	frames, err := sc.cfg.DB.QueryHarnessFrames(sc.cfg.SessionName, "", nil, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames: %v", err)
	}
	// We expect at minimum: in/hello, out/hello_ack, in/session_shutdown.
	// Some test environments may write more frames (e.g. an internal
	// state_change) — assert that the canonical set is present rather than
	// hard-coding the total count.
	type key struct{ dir, typ string }
	seen := map[key]bool{}
	for _, f := range frames {
		seen[key{f.Direction, f.Type}] = true
	}
	want := []key{
		{"in", "hello"},
		{"out", "hello_ack"},
		{"in", "session_shutdown"},
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("expected archived frame %+v; saw %+v", k, seen)
		}
	}
}

// TestSocketPipe_ArchivesEventFrames verifies that tool_call/tool_result/
// msg_assistant frames sent by the fake extension show up in harness_frames
// in chronological order with the correct direction.
func TestSocketPipe_ArchivesEventFrames(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	sendJSON(t, conn, map[string]any{"type": "tool_call", "tool_id": "bash", "input": "ls"})
	sendJSON(t, conn, map[string]any{"type": "tool_result", "tool_id": "bash", "output": "ok"})
	sendJSON(t, conn, map[string]any{"type": "msg_assistant", "content": "done"})

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	in, err := sc.cfg.DB.QueryHarnessFrames(sc.cfg.SessionName, "in", nil, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames in: %v", err)
	}
	wantTypes := []string{"hello", "tool_call", "tool_result", "msg_assistant", "session_shutdown"}
	gotTypes := []string{}
	for _, f := range in {
		gotTypes = append(gotTypes, f.Type)
	}
	if strings.Join(gotTypes, ",") != strings.Join(wantTypes, ",") {
		t.Errorf("inbound types = %v; want %v", gotTypes, wantTypes)
	}

	// Each payload must be the raw JSONL bytes (no trailing newline,
	// parseable as JSON). Spot-check tool_call.
	for _, f := range in {
		if f.Type == "tool_call" {
			if !strings.Contains(f.Payload, `"tool_id":"bash"`) {
				t.Errorf("tool_call payload = %q; expected tool_id field", f.Payload)
			}
			if strings.HasSuffix(f.Payload, "\n") {
				t.Errorf("payload should not have trailing newline: %q", f.Payload)
			}
		}
	}
}

// TestSocketPipe_ArchivesOutboundDeliverPrompt verifies that frames sent via
// DeliverPrompt are archived with direction=out.
func TestSocketPipe_ArchivesOutboundDeliverPrompt(t *testing.T) {
	sockPath := shortSockPath(t)
	sc := newSocketPipeSidecar(t, sockPath)
	wait := runSocketPipeSidecar(sc)

	conn, _ := dialAndHandshake(t, sockPath)

	// Trigger an outbound prompt frame.
	if !sc.DeliverPrompt("hello world", "nextTurn") {
		t.Fatal("DeliverPrompt returned false; expected true")
	}

	// Read it on the fake extension side so we know it's been written.
	got := readJSON(t, conn)
	if got["type"] != "prompt" {
		t.Errorf("read type = %v, want prompt", got["type"])
	}

	// The archive write happens in the writer goroutine before conn.Write,
	// so by now the row must exist. Allow a tiny grace window for the DB
	// commit to land.
	waitForHarnessFrameCount(t, sc.cfg.DB, sc.cfg.SessionName, 3, 2*time.Second)

	sendJSON(t, conn, map[string]any{"type": "session_shutdown"})
	conn.Close()
	if err := wait(); err != nil {
		t.Errorf("runStartupSocketPipe returned error: %v", err)
	}

	out, err := sc.cfg.DB.QueryHarnessFrames(sc.cfg.SessionName, "out", []string{"prompt"}, "")
	if err != nil {
		t.Fatalf("QueryHarnessFrames: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("outbound prompt frames = %d, want 1", len(out))
	}
	if !strings.Contains(out[0].Payload, `"text":"hello world"`) {
		t.Errorf("prompt payload = %q; expected text field", out[0].Payload)
	}
}
