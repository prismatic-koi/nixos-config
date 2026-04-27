package cmd

// checkin_legacy.go — legacy screen-scrape fallback path for sessions that
// have no DB rows. Kept for backward compatibility.

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// runCheckinSessionLegacy is the old screen-scrape path, kept as a fallback.
func runCheckinSessionLegacy(session string, height int) error {
	if !tmux.HasSession(session) {
		return fmt.Errorf("session %q not found\nrun `prism list-sessions` to see available sessions", session)
	}

	result, err := tmux.CapturePaneScreen(session, height)
	if err != nil {
		return fmt.Errorf("checkin %s: %w", session, err)
	}

	state := tmux.AgentStateOf(session)
	if state == "" {
		state = string(agent.StateIdle)
	}

	styleBold := lipgloss.NewStyle().Bold(true)
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleState := stateStyle(state)

	fmt.Printf("%s %s\n\n", styleBold.Render("checkin:"), session)
	fmt.Printf("state: %s\n\n", styleState.Render(state))
	if result.Screen != "" {
		fmt.Println(result.Screen)
		fmt.Println()
	}
	fmt.Println(styleDim.Render("── end of screen capture ──"))
	return nil
}
