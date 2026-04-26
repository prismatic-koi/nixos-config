package cmd

// checkin_turns.go — assistant-turn-centric rendering path (the primary default
// view). renderCheckinTurns shows assistant messages interleaved with user
// messages, state_change markers, and tool-call one-liners in chronological order.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
	"github.com/prismatic-koi/prism/internal/tmux"
)

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
