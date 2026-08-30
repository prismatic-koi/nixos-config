package cmd

import (
	"encoding/json"
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
// unambiguous), or a session name. jsonMode emits JSON when true.
func runStatsDetail(arg string, forceInstance bool, jsonMode bool) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	defer d.Close()

	sess, err := resolveSessionArg(d, arg, forceInstance)
	if err != nil {
		return err
	}

	if jsonMode {
		out := map[string]any{"session": sess}
		// Surface the spawn_inputs audit columns alongside the sessions
		// row so callers scripting against `prism stats <id> --json` can
		// read containers_flag without a second query.
		// Missing rows (sessions with no spawn_inputs row) omit the key
		// entirely — downstream consumers must use "in" rather than truthy
		// checks to distinguish "unknown" from "false".
		if si, siErr := d.SpawnInputsByInstanceID(sess.InstanceID); siErr == nil && si != nil {
			out["spawn_inputs"] = spawnInputsJSON(si)
		}
		data, merr := json.Marshal(out)
		if merr != nil {
			return fmt.Errorf("stats --json: marshal session: %w", merr)
		}
		fmt.Println(string(data))
		return nil
	}

	renderIncarnationDetail(d, sess)
	return nil
}

// spawnInputsJSON projects db.SpawnInputs onto the audit-only subset surfaced
// by `prism stats <instance-id> --json`. Conversation-bearing columns
// (prompt_text, prompt_source, model_variant_overrides, extras) are
// intentionally omitted — the same boundary that CompareInputs enforces for
// the host-API /stats endpoint applies here.
func spawnInputsJSON(si *db.SpawnInputs) map[string]any {
	m := map[string]any{
		"containers_flag":        si.ContainersFlag,
		"host_mode_flag":         si.HostModeFlag,
		"ignore_concurrency_cap": si.IgnoreConcurrencyCap,
	}
	if si.ProfileName != nil {
		m["profile_name"] = *si.ProfileName
	}
	if si.HarnessFlag != nil {
		m["harness_flag"] = *si.HarnessFlag
	}
	if si.IsolationFlag != nil {
		m["isolation_flag"] = *si.IsolationFlag
	}
	if si.IsolationMode != nil {
		m["isolation_mode"] = *si.IsolationMode
	}
	if si.AgentFlag != nil {
		m["agent_flag"] = *si.AgentFlag
	}
	if si.BranchFlag != nil {
		m["branch_flag"] = *si.BranchFlag
	}
	if si.PRNumber != nil {
		m["pr_number"] = *si.PRNumber
	}
	if si.AbtestPairID != nil {
		m["abtest_pair_id"] = *si.AbtestPairID
	}
	return m
}

// renderIncarnationDetailFromSession renders the session detail for the proxy
// path. outcome is the spawn_outcome row proxied alongside the session by
// the host-API view=detail response — nil means the session
// has not yet computed one (still live), which the renderer surfaces as
// "not yet available", not as zero.
func renderIncarnationDetailFromSession(sess *db.Session, outcome *db.SpawnOutcome) {
	if sess == nil {
		return
	}
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

	if sess.ArchivePath != nil && *sess.ArchivePath != "" {
		fmt.Printf("%s %s\n", styleLabel.Render("archive:"), *sess.ArchivePath)
	} else {
		fmt.Printf("%s %s\n", styleLabel.Render("archive:"), styleDim.Render("(not yet archived)"))
	}
	fmt.Println()
	renderTokenUsageBlock(styleHeader, styleLabel, styleDim, outcome)
}

// renderTokenUsageBlock renders the "Token Usage" block shared by the
// host-direct and sandbox-proxy detail renderers, from a *db.SpawnOutcome.
// outcome nil means the session has not yet had a spawn_outcome row
// computed — typically because the session is still active — and is rendered
// as an explicit "not yet available" message rather than as zero values
// presented as real data. A cost of exactly 0 is expected under subscription
// profiles (for example, anthropic-pi) and is not treated as missing data.
func renderTokenUsageBlock(styleHeader, styleLabel, styleDim lipgloss.Style, outcome *db.SpawnOutcome) {
	fmt.Println(styleHeader.Render("Token Usage"))
	if outcome == nil {
		fmt.Println(styleDim.Render("  not yet available (session still active; spawn_outcome not yet computed)"))
		return
	}
	totalInput := outcome.TokensInputTotal
	totalOutput := outcome.TokensOutputTotal
	totalCacheRead := outcome.TokensCacheReadTotal
	totalCacheWrite := outcome.TokensCacheWriteTotal
	totalCost := outcome.CostUSDTotal
	if totalInput+totalOutput > 0 {
		fmt.Printf("  %s %s\n", styleLabel.Render("input:"), formatTokenCount(int(totalInput)))
		fmt.Printf("  %s %s\n", styleLabel.Render("output:"), formatTokenCount(int(totalOutput)))
		if totalCacheRead > 0 {
			fmt.Printf("  %s %s\n", styleLabel.Render("cache read:"), formatTokenCount(int(totalCacheRead)))
		}
		if totalCacheWrite > 0 {
			fmt.Printf("  %s %s\n", styleLabel.Render("cache write:"), formatTokenCount(int(totalCacheWrite)))
		}
		// Cost is legitimately 0 under subscription profiles (no per-token
		// billing) — render it whenever token usage is present so a $0 run
		// is visibly distinct from "no data", rather than silently omitted.
		// formatCost renders anything under a cent as "<$0.01", which would
		// misrepresent an exact $0 subscription run, so that case is spelled
		// out explicitly instead.
		costStr := "$0.00"
		if totalCost > 0 {
			costStr = formatCost(totalCost)
		}
		fmt.Printf("  %s %s\n", styleLabel.Render("est. cost:"), costStr)
	} else {
		fmt.Println(styleDim.Render("  no token data"))
	}
}

// resolveSessionArg resolves an argument to a single sessions row.
// Disambiguation rules:
//  1. Full 36-char UUID (or --instance flag) → SessionByInstanceID
//  2. Exact match in sessions.session_name → MostRecentSessionForName
//  3. UUID prefix → SessionsByInstanceIDPrefix (must be unambiguous)
//  4. Not found → error
func resolveSessionArg(d *db.DB, arg string, forceInstance bool) (*db.Session, error) {
	// Delegates to db.ResolveSessionArg so the host-API proxy path resolves
	// session names / instance-id prefixes byte-for-byte identically.
	return d.ResolveSessionArg(arg, forceInstance)
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

	// Spawn Inputs block — surfaces the audit columns written at spawn time.
	// When no spawn_inputs row exists (sessions with no spawn_inputs row, or
	// rows created outside the SpawnSession chokepoint) the whole block is
	// omitted rather than rendering a sea of "—" labels. Mirrors the field
	// set surfaced by `prism stats compare`'s Spawn Inputs block. New audit
	// columns land here too.
	if si, siErr := d.SpawnInputsByInstanceID(sess.InstanceID); siErr == nil && si != nil {
		fmt.Println(styleHeader.Render("Spawn Inputs"))
		if si.ProfileName != nil && *si.ProfileName != "" {
			fmt.Printf("  %s %s\n", styleLabel.Render("profile_name:"), *si.ProfileName)
		}
		if si.HarnessFlag != nil && *si.HarnessFlag != "" {
			fmt.Printf("  %s %s\n", styleLabel.Render("harness_flag:"), *si.HarnessFlag)
		}
		if si.IsolationMode != nil && *si.IsolationMode != "" {
			fmt.Printf("  %s %s\n", styleLabel.Render("isolation_mode:"), *si.IsolationMode)
		}
		if si.IsolationFlag != nil && *si.IsolationFlag != "" {
			fmt.Printf("  %s %s\n", styleLabel.Render("isolation_flag:"), *si.IsolationFlag)
		}
		fmt.Printf("  %s %t\n", styleLabel.Render("host_mode_flag:"), si.HostModeFlag)
		fmt.Printf("  %s %t\n", styleLabel.Render("containers_flag:"), si.ContainersFlag)
		if si.AgentFlag != nil && *si.AgentFlag != "" {
			fmt.Printf("  %s %s\n", styleLabel.Render("agent_flag:"), *si.AgentFlag)
		}
		if si.BranchFlag != nil && *si.BranchFlag != "" {
			fmt.Printf("  %s %s\n", styleLabel.Render("branch_flag:"), *si.BranchFlag)
		}
		if si.AbtestPairID != nil && *si.AbtestPairID != "" {
			fmt.Printf("  %s %s\n", styleLabel.Render("abtest_pair_id:"), *si.AbtestPairID)
		}
		fmt.Println()
	}

	// Token/cost totals: the persisted-or-computed spawn_outcome row, the
	// same source used by the sandbox proxy path so the two paths render
	// byte-identical output.
	outcome := d.CompareRunOutcome(sess)
	renderTokenUsageBlock(styleHeader, styleLabel, styleDim, outcome)
}

// renderSessionDetail renders the detailed block format for a single harness session.
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

// renderSessionCompactTable renders a compact one-row-per-agent-session table
// for tmux sessions that contain multiple harness sessions. If detail is true,
// each session is rendered as a full block instead.
func renderSessionCompactTable(sessionName string, metrics []*sessionMetrics, status *db.Status, detail bool) {
	styleHeader := lipgloss.NewStyle().Bold(true)
	styleLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	// Compute summary totals. Count real (non-legacy) harness sessions separately
	// from the legacy sentinel group — the legacy group is a synthetic bucket for
	// pre-sidecar events, not a real harness session.
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
	fmt.Printf("%s %d harness sessions%s%s · %d turns · %s\n",
		styleLabel.Render("summary:"),
		realSessionCount,
		modelCountStr,
		legacySuffix,
		totalTurns,
		costStr,
	)
	fmt.Println()

	if detail {
		// --detail: render each harness session as a full block.
		for i, m := range metrics {
			if i > 0 {
				fmt.Println(styleDim.Render(strings.Repeat("─", 60)))
				fmt.Println()
			}
			if m.isLegacy() {
				fmt.Printf("%s %s\n", styleLabel.Render("harness session:"), styleDim.Render("(legacy, pre-sidecar)"))
				if m.ModelID == "" {
					m.ModelID = "(legacy)"
				}
			} else {
				fmt.Printf("%s %s\n", styleLabel.Render("harness session:"), styleDim.Render(m.HarnessSessionID))
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
	fmt.Println(styleDim.Render("use --detail to see full metrics for each harness session"))
}
