package cmd

// prism stats — session observability and metrics reporting.
//
// Usage:
//
//	prism stats                   summary table of all active sessions across all repos
//	prism stats <session>         per-session detail
//	prism stats --days N          historical aggregate over the last N days
//	prism stats model             per-model performance breakdown over last 7 days
//	prism stats model --days N    per-model performance breakdown over last N days
//	prism stats --doomloops       doom_loop_detected events from the last 7 days
//	prism stats --doomloops --days N  doom loop events over the last N days
//	prism stats <session> --doomloops filter doom loop events to a specific session
//	prism stats --denials         permission_denied events from the last 7 days
//	prism stats --denials --days N  permission denied events over the last N days
//	prism stats <session> --denials filter permission denied events to a specific session
//	prism stats --asks            permission_ask events from the last 7 days
//	prism stats --asks --days N   permission ask events over the last N days
//	prism stats <session> --asks  filter permission ask events to a specific session

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

// legacySentinel is the key used to group events that have a NULL harness_session_id.
// These are pre-sidecar "legacy" events that predate agent session tracking.
const legacySentinel = ""

// modelCosts contains per-million-token pricing for known models.
// Cost is in USD. Keys are "providerID/modelID" exactly as stored in payloads —
// these must match the model IDs emitted by the pi plugin verbatim.
// Add new entries when new models are configured.
var modelCosts = map[string]struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}{
	// Anthropic direct models (hyphens as version separators).
	"anthropic/claude-sonnet-4-6": {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
	"anthropic/claude-opus-4-6":   {Input: 15.0, Output: 75.0, CacheRead: 1.50, CacheWrite: 18.75},
	"anthropic/claude-haiku-4-5":  {Input: 0.80, Output: 4.0, CacheRead: 0.08, CacheWrite: 1.00},
	// GitHub Copilot models (dots as version separators — different from Anthropic direct).
	"github-copilot/claude-sonnet-4.6": {Input: 3.0, Output: 15.0, CacheRead: 0.30, CacheWrite: 3.75},
	"github-copilot/claude-opus-4.6":   {Input: 15.0, Output: 75.0, CacheRead: 1.50, CacheWrite: 18.75},
	"github-copilot/claude-haiku-4.5":  {Input: 0.80, Output: 4.0, CacheRead: 0.08, CacheWrite: 1.00},
	// Google Gemini models.
	"google/gemini-3-flash-preview":        {Input: 0.15, Output: 0.60, CacheRead: 0.0375, CacheWrite: 0},
	"google/gemini-3.1-flash-lite-preview": {Input: 0.075, Output: 0.30, CacheRead: 0.01875, CacheWrite: 0},
}

var statsCmd = &cobra.Command{
	Use:   "stats [instance-id|session-name]",
	Short: "Session metrics and statistics",
	Long: `Display metrics and statistics for agent session incarnations.

With no arguments, shows one row per incarnation in the sessions table,
ordered by started_at DESC (most recent first).

With an argument that is a full 36-character UUID (or an unambiguous prefix),
shows detail for the matching incarnation. Use --instance to force UUID lookup
even when the argument might also match a session name.

With a session-name argument, shows detail for the most recent incarnation of
that session name.

Filter flags:
  --repo <name>     only show incarnations where sessions.repo matches
  --since <date>    only show incarnations started on or after <date>
                    (ISO 8601 or YYYY-MM-DD, e.g. 2026-04-01)

Use --doomloops to show doom_loop_detected events. Defaults to the last 7 days
cross-session; combine with --days N to change the window; combine with a
session name argument to filter to a specific session.

Use --denials to show permission_denied events aggregated by (session, tool).
Defaults to the last 7 days cross-session; combine with --days N to change the
window; combine with a session name argument to filter to a specific session.

Use --asks to show permission_ask events aggregated by (session, tool, pattern).
Defaults to the last 7 days cross-session; combine with --days N to change the
window; combine with a session name argument to filter to a specific session.

Use --days N to show historical aggregate statistics over the last N days.

Use the 'model' subcommand for a per-provider/model performance breakdown.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runStats,
}

func init() {
	statsCmd.Flags().Int("days", 0, "Show aggregate statistics over the last N days (historical view)")
	statsCmd.Flags().Bool("detail", false, "Force detailed block format even for multi-session tmux sessions")
	statsCmd.Flags().Bool("doomloops", false, "Show doom_loop_detected events (last 7 days by default)")
	statsCmd.Flags().Bool("denials", false, "Show permission_denied events aggregated by (session, tool) (last 7 days by default)")
	statsCmd.Flags().Bool("asks", false, "Show permission_ask events aggregated by (session, tool, pattern) (last 7 days by default)")
	statsCmd.Flags().String("repo", "", "Filter rows to those where sessions.repo equals this value")
	statsCmd.Flags().String("since", "", "Filter rows to those started on or after this date (ISO 8601 or YYYY-MM-DD)")
	statsCmd.Flags().Bool("instance", false, "Treat the argument as a full instance_id (UUID) even if it might match a session name")
	statsCmd.Flags().String("group-by", "", "Render a breakdown table grouped by this axis: harness, profile, variant, model")
	statsCmd.Flags().Bool("abtest", false, "List all A/B test pairs with summary metrics")
	statsCmd.Flags().Bool("json", false, "Emit structured JSON to stdout instead of the human-readable table. The shape mirrors the host-API response for the same view.")
	rootCmd.AddCommand(statsCmd)
}

func init() {
	modelCmd.Flags().Int("days", 7, "Number of days to include (default 7)")
	statsCmd.AddCommand(modelCmd)
}

// parseSinceFlag parses the --since flag value into a Unix millisecond timestamp.
// Returns (0, nil) when since is empty. Returns an error when unparseable.
func parseSinceFlag(since string) (int64, error) {
	if since == "" {
		return 0, nil
	}
	// Try common date formats.
	formats := []string{
		"2006-01-02",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		time.RFC3339,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, since); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("cannot parse --since value %q: expected ISO 8601 date (e.g. 2026-04-01)", since)
}

func runStats(cmd *cobra.Command, args []string) error {
	days, _ := cmd.Flags().GetInt("days")
	doomloops, _ := cmd.Flags().GetBool("doomloops")
	denials, _ := cmd.Flags().GetBool("denials")
	asks, _ := cmd.Flags().GetBool("asks")
	abtest, _ := cmd.Flags().GetBool("abtest")
	repoFilter, _ := cmd.Flags().GetString("repo")
	sinceStr, _ := cmd.Flags().GetString("since")
	forceInstance, _ := cmd.Flags().GetBool("instance")
	groupBy, _ := cmd.Flags().GetString("group-by")
	jsonMode, _ := cmd.Flags().GetBool("json")

	// --abtest: list all A/B test pairs with summary metrics. runStatsAbtestFlag
	// performs its own PRISM_HOST_API proxy dispatch (issue #2098) so a
	// sandboxed session lists pairs from the host DB rather than the empty
	// shadow DB. The jsonMode arg honours the prism-wide --json convention
	// (issue #2099 Bug 2): on success the function emits a single JSON
	// document mirroring the /stats?view=abtest_list shape.
	if abtest {
		return runStatsAbtestFlag(jsonMode)
	}

	// Validate --group-by early so we fail fast on bad input.
	if groupBy != "" {
		validAxes := []string{"harness", "profile", "variant", "model"}
		valid := false
		for _, a := range validAxes {
			if groupBy == a {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("unknown --group-by value %q; valid axes: harness, profile, variant, model", groupBy)
		}
	}

	// PRISM_HOST_API proxy dispatch: when running inside a container, proxy
	// read operations to the host sidecar rather than opening the shadow DB.
	//
	// Two cases fall through to the local DB path even inside a container:
	//   - --group-by <axis>: breakdown table that aggregates across sessions;
	//     not a sandbox visibility concern (coordinators run on the host).
	//   - --days N with no event flags and no session arg: maps to
	//     runStatsHistorical, which aggregates events differently from the
	//     per-incarnation summary view returned by /stats?view=summary.
	// --abtest has already returned above.
	// --instance on the proxy path is forwarded as part of the session arg
	// (the session string is passed verbatim to view=detail).
	historical := days > 0 && !doomloops && !denials && !asks && len(args) == 0
	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" && groupBy == "" && !historical {
		return runStatsProxy(cmd, args, apiURL, days, doomloops, denials, asks, repoFilter, sinceStr, jsonMode)
	}

	// --doomloops, --denials, and --asks bypass the per-incarnation view.
	// They each have their own session-filter path and --days is additive.
	if doomloops || denials || asks {
		// Default window is 7 days; --days N overrides it.
		window := 7
		if days > 0 {
			window = days
		}
		sessionFilter := ""
		if len(args) == 1 {
			sessionFilter = args[0]
		}

		if doomloops {
			if err := runStatsDoomLoops(sessionFilter, window, jsonMode); err != nil {
				return err
			}
		}
		if denials {
			if err := runStatsDenials(sessionFilter, window, jsonMode); err != nil {
				return err
			}
		}
		if asks {
			if err := runStatsAsks(sessionFilter, window, jsonMode); err != nil {
				return err
			}
		}
		return nil
	}

	// --group-by: breakdown table grouped by the given axis.
	if groupBy != "" {
		// Compute sinceMs from --days or --since for filtering.
		var sinceMs int64
		if days > 0 {
			sinceMs = time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
		} else if sinceStr != "" {
			ms, err := parseSinceFlag(sinceStr)
			if err != nil {
				return err
			}
			sinceMs = ms
		}
		return runStatsGroupBy(groupBy, sinceMs)
	}

	// --days: historical aggregate view.
	if days > 0 && len(args) > 0 {
		return fmt.Errorf("--days is mutually exclusive with a session name")
	}
	if days > 0 {
		return runStatsHistorical(days)
	}

	// Parse --since before doing anything else so we fail-fast on bad input.
	sinceMs, err := parseSinceFlag(sinceStr)
	if err != nil {
		return err
	}

	// With an argument: detail view for a specific incarnation or session name.
	if len(args) == 1 {
		return runStatsDetail(args[0], forceInstance, jsonMode)
	}

	// No argument: per-incarnation summary table.
	return runStatsIncarnations(repoFilter, sinceMs, jsonMode)
}

// runStatsProxy handles the PRISM_HOST_API proxy path for prism stats.
// It dispatches to the host sidecar's /stats endpoint and renders the result
// using the same functions as the direct-DB path, keeping rendering on the
// CLI side (byte-identical output between host and proxy paths).
func runStatsProxy(cmd *cobra.Command, args []string, apiURL string, days int, doomloops, denials, asks bool, repoFilter, sinceStr string, jsonMode bool) error {
	// Default window for event views.
	window := 7
	if days > 0 {
		window = days
	}
	sessionFilter := ""
	if len(args) == 1 {
		sessionFilter = args[0]
	}

	if doomloops || denials || asks {
		if doomloops {
			raw, err := proxyStats(apiURL, "doomloops", sessionFilter, window, "", 0)
			if err != nil {
				return err
			}
			var resp struct {
				Events []db.Event `json:"events"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return fmt.Errorf("stats proxy: unmarshal doomloops response: %w", err)
			}
			if jsonMode {
				return printJSON(raw)
			}
			renderStatsDoomLoops(sessionFilter, window, resp.Events)
		}
		if denials {
			raw, err := proxyStats(apiURL, "denials", sessionFilter, window, "", 0)
			if err != nil {
				return err
			}
			var resp struct {
				Events []db.Event `json:"events"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return fmt.Errorf("stats proxy: unmarshal denials response: %w", err)
			}
			if jsonMode {
				return printJSON(raw)
			}
			renderStatsDenials(sessionFilter, window, resp.Events)
		}
		if asks {
			raw, err := proxyStats(apiURL, "asks", sessionFilter, window, "", 0)
			if err != nil {
				return err
			}
			var resp struct {
				Events []db.Event `json:"events"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return fmt.Errorf("stats proxy: unmarshal asks response: %w", err)
			}
			if jsonMode {
				return printJSON(raw)
			}
			renderStatsAsks(sessionFilter, window, resp.Events)
		}
		return nil
	}

	// Per-session detail view.
	if sessionFilter != "" {
		raw, err := proxyStats(apiURL, "detail", sessionFilter, 0, "", 0)
		if err != nil {
			return err
		}
		var resp struct {
			Session *db.Session `json:"session"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("stats proxy: unmarshal detail response: %w", err)
		}
		if jsonMode {
			return printJSON(raw)
		}
		if resp.Session == nil {
			return fmt.Errorf("stats: %q not found", sessionFilter)
		}
		// Render the session detail using the incarnation renderer.
		// We need a DB handle for token lookup on the direct path, but for the
		// proxy path we render the sessions fields only (token data not proxied).
		renderIncarnationDetailFromSession(resp.Session)
		return nil
	}

	// Summary view (no session, no event flag).
	// Parse sinceMs for the query.
	var sinceMs int64
	if sinceStr != "" {
		ms, err := parseSinceFlag(sinceStr)
		if err != nil {
			return err
		}
		sinceMs = ms
	} else if days > 0 {
		sinceMs = time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	}
	raw, err := proxyStats(apiURL, "summary", "", 0, repoFilter, sinceMs)
	if err != nil {
		return err
	}
	var resp struct {
		Sessions []db.Session `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("stats proxy: unmarshal summary response: %w", err)
	}
	if jsonMode {
		return printJSON(raw)
	}
	return renderStatsIncarnationsFromSessions(resp.Sessions)
}

// printJSON writes raw JSON to stdout followed by a newline. Used by all
// --json paths to emit structured output.
func printJSON(raw []byte) error {
	_, err := fmt.Fprintln(os.Stdout, string(raw))
	return err
}
