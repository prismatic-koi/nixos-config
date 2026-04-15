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
//	prism checkin <session> --types <list>    comma-separated event types
//	prism checkin <session> --verbose         full tool args/results (no truncation)
//	prism checkin --all                       (no-arg) list all sessions across all repos

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// childEvent holds a single child event (tool_call, tool_result, permission_ask,
// permission_denied, or thinking) associated with a parent assistant turn.
type childEvent struct {
	eventType string
	payload   string
}

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
	checkinCmd.Flags().String("types", "", "Comma-separated event types to filter. Orthogonal escape hatch for targeted queries (e.g. --types audit). The default rich view already includes state_change, msg_user, msg_assistant, tool_call, tool_result, permission_ask, and permission_denied.")
	checkinCmd.Flags().Bool("verbose", false, "Show full tool args/results without truncation")
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

	return runCheckinSession(args[0], last, beforePtr, afterPtr, types, verbose)
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
// follows them in the timeline.
//
// assistantEvents may be nil (session has events but no assistant turns yet);
// in that case only the header and footer are rendered.
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
	childrenByMsgID := make(map[string][]childEvent)
	if serr == nil {
		for _, e := range secondary {
			msgID := extractMessageID(e.Payload)
			if msgID == "" {
				continue
			}
			childrenByMsgID[msgID] = append(childrenByMsgID[msgID], childEvent{e.Type, e.Payload})
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
	// We use QueryEventsByMessageIDs with no messageID filter — instead we do a
	// plain QueryEvents for msg_user and filter by timestamp in Go.
	userEventsAll, _ := d.QueryEvents(session, 0, nil, nil, []string{"msg_user"})
	var userEventsInWindow []db.Event
	for _, ue := range userEventsAll {
		if !ue.CreatedAt.Before(earliest) && !ue.CreatedAt.After(latest) {
			userEventsInWindow = append(userEventsInWindow, ue)
		}
	}

	// Fetch state_change events within [earliest, latest] and include them in
	// the timeline so state transitions are visible in the default view.
	// Fetch state_change events from [earliest, +∞) with an open upper bound.
	// The terminal state transition (e.g. active → finished) almost always
	// occurs after the last assistant turn and would be excluded by a closed
	// upper bound. Since there are typically only 2–3 state_change events per
	// session, fetching all and filtering by lower bound is inexpensive.
	stateChangeEventsAll, _ := d.QueryEvents(session, 0, nil, nil, []string{"state_change"})
	var stateChangeEventsInWindow []db.Event
	for _, sc := range stateChangeEventsAll {
		if !sc.CreatedAt.Before(earliest) {
			stateChangeEventsInWindow = append(stateChangeEventsInWindow, sc)
		}
	}

	// Merge assistant, user, and state_change events into a single timeline sorted ASC.
	// assistantEvents is already ASC from QueryEvents; userEventsInWindow is also ASC.
	type timelineEntry struct {
		isUser        bool
		isStateChange bool
		event         db.Event
	}
	timeline := make([]timelineEntry, 0, len(assistantEvents)+len(userEventsInWindow)+len(stateChangeEventsInWindow))
	for _, ae := range assistantEvents {
		timeline = append(timeline, timelineEntry{isUser: false, event: ae})
	}
	for _, ue := range userEventsInWindow {
		timeline = append(timeline, timelineEntry{isUser: true, event: ue})
	}
	for _, sc := range stateChangeEventsInWindow {
		timeline = append(timeline, timelineEntry{isStateChange: true, event: sc})
	}
	// Sort by created_at ASC (stable, preserving insertion order for ties).
	for i := 1; i < len(timeline); i++ {
		for j := i; j > 0 && timeline[j].event.CreatedAt.Before(timeline[j-1].event.CreatedAt); j-- {
			timeline[j], timeline[j-1] = timeline[j-1], timeline[j]
		}
	}

	// isSubagentEntry returns true when an entry's agent differs from the root
	// agent, indicating it belongs to a subagent invocation. When rootAgentName
	// is empty (pre-migration sessions), all entries are treated as root-agent
	// entries to preserve current behaviour. state_change entries are never
	// considered subagent entries.
	isSubagentEntry := func(entry timelineEntry) bool {
		if entry.isStateChange {
			return false
		}
		if rootAgentName == "" {
			return false
		}
		var entryAgent string
		if entry.isUser {
			var up payload.MsgUser
			if err := json.Unmarshal([]byte(entry.event.Payload), &up); err == nil {
				entryAgent = up.Agent
			}
		} else {
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

		if isSubagentEntry(entry) && !verbose {
			// Collapse consecutive subagent turns into a single summary line.
			// Count tool calls across all subagent entries in this run and
			// measure the duration from first to last event.
			runStart := entry.event.CreatedAt
			runEnd := entry.event.CreatedAt
			toolCalls := 0
			subagentName := ""

			j := i
			for j < len(timeline) && isSubagentEntry(timeline[j]) {
				e := timeline[j]
				if e.event.CreatedAt.After(runEnd) {
					runEnd = e.event.CreatedAt
				}
				if !e.isUser {
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
		ts := e.CreatedAt.Local().Format("2006-01-02 15:04:05")

		// In verbose mode, prefix subagent lines with an indent marker.
		prefix := ""
		if isSubagentEntry(entry) && verbose {
			prefix = "  │ "
		}

		// State-change entries render inline with a ● marker (session-level, no agent prefix).
		if entry.isStateChange {
			var sc payload.StateChange
			if jerr := json.Unmarshal([]byte(e.Payload), &sc); jerr == nil && sc.State != "" {
				fmt.Printf("[%s] ● %s\n\n", ts, sc.State)
			}
			i++
			continue
		}

		if entry.isUser {
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
		renderChildEventsForTurn(childrenByMsgID[msgID], verbose, prefix)
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

// renderChildEvent prints a single child event (tool call, result, permission,
// or thinking) indented under its parent assistant turn. prefix is prepended
// before the leading spaces (used for verbose subagent indentation).
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
				fmt.Printf("[%s] ▶ user  [%s]\n%s\n\n", ts, label, text)
			} else {
				fmt.Printf("[%s] ▶ user\n%s\n\n", ts, text)
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
			if e.Type == "state_change" {
				var sc payload.StateChange
				if err := json.Unmarshal([]byte(e.Payload), &sc); err == nil && sc.State != "" {
					fmt.Printf("[%s] ● %s\n\n", ts, sc.State)
					continue
				}
			}
			// compaction, error, etc.
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
// It follows the same rendering logic as runCheckinSession but works from
// pre-fetched events rather than the local DB.
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
				fmt.Printf("[%s] ▶ user  [%s]\n%s\n\n", ts, label, text)
			} else {
				fmt.Printf("[%s] ▶ user\n%s\n\n", ts, text)
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

		case "state_change":
			var sc payload.StateChange
			if err := json.Unmarshal([]byte(e.Payload), &sc); err == nil && sc.State != "" {
				fmt.Printf("[%s] ● %s\n\n", ts, sc.State)
			}

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

// renderChildEventsForTurn renders all child events for an assistant turn.
//
// Default mode: tool_call+result pairs are combined into one-liners using
// toolOneLiner. Each tool_call is matched with the first unused tool_result
// for the same tool name that appears after it in the list.
//
// Verbose mode: full args and full result for each event, rendered separately
// using renderChildEvent (existing behaviour).
func renderChildEventsForTurn(children []childEvent, verbose bool, prefix string) {
	if verbose {
		for _, c := range children {
			renderChildEvent(c.eventType, c.payload, verbose, prefix)
		}
		return
	}

	// Default mode: pair tool_calls with results into one-liners.
	used := make([]bool, len(children))
	for i, c := range children {
		if used[i] {
			continue
		}
		switch c.eventType {
		case "tool_call":
			var tc payload.ToolCall
			if err := json.Unmarshal([]byte(c.payload), &tc); err != nil {
				fmt.Printf("%s  → (tool_call parse error)\n", prefix)
				used[i] = true
				continue
			}
			// Find the first matching unused tool_result after this call.
			result := ""
			for j := i + 1; j < len(children); j++ {
				if used[j] || children[j].eventType != "tool_result" {
					continue
				}
				var tr payload.ToolResult
				if err := json.Unmarshal([]byte(children[j].payload), &tr); err != nil {
					continue
				}
				if tr.Tool == tc.Tool {
					result = tr.Result
					used[j] = true
					break
				}
			}
			line := toolOneLiner(tc.Tool, tc.Args, result)
			fmt.Printf("%s  → %s\n", prefix, line)
			used[i] = true
		case "tool_result":
			// Orphaned result (no matching call) — skip in default mode.
			used[i] = true
		default:
			// permission_ask, permission_denied, thinking
			renderChildEvent(c.eventType, c.payload, verbose, prefix)
			used[i] = true
		}
	}
}

// toolOneLiner returns a single-line summary for a tool call + result pair.
// Format: "<tool>: <key_arg> <result_summary>"
func toolOneLiner(toolName, args, result string) string {
	switch toolName {
	case "bash":
		// The sidecar stores bash args as a JSON object {"command":"..."}.
		// Extract the command string; fall back to raw args if not JSON.
		cmd := args
		trimmedArgs := strings.TrimSpace(args)
		if len(trimmedArgs) > 0 && trimmedArgs[0] == '{' {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(trimmedArgs), &obj); err == nil {
				if raw, ok := obj["command"]; ok {
					var s string
					if json.Unmarshal(raw, &s) == nil && s != "" {
						cmd = s
					}
				}
			}
		}
		cmd = truncateRunes(cmd, 80)
		summary := bashResultSummary(result)
		return fmt.Sprintf("bash: %s %s", cmd, summary)

	case "read", "read_file":
		fp := extractToolFilePath(args)
		if result == "" {
			return fmt.Sprintf("%s: %s ✓", toolName, fp)
		}
		n := countResultLines(result)
		return fmt.Sprintf("%s: %s ✓ (%d lines)", toolName, fp, n)

	case "edit", "edit_file":
		fp := extractToolFilePath(args)
		if looksLikeToolError(result) {
			return fmt.Sprintf("%s: %s ✗", toolName, fp)
		}
		return fmt.Sprintf("%s: %s ✓", toolName, fp)

	case "write", "write_file":
		fp := extractToolFilePath(args)
		if looksLikeToolError(result) {
			return fmt.Sprintf("%s: %s ✗", toolName, fp)
		}
		return fmt.Sprintf("%s: %s ✓", toolName, fp)

	case "glob":
		pat := extractFirstArgument(args)
		n := countResultLines(result)
		if n == 0 {
			return fmt.Sprintf("glob: %s no matches", pat)
		}
		return fmt.Sprintf("glob: %s %d matches", pat, n)

	case "grep", "ripgrep":
		pat := extractFirstArgument(args)
		n := countResultLines(result)
		if n == 0 {
			return fmt.Sprintf("grep: %s no matches", pat)
		}
		return fmt.Sprintf("grep: %s %d matches", pat, n)

	case "task":
		desc := truncateRunes(args, 60)
		if looksLikeToolError(result) {
			return fmt.Sprintf("task: %s ✗", desc)
		}
		return fmt.Sprintf("task: %s ✓", desc)

	case "todowrite", "todo_write":
		return "todowrite ✓"

	default:
		keyArg := truncateRunes(args, 60)
		if result == "" {
			return fmt.Sprintf("%s: %s ✓", toolName, keyArg)
		}
		first := firstMeaningfulLine(result, 80)
		if first == "" {
			return fmt.Sprintf("%s: %s ✓", toolName, keyArg)
		}
		return fmt.Sprintf("%s: %s — %s", toolName, keyArg, first)
	}
}

// truncateRunes truncates s to at most n runes, appending "..." if truncated.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// bashResultSummary returns the result summary portion for a bash one-liner.
// Returns "✓" for empty output; "— <first line>" for non-empty output.
func bashResultSummary(result string) string {
	result = strings.TrimSpace(result)
	if result == "" {
		return "✓"
	}
	line := firstMeaningfulLine(result, 80)
	if line == "" {
		return "✓"
	}
	return "— " + line
}

// firstMeaningfulLine returns the first non-empty, non-whitespace line of s,
// truncated to maxLen runes. Returns "" if no such line exists.
func firstMeaningfulLine(s string, maxLen int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return truncateRunes(line, maxLen)
		}
	}
	return ""
}

// extractToolFilePath extracts a file path from tool args.
// Tries JSON parsing first (for {filePath:...} shapes), falls back to raw string.
// Returns the base filename component for brevity.
func extractToolFilePath(args string) string {
	args = strings.TrimSpace(args)
	if len(args) > 0 && args[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(args), &obj); err == nil {
			for _, key := range []string{"filePath", "file_path", "path", "filename"} {
				if raw, ok := obj[key]; ok {
					var s string
					if err := json.Unmarshal(raw, &s); err == nil && s != "" {
						return filepath.Base(s)
					}
				}
			}
		}
	}
	if args == "" {
		return args
	}
	return filepath.Base(args)
}

// extractFirstArgument extracts the primary argument (pattern, query, etc.)
// from tool args. Tries JSON parsing first, falls back to the raw string.
func extractFirstArgument(args string) string {
	args = strings.TrimSpace(args)
	if len(args) > 0 && args[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(args), &obj); err == nil {
			for _, key := range []string{"pattern", "query", "glob", "regex", "include"} {
				if raw, ok := obj[key]; ok {
					var s string
					if err := json.Unmarshal(raw, &s); err == nil && s != "" {
						return truncateRunes(s, 60)
					}
				}
			}
		}
	}
	return truncateRunes(args, 60)
}

// countResultLines counts non-empty lines in the result string.
func countResultLines(result string) int {
	result = strings.TrimSpace(result)
	if result == "" {
		return 0
	}
	n := 0
	for _, line := range strings.Split(result, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// looksLikeToolError returns true when a tool result appears to indicate failure.
// It checks for error/failed/exception as a line prefix, not anywhere in the text,
// to avoid false positives from output that merely mentions these words
// (e.g. "Ran 42 tests, 0 failed." or "No errors found.").
func looksLikeToolError(result string) bool {
	if result == "" {
		return false
	}
	for _, line := range strings.Split(result, "\n") {
		l := strings.TrimSpace(strings.ToLower(line))
		if l == "" {
			continue
		}
		if l == "error" || strings.HasPrefix(l, "error:") || strings.HasPrefix(l, "error ") ||
			l == "failed" || strings.HasPrefix(l, "failed:") || strings.HasPrefix(l, "failed ") ||
			l == "exception" || strings.HasPrefix(l, "exception:") || strings.HasPrefix(l, "exception ") {
			return true
		}
	}
	return false
}
