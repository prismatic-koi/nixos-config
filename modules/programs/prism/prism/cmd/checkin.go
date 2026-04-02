package cmd

// prism checkin — show recent conversation events for a session from DB,
// falling back to tmux screen-scrape when the session has no DB rows.
//
// Usage:
//
//	prism checkin [<session>]
//	prism checkin <session> --last N          show last N events (default 10)
//	prism checkin <session> --from <id>       show N events forward from event ID
//	prism checkin <session> --before <id>     show N events backward from event ID
//	prism checkin <session> --types <list>    comma-separated event types
//	prism checkin <session> --verbose         full tool args/results (no truncation)
//	prism checkin --all                       (no-arg) list all sessions across all repos

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
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
	checkinCmd.Flags().Int("last", 10, "Number of events to show")
	checkinCmd.Flags().String("from", "", "Show events forward from this event ID")
	checkinCmd.Flags().String("before", "", "Show events backward from this event ID")
	checkinCmd.Flags().String("types", "", "Comma-separated event types to filter (default includes msg_user, msg_assistant, tool_call, tool_result, permission_ask, permission_denied)")
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

	// Parse types; nil means "use default" (msg_user, msg_assistant).
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
func runCheckinSession(session string, limit int, before, after *string, types []string, verbose bool) error {
	// Default event types when none explicitly requested.
	// The spec describes the user-facing default as ["msg_user","msg_assistant"],
	// but inline rendering of tool calls and permission prompts under assistant
	// messages requires those types to also be present in the query result.
	// We therefore extend the internal default to include them. When --types is
	// set explicitly by the user, only the requested types are fetched (tool_call
	// etc. are excluded unless specified, so inline rendering is suppressed).
	queryTypes := types
	if len(queryTypes) == 0 {
		queryTypes = []string{"msg_user", "msg_assistant", "tool_call", "tool_result", "permission_ask", "permission_denied"}
	}

	d, err := openDB()
	if err == nil {
		defer d.Close()
		events, qerr := d.QueryEvents(session, limit, before, after, queryTypes)
		if qerr == nil && len(events) > 0 {
			return renderCheckinEvents(session, d, events, verbose)
		}
	}

	// No DB rows (or DB unavailable) — fall back to screen capture.
	return runCheckinSessionLegacy(session, 100)
}

// renderCheckinEvents prints the DB-sourced event log to stdout.
func renderCheckinEvents(session string, d *db.DB, events []db.Event, verbose bool) error {
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
		state = "idle"
	}

	fmt.Printf("checkin: %s\n\n", session)
	fmt.Printf("state: %s\n\n", state)

	// Build a map from messageId → inline events (tool_call, tool_result,
	// permission_ask, permission_denied) so that we can render them under
	// the correct msg_assistant row regardless of event ordering in the DB.
	// The plugin writes msg_assistant only when message.updated fires with
	// completed != null, which happens AFTER tool_call/permission_ask rows
	// have already been inserted — so proximity-based association is wrong.
	type inlineEvent struct {
		eventType string
		payload   string
	}
	inlineByMsgID := make(map[string][]inlineEvent)
	for _, e := range events {
		switch e.Type {
		case "tool_call", "tool_result", "permission_ask", "permission_denied":
			msgID := payloadMessageID(e.Payload)
			if msgID != "" {
				inlineByMsgID[msgID] = append(inlineByMsgID[msgID], inlineEvent{e.Type, e.Payload})
			}
		}
	}

	// Track which messageIds actually have a msg_assistant event in the
	// result set. Tool/permission events are only suppressed (rendered
	// inline under their parent assistant message) when that parent is
	// present. Without this, --last N returning only tail tool events
	// would suppress everything and produce empty output.
	assistantMsgIDs := make(map[string]bool)
	for _, e := range events {
		if e.Type == "msg_assistant" {
			msgID := payloadMessageID(e.Payload)
			if msgID != "" {
				assistantMsgIDs[msgID] = true
			}
		}
	}

	for _, e := range events {
		ts := e.CreatedAt.Local().Format("2006-01-02 15:04:05")

		switch e.Type {
		case "msg_user":
			text := payloadText(e.Payload)
			fmt.Printf("[%s] user\n%s\n\n", ts, text)

		case "msg_assistant":
			text := payloadText(e.Payload)
			fmt.Printf("[%s] assistant\n%s\n", ts, text)

			// Render tool_call / tool_result / permission_ask / permission_denied
			// events that belong to this message, looked up by messageId.
			msgID := payloadMessageID(e.Payload)
			if msgID != "" {
				for _, ie := range inlineByMsgID[msgID] {
					switch ie.eventType {
					case "tool_call":
						tool, args := payloadToolCall(ie.payload)
						if !verbose && len(args) > 80 {
							args = args[:80] + "..."
						}
						fmt.Printf("  → %s: %s\n", tool, args)
					case "tool_result":
						result := payloadToolResult(ie.payload)
						if !verbose && len(result) > 80 {
							result = result[:80] + "..."
						}
						fmt.Printf("  → result: %s\n", result)
					case "permission_ask":
						tool, patterns := payloadPermissionAsk(ie.payload)
						fmt.Printf("  [⏳ waiting for approval: %s — %s]\n", tool, patterns)
					case "permission_denied":
						tool := payloadPermissionDeniedTool(ie.payload)
						fmt.Printf("  [❌ denied: %s]\n", tool)
					}
				}
			}
			fmt.Println()

		case "tool_call":
			// Standalone tool_call — skip if its parent msg_assistant is in the
			// result set (it will be rendered inline under that message).
			msgID := payloadMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			tool, args := payloadToolCall(e.Payload)
			if !verbose && len(args) > 80 {
				args = args[:80] + "..."
			}
			fmt.Printf("  → %s: %s\n", tool, args)

		case "tool_result":
			msgID := payloadMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			result := payloadToolResult(e.Payload)
			if !verbose && len(result) > 80 {
				result = result[:80] + "..."
			}
			fmt.Printf("  → result: %s\n", result)

		case "permission_ask":
			msgID := payloadMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			tool, patterns := payloadPermissionAsk(e.Payload)
			fmt.Printf("[⏳ waiting for approval: %s — %s]\n", tool, patterns)

		case "permission_denied":
			msgID := payloadMessageID(e.Payload)
			if msgID != "" && assistantMsgIDs[msgID] {
				continue
			}
			tool := payloadPermissionDeniedTool(e.Payload)
			fmt.Printf("[❌ denied: %s]\n", tool)

		default:
			// state_change, compaction, error, etc. — shown when explicitly
			// included via --types.
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
		state = "idle"
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
			state = "idle"
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
			state = "idle"
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

// --- payload parsing helpers ---

// payloadText extracts the "text" field from a msg_user / msg_assistant payload.
// Falls back to "(message <messageId>)" if text is absent.
func payloadText(raw string) string {
	var p struct {
		Text      string `json:"text"`
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return raw
	}
	if p.Text != "" {
		return p.Text
	}
	if p.MessageID != "" {
		return fmt.Sprintf("(message %s)", p.MessageID)
	}
	return raw
}

// payloadMessageID returns the messageId from a payload, or empty string.
func payloadMessageID(raw string) string {
	var p struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ""
	}
	return p.MessageID
}

// payloadToolCall extracts tool name and args from a tool_call payload.
func payloadToolCall(raw string) (tool, args string) {
	var p struct {
		Tool string `json:"tool"`
		Args string `json:"args"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "unknown", raw
	}
	return p.Tool, p.Args
}

// payloadToolResult extracts the result string from a tool_result payload.
func payloadToolResult(raw string) string {
	var p struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return raw
	}
	return p.Result
}

// payloadPermissionAsk extracts tool and patterns from a permission_ask payload.
func payloadPermissionAsk(raw string) (tool, patterns string) {
	var p struct {
		Tool     string   `json:"tool"`
		Patterns []string `json:"patterns"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "unknown", raw
	}
	return p.Tool, strings.Join(p.Patterns, ", ")
}

// payloadPermissionDeniedTool extracts the tool name from a permission_denied payload.
func payloadPermissionDeniedTool(raw string) string {
	var p struct {
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "unknown"
	}
	if p.Tool == "" {
		return "unknown"
	}
	return p.Tool
}
