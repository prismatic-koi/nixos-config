// Package narrative renders iris agent events as human-readable narrative
// lines, modelled on prism's `prism checkin` output.
//
// This package is the canonical home for the per-event rendering logic that
// is shared by the iris TUI's event pane and the `iris checkin` CLI. It used
// to live at internal/iris/tui/narrative.go (issue #1676); moving it out of
// the tui package keeps the rendering logic decoupled from any specific
// surface so it can be reused by CLI subcommands without a tui import.
//
// # What it does
//
// RenderEvent converts a single DaemonSessionEventFrame-equivalent triple
// (rowID, eventType, payloadJSON) into zero or more NarrativeLines. The
// pairing of tool_call ↔ tool_result is done by the caller (typically the
// TUI's model) by matching MessageIDs across the line buffer.
//
// Higher-level rendering (the turn-grouped layout used by `iris checkin`)
// builds on the exported helpers — ToolKeyArg, ToolResultSummary, TurnLabel,
// ExtractMessageID — to assemble assistant-turn-centric output without
// duplicating the per-tool parsing rules.
//
// # Event types handled
//
//	msg_user           — ▶ user  [agent · model]\n<text>
//	msg_assistant      — [HH:MM:SS] assistant  [agent · model]\n<text>
//	tool_call          — → <tool>: <key-arg>
//	tool_result        — (paired by caller; emitted as "    ↳ result: …")
//	state_change       — ● <state>
//	permission_ask     — ⚠ permission: <tool>
//	permission_denied  — ✗ permission denied: <tool>
//	thinking           — · thinking…
//	turn_start         — (collapsed; nil)
//	turn_end           — (collapsed; nil)
//	<other>            — [HH:MM:SS] <type>
package narrative

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/payload"
)

// NarrativeLine is a single rendered line in an event pane.
//
// One source event can produce zero (collapsed), one, or two lines (e.g. a
// header + body for msg_assistant). Callers index into a slice of these to
// build the displayed pane.
type NarrativeLine struct {
	// Text is the display string. May contain no leading timestamp (tool
	// one-liners) so callers must not assume a fixed prefix.
	Text string
	// EventType is the source event type (used by the TUI for colour
	// styling — checkin ignores it).
	EventType string
	// RowID is the source event's DB row identity. Used by the TUI for
	// deduplication of replayed-vs-live events.
	RowID int64
	// MessageID links tool_call ↔ tool_result for pairing inside the
	// caller's line buffer.
	MessageID string
	// IsPaired is set to true on a tool_call line once its result arrives
	// and has been folded into Text.
	IsPaired bool
	// ResultText is the one-liner result, appended when tool_result arrives
	// — exposed separately so the caller can choose its own indent/style.
	ResultText string
}

// RenderEvent converts a daemon session_event frame triple into one or more
// NarrativeLines. It never returns nil but may return an empty slice for
// noisy events that are intentionally collapsed (thinking, turn_start, etc).
//
// The rendered timestamp uses time.Now() as a best-effort wall clock; the
// daemon owns authoritative timestamps and callers that have an event
// CreatedAt should prefer that for stable output. The TUI uses Now() because
// its event stream is intrinsically live; the checkin CLI overlays its own
// timestamps when assembling output.
func RenderEvent(rowID int64, eventType, payloadJSON string) []NarrativeLine {
	ts := time.Now().Format("15:04:05")

	switch eventType {
	case "msg_user":
		var p payload.MsgUser
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			p.Text = payloadJSON
		}
		text := p.Text
		if text == "" {
			text = "(no text)"
		}
		label := TurnLabel(p.Agent, p.Model)
		var header string
		if label != "" {
			header = fmt.Sprintf("[%s] ▶ user  [%s]", ts, label)
		} else {
			header = fmt.Sprintf("[%s] ▶ user", ts)
		}
		return []NarrativeLine{
			{Text: header, EventType: eventType, RowID: rowID, MessageID: p.MessageID},
			{Text: text, EventType: eventType + "_body", RowID: rowID},
		}

	case "msg_assistant":
		var p payload.MsgAssistant
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			p.Text = payloadJSON
		}
		text := p.Text
		if text == "" {
			text = "(no text)"
		}
		label := TurnLabel(p.Agent, p.Model)
		var header string
		if label != "" {
			header = fmt.Sprintf("[%s] assistant  [%s]", ts, label)
		} else {
			header = fmt.Sprintf("[%s] assistant", ts)
		}
		return []NarrativeLine{
			{Text: header, EventType: eventType, RowID: rowID, MessageID: p.MessageID},
			{Text: text, EventType: eventType + "_body", RowID: rowID},
		}

	case "tool_call":
		var p payload.ToolCall
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return []NarrativeLine{
				{Text: fmt.Sprintf("[%s] → tool_call (parse error)", ts), EventType: eventType, RowID: rowID},
			}
		}
		keyArg := ToolKeyArg(p.Tool, p.Args)
		var text string
		if keyArg != "" {
			text = fmt.Sprintf("  → %s: %s", p.Tool, keyArg)
		} else {
			text = fmt.Sprintf("  → %s", p.Tool)
		}
		return []NarrativeLine{
			{Text: text, EventType: eventType, RowID: rowID, MessageID: p.MessageID},
		}

	case "tool_result":
		var p payload.ToolResult
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return nil
		}
		summary := ToolResultSummary(p.Tool, p.Result)
		return []NarrativeLine{
			{Text: fmt.Sprintf("    ↳ result: %s", summary), EventType: eventType, RowID: rowID, MessageID: p.MessageID},
		}

	case "state_change":
		var p payload.StateChange
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			p.State = payloadJSON
		}
		state := p.State
		if state == "" {
			state = "(unknown)"
		}
		return []NarrativeLine{
			{Text: fmt.Sprintf("[%s] ● %s", ts, state), EventType: eventType, RowID: rowID},
		}

	case "permission_ask":
		var p payload.PermissionAsk
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return []NarrativeLine{
				{Text: fmt.Sprintf("[%s] ⚠ permission ask", ts), EventType: eventType, RowID: rowID},
			}
		}
		tool := string(p.Tool)
		if tool == "" {
			tool = "unknown"
		}
		return []NarrativeLine{
			{Text: fmt.Sprintf("[%s] ⚠ permission: %s", ts, tool), EventType: eventType, RowID: rowID, MessageID: p.MessageID},
		}

	case "permission_denied":
		var p payload.PermissionDenied
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return []NarrativeLine{
				{Text: fmt.Sprintf("[%s] ✗ permission denied", ts), EventType: eventType, RowID: rowID},
			}
		}
		tool := p.Tool
		if tool == "" {
			tool = "unknown"
		}
		return []NarrativeLine{
			{Text: fmt.Sprintf("[%s] ✗ permission denied: %s", ts, tool), EventType: eventType, RowID: rowID, MessageID: p.MessageID},
		}

	case "thinking":
		return []NarrativeLine{
			{Text: fmt.Sprintf("[%s] · thinking…", ts), EventType: eventType, RowID: rowID},
		}

	case "turn_start", "turn_end":
		return nil

	default:
		return []NarrativeLine{
			{Text: fmt.Sprintf("[%s] %s", ts, eventType), EventType: eventType, RowID: rowID},
		}
	}
}

// --- exported helpers (used by checkin's turn-grouped renderer) ---

// TurnLabel builds the "[agent · model]" label for a turn header. Returns
// an empty string when both inputs are empty so callers can suppress the
// brackets entirely.
func TurnLabel(agent, model string) string {
	if agent == "" && model == "" {
		return ""
	}
	if agent == "" {
		return model
	}
	if model == "" {
		return agent
	}
	return agent + " · " + model
}

// ToolKeyArg extracts the primary display argument for a tool one-liner.
// The output is bounded at ~80 runes for display in narrow panes.
//
// Per-tool rules:
//
//	bash/Bash         — first ~80 chars of the command string
//	read/edit/write   — file path (filePath / path / file_path keys)
//	task/Task         — first ~80 chars of description / prompt
//	glob/Glob         — pattern
//	grep/Grep         — pattern / regex / query
//	todowrite/*       — "" (tool name alone is enough)
//	<other>           — first ~80 chars of raw args
func ToolKeyArg(tool, args string) string {
	switch tool {
	case "bash", "Bash":
		cmd := ExtractBashCommand(args)
		if len([]rune(cmd)) > 80 {
			return string([]rune(cmd)[:80]) + "..."
		}
		return cmd

	case "read", "Read":
		return ExtractStringField(args, "filePath", "path", "file_path")

	case "edit", "Edit":
		return ExtractStringField(args, "filePath", "path", "file_path")

	case "write", "Write":
		return ExtractStringField(args, "filePath", "path", "file_path")

	case "task", "Task":
		desc := ExtractStringField(args, "description", "desc", "prompt")
		if len([]rune(desc)) > 80 {
			return string([]rune(desc)[:80]) + "..."
		}
		return desc

	case "glob", "Glob":
		return ExtractStringField(args, "pattern", "glob")

	case "grep", "Grep":
		return ExtractStringField(args, "pattern", "regex", "query")

	case "todowrite", "TodoWrite", "Todowrite":
		return ""

	default:
		if len([]rune(args)) > 80 {
			return string([]rune(args)[:80]) + "..."
		}
		return args
	}
}

// ToolResultSummary produces a one-line result summary for a tool's output.
// Bash gets error-heuristic detection; read counts lines; edit/write reduce
// to ✓/✗; glob/grep count match lines.
func ToolResultSummary(tool, result string) string {
	switch tool {
	case "bash", "Bash":
		if result == "" {
			return "✓"
		}
		lower := strings.ToLower(result)
		isErr := strings.Contains(lower, "error:") ||
			strings.Contains(lower, "command not found") ||
			strings.Contains(lower, "exit status") ||
			strings.Contains(lower, "permission denied") ||
			strings.Contains(lower, "no such file")
		line := FirstMeaningfulLine(result)
		if isErr {
			if len([]rune(line)) > 60 {
				return "✗ " + string([]rune(line)[:60]) + "..."
			}
			return "✗ " + line
		}
		if len([]rune(line)) > 60 {
			return string([]rune(line)[:60]) + "..."
		}
		return line

	case "read", "Read":
		if result == "" {
			return "✓ (0 lines)"
		}
		n := strings.Count(result, "\n") + 1
		if strings.HasSuffix(result, "\n") && n > 1 {
			n--
		}
		return fmt.Sprintf("✓ (%d lines)", n)

	case "edit", "Edit", "write", "Write", "task", "Task":
		if IsErrorResult(result) {
			return "✗"
		}
		return "✓"

	case "glob", "Glob", "grep", "Grep":
		return MatchCountSummary(result)

	case "todowrite", "TodoWrite", "Todowrite":
		return "✓"

	default:
		if result == "" {
			return "✓"
		}
		line := FirstMeaningfulLine(result)
		if len([]rune(line)) > 60 {
			return string([]rune(line)[:60]) + "..."
		}
		return line
	}
}

// IsErrorResult applies conservative heuristics to tell whether a tool
// result string represents an error. Used by edit/write/task to choose
// between ✓ and ✗.
func IsErrorResult(result string) bool {
	if strings.Contains(result, "✗") {
		return true
	}
	lower := strings.ToLower(result)
	if strings.HasPrefix(lower, "error") ||
		strings.Contains(lower, "\nerror") {
		return true
	}
	return strings.Contains(lower, "failed:") ||
		strings.Contains(lower, "failed\n") ||
		strings.HasSuffix(lower, "failed")
}

// FirstMeaningfulLine returns the first non-empty, non-whitespace line of s.
// When the entire string is whitespace, it falls back to the raw string so
// the caller always gets something printable.
func FirstMeaningfulLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return s
}

// MatchCountSummary returns "N matches" / "1 match" / "no matches" for a
// glob/grep result string.
func MatchCountSummary(result string) string {
	if result == "" {
		return "no matches"
	}
	count := 0
	for _, line := range strings.Split(result, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	if count == 0 {
		return "no matches"
	}
	if count == 1 {
		return "1 match"
	}
	return fmt.Sprintf("%d matches", count)
}

// ExtractBashCommand returns the command string from bash tool args, which
// may be either a plain JSON string or an object with a "command" / "cmd"
// field.
func ExtractBashCommand(args string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err == nil {
		for _, key := range []string{"command", "cmd"} {
			if raw, ok := obj[key]; ok {
				var s string
				if err := json.Unmarshal(raw, &s); err == nil {
					return s
				}
			}
		}
	}
	var s string
	if err := json.Unmarshal([]byte(args), &s); err == nil {
		return s
	}
	return args
}

// ExtractStringField returns the first matching string field from args
// (treated as a JSON object). When args is not a JSON object but is a
// plain JSON string, that string is returned. Otherwise the raw args
// string is returned.
func ExtractStringField(args string, keys ...string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		var s string
		if err2 := json.Unmarshal([]byte(args), &s); err2 == nil {
			return s
		}
		return args
	}
	for _, key := range keys {
		if raw, ok := obj[key]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil {
				return s
			}
		}
	}
	return args
}

// ExtractMessageID returns the messageId field from any event payload JSON.
// All event payloads that have a messageId share the same JSON tag layout,
// so we unmarshal into payload.MsgUser opportunistically — any struct with
// a MessageID field would work the same way.
func ExtractMessageID(raw string) string {
	var p payload.MsgUser
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ""
	}
	return p.MessageID
}
