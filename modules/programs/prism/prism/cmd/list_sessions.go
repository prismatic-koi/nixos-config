package cmd

// prism list-sessions — print all agent sessions (excluding infrastructure
// sessions like scratchpad and prism-dashboard) in a human-readable table.
// Useful for scripting and as a standalone companion to `prism checkin`.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/tmux"
)

var listSessionsCmd = &cobra.Command{
	Use:   "list-sessions",
	Short: "List agent sessions with their state and title",
	RunE: func(cmd *cobra.Command, args []string) error {
		sessions, err := tmux.Sessions()
		if err != nil {
			return err
		}
		sessions = filterStatusSessions(sessions)

		if len(sessions) == 0 {
			fmt.Println("no agent sessions found")
			return nil
		}

		styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
		styleName := lipgloss.NewStyle().Bold(true)
		styleTitle := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

		fmt.Println(styleHeader.Render(fmt.Sprintf("%-40s  %-8s  %s", "SESSION", "STATE", "TITLE")))
		for _, s := range sessions {
			state := s.AgentState
			if state == "" {
				state = "idle"
			}
			title := s.AgentTitle
			if title == "" {
				title = "—"
			}
			if len(title) > 60 {
				title = title[:57] + "..."
			}
			// Colour the state field.
			var stateColour string
			switch state {
			case "active":
				stateColour = ColorPurple
			case "waiting":
				stateColour = ColorYellow
			case "finished":
				stateColour = ColorGreen
			default:
				stateColour = ColorSecondary
			}
			stateStyled := lipgloss.NewStyle().Foreground(lipgloss.Color(stateColour)).
				Render(fmt.Sprintf("%-8s", state))

			// Only bold worktree sessions (project@branch).
			nameStyle := styleTitle
			if strings.Contains(s.Name, "@") {
				nameStyle = styleName
			}

			fmt.Printf("%s  %s  %s\n",
				nameStyle.Render(fmt.Sprintf("%-40s", s.Name)),
				stateStyled,
				styleTitle.Render(title),
			)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(listSessionsCmd)
}
