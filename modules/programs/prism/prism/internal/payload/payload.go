// Package payload defines canonical typed structs for agent_events payload JSON.
//
// Every event type stored in the agent_events table has a corresponding struct
// here. The plugin (TypeScript) marshals event payloads to match these struct
// field names exactly. Go consumers unmarshal via these types so that a field
// mismatch shows up as a zero value rather than as silent output failure.
//
// JSON field names match the plugin's output verbatim (camelCase).
package payload

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
type MsgAssistant struct {
	MessageID string `json:"messageId"`
	Text      string `json:"text"`
	Agent     string `json:"agent"`
	Model     string `json:"model"`
}

// ToolCall is the payload for tool_call events.
type ToolCall struct {
	Tool      string `json:"tool"`
	Args      string `json:"args"`
	MessageID string `json:"messageId"`
}

// ToolResult is the payload for tool_result events.
type ToolResult struct {
	Tool      string `json:"tool"`
	Result    string `json:"result"`
	MessageID string `json:"messageId"`
}

// PermissionAsk is the payload for permission_ask events.
type PermissionAsk struct {
	Tool      string   `json:"tool"`
	Patterns  []string `json:"patterns"`
	MessageID string   `json:"messageId"`
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
