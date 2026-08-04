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
//
// Sub-files:
//
//	checkin_turns.go   — renderCheckinTurns (primary interleaved-turn display path)
//	checkin_raw.go     — renderCheckinEventsRaw, renderChildEvent*, renderProxiedCheckin
//	checkin_review.go  — runCheckinReviewRounds, runCheckinReviewRoundsByGroup
//	checkin_list.go    — runCheckinNoArg, printSessionTable
//	checkin_tools.go   — payload-parsing and result-summary helpers
//	checkin_legacy.go  — runCheckinSessionLegacy (screen-scrape fallback)

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

var checkinCmd = &cobra.Command{
	Use:   "checkin [session]",
	Short: "Show recent conversation events for a session",
	Long: `Show the recent conversation history for a named session, read from the
prism DB. Falls back to tmux screen-scrape for sessions that have no DB rows.

With no argument, lists available sessions for the current repo (use --all
for all repos).

Use --compare <session-a> <session-b> to show a side-by-side narrative view
of two paired A/B test sessions (they must share an abtest_pair_id). Add
--diff to emit a text diff suitable for piping into a viewer.`,
	Args: cobra.MaximumNArgs(2),
	RunE: runCheckin,
}

func init() {
	checkinCmd.Flags().Int("last", 10, "Number of conversation turns to show")
	checkinCmd.Flags().String("from", "", "Show events forward from this event ID")
	checkinCmd.Flags().String("before", "", "Show events backward from this event ID")
	checkinCmd.Flags().String("types", "", "Orthogonal event-type filter (e.g. --types audit, --types state_change). When set, routes to the raw-event path instead of the rich default view.")
	checkinCmd.Flags().BoolP("verbose", "v", false, "Show full tool args/results without truncation (forensic mode)")
	checkinCmd.Flags().Bool("all", false, "List all sessions across all repos (no-arg mode only)")
	checkinCmd.Flags().Bool("compare", false, "Side-by-side comparison of two A/B test sessions (requires two session args)")
	checkinCmd.Flags().Bool("diff", false, "Emit a text diff (use with --compare)")
	checkinCmd.Flags().Bool("json", false, "Emit structured JSON ({\"session\":\"...\",\"state\":\"...\",\"events\":[...]}) instead of the human-readable rendering")
	rootCmd.AddCommand(checkinCmd)
}

func runCheckin(cmd *cobra.Command, args []string) error {
	compare, _ := cmd.Flags().GetBool("compare")
	diffMode, _ := cmd.Flags().GetBool("diff")

	// --compare requires exactly two session arguments.
	if compare || diffMode {
		if len(args) != 2 {
			return fmt.Errorf("checkin --compare requires exactly two session arguments: prism checkin --compare <session-a> <session-b>")
		}
		return runCheckinCompare(args[0], args[1], diffMode)
	}

	if len(args) == 0 {
		showAll, _ := cmd.Flags().GetBool("all")
		return runCheckinNoArg(showAll)
	}

	last, _ := cmd.Flags().GetInt("last")
	fromID, _ := cmd.Flags().GetString("from")
	beforeID, _ := cmd.Flags().GetString("before")
	typesRaw, _ := cmd.Flags().GetString("types")
	verbose, _ := cmd.Flags().GetBool("verbose")
	jsonMode, _ := cmd.Flags().GetBool("json")

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

	// Render a review-group summary only when the user explicitly requests it
	// via the "~review" suffix. A plain "prism checkin <session>" must always
	// show the session's own conversation, even if that session has previously
	// called "prism review" and is registered as a parent_session in
	// session_groups.
	if strings.HasSuffix(sessionArg, "~review") {
		// Strip the "~review" suffix to get the parent session name, then use
		// the DB-backed path. Fall back to the legacy name-prefix scan when no
		// group members are found (pre-migration sessions).
		parentSession := strings.TrimSuffix(sessionArg, "~review")
		if d, dbErr := openDB(); dbErr == nil {
			hasGroup, groupErr := d.HasReviewGroup(parentSession)
			d.Close()
			if groupErr == nil && hasGroup {
				return runCheckinReviewRoundsByGroup(parentSession, verbose)
			}
		}
		// Pre-migration fallback: no DB group found; use the legacy name-prefix scan.
		log.Printf("[deprecation] checkin: no session_groups row for %q — falling back to ~review name heuristic", sessionArg)
		return runCheckinReviewRounds(sessionArg, verbose)
	}

	return runCheckinSession(sessionArg, last, beforePtr, afterPtr, types, verbose, jsonMode)
}

// runCheckinSession is the DB-backed path. Falls back to legacy screen-scrape
// if the DB has no rows for this session.
//
// Permission: the caller's role and repo scope are checked on BOTH routes out
// of this function (issue #2619). A sandboxed caller proxies to the host-API
// `/checkin` endpoint, which applies the tiers server-side. A `host`-mode
// caller takes the direct route below, where authorizeDirectCheckin applies
// the same predicate — see cmd/checkin_permission.go.
//
// When no explicit types are requested, this uses the assistant-turn-centric
// rendering mode: --last N means N assistant turns, not N raw events.
// For each turn it fetches all associated tool/permission/thinking events via
// a secondary query keyed by messageId. msg_user events within the time window
// of the fetched assistant turns are also included.
// jsonMode causes the raw host-API response (or a JSON-encoded local result) to
// be emitted to stdout instead of the human-readable rendering.
func runCheckinSession(session string, limit int, before, after *string, types []string, verbose bool, jsonMode bool) error {
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		raw, err := proxyCheckin(apiURL, session, limit, before, after, types, verbose)
		if err != nil {
			return err
		}
		if jsonMode {
			return printJSON(raw)
		}
		return renderProxiedCheckin(raw, verbose)
	}

	// Direct route: the host-API handler never ran, so apply its permission
	// predicate here. Fails closed — an unresolvable caller, an unreadable DB,
	// or a refusal all return a non-zero exit before any history is read.
	if permErr := authorizeDirectCheckin(session); permErr != nil {
		return permErr
	}

	// When --json is requested on the direct-DB path, query events and emit
	// the same JSON shape as the host-API /checkin endpoint.
	if jsonMode {
		return runCheckinSessionJSON(session, limit, before, after, types)
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

// runCheckinSessionJSON emits the checkin data as JSON in the same shape as
// the host-API /checkin response: {"session":"...","state":"...","events":[...]}.
// Used by the direct-DB path when --json is set, producing byte-identical output
// to the proxy path (both produce the same JSON shape for the same inputs).
func runCheckinSessionJSON(session string, limit int, before, after *string, types []string) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("checkin --json: open db: %w", err)
	}
	defer d.Close()

	// Fetch session state.
	var state string
	if status, stErr := d.CurrentStatus(session); stErr == nil && status != nil {
		state = status.State
	}

	// Fetch events using the same logic as the host-API /checkin handler.
	var events []db.Event
	// assistantCount tracks how many assistant turns were fetched so we can
	// determine whether the result was truncated at the limit.
	assistantCount := 0
	if len(types) > 0 {
		evts, qerr := d.QueryEvents(session, limit, before, after, types)
		if qerr != nil {
			return fmt.Errorf("checkin --json: query events: %w", qerr)
		}
		events = evts
		assistantCount = len(evts)
	} else {
		// Default assistant-turn-centric mode: mirror host-API logic.
		assistantEvents, qerr := d.QueryEvents(session, limit, before, after, []string{"msg_assistant"})
		if qerr != nil {
			return fmt.Errorf("checkin --json: query events: %w", qerr)
		}
		assistantCount = len(assistantEvents)
		if len(assistantEvents) > 0 {
			// Collect messageIds and fetch child events.
			messageIDs := make([]string, 0, len(assistantEvents))
			for _, e := range assistantEvents {
				if msgID := extractMessageID(e.Payload); msgID != "" {
					messageIDs = append(messageIDs, msgID)
				}
			}
			childTypes := []string{"tool_call", "tool_result", "permission_ask", "permission_denied", "thinking"}
			childEvents, _ := d.QueryEventsByMessageIDs(session, messageIDs, childTypes)

			// Fetch user events in time window.
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
			allUserEvents, _ := d.QueryEvents(session, 0, nil, nil, []string{"msg_user"})
			var userEvents []db.Event
			for _, ue := range allUserEvents {
				if !ue.CreatedAt.Before(earliest) && !ue.CreatedAt.After(latest) {
					userEvents = append(userEvents, ue)
				}
			}

			// Merge all into a single sorted timeline (insertion sort, ASC).
			merged := make([]db.Event, 0, len(assistantEvents)+len(childEvents)+len(userEvents))
			merged = append(merged, assistantEvents...)
			merged = append(merged, childEvents...)
			merged = append(merged, userEvents...)
			for i := 1; i < len(merged); i++ {
				for j := i; j > 0 && merged[j].CreatedAt.Before(merged[j-1].CreatedAt); j-- {
					merged[j], merged[j-1] = merged[j-1], merged[j]
				}
			}
			events = merged
		}
	}

	if events == nil {
		events = []db.Event{}
	}

	// Truncation: when assistantCount == limit the DB may have more turns.
	// The agent can page backward via --before=<next_before>.
	truncated := limit > 0 && assistantCount >= limit
	out := map[string]any{
		"session":   session,
		"state":     state,
		"events":    events,
		"truncated": truncated,
	}
	if truncated && len(events) > 0 {
		out["hint"] = "more turns may exist — pass --before=<next_before> to page backward"
		// Find the oldest event ID in the returned window.
		oldest := events[0]
		for _, e := range events[1:] {
			if e.CreatedAt.Before(oldest.CreatedAt) {
				oldest = e
			}
		}
		out["next_before"] = oldest.ID
	}
	data, merr := json.Marshal(out)
	if merr != nil {
		return fmt.Errorf("checkin --json: marshal: %w", merr)
	}
	return printJSON(data)
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
