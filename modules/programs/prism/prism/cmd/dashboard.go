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
	colorful "github.com/lucasb-eyer/go-colorful"
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

// ── header art ────────────────────────────────────────────────────────────────

// artLines is the DSOTM-inspired prism header: small figlet "PRISM" with a
// triangle whose apex sits above the M and whose right face emits a short
// rainbow spectrum fan. The triangle left face merges with the figlet right edge.
var artLines = []string{
	`                       /\`,
	`                      /  \·───────────`,
	`  ___  ___ ___ ___ __/ __ \·───────────`,
	` | _ \| _ \_ _/ __|  \/  | \·───────────`,
	` |  _/|   /| |\__ \ |\/| |  \·───────────`,
	` |_|  |_|_\___|___/_|  |_|   \·───────────`,
	`                 /____________\`,
}

// artWidth is the width of the widest art line, used to normalise gradient positions.
const artWidth = 42

// rainbowAt returns the everforest spectrum colour at normalised position t ∈ [0,1].
// Uses the 5 available accent colours as stops: red → yellow → green → blue → purple.
// (orange and aqua are not in the ldflags palette, but the interpolation between
// red→yellow and green→blue naturally produces those intermediate hues.)
func rainbowAt(t float64) colorful.Color {
	stops := []colorful.Color{
		mustHex(ColorRed),
		mustHex(ColorYellow),
		mustHex(ColorGreen),
		mustHex(ColorBlue),
		mustHex(ColorPurple),
	}
	scaled := t * float64(len(stops)-1)
	i := int(scaled)
	if i >= len(stops)-1 {
		return stops[len(stops)-1]
	}
	frac := scaled - float64(i)
	r1, g1, b1 := stops[i].R, stops[i].G, stops[i].B
	r2, g2, b2 := stops[i+1].R, stops[i+1].G, stops[i+1].B
	return colorful.Color{
		R: r1 + (r2-r1)*frac,
		G: g1 + (g2-g1)*frac,
		B: b1 + (b2-b1)*frac,
	}
}

func mustHex(h string) colorful.Color {
	c, err := colorful.Hex(h)
	if err != nil {
		return colorful.Color{}
	}
	return c
}

// rainbowLine renders a single art line with a left-to-right rainbow gradient.
// Spaces are passed through unstyled so the background shows through.
func rainbowLine(line string) string {
	var sb strings.Builder
	col := 0
	for _, ch := range line {
		if ch == ' ' {
			sb.WriteRune(' ')
		} else {
			t := float64(col) / float64(artWidth)
			c := rainbowAt(t)
			hex := fmt.Sprintf("#%02x%02x%02x",
				uint8(c.R*255+0.5),
				uint8(c.G*255+0.5),
				uint8(c.B*255+0.5),
			)
			sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render(string(ch)))
		}
		col++
	}
	return sb.String()
}

// renderArt returns the full header art block right-aligned within termWidth.
func renderArt(termWidth int) string {
	// Pad each line on the left so the block sits flush against the right edge.
	leftPad := termWidth - artWidth
	if leftPad < 0 {
		leftPad = 0
	}
	prefix := strings.Repeat(" ", leftPad)
	var sb strings.Builder
	for _, line := range artLines {
		sb.WriteString(prefix)
		sb.WriteString(rainbowLine(line))
		sb.WriteString("\n")
	}
	return sb.String()
}

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
	styleFg := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorForeground))

	const sessionW = 28
	const stateW = 10
	const statW = 22
	const dotW = 2
	// Title gets whatever is left after the fixed columns, min 10.
	titleW := m.width - (1 + dotW + sessionW + 2 + stateW + 2 + statW + 2)
	if titleW < 10 {
		titleW = 0 // not enough room, hide it
	}

	var sb strings.Builder

	// ── header art ──────────────────────────────────────────────────────────
	sb.WriteString(renderArt(m.width))

	// Dim separator between art and column headers.
	sb.WriteString(styleDim.Render(strings.Repeat("─", m.width)))
	sb.WriteString("\n")

	header := fmt.Sprintf(" %-*s%-*s  %-*s  %-*s", dotW, "", sessionW, "session", stateW, "state", statW, "changes")
	if titleW > 0 {
		header += fmt.Sprintf("  %-*s", titleW, "title")
	}
	sb.WriteString(styleHeader.Render(header))
	sb.WriteString("\n\n")

	if len(m.sessions) == 0 {
		sb.WriteString(styleDim.Render("  no active sessions"))
		sb.WriteString("\n")
	}

	for i, s := range m.sessions {
		isHere := s.Name == m.currentSession
		isSelected := i == m.cursor

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

		sessionDisplay := s.Name
		if len(sessionDisplay) > sessionW {
			sessionDisplay = sessionDisplay[:sessionW-1] + "…"
		}

		stat := git.Stat(s.AgentPath)
		var statPlain string
		if stat.Files == 0 {
			statPlain = "—"
		} else {
			fileWord := "files"
			if stat.Files == 1 {
				fileWord = "file "
			}
			statPlain = fmt.Sprintf("%d %s +%d -%d", stat.Files, fileWord, stat.Insertions, stat.Deletions)
		}

		title := s.AgentTitle
		if titleW > 0 && len(title) > titleW {
			title = title[:titleW-1] + "…"
		}

		if isSelected {
			// Bar colour: state colour for active states, primary for idle/finished.
			barBg := lipgloss.Color(ColorPrimary)
			switch s.AgentState {
			case "active", "waiting", "compacting", "error":
				if c, ok := stateStyle(s.AgentState).GetForeground().(lipgloss.Color); ok {
					barBg = c
				}
			}

			plain := fmt.Sprintf(" %s%-*s  %-*s  %-*s",
				dot, sessionW, sessionDisplay, stateW, stateLabel(s.AgentState), statW, statPlain)
			if titleW > 0 {
				plain += fmt.Sprintf("  %s", title)
			}
			row := lipgloss.NewStyle().
				Foreground(lipgloss.Color(ColorBg0)).
				Background(barBg).
				Bold(true).
				Width(m.width).
				Render(plain)
			sb.WriteString(row + "\n")
		} else {
			// Unselected: coloured state + coloured diff, normal fg for the rest.
			stateStr := lipgloss.NewStyle().
				Foreground(stateStyle(s.AgentState).GetForeground()).
				Render(fmt.Sprintf("%-*s", stateW, stateLabel(s.AgentState)))

			var statStr string
			if stat.Files == 0 {
				statStr = styleDim.Render(fmt.Sprintf("%-*s", statW, "—"))
			} else {
				fileWord := "files"
				if stat.Files == 1 {
					fileWord = "file "
				}
				// Build plain portion then pad, append coloured +/- separately.
				plain := fmt.Sprintf("%d %s ", stat.Files, fileWord)
				coloured := styleIns.Render(fmt.Sprintf("+%d", stat.Insertions)) +
					" " + styleDel.Render(fmt.Sprintf("-%d", stat.Deletions))
				// Pad to statW based on visible width of plain + raw +N -N.
				rawStat := fmt.Sprintf("%d %s +%d -%d", stat.Files, fileWord, stat.Insertions, stat.Deletions)
				pad := statW - len(rawStat)
				if pad < 0 {
					pad = 0
				}
				statStr = styleFg.Render(plain) + coloured + strings.Repeat(" ", pad)
			}

			prefix := styleFg.Render(fmt.Sprintf(" %s%-*s  ", dot, sessionW, sessionDisplay))
			row := prefix + stateStr + styleFg.Render("  ") + statStr
			if titleW > 0 && title != "" {
				row += styleDim.Render("  " + title)
			}
			sb.WriteString(row + "\n")
		}
	}

	sb.WriteString("\n")
	hint := "  j/k move  enter switch  q close"
	count := fmt.Sprintf("%d sessions  ", len(m.sessions))
	// Right-align the session count: pad between hint and count to fill m.width.
	pad := m.width - len(hint) - len(count)
	if pad < 1 {
		pad = 1
	}
	sb.WriteString(styleDim.Render(hint + strings.Repeat(" ", pad) + count))
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
