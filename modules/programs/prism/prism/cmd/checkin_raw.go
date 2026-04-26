package cmd

// checkin_raw.go — raw event rendering paths:
//   - renderCheckinEventsRaw: used when --types is explicitly set
//   - renderChildEvent: single child event in legacy raw style
//   - renderChildEventVerbose: single child event with full args/results
//   - renderChildEventsDefault: paired tool one-liners in default mode
//   - renderProxiedCheckin: renders checkin data returned by the host-API /checkin endpoint

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// childEventItem holds an event type and its raw payload JSON.
// Used for grouping child events (tool_call, tool_result, permission_ask,
// permission_denied, thinking) under their parent assistant turn.
type childEventItem struct {
	eventType string
	payload   string
}

// renderCheckinEventsRaw prints raw events (used when --types is explicit).
func renderCheckinEventsRaw(session string, d *db.DB, events []db.Event, verbose bool) error {
	// Fetch state from DB; fall back to tmux if not found.
	state := ""
	status, err := d.CurrentStatus(session)
	if err == nil && status != nil {
		state = status.State
	}
	if state == "" {
		state = tmux.AgentStateOf(session)
	}
	if state == "" {
		state = string(agent.StateIdle)
	}

	fmt.Printf("checkin: %s\n\n", session)
	fmt.Printf("state: %s\n\n", state)

	// Build a map from messageId → inline events (tool_call, tool_result,
	// permission_ask, permission_denied) so that we can render them under
	// the correct msg_assistant row regardless of event ordering in the DB.
	type inlineEvent struct {
		eventType string
		payload   string
	}
	inlineByMsgID := make(map[string][]inlineEvent)
	for _, e := range events {
		switch e.Type {
		case "tool_call", "tool_result", "permission_ask", "permission_denied":
			msgID := extractMessageID(e.Payload)
			if msgID != "" {
				inlineByMsgID[msgID] = append(inlineByMsgID[msgID], inlineEvent{e.Type, e.Payload})
			}
		}
	}

	// Track which messageIds actually have a msg_assistant event in the result set.
	assistantMsgIDs := make(map[string]bool)
	for _, e := range events {
		if e.Type == "msg_assistant" {
			msgID := extractMessageID(e.Payload)
			if msgID != "" {
				assistantMsgIDs[msgID] = true
			}
		}
	}

	for _, e := range events {
		ts := e.CreatedAt.Local().Format("2006-01-02 15:04:05")

		switch e.Type {
		case "msg_user":
			var up payload.MsgUser
			if err := json.Unmarshal([]byte(e.Payload), &up); err != nil {
				up.Text = e.Payload
			}
			text := up.Text
			if text == "" {
				text = "(no text)"
			}
			label := turnLabel(up.Agent, up.Model)
			if label != "" {
				fmt.Printf("[%s] user  [%s]\n%s\n\n", ts, label, text)
			} else {
				fmt.Printf("[%s] user\n%s\n\n", ts, text)
			}

		case "msg_assistant":
			var ap payload.MsgAssistant
			if err := json.Unmarshal([]byte(e.Payload), &ap); err != nil {
				ap.Text = e.Payload
			}
			text := ap.Text
			if text == "" {
				text = "(no text)"
			}
			label := turnLabel(ap.Agent, ap.Model)
			if label != "" {
				fmt.Printf("[%s] assistant  [%s]\n%s\n", ts, label, text)
			} else {
				fmt.Printf("[%s] assistant\n%s\n", ts, text)
			}

			// Render inline children.
			msgID := extractMessageID(e.Payload)
			if msgID != "" {
				for _, ie := range inlineByMsgID[msgID] {
					renderChildEvent(ie.eventType, ie.payload, verbose, "")
				}
			}
			fmt.Println()

		case "tool_call":
			msgID := extractMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			renderChildEvent("tool_call", e.Payload, verbose, "")

		case "tool_result":
			msgID := extractMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			renderChildEvent("tool_result", e.Payload, verbose, "")

		case "permission_ask":
			msgID := extractMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			renderChildEvent("permission_ask", e.Payload, verbose, "")

		case "permission_denied":
			msgID := extractMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			renderChildEvent("permission_denied", e.Payload, verbose, "")

		default:
			// state_change, compaction, error, etc.
			fmt.Printf("[%s] %s: %s\n", ts, e.Type, e.Payload)
		}
	}

	fmt.Println("── end of event log ──")
	return nil
}

// renderChildEvent prints a single child event using the legacy raw-event style.
// Used by renderCheckinEventsRaw (--types path) and renderProxiedCheckin.
// prefix is prepended before the leading spaces (used for subagent indentation).
func renderChildEvent(eventType, rawPayload string, verbose bool, prefix string) {
	switch eventType {
	case "tool_call":
		var p payload.ToolCall
		if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
			fmt.Printf("%s  → (tool_call parse error)\n", prefix)
			return
		}
		args := p.Args
		if !verbose && len(args) > 80 {
			args = args[:80] + "..."
		}
		fmt.Printf("%s  → %s: %s [✓]\n", prefix, p.Tool, args)

	case "tool_result":
		var p payload.ToolResult
		if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
			fmt.Printf("%s  → (tool_result parse error)\n", prefix)
			return
		}
		result := p.Result
		if !verbose && len(result) > 80 {
			result = result[:80] + "..."
		}
		fmt.Printf("%s  → result: %s\n", prefix, result)

	case "permission_ask":
		var p payload.PermissionAsk
		if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
			fmt.Printf("%s  [⏳ waiting for approval: (parse error)]\n", prefix)
			return
		}
		tool := string(p.Tool)
		if tool == "" {
			tool = "unknown"
		}
		if len(p.Patterns) > 0 {
			fmt.Printf("%s  [⏳ waiting for approval: %s — %s]\n", prefix, tool, strings.Join(p.Patterns, ", "))
		} else {
			fmt.Printf("%s  [⏳ waiting for approval: %s]\n", prefix, tool)
		}

	case "permission_denied":
		var p payload.PermissionDenied
		if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
			fmt.Printf("%s  [❌ denied: (parse error)]\n", prefix)
			return
		}
		tool := p.Tool
		if tool == "" {
			tool = "unknown"
		}
		fmt.Printf("%s  [❌ denied: %s]\n", prefix, tool)

	case "thinking":
		var p payload.Thinking
		if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
			return
		}
		if p.Text != "" {
			t := p.Text
			if !verbose && len(t) > 120 {
				t = t[:120] + "..."
			}
			fmt.Printf("%s  [thinking: %s]\n", prefix, t)
		}
	}
}

// renderChildEventVerbose prints a single child event with full args/results
// (no truncation). Used in verbose mode under renderCheckinTurns.
func renderChildEventVerbose(eventType, rawPayload string, prefix string) {
	switch eventType {
	case "tool_call":
		var p payload.ToolCall
		if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
			fmt.Printf("%s  → (tool_call parse error)\n", prefix)
			return
		}
		fmt.Printf("%s  → %s: %s\n", prefix, p.Tool, p.Args)

	case "tool_result":
		var p payload.ToolResult
		if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
			fmt.Printf("%s  → (tool_result parse error)\n", prefix)
			return
		}
		fmt.Printf("%s  → result: %s\n", prefix, p.Result)

	case "permission_ask":
		var p payload.PermissionAsk
		if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
			fmt.Printf("%s  [⏳ waiting for approval: (parse error)]\n", prefix)
			return
		}
		tool := string(p.Tool)
		if tool == "" {
			tool = "unknown"
		}
		if len(p.Patterns) > 0 {
			fmt.Printf("%s  [⏳ waiting for approval: %s — %s]\n", prefix, tool, strings.Join(p.Patterns, ", "))
		} else {
			fmt.Printf("%s  [⏳ waiting for approval: %s]\n", prefix, tool)
		}

	case "permission_denied":
		var p payload.PermissionDenied
		if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
			fmt.Printf("%s  [❌ denied: (parse error)]\n", prefix)
			return
		}
		tool := p.Tool
		if tool == "" {
			tool = "unknown"
		}
		fmt.Printf("%s  [❌ denied: %s]\n", prefix, tool)

	case "thinking":
		var p payload.Thinking
		if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
			return
		}
		if p.Text != "" {
			fmt.Printf("%s  [thinking: %s]\n", prefix, p.Text)
		}
	}
}

// renderChildEventsDefault renders child events under an assistant turn in the
// rich default mode (no --verbose). Tool calls and their results are paired
// into a single one-liner: `→ <tool>: <key_arg> <result_summary>`.
//
// The key arg and result summary format per tool:
//   - bash:      first ~80 chars of command | first meaningful output line or ✓ (empty) or ✗ + stderr
//   - read:      file path | ✓ (N lines)
//   - edit/write: file path | ✓ or ✗
//   - task:      description | ✓ or ✗
//   - glob/grep: pattern | N matches or no matches
//   - todowrite: (dim or omit key arg) | ✓
//
// Tool results are matched positionally to tool calls of the same tool name
// within the message. Permission and thinking events are rendered as before.
func renderChildEventsDefault(children []childEventItem, prefix string) {
	// Split children into tool_calls, tool_results (by tool name for pairing),
	// and other events (permission_ask, permission_denied, thinking).
	//
	// Pairing strategy: maintain a per-tool FIFO queue of results. When we
	// encounter a tool_call for tool T, dequeue the next result for T (if any).
	// This handles the common case where results appear in the same order as calls.
	type toolCallEntry struct {
		tool    string
		args    string
		payload string
	}
	type toolResultEntry struct {
		tool    string
		result  string
		payload string
	}

	// Collect tool calls and results in order.
	var toolCalls []toolCallEntry
	resultsByTool := make(map[string][]toolResultEntry)

	// Also collect other events in order (permission_ask, permission_denied, thinking)
	// to be rendered after the tool one-liners.
	type otherEvent struct {
		eventType string
		payload   string
	}
	var others []otherEvent

	for _, c := range children {
		switch c.eventType {
		case "tool_call":
			var p payload.ToolCall
			if err := json.Unmarshal([]byte(c.payload), &p); err == nil {
				toolCalls = append(toolCalls, toolCallEntry{tool: p.Tool, args: p.Args, payload: c.payload})
				// Pre-index result slot (will be filled when result arrives).
			} else {
				toolCalls = append(toolCalls, toolCallEntry{tool: "?", args: "", payload: c.payload})
			}
		case "tool_result":
			var p payload.ToolResult
			if err := json.Unmarshal([]byte(c.payload), &p); err == nil {
				resultsByTool[p.Tool] = append(resultsByTool[p.Tool], toolResultEntry{tool: p.Tool, result: p.Result, payload: c.payload})
			}
		default:
			others = append(others, otherEvent{c.eventType, c.payload})
		}
	}

	// Render tool one-liners, consuming results from the per-tool FIFO queue.
	usedResults := make(map[string]int) // tool → count consumed
	for _, tc := range toolCalls {
		// Dequeue the next result for this tool.
		resultList := resultsByTool[tc.tool]
		usedIdx := usedResults[tc.tool]
		var resultSummary string
		if usedIdx < len(resultList) {
			resultSummary = toolResultSummary(tc.tool, resultList[usedIdx].result)
			usedResults[tc.tool] = usedIdx + 1
		} else {
			// No result available (still running or not recorded).
			resultSummary = ""
		}

		keyArg := toolKeyArg(tc.tool, tc.args)
		switch {
		case keyArg != "" && resultSummary != "":
			fmt.Printf("%s  → %s: %s %s\n", prefix, tc.tool, keyArg, resultSummary)
		case keyArg != "":
			fmt.Printf("%s  → %s: %s\n", prefix, tc.tool, keyArg)
		case resultSummary != "":
			fmt.Printf("%s  → %s: %s\n", prefix, tc.tool, resultSummary)
		default:
			fmt.Printf("%s  → %s\n", prefix, tc.tool)
		}
	}

	// Render other events (permission_ask, permission_denied, thinking).
	for _, o := range others {
		renderChildEvent(o.eventType, o.payload, false, prefix)
	}
}

// renderProxiedCheckin renders checkin output from the raw JSON returned by the
// host-API /checkin endpoint. The JSON has the shape:
//
//	{"session":"<name>", "state":"<state>", "events":[...db.Event...]}
//
// NOTE: This function uses the legacy raw-event rendering (separate tool_call
// and tool_result lines) rather than the rich default one-liner format. The
// sidecar /checkin endpoint returns flat raw events, and the assistant-turn-centric
// pairing logic used by renderCheckinTurns requires either a live DB connection
// or a secondary query to resolve children by messageId — both of which are
// unavailable in the proxy context. Upgrading this path to match the rich default
// is tracked as future work.
func renderProxiedCheckin(raw []byte, verbose bool) error {
	var resp struct {
		Session string     `json:"session"`
		State   string     `json:"state"`
		Events  []db.Event `json:"events"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("checkin proxy: unmarshal response: %w", err)
	}

	state := resp.State
	if state == "" {
		state = "idle"
	}

	fmt.Printf("checkin: %s\n\n", resp.Session)
	fmt.Printf("state: %s\n\n", state)

	if len(resp.Events) == 0 {
		fmt.Println("── end of event log ──")
		return nil
	}

	// Use the raw-event renderer for simplicity — the sidecar returns raw events.
	// Build a map for inline children keyed by messageId.
	type inlineEvent struct {
		eventType string
		payload   string
	}
	inlineByMsgID := make(map[string][]inlineEvent)
	for _, e := range resp.Events {
		switch e.Type {
		case "tool_call", "tool_result", "permission_ask", "permission_denied", "thinking":
			msgID := extractMessageID(e.Payload)
			if msgID != "" {
				inlineByMsgID[msgID] = append(inlineByMsgID[msgID], inlineEvent{e.Type, e.Payload})
			}
		}
	}

	assistantMsgIDs := make(map[string]bool)
	for _, e := range resp.Events {
		if e.Type == "msg_assistant" {
			msgID := extractMessageID(e.Payload)
			if msgID != "" {
				assistantMsgIDs[msgID] = true
			}
		}
	}

	for _, e := range resp.Events {
		ts := e.CreatedAt.Local().Format("2006-01-02 15:04:05")

		switch e.Type {
		case "msg_user":
			var up payload.MsgUser
			if err := json.Unmarshal([]byte(e.Payload), &up); err != nil {
				up.Text = e.Payload
			}
			text := up.Text
			if text == "" {
				text = "(no text)"
			}
			label := turnLabel(up.Agent, up.Model)
			if label != "" {
				fmt.Printf("[%s] user  [%s]\n%s\n\n", ts, label, text)
			} else {
				fmt.Printf("[%s] user\n%s\n\n", ts, text)
			}

		case "msg_assistant":
			var ap payload.MsgAssistant
			if err := json.Unmarshal([]byte(e.Payload), &ap); err != nil {
				ap.Text = e.Payload
			}
			text := ap.Text
			if text == "" {
				text = "(no text)"
			}
			label := turnLabel(ap.Agent, ap.Model)
			if label != "" {
				fmt.Printf("[%s] assistant  [%s]\n%s\n", ts, label, text)
			} else {
				fmt.Printf("[%s] assistant\n%s\n", ts, text)
			}

			msgID := extractMessageID(e.Payload)
			if msgID != "" {
				for _, ie := range inlineByMsgID[msgID] {
					renderChildEvent(ie.eventType, ie.payload, verbose, "")
				}
			}
			fmt.Println()

		case "tool_call":
			msgID := extractMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			renderChildEvent("tool_call", e.Payload, verbose, "")

		case "tool_result":
			msgID := extractMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			renderChildEvent("tool_result", e.Payload, verbose, "")

		case "permission_ask":
			msgID := extractMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			renderChildEvent("permission_ask", e.Payload, verbose, "")

		case "permission_denied":
			msgID := extractMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			renderChildEvent("permission_denied", e.Payload, verbose, "")

		case "thinking":
			msgID := extractMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			renderChildEvent("thinking", e.Payload, verbose, "")

		default:
			fmt.Printf("[%s] %s: %s\n", ts, e.Type, e.Payload)
		}
	}

	fmt.Println("── end of event log ──")
	return nil
}
