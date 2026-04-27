package cmd

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

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
// Returns "—" for durations that are <= 0 or that saturate time.Duration
// (i.e. >= math.MaxInt64/2), which indicates an unrecoverable zero-time row.
func formatDurationLong(d time.Duration) string {
	if d <= 0 || d >= time.Duration(math.MaxInt64/2) {
		return "—"
	}
	if d < time.Second {
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
