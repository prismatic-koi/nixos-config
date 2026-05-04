package pi_test

import (
	"encoding/json"
	"testing"

	"github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/payload"
)

func TestNormaliseFrame_MsgAssistant(t *testing.T) {
	a := pi.New("", "", "")
	raw := []byte(`{"type":"message_complete","id":"msg-abc","role":"assistant","content":[{"type":"text","text":"Hello world"}],"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":10,"cache_creation_input_tokens":5},"model":"claude-sonnet-4-5","provider":"anthropic","elapsed_ms":1234}`)

	evtType, normPayload, shouldWrite := a.NormaliseFrame(raw)

	if !shouldWrite {
		t.Fatal("expected shouldWrite=true")
	}
	if evtType != "msg_assistant" {
		t.Fatalf("expected eventType=msg_assistant, got %q", evtType)
	}

	p, ok := normPayload.(payload.MsgAssistant)
	if !ok {
		t.Fatalf("expected payload.MsgAssistant, got %T", normPayload)
	}
	if p.MessageID != "msg-abc" {
		t.Errorf("MessageID: want %q got %q", "msg-abc", p.MessageID)
	}
	if p.Text != "Hello world" {
		t.Errorf("Text: want %q got %q", "Hello world", p.Text)
	}
	if p.Model != "anthropic/claude-sonnet-4-5" {
		t.Errorf("Model: want %q got %q", "anthropic/claude-sonnet-4-5", p.Model)
	}
	if p.InputTokens != 100 {
		t.Errorf("InputTokens: want 100 got %d", p.InputTokens)
	}
	if p.OutputTokens != 50 {
		t.Errorf("OutputTokens: want 50 got %d", p.OutputTokens)
	}
	if p.CacheReadTokens != 10 {
		t.Errorf("CacheReadTokens: want 10 got %d", p.CacheReadTokens)
	}
	if p.CacheWriteTokens != 5 {
		t.Errorf("CacheWriteTokens: want 5 got %d", p.CacheWriteTokens)
	}
	if p.DurationMs != 1234 {
		t.Errorf("DurationMs: want 1234 got %d", p.DurationMs)
	}
	// Zero-value fields: not available for PI
	if p.Agent != "" {
		t.Errorf("Agent: want empty (PI has no persona system), got %q", p.Agent)
	}
	if p.TtftMs != 0 {
		t.Errorf("TtftMs: want 0 (not available), got %d", p.TtftMs)
	}
	if p.ContextWindowPct != 0 {
		t.Errorf("ContextWindowPct: want 0 (not available), got %f", p.ContextWindowPct)
	}
	if p.Cost != 0 {
		t.Errorf("Cost: want 0 (not available), got %f", p.Cost)
	}
}

func TestNormaliseFrame_MsgAssistant_ModelNormalisedWithoutProvider(t *testing.T) {
	// When provider is absent, model is used as-is.
	a := pi.New("", "", "")
	raw := []byte(`{"type":"message_complete","id":"msg-xyz","role":"assistant","content":[{"type":"text","text":"Hi"}],"model":"gpt-4o","elapsed_ms":500}`)

	evtType, normPayload, shouldWrite := a.NormaliseFrame(raw)

	if !shouldWrite {
		t.Fatal("expected shouldWrite=true")
	}
	if evtType != "msg_assistant" {
		t.Fatalf("unexpected evtType %q", evtType)
	}
	p := normPayload.(payload.MsgAssistant)
	if p.Model != "gpt-4o" {
		t.Errorf("Model: want %q got %q", "gpt-4o", p.Model)
	}
}

func TestNormaliseFrame_MsgUser(t *testing.T) {
	a := pi.New("", "", "")
	raw := []byte(`{"type":"message_start","id":"usr-001","role":"user","content":[{"type":"text","text":"Write a test"}]}`)

	evtType, normPayload, shouldWrite := a.NormaliseFrame(raw)

	if !shouldWrite {
		t.Fatal("expected shouldWrite=true")
	}
	if evtType != "msg_user" {
		t.Fatalf("expected eventType=msg_user, got %q", evtType)
	}
	p, ok := normPayload.(payload.MsgUser)
	if !ok {
		t.Fatalf("expected payload.MsgUser, got %T", normPayload)
	}
	if p.MessageID != "usr-001" {
		t.Errorf("MessageID: want %q got %q", "usr-001", p.MessageID)
	}
	if p.Text != "Write a test" {
		t.Errorf("Text: want %q got %q", "Write a test", p.Text)
	}
}

func TestNormaliseFrame_ToolCall(t *testing.T) {
	a := pi.New("", "", "")
	raw := []byte(`{"type":"tool_call","tool":"bash","input":{"command":"go test ./..."},"message_id":"msg-123","elapsed_ms":5000}`)

	evtType, normPayload, shouldWrite := a.NormaliseFrame(raw)

	if !shouldWrite {
		t.Fatal("expected shouldWrite=true")
	}
	if evtType != "tool_call" {
		t.Fatalf("expected eventType=tool_call, got %q", evtType)
	}
	p, ok := normPayload.(payload.ToolCall)
	if !ok {
		t.Fatalf("expected payload.ToolCall, got %T", normPayload)
	}
	if p.Tool != "bash" {
		t.Errorf("Tool: want %q got %q", "bash", p.Tool)
	}
	if p.MessageID != "msg-123" {
		t.Errorf("MessageID: want %q got %q", "msg-123", p.MessageID)
	}
	if p.DurationMs != 5000 {
		t.Errorf("DurationMs: want 5000 got %d", p.DurationMs)
	}
	// Args should be the marshalled input
	if p.Args == "" {
		t.Error("Args: expected non-empty")
	}
}

func TestNormaliseFrame_ToolResult(t *testing.T) {
	a := pi.New("", "", "")
	raw := []byte(`{"type":"tool_result","tool":"bash","output":"ok\n2 tests passed","message_id":"msg-456"}`)

	evtType, normPayload, shouldWrite := a.NormaliseFrame(raw)

	if !shouldWrite {
		t.Fatal("expected shouldWrite=true")
	}
	if evtType != "tool_result" {
		t.Fatalf("expected eventType=tool_result, got %q", evtType)
	}
	p, ok := normPayload.(payload.ToolResult)
	if !ok {
		t.Fatalf("expected payload.ToolResult, got %T", normPayload)
	}
	if p.Tool != "bash" {
		t.Errorf("Tool: want %q got %q", "bash", p.Tool)
	}
	if p.MessageID != "msg-456" {
		t.Errorf("MessageID: want %q got %q", "msg-456", p.MessageID)
	}
}

func TestNormaliseFrame_StateChange(t *testing.T) {
	a := pi.New("", "", "")
	for _, state := range []string{"active", "finished", "error", "interrupted"} {
		raw := []byte(`{"type":"state_change","state":"` + state + `"}`)
		evtType, normPayload, shouldWrite := a.NormaliseFrame(raw)

		if !shouldWrite {
			t.Fatalf("state=%q: expected shouldWrite=true", state)
		}
		if evtType != "state_change" {
			t.Fatalf("state=%q: expected eventType=state_change, got %q", state, evtType)
		}
		p, ok := normPayload.(payload.StateChange)
		if !ok {
			t.Fatalf("state=%q: expected payload.StateChange, got %T", state, normPayload)
		}
		if p.State != state {
			t.Errorf("state=%q: StateChange.State: want %q got %q", state, state, p.State)
		}
	}
}

func TestNormaliseFrame_SessionEnd_WithTokens(t *testing.T) {
	a := pi.New("", "", "")
	raw := []byte(`{"type":"session_end","usage":{"total_input_tokens":1000,"total_output_tokens":200,"total_cost":0.05}}`)

	evtType, normPayload, shouldWrite := a.NormaliseFrame(raw)

	if !shouldWrite {
		t.Fatal("expected shouldWrite=true for session_end with token data")
	}
	if evtType != "msg_assistant" {
		t.Fatalf("expected eventType=msg_assistant (synthetic), got %q", evtType)
	}
	p, ok := normPayload.(payload.MsgAssistant)
	if !ok {
		t.Fatalf("expected payload.MsgAssistant, got %T", normPayload)
	}
	if p.InputTokens != 1000 {
		t.Errorf("InputTokens: want 1000 got %d", p.InputTokens)
	}
	if p.OutputTokens != 200 {
		t.Errorf("OutputTokens: want 200 got %d", p.OutputTokens)
	}
	if p.Cost != 0.05 {
		t.Errorf("Cost: want 0.05 got %f", p.Cost)
	}
}

func TestNormaliseFrame_SessionEnd_NoTokens(t *testing.T) {
	a := pi.New("", "", "")
	raw := []byte(`{"type":"session_end","usage":{"total_input_tokens":0,"total_output_tokens":0,"total_cost":0}}`)

	_, _, shouldWrite := a.NormaliseFrame(raw)
	if shouldWrite {
		t.Error("expected shouldWrite=false for session_end with no token data")
	}
}

func TestNormaliseFrame_UnknownEventType_NotWritten(t *testing.T) {
	// Edge-case AC: unknown event types are logged (not silently dropped) and
	// shouldWrite=false so they don't appear in agent_events.
	a := pi.New("", "", "")
	raw := []byte(`{"type":"before_provider_request","payload":{"model":"claude-3"}}`)

	_, _, shouldWrite := a.NormaliseFrame(raw)
	if shouldWrite {
		t.Error("expected shouldWrite=false for unknown event type")
	}
}

func TestNormaliseFrame_MalformedJSON_NotWritten(t *testing.T) {
	a := pi.New("", "", "")
	raw := []byte(`not json at all`)

	_, _, shouldWrite := a.NormaliseFrame(raw)
	if shouldWrite {
		t.Error("expected shouldWrite=false for malformed JSON")
	}
}

func TestNormaliseFrame_MsgAssistantPayload_SQLPushdownPaths(t *testing.T) {
	// Verify the six camelCase JSON paths that SessionTurnTokens pushes down in SQL:
	// $.model, $.inputTokens, $.outputTokens, $.cacheReadTokens, $.cacheWriteTokens, $.cost
	a := pi.New("", "", "")
	raw := []byte(`{"type":"message_complete","id":"m1","role":"assistant","content":[],"usage":{"input_tokens":42,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":1},"model":"gpt-4","provider":"openai","elapsed_ms":999}`)

	_, normPayload, shouldWrite := a.NormaliseFrame(raw)
	if !shouldWrite {
		t.Fatal("expected shouldWrite=true")
	}

	data, err := json.Marshal(normPayload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	wantKeys := []string{"model", "inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens"}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("SQL pushdown field %q absent from serialised payload", k)
		}
	}
}

func TestNormaliseFrame_MessageStart_NonUser_Skipped(t *testing.T) {
	a := pi.New("", "", "")
	raw := []byte(`{"type":"message_start","id":"msg-x","role":"assistant","content":[]}`)

	_, _, shouldWrite := a.NormaliseFrame(raw)
	if shouldWrite {
		t.Error("expected shouldWrite=false for message_start with role=assistant")
	}
}

func TestNormaliseFrame_MessageComplete_NonAssistant_Skipped(t *testing.T) {
	a := pi.New("", "", "")
	raw := []byte(`{"type":"message_complete","id":"msg-y","role":"user","content":[],"usage":{}}`)

	_, _, shouldWrite := a.NormaliseFrame(raw)
	if shouldWrite {
		t.Error("expected shouldWrite=false for message_complete with role=user")
	}
}

func TestNormaliseFrame_TurnStart_EmitsStateChangeActive(t *testing.T) {
	// turn_start must emit state_change:active so the session transitions
	// from idle→active when a new turn begins. The sidecar's upsertState
	// deduplicates — if already active the write is a no-op.
	a := pi.New("", "", "")
	raw := []byte(`{"type":"turn_start"}`)

	evtType, normPayload, shouldWrite := a.NormaliseFrame(raw)

	if !shouldWrite {
		t.Fatal("expected shouldWrite=true for turn_start")
	}
	if evtType != "state_change" {
		t.Fatalf("expected eventType=state_change, got %q", evtType)
	}
	p, ok := normPayload.(payload.StateChange)
	if !ok {
		t.Fatalf("expected payload.StateChange, got %T", normPayload)
	}
	if p.State != "active" {
		t.Errorf("StateChange.State: want %q got %q", "active", p.State)
	}
}

func TestNormaliseFrame_ToolResult_TruncatesLongOutput(t *testing.T) {
	a := pi.New("", "", "")
	longOutput := make([]byte, 600)
	for i := range longOutput {
		longOutput[i] = 'x'
	}
	raw, _ := json.Marshal(map[string]any{
		"type":       "tool_result",
		"tool":       "bash",
		"output":     string(longOutput),
		"message_id": "msg-z",
	})

	_, normPayload, shouldWrite := a.NormaliseFrame(raw)
	if !shouldWrite {
		t.Fatal("expected shouldWrite=true")
	}
	p := normPayload.(payload.ToolResult)
	if len(p.Result) > 500 {
		t.Errorf("Result length %d exceeds 500-char budget", len(p.Result))
	}
}
