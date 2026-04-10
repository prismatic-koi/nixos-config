package cmd

// prism stats — session observability and metrics reporting.
//
// Usage:
//
//	prism stats                   summary table of all active sessions across all repos
//	prism stats <session>         per-session detail
//	prism stats --all             same as no flags (kept for backwards compatibility)
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
The --all flag is accepted for backwards compatibility and is a no-op.

With a session name argument, shows detailed per-session metrics including
token usage, cost, duration, tool breakdown, and turn timing.

Use --days N to show aggregate statistics over the last N days.

Use the 'model' subcommand for a per-provider/model performance breakdown.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runStats,
}

func init() {
	statsCmd.Flags().Bool("all", false, "No-op, kept for backwards compatibility (all repos are always shown)")
	statsCmd.Flags().Int("days", 0, "Show aggregate statistics over the last N days")
	rootCmd.AddCommand(statsCmd)
}

func runStats(cmd *cobra.Command, args []string) error {
	days, _ := cmd.Flags().GetInt("days")
	showAll, _ := cmd.Flags().GetBool("all")

	if days > 0 && len(args) > 0 {
		return fmt.Errorf("--days is mutually exclusive with a session name")
	}

	if days > 0 {
		return runStatsHistorical(days)
	}

	if len(args) == 1 {
		return runStatsSession(args[0])
	}

	return runStatsSummary(showAll)
}

// ---------- per-session detail ----------

// sessionMetrics holds aggregated metrics for a single session.
type sessionMetrics struct {
	SessionName string
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

func collectMetrics(events []db.Event) *sessionMetrics {
	m := &sessionMetrics{
		ToolCalls:     make(map[string]int),
		ToolDurations: make(map[string][]time.Duration),
	}

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
				if m.ModelID == "" && p.Model != "" {
					m.ModelID = p.Model
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

	return m
}

func runStatsSession(session string) error {
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

	m := collectMetrics(events)

	// Overlay status info.
	if status != nil {
		m.State = status.State
		if status.RootAgentName != nil && *status.RootAgentName != "" {
			m.AgentName = *status.RootAgentName
		}
		if status.RootModelID != nil && *status.RootModelID != "" {
			m.ModelID = *status.RootModelID
		}
	}

	renderSessionDetail(m)
	return nil
}

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

// ---------- summary table ----------

// runStatsSummary shows a summary table of all active sessions across all repos.
// showAll is accepted for backwards compatibility but is now a no-op.
func runStatsSummary(_ bool) error {
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
		m := collectMetrics(events)
		if s.RootAgentName != nil && *s.RootAgentName != "" {
			m.AgentName = *s.RootAgentName
		}
		if s.RootModelID != nil && *s.RootModelID != "" {
			m.ModelID = *s.RootModelID
		}
		rows = append(rows, summaryRow{
			Session:  s.SessionName,
			Agent:    truncateStr(agentShortName(m.AgentName), 12),
			State:    s.State,
			Duration: m.duration(),
			Tokens:   m.InputTokens + m.OutputTokens,
			Cost:     m.totalCost(),
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

	for session, events := range bySession {
		totalSessions++
		m := collectMetrics(events)
		// Enrich from status table for model info.
		status, _ := d.CurrentStatus(session)
		if status != nil {
			if status.RootModelID != nil && *status.RootModelID != "" {
				m.ModelID = *status.RootModelID
			}
		}
		cost := m.totalCost()
		totalCost += cost
		totalDuration += m.duration()
		for tool, count := range m.ToolCalls {
			toolCounts[tool] += count
		}
		if cost > 0 {
			sessionCosts = append(sessionCosts, sessionCostEntry{session, cost})
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
	Sessions     map[string]struct{} // distinct session IDs
	DurationsMs  []float64           // durationMs values for P50 (zero values excluded)
	TtftMs       []float64           // ttftMs values for P50 (zero/absent values excluded)
	TokPerSec    []float64           // output tokens/sec per turn (zero-duration turns excluded)
	InputTokens  int
	OutputTokens int
	Cost         float64
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
				Provider: provider,
				Model:    model,
				Sessions: make(map[string]struct{}),
			}
			metrics[key] = m
		}

		m.Turns++
		m.Sessions[e.SessionName] = struct{}{}
		m.InputTokens += p.InputTokens
		m.OutputTokens += p.OutputTokens

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

// formatLatency formats a duration given in milliseconds as "Xs" or "Xm Ys".
func formatLatency(ms float64) string {
	secs := int(ms / 1000)
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	mins := secs / 60
	s := secs % 60
	return fmt.Sprintf("%dm %ds", mins, s)
}

var modelCmd = &cobra.Command{
	Use:   "model",
	Short: "Per-provider/model performance breakdown",
	Long: `Show a performance breakdown by provider and model over the specified time window.

By default shows the last 7 days across all repos. Use --days N to change the
window.

TTFT p50 shows median time-to-first-token (request sent → first streaming chunk
received). DUR p50 shows median full turn duration (request sent → complete
response received).`,
	Args: cobra.NoArgs,
	RunE: runStatsModel,
}

func init() {
	modelCmd.Flags().Int("days", 7, "Number of days to include (default 7)")
	modelCmd.Flags().Bool("all", false, "No-op, kept for consistency (always cross-repo)")
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
	)

	// Header row.
	header := fmt.Sprintf("%-*s  %-*s  %*s  %*s  %*s  %*s  %*s  %*s  %*s  %*s",
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

		fmt.Printf("%-*s  %-*s  %*d  %*s  %*s  %*s  %*s  %*s  %*s  %*d\n",
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
		)
	}

	fmt.Println()
	fmt.Println(styleDim.Render("Note: TTFT p50 = time to first token (request→first chunk); DUR p50 = full turn duration (request→complete response)."))
}
