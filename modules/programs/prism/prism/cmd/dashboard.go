package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ── theme colours ─────────────────────────────────────────────────────────────
// Injected at build time via ldflags from the Nix module so they match the
// user's active theme. Defaults are gruvbox-dark fallbacks.
var (
	ColorPrimary    = "#d4be98"
	ColorSecondary  = "#a89984"
	ColorPurple     = "#d3869b"
	ColorYellow     = "#d8a657"
	ColorGreen      = "#a9b665"
	ColorBlue       = "#7daea3"
	ColorRed        = "#ea6962"
	ColorForeground = "#d3c6aa"
	ColorBg0        = "#2d353b"
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
	if l, ok := labels[state]; ok {
		return l
	}
	return state
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
	sessions          []tmux.Session
	cursor            int
	cursorInitialised bool // true once we've snapped cursor to currentSession
	width             int
	height            int
	client            string
	popup             bool
	currentSession    string // session the viewing client is in
}

func newDashModel(client string, popup bool) dashModel {
	// CallerSession reads @prism_caller stamped by the tmux binding — the only
	// reliable way to know which session the viewer came from.
	currentSession := tmux.CallerSession()
	inDashSession := currentSession == dashSession || currentSession == ""
	return dashModel{
		client:         client,
		popup:          popup || inDashSession,
		currentSession: currentSession,
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
		m.sessions = filterSessions([]tmux.Session(msg))
		m.currentSession = tmux.CallerSession()
		if !m.cursorInitialised {
			// Snap cursor to the current session on first load.
			m.cursorInitialised = true
			for i, s := range m.sessions {
				if s.Name == m.currentSession {
					m.cursor = i
					break
				}
			}
		}
		if m.cursor >= len(m.sessions) {
			m.cursor = max(0, len(m.sessions)-1)
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc":
			if m.popup {
				// Direct popup mode (C-w): just quit, display-popup -E closes it.
				return m, tea.Quit
			}
			// Persistent session mode (prefix+D): switch caller client back to
			// their previous session. TUI keeps running via the restart loop.
			return m, tea.Sequence(
				func() tea.Msg {
					if client := tmux.CallerClient(); client != "" {
						_ = tmux.SwitchClient(client, tmux.CallerSession())
					}
					return nil
				},
				tea.Quit,
			)

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
					_ = tmux.SelectAgentWindow(selected.Name)
					// Use the caller client stamped at popup-open time —
					// m.client is the process client, not the viewer's client.
					if client := tmux.CallerClient(); client != "" {
						_ = tmux.SwitchClient(client, selected.Name)
					}
					return nil
				},
				tea.Quit,
			)
		}
	}
	return m, nil
}

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

// ── view ──────────────────────────────────────────────────────────────────────

func (m dashModel) View() string {
	if m.width == 0 {
		return ""
	}

	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleHeader := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)
	styleIns := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorGreen))
	styleDel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorRed))

	// Full-width row styles — same pattern as the picker.
	rowW := m.width
	styleRowSelected := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorBg0)).
		Background(lipgloss.Color(ColorPrimary)).
		Bold(true).
		Width(rowW)
	styleRowNormal := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorForeground)).
		Width(rowW)

	// For selected rows the ins/del colours need to be readable on ColorPrimary bg.
	styleInsSelected := lipgloss.NewStyle().
		Foreground(lipgloss.Color(ColorBg0)).
		Background(lipgloss.Color(ColorPrimary)).
		Bold(true)
	styleDelSelected := styleInsSelected

	const sessionW = 28
	const stateW = 10
	const dotW = 2 // ◆/●/space + space

	var sb strings.Builder

	sb.WriteString("\n")
	sb.WriteString(styleHeader.Render(
		fmt.Sprintf(" %-*s%-*s  %-*s  %s", dotW, "", sessionW, "session", stateW, "state", "changes"),
	))
	sb.WriteString("\n\n")

	if len(m.sessions) == 0 {
		sb.WriteString(styleDim.Render("  no active sessions"))
		sb.WriteString("\n")
	}

	for i, s := range m.sessions {
		isHere := s.Name == m.currentSession
		isSelected := i == m.cursor
		isFg := ColorForeground
		if isSelected {
			isFg = ColorBg0
		}

		// dot: ◆ = you are here, ● = someone else attached, space = unattached
		var dot string
		switch {
		case isHere:
			dot = "◆ "
		case s.ClientCount > 0:
			dot = "● "
		default:
			dot = "  "
		}

		// state label coloured by agent state (overridden to bg0 when selected)
		stateColour := stateStyle(s.AgentState).GetForeground()
		if isSelected {
			stateColour = lipgloss.Color(ColorBg0)
		}
		stateStr := lipgloss.NewStyle().
			Foreground(stateColour).
			Background(func() lipgloss.Color {
				if isSelected {
					return lipgloss.Color(ColorPrimary)
				}
				return lipgloss.Color("")
			}()).
			Bold(isSelected).
			Render(fmt.Sprintf("%-*s", stateW, stateLabel(s.AgentState)))

		sessionDisplay := s.Name
		if len(sessionDisplay) > sessionW {
			sessionDisplay = sessionDisplay[:sessionW-1] + "…"
		}

		stat := git.Stat(s.AgentPath)
		var statStr string
		if stat.Files == 0 {
			statStr = lipgloss.NewStyle().Foreground(lipgloss.Color(isFg)).Faint(true).Render("—")
		} else {
			fileWord := "files"
			if stat.Files == 1 {
				fileWord = "file "
			}
			insStyle := styleIns
			delStyle := styleDel
			if isSelected {
				insStyle = styleInsSelected
				delStyle = styleDelSelected
			}
			statStr = fmt.Sprintf("%d %s %s %s",
				stat.Files,
				fileWord,
				insStyle.Render(fmt.Sprintf("+%d", stat.Insertions)),
				delStyle.Render(fmt.Sprintf("-%d", stat.Deletions)),
			)
		}

		// Build the fixed-width portion (dot + session + state) then append stat.
		// We can't include statStr in the Width() render because it contains ANSI
		// escapes that would mess up the width calculation.
		fixed := fmt.Sprintf(" %s%-*s  %s  ", dot, sessionW, sessionDisplay, stateStr)
		var row string
		if isSelected {
			row = styleRowSelected.Render(fixed) + statStr
		} else {
			row = styleRowNormal.Render(fixed) + statStr
		}
		sb.WriteString(row + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(styleDim.Render("  j/k move  enter switch  q close"))
	sb.WriteString("\n")

	return sb.String()
}

// ── cobra command ─────────────────────────────────────────────────────────────

const dashSession = "prism-dashboard"

// ensureDashSession creates the prism-dashboard session if it doesn't exist.
// The session command is a restart loop so it survives the TUI exiting.
func ensureDashSession() error {
	if tmux.HasSession(dashSession) {
		return nil
	}
	c := exec.Command("tmux", "new-session", "-ds", dashSession, "-n", "dashboard",
		"while prism dashboard --popup; do true; done")
	return c.Run()
}

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Live agent status dashboard",
	RunE: func(cmd *cobra.Command, args []string) error {
		popup, _ := cmd.Flags().GetBool("popup")

		// --popup: we are already attached inside the persistent session,
		// just run the TUI directly.
		if popup {
			client, _ := tmux.CurrentClient()
			m := newDashModel(client, popup)
			p := tea.NewProgram(m, tea.WithAltScreen())
			_, err := p.Run()
			return err
		}

		inTmux := os.Getenv("TMUX") != ""

		if inTmux {
			// Inside tmux as a CLI call: ensure session exists, switch to it.
			if err := ensureDashSession(); err != nil {
				return err
			}
			client, _ := tmux.CurrentClient()
			return tmux.SwitchClient(client, dashSession)
		}

		// Outside tmux: ensure session exists, then exec tmux attach so this
		// process is replaced by a full tmux client attached to the dashboard.
		if err := ensureDashSession(); err != nil {
			return err
		}
		return syscallExecTmux(dashSession)
	},
}

// syscallExecTmux replaces the current process with tmux attached to session
// using syscall.Exec so no parent process remains.
func syscallExecTmux(session string) error {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}
	return syscall.Exec(tmuxBin, []string{"tmux", "attach-session", "-t", session}, os.Environ())
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
