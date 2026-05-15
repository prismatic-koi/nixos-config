package iris

// protocol.go — wire-protocol frame types for the iris harness socket.
//
// The iris harness socket extends the existing pi-wire-protocol.md with four
// new frame types to support the registerTool() override mechanism described
// in daemon-mode-design.md §3.3.2. These frames flow between the prism
// extension (running inside pi) and the iris daemon.
//
// All frames are JSON-line encoded (one JSON object per '\n'-terminated line),
// following the framing rules in pi-wire-protocol.md §3.
//
// Existing frames (hello, hello_ack, tool_call, tool_result, state_change,
// msg_assistant, turn_start, turn_end, etc.) are consumed by the harness
// socket handler without modification. Only the four new frames defined here
// are handled by the iris tool dispatcher.

// ToolExecFrame is sent by the pi extension to the iris daemon when an
// overridden tool's execute() callback is invoked.
//
//	{type: "tool_exec", id: <toolCallID>, name: <toolName>, args: <argsObject>}
//
// id must be the pi-supplied tool call ID (used for response correlation).
// Parallel tool calls may be in flight simultaneously on a single connection;
// the id field is the only correlation key.
type ToolExecFrame struct {
	Type string         `json:"type"` // "tool_exec"
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// ToolExecUpdateFrame is sent by the iris daemon to the pi extension to stream
// partial tool output during long-running tool calls (especially bash). Zero
// or more update frames may precede the terminal ToolExecResultFrame.
//
//	{type: "tool_exec_update", id: <toolCallID>, content: <partialOutput>}
type ToolExecUpdateFrame struct {
	Type    string `json:"type"`    // "tool_exec_update"
	ID      string `json:"id"`
	Content string `json:"content"`
}

// ToolExecResultFrame is sent by the iris daemon to the pi extension when
// a tool call completes (or fails). This frame terminates the tool call;
// no further update frames for the same id will be sent.
//
//	{type: "tool_exec_result", id: <toolCallID>, success: <bool>,
//	 isError: <bool>, output: <string>, details?: <object>}
//
// success is true when the subprocess exited with code 0 and produced no
// error. isError mirrors pi's convention for tool results — true when the
// result should be surfaced as a tool error to the LLM. output contains
// combined stdout+stderr. details is optional additional structured data.
type ToolExecResultFrame struct {
	Type    string         `json:"type"`           // "tool_exec_result"
	ID      string         `json:"id"`
	Success bool           `json:"success"`
	IsError bool           `json:"is_error"`
	Output  string         `json:"output"`
	Details map[string]any `json:"details,omitempty"`
}

// ToolAbortFrame is sent by the pi extension to the iris daemon when the
// pi AbortSignal fires during an in-flight tool call. The daemon kills the
// associated subprocess and returns a ToolExecResultFrame with
// success=false, isError=true, output="aborted".
//
//	{type: "tool_abort", id: <toolCallID>}
type ToolAbortFrame struct {
	Type string `json:"type"` // "tool_abort"
	ID   string `json:"id"`
}

// FrameType constants for the four new harness socket frame types.
const (
	FrameTypeToolExec       = "tool_exec"
	FrameTypeToolExecUpdate = "tool_exec_update"
	FrameTypeToolExecResult = "tool_exec_result"
	FrameTypeToolAbort      = "tool_abort"
)

// HelloFrame is the first frame sent by the pi extension on connection.
// Existing definition from pi-wire-protocol.md §4.1 — reproduced here for
// the harness socket handler.
type HelloFrame struct {
	Type            string `json:"type"`             // "hello"
	ProtocolVersion int    `json:"protocol_version"`
	Harness         string `json:"harness"`
	HarnessVersion  string `json:"harness_version"`
}

// HelloAckFrame is the daemon's response to HelloFrame.
// Existing definition from pi-wire-protocol.md §4.2 — reproduced here.
type HelloAckFrame struct {
	Type            string `json:"type"`              // "hello_ack"
	ProtocolVersion int    `json:"protocol_version"`
	SessionName     string `json:"session_name"`
	SessionRole     string `json:"session_role"`
	IsolationMode   string `json:"isolation_mode"`
	InstanceID      string `json:"instance_id"`
}

// GenericFrame is used for frame type dispatch — we decode the minimal
// envelope first, then decode the full frame based on the type.
type GenericFrame struct {
	Type string `json:"type"`
}

// ProtocolVersion is the wire protocol version iris speaks. Must match the
// extension's PROTOCOL_VERSION constant (currently 2, bumped in #1434).
const ProtocolVersion = 2
