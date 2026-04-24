package cmd

// prism sessions — general query surface over the sessions table.
//
// Usage:
//
//	prism sessions list                  tabular listing of all sessions rows
//	prism sessions list --repo <name>    filter by repo
//	prism sessions list --since <date>   filter by started_at
//	prism sessions list --json           emit one JSON object per line (JSONL)

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Query the sessions table",
	Long:  `General query surface over the sessions table. Use subcommands to list sessions.`,
}

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all session incarnations",
	Long: `List all rows in the sessions table as a tabular display.

Use --repo to filter by repo name, --since to filter by start date, and
--json to emit one JSON object per line (JSONL) for scripting.`,
	Args: cobra.NoArgs,
	RunE: runSessionsList,
}

func init() {
	sessionsListCmd.Flags().String("repo", "", "Filter by repo name")
	sessionsListCmd.Flags().String("since", "", "Filter by started_at >= date (ISO 8601 or YYYY-MM-DD)")
	sessionsListCmd.Flags().Bool("json", false, "Emit one JSON object per line (JSONL)")
	sessionsCmd.AddCommand(sessionsListCmd)
	rootCmd.AddCommand(sessionsCmd)
}

// sessionJSONRow is the JSONL schema for one sessions row.
type sessionJSONRow struct {
	InstanceID       string  `json:"instanceId"`
	SessionName      string  `json:"sessionName"`
	AgentRole        *string `json:"agentRole"`
	RootAgentName    *string `json:"rootAgentName"`
	Repo             string  `json:"repo"`
	Worktree         string  `json:"worktree"`
	Harness          string  `json:"harness"`
	HarnessSessionID *string `json:"harnessSessionId,omitempty"`
	GroupID          *string `json:"groupId,omitempty"`
	StartedAt        string  `json:"startedAt"` // RFC3339
	EndedAt          *string `json:"endedAt,omitempty"`
	EndState         *string `json:"endState,omitempty"`
	ArchivePath      *string `json:"archivePath,omitempty"`
	PrismVersion     *string `json:"prismVersion,omitempty"`
}

func runSessionsList(cmd *cobra.Command, _ []string) error {
	repoFilter, _ := cmd.Flags().GetString("repo")
	sinceStr, _ := cmd.Flags().GetString("since")
	jsonMode, _ := cmd.Flags().GetBool("json")

	sinceMs, err := parseSinceFlag(sinceStr)
	if err != nil {
		return err
	}

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("sessions list: %w", err)
	}
	defer d.Close()

	var sessions []db.Session
	switch {
	case repoFilter != "" && sinceMs > 0:
		sessions, err = d.SessionsForRepoSince(repoFilter, sinceMs)
	case repoFilter != "":
		sessions, err = d.SessionsForRepo(repoFilter)
	case sinceMs > 0:
		sessions, err = d.SessionsSince(sinceMs)
	default:
		sessions, err = d.AllSessions()
	}
	if err != nil {
		return fmt.Errorf("sessions list: %w", err)
	}

	if jsonMode {
		return renderSessionsListJSON(sessions)
	}

	return renderSessionsListTable(sessions)
}

func renderSessionsListJSON(sessions []db.Session) error {
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
		b, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("sessions list: marshal: %w", err)
		}
		fmt.Println(string(b))
	}
	return nil
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
