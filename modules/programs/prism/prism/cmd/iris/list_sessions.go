package main

// list_sessions.go — `iris sessions list` implementation.
//
// Renders the sessions_snapshot from the iris daemon as either a human-readable
// table or a JSON array (--json). The JSON shape is the stable contract for
// scripting and parity tests (#1669):
//
//	[
//	  {
//	    "id": "<full uuid>",
//	    "state": "active",
//	    "role": "coordinator",
//	    "worktree": "/full/path/to/worktree",
//	    "harness_session_id": "/home/ben/.pi/agent/.../uuid.jsonl",
//	    "created_at": "2026-05-16T10:05:48Z",   // RFC3339
//	    "uptime_seconds": 201
//	  },
//	  ...
//	]
//
// Empty list renders as `[]` in JSON and as just the header row in the table.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

// sessionListJSONRow is the stable JSON shape for `iris sessions list --json`.
// Field names and types are AC-fixed: renames and removals are breaking
// changes. Adding fields is fine.
type sessionListJSONRow struct {
	ID               string `json:"id"`
	State            string `json:"state"`
	Role             string `json:"role"`
	Worktree         string `json:"worktree"`
	HarnessSessionID string `json:"harness_session_id"`
	CreatedAt        string `json:"created_at"`     // RFC3339
	UptimeSeconds    int64  `json:"uptime_seconds"` // wall-clock since created_at
	// Name is informational — useful for cross-referencing with the TUI
	// and `iris spawn` output. Not in the AC's minimum field set but
	// stable nonetheless.
	Name string `json:"name"`
}

// Lipgloss colours: mirror the iris TUI palette so the CLI feels at home
// with the TUI. Hard-coded here (rather than imported) because the TUI's
// colour constants are package-private to internal/iris/tui.
const (
	cliColPrimary   = "#d79921" // yellow
	cliColSecondary = "#928374" // grey
)

// runSessionsList is the cobra RunE for `iris sessions list`.
func runSessionsList(cmd *cobra.Command, _ []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	sockPath := resolveSocketPath(cmd)

	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()

	snap, err := fetchSessionsSnapshot(ctx, sockPath)
	if err != nil {
		return err
	}

	if jsonMode {
		return renderSessionsListJSON(cmd.OutOrStdout(), snap.Sessions, time.Now())
	}
	return renderSessionsListTable(cmd.OutOrStdout(), snap.Sessions, time.Now())
}

// renderSessionsListJSON writes the stable JSON-array form of the snapshot to w.
// `sessions` may be nil/empty — the output is then `[]`.
//
// `now` is parameterised so tests can pin uptime_seconds to a deterministic
// value. Production callers pass time.Now().
func renderSessionsListJSON(w io.Writer, sessions []iris.SessionSnapshot, now time.Time) error {
	rows := make([]sessionListJSONRow, 0, len(sessions))
	for _, s := range sessions {
		var uptime int64
		if t, err := time.Parse(time.RFC3339, s.StartedAt); err == nil {
			d := now.Sub(t)
			if d < 0 {
				d = 0
			}
			uptime = int64(d.Seconds())
		}
		rows = append(rows, sessionListJSONRow{
			ID:               s.InstanceID,
			State:            s.State,
			Role:             s.Role,
			Worktree:         s.Worktree,
			HarnessSessionID: s.HarnessSessionID,
			CreatedAt:        s.StartedAt,
			UptimeSeconds:    uptime,
			Name:             s.Name,
		})
	}
	// Marshal indented for readability — humans pipe `--json` into `jq`
	// often enough that pretty-printing pays for itself, and parsers don't
	// care.
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return fmt.Errorf("iris sessions list --json: marshal: %w", err)
	}
	if _, err := fmt.Fprintln(w, string(data)); err != nil {
		return err
	}
	return nil
}

// renderSessionsListTable writes the human-readable table to w.
//
// Column widths are fixed so the output is grep-able. The SESSION column
// shows the logical session name (e.g. `hass-config/main`) — this is the
// disambiguating identifier across worktrees and the value users pass to
// `iris prompt`, `iris kill`, etc. The instance UUID remains available in
// the --json output (`id` field). WORKTREE shows the worktree basename;
// the full path is in --json. See issue #1738 for the SESSION-column
// rationale: the previous UUID-prefix display was opaque and gave users
// no way to map a row back to a worktree at a glance.
func renderSessionsListTable(w io.Writer, sessions []iris.SessionSnapshot, now time.Time) error {
	const (
		wSession  = 32
		wState    = 10
		wRole     = 12
		wWorktree = 24
		wUptime   = 10
	)

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cliColSecondary))
	header := fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %-*s",
		wSession, "SESSION",
		wState, "STATE",
		wRole, "ROLE",
		wWorktree, "WORKTREE",
		wUptime, "UPTIME",
	)
	if _, err := fmt.Fprintln(w, styleHeader.Render(header)); err != nil {
		return err
	}

	if len(sessions) == 0 {
		// AC: empty list → header only, no body, no "no sessions" message.
		return nil
	}

	stateStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(cliColPrimary))
	for _, s := range sessions {
		name := truncateForColumn(s.Name, wSession)
		state := truncateForColumn(s.State, wState)
		role := truncateForColumn(s.Role, wRole)
		worktreeBase := filepath.Base(s.Worktree)
		if worktreeBase == "." || worktreeBase == "/" {
			// Defensive: don't render an opaque "." for unusual paths.
			worktreeBase = s.Worktree
		}
		worktree := truncateForColumn(worktreeBase, wWorktree)
		uptime := formatUptime(s.StartedAt, now)

		_, err := fmt.Fprintf(w, "%-*s  %s  %-*s  %-*s  %-*s\n",
			wSession, name,
			stateStyle.Render(fmt.Sprintf("%-*s", wState, state)),
			wRole, role,
			wWorktree, worktree,
			wUptime, uptime,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// truncateForColumn truncates s to maxLen runes, appending "…" if it had to
// drop characters. Returns the literal s when already short enough.
func truncateForColumn(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 2 {
		return s[:maxLen]
	}
	return s[:maxLen-1] + "…"
}

// formatUptime renders the wall-clock duration since startedAt as a compact
// string (`3m21s`, `1h04m`, `42s`). Returns "—" when startedAt is unparseable
// or in the future.
func formatUptime(startedAt string, now time.Time) string {
	if startedAt == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return "—"
	}
	d := now.Sub(t)
	if d < 0 {
		return "—"
	}
	if d < time.Second {
		return "<1s"
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh%02dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm%02ds", mins, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

