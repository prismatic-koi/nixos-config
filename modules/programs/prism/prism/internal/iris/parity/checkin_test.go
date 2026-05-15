package parity_test

// checkin_test.go — §10.3 checklist item: "Checkin (read conversation
// history from a session)".
//
// D-10 AC (functional, checkin):
//
//   A test invokes the iris equivalent of `prism checkin <session>` and
//   asserts the returned conversation history matches the assistant turns
//   and tool calls that pi emitted during the session, sourced from the
//   iris DB's narrative view.
//
// The iris checkin path reads from the same agent_events / session_status
// surface that prism checkin reads from — they share the DB schema
// (§10.4). For the parity contract we:
//
//   - drive a fake extension that emits a representative sequence of
//     observation events (msg_assistant, tool_call, tool_result) and a
//     session_status frame;
//   - read back via the DB's AllSessionEvents (the same query the iris
//     narrative-view code paths use) and assert each emitted event is
//     visible with the expected payload;
//   - also assert that the post-PR-#1657 ordering invariant holds:
//     session_status's harness_session_id is persisted before the
//     subscriber would see the event row in the same instant.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

func TestParityCheckin_ConversationHistory(t *testing.T) {
	iso := iristest.NewIsolated(t)
	fs := newFakeSession(t, iso, fakeSessionOptions{Role: "worker"})

	// Emit a session_status first so harness_session_id is persisted (the
	// D-10 watch-out about PR #1657 ordering applies — tests that poll for
	// harness_session_id immediately after asserting events must use the
	// post-fix ordering. Mirror that here so the assertion below is sound).
	const harnessSessionID = "pi-checkin-test-ULID-001"
	if err := writeJSONLine(fs.ExtConn, map[string]any{
		"type":       "session_status",
		"session_id": harnessSessionID,
		"phase":      "active",
	}); err != nil {
		t.Fatalf("write session_status: %v", err)
	}
	// Wait for the harness_session_id to be persisted via the DB-event-row
	// pattern recommended in the D-10 watch-outs.
	gotHID := pollForHarnessSessionID(t, iso.DB, fs.InstanceID, 3*time.Second)
	if gotHID != harnessSessionID {
		t.Errorf("harness_session_id = %q, want %q", gotHID, harnessSessionID)
	}

	// Emit a small conversation: assistant message + tool_call + tool_result.
	frames := []map[string]any{
		{
			"type":    "msg_assistant",
			"turn_id": "turn-checkin-001",
			"content": "iris-parity-checkin-assistant-text",
		},
		{
			"type":    "tool_call",
			"id":      "call-checkin-001",
			"name":    "bash",
			"args":    map[string]any{"command": "echo hello"},
		},
		{
			"type":    "tool_result",
			"id":      "call-checkin-001",
			"success": true,
			"output":  "hello\n",
		},
	}
	for _, f := range frames {
		if err := writeJSONLine(fs.ExtConn, f); err != nil {
			t.Fatalf("write %v: %v", f["type"], err)
		}
	}

	// Read back via the DB. AllSessionEvents is the same query the
	// narrative-view checkin paths use to assemble the history.
	deadline := time.Now().Add(3 * time.Second)
	var events []eventRow
	for time.Now().Before(deadline) {
		es, err := iso.DB.AllSessionEvents(fs.SessionName)
		if err != nil {
			t.Fatalf("AllSessionEvents: %v", err)
		}
		events = events[:0]
		seen := map[string]bool{}
		for _, e := range es {
			events = append(events, eventRow{Type: e.Type, Payload: e.Payload})
			seen[e.Type] = true
		}
		if seen["session_status"] && seen["msg_assistant"] && seen["tool_call"] && seen["tool_result"] {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Verify each emitted frame is round-tripped.
	wantPayloads := map[string]string{
		"msg_assistant": "iris-parity-checkin-assistant-text",
		"tool_call":     "call-checkin-001",
		"tool_result":   `"id":"call-checkin-001"`,
	}
	for typ, substr := range wantPayloads {
		var found bool
		for _, e := range events {
			if e.Type == typ && containsString(e.Payload, substr) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no %s event with payload substring %q in narrative history; got events=%v", typ, substr, summariseEvents(events))
		}
	}

	// The session_status payload must round-trip the session_id field so a
	// downstream checkin renderer can show the pi session ID. (PR #1657
	// ordering means by the time we observed the harness_session_id above,
	// the session_status agent_events row is already written.)
	var sessionStatusFound bool
	for _, e := range events {
		if e.Type != "session_status" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(e.Payload), &m); err != nil {
			continue
		}
		if m["session_id"] == harnessSessionID {
			sessionStatusFound = true
			break
		}
	}
	if !sessionStatusFound {
		t.Errorf("no session_status event with session_id=%q", harnessSessionID)
	}
}

// eventRow is a tiny shim around (type, payload) for assertion code.
type eventRow struct {
	Type    string
	Payload string
}

func containsString(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func summariseEvents(events []eventRow) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}
