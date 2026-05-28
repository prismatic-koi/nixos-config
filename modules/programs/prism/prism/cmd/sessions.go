package cmd

// prism sessions — subcommand group for session-related queries.
//
// Canonical commands:
//
//	prism sessions list               active session table (human-readable)
//	prism sessions list --all         show all repos (default: current repo only)
//	prism sessions list --json        JSON array of session-status objects
//	prism sessions status             counts by state
//	prism sessions status --tmux-format  tmux colour-formatted output
//	prism sessions status --waiting   only the waiting count
//	prism sessions status --json      JSON object keyed by state
//	prism sessions session-status --tmux-format  per-session tmux status segment
//
// Hidden backward-compat aliases:
//
//	prism list-sessions   → prism sessions list
//	prism status          → prism sessions status

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmuxstatus"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Query and inspect agent sessions",
	Long:  `Subcommand group for session-related queries. Use 'sessions list' for the active session table and 'sessions status' for state counts.`,
}

// sessionsListCmd is the canonical form of prism list-sessions.
// It lists active agent sessions with their state and title.
// prism list-sessions is kept as a hidden top-level alias.
var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List agent sessions with their state and title",
	RunE:  runListSessions,
}

// sessionsStatusCmd is the canonical form of prism status.
// It prints agent session counts by state.
// prism status is kept as a hidden top-level alias.
var sessionsStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print agent session counts",
	Long: `Print counts of agent sessions by state.

Without flags, prints a human-readable summary of all states.
Use --waiting to restrict output to the waiting count,
--tmux-format to emit a tmux-style colour-formatted string
suitable for embedding in status-right, or --json to emit a
JSON object keyed by state with integer counts.

--json and --tmux-format are mutually exclusive.`,
	RunE: runStatus,
}

// sessionsSessionStatusCmd renders a per-session tmux status segment. Today
// it only encodes the muted flag; the surface is designed to grow with
// additional per-session indicators without changing the tmux-side wiring.
var sessionsSessionStatusCmd = &cobra.Command{
	Use:   "session-status [session]",
	Short: "Emit a per-session tmux status segment",
	Long: `Emit a per-session tmux status-right segment for the session named in
PRISM_SESSION_NAME (or the positional argument when given).

With --tmux-format, the output uses tmux #[bg=...,fg=...] colour escapes.
With no targetable session, the output is empty and exit code is 0 — the
graceful-degradation contract matching FormatWaiting.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSessionStatus,
}

func init() {
	sessionsListCmd.Flags().BoolP("all", "A", false, "List ALL sessions across all repos, including other repos' workers.\nBy default the listing already includes other repos' coordinators — only their workers are hidden.")
	sessionsListCmd.Flags().Bool("json", false, "Emit structured JSON (array of session objects) to stdout instead of the human-readable table")

	sessionsStatusCmd.Flags().Bool("waiting", false, "Only output the waiting count")
	sessionsStatusCmd.Flags().Bool("tmux-format", false, "Emit tmux #[fg=...] colour-formatted output")
	sessionsStatusCmd.Flags().Bool("json", false, "Emit a JSON object keyed by state with integer counts (mutually exclusive with --tmux-format)")

	sessionsSessionStatusCmd.Flags().Bool("tmux-format", false, "Emit tmux #[bg=...,fg=...] colour-formatted output (the only currently-supported format)")

	sessionsCmd.AddCommand(sessionsListCmd)
	sessionsCmd.AddCommand(sessionsStatusCmd)
	sessionsCmd.AddCommand(sessionsSessionStatusCmd)
	rootCmd.AddCommand(sessionsCmd)
}

// runSessionStatus is the RunE for `prism sessions session-status`.
//
// Resolution order for the target session:
//   1. Positional argument when given.
//   2. PRISM_SESSION_NAME environment variable.
//   3. Empty — emit "" and exit 0 (graceful-degradation contract).
//
// A DB-lookup failure (DB closed, session not in DB) also degrades to empty
// stdout, never to an error. The status bar must never display an error.
func runSessionStatus(cmd *cobra.Command, args []string) error {
	var target string
	switch len(args) {
	case 1:
		target = args[0]
	case 0:
		target = os.Getenv("PRISM_SESSION_NAME")
	}
	if target == "" {
		return nil
	}

	d, err := openDB()
	if err != nil {
		return nil
	}
	defer d.Close()

	st, err := d.CurrentStatus(target)
	if err != nil || st == nil {
		return nil
	}

	seg := tmuxstatus.FormatSessionStatus(
		tmuxstatus.SessionStatus{Name: target, Muted: st.Muted},
		sessionStatusColors(),
	)
	if seg != "" {
		fmt.Print(seg)
	}
	return nil
}

// sessionStatusColors returns the palette slice used by FormatSessionStatus.
// Wrapped so the formatter in internal/tmuxstatus stays free of the
// cmd-package theme globals.
func sessionStatusColors() tmuxstatus.Colors {
	return tmuxstatus.Colors{
		Yellow:  ColorYellow,
		Purple:  ColorPurple,
		Green:   ColorGreen,
		Red:     ColorRed,
		Primary: ColorPrimary,
		Bg0:     ColorBg0,
	}
}

// sessionJSONRow is the snake_case JSON schema for a single sessions row.
// Null is preferred over omitting optional keys so consumers see a stable
// key set across rows.
type sessionJSONRow struct {
	InstanceID       string  `json:"instance_id"`
	SessionName      string  `json:"session_name"`
	AgentRole        *string `json:"agent_role"`
	RootAgentName    *string `json:"root_agent_name"`
	Repo             string  `json:"repo"`
	Worktree         string  `json:"worktree"`
	Harness          string  `json:"harness"`
	HarnessSessionID *string `json:"harness_session_id"`
	GroupID          *string `json:"group_id"`
	StartedAt        string  `json:"started_at"` // RFC3339
	EndedAt          *string `json:"ended_at"`
	EndState         *string `json:"end_state"`
	ArchivePath      *string `json:"archive_path"`
	PrismVersion     *string `json:"prism_version"`
}

func renderSessionsListJSON(sessions []db.Session) error {
	rows := make([]sessionJSONRow, 0, len(sessions))
	for _, s := range sessions {
		row := sessionJSONRow{
			InstanceID:       s.InstanceID,
			SessionName:      s.SessionName,
			AgentRole:        s.AgentRole,
			RootAgentName:    s.RootAgentName,
			Repo:             s.Repo,
			Worktree:         s.Worktree,
			Harness:          s.Harness,
			HarnessSessionID: s.HarnessSessionID,
			GroupID:          s.GroupID,
			StartedAt:        s.StartedAt.UTC().Format(time.RFC3339),
			EndState:         s.EndState,
			ArchivePath:      s.ArchivePath,
			PrismVersion:     s.PrismVersion,
		}
		if s.EndedAt != nil {
			endedStr := s.EndedAt.UTC().Format(time.RFC3339)
			row.EndedAt = &endedStr
		}
		rows = append(rows, row)
	}
	out := struct {
		Sessions  []sessionJSONRow `json:"sessions"`
		Truncated bool             `json:"truncated"`
	}{
		Sessions:  rows,
		Truncated: false, // sessions list is never implicitly capped
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("sessions list: marshal: %w", err)
	}
	return printJSON(data)
}

func renderSessionsListTable(sessions []db.Session) error {
	if len(sessions) == 0 {
		fmt.Println("no sessions yet")
		return nil
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	now := time.Now()

	const (
		wID      = 8
		wName    = 32
		wRepo    = 20
		wAgent   = 14
		wState   = 10
		wStarted = 19
		wDur     = 10
	)

	header := fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %-*s  %-*s  %-*s",
		wID, "INSTANCE",
		wName, "SESSION_NAME",
		wRepo, "REPO",
		wAgent, "AGENT",
		wState, "STATE",
		wStarted, "STARTED",
		wDur, "DURATION",
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

		repoStr := s.Repo
		if len(repoStr) > wRepo {
			repoStr = repoStr[:wRepo-3] + "..."
		}

		agentName := "—"
		if s.RootAgentName != nil && *s.RootAgentName != "" {
			agentName = *s.RootAgentName
		} else if s.AgentRole != nil && *s.AgentRole != "" {
			agentName = *s.AgentRole
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

		startedStr := s.StartedAt.Format("2006-01-02 15:04:05")

		var dur time.Duration
		if s.EndedAt != nil {
			dur = s.EndedAt.Sub(s.StartedAt)
		} else {
			dur = now.Sub(s.StartedAt)
		}
		durStr := formatDurationLong(dur)

		stateStyled := stateStyle(state).Render(fmt.Sprintf("%-*s", wState, truncateStr(state, wState)))

		fmt.Printf("%-*s  %-*s  %-*s  %-*s  %s  %-*s  %-*s\n",
			wID, shortID,
			wName, sessionName,
			wRepo, repoStr,
			wAgent, agentName,
			stateStyled,
			wStarted, startedStr,
			wDur, durStr,
		)
	}

	fmt.Println()
	fmt.Println(styleDim.Render("run `prism stats <instance-id>` for full metrics on a specific incarnation"))
	return nil
}
