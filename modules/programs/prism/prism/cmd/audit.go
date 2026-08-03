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
	"github.com/prismatic-koi/prism/internal/sidecar"
)

// ── audit writers ────────────────────────────────────────────────────────────
//
// Two code paths write `audit` events, and the help text and table footer
// below must name both. Neither is restated by hand:
//
//  1. Bash promotion — internal/sidecar/events.go promotes a bash tool call
//     whose command matches sidecar.HighImpactCommandPrefixes().
//  2. The tier-3 `prism checkin` troubleshooting privilege (issue #2587) —
//     internal/sidecar/checkin_permission.go records every cross-repo read
//     that the privileged-repo grant admits.
//
// The bash list is derived from the sidecar package rather than copied,
// because the copy drifted: it still named the pre-#2364 set after four
// prefixes were added, and no test noticed. Writer 2 was missed by the same
// mechanism on the PR that introduced it. audit_writers_test.go now pins
// both halves.

// privilegedCheckinWriterClause names writer 2 in prose. It is a separate
// clause because that writer is not a bash command and has no prefix to list.
const privilegedCheckinWriterClause = "each `prism checkin` that the tier-3 privileged-repo grant admits"

// auditWritersSentence renders the full "what gets recorded" sentence from
// both writers.
func auditWritersSentence() string {
	return "Audit events are written for: " +
		strings.Join(sidecar.HighImpactCommandPrefixes(), ", ") +
		"; and for " + privilegedCheckinWriterClause + "."
}

var auditCmd = &cobra.Command{
	Use:   "audit [session]",
	Short: "Query the audit trail of high-impact tool calls",
	Long: `Query the persistent audit trail of high-impact tool calls.

` + auditWritersSentence() + `

The command rows are promoted from the ephemeral harness DB to the persistent
prism DB, so they survive worktree cleanup. The privileged-checkin rows are
written directly by the host API. Both remain attributable to a specific
prism session.

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
	auditCmd.Flags().Bool("json", false, "Emit a JSON object with an events array (and a truncated bool with hint when applicable) to stdout instead of the human-readable table")
	rootCmd.AddCommand(auditCmd)
}

// auditDefaultLimit mirrors the implicit default applied by
// db.QueryAuditEvents when limit == 0 and no session filter is set. We
// duplicate the value here so --json can know whether the result is
// likely truncated (i.e. the implicit cap was hit).
const auditDefaultLimit = 20

func runAudit(cmd *cobra.Command, args []string) error {
	days, _ := cmd.Flags().GetInt("days")
	pattern, _ := cmd.Flags().GetString("pattern")
	limit, _ := cmd.Flags().GetInt("limit")
	jsonMode, _ := cmd.Flags().GetBool("json")

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

	if jsonMode {
		return renderAuditEventsJSON(events, sessionName, sinceMs, pattern, limit)
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

// auditEventJSON is the snake_case JSON shape for a single audit event.
type auditEventJSON struct {
	ID          string  `json:"id"`
	SessionName string  `json:"session_name"`
	InstanceID  *string `json:"instance_id"`
	Command     string  `json:"command"`
	Timestamp   string  `json:"timestamp"` // RFC3339
	Payload     any     `json:"payload"`
}

// renderAuditEventsJSON marshals the audit events as a JSON object containing
// an `events` array and a `truncated` bool (with `hint` when truncated).
func renderAuditEventsJSON(events []db.Event, sessionName string, sinceMs int64, pattern string, limit int) error {
	out := make([]auditEventJSON, 0, len(events))
	for _, e := range events {
		var p payload.Audit
		command := ""
		var payloadAny any = nil
		if err := json.Unmarshal([]byte(e.Payload), &p); err == nil {
			command = p.Command
			// Keep the raw payload available so consumers that need
			// additional context (cwd, exit_code, etc.) can read it.
			var generic any
			if err := json.Unmarshal([]byte(e.Payload), &generic); err == nil {
				payloadAny = generic
			}
		}
		row := auditEventJSON{
			ID:          e.ID,
			SessionName: e.SessionName,
			InstanceID:  e.InstanceID,
			Command:     command,
			Timestamp:   e.CreatedAt.UTC().Format(time.RFC3339),
			Payload:     payloadAny,
		}
		out = append(out, row)
	}

	// Truncation heuristic: we hit the cap when the result count equals the
	// effective limit. The DB layer applies an implicit default cap of 20
	// when limit == 0 and no session filter is set.
	effectiveLimit := limit
	if effectiveLimit == 0 && sessionName == "" {
		effectiveLimit = auditDefaultLimit
	}
	truncated := effectiveLimit > 0 && len(events) >= effectiveLimit

	resp := struct {
		Events    []auditEventJSON `json:"events"`
		Truncated bool             `json:"truncated"`
		Hint      *string          `json:"hint"`
	}{
		Events:    out,
		Truncated: truncated,
		Hint:      nil,
	}
	if truncated {
		hint := "results capped — pass --limit=N to raise, or refine with --pattern, --days, or a session argument"
		resp.Hint = &hint
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("audit --json: marshal: %w", err)
	}
	return printJSON(data)
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
	fmt.Println(styleDim.Render(auditWritersSentence()))
}
