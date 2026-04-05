// Package payload defines canonical typed structs for agent_events payload JSON.
//
// Every event type stored in the agent_events table has a corresponding struct
// here. The plugin (TypeScript) marshals event payloads to match these struct
// field names exactly. Go consumers unmarshal via these types so that a field
// mismatch shows up as a zero value rather than as silent output failure.
//
// JSON field names match the plugin's output verbatim (camelCase).
package payload

import "encoding/json"

// StateChange is the payload for state_change events.
type StateChange struct {
	State string `json:"state"`
}

// MsgUser is the payload for msg_user events.
// Agent and Model are populated from the message.updated event's info fields.
// Model is stored as "providerID/modelID" (e.g. "github-copilot/claude-sonnet-4.6").
type MsgUser struct {
	MessageID string `json:"messageId"`
	Text      string `json:"text"`
	Agent     string `json:"agent"`
	Model     string `json:"model"`
}

// MsgAssistant is the payload for msg_assistant events.
// Agent and Model are populated from the message.updated event's info fields.
// Model is stored as "providerID/modelID" (e.g. "github-copilot/claude-sonnet-4.6").
//
// Token usage fields (InputTokens, OutputTokens, CacheReadTokens,
// CacheWriteTokens) are populated when available from the message.updated
// event's token metadata. Zero values mean "not available" (pre-enrichment
// events or models that don't report token counts).
//
// DurationMs is the wall-clock time of the assistant turn in milliseconds,
// measured from time.created to time.completed on the message. Zero means
// "not available".
//
// ContextWindowPct is the context window utilization as a percentage (0-100),
// calculated as (inputTokens / contextWindowSize) * 100. Zero means
// "not available" (model context window size unknown or inputTokens absent).
type MsgAssistant struct {
	MessageID        string  `json:"messageId"`
	Text             string  `json:"text"`
	Agent            string  `json:"agent"`
	Model            string  `json:"model"`
	InputTokens      int     `json:"inputTokens,omitempty"`
	OutputTokens     int     `json:"outputTokens,omitempty"`
	CacheReadTokens  int     `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int     `json:"cacheWriteTokens,omitempty"`
	DurationMs       int64   `json:"durationMs,omitempty"`
	ContextWindowPct float64 `json:"contextWindowPct,omitempty"`
}

// ToolCall is the payload for tool_call events.
//
// DurationMs is the wall-clock time of the tool execution in milliseconds,
// measured from state.start to state.end on the tool part. Zero means
// "not available" (pre-enrichment events or tools that don't report timing).
type ToolCall struct {
	Tool       string `json:"tool"`
	Args       string `json:"args"`
	MessageID  string `json:"messageId"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

// ToolResult is the payload for tool_result events.
type ToolResult struct {
	Tool      string `json:"tool"`
	Result    string `json:"result"`
	MessageID string `json:"messageId"`
}

// PermissionAsk is the payload for permission_ask events.
//
// The tool field is written by the plugin as the permission-type string
// (e.g. "bash", "external_directory"). Older DB rows may have the raw tool
// call metadata object { "messageID": "...", "callID": "..." } in this field.
// PermissionToolName handles both cases transparently.
type PermissionAsk struct {
	Tool      PermissionToolName `json:"tool"`
	Patterns  []string           `json:"patterns"`
	MessageID string             `json:"messageId"`
}

// PermissionToolName is a string that can be unmarshalled from either a plain
// JSON string (current plugin output) or a JSON object (legacy DB rows that
// stored the raw tool-call metadata object instead of the permission-type
// string). When the value is an object, the field is set to the empty string
// so callers can apply their own fallback label.
type PermissionToolName string

// UnmarshalJSON implements json.Unmarshaler.
func (p *PermissionToolName) UnmarshalJSON(data []byte) error {
	// encoding/json always passes at least one byte (minimum valid JSON token).
	// Happy path: plain string value (current plugin output).
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*p = PermissionToolName(s)
		return nil
	}
	// Legacy path: JSON object — discard and leave as empty string so that
	// callers can substitute a fallback rather than failing entirely.
	if data[0] == '{' {
		*p = ""
		return nil
	}
	// null → empty string; any other token type → empty string (best-effort).
	*p = ""
	return nil
}

// PermissionDenied is the payload for permission_denied events.
type PermissionDenied struct {
	Tool      string `json:"tool"`
	MessageID string `json:"messageId"`
}

// Thinking is the payload for thinking events.
type Thinking struct {
	Text      string `json:"text"`
	MessageID string `json:"messageId"`
}

// Compaction is the payload for compaction events.
type Compaction struct {
	Note string `json:"note"`
}

// ErrorEvent is the payload for error events.
type ErrorEvent struct {
	Note string `json:"note"`
}
