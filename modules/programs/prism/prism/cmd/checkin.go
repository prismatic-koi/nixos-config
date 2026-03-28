package cmd

// prism checkin — capture the live screen of a session's agent window and
// print it as clean plain text, suitable for reading by another agent.
//
// Usage:
//
//	prism checkin <session>   capture the named session
//	prism checkin             list available sessions and exit with a hint

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/tmux"
)

var checkinCmd = &cobra.Command{
	Use:   "checkin [session]",
	Short: "Capture the live screen of a session's agent window",
	Long: `Capture and print the current screen of the agent window in the named
tmux session. Output is cleaned up (borders, scrollbar chrome stripped) so
it can be read directly by another agent.

With no argument, lists available sessions so you can pick one.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCheckin,
}

func init() {
	rootCmd.AddCommand(checkinCmd)
}

func runCheckin(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return runCheckinNoArg()
	}
	return runCheckinSession(args[0])
}

func runCheckinSession(session string) error {
	// Verify the session exists.
	if !tmux.HasSession(session) {
		return fmt.Errorf("session %q not found\nrun `prism list-sessions` to see available sessions", session)
	}

	screen, err := tmux.CapturePaneScreen(session)
	if err != nil {
		return fmt.Errorf("checkin %s: %w", session, err)
	}

	styleBold := lipgloss.NewStyle().Bold(true)
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	fmt.Printf("%s %s\n\n", styleBold.Render("checkin:"), session)
	fmt.Println(screen)
	fmt.Println()
	fmt.Println(styleDim.Render("── end of screen capture ──"))
	return nil
}

func runCheckinNoArg() error {
	sessions, err := tmux.Sessions()
	if err != nil {
		return err
	}
	sessions = filterStatusSessions(sessions)

	if len(sessions) == 0 {
		return fmt.Errorf("no agent sessions found")
	}

	styleHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ColorSecondary))
	styleName := lipgloss.NewStyle().Bold(true)
	styleState := lipgloss.NewStyle()
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
		// Truncate long titles.
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		fmt.Printf("%s  %s  %s\n",
			styleName.Render(fmt.Sprintf("%-40s", s.Name)),
			styleState.Render(fmt.Sprintf("%-8s", state)),
			styleTitle.Render(title),
		)
	}

	fmt.Println()
	hint := "run `prism checkin <session>` to inspect a session"
	fmt.Println(lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary)).Render(hint))

	// Check if all sessions look like they have no agent window — give a
	// cleaner hint if the caller is clearly not in a worktree session.
	var worktreeSessions []string
	for _, s := range sessions {
		if strings.Contains(s.Name, "@") {
			worktreeSessions = append(worktreeSessions, s.Name)
		}
	}
	if len(worktreeSessions) == 0 {
		fmt.Println(lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary)).
			Render("hint: checkin works on project@branch sessions spawned by `prism spawn`"))
	}

	return fmt.Errorf("no session specified")
}
