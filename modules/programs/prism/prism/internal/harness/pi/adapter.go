// Package pi implements the harness.Harness interface for the PI (pi-coding-agent)
// runtime. PI communicates via a TransportSocketPipe shape: the extension
// connects to a Unix socket bound by the sidecar and exchanges JSONL frames.
//
// # B5.TR — Translate payload strategy
//
// Under the Translate strategy (B5 §6), the PI adapter normalises PI's native
// JSONL event frames into pi-shaped payload structs before they are written
// to agent_events. Downstream consumers (cmd/checkin, cmd/stats, cmd/audit) work
// without harness-specific branches because every row in agent_events has the
// same camelCase JSON field layout regardless of whether the session was run
// under PI.
//
// The normalisation map is documented on NormaliseFrame below.
//
// Information loss is intentional and bounded by the existing "zero means not
// available" convention documented on payload.MsgAssistant. Fields that PI does
// not natively surface (e.g. ttftMs, contextWindowPct) are written as zero.
//
// agent_events.normalised_payload stays NULL for PI sessions. The column is
// reserved by C.1 for the Widen/Version strategies; Translate writes directly to
// payload and leaves normalised_payload unpopulated.
package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/payload"
)

// Compile-time assertion that *Adapter implements harness.Harness.
var _ harness.Harness = (*Adapter)(nil)

// Compile-time assertion that *Adapter implements harness.FrameNormaliser.
var _ harness.FrameNormaliser = (*Adapter)(nil)

// Compile-time assertion that *Adapter implements harness.StdinReceiver.
var _ harness.StdinReceiver = (*Adapter)(nil)

// Adapter implements harness.Harness for the PI runtime.
//
// PI is registered as TransportSocketPipe: the sidecar binds a Unix socket and
// the PI extension connects to it, exchanging JSONL frames. There is no HTTP
// server to dial. The stdio-shaped methods on this adapter (SetStdinPipe,
// DeliverInitialPrompt, DeliverPrompt, Subscribe, MapEvent, ExtractMessage,
// HealthCheck) exist only to satisfy the harness.Harness and related interfaces;
// they are not exercised by the socket-pipe path in production.
type Adapter struct {
	binaryPath string
	agentRole  string
	agentModel string

	mu        sync.Mutex
	stdinPipe io.WriteCloser // set after the process starts
}

// New returns a new Adapter for the PI harness.
//
// binaryPath is the path to the pi binary (e.g. /nix/store/…/bin/pi).
// agentRole and agentModel are forwarded unchanged from the harness registry
// Factory; PI does not natively use them but they are stored for completeness.
func New(binaryPath string, agentRole, agentModel string) *Adapter {
	return &Adapter{
		binaryPath: binaryPath,
		agentRole:  agentRole,
		agentModel: agentModel,
	}
}

// --- harness.Harness implementation ---

// ContainerCommand returns the command used to launch PI as the main process.
// For PI, this is just the binary path with no extra flags: PI reads its
// configuration from its config file, not from CLI flags.
func (a *Adapter) ContainerCommand() string {
	if a.binaryPath != "" {
		return a.binaryPath
	}
	return "pi"
}

// HealthCheck satisfies harness.Harness; not exercised by the socket-pipe path.
// For the socket-pipe shape, the sidecar's handlePipeFrame loop is the liveness
// signal; there is no HTTP endpoint to poll.
func (a *Adapter) HealthCheck(_ context.Context, _ int) error {
	return nil
}

// ConfigMountPath returns the XDG config path inside the container where PI
// expects its configuration directory. PI follows the standard XDG base dir:
// $HOME/.config/pi.
func (a *Adapter) ConfigMountPath() string {
	home, _ := os.UserHomeDir()
	return fmt.Sprintf("%s/.config/pi", home)
}

// DeliverInitialPrompt satisfies harness.Harness; not exercised by the
// socket-pipe path. For PI, the prompt is embedded in the agent configuration
// file rather than being written to stdin at startup.
func (a *Adapter) DeliverInitialPrompt(_ context.Context, prompt, _ string) error {
	a.mu.Lock()
	pipe := a.stdinPipe
	a.mu.Unlock()
	if pipe == nil {
		// Container mode or process not yet started — no stdin delivery.
		return nil
	}
	_, err := fmt.Fprintln(pipe, prompt)
	return err
}

// DeliverPrompt satisfies harness.Harness; not exercised by the socket-pipe
// path. Follow-up prompts are delivered via the Unix socket by the sidecar's
// handlePipeFrame loop rather than written to stdin.
func (a *Adapter) DeliverPrompt(_ context.Context, prompt string) error {
	a.mu.Lock()
	pipe := a.stdinPipe
	a.mu.Unlock()
	if pipe == nil {
		return fmt.Errorf("pi: DeliverPrompt: stdin pipe is not open")
	}
	_, err := fmt.Fprintln(pipe, prompt)
	return err
}

// SetStdinPipe satisfies harness.StdinReceiver; not exercised by the
// socket-pipe path. Stored here for potential stdio-fallback use.
func (a *Adapter) SetStdinPipe(pipe io.WriteCloser) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stdinPipe = pipe
}

// Subscribe satisfies harness.Harness; not exercised by the socket-pipe path.
// The sidecar's handlePipeFrame loop consumes PI frames directly from the Unix
// socket. Returns a closed channel.
func (a *Adapter) Subscribe(_ context.Context) (<-chan harness.HarnessEvent, error) {
	ch := make(chan harness.HarnessEvent)
	close(ch)
	return ch, nil
}

// MapEvent satisfies harness.Harness; not exercised by the socket-pipe path.
func (a *Adapter) MapEvent(_ harness.HarnessEvent) (harness.StateTransition, bool) {
	return harness.StateTransition{}, false
}

// ExtractMessage satisfies harness.Harness; not exercised by the socket-pipe path.
func (a *Adapter) ExtractMessage(_ harness.HarnessEvent) (harness.Message, bool) {
	return harness.Message{}, false
}

// CreateSession satisfies harness.Harness. It always returns "": the real PI
// session ID is populated by the sidecar's session_status handler directly
// into agent_status.harness_session_id via UpdateHarnessSessionID, bypassing
// the adapter. cmd/cleanup reads the ID back from the DB, not from this method.
func (a *Adapter) CreateSession(_ context.Context) (string, error) {
	return "", nil
}

// SessionID satisfies harness.Harness. Always returns "": the authoritative
// session ID lives in agent_status.harness_session_id (written by the sidecar
// directly to the DB; see sidecar.go session_status handler).
func (a *Adapter) SessionID() string {
	return ""
}

// ExtractEventType returns the "type" field from the raw JSON event payload.
// For PI's JSONL stream, the event type is embedded in each frame under the
// key "type".
func (a *Adapter) ExtractEventType(evt harness.HarnessEvent) string {
	if evt.Type != "" {
		return evt.Type
	}
	var frame struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(evt.Data, &frame); err == nil {
		return frame.Type
	}
	return ""
}

// ConfigEnvVar returns the environment variable name PI uses for config
// content injection. PI follows the convention PI_CONFIG_CONTENT.
func (a *Adapter) ConfigEnvVar() string {
	return "PI_CONFIG_CONTENT"
}

// RuntimeEnv returns additional environment variables needed by the PI process.
// PI_OFFLINE=1 suppresses telemetry and update-check network calls, matching
// the behaviour of the host shell alias that always sets this variable.
func (a *Adapter) RuntimeEnv() map[string]string {
	return map[string]string{
		"PI_OFFLINE": "1",
	}
}

// ValidateAgentRole reports whether the given role is supported by PI.
// PI has no native agent/persona system (RFC #606), so all roles are accepted —
// prism sets the role externally and PI runs without awareness of it.
func (a *Adapter) ValidateAgentRole(_ string) error {
	return nil
}

// EffectiveModel returns the model identifier for the given role. PI inherits
// the model from the spawn parameters; there is no per-role override in PI's
// config schema today.
func (a *Adapter) EffectiveModel(_ string) string {
	return a.agentModel
}

// --- FrameNormaliser implementation (B5.TR Translate strategy) ---

// piFrame is the minimal envelope parsed from every PI JSONL line.
// Only the fields needed for routing are declared here; per-type parsing
// happens in the switch arms of NormaliseFrame.
type piFrame struct {
	Type string `json:"type"`
}

// piMessageCompleteFrame represents PI's message_complete event, emitted when
// an LLM assistant turn finishes. Fields are snake_case (PI convention).
//
// Information loss acknowledged per B5 §6:
//   - ttftMs: PI does not expose time-to-first-token as a discrete field;
//     written as 0 (not available).
//   - contextWindowPct: requires the model's context window size, which is not
//     available from the event alone; written as 0 (not available).
//   - cost: PI may not report cost for all providers; written as 0 (not
//     available). The stats pipeline's local pricing table fallback engages.
//   - agent: PI has no built-in persona system (RFC #606); written as "".
type piMessageCompleteFrame struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

// piMessageStartFrame represents PI's message_start event, emitted when a new
// user turn begins. Fields are snake_case (PI convention).
type piMessageStartFrame struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// piToolCallFrame represents PI's tool_call event, emitted when the LLM
// requests a tool invocation. Fields are snake_case (PI convention).
type piToolCallFrame struct {
	Type      string          `json:"type"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	MessageID string          `json:"message_id"`
	ElapsedMs int64           `json:"elapsed_ms"`
}

// piToolResultFrame represents PI's tool_result event, emitted after a tool
// invocation completes. Fields are snake_case (PI convention).
type piToolResultFrame struct {
	Type      string `json:"type"`
	Tool      string `json:"tool"`
	Output    string `json:"output"`
	MessageID string `json:"message_id"`
}

// piStateChangeFrame represents PI's state_change event. The state values
// mirror agent.AgentState so that the ConsecutiveSidecarFailures SQL pushdown
// on $.state continues to work for PI sessions (B5 consumer-surface table).
type piStateChangeFrame struct {
	Type  string `json:"type"`
	State string `json:"state"`
}

// piSessionEndFrame represents PI's session_end event, which carries
// aggregated token-cost fields for the entire session. Under PI's model,
// some providers report costs at session end rather than per-message.
// When this frame is received, the accumulated totals are written as a
// synthetic msg_assistant event so that stats aggregations pick them up.
//
// This addresses the edge-case AC: "Token-cost fields that PI reports at
// session-end rather than per-message are correctly aggregated and written
// to the final msg_assistant event."
type piSessionEndFrame struct {
	Type  string `json:"type"`
	Usage struct {
		TotalInputTokens  int     `json:"total_input_tokens"`
		TotalOutputTokens int     `json:"total_output_tokens"`
		TotalCost         float64 `json:"total_cost"`
	} `json:"usage"`
}

// extractText concatenates all text parts from a content array.
func extractText(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	var parts []string
	for _, c := range content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "")
}

// marshalArgs returns the raw tool-call args JSON value, truncated to
// the 500-byte pi-budget. Post-#1783 the returned value is a
// json.RawMessage so it can be assigned directly to
// payload.ToolCall.Args. Truncation that would produce invalid JSON
// is avoided by re-wrapping over-budget input in a string — the
// resulting RawMessage is still valid JSON (a string literal), just
// not a structured object.
func marshalArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) <= 500 {
		return raw
	}
	// Over budget. Re-encode as a truncated JSON string so the
	// resulting RawMessage stays valid JSON. Consumers of
	// payload.ToolCall.Args treat string-typed args as a fallback
	// (narrative.ToolKeyArg's default branch echoes the raw value).
	truncated := string(raw[:500])
	b, err := json.Marshal(truncated)
	if err != nil {
		return nil
	}
	return b
}

// NormaliseFrame maps a raw PI JSONL frame to a canonical pi-shaped
// (eventType, payload, shouldWrite) tuple (implements FrameNormaliser).
//
// PI is registered as TransportSocketPipe; in production, frames are consumed
// by handlePipeFrame in sidecar.go. NormaliseFrame is retained for stdio
// fallback, tests, and replay tooling (e.g. piexport) that parse PI JSONL
// outside the socket-pipe path.
//
// Normalisation map:
//
//   - message_complete (role=assistant) → "msg_assistant" / payload.MsgAssistant
//   - message_start (role=user)         → "msg_user"      / payload.MsgUser
//   - tool_call                         → "tool_call"     / payload.ToolCall
//   - tool_result                       → "tool_result"   / payload.ToolResult
//   - state_change                      → "state_change"  / payload.StateChange
//   - session_end                       → "msg_assistant" / payload.MsgAssistant
//     (synthetic event carrying session-level token totals)
//   - all other types: logged at info level; shouldWrite = false
//
// The SQL pushdown invariants that Translate must preserve (B5 consumer table):
//   - $.messageId present on msg_assistant, msg_user, tool_call, tool_result
//   - $.state present on state_change with the same enum values
//   - $.model, $.inputTokens, $.outputTokens, $.cacheReadTokens,
//     $.cacheWriteTokens, $.cost present on msg_assistant (for SessionTurnTokens)
func (a *Adapter) NormaliseFrame(rawLine []byte) (eventType string, normPayload any, shouldWrite bool) {
	var envelope piFrame
	if err := json.Unmarshal(rawLine, &envelope); err != nil {
		log.Printf("pi: NormaliseFrame: parse envelope: %v (raw: %.200s)", err, rawLine)
		return "", nil, false
	}

	switch envelope.Type {
	case "message_complete":
		var f piMessageCompleteFrame
		if err := json.Unmarshal(rawLine, &f); err != nil {
			log.Printf("pi: NormaliseFrame: parse message_complete: %v", err)
			return "", nil, false
		}
		if f.Role != "assistant" {
			// message_complete for non-assistant roles — no direct pi analogue.
			log.Printf("pi: NormaliseFrame: message_complete role=%q — no equivalent prism event, skipping", f.Role)
			return "", nil, false
		}
		model := f.Model
		if f.Provider != "" && f.Model != "" {
			model = f.Provider + "/" + f.Model
		}
		p := payload.MsgAssistant{
			MessageID:        f.ID,
			Text:             extractText(f.Content),
			Agent:            "", // PI has no persona system (RFC #606)
			Model:            model,
			InputTokens:      f.Usage.InputTokens,
			OutputTokens:     f.Usage.OutputTokens,
			CacheReadTokens:  f.Usage.CacheReadInputTokens,
			CacheWriteTokens: f.Usage.CacheCreationInputTokens,
			DurationMs:       f.ElapsedMs,
			// TtftMs, ContextWindowPct, Cost: zero = not available (B5 §6)
		}
		return "msg_assistant", p, true

	case "message_start":
		var f piMessageStartFrame
		if err := json.Unmarshal(rawLine, &f); err != nil {
			log.Printf("pi: NormaliseFrame: parse message_start: %v", err)
			return "", nil, false
		}
		if f.Role != "user" {
			log.Printf("pi: NormaliseFrame: message_start role=%q — no equivalent prism event, skipping", f.Role)
			return "", nil, false
		}
		p := payload.MsgUser{
			MessageID: f.ID,
			Text:      extractText(f.Content),
		}
		return "msg_user", p, true

	case "tool_call":
		var f piToolCallFrame
		if err := json.Unmarshal(rawLine, &f); err != nil {
			log.Printf("pi: NormaliseFrame: parse tool_call: %v", err)
			return "", nil, false
		}
		// Post-#1783: payload.ToolCall renamed Tool→Name,
		// MessageID→ID, Args:string→Args:json.RawMessage. The
		// stdio adapter's input shape (snake_case JSONL) is
		// unchanged — the adapter just translates into the new
		// canonical struct names.
		//
		// Post-#1787: the stdio adapter's piToolCallFrame.MessageID
		// is PI's parent-assistant message id (the same field that
		// `msg_assistant` carries as `$.messageId`). It maps to the
		// new ParentMessageID field so the checkin secondary-query
		// pushdown can join this row back to its assistant turn.
		// We also keep it in ID for backward compatibility with any
		// stdio-path consumer that pairs on ID (the stdio frame
		// shape never had a distinct tool-call id).
		p := payload.ToolCall{
			Name:            f.Tool,
			Args:            marshalArgs(f.Input),
			ID:              f.MessageID,
			ParentMessageID: f.MessageID,
			DurationMs:      f.ElapsedMs,
		}
		return "tool_call", p, true

	case "tool_result":
		var f piToolResultFrame
		if err := json.Unmarshal(rawLine, &f); err != nil {
			log.Printf("pi: NormaliseFrame: parse tool_result: %v", err)
			return "", nil, false
		}
		output := f.Output
		truncated := false
		if len(output) > 500 {
			output = output[:500]
			truncated = true
		}
		// The stdio adapter has no error/success signal on its
		// input frame, so Success defaults to true. Consumers that
		// need to detect failures fall back to per-tool result
		// summarisation heuristics (narrative.ToolResultSummary).
		//
		// ParentMessageID mirrors the ToolCall mapping above
		// (#1787): the stdio frame's `message_id` is the parent
		// assistant turn id, which is what the secondary-query
		// pushdown joins on.
		p := payload.ToolResult{
			ID:              f.MessageID,
			ParentMessageID: f.MessageID,
			Success:         true,
			Output:          output,
			Truncated:       truncated,
		}
		return "tool_result", p, true

	case "state_change":
		var f piStateChangeFrame
		if err := json.Unmarshal(rawLine, &f); err != nil {
			log.Printf("pi: NormaliseFrame: parse state_change: %v", err)
			return "", nil, false
		}
		p := payload.StateChange{
			State: f.State,
		}
		return "state_change", p, true

	case "session_end":
		// PI may report aggregate token costs at session end rather than per-message.
		// Write a synthetic msg_assistant event so SessionTurnTokens picks them up.
		var f piSessionEndFrame
		if err := json.Unmarshal(rawLine, &f); err != nil {
			log.Printf("pi: NormaliseFrame: parse session_end: %v", err)
			return "", nil, false
		}
		if f.Usage.TotalInputTokens == 0 && f.Usage.TotalOutputTokens == 0 && f.Usage.TotalCost == 0 {
			// No useful token data — skip rather than writing a zero-cost event.
			log.Printf("pi: NormaliseFrame: session_end has no token usage, skipping")
			return "", nil, false
		}
		p := payload.MsgAssistant{
			InputTokens:  f.Usage.TotalInputTokens,
			OutputTokens: f.Usage.TotalOutputTokens,
			Cost:         f.Usage.TotalCost,
		}
		return "msg_assistant", p, true

	default:
		// PI event type with no direct mapping: log at info level and skip.
		// Logged (not silently dropped) per the edge-case AC.
		log.Printf("pi: NormaliseFrame: unknown event type %q — no equivalent prism event, skipping", envelope.Type)
		return "", nil, false
	}
}

// buildPICommand constructs the exec.Cmd for the PI harness binary.
// Exported so that tests can inspect the command without running it.
func buildPICommand(ctx context.Context, binaryPath string) *exec.Cmd {
	if binaryPath == "" {
		binaryPath = "pi"
	}
	cmd := exec.CommandContext(ctx, binaryPath)
	return cmd
}

// scanJSONL reads the JSONL stdout of a PI process and calls onFrame for each
// frame. It is extracted here so it can be tested independently of the sidecar.
func scanJSONL(r io.Reader, onFrame func([]byte)) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		onFrame(line)
	}
	return sc.Err()
}
