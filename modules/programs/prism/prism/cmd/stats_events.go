package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/payload"
)

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
