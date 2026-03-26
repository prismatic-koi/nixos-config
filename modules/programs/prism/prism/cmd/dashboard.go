package cmd

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ── theme colours ─────────────────────────────────────────────────────────────
// These are injected at build time via ldflags from the Nix module so they
// match the user's active theme. Defaults are gruvbox-dark fallbacks.
var (
	ColorPrimary   = "#d4be98"
	ColorSecondary = "#a89984"
	ColorPurple    = "#d3869b"
	ColorYellow    = "#d8a657"
	ColorGreen     = "#a9b665"
	ColorBlue      = "#7daea3"
	ColorRed       = "#ea6962"
	ColorBg1       = "#3c3836"
)

// ── styles ────────────────────────────────────────────────────────────────────

func stateStyle(state string) lipgloss.Style {
	color := ColorSecondary
	switch state {
	case "active":
		color = ColorPurple
	case "waiting":
		color = ColorYellow
	case "finished":
		color = ColorGreen
	case "compacting":
		color = ColorBlue
	case "error":
		color = ColorRed
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color))
}

func stateLabel(state string) string {
	labels := map[string]string{
		"active":     "active",
		"waiting":    "waiting",
		"finished":   "finished",
		"compacting": "compacting",
		"error":      "error",
		"":           "idle",
	}
	l, ok := labels[state]
	if !ok {
		return state
	}
	return l
}

// ── messages ──────────────────────────────────────────────────────────────────

type tickMsg time.Time
type sessionsMsg []tmux.Session

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func fetchSessions() tea.Msg {
	sessions, err := tmux.Sessions()
	if err != nil {
		return sessionsMsg(nil)
	}
	return sessionsMsg(sessions)
}

// ── model ─────────────────────────────────────────────────────────────────────

type dashModel struct {
	sessions []tmux.Session
	cursor   int
	width    int
	height   int
	client   string // tmux client name for switch-client
	popup    bool   // running inside display-popup (affects quit behaviour)
}

func newDashModel(client string, popup bool) dashModel {
	return dashModel{
		client: client,
		popup:  popup,
	}
}

func (m dashModel) Init() tea.Cmd {
	return tea.Batch(fetchSessions, tick())
}

func (m dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		return m, tea.Batch(fetchSessions, tick())

	case sessionsMsg:
		// Preserve cursor position across refreshes
		m.sessions = filterSessions([]tmux.Session(msg))
		if m.cursor >= len(m.sessions) {
			m.cursor = max(0, len(m.sessions)-1)
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc":
			return m, tea.Quit

		case "j", "down":
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
			}

		case "k", "up":
			if m.cursor > 0 {
				m.cursor--
			}

		case "enter":
			if len(m.sessions) == 0 {
				return m, nil
			}
			selected := m.sessions[m.cursor]
			return m, tea.Sequence(
				func() tea.Msg {
					// Select agent window in target session
					_ = tmux.SelectAgentWindow(selected.Name)
					// Switch calling client to target session
					if m.client != "" {
						_ = tmux.SwitchClient(m.client, selected.Name)
					}
					return nil
				},
				tea.Quit,
			)
		}
	}
	return m, nil
}

// filterSessions separates project sessions from scratchpad/dashboard.
func filterSessions(all []tmux.Session) []tmux.Session {
	var out []tmux.Session
	for _, s := range all {
		if s.Name == "scratchpad" || s.Name == "prism-dashboard" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (m dashModel) View() string {
	if m.width == 0 {
		return ""
	}

	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorPrimary)).
		Bold(true)
	dim := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorSecondary))
	cursor := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorPrimary)).
		Bold(true)

	const sessionW = 32
	const stateW = 10
	filesW := max(m.width-sessionW-stateW-8, 10)

	divider := dim.Render(strings.Repeat("─", min(m.width, sessionW+stateW+filesW+4)))

	var sb strings.Builder

	sb.WriteString("\n")
	sb.WriteString(header.Render(
		fmt.Sprintf("  %-*s  %-*s  %s", sessionW, "session", stateW, "state", "changed files"),
	))
	sb.WriteString("\n")
	sb.WriteString(divider)
	sb.WriteString("\n")

	if len(m.sessions) == 0 {
		sb.WriteString(dim.Render("  no active sessions"))
		sb.WriteString("\n")
	}

	for i, s := range m.sessions {
		sStyle := stateStyle(s.AgentState)
		label := fmt.Sprintf("%-*s", stateW, stateLabel(s.AgentState))

		files := git.ChangedFiles(s.AgentPath)
		var filesStr string
		if len(files) == 0 {
			filesStr = dim.Render("—")
		} else {
			shown := files
			extra := 0
			if len(shown) > 6 {
				shown = shown[:6]
				extra = len(files) - 6
			}
			filesStr = strings.Join(shown, ", ")
			if extra > 0 {
				filesStr += fmt.Sprintf(" +%d", extra)
			}
		}
		if len(filesStr) > filesW {
			filesStr = filesStr[:filesW-1] + "…"
		}

		sessionDisplay := s.Name
		if len(sessionDisplay) > sessionW {
			sessionDisplay = sessionDisplay[:sessionW-1] + "…"
		}

		var prefix string
		if i == m.cursor {
			prefix = cursor.Render("> ")
		} else {
			prefix = "  "
		}

		line := fmt.Sprintf("%s%s  %s  %s",
			prefix,
			sStyle.Render(fmt.Sprintf("%-*s", sessionW, sessionDisplay)),
			sStyle.Render(label),
			filesStr,
		)
		sb.WriteString(line)
		sb.WriteString("\n")
	}

	sb.WriteString(divider)
	sb.WriteString("\n")
	sb.WriteString(dim.Render("  j/k move  enter switch  q close"))
	sb.WriteString("\n")

	return sb.String()
}

// ── cobra command ──────────────────────────────────────────────────────────────

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Live agent status dashboard",
	Long:  "Show a live-updating dashboard of all prism sessions and their agent states.",
	RunE: func(cmd *cobra.Command, args []string) error {
		popup, _ := cmd.Flags().GetBool("popup")

		client, _ := tmux.CurrentClient()

		m := newDashModel(client, popup)
		p := tea.NewProgram(m, tea.WithAltScreen())
		_, err := p.Run()
		return err
	},
}

func init() {
	dashboardCmd.Flags().Bool("popup", false, "Running inside a tmux display-popup")
	rootCmd.AddCommand(dashboardCmd)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
