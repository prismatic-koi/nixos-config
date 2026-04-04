// Package dashboard implements the prism live-agent-status dashboard TUI.
// This file contains colour variables and lipgloss style helpers.
package dashboard

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/config"
)

// Theme colour vars loaded from config at package init.
// Gruvbox-dark defaults are used when no config file is present.
var (
	ColorPrimary    string
	ColorSecondary  string
	ColorPurple     string
	ColorYellow     string
	ColorGreen      string
	ColorBlue       string
	ColorRed        string
	ColorForeground string
	ColorBg0        string
)

func init() {
	cfg := config.Load()
	ColorPrimary = cfg.ColorPrimary
	ColorSecondary = cfg.ColorSecondary
	ColorPurple = cfg.ColorPurple
	ColorYellow = cfg.ColorYellow
	ColorGreen = cfg.ColorGreen
	ColorBlue = cfg.ColorBlue
	ColorRed = cfg.ColorRed
	ColorForeground = cfg.ColorForeground
	ColorBg0 = cfg.ColorBg0
}

// StateStyle returns a lipgloss style with the foreground colour for a given
// agent state string.
func StateStyle(state string) lipgloss.Style {
	return stateStyle(state)
}

// stateStyle is the internal implementation used within the package.
func stateStyle(state string) lipgloss.Style {
	color := ColorSecondary
	switch agent.AgentState(state) {
	case agent.StateActive:
		color = ColorPurple
	case agent.StateWaiting:
		color = ColorYellow
	case agent.StateFinished:
		color = ColorGreen
	case agent.StateInterrupted:
		color = ColorRed
	case agent.StateCompacting:
		color = ColorBlue
	case agent.StateError:
		color = ColorRed
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color))
}

// stateLabel returns a human-readable label for an agent state string.
func stateLabel(state string) string {
	labels := map[agent.AgentState]string{
		agent.StateActive:      "active",
		agent.StateWaiting:     "waiting",
		agent.StateFinished:    "finished",
		agent.StateInterrupted: "interrupted",
		agent.StateCompacting:  "compacting",
		agent.StateError:       "error",
		"":                     "idle",
	}
	if l, ok := labels[agent.AgentState(state)]; ok {
		return l
	}
	return state
}
