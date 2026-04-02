package payload_test

import (
	"encoding/json"
	"testing"

	"github.com/prismatic-koi/prism/internal/payload"
)

// roundtrip marshals v to JSON and unmarshals back into a new value of the
// same type, then compares the two for equality.
func roundtrip[T any](t *testing.T, v T) T {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestStateChange_Roundtrip(t *testing.T) {
	in := payload.StateChange{State: "active"}
	out := roundtrip(t, in)
	if out.State != in.State {
		t.Errorf("State: got %q, want %q", out.State, in.State)
	}
}

func TestMsgUser_Roundtrip(t *testing.T) {
	in := payload.MsgUser{
		MessageID: "msg-abc123",
		Text:      "fix the failing test",
		Agent:     "build",
		Model:     "github-copilot/claude-sonnet-4.6",
	}
	out := roundtrip(t, in)
	if out.MessageID != in.MessageID {
		t.Errorf("MessageID: got %q, want %q", out.MessageID, in.MessageID)
	}
	if out.Text != in.Text {
		t.Errorf("Text: got %q, want %q", out.Text, in.Text)
	}
	if out.Agent != in.Agent {
		t.Errorf("Agent: got %q, want %q", out.Agent, in.Agent)
	}
	if out.Model != in.Model {
		t.Errorf("Model: got %q, want %q", out.Model, in.Model)
	}
}

func TestMsgUser_EmptyAgentModel_Roundtrip(t *testing.T) {
	// AC-4: when agent/model are absent, fields are empty string and no error.
	in := payload.MsgUser{MessageID: "msg-xyz", Text: "hello"}
	out := roundtrip(t, in)
	if out.Agent != "" {
		t.Errorf("Agent: got %q, want empty string", out.Agent)
	}
	if out.Model != "" {
		t.Errorf("Model: got %q, want empty string", out.Model)
	}
}

func TestMsgAssistant_Roundtrip(t *testing.T) {
	in := payload.MsgAssistant{
		MessageID: "msg-def456",
		Text:      "Let me look at the test...",
		Agent:     "build",
		Model:     "github-copilot/claude-opus-4-5",
	}
	out := roundtrip(t, in)
	if out.MessageID != in.MessageID {
		t.Errorf("MessageID: got %q, want %q", out.MessageID, in.MessageID)
	}
	if out.Text != in.Text {
		t.Errorf("Text: got %q, want %q", out.Text, in.Text)
	}
	if out.Agent != in.Agent {
		t.Errorf("Agent: got %q, want %q", out.Agent, in.Agent)
	}
	if out.Model != in.Model {
		t.Errorf("Model: got %q, want %q", out.Model, in.Model)
	}
}

func TestToolCall_Roundtrip(t *testing.T) {
	in := payload.ToolCall{Tool: "bash", Args: `{"command":"go test ./..."}`, MessageID: "msg-1"}
	out := roundtrip(t, in)
	if out.Tool != in.Tool {
		t.Errorf("Tool: got %q, want %q", out.Tool, in.Tool)
	}
	if out.Args != in.Args {
		t.Errorf("Args: got %q, want %q", out.Args, in.Args)
	}
	if out.MessageID != in.MessageID {
		t.Errorf("MessageID: got %q, want %q", out.MessageID, in.MessageID)
	}
}

func TestToolResult_Roundtrip(t *testing.T) {
	in := payload.ToolResult{Tool: "bash", Result: "ok", MessageID: "msg-1"}
	out := roundtrip(t, in)
	if out.Tool != in.Tool {
		t.Errorf("Tool: got %q, want %q", out.Tool, in.Tool)
	}
	if out.Result != in.Result {
		t.Errorf("Result: got %q, want %q", out.Result, in.Result)
	}
	if out.MessageID != in.MessageID {
		t.Errorf("MessageID: got %q, want %q", out.MessageID, in.MessageID)
	}
}

func TestPermissionAsk_Roundtrip(t *testing.T) {
	in := payload.PermissionAsk{
		Tool:      "bash",
		Patterns:  []string{"rm -rf *", "curl *"},
		MessageID: "msg-1",
	}
	out := roundtrip(t, in)
	if out.Tool != in.Tool {
		t.Errorf("Tool: got %q, want %q", out.Tool, in.Tool)
	}
	if len(out.Patterns) != len(in.Patterns) {
		t.Fatalf("Patterns len: got %d, want %d", len(out.Patterns), len(in.Patterns))
	}
	for i, p := range in.Patterns {
		if out.Patterns[i] != p {
			t.Errorf("Patterns[%d]: got %q, want %q", i, out.Patterns[i], p)
		}
	}
	if out.MessageID != in.MessageID {
		t.Errorf("MessageID: got %q, want %q", out.MessageID, in.MessageID)
	}
}

func TestPermissionDenied_Roundtrip(t *testing.T) {
	in := payload.PermissionDenied{Tool: "bash", MessageID: "msg-1"}
	out := roundtrip(t, in)
	if out.Tool != in.Tool {
		t.Errorf("Tool: got %q, want %q", out.Tool, in.Tool)
	}
	if out.MessageID != in.MessageID {
		t.Errorf("MessageID: got %q, want %q", out.MessageID, in.MessageID)
	}
}

func TestThinking_Roundtrip(t *testing.T) {
	in := payload.Thinking{Text: "I should check the logs first", MessageID: "msg-1"}
	out := roundtrip(t, in)
	if out.Text != in.Text {
		t.Errorf("Text: got %q, want %q", out.Text, in.Text)
	}
	if out.MessageID != in.MessageID {
		t.Errorf("MessageID: got %q, want %q", out.MessageID, in.MessageID)
	}
}

func TestCompaction_Roundtrip(t *testing.T) {
	in := payload.Compaction{Note: "compaction started"}
	out := roundtrip(t, in)
	if out.Note != in.Note {
		t.Errorf("Note: got %q, want %q", out.Note, in.Note)
	}
}

func TestErrorEvent_Roundtrip(t *testing.T) {
	in := payload.ErrorEvent{Note: "session error"}
	out := roundtrip(t, in)
	if out.Note != in.Note {
		t.Errorf("Note: got %q, want %q", out.Note, in.Note)
	}
}

// TestJSONFieldNames verifies that the JSON field names match what the plugin
// emits (camelCase). A field name mismatch would produce zero values, not errors.
func TestJSONFieldNames(t *testing.T) {
	// Unmarshal a hand-crafted JSON blob that uses the exact field names the
	// plugin writes. If any field name is wrong, the field will be zero.
	raw := `{"messageId":"abc","text":"hello","agent":"build","model":"gh/claude"}`
	var mu payload.MsgUser
	if err := json.Unmarshal([]byte(raw), &mu); err != nil {
		t.Fatalf("unmarshal MsgUser: %v", err)
	}
	if mu.MessageID != "abc" {
		t.Errorf("MsgUser.MessageID: got %q, want \"abc\" (check json:\"messageId\" tag)", mu.MessageID)
	}
	if mu.Agent != "build" {
		t.Errorf("MsgUser.Agent: got %q, want \"build\" (check json:\"agent\" tag)", mu.Agent)
	}
	if mu.Model != "gh/claude" {
		t.Errorf("MsgUser.Model: got %q, want \"gh/claude\" (check json:\"model\" tag)", mu.Model)
	}

	rawTC := `{"tool":"bash","args":"go build","messageId":"abc"}`
	var tc payload.ToolCall
	if err := json.Unmarshal([]byte(rawTC), &tc); err != nil {
		t.Fatalf("unmarshal ToolCall: %v", err)
	}
	if tc.Tool != "bash" {
		t.Errorf("ToolCall.Tool: got %q, want \"bash\"", tc.Tool)
	}
	if tc.MessageID != "abc" {
		t.Errorf("ToolCall.MessageID: got %q, want \"abc\" (check json:\"messageId\" tag)", tc.MessageID)
	}
}
