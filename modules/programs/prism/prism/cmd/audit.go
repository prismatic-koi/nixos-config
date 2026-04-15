package cmd

// prism audit — query the persistent audit trail of high-impact tool calls.
//
// Usage:
//
//	prism audit                          last 20 audit events across all sessions
//	prism audit <session>                audit events for a specific session
//	prism audit --days N                 events from the last N days
//	prism audit --pattern "gh pr merge"  filter by command substring

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/payload"
)

var auditCmd = &cobra.Command{
	Use:   "audit [session]",
	Short: "Query the audit trail of high-impact tool calls",
	Long: `Query the persistent audit trail of high-impact bash tool calls.

High-impact commands (gh pr merge, git push, gh pr create, gh issue close,
prism spawn, prism cleanup, prism prompt) are promoted from the ephemeral
opencode DB to the persistent prism DB as 'audit' events, so they survive
worktree cleanup and remain attributable to a specific prism session.

With no arguments, shows the last 20 audit events across all sessions.

With a session name, shows audit events for that session only.

Use --days N to restrict to events from the last N days.
Use --pattern TEXT to filter by a command substring (case-insensitive).`,
	Args: cobra.MaximumNArgs(1),
	RunE: runAudit,
}

func init() {
	auditCmd.Flags().Int("days", 0, "Restrict to events from the last N days")
	auditCmd.Flags().String("pattern", "", "Filter by command substring (case-insensitive)")
	auditCmd.Flags().Int("limit", 0, "Maximum number of events to return (default 20 when no session filter)")
	rootCmd.AddCommand(auditCmd)
}

func runAudit(cmd *cobra.Command, args []string) error {
	days, _ := cmd.Flags().GetInt("days")
	pattern, _ := cmd.Flags().GetString("pattern")
	limit, _ := cmd.Flags().GetInt("limit")

	sessionName := ""
	if len(args) == 1 {
		sessionName = args[0]
	}

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer d.Close()

	var sinceMs int64
	if days > 0 {
		sinceMs = time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	}

	events, err := d.QueryAuditEvents(sessionName, sinceMs, pattern, limit)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}

	if len(events) == 0 {
		if sessionName != "" {
			fmt.Printf("no audit events found for session %q\n", sessionName)
		} else {
			fmt.Println("no audit events found")
		}
		if pattern != "" {
			fmt.Printf("  (filter: %q)\n", pattern)
		}
		return nil
	}

	renderAuditEvents(events)
	return nil
}

func renderAuditEvents(events []db.Event) {
	styleHeader := lipgloss.NewStyle().Bold(true)
	styleLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleCmd := lipgloss.NewStyle().Bold(true)

	fmt.Println(styleHeader.Render(fmt.Sprintf("Audit Trail (%d events)", len(events))))
	fmt.Println()

	// Column header.
	const (
		wTime    = 19
		wSession = 32
		wCommand = 60
	)
	header := fmt.Sprintf("%-*s  %-*s  %-*s",
		wTime, "TIME",
		wSession, "SESSION",
		wCommand, "COMMAND",
	)
	fmt.Println(styleDim.Render(header))
	fmt.Println(styleDim.Render(strings.Repeat("─", len(header))))

	for _, e := range events {
		var p payload.Audit
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			// Fallback: print raw payload on unmarshal failure.
			fmt.Printf("%-*s  %-*s  %s\n",
				wTime, e.CreatedAt.Format("2006-01-02 15:04:05"),
				wSession, truncateStr(e.SessionName, wSession),
				e.Payload,
			)
			continue
		}

		sessionStr := p.SessionName
		if sessionStr == "" {
			sessionStr = e.SessionName
		}

		cmdStr := p.Command
		if cmdStr == "" {
			cmdStr = "(no command)"
		}

		timeStr := e.CreatedAt.Format("2006-01-02 15:04:05")

		sessionTrunc := truncateStr(sessionStr, wSession)
		cmdTrunc := truncateStr(cmdStr, wCommand)

		fmt.Printf("%s  %s  %s\n",
			styleDim.Render(fmt.Sprintf("%-*s", wTime, timeStr)),
			styleLabel.Render(fmt.Sprintf("%-*s", wSession, sessionTrunc)),
			styleCmd.Render(cmdTrunc),
		)
	}

	fmt.Println()
	fmt.Println(styleDim.Render("Audit events are written for: gh pr merge/create, gh issue close, git push, prism spawn/cleanup/prompt"))
}
