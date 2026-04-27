package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/db"
)

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
		wID     = 8
		wName   = 32
		wAgent  = 14
		wState  = 10
		wDur    = 10
		wTokens = 8
		wCost   = 8
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
