package cmd

// prism checkin — show recent conversation events for a session from DB,
// falling back to tmux screen-scrape when the session has no DB rows.
//
// Usage:
//
//	prism checkin [<session>]
//	prism checkin <session> --last N          show last N turns (default 10)
//	prism checkin <session> --from <id>       show N events forward from event ID
//	prism checkin <session> --before <id>     show N events backward from event ID
//	prism checkin <session> --types <list>    comma-separated event types (orthogonal filter)
//	prism checkin <session> --verbose / -v    full tool args/results (no truncation)
//	prism checkin --all                       (no-arg) list all sessions across all repos
//
// Default output (no flags): interleaved assistant messages + state changes +
// tool call one-liners with truncated results. --verbose shows full forensic
// output. --types is an orthogonal filter for targeted queries (e.g. --types audit).

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/tmux"
)

var checkinCmd = &cobra.Command{
	Use:   "checkin [session]",
	Short: "Show recent conversation events for a session",
	Long: `Show the recent conversation history for a named session, read from the
prism DB. Falls back to tmux screen-scrape for sessions that have no DB rows.

With no argument, lists available sessions for the current repo (use --all
for all repos).`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCheckin,
}

func init() {
	checkinCmd.Flags().Int("last", 10, "Number of conversation turns to show")
	checkinCmd.Flags().String("from", "", "Show events forward from this event ID")
	checkinCmd.Flags().String("before", "", "Show events backward from this event ID")
	checkinCmd.Flags().String("types", "", "Orthogonal event-type filter (e.g. --types audit, --types state_change). When set, routes to the raw-event path instead of the rich default view.")
	checkinCmd.Flags().BoolP("verbose", "v", false, "Show full tool args/results without truncation (forensic mode)")
	checkinCmd.Flags().Bool("all", false, "List all sessions across all repos (no-arg mode only)")
	rootCmd.AddCommand(checkinCmd)
}

func runCheckin(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		showAll, _ := cmd.Flags().GetBool("all")
		return runCheckinNoArg(showAll)
	}

	last, _ := cmd.Flags().GetInt("last")
	fromID, _ := cmd.Flags().GetString("from")
	beforeID, _ := cmd.Flags().GetString("before")
	typesRaw, _ := cmd.Flags().GetString("types")
	verbose, _ := cmd.Flags().GetBool("verbose")

	var afterPtr *string
	if fromID != "" {
		afterPtr = &fromID
	}
	var beforePtr *string
	if beforeID != "" {
		beforePtr = &beforeID
	}

	// Parse types; nil means "use default" (message-turn-centric mode).
	var types []string
	if typesRaw != "" {
		for _, t := range strings.Split(typesRaw, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				types = append(types, t)
			}
		}
	}

	sessionArg := args[0]

	// Special case: if the session argument ends with "~review" (no round number),
	// prefix-match all ~review-* rounds for the parent session and display a
	// summary of each round.
	if strings.HasSuffix(sessionArg, "~review") {
		return runCheckinReviewRounds(sessionArg, verbose)
	}

	return runCheckinSession(sessionArg, last, beforePtr, afterPtr, types, verbose)
}

// runCheckinSession is the DB-backed path. Falls back to legacy screen-scrape
// if the DB is unavailable or has no rows for this session.
//
// When no explicit types are requested, this uses the assistant-turn-centric
// rendering mode: --last N means N assistant turns, not N raw events.
// For each turn it fetches all associated tool/permission/thinking events via
// a secondary query keyed by messageId. msg_user events within the time window
// of the fetched assistant turns are also included.
func runCheckinSession(session string, limit int, before, after *string, types []string, verbose bool) error {
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		raw, err := proxyCheckin(apiURL, session, limit, before, after, types, verbose)
		if err != nil {
			return err
		}
		return renderProxiedCheckin(raw, verbose)
	}
	// When --types is explicitly set, use the old raw-event query path.
	// The new assistant-turn-centric path only applies to the default view.
	if len(types) > 0 {
		return runCheckinSessionRaw(session, limit, before, after, types, verbose)
	}

	d, err := openDB()
	if err == nil {
		defer d.Close()
		// Primary query: fetch last N msg_assistant events to get N assistant turns.
		assistantEvents, qerr := d.QueryEvents(session, limit, before, after, []string{"msg_assistant"})
		if qerr == nil && len(assistantEvents) > 0 {
			return renderCheckinTurns(session, d, assistantEvents, verbose)
		}
		// If DB is open but no msg_assistant rows exist, check whether there are
		// any events at all (e.g. a session with only msg_user). In that case show
		// just the header+footer rather than falling back to the screen-scrape.
		if qerr == nil {
			anyEvents, aerr := d.QueryEvents(session, 1, nil, nil, nil)
			if aerr == nil && len(anyEvents) > 0 {
				// Session has events but no assistant turns yet — render header only.
				return renderCheckinTurns(session, d, nil, verbose)
			}
		}
	}

	// No DB rows (or DB unavailable) — fall back to screen capture.
	return runCheckinSessionLegacy(session, 100)
}

// runCheckinSessionRaw is the legacy raw-event query path, used when --types
// is explicitly specified. It returns raw events without turn grouping.
func runCheckinSessionRaw(session string, limit int, before, after *string, types []string, verbose bool) error {
	d, err := openDB()
	if err == nil {
		defer d.Close()
		events, qerr := d.QueryEvents(session, limit, before, after, types)
		if qerr == nil && len(events) > 0 {
			return renderCheckinEventsRaw(session, d, events, verbose)
		}
	}
	return runCheckinSessionLegacy(session, 100)
}

// renderCheckinTurns renders conversation turns using the assistant-turn-centric
// approach: one turn = one assistant message + its tool/permission/thinking children.
// msg_user events whose created_at falls within the time window of the fetched
// assistant turns are also rendered, immediately before the assistant turn that
// follows them in the timeline. state_change events within the window are also
// interleaved with a ● marker.
//
// assistantEvents may be nil (session has events but no assistant turns yet);
// in that case only the header and footer are rendered.
//
// Default mode (verbose=false): rich one-liner per tool call, state changes
// shown inline, msg_user shown with ▶ prefix.
//
// Verbose mode (verbose=true): full tool args + full results, no truncation;
// same as prior behaviour. Subagent turns shown inline with │ prefix.
//
// Subagent turns (where the agent field differs from the session's root agent)
// are collapsed into a single summary line in default mode. In verbose mode they
// are shown inline with a visual indent prefix.
func renderCheckinTurns(session string, d *db.DB, assistantEvents []db.Event, verbose bool) error {
	// Fetch state from DB; fall back to tmux if not found.
	state := ""
	var rootAgentName string
	status, err := d.CurrentStatus(session)
	if err == nil && status != nil {
		state = status.State
		if status.RootAgentName != nil {
			rootAgentName = *status.RootAgentName
		}
	}
	if state == "" {
		state = tmux.AgentStateOf(session)
	}
	if state == "" {
		state = string(agent.StateIdle)
	}

	fmt.Printf("checkin: %s\n\n", session)
	fmt.Printf("state: %s\n\n", state)

	if len(assistantEvents) == 0 {
		fmt.Println("── end of event log ──")
		return nil
	}

	// Collect all messageIds from the assistant events.
	messageIDs := make([]string, 0, len(assistantEvents))
	for _, e := range assistantEvents {
		msgID := extractMessageID(e.Payload)
		if msgID != "" {
			messageIDs = append(messageIDs, msgID)
		}
	}

	// Secondary query: fetch tool calls, results, permission events, and
	// thinking events that share a messageId with one of the assistant events.
	childTypes := []string{"tool_call", "tool_result", "permission_ask", "permission_denied", "thinking"}
	secondary, serr := d.QueryEventsByMessageIDs(session, messageIDs, childTypes)

	// Organise children by messageId.
	childrenByMsgID := make(map[string][]childEventItem)
	if serr == nil {
		for _, e := range secondary {
			msgID := extractMessageID(e.Payload)
			if msgID == "" {
				continue
			}
			childrenByMsgID[msgID] = append(childrenByMsgID[msgID], childEventItem{e.Type, e.Payload})
		}
	}

	// Determine the time window spanned by the fetched assistant turns so that
	// we can include msg_user and state_change events that fall within it.
	earliest := assistantEvents[0].CreatedAt
	latest := assistantEvents[len(assistantEvents)-1].CreatedAt
	for _, ae := range assistantEvents {
		if ae.CreatedAt.Before(earliest) {
			earliest = ae.CreatedAt
		}
		if ae.CreatedAt.After(latest) {
			latest = ae.CreatedAt
		}
	}

	// Fetch msg_user events within [earliest, latest] (inclusive).
	userEventsAll, _ := d.QueryEvents(session, 0, nil, nil, []string{"msg_user"})
	var userEventsInWindow []db.Event
	for _, ue := range userEventsAll {
		if !ue.CreatedAt.Before(earliest) && !ue.CreatedAt.After(latest) {
			userEventsInWindow = append(userEventsInWindow, ue)
		}
	}

	// Fetch state_change events within [earliest, latest] (inclusive).
	// These are interleaved chronologically with ● markers in the default view.
	stateChangeEventsAll, _ := d.QueryEvents(session, 0, nil, nil, []string{"state_change"})
	var stateChangeEventsInWindow []db.Event
	for _, se := range stateChangeEventsAll {
		if !se.CreatedAt.Before(earliest) && !se.CreatedAt.After(latest) {
			stateChangeEventsInWindow = append(stateChangeEventsInWindow, se)
		}
	}

	// Merge assistant events, user events, and state_change events into a
	// single timeline sorted ASC.
	const (
		entryAssistant   = 0
		entryUser        = 1
		entryStateChange = 2
	)
	type timelineEntry struct {
		entryKind int // entryAssistant, entryUser, or entryStateChange
		event     db.Event
	}
	timeline := make([]timelineEntry, 0, len(assistantEvents)+len(userEventsInWindow)+len(stateChangeEventsInWindow))
	for _, ae := range assistantEvents {
		timeline = append(timeline, timelineEntry{entryKind: entryAssistant, event: ae})
	}
	for _, ue := range userEventsInWindow {
		timeline = append(timeline, timelineEntry{entryKind: entryUser, event: ue})
	}
	for _, se := range stateChangeEventsInWindow {
		timeline = append(timeline, timelineEntry{entryKind: entryStateChange, event: se})
	}
	// Sort by created_at ASC (stable insertion sort, preserving insertion order for ties).
	for i := 1; i < len(timeline); i++ {
		for j := i; j > 0 && timeline[j].event.CreatedAt.Before(timeline[j-1].event.CreatedAt); j-- {
			timeline[j], timeline[j-1] = timeline[j-1], timeline[j]
		}
	}

	// isSubagentEntry returns true when an entry's agent differs from the root
	// agent, indicating it belongs to a subagent invocation. When rootAgentName
	// is empty (pre-migration sessions), all entries are treated as root-agent
	// entries to preserve current behaviour.
	isSubagentEntry := func(entry timelineEntry) bool {
		if rootAgentName == "" {
			return false
		}
		var entryAgent string
		switch entry.entryKind {
		case entryUser:
			var up payload.MsgUser
			if err := json.Unmarshal([]byte(entry.event.Payload), &up); err == nil {
				entryAgent = up.Agent
			}
		case entryAssistant:
			var ap payload.MsgAssistant
			if err := json.Unmarshal([]byte(entry.event.Payload), &ap); err == nil {
				entryAgent = ap.Agent
			}
		}
		return entryAgent != "" && entryAgent != rootAgentName
	}

	// Render the merged timeline, collapsing subagent runs in default mode.
	i := 0
	for i < len(timeline) {
		entry := timeline[i]

		// state_change events are never considered subagent entries — render them always.
		if entry.entryKind == entryStateChange {
			var sc payload.StateChange
			if jerr := json.Unmarshal([]byte(entry.event.Payload), &sc); jerr != nil {
				sc.State = entry.event.Payload
			}
			ts := entry.event.CreatedAt.Local().Format("15:04:05")
			newState := sc.State
			if newState == "" {
				newState = "(unknown)"
			}
			fmt.Printf("[%s] ● %s\n\n", ts, newState)
			i++
			continue
		}

		if isSubagentEntry(entry) && !verbose {
			// Collapse consecutive subagent turns into a single summary line.
			// Count tool calls across all subagent entries in this run and
			// measure the duration from first to last event.
			// state_change events that fall between subagent turns are rendered
			// inline before being consumed so they are not silently dropped.
			runStart := entry.event.CreatedAt
			runEnd := entry.event.CreatedAt
			toolCalls := 0
			subagentName := ""

			j := i
			for j < len(timeline) && (timeline[j].entryKind == entryStateChange || isSubagentEntry(timeline[j])) {
				e := timeline[j]
				if e.entryKind == entryStateChange {
					// Render state_change inline rather than silently dropping it.
					var sc payload.StateChange
					if jerr := json.Unmarshal([]byte(e.event.Payload), &sc); jerr != nil {
						sc.State = e.event.Payload
					}
					scTS := e.event.CreatedAt.Local().Format("15:04:05")
					scState := sc.State
					if scState == "" {
						scState = "(unknown)"
					}
					fmt.Printf("[%s] ● %s\n\n", scTS, scState)
					j++
					continue
				}
				if e.event.CreatedAt.After(runEnd) {
					runEnd = e.event.CreatedAt
				}
				if e.entryKind == entryAssistant {
					var ap payload.MsgAssistant
					if err := json.Unmarshal([]byte(e.event.Payload), &ap); err == nil {
						if subagentName == "" {
							subagentName = ap.Agent
						}
					}
					msgID := extractMessageID(e.event.Payload)
					for _, child := range childrenByMsgID[msgID] {
						if child.eventType == "tool_call" {
							toolCalls++
						}
					}
				}
				j++
			}

			dur := runEnd.Sub(runStart)
			label := subagentName
			if label == "" {
				label = "subagent"
			}
			durStr := formatDuration(dur)
			if toolCalls > 0 {
				fmt.Printf("  └─ %s — %d tool call", label, toolCalls)
				if toolCalls != 1 {
					fmt.Print("s")
				}
				fmt.Printf(" · %s\n\n", durStr)
			} else {
				fmt.Printf("  └─ %s · %s\n\n", label, durStr)
			}
			i = j
			continue
		}

		e := entry.event
		ts := e.CreatedAt.Local().Format("15:04:05")

		// In verbose mode, prefix subagent lines with an indent marker.
		prefix := ""
		if isSubagentEntry(entry) && verbose {
			prefix = "  │ "
		}

		if entry.entryKind == entryUser {
			var up payload.MsgUser
			if jerr := json.Unmarshal([]byte(e.Payload), &up); jerr != nil {
				up.Text = e.Payload
			}
			text := up.Text
			if text == "" {
				text = "(no text)"
			}
			label := turnLabel(up.Agent, up.Model)
			if label != "" {
				fmt.Printf("%s[%s] ▶ user  [%s]\n", prefix, ts, label)
			} else {
				fmt.Printf("%s[%s] ▶ user\n", prefix, ts)
			}
			fmt.Printf("%s%s\n\n", prefix, text)
			i++
			continue
		}

		// Assistant event.
		var ap payload.MsgAssistant
		if jerr := json.Unmarshal([]byte(e.Payload), &ap); jerr != nil {
			ap.Text = e.Payload
		}
		alabel := turnLabel(ap.Agent, ap.Model)
		if alabel != "" {
			fmt.Printf("%s[%s] assistant  [%s]\n", prefix, ts, alabel)
		} else {
			fmt.Printf("%s[%s] assistant\n", prefix, ts)
		}
		atext := ap.Text
		if atext == "" {
			atext = "(no text)"
		}
		fmt.Printf("%s%s\n", prefix, atext)

		msgID := extractMessageID(e.Payload)
		children := childrenByMsgID[msgID]
		if verbose {
			// Verbose mode: render each child event individually (full args/results).
			for _, child := range children {
				renderChildEventVerbose(child.eventType, child.payload, prefix)
			}
		} else {
			// Default mode: render paired tool one-liners + permission/thinking events.
			renderChildEventsDefault(children, prefix)
		}
		fmt.Println()
		i++
	}

	fmt.Println("── end of event log ──")
	return nil
}

// formatDuration formats a duration as "Xm Ys" or "<1s".
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	secs := int(d.Seconds())
	mins := secs / 60
	secs = secs % 60
	if mins > 0 {
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

// turnLabel builds the "[agent · model]" label for a turn header.
// Returns an empty string when both agent and model are absent.
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

// childEventItem holds an event type and its raw payload JSON.
// Used for grouping child events (tool_call, tool_result, permission_ask,
// permission_denied, thinking) under their parent assistant turn.
type childEventItem struct {
	eventType string
	payload   string
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

// toolKeyArg extracts the key argument for the one-liner display per tool type.
// For bash: first ~80 chars of the command string.
// For read/edit/write: the file path.
// For task: the description.
// For glob/grep: the pattern.
// For todowrite: empty string (tool name alone is sufficient).
// For unknown tools: first ~80 chars of args.
func toolKeyArg(tool, args string) string {
	switch tool {
	case "bash", "Bash":
		// Args for bash is typically the command string directly.
		cmd := extractBashCommand(args)
		if len([]rune(cmd)) > 80 {
			runes := []rune(cmd)
			return string(runes[:80]) + "..."
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
			runes := []rune(desc)
			return string(runes[:80]) + "..."
		}
		return desc

	case "glob", "Glob":
		return extractStringField(args, "pattern", "glob")

	case "grep", "Grep":
		return extractStringField(args, "pattern", "regex", "query")

	case "todowrite", "TodoWrite", "Todowrite":
		return ""

	default:
		// Generic: first ~80 chars of raw args.
		if len([]rune(args)) > 80 {
			runes := []rune(args)
			return string(runes[:80]) + "..."
		}
		return args
	}
}

// toolResultSummary produces a one-line result summary for the given tool and
// raw result string.
func toolResultSummary(tool, result string) string {
	switch tool {
	case "bash", "Bash":
		return bashResultSummary(result)

	case "read", "Read":
		// Count lines in result.
		if result == "" {
			return "✓ (0 lines)"
		}
		n := strings.Count(result, "\n") + 1
		// If result ends with a trailing newline, don't count the empty last line.
		if strings.HasSuffix(result, "\n") && n > 1 {
			n--
		}
		return fmt.Sprintf("✓ (%d lines)", n)

	case "edit", "Edit":
		if isErrorResult(result) {
			return "✗"
		}
		return "✓"

	case "write", "Write":
		if isErrorResult(result) {
			return "✗"
		}
		return "✓"

	case "task", "Task":
		if isErrorResult(result) {
			return "✗"
		}
		return "✓"

	case "glob", "Glob":
		return matchCountSummary(result)

	case "grep", "Grep":
		return matchCountSummary(result)

	case "todowrite", "TodoWrite", "Todowrite":
		return "✓"

	default:
		// Generic: first meaningful line or ✓ if empty.
		if result == "" {
			return "✓"
		}
		line := firstMeaningfulLine(result)
		if len([]rune(line)) > 60 {
			runes := []rune(line)
			return string(runes[:60]) + "..."
		}
		return line
	}
}

// bashResultSummary extracts a one-line summary from a bash tool result.
// Returns ✓ for empty output, ✗ + first stderr line for errors, or the
// first meaningful output line otherwise.
func bashResultSummary(result string) string {
	if result == "" {
		return "✓"
	}
	// Check for common error indicators in the result.
	lower := strings.ToLower(result)
	isErr := strings.Contains(lower, "error:") ||
		strings.Contains(lower, "command not found") ||
		strings.Contains(lower, "exit status") ||
		strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "no such file")

	line := firstMeaningfulLine(result)
	if isErr {
		if len([]rune(line)) > 60 {
			runes := []rune(line)
			return "✗ " + string(runes[:60]) + "..."
		}
		return "✗ " + line
	}
	if len([]rune(line)) > 60 {
		runes := []rune(line)
		return string(runes[:60]) + "..."
	}
	return line
}

// isErrorResult returns true if the result string looks like an error.
// Uses conservative heuristics to avoid false positives — e.g. a file named
// "error_handler.go" or a commit message containing "error" should not trigger
// this. We require the error marker to appear at the start of the result, at
// the start of a line, or as part of a well-known error pattern.
func isErrorResult(result string) bool {
	if strings.Contains(result, "✗") {
		return true
	}
	lower := strings.ToLower(result)
	// "Error:" at the beginning of the result or after a newline.
	if strings.HasPrefix(lower, "error") ||
		strings.Contains(lower, "\nerror") {
		return true
	}
	// Explicit failure patterns.
	return strings.Contains(lower, "failed:") ||
		strings.Contains(lower, "failed\n") ||
		strings.HasSuffix(lower, "failed")
}

// matchCountSummary returns "N matches" or "no matches" from a glob/grep result.
func matchCountSummary(result string) string {
	if result == "" {
		return "no matches"
	}
	// Count non-empty lines as matches.
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

// firstMeaningfulLine returns the first non-empty, non-whitespace line from s.
func firstMeaningfulLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return s
}

// extractBashCommand extracts the command string from bash tool args.
// The args may be a plain string (the command itself) or a JSON object
// with a "command" or "cmd" field.
func extractBashCommand(args string) string {
	// Try JSON object first.
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
	// Try plain JSON string.
	var s string
	if err := json.Unmarshal([]byte(args), &s); err == nil {
		return s
	}
	// Fall back to raw args.
	return args
}

// extractStringField extracts a string value from a JSON object by trying
// each key in order. Returns the raw string if none match or if args is not JSON.
func extractStringField(args string, keys ...string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		// Not a JSON object — try plain JSON string.
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

// extractMessageID returns the messageId field from a payload JSON string.
func extractMessageID(raw string) string {
	var p payload.MsgUser // any struct with MessageID works here
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ""
	}
	return p.MessageID
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

// runCheckinSessionLegacy is the old screen-scrape path, kept as a fallback.
func runCheckinSessionLegacy(session string, height int) error {
	if !tmux.HasSession(session) {
		return fmt.Errorf("session %q not found\nrun `prism list-sessions` to see available sessions", session)
	}

	result, err := tmux.CapturePaneScreen(session, height)
	if err != nil {
		return fmt.Errorf("checkin %s: %w", session, err)
	}

	state := tmux.AgentStateOf(session)
	if state == "" {
		state = string(agent.StateIdle)
	}

	styleBold := lipgloss.NewStyle().Bold(true)
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleState := stateStyle(state)

	fmt.Printf("%s %s\n\n", styleBold.Render("checkin:"), session)
	fmt.Printf("state: %s\n\n", styleState.Render(state))
	if result.Screen != "" {
		fmt.Println(result.Screen)
		fmt.Println()
	}
	fmt.Println(styleDim.Render("── end of screen capture ──"))
	return nil
}

// runCheckinNoArg lists sessions from the DB (scoped to the current repo by
// default), falling back to tmux.Sessions() if the DB is unavailable.
func runCheckinNoArg(showAll bool) error {
	// Derive currentRepo from CWD using same logic as list-sessions.
	currentRepo := ""
	cwd, err := os.Getwd()
	if err != nil {
		cwd = os.Getenv("PRISM_SPAWN_PATH")
	}
	if cwd != "" {
		currentRepo = deriveRepo(cwd)
	}

	d, dbErr := openDB()
	if dbErr == nil {
		defer d.Close()

		var (
			ss       []db.Status
			queryErr error
		)
		if showAll {
			ss, queryErr = d.AllActiveStatus()
		} else if currentRepo != "" {
			ss, queryErr = d.AllActiveStatusForRepo(currentRepo)
		} else {
			ss, queryErr = d.AllActiveStatus()
		}

		if queryErr == nil {
			return printSessionTable(ss)
		}
	}

	// DB unavailable — fall back to tmux.
	sessions, terr := tmux.Sessions()
	if terr != nil {
		return terr
	}

	if len(sessions) == 0 {
		return fmt.Errorf("no agent sessions found")
	}

	// Convert tmux sessions to a minimal Status slice for the shared renderer.
	var ss []db.Status
	for _, s := range sessions {
		if s.Name == "scratchpad" || s.Name == "prism-dashboard" {
			continue
		}
		state := s.AgentState
		if state == "" {
			state = string(agent.StateIdle)
		}
		title := s.AgentTitle
		st := db.Status{
			SessionName: s.Name,
			State:       state,
		}
		if title != "" {
			st.Title = &title
		}
		ss = append(ss, st)
	}

	return printSessionTable(ss)
}

// printSessionTable renders a SESSION/STATE/TITLE table and a hint line.
// Returns an error only when the list is empty (to guide the user).
func printSessionTable(ss []db.Status) error {
	if len(ss) == 0 {
		fmt.Println("no agent sessions found")
		return nil
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleName := lipgloss.NewStyle().Bold(true)
	styleTitle := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	fmt.Println(styleHeader.Render(fmt.Sprintf("%-40s  %-8s  %s", "SESSION", "STATE", "TITLE")))
	for _, s := range ss {
		state := s.State
		if state == "" {
			state = string(agent.StateIdle)
		}
		title := "—"
		if s.Title != nil && *s.Title != "" {
			title = *s.Title
		}
		if runes := []rune(title); len(runes) > 60 {
			title = string(runes[:57]) + "..."
		}
		stateStyled := stateStyle(state).Render(fmt.Sprintf("%-8s", state))
		nameStyle := styleTitle
		if strings.Contains(s.SessionName, "@") {
			nameStyle = styleName
		}
		fmt.Printf("%s  %s  %s\n",
			nameStyle.Render(fmt.Sprintf("%-40s", s.SessionName)),
			stateStyled,
			styleTitle.Render(title),
		)
	}

	fmt.Println()
	hint := "run `prism checkin <session>` to inspect a session"
	fmt.Println(lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary)).Render(hint))

	return nil
}

// runCheckinReviewRounds handles `prism checkin <parent>~review` — a prefix
// match that lists all review agent sessions for the parent, grouped by round.
//
// Supports both the new per-agent session shape (PR-C):
//
//	<parent>~review-<N>-<agent>
//
// and old-shape round sessions (pre-PR-C):
//
//	<parent>~review-<N>          (pure integer suffix — old round session)
//	<parent>~review-<N>~<agent>  (old agent sub-session)
//
// Default mode: one summary entry per agent session (last msg_assistant output).
// Verbose mode (--verbose): shows the full per-agent conversation.
func runCheckinReviewRounds(reviewPrefix string, verbose bool) error {
	// reviewPrefix is e.g. "nixos-config@feature~review" — agent sessions
	// are "nixos-config@feature~review-1-review-goal", etc.
	roundPrefix := reviewPrefix + "-"

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("checkin review: open db: %w", err)
	}
	defer d.Close()

	// Find all sessions (active and ended) whose name starts with roundPrefix.
	all, err := d.AllStatusesWithPrefix(roundPrefix)
	if err != nil {
		return fmt.Errorf("checkin review: query db: %w", err)
	}

	if len(all) == 0 {
		fmt.Printf("no review sessions found matching %q\n", reviewPrefix)
		return nil
	}

	// Classify sessions and group by round number.
	// New shape: <prefix>N-<agent>  → roundN = N, label = ~review-N-<agent>
	// Old round: <prefix>N          → roundN = N, label = ~review-N
	// Old agent: <prefix>N~<agent>  → roundN = N, label = ~review-N~<agent>
	type agentEntry struct {
		sessionName string
		label       string
		state       string
	}
	roundAgents := make(map[int][]agentEntry) // round number → agent sessions
	var roundNums []int
	seenRounds := make(map[int]bool)

	for _, s := range all {
		suffix := strings.TrimPrefix(s.SessionName, roundPrefix)
		// suffix examples:
		//   new shape:  "1-review-goal"
		//   old round:  "1"
		//   old agent:  "1~review-goal"

		var roundN int
		var agentLabel string

		if dashIdx := strings.Index(suffix, "-"); dashIdx > 0 {
			// New shape: N-<agent-name> (no ~ in agent part).
			nStr := suffix[:dashIdx]
			n, convErr := strconv.Atoi(nStr)
			if convErr != nil {
				continue // not a recognised shape
			}
			agentPart := suffix[dashIdx+1:]
			if strings.Contains(agentPart, "~") {
				continue // old agent sub-session (N-something with ~)
			}
			roundN = n
			agentLabel = "~review-" + suffix // e.g. ~review-1-review-goal
		} else if tildeIdx := strings.Index(suffix, "~"); tildeIdx > 0 {
			// Old agent sub-session shape: N~<agent>.
			nStr := suffix[:tildeIdx]
			n, convErr := strconv.Atoi(nStr)
			if convErr != nil {
				continue
			}
			roundN = n
			agentLabel = "~review-" + suffix // e.g. ~review-1~review-goal
		} else {
			// Old round session shape: pure integer N.
			n, convErr := strconv.Atoi(suffix)
			if convErr != nil {
				continue
			}
			roundN = n
			agentLabel = "~review-" + suffix // e.g. ~review-1
		}

		if !seenRounds[roundN] {
			seenRounds[roundN] = true
			roundNums = append(roundNums, roundN)
		}
		roundAgents[roundN] = append(roundAgents[roundN], agentEntry{
			sessionName: s.SessionName,
			label:       agentLabel,
			state:       s.State,
		})
	}

	// Sort round numbers ascending.
	for i := 1; i < len(roundNums); i++ {
		key := roundNums[i]
		j := i - 1
		for j >= 0 && roundNums[j] > key {
			roundNums[j+1] = roundNums[j]
			j--
		}
		roundNums[j+1] = key
	}

	styleBold := lipgloss.NewStyle().Bold(true)
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	for _, roundN := range roundNums {
		agents := roundAgents[roundN]

		// Sort agents within the round alphabetically by label.
		for i := 1; i < len(agents); i++ {
			key := agents[i]
			j := i - 1
			for j >= 0 && agents[j].label > key.label {
				agents[j+1] = agents[j]
				j--
			}
			agents[j+1] = key
		}

		fmt.Printf("%s\n\n", styleBold.Render(fmt.Sprintf("round %d (%d session(s))", roundN, len(agents))))

		for _, ag := range agents {
			fmt.Printf("  %s\n", styleBold.Render(ag.label))
			if ag.state != "" {
				stateStyled := stateStyle(ag.state).Render(ag.state)
				fmt.Printf("  state: %s\n", stateStyled)
			}

			if verbose {
				// Show full conversation for this agent session.
				if err := runCheckinSession(ag.sessionName, 20, nil, nil, nil, true); err != nil {
					fmt.Fprintf(os.Stderr, "  [error reading session %s: %v]\n", ag.label, err)
				}
			} else {
				// Show summary: last msg_assistant event.
				events, qerr := d.QueryEvents(ag.sessionName, 1, nil, nil, []string{"msg_assistant"})
				if qerr != nil || len(events) == 0 {
					fmt.Println(styleDim.Render("  (no output recorded)"))
				} else {
					e := events[len(events)-1]
					ts := e.CreatedAt.Local().Format("15:04:05")
					var ap struct {
						Text string `json:"text"`
					}
					text := e.Payload
					if jerr := json.Unmarshal([]byte(e.Payload), &ap); jerr == nil && ap.Text != "" {
						text = ap.Text
					}
					fmt.Printf("  [%s]\n  %s\n", ts, strings.ReplaceAll(text, "\n", "\n  "))
				}
			}
			fmt.Println()
		}

		fmt.Println(styleDim.Render("── end of round ──"))
		fmt.Println()
	}

	return nil
}
