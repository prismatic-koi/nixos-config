package cmd

// prism checkin — capture the live screen of a session's agent window and
// print it as clean plain text, suitable for reading by another agent.
//
// Usage:
//
//	prism checkin <session>                       capture with default height (100 rows)
//	prism checkin <session> --height N            capture with N rows
//	prism checkin <session> --kill-client         kill attached clients first for a full capture
//	prism checkin                                 list available sessions and exit with a hint

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

The window is temporarily expanded to --height rows before capturing. When no
clients are attached to the session the TUI reflows to fill the full height,
giving a much richer capture. If a client is attached the capture will be
limited to what the client's window size allows — a warning is printed so the
calling agent knows the output may be truncated.

Use --kill-client to disconnect any attached clients before capturing. This
gives the full picture but closes the user's view of the session.

With no argument, lists available sessions so you can pick one.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCheckin,
}

func init() {
	checkinCmd.Flags().Int("height", tmux.DefaultCaptureHeight, "Number of rows to expand the window to before capturing (default 100)")
	checkinCmd.Flags().Bool("kill-client", false, "Disconnect attached clients before capturing for a full capture")
	rootCmd.AddCommand(checkinCmd)
}

func runCheckin(cmd *cobra.Command, args []string) error {
	height, _ := cmd.Flags().GetInt("height")
	killClient, _ := cmd.Flags().GetBool("kill-client")
	if len(args) == 0 {
		return runCheckinNoArg()
	}
	return runCheckinSession(args[0], height, killClient)
}

func runCheckinSession(session string, height int, killClient bool) error {
	if !tmux.HasSession(session) {
		return fmt.Errorf("session %q not found\nrun `prism list-sessions` to see available sessions", session)
	}

	if killClient {
		if err := tmux.KillSessionClients(session); err != nil {
			return fmt.Errorf("kill clients: %w", err)
		}
	}

	result, err := tmux.CapturePaneScreen(session, height)
	if err != nil {
		return fmt.Errorf("checkin %s: %w", session, err)
	}

	styleBold := lipgloss.NewStyle().Bold(true)
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleWarn := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorYellow))

	fmt.Printf("%s %s\n\n", styleBold.Render("checkin:"), session)

	if len(result.ClientsAttached) > 0 {
		fmt.Println(styleWarn.Render(fmt.Sprintf(
			"warning: %d client(s) attached (%s) — capture may be limited to their window size.\n"+
				"For a full capture, re-run with --kill-client (this will close the user's view).",
			len(result.ClientsAttached),
			strings.Join(result.ClientsAttached, ", "),
		)))
		fmt.Println()
	}

	fmt.Println(result.Screen)
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
