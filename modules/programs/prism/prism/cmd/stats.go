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

// legacySentinel is the key used to group events that have a NULL opencode_sid.
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
	Use:   "stats [session]",
	Short: "Session metrics and statistics",
	Long: `Display metrics and statistics for agent sessions.

With no arguments, shows a summary table of all active sessions across all repos.

With a session name argument, shows per-session metrics. Long-lived sessions
(e.g. nixos-config@main) that contain multiple opencode sessions are shown as
a compact table — one row per opencode session. Short-lived worktree sessions
with a single opencode session are shown as a detailed block.

Use --detail to force the detailed block format even for multi-session tmux sessions.

Use --days N to show aggregate statistics over the last N days.

Use the 'model' subcommand for a per-provider/model performance breakdown.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runStats,
}

func init() {
	statsCmd.Flags().Int("days", 0, "Show aggregate statistics over the last N days")
	statsCmd.Flags().Bool("detail", false, "Force detailed block format even for multi-session tmux sessions")
	rootCmd.AddCommand(statsCmd)
}

func runStats(cmd *cobra.Command, args []string) error {
	days, _ := cmd.Flags().GetInt("days")
	detail, _ := cmd.Flags().GetBool("detail")

	if days > 0 && len(args) > 0 {
		return fmt.Errorf("--days is mutually exclusive with a session name")
	}

	if days > 0 {
		return runStatsHistorical(days)
	}

	if len(args) == 1 {
		return runStatsSession(args[0], detail)
	}

	return runStatsSummary()
}

// ---------- per-session detail ----------

// sessionMetrics holds aggregated metrics for a single opencode session.
type sessionMetrics struct {
	SessionName string
	OpencodeSID string // empty string = legacy sentinel (NULL opencode_sid)
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
		return 0
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
	return m.OpencodeSID == legacySentinel
}

// collectMetrics builds a sessionMetrics from a slice of events that all belong
// to the same opencode session (or the legacy sentinel group). The opencodeSID
// parameter is the key used for this group (legacySentinel for NULL-sid events).
//
// The model field is set to the coordinator/root agent's model if one is present
// (agent field == "coordinator"), falling back to the most-frequently-seen model.
// This gives a more meaningful "session model" than first-seen.
func collectMetrics(events []db.Event, opencodeSID string) *sessionMetrics {
	m := &sessionMetrics{
		OpencodeSID:   opencodeSID,
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

// groupEventsByOpencodeSID partitions events by opencode_sid, preserving
// insertion order for the first occurrence of each key.
// Events with a NULL opencode_sid are grouped under legacySentinel ("").
// Returns the grouped map and the ordered list of keys.
func groupEventsByOpencodeSID(events []db.Event) (map[string][]db.Event, []string) {
	grouped := make(map[string][]db.Event)
	var order []string

	for _, e := range events {
		key := legacySentinel
		if e.OpencodeSID != nil {
			key = *e.OpencodeSID
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

	// Group events by opencode_sid.
	grouped, order := groupEventsByOpencodeSID(events)

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
				fmt.Printf("%s %s\n", styleLabel.Render("opencode session:"), styleDim.Render(m.OpencodeSID))
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
		grouped, order := groupEventsByOpencodeSID(events)
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
		grouped, order := groupEventsByOpencodeSID(evts)
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
// Sessions are counted by distinct opencode_sid (not session_name) so that
// long-lived tmux sessions with multiple opencode sessions are counted correctly.
// Events with a NULL opencode_sid are counted as distinct sessions only if the
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

		// Count sessions by opencode_sid when available; fall back to session_name
		// for NULL-sid (legacy) events so they still contribute a session count.
		if e.OpencodeSID != nil && *e.OpencodeSID != "" {
			m.Sessions[*e.OpencodeSID] = struct{}{}
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
		if costs, ok := modelCosts[key]; ok {
			m.Cost += (float64(p.InputTokens)*costs.Input +
				float64(p.OutputTokens)*costs.Output +
				float64(p.CacheReadTokens)*costs.CacheRead +
				float64(p.CacheWriteTokens)*costs.CacheWrite) / 1_000_000
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
