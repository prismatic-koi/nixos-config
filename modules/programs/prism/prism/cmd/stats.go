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
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// legacySentinel is the key used to group events that have a NULL harness_session_id.
// These are pre-sidecar "legacy" events that predate opencode session tracking.
const legacySentinel = ""

// modelCosts contains per-million-token pricing for known models.
// Cost is in USD. Keys are "providerID/modelID" exactly as stored in payloads —
// these must match the model IDs emitted by the opencode plugin verbatim.
// Add new entries when new models are configured in opencode.nix.
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
	rootCmd.AddCommand(statsCmd)
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
	repoFilter, _ := cmd.Flags().GetString("repo")
	sinceStr, _ := cmd.Flags().GetString("since")
	forceInstance, _ := cmd.Flags().GetBool("instance")

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
			if err := runStatsDoomLoops(sessionFilter, window); err != nil {
				return err
			}
		}
		if denials {
			if err := runStatsDenials(sessionFilter, window); err != nil {
				return err
			}
		}
		if asks {
			if err := runStatsAsks(sessionFilter, window); err != nil {
				return err
			}
		}
		return nil
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
		return runStatsDetail(args[0], forceInstance)
	}

	// No argument: per-incarnation summary table.
	return runStatsIncarnations(repoFilter, sinceMs)
}
