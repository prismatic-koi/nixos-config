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
	"log"
	"os"
	"strings"

	"github.com/spf13/cobra"
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


