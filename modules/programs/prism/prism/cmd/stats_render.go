package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/db"
)

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
