package main

// checkin.go — `iris checkin <session>` narrative-view CLI subcommand.
//
// This is the iris equivalent of `prism checkin <session>`. It renders a
// session's recent events as interleaved assistant turns, tool one-liners,
// and state changes — the canonical "what did this session just do" view.
//
// # Data source
//
// Reads iris.db directly via internal/db. Read-only DB queries do not need
// to route through the daemon socket (the all-surfaces-go-through-daemon
// rule from #1668/#1669 is about *write* operations and shared mutable
// state). This carve-out is documented in issue #1676.
//
// # Session resolution
//
// Accepts either a full session_name OR a 12-char-or-longer unique
// instance_id prefix. The prefix path mirrors the form used by
// `iris sessions list`'s SESSION column (the first 12 chars of the UUID).
// Ambiguous prefixes return a clear error listing all candidates.
//
// # Output
//
// Default human-readable view: assistant-turn-centric — for each fetched
// msg_assistant event we render the turn header + body, then interleave its
// child tool_call / tool_result / permission / thinking events as one-liner
// indented lines beneath. msg_user and state_change events within the
// window are interleaved chronologically.
//
// --json: emits a single JSON array of event objects with stable field
// names ({id, session_name, type, payload, created_at, ...}). Empty
// session → "[]\n".
//
// --verbose: turns off the per-tool truncation and renders raw args/result
// instead of the one-liner summary.
//
// --from / --before: forward / backward pagination from an event ID.
// Mutually exclusive — passing both is a usage error.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/narrative"
	"github.com/prismatic-koi/prism/internal/payload"
)

// checkinCmd implements `iris checkin <session>`.
var checkinCmd = &cobra.Command{
	Use:   "checkin <session>",
	Short: "Show recent conversation events for an iris session (narrative view)",
	Long: `Render the recent conversation history of a single iris session as a
narrative view — assistant turns, tool one-liners, and state changes
interleaved chronologically.

The session argument may be either:
  - the full session_name (e.g. iris-test@feature-x), OR
  - a 12-char-or-longer prefix of the session's instance_id (UUID).

When a prefix matches multiple sessions the command fails with a list of
candidates so the operator can disambiguate.

This subcommand reads iris.db directly (via the internal/db package). It
does not require the iris daemon to be running. If the DB file is missing
or unreadable, the command exits non-zero with an error pointing at the
DB path under ~/.local/state/iris/iris.db.`,
	Args:          cobra.ExactArgs(1),
	RunE:          runCheckin,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	checkinCmd.Flags().Int("last", 10, "Number of assistant turns to show (use 0 to print only the state line)")
	checkinCmd.Flags().BoolP("verbose", "v", false, "Show full tool args and results with no truncation")
	checkinCmd.Flags().Bool("json", false, "Emit a JSON array of agent_events rows instead of the narrative view")
	checkinCmd.Flags().String("from", "", "Show events forward from this event ID (mutually exclusive with --before)")
	checkinCmd.Flags().String("before", "", "Show events backward from this event ID (mutually exclusive with --from)")
	rootCmd.AddCommand(checkinCmd)
}

// runCheckin is the cobra RunE for `iris checkin <session>`.
//
// Flow:
//
//  1. Validate flag combinations (--from / --before mutually exclusive).
//  2. Open the iris DB (read-only; the file is shared with the daemon but
//     SQLite handles concurrent readers).
//  3. Resolve the session argument to a canonical session_name.
//  4. Fetch the session's current state.
//  5. Fetch events and either emit JSON or render the narrative view.
func runCheckin(cmd *cobra.Command, args []string) error {
	last, _ := cmd.Flags().GetInt("last")
	verbose, _ := cmd.Flags().GetBool("verbose")
	jsonMode, _ := cmd.Flags().GetBool("json")
	fromID, _ := cmd.Flags().GetString("from")
	beforeID, _ := cmd.Flags().GetString("before")

	if fromID != "" && beforeID != "" {
		return fmt.Errorf("iris checkin: --from and --before are mutually exclusive")
	}
	if last < 0 {
		return fmt.Errorf("iris checkin: --last must be ≥ 0, got %d", last)
	}

	dbPath := iris.ResolvePaths().DB
	d, err := openIrisDBForRead(dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	canonicalName, err := resolveSession(d, args[0])
	if err != nil {
		return err
	}

	var beforePtr, afterPtr *string
	if beforeID != "" {
		beforePtr = &beforeID
	}
	if fromID != "" {
		afterPtr = &fromID
	}

	if jsonMode {
		return runCheckinJSON(cmd.OutOrStdout(), d, canonicalName, last, beforePtr, afterPtr)
	}
	return runCheckinNarrative(cmd.OutOrStdout(), d, canonicalName, last, beforePtr, afterPtr, verbose)
}

// openIrisDBForRead opens the iris DB and wraps the "missing/unreadable"
// failure mode with an operator-friendly message that points at both the
// DB path and the daemon (which would normally create the file).
func openIrisDBForRead(dbPath string) (*db.DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("iris checkin: iris database not found at %s; has the iris daemon ever run? (start it with `systemctl --user start iris`)", dbPath)
		}
		// Permission denied etc. — surface verbatim so the operator sees
		// the syscall-level cause.
		return nil, fmt.Errorf("iris checkin: cannot read iris database at %s: %w", dbPath, err)
	}
	d, err := iris.OpenDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("iris checkin: open iris database at %s: %w", dbPath, err)
	}
	return d, nil
}

// minPrefixLen is the minimum length of an instance_id prefix accepted by
// resolveSession. 12 chars mirrors `iris sessions list`'s SESSION column
// and gives ~48 bits of entropy — practically no chance of collision
// across a user's session history. Shorter inputs are still considered
// candidates for the session_name path (exact match) but never for the
// instance_id-prefix path.
const minPrefixLen = 12

// resolveSession resolves a user-supplied session argument to a canonical
// session_name. The argument may be:
//
//   - An exact session_name match (preferred — sessions table has it).
//   - A 12-char-or-longer prefix of an instance_id.
//
// Resolution order: exact session_name first (so a literal name always
// wins, even if it happens to be 12 chars and could collide with a UUID
// prefix). On no name match, fall back to instance_id-prefix lookup.
//
// Errors:
//
//   - "no such session: <arg>" — neither path matched.
//   - "ambiguous session prefix" — prefix matched multiple sessions;
//     the message lists the candidates (short id, name, role, started_at)
//     so the operator can re-run with a longer prefix or the full name.
func resolveSession(d *db.DB, arg string) (string, error) {
	if arg == "" {
		return "", fmt.Errorf("iris checkin: empty session argument")
	}

	// Exact session_name first. SessionsByName returns rows in started_at
	// DESC order, so the first row is the most recent incarnation. A name
	// hit always wins over a prefix hit (see comment above).
	byName, err := d.SessionsByName(arg)
	if err != nil {
		return "", fmt.Errorf("iris checkin: lookup session by name: %w", err)
	}
	if len(byName) > 0 {
		return byName[0].SessionName, nil
	}

	// Fall back to instance_id-prefix lookup, but only when the input is
	// long enough to give meaningful entropy.
	if len(arg) < minPrefixLen {
		return "", fmt.Errorf("iris checkin: no such session: %q (a 12-char-or-longer instance_id prefix is required for UUID lookup)", arg)
	}

	byPrefix, err := d.SessionsByInstanceIDPrefix(arg)
	if err != nil {
		return "", fmt.Errorf("iris checkin: lookup session by prefix: %w", err)
	}
	if len(byPrefix) == 0 {
		return "", fmt.Errorf("iris checkin: no such session: %q", arg)
	}
	if len(byPrefix) > 1 {
		var b strings.Builder
		fmt.Fprintf(&b, "iris checkin: ambiguous session prefix %q matches %d sessions:\n", arg, len(byPrefix))
		for _, s := range byPrefix {
			role := ""
			if s.AgentRole != nil {
				role = *s.AgentRole
			}
			fmt.Fprintf(&b, "  %s  %-32s  %-12s  started %s\n",
				shortID(s.InstanceID), s.SessionName, role,
				s.StartedAt.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "re-run with a longer prefix or the full session_name.")
		return "", errors.New(b.String())
	}
	return byPrefix[0].SessionName, nil
}

// shortID returns the 12-char display form of an instance_id. Mirrors the
// truncation used by `iris sessions list`'s SESSION column.
func shortID(id string) string {
	if len(id) >= 12 {
		return id[:12]
	}
	return id
}

// runCheckinNarrative emits the human-readable narrative view to w.
//
// The structure mirrors prism's renderCheckinTurns:
//
//  1. Print the "checkin: <session>" / "state: <state>" header.
//  2. Fetch the last N msg_assistant events as the primary turn list
//     (or use --from / --before windows). N==0 → header + footer only.
//  3. Fetch child events (tool_call, tool_result, permission_*, thinking)
//     keyed by messageId.
//  4. Fetch msg_user and state_change events within the assistant-turn
//     time window so they can be interleaved chronologically.
//  5. Render each timeline entry, pairing tool_call ↔ tool_result by
//     messageId so the result summary appears inline beneath the call.
func runCheckinNarrative(w io.Writer, d *db.DB, session string, last int, before, after *string, verbose bool) error {
	state := "(unknown)"
	if status, err := d.CurrentStatus(session); err == nil && status != nil {
		state = status.State
	}

	fmt.Fprintf(w, "checkin: %s\n\n", session)
	fmt.Fprintf(w, "state: %s\n\n", state)

	// last==0 — header + footer only, no body.
	if last == 0 {
		fmt.Fprintln(w, "── end of event log ──")
		return nil
	}

	assistantEvents, err := d.QueryEvents(session, last, before, after, []string{"msg_assistant"})
	if err != nil {
		return fmt.Errorf("iris checkin: query assistant events: %w", err)
	}
	if len(assistantEvents) == 0 {
		// Session may still have events of other types (e.g. user prompt
		// queued before any assistant turn). Surface a brief explanatory
		// line so the operator doesn't think the session is broken.
		fmt.Fprintln(w, "(no assistant turns yet)")
		fmt.Fprintln(w, "── end of event log ──")
		return nil
	}

	// Collect messageIds for the secondary child query.
	messageIDs := make([]string, 0, len(assistantEvents))
	for _, e := range assistantEvents {
		if mid := narrative.ExtractMessageID(e.Payload); mid != "" {
			messageIDs = append(messageIDs, mid)
		}
	}
	childTypes := []string{"tool_call", "tool_result", "permission_ask", "permission_denied", "thinking"}
	children, _ := d.QueryEventsByMessageIDs(session, messageIDs, childTypes)

	// Bucket children by messageId for fast lookup at render time.
	childrenByMsgID := make(map[string][]childItem, len(messageIDs))
	for _, e := range children {
		mid := narrative.ExtractMessageID(e.Payload)
		if mid == "" {
			continue
		}
		childrenByMsgID[mid] = append(childrenByMsgID[mid], childItem{eventType: e.Type, payload: e.Payload})
	}

	// Time window spanned by the fetched assistant turns. We pull
	// msg_user and state_change events within this window so they
	// interleave chronologically.
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

	userEventsAll, _ := d.QueryEvents(session, 0, nil, nil, []string{"msg_user"})
	var userEventsInWindow []db.Event
	for _, ue := range userEventsAll {
		if !ue.CreatedAt.Before(earliest) && !ue.CreatedAt.After(latest) {
			userEventsInWindow = append(userEventsInWindow, ue)
		}
	}
	stateChangeAll, _ := d.QueryEvents(session, 0, nil, nil, []string{"state_change"})
	var stateChangeInWindow []db.Event
	for _, se := range stateChangeAll {
		if !se.CreatedAt.Before(earliest) && !se.CreatedAt.After(latest) {
			stateChangeInWindow = append(stateChangeInWindow, se)
		}
	}

	// Build the merged chronological timeline.
	const (
		kindAssistant   = 0
		kindUser        = 1
		kindStateChange = 2
	)
	type timelineEntry struct {
		kind  int
		event db.Event
	}
	timeline := make([]timelineEntry, 0, len(assistantEvents)+len(userEventsInWindow)+len(stateChangeInWindow))
	for _, ae := range assistantEvents {
		timeline = append(timeline, timelineEntry{kind: kindAssistant, event: ae})
	}
	for _, ue := range userEventsInWindow {
		timeline = append(timeline, timelineEntry{kind: kindUser, event: ue})
	}
	for _, se := range stateChangeInWindow {
		timeline = append(timeline, timelineEntry{kind: kindStateChange, event: se})
	}
	sort.SliceStable(timeline, func(i, j int) bool {
		return timeline[i].event.CreatedAt.Before(timeline[j].event.CreatedAt)
	})

	// Render.
	for _, entry := range timeline {
		ts := entry.event.CreatedAt.Local().Format("15:04:05")
		switch entry.kind {
		case kindStateChange:
			var sc payload.StateChange
			if jerr := json.Unmarshal([]byte(entry.event.Payload), &sc); jerr != nil {
				sc.State = entry.event.Payload
			}
			s := sc.State
			if s == "" {
				s = "(unknown)"
			}
			fmt.Fprintf(w, "[%s] ● %s\n\n", ts, s)
		case kindUser:
			var up payload.MsgUser
			if jerr := json.Unmarshal([]byte(entry.event.Payload), &up); jerr != nil {
				up.Text = entry.event.Payload
			}
			text := up.Text
			if text == "" {
				text = "(no text)"
			}
			label := narrative.TurnLabel(up.Agent, up.Model)
			if label != "" {
				fmt.Fprintf(w, "[%s] ▶ user  [%s]\n", ts, label)
			} else {
				fmt.Fprintf(w, "[%s] ▶ user\n", ts)
			}
			fmt.Fprintf(w, "%s\n\n", text)
		case kindAssistant:
			var ap payload.MsgAssistant
			if jerr := json.Unmarshal([]byte(entry.event.Payload), &ap); jerr != nil {
				ap.Text = entry.event.Payload
			}
			label := narrative.TurnLabel(ap.Agent, ap.Model)
			if label != "" {
				fmt.Fprintf(w, "[%s] assistant  [%s]\n", ts, label)
			} else {
				fmt.Fprintf(w, "[%s] assistant\n", ts)
			}
			atext := ap.Text
			if atext == "" {
				atext = "(no text)"
			}
			fmt.Fprintln(w, atext)

			mid := narrative.ExtractMessageID(entry.event.Payload)
			renderChildEvents(w, childrenByMsgID[mid], verbose)
			fmt.Fprintln(w)
		}
	}

	fmt.Fprintln(w, "── end of event log ──")
	return nil
}

// childItem is a tool/permission/thinking event bucketed under its parent
// msg_assistant's messageId for rendering.
type childItem struct {
	eventType string
	payload   string
}

// renderChildEvents writes the tool / permission / thinking events
// associated with a single assistant turn beneath its body. tool_call ↔
// tool_result are paired in-line via MessageID; orphan results render
// standalone.
//
// verbose=false produces the one-liner form (the default --help output
// shows this). verbose=true dumps raw args and full result text so the
// operator can copy-paste reproducers.
func renderChildEvents(w io.Writer, children []childItem, verbose bool) {
	// First pass: collect tool_result payloads by messageId so we can
	// fold them into their matching tool_call rendering.
	resultByMsgID := make(map[string]payload.ToolResult, len(children))
	for _, c := range children {
		if c.eventType != "tool_result" {
			continue
		}
		var tr payload.ToolResult
		if err := json.Unmarshal([]byte(c.payload), &tr); err != nil {
			continue
		}
		if tr.MessageID != "" {
			resultByMsgID[tr.MessageID] = tr
		}
	}

	seenResultMsgIDs := make(map[string]bool, len(resultByMsgID))

	for _, c := range children {
		switch c.eventType {
		case "tool_call":
			var tc payload.ToolCall
			if err := json.Unmarshal([]byte(c.payload), &tc); err != nil {
				fmt.Fprintln(w, "  → tool_call (parse error)")
				continue
			}
			if verbose {
				fmt.Fprintf(w, "  → %s\n", tc.Tool)
				fmt.Fprintf(w, "    args: %s\n", tc.Args)
				if tr, ok := resultByMsgID[tc.MessageID]; ok {
					fmt.Fprintf(w, "    result: %s\n", tr.Result)
					seenResultMsgIDs[tc.MessageID] = true
				}
				continue
			}
			key := narrative.ToolKeyArg(tc.Tool, tc.Args)
			if tr, ok := resultByMsgID[tc.MessageID]; ok {
				summary := narrative.ToolResultSummary(tc.Tool, tr.Result)
				if key != "" {
					fmt.Fprintf(w, "  → %s: %s %s\n", tc.Tool, key, summary)
				} else {
					fmt.Fprintf(w, "  → %s %s\n", tc.Tool, summary)
				}
				seenResultMsgIDs[tc.MessageID] = true
			} else {
				// No result yet (in-flight) — render the call alone.
				if key != "" {
					fmt.Fprintf(w, "  → %s: %s\n", tc.Tool, key)
				} else {
					fmt.Fprintf(w, "  → %s\n", tc.Tool)
				}
			}
		case "tool_result":
			// Already folded into its tool_call above; skip unless orphaned.
			var tr payload.ToolResult
			if err := json.Unmarshal([]byte(c.payload), &tr); err != nil {
				continue
			}
			if seenResultMsgIDs[tr.MessageID] {
				continue
			}
			if verbose {
				fmt.Fprintf(w, "  ↳ result (orphan): %s\n", tr.Result)
				continue
			}
			fmt.Fprintf(w, "  ↳ result (orphan): %s\n", narrative.ToolResultSummary(tr.Tool, tr.Result))
		case "permission_ask":
			var pa payload.PermissionAsk
			if err := json.Unmarshal([]byte(c.payload), &pa); err != nil {
				fmt.Fprintln(w, "  ⚠ permission ask")
				continue
			}
			tool := string(pa.Tool)
			if tool == "" {
				tool = "unknown"
			}
			fmt.Fprintf(w, "  ⚠ permission: %s\n", tool)
		case "permission_denied":
			var pd payload.PermissionDenied
			if err := json.Unmarshal([]byte(c.payload), &pd); err != nil {
				fmt.Fprintln(w, "  ✗ permission denied")
				continue
			}
			tool := pd.Tool
			if tool == "" {
				tool = "unknown"
			}
			fmt.Fprintf(w, "  ✗ permission denied: %s\n", tool)
		case "thinking":
			fmt.Fprintln(w, "  · thinking…")
		}
	}
}

// runCheckinJSON emits a single JSON array of agent_events rows for the
// session, using the --last / --from / --before window. Empty session →
// "[]\n". Field names match the agent_events schema (and are stable; the
// AC fixes them as a scripting contract).
//
// Unlike the narrative view, JSON returns ALL events in the window
// (assistant + user + state_change + tool + permission + thinking), not
// just the assistant-turn subset. Scripts that want to count tool calls
// or filter by type should not have to make a second query to get them.
func runCheckinJSON(w io.Writer, d *db.DB, session string, last int, before, after *string) error {
	// last==0 produces an empty array (no events fetched). The state is
	// not part of the JSON contract — the array shape matches prism's
	// /checkin endpoint's "events" subfield for portability.
	if last == 0 {
		_, err := fmt.Fprintln(w, "[]")
		return err
	}

	// Fetch the assistant-turn window first to anchor the time range.
	assistantEvents, err := d.QueryEvents(session, last, before, after, []string{"msg_assistant"})
	if err != nil {
		return fmt.Errorf("iris checkin --json: query assistant events: %w", err)
	}

	var events []db.Event
	if len(assistantEvents) == 0 {
		events = []db.Event{}
	} else {
		// Pull all event types within the assistant-turn window. We use a
		// time-window filter rather than a second SQL query because the
		// helper API exposes type filtering but not arbitrary timestamp
		// ranges — this keeps the changeset minimal.
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
		all, qerr := d.QueryEvents(session, 0, nil, nil, nil)
		if qerr != nil {
			return fmt.Errorf("iris checkin --json: query all events: %w", qerr)
		}
		for _, e := range all {
			if !e.CreatedAt.Before(earliest) && !e.CreatedAt.After(latest) {
				events = append(events, e)
			}
		}
		// Re-sort ASC (QueryEvents already does, but be defensive after
		// the filter loop).
		sort.SliceStable(events, func(i, j int) bool {
			return events[i].CreatedAt.Before(events[j].CreatedAt)
		})
	}

	// Project to the stable JSON shape.
	rows := make([]checkinJSONEvent, 0, len(events))
	for _, e := range events {
		rows = append(rows, eventToJSON(e))
	}
	data, merr := json.MarshalIndent(rows, "", "  ")
	if merr != nil {
		return fmt.Errorf("iris checkin --json: marshal: %w", merr)
	}
	_, werr := fmt.Fprintln(w, string(data))
	return werr
}

// checkinJSONEvent is the stable JSON shape emitted by `iris checkin --json`.
// Field names mirror the agent_events SQL columns so a downstream consumer
// can correlate this output against a direct DB query.
//
// Adding fields is fine; renames and removals are breaking changes.
type checkinJSONEvent struct {
	ID               string `json:"id"`
	SessionName      string `json:"session_name"`
	Repo             string `json:"repo"`
	Worktree         string `json:"worktree"`
	HarnessSessionID string `json:"harness_session_id,omitempty"`
	InstanceID       string `json:"instance_id,omitempty"`
	Type             string `json:"type"`
	Payload          string `json:"payload"`
	CreatedAt        string `json:"created_at"` // RFC3339
}

func eventToJSON(e db.Event) checkinJSONEvent {
	out := checkinJSONEvent{
		ID:          e.ID,
		SessionName: e.SessionName,
		Repo:        e.Repo,
		Worktree:    e.Worktree,
		Type:        e.Type,
		Payload:     e.Payload,
		CreatedAt:   e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if e.HarnessSessionID != nil {
		out.HarnessSessionID = *e.HarnessSessionID
	}
	if e.InstanceID != nil {
		out.InstanceID = *e.InstanceID
	}
	return out
}
