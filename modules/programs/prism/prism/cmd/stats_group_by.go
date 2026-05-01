package cmd

// runStatsGroupBy renders a breakdown table for prism stats --group-by <axis>.
//
// Each row in the output represents one distinct value of the group-by column
// (e.g. a harness name). Rows are ordered by session count descending. NULL
// values in the column are grouped into a single "(none)" row.
//
// sinceMs is passed through from --days / --since so that filtering is applied
// before grouping.

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/db"
)

func runStatsGroupBy(axis string, sinceMs int64) error {
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("stats --group-by: %w", err)
	}
	defer d.Close()

	rows, err := d.SpawnOutcomeGroupBy(axis, sinceMs)
	if err != nil {
		// The DB method returns a user-friendly error for invalid axes.
		return err
	}

	if len(rows) == 0 {
		fmt.Printf("no sessions with spawn_inputs and spawn_outcome recorded (group-by %s)\n", axis)
		return nil
	}

	renderGroupByTable(axis, rows)
	return nil
}

// renderGroupByTable prints the group-by breakdown table to stdout.
func renderGroupByTable(axis string, rows []db.GroupByRow) {
	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleBold := lipgloss.NewStyle().Bold(true)

	title := fmt.Sprintf("Breakdown by %s", axis)
	fmt.Println(styleBold.Render(title))
	fmt.Println()

	const (
		wGroup    = 24
		wSessions = 8
		wCost     = 9
		wTokens   = 9
		wDur      = 10
		wTools    = 7
		wErrors   = 7
	)

	header := fmt.Sprintf("%-*s  %*s  %*s  %*s  %*s  %*s  %*s",
		wGroup, strings.ToUpper(axis),
		wSessions, "SESSIONS",
		wCost, "COST",
		wTokens, "TOKENS",
		wDur, "AVG DUR",
		wTools, "TOOLS",
		wErrors, "ERRORS",
	)
	fmt.Println(styleHeader.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header))))

	for _, r := range rows {
		label := r.GroupValue
		if label == "" {
			label = "(none)"
		}
		if len(label) > wGroup {
			label = label[:wGroup-3] + "..."
		}

		costStr := "—"
		if r.CostUSDTotal > 0 {
			costStr = formatCost(r.CostUSDTotal)
		}

		totalTokens := r.TokensInputTotal + r.TokensOutputTotal
		tokStr := "—"
		if totalTokens > 0 {
			tokStr = formatTokenCount(int(totalTokens))
		}

		durStr := "—"
		if r.AvgDurationMs != nil && *r.AvgDurationMs > 0 {
			avgDur := time.Duration(*r.AvgDurationMs) * time.Millisecond
			durStr = formatDurationLong(avgDur)
		}

		fmt.Printf("%-*s  %*d  %*s  %*s  %*s  %*d  %*d\n",
			wGroup, label,
			wSessions, r.SessionCount,
			wCost, costStr,
			wTokens, tokStr,
			wDur, durStr,
			wTools, r.ToolCallCount,
			wErrors, r.ErrorEventCount,
		)
	}

	fmt.Println()
}
