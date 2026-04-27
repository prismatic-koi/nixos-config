package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// ---------- model performance breakdown ----------

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
