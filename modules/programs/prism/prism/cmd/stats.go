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
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
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

// ---------- per-incarnation summary table ----------

// runStatsIncarnations shows one row per row in the sessions table, ordered by
// started_at DESC. Filtered by repo and/or sinceMs when provided.
func runStatsIncarnations(repoFilter string, sinceMs int64) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	defer d.Close()

	var sessions []db.Session
	switch {
	case repoFilter != "" && sinceMs > 0:
		sessions, err = d.SessionsForRepoSince(repoFilter, sinceMs)
	case repoFilter != "":
		sessions, err = d.SessionsForRepo(repoFilter)
	case sinceMs > 0:
		sessions, err = d.SessionsSince(sinceMs)
	default:
		sessions, err = d.AllSessions()
	}
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}

	if len(sessions) == 0 {
		fmt.Println("no sessions yet")
		return nil
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	now := time.Now()

	const (
		wID      = 8
		wName    = 32
		wAgent   = 14
		wState   = 10
		wDur     = 10
		wTokens  = 8
		wCost    = 8
	)

	header := fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %-*s  %*s  %*s",
		wID, "INSTANCE",
		wName, "SESSION_NAME",
		wAgent, "AGENT",
		wState, "STATE",
		wDur, "DURATION",
		wTokens, "TOKENS",
		wCost, "COST",
	)
	fmt.Println(styleHeader.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header))))

	for _, s := range sessions {
		shortID := s.InstanceID
		if len(shortID) > wID {
			shortID = shortID[:wID]
		}

		sessionName := s.SessionName
		if len(sessionName) > wName {
			sessionName = sessionName[:wName-3] + "..."
		}

		agentName := "—"
		if s.RootAgentName != nil && *s.RootAgentName != "" {
			agentName = agentShortName(*s.RootAgentName)
		} else if s.AgentRole != nil && *s.AgentRole != "" {
			agentName = agentShortName(*s.AgentRole)
		}
		if len(agentName) > wAgent {
			agentName = agentName[:wAgent-3] + "..."
		}

		state := "active"
		if s.EndState != nil && *s.EndState != "" {
			state = *s.EndState
		} else if s.EndedAt != nil {
			state = "ended"
		}

		var dur time.Duration
		if s.EndedAt != nil {
			dur = s.EndedAt.Sub(s.StartedAt)
		} else {
			dur = now.Sub(s.StartedAt)
		}
		durStr := formatDurationLong(dur)

		// Compute token/cost totals from agent_events for this instance_id.
		turns, terr := d.SessionTurnTokens(s.InstanceID)
		var totalTokens int
		var totalCost float64
		if terr == nil {
			for _, t := range turns {
				totalTokens += t.Input + t.Output
				totalCost += computeTurnCost(t)
			}
		}

		tokStr := "—"
		if totalTokens > 0 {
			tokStr = formatTokenCount(totalTokens)
		}
		costStr := "—"
		if totalCost > 0 {
			costStr = formatCost(totalCost)
		}

		stateStyled := stateStyle(state).Render(fmt.Sprintf("%-*s", wState, truncateStr(state, wState)))

		fmt.Printf("%-*s  %-*s  %-*s  %s  %-*s  %*s  %*s\n",
			wID, shortID,
			wName, sessionName,
			wAgent, agentName,
			stateStyled,
			wDur, durStr,
			wTokens, tokStr,
			wCost, costStr,
		)
	}

	fmt.Println()
	fmt.Println(styleDim.Render("run `prism stats <instance-id>` or `prism stats <session-name>` for detail"))
	return nil
}

// computeTurnCost computes the cost for a single msg_assistant turn.
// Uses the local pricing table when the model is known; falls back to
// the event-reported cost for unknown models (e.g. openrouter/*).
func computeTurnCost(t db.TokenTurn) float64 {
	costs, ok := modelCosts[t.Model]
	if !ok {
		return t.EventCost
	}
	return (float64(t.Input)*costs.Input +
		float64(t.Output)*costs.Output +
		float64(t.CacheRead)*costs.CacheRead +
		float64(t.CacheWrite)*costs.CacheWrite) / 1_000_000
}

// ---------- per-incarnation detail view ----------

// runStatsDetail resolves an argument to a specific sessions row and renders
// detail for it. The argument may be a full UUID, a UUID prefix (when
// unambiguous), or a session name.
func runStatsDetail(arg string, forceInstance bool) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	defer d.Close()

	sess, err := resolveSessionArg(d, arg, forceInstance)
	if err != nil {
		return err
	}

	renderIncarnationDetail(d, sess)
	return nil
}

// resolveSessionArg resolves an argument to a single sessions row.
// Disambiguation rules (from issue #999):
//  1. Full 36-char UUID (or --instance flag) → SessionByInstanceID
//  2. Exact match in sessions.session_name → MostRecentSessionForName
//  3. UUID prefix → SessionsByInstanceIDPrefix (must be unambiguous)
//  4. Not found → error
func resolveSessionArg(d *db.DB, arg string, forceInstance bool) (*db.Session, error) {
	// Step 1: full UUID (36 chars) or --instance flag.
	if forceInstance || len(arg) == 36 {
		sess, err := d.SessionByInstanceID(arg)
		if err != nil {
			return nil, fmt.Errorf("stats: lookup instance %q: %w", arg, err)
		}
		if sess != nil {
			return sess, nil
		}
		return nil, fmt.Errorf("stats: instance %q not found", arg)
	}

	// Step 2: try exact session_name match first.
	sess, err := d.MostRecentSessionForName(arg)
	if err != nil {
		return nil, fmt.Errorf("stats: lookup session name %q: %w", arg, err)
	}
	if sess != nil {
		return sess, nil
	}

	// Step 3: try UUID prefix match.
	matches, err := d.SessionsByInstanceIDPrefix(arg)
	if err != nil {
		return nil, fmt.Errorf("stats: lookup instance prefix %q: %w", arg, err)
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	if len(matches) > 1 {
		var candidates []string
		for _, m := range matches {
			candidates = append(candidates, m.InstanceID)
		}
		return nil, fmt.Errorf("stats: %q is ambiguous — multiple incarnations match:\n  %s\nuse the full instance_id to disambiguate",
			arg, strings.Join(candidates, "\n  "))
	}

	return nil, fmt.Errorf("stats: %q is not a known instance_id or session_name", arg)
}

// renderIncarnationDetail renders the full detail block for a single sessions row.
func renderIncarnationDetail(d *db.DB, sess *db.Session) {
	styleHeader := lipgloss.NewStyle().Bold(true)
	styleLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	now := time.Now()

	fmt.Println(styleHeader.Render("incarnation: " + sess.InstanceID))
	fmt.Println()

	fmt.Printf("%s %s\n", styleLabel.Render("session:"), sess.SessionName)
	fmt.Printf("%s %s\n", styleLabel.Render("repo:"), sess.Repo)

	if sess.RootAgentName != nil && *sess.RootAgentName != "" {
		fmt.Printf("%s %s\n", styleLabel.Render("agent:"), *sess.RootAgentName)
	} else if sess.AgentRole != nil && *sess.AgentRole != "" {
		fmt.Printf("%s %s\n", styleLabel.Render("agent:"), *sess.AgentRole)
	}

	state := "active"
	if sess.EndState != nil && *sess.EndState != "" {
		state = *sess.EndState
	} else if sess.EndedAt != nil {
		state = "ended"
	}
	fmt.Printf("%s %s\n", styleLabel.Render("state:"), stateStyle(state).Render(state))

	fmt.Printf("%s %s\n", styleLabel.Render("started:"), sess.StartedAt.Format("2006-01-02 15:04:05"))
	if sess.EndedAt != nil {
		fmt.Printf("%s %s\n", styleLabel.Render("ended:"), sess.EndedAt.Format("2006-01-02 15:04:05"))
		dur := sess.EndedAt.Sub(sess.StartedAt)
		fmt.Printf("%s %s\n", styleLabel.Render("duration:"), formatDurationLong(dur))
	} else {
		dur := now.Sub(sess.StartedAt)
		fmt.Printf("%s %s\n", styleLabel.Render("duration:"), formatDurationLong(dur))
	}

	// Archive path.
	if sess.ArchivePath != nil && *sess.ArchivePath != "" {
		fmt.Printf("%s %s\n", styleLabel.Render("archive:"), *sess.ArchivePath)
	} else {
		fmt.Printf("%s %s\n", styleLabel.Render("archive:"), styleDim.Render("(not yet archived)"))
	}
	fmt.Println()

	// Token/cost totals from agent_events.
	fmt.Println(styleHeader.Render("Token Usage"))
	turns, err := d.SessionTurnTokens(sess.InstanceID)
	if err != nil || len(turns) == 0 {
		fmt.Println(styleDim.Render("  no token data (pre-migration events excluded)"))
	} else {
		var totalInput, totalOutput, totalCacheRead, totalCacheWrite int
		var totalCost float64
		for _, t := range turns {
			totalInput += t.Input
			totalOutput += t.Output
			totalCacheRead += t.CacheRead
			totalCacheWrite += t.CacheWrite
			totalCost += computeTurnCost(t)
		}
		if totalInput+totalOutput > 0 {
			fmt.Printf("  %s %s\n", styleLabel.Render("input:"), formatTokenCount(totalInput))
			fmt.Printf("  %s %s\n", styleLabel.Render("output:"), formatTokenCount(totalOutput))
			if totalCacheRead > 0 {
				fmt.Printf("  %s %s\n", styleLabel.Render("cache read:"), formatTokenCount(totalCacheRead))
			}
			if totalCacheWrite > 0 {
				fmt.Printf("  %s %s\n", styleLabel.Render("cache write:"), formatTokenCount(totalCacheWrite))
			}
			if totalCost > 0 {
				fmt.Printf("  %s %s\n", styleLabel.Render("est. cost:"), formatCost(totalCost))
			}
		} else {
			fmt.Println(styleDim.Render("  no token data"))
		}
	}
}

// ---------- legacy per-session detail (kept for --doomloops/denials/asks) ----------

// sessionMetrics holds aggregated metrics for a single opencode session.
type sessionMetrics struct {
	SessionName string
	HarnessSessionID string // empty string = legacy sentinel (NULL harness_session_id)
	AgentName   string
	ModelID     string
	State       string

	// Duration from first to last event.
	FirstEvent time.Time
	LastEvent  time.Time

	// Token totals.
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int

	// EventCost is the sum of per-turn costs reported directly in the opencode
	// SSE event payload (MsgAssistant.Cost). It is used as a fallback when the
	// model is not present in the local modelCosts pricing table — for example,
	// openrouter/* models that are billed at rates not known to this client.
	EventCost float64

	// Turn metrics.
	AssistantTurns int
	UserTurns      int
	TurnDurations  []time.Duration // assistant turn durations
	PeakContextPct float64

	// Tool metrics.
	ToolCalls     map[string]int             // tool name → count
	ToolDurations map[string][]time.Duration // tool name → durations

	// Compaction count.
	Compactions int

	// Subagent invocations.
	SubagentInvocations []subagentInvocation
}

// subagentInvocation records a single subagent lifecycle.
type subagentInvocation struct {
	Agent    string
	Duration time.Duration
}

func (m *sessionMetrics) totalCost() float64 {
	model := m.ModelID
	costs, ok := modelCosts[model]
	if !ok {
		// Model not in local pricing table — fall back to the cost reported
		// directly in the event payload (e.g. openrouter/* models).
		return m.EventCost
	}
	return (float64(m.InputTokens)*costs.Input +
		float64(m.OutputTokens)*costs.Output +
		float64(m.CacheReadTokens)*costs.CacheRead +
		float64(m.CacheWriteTokens)*costs.CacheWrite) / 1_000_000
}

func (m *sessionMetrics) duration() time.Duration {
	if m.FirstEvent.IsZero() || m.LastEvent.IsZero() {
		return 0
	}
	return m.LastEvent.Sub(m.FirstEvent)
}

func (m *sessionMetrics) avgTurnDuration() time.Duration {
	if len(m.TurnDurations) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range m.TurnDurations {
		total += d
	}
	return total / time.Duration(len(m.TurnDurations))
}

func (m *sessionMetrics) longestTurnDuration() time.Duration {
	var longest time.Duration
	for _, d := range m.TurnDurations {
		if d > longest {
			longest = d
		}
	}
	return longest
}

func (m *sessionMetrics) totalToolCalls() int {
	n := 0
	for _, c := range m.ToolCalls {
		n += c
	}
	return n
}

// isLegacy reports whether this sessionMetrics represents the legacy NULL-sid
// sentinel group.
func (m *sessionMetrics) isLegacy() bool {
	return m.HarnessSessionID == legacySentinel
}

// collectMetrics builds a sessionMetrics from a slice of events that all belong
// to the same harness session (or the legacy sentinel group). The harnessSessionID
// parameter is the key used for this group (legacySentinel for NULL-sid events).
//
// The model field is set to the coordinator/root agent's model if one is present
// (agent field == "coordinator"), falling back to the most-frequently-seen model.
// This gives a more meaningful "session model" than first-seen.
func collectMetrics(events []db.Event, harnessSessionID string) *sessionMetrics {
	m := &sessionMetrics{
		HarnessSessionID: harnessSessionID,
		ToolCalls:     make(map[string]int),
		ToolDurations: make(map[string][]time.Duration),
	}

	// Track model turn counts and coordinator model for model selection.
	modelTurnCounts := make(map[string]int)
	var coordinatorModel string

	for _, e := range events {
		if m.SessionName == "" {
			m.SessionName = e.SessionName
		}
		if m.FirstEvent.IsZero() || e.CreatedAt.Before(m.FirstEvent) {
			m.FirstEvent = e.CreatedAt
		}
		if e.CreatedAt.After(m.LastEvent) {
			m.LastEvent = e.CreatedAt
		}

		switch e.Type {
		case "msg_assistant":
			var p payload.MsgAssistant
			if err := json.Unmarshal([]byte(e.Payload), &p); err == nil {
				m.AssistantTurns++
				m.InputTokens += p.InputTokens
				m.OutputTokens += p.OutputTokens
				m.CacheReadTokens += p.CacheReadTokens
				m.CacheWriteTokens += p.CacheWriteTokens
				m.EventCost += p.Cost
				if p.DurationMs > 0 {
					m.TurnDurations = append(m.TurnDurations, time.Duration(p.DurationMs)*time.Millisecond)
				}
				if p.ContextWindowPct > m.PeakContextPct {
					m.PeakContextPct = p.ContextWindowPct
				}
				if m.AgentName == "" && p.Agent != "" {
					m.AgentName = p.Agent
				}
				if p.Model != "" {
					modelTurnCounts[p.Model]++
					// Prefer coordinator model: take the first coordinator model seen.
					if p.Agent == "coordinator" && coordinatorModel == "" {
						coordinatorModel = p.Model
					}
				}
			}

		case "msg_user":
			m.UserTurns++

		case "tool_call":
			var p payload.ToolCall
			if err := json.Unmarshal([]byte(e.Payload), &p); err == nil {
				m.ToolCalls[p.Tool]++
				if p.DurationMs > 0 {
					m.ToolDurations[p.Tool] = append(m.ToolDurations[p.Tool], time.Duration(p.DurationMs)*time.Millisecond)
				}
			}

		case "compaction":
			m.Compactions++

		case "subagent_end":
			var p payload.SubagentEnd
			if err := json.Unmarshal([]byte(e.Payload), &p); err == nil {
				dur := time.Duration(0)
				if p.DurationMs > 0 {
					dur = time.Duration(p.DurationMs) * time.Millisecond
				}
				m.SubagentInvocations = append(m.SubagentInvocations, subagentInvocation{
					Agent:    p.Agent,
					Duration: dur,
				})
			}
		}
	}

	// Determine the best model: coordinator model takes priority, then
	// fall back to the most-frequently-seen model.
	if coordinatorModel != "" {
		m.ModelID = coordinatorModel
	} else {
		// Pick most frequent model.
		var bestModel string
		var bestCount int
		for model, count := range modelTurnCounts {
			if count > bestCount || (count == bestCount && model < bestModel) {
				bestModel = model
				bestCount = count
			}
		}
		m.ModelID = bestModel
	}

	return m
}

// groupEventsByHarnessSessionID partitions events by harness_session_id, preserving
// insertion order for the first occurrence of each key.
// Events with a NULL harness_session_id are grouped under legacySentinel ("").
// Returns the grouped map and the ordered list of keys.
func groupEventsByHarnessSessionID(events []db.Event) (map[string][]db.Event, []string) {
	grouped := make(map[string][]db.Event)
	var order []string

	for _, e := range events {
		key := legacySentinel
		if e.HarnessSessionID != nil {
			key = *e.HarnessSessionID
		}
		if _, exists := grouped[key]; !exists {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], e)
	}

	return grouped, order
}

func runStatsSession(session string, detail bool) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	defer d.Close()

	// Get status info.
	status, _ := d.CurrentStatus(session)

	// Get all events for this session.
	events, err := d.AllSessionEvents(session)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}

	if len(events) == 0 {
		if status == nil {
			return fmt.Errorf("session %q not found", session)
		}
		fmt.Printf("session: %s\n", session)
		fmt.Printf("state:   %s\n", status.State)
		fmt.Println("\nno metrics data available")
		return nil
	}

	// Group events by harness_session_id.
	grouped, order := groupEventsByHarnessSessionID(events)

	// Build a sessionMetrics per opencode session.
	var allMetrics []*sessionMetrics
	for _, key := range order {
		m := collectMetrics(grouped[key], key)
		allMetrics = append(allMetrics, m)
	}

	// Apply live status (state, agent name) to the last non-legacy session only.
	// Older sessions within the same tmux session should not inherit the current
	// live state — they are historical and completed.
	if status != nil {
		for i := len(allMetrics) - 1; i >= 0; i-- {
			if !allMetrics[i].isLegacy() {
				allMetrics[i].State = status.State
				if status.RootAgentName != nil && *status.RootAgentName != "" {
					allMetrics[i].AgentName = *status.RootAgentName
				}
				break
			}
		}
	}

	// Determine rendering mode:
	// - Exactly one opencode session, no legacy group → detailed block
	// - Only legacy group (all NULL-sid events), no real sessions → detailed block labelled legacy
	// - Multiple opencode sessions, OR mixed legacy+real, OR --detail flag → compact table or forced detail
	nonLegacyCount := 0
	hasLegacy := false
	for _, key := range order {
		if key == legacySentinel {
			hasLegacy = true
		} else {
			nonLegacyCount++
		}
	}

	// Use detailed block only for pure single-session cases (no mixing with legacy).
	if !detail && nonLegacyCount == 1 && !hasLegacy {
		// Exactly one opencode session, no legacy data — detailed block format.
		for _, m := range allMetrics {
			if !m.isLegacy() {
				renderSessionDetail(m)
				return nil
			}
		}
	}

	if !detail && nonLegacyCount == 0 && hasLegacy {
		// Only legacy events (all NULL-sid) — render as detailed block with legacy label.
		if len(allMetrics) == 1 {
			m := allMetrics[0]
			// Mark model as legacy if there's no model info.
			if m.ModelID == "" {
				m.ModelID = "(legacy)"
			}
			m.AgentName = "(legacy, pre-sidecar)"
			renderSessionDetail(m)
			return nil
		}
	}

	// Multi-session (or --detail forced): render compact table.
	renderSessionCompactTable(session, allMetrics, status, detail)
	return nil
}

// renderSessionDetail renders the detailed block format for a single opencode session.
func renderSessionDetail(m *sessionMetrics) {
	styleHeader := lipgloss.NewStyle().Bold(true)
	styleLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	fmt.Println(styleHeader.Render("session: " + m.SessionName))
	fmt.Println()

	// Agent and model.
	if m.AgentName != "" {
		fmt.Printf("%s %s\n", styleLabel.Render("agent:"), m.AgentName)
	}
	if m.ModelID != "" {
		fmt.Printf("%s %s\n", styleLabel.Render("model:"), m.ModelID)
	}
	if m.State != "" {
		fmt.Printf("%s %s\n", styleLabel.Render("state:"), stateStyle(m.State).Render(m.State))
	}
	fmt.Printf("%s %s\n", styleLabel.Render("duration:"), formatDurationLong(m.duration()))
	fmt.Println()

	// Token usage.
	fmt.Println(styleHeader.Render("Token Usage"))
	totalTokens := m.InputTokens + m.OutputTokens
	if totalTokens > 0 {
		fmt.Printf("  %s %s\n", styleLabel.Render("input:"), formatTokenCount(m.InputTokens))
		fmt.Printf("  %s %s\n", styleLabel.Render("output:"), formatTokenCount(m.OutputTokens))
		if m.CacheReadTokens > 0 {
			fmt.Printf("  %s %s\n", styleLabel.Render("cache read:"), formatTokenCount(m.CacheReadTokens))
		}
		if m.CacheWriteTokens > 0 {
			fmt.Printf("  %s %s\n", styleLabel.Render("cache write:"), formatTokenCount(m.CacheWriteTokens))
		}
		cost := m.totalCost()
		if cost > 0 {
			fmt.Printf("  %s %s\n", styleLabel.Render("est. cost:"), formatCost(cost))
		}
	} else {
		fmt.Println(styleDim.Render("  no token data"))
	}
	fmt.Println()

	// Turn metrics.
	fmt.Println(styleHeader.Render("Turns"))
	fmt.Printf("  %s %d user, %d assistant\n", styleLabel.Render("count:"), m.UserTurns, m.AssistantTurns)
	if len(m.TurnDurations) > 0 {
		fmt.Printf("  %s %s\n", styleLabel.Render("avg turn:"), formatDurationLong(m.avgTurnDuration()))
		fmt.Printf("  %s %s\n", styleLabel.Render("longest:"), formatDurationLong(m.longestTurnDuration()))
	}
	if m.PeakContextPct > 0 {
		fmt.Printf("  %s %.1f%%\n", styleLabel.Render("peak context:"), m.PeakContextPct)
	}
	if m.Compactions > 0 {
		fmt.Printf("  %s %d\n", styleLabel.Render("compactions:"), m.Compactions)
	}
	fmt.Println()

	// Tool breakdown.
	fmt.Println(styleHeader.Render("Tool Calls"))
	if m.totalToolCalls() > 0 {
		// Sort tools by count descending.
		type toolEntry struct {
			name  string
			count int
		}
		var tools []toolEntry
		for name, count := range m.ToolCalls {
			tools = append(tools, toolEntry{name, count})
		}
		sort.Slice(tools, func(i, j int) bool { return tools[i].count > tools[j].count })

		for _, t := range tools {
			avgStr := ""
			if durations, ok := m.ToolDurations[t.name]; ok && len(durations) > 0 {
				var total time.Duration
				for _, d := range durations {
					total += d
				}
				avg := total / time.Duration(len(durations))
				avgStr = fmt.Sprintf("  avg %s", formatDurationLong(avg))
			}
			fmt.Printf("  %-20s %4d%s\n", t.name, t.count, styleDim.Render(avgStr))
		}
	} else {
		fmt.Println(styleDim.Render("  no tool data"))
	}

	// Subagent invocations.
	if len(m.SubagentInvocations) > 0 {
		fmt.Println()
		fmt.Println(styleHeader.Render("Subagent Invocations"))
		// Aggregate by agent name.
		type subStats struct {
			count    int
			totalDur time.Duration
		}
		byAgent := make(map[string]*subStats)
		for _, inv := range m.SubagentInvocations {
			s, ok := byAgent[inv.Agent]
			if !ok {
				s = &subStats{}
				byAgent[inv.Agent] = s
			}
			s.count++
			s.totalDur += inv.Duration
		}
		// Sort by count descending.
		type agentEntry struct {
			name  string
			stats *subStats
		}
		var agents []agentEntry
		for name, stats := range byAgent {
			agents = append(agents, agentEntry{name, stats})
		}
		sort.Slice(agents, func(i, j int) bool { return agents[i].stats.count > agents[j].stats.count })
		for _, a := range agents {
			avgDur := ""
			if a.stats.totalDur > 0 && a.stats.count > 0 {
				avg := a.stats.totalDur / time.Duration(a.stats.count)
				avgDur = fmt.Sprintf("  avg %s", formatDurationLong(avg))
			}
			fmt.Printf("  %-20s %4d%s\n", a.name, a.stats.count, styleDim.Render(avgDur))
		}
	}
}

// renderSessionCompactTable renders a compact one-row-per-opencode-session table
// for tmux sessions that contain multiple opencode sessions. If detail is true,
// each session is rendered as a full block instead.
func renderSessionCompactTable(sessionName string, metrics []*sessionMetrics, status *db.Status, detail bool) {
	styleHeader := lipgloss.NewStyle().Bold(true)
	styleLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	// Compute summary totals. Count real (non-legacy) opencode sessions separately
	// from the legacy sentinel group — the legacy group is a synthetic bucket for
	// pre-sidecar events, not a real opencode session.
	var totalTurns int
	var totalCost float64
	var realSessionCount int
	var hasLegacyGroup bool
	distinctModels := make(map[string]struct{})
	for _, m := range metrics {
		totalTurns += m.AssistantTurns
		totalCost += m.totalCost()
		if m.isLegacy() {
			hasLegacyGroup = true
		} else {
			realSessionCount++
			if m.ModelID != "" {
				distinctModels[m.ModelID] = struct{}{}
			}
		}
	}

	fmt.Println(styleHeader.Render("session: " + sessionName))
	if status != nil && status.State != "" {
		fmt.Printf("%s %s\n", styleLabel.Render("state:"), stateStyle(status.State).Render(status.State))
	}
	fmt.Println()

	// Summary line.
	modelCountStr := ""
	if len(distinctModels) > 1 {
		modelCountStr = fmt.Sprintf(", %d models", len(distinctModels))
	} else if len(distinctModels) == 1 {
		for m := range distinctModels {
			modelCountStr = fmt.Sprintf(", model: %s", m)
		}
	}
	legacySuffix := ""
	if hasLegacyGroup {
		legacySuffix = " (+ legacy events)"
	}
	costStr := "—"
	if totalCost > 0 {
		costStr = formatCost(totalCost)
	}
	fmt.Printf("%s %d opencode sessions%s%s · %d turns · %s\n",
		styleLabel.Render("summary:"),
		realSessionCount,
		modelCountStr,
		legacySuffix,
		totalTurns,
		costStr,
	)
	fmt.Println()

	if detail {
		// --detail: render each opencode session as a full block.
		for i, m := range metrics {
			if i > 0 {
				fmt.Println(styleDim.Render(strings.Repeat("─", 60)))
				fmt.Println()
			}
			if m.isLegacy() {
				fmt.Printf("%s %s\n", styleLabel.Render("opencode session:"), styleDim.Render("(legacy, pre-sidecar)"))
				if m.ModelID == "" {
					m.ModelID = "(legacy)"
				}
			} else {
				fmt.Printf("%s %s\n", styleLabel.Render("opencode session:"), styleDim.Render(m.HarnessSessionID))
			}
			fmt.Println()
			renderSessionDetail(m)
		}
		return
	}

	// Compact table mode.
	const (
		wStarted  = 20
		wDuration = 10
		wModel    = 30
		wTurns    = 6
		wInput    = 8
		wOutput   = 8
		wCost     = 8
	)

	header := fmt.Sprintf("%-*s  %-*s  %-*s  %*s  %*s  %*s  %*s",
		wStarted, "STARTED",
		wDuration, "DURATION",
		wModel, "MODEL",
		wTurns, "TURNS",
		wInput, "INPUT",
		wOutput, "OUTPUT",
		wCost, "COST",
	)
	fmt.Println(styleDim.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header))))

	for _, m := range metrics {
		// Session identifier label.
		startedStr := "—"
		if !m.FirstEvent.IsZero() {
			startedStr = m.FirstEvent.Format("2006-01-02 15:04")
		}

		durStr := formatDurationLong(m.duration())

		modelStr := m.ModelID
		if modelStr == "" {
			modelStr = "—"
		}
		if m.isLegacy() {
			// For the legacy sentinel row, show "(legacy)" in both STARTED and MODEL
			// columns. The label is kept short to fit the 20-char STARTED column.
			// The --detail mode uses the longer "(legacy, pre-sidecar)" label where
			// there is no width constraint.
			// Note: modelStr may be "—" here (assigned above when m.ModelID == ""),
			// so we also check for "—" to catch the no-model-data case.
			if modelStr == "—" || modelStr == "" || modelStr == "(legacy)" {
				modelStr = "(legacy)"
			}
			startedStr = "(legacy)"
		}
		if len(modelStr) > wModel {
			modelStr = modelStr[:wModel-3] + "..."
		}

		turnsStr := fmt.Sprintf("%d", m.AssistantTurns)
		inputStr := "—"
		if m.InputTokens > 0 {
			inputStr = formatTokenCount(m.InputTokens)
		}
		outputStr := "—"
		if m.OutputTokens > 0 {
			outputStr = formatTokenCount(m.OutputTokens)
		}
		costStr := "—"
		if c := m.totalCost(); c > 0 {
			costStr = formatCost(c)
		}

		fmt.Printf("%-*s  %-*s  %-*s  %*s  %*s  %*s  %*s\n",
			wStarted, startedStr,
			wDuration, durStr,
			wModel, modelStr,
			wTurns, turnsStr,
			wInput, inputStr,
			wOutput, outputStr,
			wCost, costStr,
		)
	}

	fmt.Println()
	fmt.Println(styleDim.Render("use --detail to see full metrics for each opencode session"))
}

// ---------- summary table ----------

// runStatsSummary shows a summary table of all active sessions across all repos.
func runStatsSummary() error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	defer d.Close()

	statuses, err := d.AllActiveStatus()
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}

	if len(statuses) == 0 {
		fmt.Println("no active sessions found")
		return nil
	}

	// Collect metrics per session.
	type summaryRow struct {
		Session  string
		Agent    string
		State    string
		Duration time.Duration
		Tokens   int
		Cost     float64
	}

	var rows []summaryRow
	for _, s := range statuses {
		events, _ := d.AllSessionEvents(s.SessionName)
		grouped, order := groupEventsByHarnessSessionID(events)
		// For the summary table, accumulate tokens, cost, and duration across
		// all opencode sessions within the tmux session. Cost is summed per-session
		// (each session uses its own model's pricing) rather than applying a single
		// model's rate to all tokens — critical for sessions that spanned model changes.
		var totalInput, totalOutput int
		var totalCost float64
		var firstEvent, lastEvent time.Time
		for _, key := range order {
			m := collectMetrics(grouped[key], key)
			totalInput += m.InputTokens
			totalOutput += m.OutputTokens
			totalCost += m.totalCost()
			if !m.FirstEvent.IsZero() && (firstEvent.IsZero() || m.FirstEvent.Before(firstEvent)) {
				firstEvent = m.FirstEvent
			}
			if m.LastEvent.After(lastEvent) {
				lastEvent = m.LastEvent
			}
		}
		dur := time.Duration(0)
		if !firstEvent.IsZero() && !lastEvent.IsZero() {
			dur = lastEvent.Sub(firstEvent)
		}

		// Determine agent name for display from status (most up-to-date).
		agentName := ""
		if s.RootAgentName != nil && *s.RootAgentName != "" {
			agentName = *s.RootAgentName
		}

		rows = append(rows, summaryRow{
			Session:  s.SessionName,
			Agent:    truncateStr(agentShortName(agentName), 12),
			State:    s.State,
			Duration: dur,
			Tokens:   totalInput + totalOutput,
			Cost:     totalCost,
		})
	}

	// Render table.
	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleName := lipgloss.NewStyle().Bold(true)
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	fmt.Println(styleHeader.Render(fmt.Sprintf("%-36s  %-12s  %-12s  %-10s  %8s  %8s",
		"SESSION", "AGENT", "STATE", "DURATION", "TOKENS", "COST")))

	for _, r := range rows {
		stateStyled := stateStyle(r.State).Render(fmt.Sprintf("%-12s", r.State))
		tokStr := "—"
		if r.Tokens > 0 {
			tokStr = formatTokenCount(r.Tokens)
		}
		costStr := "—"
		if r.Cost > 0 {
			costStr = formatCost(r.Cost)
		}
		durStr := "—"
		if r.Duration > 0 {
			durStr = formatDurationLong(r.Duration)
		}
		sessionName := r.Session
		if len(sessionName) > 36 {
			sessionName = sessionName[:33] + "..."
		}
		fmt.Printf("%s  %-12s  %s  %-10s  %8s  %8s\n",
			styleName.Render(fmt.Sprintf("%-36s", sessionName)),
			styleDim.Render(r.Agent),
			stateStyled,
			durStr,
			tokStr,
			costStr,
		)
	}

	fmt.Println()
	fmt.Println(styleDim.Render("run `prism stats <session>` for detailed metrics"))
	return nil
}

// ---------- historical aggregate ----------

func runStatsHistorical(days int) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	defer d.Close()

	sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	events, err := d.EventsSince(sinceMs)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}

	if len(events) == 0 {
		fmt.Printf("no events in the last %d days\n", days)
		return nil
	}

	// Group events by session.
	bySession := make(map[string][]db.Event)
	for _, e := range events {
		bySession[e.SessionName] = append(bySession[e.SessionName], e)
	}

	// Aggregate metrics.
	var totalSessions int
	var totalCost float64
	var totalDuration time.Duration
	toolCounts := make(map[string]int)
	type sessionCostEntry struct {
		Session string
		Cost    float64
	}
	var sessionCosts []sessionCostEntry

	for session, evts := range bySession {
		totalSessions++
		// Sum across all opencode sessions within this tmux session.
		grouped, order := groupEventsByHarnessSessionID(evts)
		// Hoist status lookup outside the inner loop to avoid N+1 queries when
		// multiple opencode SID groups have no model data.
		st, _ := d.CurrentStatus(session)
		var sessionCost float64
		var sessionDur time.Duration
		var firstEvent, lastEvent time.Time
		for _, key := range order {
			m := collectMetrics(grouped[key], key)
			// Enrich model from status table for cost calculation.
			if m.ModelID == "" && st != nil && st.RootModelID != nil && *st.RootModelID != "" {
				m.ModelID = *st.RootModelID
			}
			sessionCost += m.totalCost()
			for tool, count := range m.ToolCalls {
				toolCounts[tool] += count
			}
			if !m.FirstEvent.IsZero() && (firstEvent.IsZero() || m.FirstEvent.Before(firstEvent)) {
				firstEvent = m.FirstEvent
			}
			if m.LastEvent.After(lastEvent) {
				lastEvent = m.LastEvent
			}
		}
		if !firstEvent.IsZero() && !lastEvent.IsZero() {
			sessionDur = lastEvent.Sub(firstEvent)
		}
		totalCost += sessionCost
		totalDuration += sessionDur
		if sessionCost > 0 {
			sessionCosts = append(sessionCosts, sessionCostEntry{session, sessionCost})
		}
	}

	avgDuration := time.Duration(0)
	if totalSessions > 0 {
		avgDuration = totalDuration / time.Duration(totalSessions)
	}

	styleHeader := lipgloss.NewStyle().Bold(true)
	styleLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	fmt.Println(styleHeader.Render(fmt.Sprintf("Aggregate Statistics — last %d days", days)))
	fmt.Println()
	fmt.Printf("  %s %d\n", styleLabel.Render("sessions:"), totalSessions)
	costStr := "—"
	if totalCost > 0 {
		costStr = formatCost(totalCost)
	}
	fmt.Printf("  %s %s\n", styleLabel.Render("total cost:"), costStr)
	fmt.Printf("  %s %s\n", styleLabel.Render("avg duration:"), formatDurationLong(avgDuration))
	fmt.Println()

	// Tool breakdown.
	if len(toolCounts) > 0 {
		fmt.Println(styleHeader.Render("Tool Calls"))
		type toolEntry struct {
			name  string
			count int
		}
		var tools []toolEntry
		for name, count := range toolCounts {
			tools = append(tools, toolEntry{name, count})
		}
		sort.Slice(tools, func(i, j int) bool { return tools[i].count > tools[j].count })
		for _, t := range tools {
			fmt.Printf("  %-20s %4d\n", t.name, t.count)
		}
		fmt.Println()
	}

	// Cost by session.
	if len(sessionCosts) > 0 {
		sort.Slice(sessionCosts, func(i, j int) bool { return sessionCosts[i].Cost > sessionCosts[j].Cost })
		fmt.Println(styleHeader.Render("Cost by Session"))
		for _, sc := range sessionCosts {
			sessionName := sc.Session
			if len(sessionName) > 40 {
				sessionName = sessionName[:37] + "..."
			}
			fmt.Printf("  %-40s %s\n", sessionName, styleDim.Render(formatCost(sc.Cost)))
		}
	}

	return nil
}

// ---------- doom-loop events ----------

// runStatsDoomLoops queries doom_loop_detected events and renders them as a
// table sorted by timestamp descending. sessionFilter narrows to a specific
// session when non-empty. days is the look-back window (must be > 0).
func runStatsDoomLoops(sessionFilter string, days int) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats --doomloops: %w", err)
	}
	defer d.Close()

	sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	events, err := d.QueryDoomLoopEvents(sessionFilter, sinceMs)
	if err != nil {
		return fmt.Errorf("stats --doomloops: %w", err)
	}

	styleHeader := lipgloss.NewStyle().Bold(true)
	styleHeaderDim := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	title := fmt.Sprintf("Doom Loop Events — last %d days", days)
	if sessionFilter != "" {
		title = fmt.Sprintf("Doom Loop Events — session %s, last %d days", sessionFilter, days)
	}
	fmt.Println(styleHeader.Render(title))
	fmt.Println()

	if len(events) == 0 {
		fmt.Println(styleDim.Render("  no doom_loop_detected events in the specified window"))
		return nil
	}

	const (
		wSession   = 32
		wTool      = 12
		wPattern   = 40
		wCount     = 5
		wTimestamp = 19
	)

	header := fmt.Sprintf("%-*s  %-*s  %-*s  %*s  %-*s",
		wSession, "SESSION",
		wTool, "TOOL",
		wPattern, "ARG PATTERN",
		wCount, "COUNT",
		wTimestamp, "TIMESTAMP",
	)
	fmt.Println(styleHeaderDim.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header))))

	for _, e := range events {
		var p payload.DoomLoopDetected
		_ = json.Unmarshal([]byte(e.Payload), &p)

		sessionStr := e.SessionName
		if len(sessionStr) > wSession {
			sessionStr = sessionStr[:wSession-3] + "..."
		}

		toolStr := p.Tool
		if len(toolStr) > wTool {
			toolStr = toolStr[:wTool-3] + "..."
		}

		patternStr := p.Pattern
		if len(patternStr) > wPattern {
			patternStr = patternStr[:wPattern-3] + "..."
		}

		countStr := fmt.Sprintf("%d", p.Count)
		if p.Count == 0 {
			countStr = "—"
		}

		tsStr := e.CreatedAt.Format("2006-01-02 15:04:05")

		fmt.Printf("%-*s  %-*s  %-*s  %*s  %-*s\n",
			wSession, sessionStr,
			wTool, toolStr,
			wPattern, patternStr,
			wCount, countStr,
			wTimestamp, tsStr,
		)
	}

	fmt.Println()
	if sessionFilter == "" {
		fmt.Println(styleDim.Render("use `prism stats <session> --doomloops` to filter to a specific session"))
	}
	return nil
}

// ---------- permission denied events ----------

// denialKey is the aggregation key for permission_denied events.
type denialKey struct {
	Session string
	Tool    string
}

// runStatsDenials queries permission_denied events and renders them as a table
// aggregated by (session_name, tool), sorted by count descending.
// sessionFilter narrows to a specific session when non-empty.
// days is the look-back window (must be > 0).
func runStatsDenials(sessionFilter string, days int) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats --denials: %w", err)
	}
	defer d.Close()

	// Validate session exists when a filter is provided.
	if sessionFilter != "" {
		status, err := d.CurrentStatus(sessionFilter)
		if err != nil {
			return fmt.Errorf("stats --denials: %w", err)
		}
		events, _ := d.AllSessionEvents(sessionFilter)
		if status == nil && len(events) == 0 {
			return fmt.Errorf("session %q not found", sessionFilter)
		}
	}

	sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	events, err := d.QueryPermissionEvents("permission_denied", sessionFilter, sinceMs)
	if err != nil {
		return fmt.Errorf("stats --denials: %w", err)
	}

	styleHeader := lipgloss.NewStyle().Bold(true)
	styleHeaderDim := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	title := fmt.Sprintf("Permission Denials — last %d days", days)
	if sessionFilter != "" {
		title = fmt.Sprintf("Permission Denials — session %s, last %d days", sessionFilter, days)
	}
	fmt.Println(styleHeader.Render(title))
	fmt.Println()

	if len(events) == 0 {
		msg := fmt.Sprintf("No permission denials in the last %d days", days)
		if sessionFilter != "" {
			msg = fmt.Sprintf("No permission denials for session %s in the last %d days", sessionFilter, days)
		}
		fmt.Println(styleDim.Render("  " + msg))
		return nil
	}

	// Aggregate by (session, tool).
	counts := make(map[denialKey]int)
	for _, e := range events {
		var p payload.PermissionDenied
		_ = json.Unmarshal([]byte(e.Payload), &p)
		tool := p.Tool
		if tool == "" {
			tool = "<unknown>"
		}
		counts[denialKey{Session: e.SessionName, Tool: tool}]++
	}

	// Sort by count descending, then session, then tool.
	type denialRow struct {
		Session string
		Tool    string
		Count   int
	}
	var rows []denialRow
	for k, c := range counts {
		rows = append(rows, denialRow{Session: k.Session, Tool: k.Tool, Count: c})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if rows[i].Session != rows[j].Session {
			return rows[i].Session < rows[j].Session
		}
		return rows[i].Tool < rows[j].Tool
	})

	const (
		wDSession = 36
		wDTool    = 20
		wDCount   = 5
	)

	header := fmt.Sprintf("%-*s  %-*s  %*s",
		wDSession, "SESSION",
		wDTool, "TOOL",
		wDCount, "COUNT",
	)
	fmt.Println(styleHeaderDim.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header))))

	for _, r := range rows {
		sessionStr := r.Session
		if len(sessionStr) > wDSession {
			sessionStr = sessionStr[:wDSession-3] + "..."
		}
		toolStr := r.Tool
		if len(toolStr) > wDTool {
			toolStr = toolStr[:wDTool-3] + "..."
		}
		fmt.Printf("%-*s  %-*s  %*d\n",
			wDSession, sessionStr,
			wDTool, toolStr,
			wDCount, r.Count,
		)
	}

	fmt.Println()
	if sessionFilter == "" {
		fmt.Println(styleDim.Render("use `prism stats <session> --denials` to filter to a specific session"))
	}
	return nil
}

// ---------- permission ask events ----------

// askKey is the aggregation key for permission_ask events.
type askKey struct {
	Session string
	Tool    string
	Pattern string
}

// runStatsAsks queries permission_ask events and renders them as a table
// aggregated by (session_name, tool, pattern), sorted by count descending.
// sessionFilter narrows to a specific session when non-empty.
// days is the look-back window (must be > 0).
func runStatsAsks(sessionFilter string, days int) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats --asks: %w", err)
	}
	defer d.Close()

	// Validate session exists when a filter is provided.
	if sessionFilter != "" {
		status, err := d.CurrentStatus(sessionFilter)
		if err != nil {
			return fmt.Errorf("stats --asks: %w", err)
		}
		events, _ := d.AllSessionEvents(sessionFilter)
		if status == nil && len(events) == 0 {
			return fmt.Errorf("session %q not found", sessionFilter)
		}
	}

	sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	events, err := d.QueryPermissionEvents("permission_ask", sessionFilter, sinceMs)
	if err != nil {
		return fmt.Errorf("stats --asks: %w", err)
	}

	styleHeader := lipgloss.NewStyle().Bold(true)
	styleHeaderDim := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	title := fmt.Sprintf("Permission Asks — last %d days", days)
	if sessionFilter != "" {
		title = fmt.Sprintf("Permission Asks — session %s, last %d days", sessionFilter, days)
	}
	fmt.Println(styleHeader.Render(title))
	fmt.Println()

	if len(events) == 0 {
		msg := fmt.Sprintf("No permission asks in the last %d days", days)
		if sessionFilter != "" {
			msg = fmt.Sprintf("No permission asks for session %s in the last %d days", sessionFilter, days)
		}
		fmt.Println(styleDim.Render("  " + msg))
		return nil
	}

	// Aggregate by (session, tool, pattern). Each pattern in the patterns slice
	// produces a separate aggregation row per the spec.
	counts := make(map[askKey]int)
	for _, e := range events {
		var p payload.PermissionAsk
		_ = json.Unmarshal([]byte(e.Payload), &p)
		tool := string(p.Tool)
		if tool == "" {
			tool = "<unknown>"
		}
		if len(p.Patterns) == 0 {
			counts[askKey{Session: e.SessionName, Tool: tool, Pattern: "<no pattern>"}]++
		} else {
			for _, pat := range p.Patterns {
				if pat == "" {
					pat = "<no pattern>"
				}
				counts[askKey{Session: e.SessionName, Tool: tool, Pattern: pat}]++
			}
		}
	}

	// Sort by count descending, then session, tool, pattern.
	type askRow struct {
		Session string
		Tool    string
		Pattern string
		Count   int
	}
	var rows []askRow
	for k, c := range counts {
		rows = append(rows, askRow{Session: k.Session, Tool: k.Tool, Pattern: k.Pattern, Count: c})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if rows[i].Session != rows[j].Session {
			return rows[i].Session < rows[j].Session
		}
		if rows[i].Tool != rows[j].Tool {
			return rows[i].Tool < rows[j].Tool
		}
		return rows[i].Pattern < rows[j].Pattern
	})

	const (
		wASession = 36
		wATool    = 20
		wAPattern = 30
		wACount   = 5
	)

	header := fmt.Sprintf("%-*s  %-*s  %-*s  %*s",
		wASession, "SESSION",
		wATool, "TOOL",
		wAPattern, "PATTERN",
		wACount, "COUNT",
	)
	fmt.Println(styleHeaderDim.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header))))

	for _, r := range rows {
		sessionStr := r.Session
		if len(sessionStr) > wASession {
			sessionStr = sessionStr[:wASession-3] + "..."
		}
		toolStr := r.Tool
		if len(toolStr) > wATool {
			toolStr = toolStr[:wATool-3] + "..."
		}
		patternStr := r.Pattern
		if len(patternStr) > wAPattern {
			patternStr = patternStr[:wAPattern-3] + "..."
		}
		fmt.Printf("%-*s  %-*s  %-*s  %*d\n",
			wASession, sessionStr,
			wATool, toolStr,
			wAPattern, patternStr,
			wACount, r.Count,
		)
	}

	fmt.Println()
	if sessionFilter == "" {
		fmt.Println(styleDim.Render("use `prism stats <session> --asks` to filter to a specific session"))
	}
	return nil
}

// ---------- formatting helpers ----------

// formatTokenCount formats a token count as "1.2K", "45K", etc.
func formatTokenCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 10000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	if n < 1000000 {
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1000000)
}

// formatCost formats a dollar cost as "~$0.42" or "~$12.34".
func formatCost(cost float64) string {
	if cost < 0.01 {
		return "<$0.01"
	}
	// Round to 2 decimal places.
	rounded := math.Round(cost*100) / 100
	return fmt.Sprintf("~$%.2f", rounded)
}

// formatDurationLong formats a duration as "1h 23m", "5m 12s", or "<1s".
func formatDurationLong(d time.Duration) string {
	if d < time.Second {
		if d == 0 {
			return "—"
		}
		return "<1s"
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

// agentShortName returns a shortened version of the agent name.
func agentShortName(name string) string {
	if name == "" {
		return "—"
	}
	return name
}

// truncateStr truncates a string to maxLen, adding "..." if needed.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 4 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// ---------- model performance breakdown ----------

// modelMetrics tracks per-model metrics accumulated across turns.
type modelMetrics struct {
	Provider     string
	Model        string
	Turns        int
	Sessions     map[string]struct{} // distinct opencode session IDs
	DurationsMs  []float64           // durationMs values for P50 (zero values excluded)
	TtftMs       []float64           // ttftMs values for P50 (zero/absent values excluded)
	TokPerSec    []float64           // output tokens/sec per turn (zero-duration turns excluded)
	InputTokens  int
	OutputTokens int
	Cost         float64
	AgentCounts  map[string]int // agent name → turn count for this model
}

// percentileFloat64 returns the p-th percentile of a sorted (or unsorted) slice.
// vals is sorted in-place. Returns 0 for an empty slice.
func percentileFloat64(vals []float64, p int) float64 {
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	if len(vals) == 1 {
		return vals[0]
	}
	// Nearest-rank method.
	idx := int(math.Ceil(float64(p)/100.0*float64(len(vals)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	return vals[idx]
}

// splitModel splits a "provider/model" string into its two parts.
// If there is no "/" the whole string is returned as the model name with an
// empty provider.
func splitModel(modelID string) (provider, model string) {
	idx := strings.IndexByte(modelID, '/')
	if idx < 0 {
		return "", modelID
	}
	return modelID[:idx], modelID[idx+1:]
}

// collectModelMetrics groups msg_assistant events by provider/model and builds
// per-model metrics. Turns with durationMs == 0 are excluded from latency and
// throughput calculations.
//
// Sessions are counted by distinct harness_session_id (not session_name) so that
// long-lived tmux sessions with multiple opencode sessions are counted correctly.
// Events with a NULL harness_session_id are counted as distinct sessions only if the
// session_name is distinct — they're bucketed by session_name as a fallback.
func collectModelMetrics(events []db.Event) map[string]*modelMetrics {
	metrics := make(map[string]*modelMetrics)

	for _, e := range events {
		if e.Type != "msg_assistant" {
			continue
		}

		var p payload.MsgAssistant
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			continue
		}
		if p.Model == "" {
			continue
		}

		provider, model := splitModel(p.Model)
		key := p.Model // full "provider/model" as map key
		m, ok := metrics[key]
		if !ok {
			m = &modelMetrics{
				Provider:    provider,
				Model:       model,
				Sessions:    make(map[string]struct{}),
				AgentCounts: make(map[string]int),
			}
			metrics[key] = m
		}

		m.Turns++

		// Count sessions by harness_session_id when available; fall back to session_name
		// for NULL-sid (legacy) events so they still contribute a session count.
		if e.HarnessSessionID != nil && *e.HarnessSessionID != "" {
			m.Sessions[*e.HarnessSessionID] = struct{}{}
		} else {
			m.Sessions["legacy:"+e.SessionName] = struct{}{}
		}

		m.InputTokens += p.InputTokens
		m.OutputTokens += p.OutputTokens

		// Track agent breakdown.
		if p.Agent != "" {
			m.AgentCounts[p.Agent]++
		}

		// Cost using the full key (provider/model).
		// If the model is not in the local pricing table, fall back to the
		// cost reported in the event payload (e.g. openrouter/* models).
		if costs, ok := modelCosts[key]; ok {
			m.Cost += (float64(p.InputTokens)*costs.Input +
				float64(p.OutputTokens)*costs.Output +
				float64(p.CacheReadTokens)*costs.CacheRead +
				float64(p.CacheWriteTokens)*costs.CacheWrite) / 1_000_000
		} else {
			m.Cost += p.Cost
		}

		// Latency and throughput only for turns with valid duration.
		if p.DurationMs > 0 {
			m.DurationsMs = append(m.DurationsMs, float64(p.DurationMs))
			secs := float64(p.DurationMs) / 1000.0
			tokPerSec := float64(p.OutputTokens) / secs
			m.TokPerSec = append(m.TokPerSec, tokPerSec)
		}

		// TTFT only for turns with a non-zero ttftMs value.
		if p.TtftMs > 0 {
			m.TtftMs = append(m.TtftMs, float64(p.TtftMs))
		}
	}

	return metrics
}

// formatAgentSummary returns a compact agent summary string for the AGENTS column.
// Shows the dominant agent name, followed by "(×N)" if there are N distinct agents.
// Example: "coordinator (×3)" means coordinator ran most turns and 3 distinct
// agent types ran on this model.
func formatAgentSummary(agentCounts map[string]int) string {
	if len(agentCounts) == 0 {
		return "—"
	}

	// Find the dominant agent (most turns).
	var dominant string
	var dominantCount int
	for agent, count := range agentCounts {
		if count > dominantCount || (count == dominantCount && agent < dominant) {
			dominant = agent
			dominantCount = count
		}
	}

	if len(agentCounts) == 1 {
		return dominant
	}
	return fmt.Sprintf("%s (×%d)", dominant, len(agentCounts))
}

// formatLatency formats a duration given in milliseconds as "X.Xs" or "Xm Ys".
// Values that would round up to "60.0s" (i.e. ≥ 59,950 ms) are shown in the
// minute format instead.
func formatLatency(ms float64) string {
	if ms < 59_950 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	totalSecs := int(math.Round(ms / 1000))
	mins := totalSecs / 60
	secs := totalSecs % 60
	return fmt.Sprintf("%dm %ds", mins, secs)
}

var modelCmd = &cobra.Command{
	Use:   "model",
	Short: "Per-provider/model performance breakdown",
	Long: `Show a performance breakdown by provider and model over the specified time window.

By default shows the last 7 days across all repos. Use --days N to change the
window.

TTFT p50 shows median time-to-first-token (request sent → first streaming chunk
received). DUR p50 shows median full turn duration (request sent → complete
response received).

The AGENTS column shows the dominant agent type for each model, with a count of
distinct agent types in parentheses when more than one agent ran on that model.
Example: "coordinator (×3)" means the coordinator was dominant and 3 distinct
agent types used that model.`,
	Args: cobra.NoArgs,
	RunE: runStatsModel,
}

func init() {
	modelCmd.Flags().Int("days", 7, "Number of days to include (default 7)")
	statsCmd.AddCommand(modelCmd)
}

func runStatsModel(cmd *cobra.Command, _ []string) error {
	days, _ := cmd.Flags().GetInt("days")
	if days <= 0 {
		return fmt.Errorf("--days must be greater than 0")
	}

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats model: %w", err)
	}
	defer d.Close()

	sinceMs := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	events, err := d.EventsSince(sinceMs)
	if err != nil {
		return fmt.Errorf("stats model: %w", err)
	}

	metrics := collectModelMetrics(events)

	if len(metrics) == 0 {
		fmt.Printf("no model data in the last %d days\n", days)
		return nil
	}

	renderModelBreakdown(metrics, days)
	return nil
}

func renderModelBreakdown(metrics map[string]*modelMetrics, days int) {
	// Convert to slice for sorting.
	type modelRow struct {
		key string
		m   *modelMetrics
	}
	var rows []modelRow
	for key, m := range metrics {
		rows = append(rows, modelRow{key, m})
	}

	// Sort by total cost descending; ties sorted by PROVIDER then MODEL ascending.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].m.Cost != rows[j].m.Cost {
			return rows[i].m.Cost > rows[j].m.Cost
		}
		if rows[i].m.Provider != rows[j].m.Provider {
			return rows[i].m.Provider < rows[j].m.Provider
		}
		return rows[i].m.Model < rows[j].m.Model
	})

	styleHeader := lipgloss.NewStyle().Bold(true)
	styleHeaderDim := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	fmt.Println(styleHeader.Render(fmt.Sprintf("Model Performance — last %d days", days)))
	fmt.Println()

	// Column widths.
	const (
		wProvider = 18
		wModel    = 28
		wTurns    = 6
		wTtft     = 9
		wDur      = 9
		wTokS     = 10
		wInput    = 8
		wOutput   = 8
		wCost     = 8
		wSessions = 8
		wAgents   = 20
	)

	// Header row.
	header := fmt.Sprintf("%-*s  %-*s  %*s  %*s  %*s  %*s  %*s  %*s  %*s  %*s  %-*s",
		wProvider, "PROVIDER",
		wModel, "MODEL",
		wTurns, "TURNS",
		wTtft, "TTFT p50",
		wDur, "DUR p50",
		wTokS, "TOK/S p50",
		wInput, "INPUT",
		wOutput, "OUTPUT",
		wCost, "COST",
		wSessions, "SESSIONS",
		wAgents, "AGENTS",
	)
	fmt.Println(styleHeaderDim.Render(header))

	// Separator.
	sep := strings.Repeat("─", len(header))
	fmt.Println(styleDim.Render(sep))

	for _, row := range rows {
		m := row.m

		// TTFT P50.
		ttftStr := "-"
		if len(m.TtftMs) > 0 {
			p50ms := percentileFloat64(m.TtftMs, 50)
			ttftStr = formatLatency(p50ms)
		}

		// Full turn duration P50.
		durStr := "-"
		if len(m.DurationsMs) > 0 {
			p50ms := percentileFloat64(m.DurationsMs, 50)
			durStr = formatLatency(p50ms)
		}

		// Throughput P50.
		tokStr := "-"
		if len(m.TokPerSec) > 0 {
			p50tps := percentileFloat64(m.TokPerSec, 50)
			tokStr = fmt.Sprintf("%.0f t/s", p50tps)
		}

		// Cost.
		costStr := "-"
		if _, ok := modelCosts[row.key]; ok {
			costStr = formatCost(m.Cost)
		}

		provider := m.Provider
		if provider == "" {
			provider = "(unknown)"
		}

		agentStr := formatAgentSummary(m.AgentCounts)

		fmt.Printf("%-*s  %-*s  %*d  %*s  %*s  %*s  %*s  %*s  %*s  %*d  %-*s\n",
			wProvider, provider,
			wModel, m.Model,
			wTurns, m.Turns,
			wTtft, ttftStr,
			wDur, durStr,
			wTokS, tokStr,
			wInput, formatTokenCount(m.InputTokens),
			wOutput, formatTokenCount(m.OutputTokens),
			wCost, costStr,
			wSessions, len(m.Sessions),
			wAgents, agentStr,
		)
	}

	fmt.Println()
	fmt.Println(styleDim.Render("Note: TTFT p50 = time to first token (request→first chunk); DUR p50 = full turn duration (request→complete response)."))
	fmt.Println(styleDim.Render("      SESSIONS = distinct opencode sessions; AGENTS = dominant agent type (×N = N distinct agent types)."))
}
