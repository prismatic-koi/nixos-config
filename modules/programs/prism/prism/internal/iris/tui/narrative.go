package tui

// narrative.go — narrative event renderer for the TUI right pane.
//
// renderEventLine converts a DaemonSessionEventFrame into a human-readable
// one-line string in the same narrative format as `prism checkin`.
//
// The rendering is intentionally flat (one frame → one or two display lines)
// because the right pane buffers a scrollable list of rendered lines rather
// than a structured model. The pairing of tool_call + tool_result is done by
// matching on payload MessageIDs across the line buffer.
//
// Event types handled:
//
//	msg_user       — ▶ user  [agent · model]\n<text>
//	msg_assistant  — [HH:MM:SS] assistant  [agent · model]\n<text>
//	tool_call      — → <tool>: <key-arg>
//	tool_result    — (inline: appended to matching tool_call line)
//	state_change   — ● <state>
//	permission_ask — ⚠ permission: <tool>
//	permission_denied — ✗ permission denied: <tool>
//	thinking       — (collapsed: "· thinking")
//	turn_start     — (collapsed)
//	turn_end       — (collapsed)
//	<other>        — [HH:MM:SS] <type>

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/payload"
)

// NarrativeLine is a rendered line in the event pane.
type NarrativeLine struct {
	// Text is the display string.
	Text string
	// EventType is the source event type (used for styling).
	EventType string
	// RowID is the source event's DB row ID (for deduplication).
	RowID int64
	// MessageID links tool_call ↔ tool_result for pairing.
	MessageID string
	// IsPaired is set to true on a tool_call line once its result arrives.
	IsPaired bool
	// ResultText is the one-liner result, appended when tool_result arrives.
	ResultText string
}

// RenderEvent converts a daemon session_event frame into one or more
// NarrativeLines. It never returns nil but may return an empty slice for
// noisy events that are intentionally collapsed (thinking, turn_start, etc).
func RenderEvent(rowID int64, eventType, payloadJSON string) []NarrativeLine {
	ts := time.Now().Format("15:04:05") // best-effort timestamp; daemon has authoritative time

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
		label := turnLabel(p.Agent, p.Model)
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
		label := turnLabel(p.Agent, p.Model)
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
		keyArg := toolKeyArg(p.Tool, p.Args)
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
		// Tool results are matched against existing tool_call lines by the
		// TUI model (appendToolResult). We still return a line in case there
		// is no matching call (orphaned result).
		var p payload.ToolResult
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return nil
		}
		summary := toolResultSummary(p.Tool, p.Result)
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
		// Collapse thinking events into a single indicator line.
		return []NarrativeLine{
			{Text: fmt.Sprintf("[%s] · thinking…", ts), EventType: eventType, RowID: rowID},
		}

	case "turn_start", "turn_end":
		// Intentionally collapsed — noisy heartbeat events.
		return nil

	default:
		// Show unknown events so the user can see daemon activity.
		return []NarrativeLine{
			{Text: fmt.Sprintf("[%s] %s", ts, eventType), EventType: eventType, RowID: rowID},
		}
	}
}

// --- helpers (adapted from cmd/checkin_tools.go without DB dependency) ---

// turnLabel builds the "[agent · model]" label for a turn header.
func turnLabel(agent, model string) string {
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

// toolKeyArg extracts the primary display argument for a tool one-liner.
func toolKeyArg(tool, args string) string {
	switch tool {
	case "bash", "Bash":
		cmd := extractBashCommand(args)
		if len([]rune(cmd)) > 80 {
			return string([]rune(cmd)[:80]) + "..."
		}
		return cmd

	case "read", "Read":
		return extractStringField(args, "filePath", "path", "file_path")

	case "edit", "Edit":
		return extractStringField(args, "filePath", "path", "file_path")

	case "write", "Write":
		return extractStringField(args, "filePath", "path", "file_path")

	case "task", "Task":
		desc := extractStringField(args, "description", "desc", "prompt")
		if len([]rune(desc)) > 80 {
			return string([]rune(desc)[:80]) + "..."
		}
		return desc

	case "glob", "Glob":
		return extractStringField(args, "pattern", "glob")

	case "grep", "Grep":
		return extractStringField(args, "pattern", "regex", "query")

	default:
		if len([]rune(args)) > 80 {
			return string([]rune(args)[:80]) + "..."
		}
		return args
	}
}

// toolResultSummary produces a one-line result summary.
func toolResultSummary(tool, result string) string {
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
		line := firstMeaningfulLine(result)
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

	case "edit", "Edit", "write", "Write":
		if isErrorResult(result) {
			return "✗"
		}
		return "✓"

	case "glob", "Glob", "grep", "Grep":
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

	default:
		if result == "" {
			return "✓"
		}
		line := firstMeaningfulLine(result)
		if len([]rune(line)) > 60 {
			return string([]rune(line)[:60]) + "..."
		}
		return line
	}
}

func isErrorResult(result string) bool {
	if strings.Contains(result, "✗") {
		return true
	}
	lower := strings.ToLower(result)
	return strings.HasPrefix(lower, "error") ||
		strings.Contains(lower, "\nerror") ||
		strings.Contains(lower, "failed:") ||
		strings.Contains(lower, "failed\n") ||
		strings.HasSuffix(lower, "failed")
}

func firstMeaningfulLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return s
}

func extractBashCommand(args string) string {
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

func extractStringField(args string, keys ...string) string {
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
