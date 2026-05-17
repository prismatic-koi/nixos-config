package tui

// renderer.go — visitor-style event renderer for the iris TUI's
// conversation pane (issue #1767, child 2 of the bubbletea-native iris
// TUI design — docs/iris-tui-design.md).
//
// # Why a visitor / dispatch table
//
// Before this file the right-pane renderer was a single call to
// narrative.RenderEvent followed by inline per-event-type styling in
// styleEventLine(). Every new event type required touching both the
// rendering and the styling site, and there was no clean seam for
// child PR 3 (streaming msg_assistant deltas) or child PR 4 (rich
// tool-call cards) to plug into.
//
// The eventRenderer wraps a map[string]eventHandler keyed on the
// agent_events.type column value. dispatch() looks up the handler and
// falls back to a debug-visible default for unknown types — unknown
// types never panic the TUI; they render as a single muted "[HH:MM:SS]
// <type>" line so an operator can see something arrived without
// crashing the renderer. session_status is mapped to a no-op handler
// per the design doc renderer table ("Suppressed in conversation;
// visible only in sidebar's per-session row").
//
// # Why we still use narrative.NarrativeLine as the slot type
//
// The Model already stores rendered events as []narrative.NarrativeLine
// — RowID for de-dupe, MessageID for tool_call ↔ tool_result pairing,
// EventType for downstream styling. Keeping that slot type means the
// dispatcher can replace narrative.RenderEvent in handleDaemonFrame
// without disturbing the dedupe/pairing logic. Per-handler functions
// in this file are free to produce richer output (multi-line blocks
// for extension_error, prominent visual rows for tool_call) while
// still flowing through the same buffer.
//
// # Streaming readiness (child PR 3)
//
// renderMsgAssistant currently produces a complete two-line block
// (header + body) on every msg_assistant arrival — child 2 is
// explicitly "no streaming yet, render complete rows when they
// arrive". Child PR 3 will swap this handler for a streaming-aware
// variant that coalesces deltas by MessageID; the rest of the
// pipeline (dispatch, styling, the model buffer) does not change.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/iris/narrative"
	"github.com/prismatic-koi/prism/internal/payload"
)

// Event type constants used by the dispatcher. Mirrored from the
// agent_events.type values written by writeObservationEvent in
// internal/iris/harness_socket.go and the visual-mapping table in
// docs/iris-tui-design.md. Defined here (rather than imported from
// internal/iris) because there is no canonical home for these strings
// in the iris package today; this file is the single point of truth
// for "which event types does the TUI's conversation pane know about".
const (
	evTypeMsgAssistant   = "msg_assistant"
	evTypeMsgUser        = "msg_user"
	evTypeToolCall       = "tool_call"
	evTypeToolResult     = "tool_result"
	evTypeStateChange    = "state_change"
	evTypeExtensionError = "extension_error"
	evTypeSessionStatus  = "session_status"
	evTypePermissionAsk  = "permission_ask"
	evTypePermDenied     = "permission_denied"
	evTypeThinking       = "thinking"
	evTypeTurnStart      = "turn_start"
	evTypeTurnEnd        = "turn_end"

	// evTypeSessionEscalated is the agent_events.type value written by
	// ClientSocket.writeSessionEscalatedEvent (internal/iris/client_socket.go)
	// when a worker calls `iris escalate` / `prism escalate`. The row
	// lands on the WORKER session's event stream (not the coordinator's),
	// so it surfaces in the coordinator-events overlay rather than the
	// coordinator's own conversation pane. The renderer still styles
	// the row prominently when a worker's stream is being viewed — the
	// styling for the line is independent of the cross-session overlay.
	evTypeSessionEscalated = "session.escalated"

	// evTypeMergeQueueNotification is a SYNTHETIC EventType label, NOT a
	// real agent_events.type value. The merge-queue watcher delivers
	// notifications as prompt text (internal/mergequeue/watcher.go); the
	// receiving coordinator's harness persists them as `msg_user` rows.
	// The TUI re-labels those rows after dispatch (see
	// handleDaemonFrame) when their text matches
	// isMergeQueueNotificationText, so styleEventLine can apply the
	// distinct merge-queue treatment without mis-styling unrelated
	// msg_user rows.
	evTypeMergeQueueNotification = "merge_queue_notification"

	// evTypeExtensionErrorBody is a synthetic EventType label attached to
	// the second line of an extension_error block (the error-message
	// body). The visual styler uses this label to render the body with
	// the same prominent red treatment as the header line. Mirrors the
	// narrative package's "_body" suffix convention for msg_assistant /
	// msg_user multi-line events.
	evTypeExtensionErrorBody = "extension_error_body"
)

// eventHandler is the per-event-type rendering function signature. A
// handler receives the agent_events row ID and the verbatim payload
// JSON, and returns zero or more NarrativeLines. Returning nil is
// explicit "suppress this event from the conversation pane" — used by
// session_status and the turn_start/turn_end framing events.
type eventHandler func(rowID int64, payloadJSON string) []narrative.NarrativeLine

// eventFallback is the signature for the unknown-event-type renderer.
// It receives the eventType as an explicit argument (handlers only
// know about their own type) so the fallback can include it in the
// rendered line.
type eventFallback func(rowID int64, eventType, payloadJSON string) []narrative.NarrativeLine

// eventRenderer is the visitor dispatch table. Lookup is by event-type
// string; misses fall through to fallback (which produces a
// debug-visible line so unknown types never silently disappear).
type eventRenderer struct {
	handlers map[string]eventHandler
	fallback eventFallback
}

// newEventRenderer builds the default dispatch table for the
// conversation pane. The handler set is fixed at construction time —
// child PR 3 (streaming) and child PR 4 (tool-call cards) can either
// swap a specific handler or wrap the renderer; this file keeps the
// dispatch site free of inline per-type logic.
func newEventRenderer() *eventRenderer {
	r := &eventRenderer{handlers: map[string]eventHandler{}}
	r.handlers[evTypeMsgAssistant] = renderMsgAssistant
	r.handlers[evTypeMsgUser] = renderMsgUser
	r.handlers[evTypeToolCall] = renderToolCall
	r.handlers[evTypeToolResult] = renderToolResult
	r.handlers[evTypeStateChange] = renderStateChange
	r.handlers[evTypeExtensionError] = renderExtensionError
	r.handlers[evTypePermissionAsk] = renderPermissionAsk
	r.handlers[evTypePermDenied] = renderPermissionDenied
	r.handlers[evTypeThinking] = renderThinking
	// Suppressed: session_status (sidebar only — design doc renderer
	// table), turn_start / turn_end (implicit visual separators, no
	// dedicated row).
	r.handlers[evTypeSessionStatus] = renderSuppressed
	r.handlers[evTypeTurnStart] = renderSuppressed
	r.handlers[evTypeTurnEnd] = renderSuppressed
	r.handlers[evTypeSessionEscalated] = renderSessionEscalated
	r.fallback = renderFallback
	return r
}

// renderSessionEscalated renders the session.escalated bus event as a
// single prominent row — the row appears on the ESCALATING worker's
// event stream (the writer is ClientSocket.writeSessionEscalatedEvent
// in internal/iris/client_socket.go, which Publish()es on the worker's
// SessionName). The handler is deliberately compact: a glyph + the
// target coordinator + a truncated prompt preview. handleDaemonFrame
// pairs this with a parallel append into Model.coordinatorEvents so the
// coordinator-events overlay can list the same event regardless of
// which session is currently focused.
//
// On payload parse failure we still render a header row so the
// operator sees that an escalation arrived — a silent drop on JSON
// shape change has cost us debug-hunts before (cf. #1764).
func renderSessionEscalated(rowID int64, payloadJSON string) []narrative.NarrativeLine {
	var p struct {
		Source     string `json:"source"`
		Target     string `json:"target"`
		Prompt     string `json:"prompt"`
		DeliveryID string `json:"delivery_id"`
	}
	_ = json.Unmarshal([]byte(payloadJSON), &p)

	preview := strings.TrimSpace(p.Prompt)
	if preview == "" {
		preview = "(no prompt body)"
	}
	// Compress to the first non-empty line; the conversation-pane row
	// is one line and a multi-line escalation prompt would render as a
	// concatenated mess otherwise.
	for _, line := range strings.Split(preview, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			preview = line
			break
		}
	}

	var text string
	switch {
	case p.Target != "":
		text = fmt.Sprintf("[%s] \u26a0 escalated to %s: %s", ts(), p.Target, preview)
	default:
		// Zero-coordinator branch (writeSessionEscalatedEvent allows
		// target="" — see client_socket.go: "zero candidates without To
		// → transition worker to escalated"). Render without a target
		// rather than printing an empty arrow.
		text = fmt.Sprintf("[%s] \u26a0 escalated (no coordinator): %s", ts(), preview)
	}
	return []narrative.NarrativeLine{
		{Text: text, EventType: evTypeSessionEscalated, RowID: rowID},
	}
}

// dispatch routes one event triple to the appropriate handler. Unknown
// event types go through the fallback. Returns nil for suppressed
// events so the caller treats them as no-ops (no buffer growth, no
// dedupe entry).
func (r *eventRenderer) dispatch(rowID int64, eventType, payloadJSON string) []narrative.NarrativeLine {
	if h, ok := r.handlers[eventType]; ok {
		return h(rowID, payloadJSON)
	}
	return r.fallback(rowID, eventType, payloadJSON)
}

// ts returns the current wall-clock "HH:MM:SS" string. Centralised so
// every handler uses the same format and a future change (e.g. honour
// agent_events.created_at when the daemon exposes it on the wire) lands
// in one place. Mirrors narrative.RenderEvent's time.Now() convention —
// the daemon frame does not currently expose created_at, and the TUI
// has nothing to do with the value other than render it, so wall-clock
// at arrival is the most useful display.
func ts() string {
	return time.Now().Format("15:04:05")
}

// renderMsgAssistant renders a complete assistant turn as a two-line
// block: a header carrying "[HH:MM:SS] assistant  [agent · model]" and
// a body carrying the message text. No streaming/coalescing — child
// PR 3 (#1768) handles deltas.
//
// On payload parse failure we still render *something* (the raw JSON
// as the body) so the user can see the event arrived; silent drop on
// parse failure has cost us bug-hunts before (cf. #1764).
func renderMsgAssistant(rowID int64, payloadJSON string) []narrative.NarrativeLine {
	var p payload.MsgAssistant
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		p.Text = payloadJSON
	}
	text := p.Text
	if text == "" {
		text = "(no text)"
	}
	label := narrative.TurnLabel(p.Agent, p.Model)
	var header string
	if label != "" {
		header = fmt.Sprintf("[%s] assistant  [%s]", ts(), label)
	} else {
		header = fmt.Sprintf("[%s] assistant", ts())
	}
	return []narrative.NarrativeLine{
		{Text: header, EventType: evTypeMsgAssistant, RowID: rowID, MessageID: p.MessageID},
		{Text: text, EventType: evTypeMsgAssistant + "_body", RowID: rowID},
	}
}

// renderMsgUser mirrors renderMsgAssistant for the user turn. Kept here
// (rather than delegated to the narrative package) so the visitor is
// the single source of truth for what the conversation pane renders.
func renderMsgUser(rowID int64, payloadJSON string) []narrative.NarrativeLine {
	var p payload.MsgUser
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		p.Text = payloadJSON
	}
	text := p.Text
	if text == "" {
		text = "(no text)"
	}
	label := narrative.TurnLabel(p.Agent, p.Model)
	var header string
	if label != "" {
		header = fmt.Sprintf("[%s] ▶ user  [%s]", ts(), label)
	} else {
		header = fmt.Sprintf("[%s] ▶ user", ts())
	}
	return []narrative.NarrativeLine{
		{Text: header, EventType: evTypeMsgUser, RowID: rowID, MessageID: p.MessageID},
		{Text: text, EventType: evTypeMsgUser + "_body", RowID: rowID},
	}
}

// renderToolCall produces a one-line "card" of the form
//
//	→ <tool>: <key-arg>
//
// using narrative.ToolKeyArg to extract the primary argument (command
// for bash, file path for read/edit/write, pattern for grep, etc.).
// The leading "→" plus indent makes a tool-call visually distinct from
// surrounding assistant text on a single row — the design doc renderer
// table's "one-line card" requirement. Multi-line/rich tool-call cards
// (with args, result preview, expand/collapse) are child PR 4's scope.
func renderToolCall(rowID int64, payloadJSON string) []narrative.NarrativeLine {
	var p payload.ToolCall
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return []narrative.NarrativeLine{
			{
				Text:      fmt.Sprintf("[%s] → tool_call (parse error)", ts()),
				EventType: evTypeToolCall,
				RowID:     rowID,
			},
		}
	}
	keyArg := narrative.ToolKeyArg(p.Tool, p.Args)
	var text string
	if keyArg != "" {
		text = fmt.Sprintf("  → %s: %s", p.Tool, keyArg)
	} else {
		text = fmt.Sprintf("  → %s", p.Tool)
	}
	return []narrative.NarrativeLine{
		{Text: text, EventType: evTypeToolCall, RowID: rowID, MessageID: p.MessageID},
	}
}

// renderToolResult renders the indented one-line summary that follows a
// tool_call. The model layer pairs this line with its matching
// tool_call (by MessageID) so the rendered conversation reads as
//
//	→ bash: echo hello ✓ hello
//
// rather than as two unrelated rows. When no matching tool_call has
// been seen yet (replay race, frame ordering), the result line is
// emitted standalone with a leading indent so the visual still reads
// as "this is a result, not a top-level statement".
func renderToolResult(rowID int64, payloadJSON string) []narrative.NarrativeLine {
	var p payload.ToolResult
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return nil
	}
	summary := narrative.ToolResultSummary(p.Tool, p.Result)
	return []narrative.NarrativeLine{
		{
			Text:      fmt.Sprintf("    ↳ result: %s", summary),
			EventType: evTypeToolResult,
			RowID:     rowID,
			MessageID: p.MessageID,
		},
	}
}

// renderStateChange renders a single dim line of the form
//
//	[HH:MM:SS] ● <state>
//
// matching the design-doc renderer table's "Dim status line: ● <state>
// with timestamp" description. Styling is applied by styleEventLine
// based on the EventType label.
func renderStateChange(rowID int64, payloadJSON string) []narrative.NarrativeLine {
	var p payload.StateChange
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		p.State = payloadJSON
	}
	state := p.State
	if state == "" {
		state = "(unknown)"
	}
	return []narrative.NarrativeLine{
		{Text: fmt.Sprintf("[%s] ● %s", ts(), state), EventType: evTypeStateChange, RowID: rowID},
	}
}

// renderExtensionError renders the "prominent block" required by the
// design-doc renderer table — two lines, both styled red+bold:
//
//	[HH:MM:SS] ⛔ extension error: <event> at <extensionPath>
//	  <error message>
//
// Two lines (rather than one) because extension errors are fatal-class
// (issue #1757): the supervisor terminates the session and writes a
// session_end row with reason="extension_error". The operator needs the
// error message immediately visible without scrolling/expanding, and
// the path/event-name context to triage. styleEventLine maps both the
// header and the "_body" suffix label to the same prominent red
// rendering so the block reads as a single unit.
//
// The payload shape mirrors iris.ExtensionErrorFrame:
//
//	{"extensionPath": "...", "event": "...", "error": "..."}
//
// Persistence path: writeObservationEvent in harness_socket.go falls
// through to writing the raw RPC frame when the type does not match
// one of the known cases (line 436). If a future PR threads
// extension_error through that path, this handler renders it; until
// then this is a forward-compatible no-op in production but is
// exercised by tests.
func renderExtensionError(rowID int64, payloadJSON string) []narrative.NarrativeLine {
	// Decode opportunistically — fall back to a generic header that
	// still flags this as an extension error if parsing fails.
	var frame struct {
		ExtensionPath string `json:"extensionPath"`
		Event         string `json:"event"`
		Error         string `json:"error"`
	}
	_ = json.Unmarshal([]byte(payloadJSON), &frame)

	var header string
	switch {
	case frame.Event != "" && frame.ExtensionPath != "":
		header = fmt.Sprintf("[%s] ⛔ extension error: %s at %s",
			ts(), frame.Event, frame.ExtensionPath)
	case frame.Event != "":
		header = fmt.Sprintf("[%s] ⛔ extension error: %s", ts(), frame.Event)
	default:
		header = fmt.Sprintf("[%s] ⛔ extension error", ts())
	}

	body := strings.TrimSpace(frame.Error)
	if body == "" {
		// Fall back to the raw payload so the operator can at least
		// see something if the frame's shape changes upstream.
		body = strings.TrimSpace(payloadJSON)
		if body == "" {
			body = "(no error message)"
		}
	}

	return []narrative.NarrativeLine{
		{Text: header, EventType: evTypeExtensionError, RowID: rowID},
		{Text: "  " + body, EventType: evTypeExtensionErrorBody, RowID: rowID},
	}
}

// renderPermissionAsk preserves the pre-#1767 rendering of permission
// asks so the conversation pane keeps working for sessions that hit
// the permission flow. Not in the design-doc renderer table's
// must-have list — covered here for behavioural parity with the
// narrative package's checkin output.
func renderPermissionAsk(rowID int64, payloadJSON string) []narrative.NarrativeLine {
	var p payload.PermissionAsk
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return []narrative.NarrativeLine{
			{Text: fmt.Sprintf("[%s] ⚠ permission ask", ts()), EventType: evTypePermissionAsk, RowID: rowID},
		}
	}
	tool := string(p.Tool)
	if tool == "" {
		tool = "unknown"
	}
	return []narrative.NarrativeLine{
		{Text: fmt.Sprintf("[%s] ⚠ permission: %s", ts(), tool),
			EventType: evTypePermissionAsk, RowID: rowID, MessageID: p.MessageID},
	}
}

// renderPermissionDenied mirrors renderPermissionAsk for the denial
// counterpart.
func renderPermissionDenied(rowID int64, payloadJSON string) []narrative.NarrativeLine {
	var p payload.PermissionDenied
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return []narrative.NarrativeLine{
			{Text: fmt.Sprintf("[%s] ✗ permission denied", ts()), EventType: evTypePermDenied, RowID: rowID},
		}
	}
	tool := p.Tool
	if tool == "" {
		tool = "unknown"
	}
	return []narrative.NarrativeLine{
		{Text: fmt.Sprintf("[%s] ✗ permission denied: %s", ts(), tool),
			EventType: evTypePermDenied, RowID: rowID, MessageID: p.MessageID},
	}
}

// renderThinking renders the collapsed "· thinking…" placeholder.
func renderThinking(rowID int64, _ string) []narrative.NarrativeLine {
	return []narrative.NarrativeLine{
		{Text: fmt.Sprintf("[%s] · thinking…", ts()), EventType: evTypeThinking, RowID: rowID},
	}
}

// renderSuppressed is the handler used by event types the design-doc
// renderer table marks as "Suppressed in conversation" (session_status)
// or "implicit visual separator" (turn_start, turn_end). Returns nil
// so the caller does NOT enter the line into the buffer or the
// dedupe map — replays of these events are also no-ops.
func renderSuppressed(_ int64, _ string) []narrative.NarrativeLine {
	return nil
}

// renderFallback handles unknown event types. Per the AC: "An
// unrecognised event type does not panic the renderer; the row is
// skipped (or rendered as a debug-visible fallback, worker's call)."
// We choose debug-visible so an operator running with the TUI open
// when a new event type lands sees that *something* arrived; silent
// skip would hide the bug-hunting clue that pre-#1764 cost us a week.
//
// Note: this is a closure-friendly 3-arg signature (eventType is
// captured), distinct from the per-type eventHandler signature.
func renderFallback(rowID int64, eventType, _ string) []narrative.NarrativeLine {
	return []narrative.NarrativeLine{
		{Text: fmt.Sprintf("[%s] %s", ts(), eventType), EventType: eventType, RowID: rowID},
	}
}
