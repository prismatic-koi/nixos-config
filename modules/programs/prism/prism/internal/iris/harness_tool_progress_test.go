package iris_test

// harness_tool_progress_test.go — iris-side coverage for the #1761
// mid-tool heartbeat. The iris HarnessSocketServer must treat
// `tool_progress` as an inbound liveness frame: read it off the wire
// without crashing AND without writing it to agent_events (the narrative
// renderer's default branch would otherwise surface it as a stream of
// "tool_progress" lines between tool_call and tool_result).

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// TestToolProgress_NotWrittenToEvents drives a tool_progress observation
// frame at iris's harness socket and confirms it is not persisted to
// agent_events. Control frames (state_change) are sent before and after
// so the test has a robust before/after signal — without those, the
// negative assertion would be racing the server goroutine's drain.
func TestToolProgress_NotWrittenToEvents(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer database.Close()

	const instanceID = "test-tool-progress-001"
	const sessionName = "test@tool-progress"
	insertTestSessionRow(t, database, instanceID, sessionName, tmp)

	sockPath := filepath.Join(tmp, "harness.sock")
	sess := &iris.SessionRecord{
		InstanceID:      instanceID,
		SessionName:     sessionName,
		Worktree:        tmp,
		Role:            "worker",
		HarnessSockPath: sockPath,
	}
	srv, err := iris.NewHarnessSocketServer(sess, database)
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

	// Bracket the heartbeats with state_change frames as before/after
	// markers — they are guaranteed to be persisted (state_change is a
	// well-known observation type), so we can wait for the "after" marker
	// to know the server has drained.
	sendFrame(t, conn, map[string]any{"type": "state_change", "state": "active"})
	for i := 0; i < 5; i++ {
		sendFrame(t, conn, map[string]any{
			"type": "tool_progress",
			"id":   "call-1",
			"name": "bash",
		})
	}
	sendFrame(t, conn, map[string]any{"type": "state_change", "state": "waiting"})

	// Wait until both state_change events are visible in the DB — that
	// guarantees the tool_progress frames in between have also been
	// processed by the dispatch loop.
	deadline := time.Now().Add(2 * time.Second)
	var activeSeen, waitingSeen bool
	for time.Now().Before(deadline) {
		all, err := database.AllSessionEvents(sessionName)
		if err != nil {
			t.Fatalf("AllSessionEvents: %v", err)
		}
		activeSeen = false
		waitingSeen = false
		for _, e := range all {
			if e.Type == "state_change" {
				// Crude but sufficient: payload contains "active" / "waiting".
				if strings.Contains(string(e.Payload), "active") && !activeSeen {
					activeSeen = true
				} else if strings.Contains(string(e.Payload), "waiting") {
					waitingSeen = true
				}
			}
		}
		if activeSeen && waitingSeen {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !activeSeen || !waitingSeen {
		t.Fatalf("control state_change events not flushed (active=%v, waiting=%v) — test setup is broken", activeSeen, waitingSeen)
	}

	// Now assert no tool_progress rows landed in agent_events.
	all, err := database.AllSessionEvents(sessionName)
	if err != nil {
		t.Fatalf("AllSessionEvents: %v", err)
	}
	for _, e := range all {
		if e.Type == "tool_progress" {
			t.Errorf("tool_progress frame leaked into agent_events (event=%+v) — must be invisible to downstream consumers per #1761", e)
		}
	}
}
