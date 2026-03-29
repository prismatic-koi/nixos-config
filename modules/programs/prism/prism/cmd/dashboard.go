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
	`                      /\`,
	`                     /  \·───────────`,
	` ___  ___ ___ ___ __/ __ \·───────────`,
	`| _ \| _ \_ _/ __|  \/  | \·───────────`,
	`|  _/|   /| |\__ \ |\/| |  \·───────────`,
	`|_|  |_|_\___|___/_|  |_|   \·───────────`,
	`                /____________\`,
}

// artWidth is the width of the widest art line, used to normalise gradient positions.
const artWidth = 41

// artHeight is the number of lines in the art block.
const artHeight = 7

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

// rainbowLine renders a single art line with a left-to-right rainbow gradient
// normalised over artWidth columns.
func rainbowLine(line string) string {
	return rainbowLineWidth(line, artWidth)
}

// rainbowLineWidth renders a string with a rainbow gradient spread across
// totalWidth columns, so short strings like "PRISM" get the full spectrum.
func rainbowLineWidth(line string, totalWidth int) string {
	if totalWidth < 2 {
		totalWidth = 2
	}
	var sb strings.Builder
	col := 0
	for _, ch := range line {
		if ch == ' ' {
			sb.WriteRune(' ')
		} else {
			t := float64(col) / float64(totalWidth-1)
			if t > 1 {
				t = 1
			}
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

// renderHeader composites the stats panel (left) with the art block (right)
// into a single header string that fills termWidth on each line.
// When the terminal is too short, a compact 2-line header is used instead.
func renderHeader(m dashModel, styleDim, styleIns, styleDel lipgloss.Style) string {
	// ── compute stats ────────────────────────────────────────────────────────
	var nActive, nWaiting, nIdle, nFinished int
	var totalIns, totalDel int
	for _, s := range m.sessions {
		stat := git.Stat(s.AgentPath)
		totalIns += stat.Insertions
		totalDel += stat.Deletions
		switch s.AgentState {
		case "active":
			nActive++
		case "waiting":
			nWaiting++
		case "finished":
			nFinished++
		default:
			nIdle++
		}
	}

	var stateParts []string
	if nActive > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d active", nActive))
	}
	if nWaiting > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d waiting", nWaiting))
	}
	if nFinished > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d done", nFinished))
	}
	if nIdle > 0 || len(stateParts) == 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d idle", nIdle))
	}
	stateLine := strings.Join(stateParts, "  ")

	styleStatLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)
	styleStatDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	// padTo pads a rendered (ANSI) string to exactly w visible characters.
	padTo := func(s string, w int) string {
		vis := lipgloss.Width(s)
		if vis >= w {
			return s
		}
		return s + strings.Repeat(" ", w-vis)
	}

	wordmark := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true).Render("PRISM")
	const wordmarkW = 5

	// ── compact mode: terminal too short for full art block ──────────────────
	// Full header needs: artHeight + separator(1) + col-header+blank(2) + sessions + bottom-blank(1)
	fullHeaderNeeded := artHeight + 1 + 2 + len(m.sessions) + 1
	if m.height > 0 && m.height < fullHeaderNeeded {
		// 2 lines: "N sessions  STATE_SUMMARY" left + PRISM right on line 1,
		// blank line 2 for breathing room.
		sessionCount := styleStatLabel.Render(fmt.Sprintf("%d sessions", len(m.sessions)))
		stateStr := styleStatDim.Render(stateLine)
		leftContent := sessionCount + "  " + stateStr
		leftW := lipgloss.Width(leftContent)
		pad := m.width - leftW - wordmarkW
		if pad < 1 {
			pad = 1
		}
		var sb strings.Builder
		sb.WriteString(leftContent)
		sb.WriteString(strings.Repeat(" ", pad))
		sb.WriteString(wordmark)
		sb.WriteString("\n\n")
		return sb.String()
	}

	// ── full mode ────────────────────────────────────────────────────────────
	var prLine string
	if !m.ghLoaded {
		prLine = "↑ …"
	} else {
		prLine = fmt.Sprintf("↑ %d open PRs", m.ghOpenPRs)
	}

	var changesLine string
	if totalIns == 0 && totalDel == 0 {
		changesLine = styleStatDim.Render("no changes")
	} else {
		changesLine = styleIns.Render(fmt.Sprintf("+%d", totalIns)) +
			"  " +
			styleDel.Render(fmt.Sprintf("-%d", totalDel))
	}

	prRendered := styleStatDim.Render(prLine)
	stateRendered := styleStatDim.Render(stateLine)
	sessionCountLine := styleStatLabel.Render(fmt.Sprintf("%d sessions", len(m.sessions)))

	// Fixed column width — wide enough for worst-case state line (35 chars) + room.
	const statsW = 37

	// 7 stat lines matching artHeight, each exactly statsW wide.
	// Lines 0-1: blank  2: sessions  3: states  4: changes  5: PRs  6: blank
	statLines := []string{
		strings.Repeat(" ", statsW),
		strings.Repeat(" ", statsW),
		padTo(sessionCountLine, statsW),
		padTo(stateRendered, statsW),
		padTo(changesLine, statsW),
		padTo(prRendered, statsW),
		strings.Repeat(" ", statsW),
	}

	// Only render art if there is enough horizontal room.
	showArt := m.width >= statsW+artWidth

	var sb strings.Builder
	if showArt {
		middle := strings.Repeat(" ", m.width-statsW-artWidth)
		for i, artLine := range artLines {
			stat := strings.Repeat(" ", statsW)
			if i < len(statLines) {
				stat = statLines[i]
			}
			sb.WriteString(stat)
			sb.WriteString(middle)
			sb.WriteString(rainbowLine(artLine))
			sb.WriteString("\n")
		}
	} else {
		// Narrow: stats lines with PRISM wordmark right-aligned on first line.
		for i, s := range statLines {
			if i == 0 {
				pad := m.width - statsW - wordmarkW
				if pad < 0 {
					trimmed := s
					if len(trimmed) > m.width-wordmarkW {
						trimmed = trimmed[:m.width-wordmarkW]
					}
					sb.WriteString(trimmed)
					sb.WriteString(wordmark)
				} else {
					sb.WriteString(s)
					sb.WriteString(strings.Repeat(" ", pad))
					sb.WriteString(wordmark)
				}
			} else {
				sb.WriteString(s)
			}
			sb.WriteString("\n")
		}
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
type ghTickMsg time.Time
type cursorTimeoutMsg struct{}
type githubStatsMsg struct {
	openPRs int
	err     bool // true = fetch failed, keep showing previous value
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func ghTick() tea.Cmd {
	return tea.Tick(60*time.Second, func(t time.Time) tea.Msg {
		return ghTickMsg(t)
	})
}

func cursorTimeoutCmd() tea.Cmd {
	return tea.Tick(cursorTimeout, func(time.Time) tea.Msg {
		return cursorTimeoutMsg{}
	})
}

func fetchSessions() tea.Msg {
	sessions, err := tmux.Sessions()
	if err != nil {
		return sessionsMsg(nil)
	}
	return sessionsMsg(sessions)
}

// fetchGitHubStats calls gh api via GraphQL to get the viewer's open PR count.
// Runs as a tea.Cmd so it never blocks the render loop.
func fetchGitHubStats() tea.Msg {
	const query = `{ viewer { pullRequests(states: OPEN, first: 1) { totalCount } } }`
	out, err := exec.Command("gh", "api", "graphql", "-f", "query="+query).Output()
	if err != nil {
		return githubStatsMsg{err: true}
	}
	// Parse: {"data":{"viewer":{"pullRequests":{"totalCount":N}}}}
	s := string(out)
	idx := strings.Index(s, `"totalCount":`)
	if idx < 0 {
		return githubStatsMsg{err: true}
	}
	s = s[idx+len(`"totalCount":`):]
	end := strings.IndexAny(s, ",}")
	if end < 0 {
		return githubStatsMsg{err: true}
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(s[:end]), "%d", &n)
	return githubStatsMsg{openPRs: n}
}

// ── model ─────────────────────────────────────────────────────────────────────

// cursorTimeout is how long the cursor bar stays visible after the last keypress
// in persistent (non-popup) dashboard mode.
const cursorTimeout = 3 * time.Second

type dashModel struct {
	sessions          []tmux.Session
	cursor            int
	cursorInitialised bool // true once we've snapped cursor to currentSession
	width             int
	height            int
	client            string
	callerClient      string // @prism_caller_client captured at init time (immutable)
	popup             bool
	currentSession    string // session the viewing client is in
	ghOpenPRs         int
	ghLoaded          bool // false = still fetching, show "…"
	cursorActive      bool // true = show selection bar; false = passive watch mode
}

func newDashModel(client string, popup bool) dashModel {
	// CallerSession reads @prism_caller stamped by the tmux binding — the only
	// reliable way to know which session the viewer came from.
	currentSession := tmux.CallerSession()
	// Capture CallerClient once at init time. The global stamp @prism_caller_client
	// is overwritten each time any client opens the dashboard, so reading it
	// inside a handler would return whatever client opened the dashboard most
	// recently — not necessarily the one interacting with this model instance.
	callerClient := tmux.CallerClient()
	// inDashSession: we are the persistent prism-dashboard session itself.
	// In this mode the flag `popup` passed via --popup is true (the restart
	// loop calls `prism dashboard --popup`), so isPopup captures C-w popups
	// only when caller session is NOT the dashboard.
	inDashSession := currentSession == dashSession || currentSession == ""
	isPopup := popup && !inDashSession
	return dashModel{
		client:         client,
		callerClient:   callerClient,
		popup:          isPopup,
		currentSession: currentSession,
		// Popup (C-w) always shows cursor. Persistent session starts passive.
		cursorActive: isPopup,
	}
}

func (m dashModel) Init() tea.Cmd {
	return tea.Batch(fetchSessions, tick(), fetchGitHubStats, ghTick())
}

func (m dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		return m, tea.Batch(fetchSessions, tick())

	case ghTickMsg:
		return m, tea.Batch(fetchGitHubStats, ghTick())

	case githubStatsMsg:
		if !msg.err {
			m.ghOpenPRs = msg.openPRs
		}
		m.ghLoaded = true

	case cursorTimeoutMsg:
		if !m.popup {
			m.cursorActive = false
		}

	case tea.BlurMsg:
		if !m.popup {
			m.cursorActive = false
		}

	case tea.FocusMsg:
		// Focus regained — cursor stays hidden until user presses j/k.

	case sessionsMsg:
		m.sessions = filterSessions([]tmux.Session(msg))
		// Do not re-read CallerSession() here — it may have changed if another
		// client opened the dashboard. Use the value captured at init time.
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
					if m.callerClient != "" {
						_ = tmux.SwitchClient(m.callerClient, m.currentSession)
					}
					return nil
				},
				tea.Quit,
			)

		case "j", "down":
			if !m.cursorActive {
				// First keypress just activates the cursor without moving.
				m.cursorActive = true
				return m, cursorTimeoutCmd()
			}
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
			}
			return m, cursorTimeoutCmd()

		case "k", "up":
			if !m.cursorActive {
				m.cursorActive = true
				return m, cursorTimeoutCmd()
			}
			if m.cursor > 0 {
				m.cursor--
			}
			return m, cursorTimeoutCmd()

		case "enter":
			if !m.cursorActive {
				// In persistent-session mode the cursor starts inactive (passive
				// watch mode). Mirror the j/k behaviour: first Enter activates the
				// cursor without immediately switching, so the user can confirm the
				// highlighted session before committing.
				if !m.popup {
					m.cursorActive = true
					return m, cursorTimeoutCmd()
				}
			}
			if len(m.sessions) == 0 {
				return m, nil
			}
			selected := m.sessions[m.cursor]
			return m, tea.Sequence(
				func() tea.Msg {
					_ = tmux.SelectAgentWindow(selected.Name)
					// dashSwitchTarget selects the right client for this model —
					// see its doc comment for the full explanation.
					if target := dashSwitchTarget(m.popup, m.client, m.callerClient); target != "" {
						_ = tmux.SwitchClient(target, selected.Name)
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

// dashSwitchTarget returns the tmux client name that should receive a
// switch-client command when the user selects a session in the dashboard.
//
// Rules:
//   - Popup (C-w) mode: the process runs inside the popup which is attached to
//     the viewer's own client. client (= CurrentClient() at startup) is the
//     correct target.
//   - Persistent-session mode: the dashboard runs in a background session; the
//     viewer's client is identified by the @prism_caller_client stamp that was
//     captured at model-init time as callerClient. Use that value if non-empty,
//     otherwise fall back to client.
//
// Either way, callers must NOT pass tmux.CallerClient() live — that reads a
// server-wide global that gets overwritten whenever any client opens the
// dashboard, causing the wrong client to be switched (the original bug).
func dashSwitchTarget(popup bool, client, callerClient string) string {
	if !popup && callerClient != "" {
		return callerClient
	}
	return client
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

	const stateW = 10
	const dotW = 2
	const sessionWMax = 28
	const sessionWMin = 14
	const statWFull = 22    // "2 files +122 -14"
	const statWCompact = 10 // "+122 -14"
	// Try full layout first; compress stat then session if too narrow.
	fixedOther := 1 + dotW + 2 + stateW + 2 + 2 // dot+session gap+state gap+stat gap
	titleW := m.width - (fixedOther + sessionWMax + statWFull)
	statW := statWFull
	sessionW := sessionWMax
	if titleW < 0 {
		// Try compact stat column first.
		titleW = m.width - (fixedOther + sessionWMax + statWCompact)
		statW = statWCompact
	}
	if titleW < 0 {
		// Still too narrow: shrink session column too.
		sessionW = max(sessionWMin, sessionWMax+titleW)
		titleW = 0
	} else if titleW < 10 {
		titleW = 0
	}

	var sb strings.Builder

	// ── header: stats left, art right ───────────────────────────────────────
	sb.WriteString(renderHeader(m, styleDim, styleIns, styleDel))

	// Rainbow separator between header and column headers.
	sb.WriteString(rainbowLineWidth(strings.Repeat("─", m.width), m.width))
	sb.WriteString("\n")

	changesHeader := "changes"
	if statW == statWCompact {
		changesHeader = "+/-"
	}
	header := fmt.Sprintf(" %-*s%-*s  %-*s  %-*s", dotW, "", sessionW, "session", stateW, "state", statW, changesHeader)
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
		} else if statW == statWCompact {
			statPlain = fmt.Sprintf("+%d -%d", stat.Insertions, stat.Deletions)
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

		if isSelected && m.cursorActive {
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
			} else if statW == statWCompact {
				// Compact: just +N -N coloured, no file count.
				coloured := styleIns.Render(fmt.Sprintf("+%d", stat.Insertions)) +
					" " + styleDel.Render(fmt.Sprintf("-%d", stat.Deletions))
				rawLen := len(fmt.Sprintf("+%d -%d", stat.Insertions, stat.Deletions))
				pad := statW - rawLen
				if pad < 0 {
					pad = 0
				}
				statStr = coloured + strings.Repeat(" ", pad)
			} else {
				fileWord := "files"
				if stat.Files == 1 {
					fileWord = "file "
				}
				// Build plain portion then pad, append coloured +/- separately.
				plain := fmt.Sprintf("%d %s ", stat.Files, fileWord)
				coloured := styleIns.Render(fmt.Sprintf("+%d", stat.Insertions)) +
					" " + styleDel.Render(fmt.Sprintf("-%d", stat.Deletions))
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
			p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithReportFocus())
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
