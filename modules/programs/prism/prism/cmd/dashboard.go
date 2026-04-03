package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	colorful "github.com/lucasb-eyer/go-colorful"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
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
func renderHeader(d dashShared, styleDim, styleIns, styleDel lipgloss.Style) string {
	// ── compute stats ────────────────────────────────────────────────────────
	var nActive, nWaiting, nIdle, nFinished, nInterrupted int
	var totalIns, totalDel int
	for _, s := range d.sessions {
		result := d.gitStats[s.AgentPath]
		if result.Ok {
			totalIns += result.Stat.Insertions
			totalDel += result.Stat.Deletions
		}
		switch agent.AgentState(s.AgentState) {
		case agent.StateActive:
			nActive++
		case agent.StateWaiting:
			nWaiting++
		case agent.StateFinished:
			nFinished++
		case agent.StateInterrupted:
			nInterrupted++
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
	if nInterrupted > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d interrupted", nInterrupted))
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
	fullHeaderNeeded := artHeight + 1 + 2 + len(d.sessions) + 1
	if d.height > 0 && d.height < fullHeaderNeeded {
		// 2 lines: "N sessions  STATE_SUMMARY" left + PRISM right on line 1,
		// blank line 2 for breathing room.
		sessionCount := styleStatLabel.Render(fmt.Sprintf("%d sessions", len(d.sessions)))
		stateStr := styleStatDim.Render(stateLine)
		leftContent := sessionCount + "  " + stateStr
		leftW := lipgloss.Width(leftContent)
		pad := d.width - leftW - wordmarkW
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
	if !d.ghLoaded {
		prLine = "↑ …"
	} else {
		prLine = fmt.Sprintf("↑ %d open PRs", d.ghOpenPRs)
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
	sessionCountLine := styleStatLabel.Render(fmt.Sprintf("%d sessions", len(d.sessions)))

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
	showArt := d.width >= statsW+artWidth

	var sb strings.Builder
	if showArt {
		middle := strings.Repeat(" ", d.width-statsW-artWidth)
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
				pad := d.width - statsW - wordmarkW
				if pad < 0 {
					trimmed := s
					if len(trimmed) > d.width-wordmarkW {
						trimmed = trimmed[:d.width-wordmarkW]
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

// ── session model ─────────────────────────────────────────────────────────────

// agentSession is the dashboard's view of a session. It is derived from
// db.Status (the authoritative source) with client attachment count added from
// a tmux query.
type agentSession struct {
	Name        string
	AgentState  string // active | waiting | finished | compacting | error | idle | ""
	AgentPath   string // worktree path — used for git diff stats
	AgentTitle  string // current session title from agent_status.title
	AgentName   string // coordinator | worker | "" — from agent_status.agent_name
	ModelID     string // model identifier from agent_status.model_id
	ClientCount int    // tmux clients currently attached (best-effort, 0 on error)
}

// statusToAgentSession converts a db.Status into an agentSession.
// clientCounts is a map from session name → client count (from tmux).
func statusToAgentSession(s db.Status, clientCounts map[string]int) agentSession {
	title := ""
	if s.Title != nil {
		title = *s.Title
	}
	agentName := ""
	if s.AgentName != nil {
		agentName = *s.AgentName
	}
	modelID := ""
	if s.ModelID != nil {
		modelID = *s.ModelID
	}
	return agentSession{
		Name:        s.SessionName,
		AgentState:  s.State,
		AgentPath:   s.Worktree,
		AgentTitle:  title,
		AgentName:   agentName,
		ModelID:     modelID,
		ClientCount: clientCounts[s.SessionName],
	}
}

// sessionRepo extracts the repo prefix from a session name.
// Session names are of the form "repo@branch"; for names without "@" the whole
// name is used as the repo key (handles scratchpad, prism-dashboard, etc.).
func sessionRepo(name string) string {
	if idx := strings.Index(name, "@"); idx >= 0 {
		return name[:idx]
	}
	return name
}

// sessionBranch extracts the branch suffix from a session name (the part after
// the first "@"). Returns the full name when there is no "@".
func sessionBranch(name string) string {
	if idx := strings.Index(name, "@"); idx >= 0 {
		return name[idx:] // keeps the "@" prefix, e.g. "@main"
	}
	return name
}

// sortDisplayed sorts a session slice in-place to match the flat visual render
// order: alphabetical by repo name, @main first within each repo, then other
// branches alphabetically. Uses insertion sort (no stdlib import needed for
// small N).
func sortDisplayed(ss []agentSession) {
	// sessionKey returns a sort key for a session: "repo\x00" for @main and
	// sessions without @, so they sort before any branch, or "repo\x01branch"
	// for other worktree branches.
	sessionKey := func(s agentSession) string {
		repo := sessionRepo(s.Name)
		branch := sessionBranch(s.Name)
		if branch == s.Name || branch == "@main" {
			// No "@" (plain session) or @main — sorts first within repo.
			return repo + "\x00" + s.Name
		}
		return repo + "\x01" + branch
	}
	for i := 1; i < len(ss); i++ {
		key := ss[i]
		keyStr := sessionKey(key)
		j := i - 1
		for j >= 0 && sessionKey(ss[j]) > keyStr {
			ss[j+1] = ss[j]
			j--
		}
		ss[j+1] = key
	}
}

// agentTypeLabel returns a display label for the agent_name value.
func agentTypeLabel(agentName string) string {
	switch agentName {
	case "coordinator":
		return "coordinator"
	case "worker":
		return "worker"
	default:
		return ""
	}
}

// ── messages ──────────────────────────────────────────────────────────────────

// RefreshMsg is sent by the sentinel watcher goroutine to trigger a DB re-fetch.
type RefreshMsg struct{}

// dashStatusMsg carries a transient status/error message to display in the dashboard.
type dashStatusMsg string

// gitStatResult holds the outcome of a git.Stat call for a single worktree.
// Ok is false when the git command failed; in that case Stat is zero and the
// caller should render "?" rather than "—".
type gitStatResult struct {
	Stat git.DiffStat
	Ok   bool
}

type sessionsMsg struct {
	sessions []agentSession
	gitStats map[string]gitStatResult
}
type ghTickMsg time.Time
type cursorTimeoutMsg struct{}
type githubStatsMsg struct {
	openPRs int
	err     bool // true = fetch failed, keep showing previous value
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

// fetchSessionsFromDB queries agent_status for all active sessions and
// enriches them with git diff stats.
func fetchSessionsFromDB() tea.Msg {
	d, err := openDB()
	if err != nil {
		// DB unavailable — return empty list rather than crashing.
		return sessionsMsg{}
	}
	defer d.Close()

	statuses, err := d.AllActiveStatus()
	if err != nil {
		return sessionsMsg{}
	}

	// Get client counts from tmux for the attachment dot indicator.
	clientCounts := tmuxClientCounts()

	sessions := make([]agentSession, 0, len(statuses))
	for _, s := range statuses {
		sessions = append(sessions, statusToAgentSession(s, clientCounts))
	}

	// Filter out internal sessions (scratchpad, prism-dashboard).
	sessions = filterAgentSessions(sessions)

	// Collect unique agent paths that need git stat computation.
	seen := map[string]bool{}
	var paths []string
	for _, s := range sessions {
		if s.AgentPath != "" && !seen[s.AgentPath] {
			seen[s.AgentPath] = true
			paths = append(paths, s.AgentPath)
		}
	}

	// Run git.Stat for each unique path concurrently.
	stats := make(map[string]gitStatResult, len(paths))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			diffStat, err := git.Stat(path)
			result := gitStatResult{Stat: diffStat, Ok: err == nil}
			mu.Lock()
			stats[path] = result
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	return sessionsMsg{sessions: sessions, gitStats: stats}
}

// tmuxClientCounts returns a map of session name → number of attached clients.
// Returns an empty map on error (attachment count is best-effort).
func tmuxClientCounts() map[string]int {
	counts := map[string]int{}
	out, err := tmux.Run("list-clients", "-F", "#{session_name}")
	if err != nil {
		return counts
	}
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name != "" {
			counts[name]++
		}
	}
	return counts
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

// ── shared data model ─────────────────────────────────────────────────────────

// cursorTimeout is how long the cursor bar stays visible after the last keypress
// in persistent (non-popup) dashboard mode.
const cursorTimeout = 3 * time.Second

// dashShared is the data shared between popup and persistent dashboard modes.
// It contains only data-layer state: sessions, filter, cursor position, display
// geometry, and GitHub stats. It deliberately has no mode-specific fields
// (no popup bool, no callerClient, no currentSession, no inDashSession).
type dashShared struct {
	sessions          []agentSession
	gitStats          map[string]gitStatResult // keyed by AgentPath; populated on sessionsMsg
	cursor            int
	cursorInitialised bool // true once we've snapped cursor to currentSession
	width             int
	height            int
	ghOpenPRs         int
	ghLoaded          bool // false = still fetching, show "…"
	loading           bool // true = first fetch not yet returned; show skeleton
	// filter mode: activated by '/', cancelled by esc/ctrl+c
	filterActive bool
	filterText   string
	// displayed is the filtered (or full) sessions list used by View/cursor.
	displayed []agentSession
	// statusMsg is a transient error/info line shown at the bottom of the view.
	statusMsg string
}

// applySessionsMsg updates shared state when a sessionsMsg arrives.
// snapSession is the session name to snap the cursor to on first load
// (pass the currentSession value from the mode-specific model).
// Returns the updated dashShared and whether a snap was performed.
func (d dashShared) applySessionsMsg(msg sessionsMsg, snapSession string) (dashShared, bool) {
	d.loading = false
	if msg.sessions != nil {
		d.sessions = msg.sessions
	}
	if msg.gitStats != nil {
		d.gitStats = msg.gitStats
	}
	needsSnap := !d.cursorInitialised && !d.filterActive
	if !d.cursorInitialised {
		d.cursorInitialised = true
	}
	d = dashRefilterShared(d)
	if needsSnap {
		for i, s := range d.displayed {
			if s.Name == snapSession {
				d.cursor = i
				break
			}
		}
	}
	return d, needsSnap
}

// handleFilterKey handles a key press in filter mode. Returns the updated
// dashShared and the tea.Cmd to run. A special tea.Cmd value of nil indicates
// no command; the caller must also check the returned bool (exitFilter=true
// means the filter was confirmed with Enter and the caller should switch
// sessions using the current cursor position).
func (d dashShared) handleFilterKey(msg tea.KeyMsg) (dashShared, bool /* exitFilter */, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return d, false, tea.Quit

	case "esc":
		d.filterActive = false
		d.filterText = ""
		d = dashRefilterShared(d)
		return d, false, nil

	case "enter":
		if len(d.displayed) == 0 {
			return d, false, nil
		}
		d.filterActive = false
		d.filterText = ""
		return d, true, nil

	case "backspace", "ctrl+h":
		if len(d.filterText) > 0 {
			runes := []rune(d.filterText)
			d.filterText = string(runes[:len(runes)-1])
			d = dashRefilterShared(d)
		}

	case "j", "down":
		if d.cursor < len(d.displayed)-1 {
			d.cursor++
		}

	case "k", "up":
		if d.cursor > 0 {
			d.cursor--
		}

	default:
		if msg.Type == tea.KeyRunes {
			d.filterText += msg.String()
			d = dashRefilterShared(d)
		}
	}
	return d, false, nil
}

func filterAgentSessions(all []agentSession) []agentSession {
	var out []agentSession
	for _, s := range all {
		if s.Name == "scratchpad" || s.Name == "prism-dashboard" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// dashRefilterShared recomputes d.displayed from d.sessions applying the active
// fuzzy filter (if any). It also clamps the cursor so it never points out of
// bounds. It returns the updated dashShared.
func dashRefilterShared(d dashShared) dashShared {
	if !d.filterActive || d.filterText == "" {
		d.displayed = make([]agentSession, len(d.sessions))
		copy(d.displayed, d.sessions)
	} else {
		var out []agentSession
		for _, s := range d.sessions {
			if fuzzyMatch(s.Name, d.filterText) {
				out = append(out, s)
			}
		}
		d.displayed = out
	}
	// Sort displayed to match visual render order so d.cursor indexes
	// correctly: alphabetical by repo, @main first within each repo,
	// then other branches alphabetically.
	sortDisplayed(d.displayed)
	if d.cursor >= len(d.displayed) {
		d.cursor = max(0, len(d.displayed)-1)
	}
	return d
}

// ── popup dashboard model ─────────────────────────────────────────────────────

// popupModel is the Bubble Tea model for the ephemeral popup dashboard
// (spawned by C-w via `tmux display-popup -E`).
//
// Lifecycle: the popup is short-lived. Pressing q/esc causes tea.Quit, which
// terminates the process; display-popup -E then closes the popup frame.
//
// Focus management: the popup runs inside the caller's own tmux client, so
// m.client is always the right switch-client target. No global tmux option
// reads are required.
type popupModel struct {
	dashShared
	client         string // tmux client running this popup (from CurrentClient())
	currentSession string // caller's current session (from --caller-session flag)
	cursorActive   bool   // always true in popup mode
}

func newPopupModel(client, callerSession string) popupModel {
	m := popupModel{
		dashShared: dashShared{
			loading: true,
		},
		client:         client,
		currentSession: callerSession,
		cursorActive:   true, // popup always shows cursor
	}
	m.displayed = m.sessions // empty at init; populated on first sessionsMsg
	return m
}

func (m popupModel) Init() tea.Cmd {
	return tea.Batch(fetchSessionsFromDB, fetchGitHubStats, ghTick())
}

func (m popupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case RefreshMsg:
		return m, fetchSessionsFromDB

	case dashStatusMsg:
		m.statusMsg = string(msg)
		return m, nil

	case ghTickMsg:
		return m, tea.Batch(fetchGitHubStats, ghTick())

	case githubStatsMsg:
		if !msg.err {
			m.ghOpenPRs = msg.openPRs
		}
		m.ghLoaded = true

	case sessionsMsg:
		m.dashShared, _ = m.dashShared.applySessionsMsg(msg, m.currentSession)

	case tea.KeyMsg:
		m.statusMsg = ""

		// In filter mode most keys are consumed by the filter input.
		if m.filterActive {
			var exitFilter bool
			var cmd tea.Cmd
			m.dashShared, exitFilter, cmd = m.dashShared.handleFilterKey(msg)
			if exitFilter {
				// Confirm selection: switch to highlighted session.
				selected := m.displayed[m.cursor]
				target := m.switchTarget()
				return m, func() tea.Msg {
					if errMsg := ensureSessionAndSwitch(selected.Name, target); errMsg != "" {
						return dashStatusMsg(errMsg)
					}
					return tea.QuitMsg{}
				}
			}
			return m, cmd
		}

		// Normal (non-filter) key handling.
		switch msg.String() {
		case "q", "esc":
			// Popup mode: just quit — display-popup -E closes the frame.
			return m, tea.Quit

		case "/":
			m.filterActive = true
			m.filterText = ""
			m.cursorActive = true
			m.dashShared = dashRefilterShared(m.dashShared)
			return m, nil

		case "j", "down":
			m.cursorActive = true
			if m.cursor < len(m.displayed)-1 {
				m.cursor++
			}
			return m, nil

		case "k", "up":
			m.cursorActive = true
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil

		case "enter":
			if len(m.displayed) == 0 {
				return m, nil
			}
			selected := m.displayed[m.cursor]
			target := m.switchTarget()
			return m, func() tea.Msg {
				if errMsg := ensureSessionAndSwitch(selected.Name, target); errMsg != "" {
					return dashStatusMsg(errMsg)
				}
				return tea.QuitMsg{}
			}
		}
	}
	return m, nil
}

// switchTarget returns the tmux client that should receive switch-client when
// the user selects a session. In popup mode the popup process runs inside the
// caller's own client, so client is the right target.
func (m popupModel) switchTarget() string {
	return m.client
}

func (m popupModel) View() string {
	return dashView(m.dashShared, m.currentSession, m.cursorActive)
}

// ── persistent dashboard model ────────────────────────────────────────────────

// persistentModel is the Bubble Tea model for the long-running persistent
// dashboard session (`prism-dashboard`).
//
// Lifecycle: the process runs indefinitely. Pressing q/esc switches the current
// client back to its previous session (switch-client -l) and returns to passive
// watch mode. The session is kept alive by the tmux session itself, not by a
// restart loop.
//
// Focus management: m.client is the only client identity needed — it is the
// client currently viewing the persistent dashboard. q/esc and Enter both
// operate on m.client (no caller state required).
type persistentModel struct {
	dashShared
	client         string // tmux client of the process (from CurrentClient())
	currentSession string // caller's session (from --caller-session flag; for "you are here")
	cursorActive   bool   // false = passive watch; true = selection mode
}

func newPersistentModel(client, callerSession string) persistentModel {
	m := persistentModel{
		dashShared: dashShared{
			loading: true,
		},
		client:         client,
		currentSession: callerSession,
		cursorActive:   false, // persistent starts in passive watch mode
	}
	m.displayed = m.sessions // empty at init; populated on first sessionsMsg
	return m
}

func (m persistentModel) Init() tea.Cmd {
	return tea.Batch(fetchSessionsFromDB, fetchGitHubStats, ghTick())
}

func (m persistentModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case RefreshMsg:
		return m, fetchSessionsFromDB

	case dashStatusMsg:
		m.statusMsg = string(msg)
		return m, nil

	case ghTickMsg:
		return m, tea.Batch(fetchGitHubStats, ghTick())

	case githubStatsMsg:
		if !msg.err {
			m.ghOpenPRs = msg.openPRs
		}
		m.ghLoaded = true

	case cursorTimeoutMsg:
		// Do not deactivate the cursor while the filter is open — the selection
		// bar must stay visible for the entire filter session.
		if !m.filterActive {
			m.cursorActive = false
		}

	case tea.BlurMsg:
		// Do not deactivate the cursor while the filter is open — the user
		// must be able to see which session Enter will select at all times.
		if !m.filterActive {
			m.cursorActive = false
		}

	case tea.FocusMsg:
		// Focus regained — cursor stays hidden until user presses j/k.

	case sessionsMsg:
		m.dashShared, _ = m.dashShared.applySessionsMsg(msg, m.currentSession)

	case tea.KeyMsg:
		m.statusMsg = ""

		// In filter mode most keys are consumed by the filter input.
		if m.filterActive {
			var exitFilter bool
			var cmd tea.Cmd
			m.dashShared, exitFilter, cmd = m.dashShared.handleFilterKey(msg)
			if exitFilter {
				// Confirm selection: switch the viewing client to the selected
				// session and return to passive watch mode. The process does NOT
				// quit — the dashboard stays alive for the next visitor.
				selected := m.displayed[m.cursor]
				client := m.client
				m.cursorActive = false
				return m, func() tea.Msg {
					if errMsg := ensureSessionAndSwitch(selected.Name, client); errMsg != "" {
						return dashStatusMsg(errMsg)
					}
					return nil
				}
			}
			return m, cmd
		}

		// Normal (non-filter) key handling.
		switch msg.String() {
		case "q", "esc":
			// Persistent mode: switch the current client back to its last session
			// (switch-client -l), then deactivate the cursor (return to passive
			// watch mode). Using -l avoids any stale caller state — it always
			// returns to wherever the client was before it switched here.
			// The process does NOT quit — the session stays alive.
			client := m.client
			m.cursorActive = false
			return m, func() tea.Msg {
				if client != "" {
					_ = tmux.SwitchClientLast(client)
				}
				return nil
			}

		case "/":
			// Activate inline fuzzy filter.
			m.filterActive = true
			m.filterText = ""
			m.cursorActive = true
			m.dashShared = dashRefilterShared(m.dashShared)
			return m, nil

		case "j", "down":
			if !m.cursorActive {
				// First keypress just activates the cursor without moving.
				m.cursorActive = true
				return m, cursorTimeoutCmd()
			}
			if m.cursor < len(m.displayed)-1 {
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
				m.cursorActive = true
				return m, cursorTimeoutCmd()
			}
			if len(m.displayed) == 0 {
				return m, nil
			}
			selected := m.displayed[m.cursor]
			client := m.client
			m.cursorActive = false
			return m, func() tea.Msg {
				// Switch the viewing client to the selected session, then return
				// to passive watch mode. The TUI stays alive.
				if errMsg := ensureSessionAndSwitch(selected.Name, client); errMsg != "" {
					return dashStatusMsg(errMsg)
				}
				return nil
			}
		}
	}
	return m, nil
}

// switchTarget returns the tmux client that should receive switch-client when
// the user selects a session in persistent mode. Always m.client — the client
// currently viewing the dashboard.
func (m persistentModel) switchTarget() string {
	return m.client
}

func (m persistentModel) View() string {
	return dashView(m.dashShared, m.currentSession, m.cursorActive)
}

// ── helpers shared between models ────────────────────────────────────────────

// dashRefilter recomputes m.displayed from m.sessions applying the active
// fuzzy filter (if any). It also clamps the cursor so it never points out of
// bounds. It returns the updated model.
//
// Deprecated: use dashRefilterShared directly. This wrapper exists for
// compatibility with existing tests.
func dashRefilter(m dashModel) dashModel {
	s := dashShared{
		sessions:     m.sessions,
		filterActive: m.filterActive,
		filterText:   m.filterText,
		cursor:       m.cursor,
		displayed:    m.displayed,
	}
	s = dashRefilterShared(s)
	m.sessions = s.sessions
	m.filterActive = s.filterActive
	m.filterText = s.filterText
	m.cursor = s.cursor
	m.displayed = s.displayed
	return m
}

// ── view ──────────────────────────────────────────────────────────────────────

// dashView is the shared rendering function for both popup and persistent modes.
// currentSession is used to show the "you are here" ◆ indicator.
// cursorActive controls whether the selection bar is shown.
func dashView(d dashShared, currentSession string, cursorActive bool) string {
	if d.width == 0 {
		// Before WindowSizeMsg: render a minimal skeleton so the first frame
		// is never blank. Use a fixed width so the output is deterministic.
		return skeletonView(80)
	}

	if d.loading {
		return skeletonView(d.width)
	}

	// fixedCore (defined below) is the irreducible column overhead. At widths
	// below fixedCore+1, the session header word "session" (7 chars) overflows
	// its 6-char slot when sessionW=0, so render a skeleton instead.
	const minUsableWidth = 22 // fixedCore+1
	if d.width < minUsableWidth {
		return skeletonView(d.width)
	}

	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleHeader := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)
	styleIns := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorGreen))
	styleDel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorRed))
	styleFg := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorForeground))
	stylePrompt := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)
	styleAgentType := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	// ── column widths ────────────────────────────────────────────────────────
	// Tree prefix for worktree child rows: "  ├── " or "  └── " (6 chars).
	// Top-level rows use no prefix; their name is padded to treePrefixW+sessionW.
	const treePrefixW = 6
	const agentTypeW = 12 // "coordinator " or "worker      " or "            "
	const stateW = 10
	const dotW = 2
	const sessionWStart = 20 // starting width; grows as columns are dropped
	const sessionWCap = 40   // maximum session width before the rest goes to title
	const statWFull = 22     // "2 files +122 -14"
	const statWCompact = 10  // "+122 -14"
	const modelWFull = 22    // e.g. "claude-sonnet-4-6    "

	// fixedCore is the non-negotiable fixed overhead: leading space + dot +
	// treePrefixW + gap-before-state + stateW.
	// Full row layout:
	//   1 + dotW + treePrefixW + sessionW
	//   + [2+agentTypeW if showType]
	//   + [2+modelWFull if showModel]
	//   + 2 + stateW
	//   + [2+statW if showStat]
	//   + [2+titleW if titleW>0]
	const fixedCore = 1 + dotW + treePrefixW + 2 + stateW

	showType := true
	showModel := true
	showStat := true
	statW := statWFull
	sessionW := sessionWStart

	// usedWidth returns the width of all non-title columns at their current settings.
	usedWidth := func() int {
		w := fixedCore + sessionW
		if showType {
			w += agentTypeW + 2
		}
		if showModel {
			w += modelWFull + 2
		}
		if showStat {
			w += statW + 2
		}
		return w
	}

	// calcTitleW returns the space available for the title column content
	// (excluding the 2-char gap before it).
	// Positive: title fits with this many chars.
	// Zero: exact fit, no title.
	// Negative: layout overflows; a column must be dropped.
	calcTitleW := func() int {
		// Total slack = d.width - usedWidth().
		// We need at least 3 chars of slack to show any title:
		//   2 (gap) + 1 (one character of title content).
		// A slack of exactly 2 means exact fit, no title (titleW = 0).
		// A slack < 2 means overflow.
		return d.width - usedWidth() - 2
	}

	// growSession offers surplus space to sessionW (up to sessionWCap) before
	// allocating the rest to titleW.  It keeps tw >= 2 so that at least a 2-char
	// title content slot remains after session growth; below that threshold the
	// title section is suppressed by the titleW>=5 guard anyway.  It modifies
	// sessionW in place and returns the updated titleW.
	growSession := func(tw int) int {
		if tw > 2 && sessionW < sessionWCap {
			gain := min(tw-2, sessionWCap-sessionW)
			sessionW += gain
			tw -= gain
		}
		return tw
	}

	// Start with the widest layout and shed columns in order until the layout fits.
	// At each step: drop column → grow session from reclaimed space → titleW is
	// whatever is left (may be 0 = no title shown but row fills terminal).
	titleW := growSession(calcTitleW())

	if titleW < 0 {
		// Compact stat column first.
		statW = statWCompact
		titleW = growSession(calcTitleW())
	}
	if titleW < 0 {
		// Drop model.
		showModel = false
		titleW = growSession(calcTitleW())
	}
	if titleW < 0 {
		// Drop stat entirely.
		showStat = false
		titleW = growSession(calcTitleW())
	}
	if titleW < 0 {
		// Drop type.  After this only session + state remain; grow session to
		// fill all available space, clamped to 0 on extremely narrow terminals.
		showType = false
		avail := d.width - fixedCore
		if avail > 0 {
			sessionW = avail
		} else {
			sessionW = 0
		}
		titleW = 0
	}

	var sb strings.Builder

	// ── header: stats left, art right ───────────────────────────────────────
	sb.WriteString(renderHeader(d, styleDim, styleIns, styleDel))

	// Rainbow separator between header and column headers.
	sb.WriteString(rainbowLineWidth(strings.Repeat("─", d.width), d.width))
	sb.WriteString("\n")

	changesHeader := "changes"
	if statW == statWCompact {
		changesHeader = "+/-"
	}
	// Column header: top-level rows have no tree prefix gap, so session column
	// spans treePrefixW+sessionW total (same total width as all data rows).
	header := fmt.Sprintf(" %-*s%-*s",
		dotW, "",
		treePrefixW+sessionW, "session",
	)
	if showType {
		header += fmt.Sprintf("  %-*s", agentTypeW, "type")
	}
	if showModel {
		header += fmt.Sprintf("  %-*s", modelWFull, "model")
	}
	header += fmt.Sprintf("  %-*s", stateW, "state")
	if showStat {
		header += fmt.Sprintf("  %-*s", statW, changesHeader)
	}
	if titleW >= 5 {
		header += fmt.Sprintf("  %-*s", titleW, "title")
	}
	sb.WriteString(styleHeader.Render(header))
	sb.WriteString("\n\n")

	sessions := d.displayed
	if len(sessions) == 0 {
		if d.filterActive {
			sb.WriteString(styleDim.Render("  no matches"))
		} else {
			sb.WriteString(styleDim.Render("  no active sessions"))
		}
		sb.WriteString("\n")
	} else if d.filterActive {
		// Flat list while filter is active (no grouping — easier to scan).
		for i, s := range sessions {
			sb.WriteString(renderSessionRow(d, s, i, "" /*treePrefix*/, currentSession, cursorActive, styleDim, styleIns, styleDel, styleFg, styleAgentType, sessionW, agentTypeW, stateW, statW, statWCompact, titleW, modelWFull, showType, showModel, showStat))
		}
	} else {
		// Flat view with inline child detection via look-ahead.
		// A session is a "child" if the sorted run for its repo has a top-level
		// row (@main or plain no-@ session) and this session is not that row.
		// This correctly handles: single repo with no @main (all top-level),
		// @main + N branches (all N are children), and multi-repo lists.
		isTopLevel := func(name string) bool {
			branch := sessionBranch(name)
			return branch == name || branch == "@main"
		}
		// groupHasTopLevel returns true if the contiguous run of same-repo
		// sessions that includes sessions[i] contains at least one top-level row.
		groupHasTopLevel := func(i int) bool {
			thisRepo := sessionRepo(sessions[i].Name)
			// Walk back to the start of this repo's run.
			start := i
			for start > 0 && sessionRepo(sessions[start-1].Name) == thisRepo {
				start--
			}
			for k := start; k < len(sessions) && sessionRepo(sessions[k].Name) == thisRepo; k++ {
				if isTopLevel(sessions[k].Name) {
					return true
				}
			}
			return false
		}
		for i, s := range sessions {
			isChild := !isTopLevel(s.Name) && groupHasTopLevel(i)
			var treePrefix string
			if isChild {
				// Look ahead to determine if this is the last child in the group.
				isLastChild := true
				thisRepo := sessionRepo(s.Name)
				for j := i + 1; j < len(sessions); j++ {
					if sessionRepo(sessions[j].Name) != thisRepo {
						break
					}
					if !isTopLevel(sessions[j].Name) {
						isLastChild = false
						break
					}
				}
				if isLastChild {
					treePrefix = "  └── "
				} else {
					treePrefix = "  ├── "
				}
			}
			sb.WriteString(renderSessionRow(d, s, i, treePrefix, currentSession, cursorActive, styleDim, styleIns, styleDel, styleFg, styleAgentType, sessionW, agentTypeW, stateW, statW, statWCompact, titleW, modelWFull, showType, showModel, showStat))
		}
	}

	// Filter prompt or help hint at the bottom.
	if d.filterActive {
		sb.WriteString("\n")
		sb.WriteString(stylePrompt.Render(" / "))
		sb.WriteString(d.filterText)
		sb.WriteString(styleDim.Render("█"))
		sb.WriteString("\n")
	} else if d.statusMsg != "" {
		sb.WriteString("\n")
		styleErr := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorRed))
		sb.WriteString(styleErr.Render("  " + d.statusMsg))
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n")
		sb.WriteString(styleDim.Render("  / filter  ↑↓/jk navigate  enter select  q quit"))
		sb.WriteString("\n")
	}

	return sb.String()
}

// renderSessionRow renders a single session row (selected or unselected).
// treePrefix is the ASCII tree connector string (e.g. "  ├── ") for child rows;
// pass "" for top-level rows. Top-level rows display the full session name
// padded to treePrefixW+sessionW; child rows display the tree prefix plus the
// branch name padded to the same total width.
func renderSessionRow(
	d dashShared,
	s agentSession,
	cursorIdx int,
	treePrefix string,
	currentSession string,
	cursorActive bool,
	styleDim, styleIns, styleDel, styleFg, styleAgentType lipgloss.Style,
	sessionW, agentTypeW, stateW, statW, statWCompact, titleW, modelW int,
	showType, showModel, showStat bool,
) string {
	isHere := s.Name == currentSession
	isSelected := cursorIdx == d.cursor

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

	// treePrefixW is always 6 runes; pad treePrefix to that width using rune
	// count (not byte count) since tree connector chars are multi-byte in UTF-8.
	const treePrefixW = 6

	// Build the session display area (treePrefixW+sessionW total width):
	// - Top-level (treePrefix=""): full session name padded to treePrefixW+sessionW.
	// - Child (treePrefix non-empty): tree prefix (6 runes) + branch name padded to sessionW.
	var sessionArea string
	totalSessionW := treePrefixW + sessionW
	if treePrefix == "" {
		// Top-level row: full session name padded to totalSessionW.
		name := s.Name
		if utf8.RuneCountInString(name) > totalSessionW {
			name = string([]rune(name)[:totalSessionW-1]) + "…"
		}
		sessionArea = fmt.Sprintf("%-*s", totalSessionW, name)
	} else {
		// Child row: pad treePrefix to treePrefixW then branch name padded to sessionW.
		paddedPrefix := treePrefix
		runeCount := utf8.RuneCountInString(paddedPrefix)
		if runeCount < treePrefixW {
			paddedPrefix += strings.Repeat(" ", treePrefixW-runeCount)
		}
		branch := sessionBranch(s.Name)
		if sessionW == 0 {
			branch = ""
		} else if utf8.RuneCountInString(branch) > sessionW {
			branch = string([]rune(branch)[:sessionW-1]) + "…"
		}
		sessionArea = paddedPrefix + fmt.Sprintf("%-*s", sessionW, branch)
	}

	agentLabel := agentTypeLabel(s.AgentName)
	paddedAgentLabel := fmt.Sprintf("%-*s", agentTypeW, agentLabel)

	modelLabel := s.ModelID
	if utf8.RuneCountInString(modelLabel) > modelW {
		modelLabel = string([]rune(modelLabel)[:modelW-1]) + "…"
	}
	paddedModelLabel := fmt.Sprintf("%-*s", modelW, modelLabel)

	result := d.gitStats[s.AgentPath]
	var statPlain string
	if s.AgentPath == "" || result.Ok && result.Stat.Files == 0 {
		statPlain = "—"
	} else if !result.Ok {
		statPlain = "?"
	} else if statW == statWCompact {
		statPlain = fmt.Sprintf("+%d -%d", result.Stat.Insertions, result.Stat.Deletions)
	} else {
		fileWord := "files"
		if result.Stat.Files == 1 {
			fileWord = "file "
		}
		statPlain = fmt.Sprintf("%d %s +%d -%d", result.Stat.Files, fileWord, result.Stat.Insertions, result.Stat.Deletions)
	}

	title := s.AgentTitle
	if titleW >= 5 && utf8.RuneCountInString(title) > titleW {
		title = string([]rune(title)[:titleW-1]) + "…"
	}

	if isSelected && cursorActive {
		// Bar colour: state colour for active states, primary for idle/finished.
		barBg := lipgloss.Color(ColorPrimary)
		switch agent.AgentState(s.AgentState) {
		case agent.StateActive, agent.StateWaiting, agent.StateCompacting, agent.StateError, agent.StateInterrupted:
			if c, ok := stateStyle(s.AgentState).GetForeground().(lipgloss.Color); ok {
				barBg = c
			}
		}

		plain := fmt.Sprintf(" %s%s", dot, sessionArea)
		if showType {
			plain += fmt.Sprintf("  %-*s", agentTypeW, agentLabel)
		}
		if showModel {
			plain += fmt.Sprintf("  %-*s", modelW, modelLabel)
		}
		plain += fmt.Sprintf("  %-*s", stateW, stateLabel(s.AgentState))
		if showStat {
			plain += fmt.Sprintf("  %-*s", statW, statPlain)
		}
		if titleW >= 5 {
			plain += fmt.Sprintf("  %s", title)
		}
		row := lipgloss.NewStyle().
			Foreground(lipgloss.Color(ColorBg0)).
			Background(barBg).
			Bold(true).
			Width(d.width).
			Render(plain)
		return row + "\n"
	}

	// Unselected: coloured state + coloured diff + dimmed agent type, normal fg for the rest.
	stateStr := lipgloss.NewStyle().
		Foreground(stateStyle(s.AgentState).GetForeground()).
		Render(fmt.Sprintf("%-*s", stateW, stateLabel(s.AgentState)))

	agentTypeStr := styleAgentType.Render(paddedAgentLabel)
	modelStr := styleDim.Render(paddedModelLabel)

	var statStr string
	if showStat {
		if s.AgentPath == "" || result.Ok && result.Stat.Files == 0 {
			statStr = styleDim.Render(fmt.Sprintf("%-*s", statW, "—"))
		} else if !result.Ok {
			statStr = styleDim.Render(fmt.Sprintf("%-*s", statW, "?"))
		} else if statW == statWCompact {
			coloured := styleIns.Render(fmt.Sprintf("+%d", result.Stat.Insertions)) +
				" " + styleDel.Render(fmt.Sprintf("-%d", result.Stat.Deletions))
			rawLen := len(fmt.Sprintf("+%d -%d", result.Stat.Insertions, result.Stat.Deletions))
			pad := statW - rawLen
			if pad < 0 {
				pad = 0
			}
			statStr = coloured + strings.Repeat(" ", pad)
		} else {
			fileWord := "files"
			if result.Stat.Files == 1 {
				fileWord = "file "
			}
			plain := fmt.Sprintf("%d %s ", result.Stat.Files, fileWord)
			coloured := styleIns.Render(fmt.Sprintf("+%d", result.Stat.Insertions)) +
				" " + styleDel.Render(fmt.Sprintf("-%d", result.Stat.Deletions))
			rawStat := fmt.Sprintf("%d %s +%d -%d", result.Stat.Files, fileWord, result.Stat.Insertions, result.Stat.Deletions)
			pad := statW - len(rawStat)
			if pad < 0 {
				pad = 0
			}
			statStr = styleFg.Render(plain) + coloured + strings.Repeat(" ", pad)
		}
	}

	prefix := styleFg.Render(fmt.Sprintf(" %s%s", dot, sessionArea))
	row := prefix
	if showType {
		row += styleFg.Render("  ") + agentTypeStr
	}
	if showModel {
		row += styleFg.Render("  ") + modelStr
	}
	row += styleFg.Render("  ") + stateStr
	if showStat {
		row += styleFg.Render("  ") + statStr
	}
	if titleW >= 5 && title != "" {
		row += styleDim.Render("  " + title)
	}
	return row + "\n"
}

// skeletonView renders a minimal loading frame shown before the first DB fetch
// completes. This prevents the blank-frame-before-first-render bug.
func skeletonView(width int) string {
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleHeader := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)

	var sb strings.Builder

	// Compact header: just the wordmark line.
	wordmark := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true).Render("PRISM")
	pad := width - 5
	if pad < 0 {
		pad = 0
	}
	sb.WriteString(strings.Repeat(" ", pad))
	sb.WriteString(wordmark)
	sb.WriteString("\n\n")

	// Separator.
	if width > 0 {
		sb.WriteString(rainbowLineWidth(strings.Repeat("─", width), width))
	}
	sb.WriteString("\n")

	// Column header.
	sb.WriteString(styleHeader.Render(" session"))
	sb.WriteString("\n\n")

	// Loading placeholder.
	sb.WriteString(styleDim.Render("  loading…"))
	sb.WriteString("\n")

	return sb.String()
}

// ── session switch helpers ────────────────────────────────────────────────────

// ensureSessionAndSwitch checks whether the named tmux session exists.
// If it does, it selects the agent window and switches the given client to it.
// If it does not, it looks up the worktree path from the DB; if the directory
// exists on disk it recreates a bare shell session there, then switches.
// Returns a non-empty error string on failure; returns "" on success.
func ensureSessionAndSwitch(sessionName, target string) string {
	if tmux.HasSession(sessionName) {
		_ = tmux.SelectAgentWindow(sessionName)
		if target != "" {
			if err := tmux.SwitchClient(target, sessionName); err != nil {
				return fmt.Sprintf("switch failed: %v", err)
			}
		}
		return ""
	}

	worktreePath := worktreePathFromDB(sessionName)
	if worktreePath == "" {
		return fmt.Sprintf("session %q has no worktree record — use prism cleanup to remove it", sessionName)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		return fmt.Sprintf("worktree directory not found: %s", worktreePath)
	}

	if err := session.Create(sessionName, worktreePath, session.Opts{Layout: session.LayoutBare}); err != nil {
		return fmt.Sprintf("could not recreate session: %v", err)
	}

	if target != "" {
		if err := tmux.SwitchClient(target, sessionName); err != nil {
			return fmt.Sprintf("switch failed: %v", err)
		}
	}
	return ""
}

// worktreePathFromDB looks up the worktree path for a session from the DB only.
// Returns "" if not found or on error.
func worktreePathFromDB(sessionName string) string {
	d, err := openDB()
	if err != nil {
		return ""
	}
	defer d.Close()
	status, err := d.CurrentStatus(sessionName)
	if err != nil || status == nil {
		return ""
	}
	return status.Worktree
}

// ── sentinel watcher ──────────────────────────────────────────────────────────

// dashSentinelPath returns the path to the dashboard sentinel file.
func dashSentinelPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "bus", ".dashboard.signal")
}

// watchDashboardSentinel starts a goroutine that polls the dashboard sentinel
// file for changes and sends a RefreshMsg to the Bubble Tea program when a
// change is detected. The goroutine exits when ctx is cancelled (call the
// returned cancel function after p.Run() returns to stop it cleanly).
//
// Uses a stat-poll rather than inotify/fsnotify to avoid adding a dependency.
// The poll interval is 200ms — well under the 1-second target from the spec.
func watchDashboardSentinel(ctx context.Context, p *tea.Program) {
	sentinelPath := dashSentinelPath()
	var lastMod time.Time
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			info, err := os.Stat(sentinelPath)
			if err == nil {
				mod := info.ModTime()
				if !mod.Equal(lastMod) {
					lastMod = mod
					p.Send(RefreshMsg{})
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
}

// ── cobra command ─────────────────────────────────────────────────────────────

const dashSession = "prism-dashboard"

// ensureDashSession creates the prism-dashboard session if it doesn't exist.
// The session runs `prism dashboard` (persistent mode) directly — no restart
// loop. The persistent dashboard keeps itself alive without exiting on quit.
//
// The session command uses the absolute path of the running prism binary
// (os.Executable) rather than the bare name "prism", so the command works
// even when the tmux pane shell does not have the Nix store path in PATH.
// Similarly, tmux.TmuxBin is used instead of the bare "tmux" string so the
// call respects the ldflags-injected binary path on NixOS.
func ensureDashSession() error {
	if tmux.HasSession(dashSession) {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		// Fall back to "prism" if we cannot resolve our own path.
		self = "prism"
	}
	dashCmd := "'" + strings.ReplaceAll(self, "'", "'\\''") + "' dashboard"
	c := exec.Command(tmux.TmuxBin, "new-session", "-ds", dashSession, "-n", "dashboard", dashCmd)
	return c.Run()
}

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Live agent status dashboard",
	RunE: func(cmd *cobra.Command, args []string) error {
		popup, _ := cmd.Flags().GetBool("popup")
		callerSession, _ := cmd.Flags().GetString("caller-session")

		if popup {
			// Popup mode (C-w): run the TUI directly inside a display-popup frame.
			// callerSession is passed via --caller-session flag so the "you are here"
			// indicator and initial cursor snap work correctly. The popup runs inside
			// the caller's own client (m.client), so no --caller-client flag is needed.
			client, _ := tmux.CurrentClient()
			m := newPopupModel(client, callerSession)
			p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithReportFocus())
			ctx, cancel := context.WithCancel(context.Background())
			watchDashboardSentinel(ctx, p)
			_, err := p.Run()
			cancel()
			return err
		}

		inTmux := os.Getenv("TMUX") != ""

		if inTmux {
			// Check if we are already in the prism-dashboard session — if so,
			// just run the persistent TUI directly (avoids re-entering).
			currentSess, _ := tmux.CurrentSession()
			if currentSess == dashSession {
				// Already in the dashboard session — run the persistent TUI.
				// callerSession is passed for the "you are here" indicator on
				// first load; it is optional (empty is fine).
				client, _ := tmux.CurrentClient()
				m := newPersistentModel(client, callerSession)
				p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithReportFocus())
				ctx, cancel := context.WithCancel(context.Background())
				watchDashboardSentinel(ctx, p)
				_, err := p.Run()
				cancel()
				return err
			}

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
// Uses tmux.TmuxBin (ldflags-injected absolute path on NixOS) so the exec
// succeeds even when "tmux" is not on the invoking shell's PATH.
func syscallExecTmux(session string) error {
	tmuxBin, err := exec.LookPath(tmux.TmuxBin)
	if err != nil {
		// LookPath fails when TmuxBin is already an absolute path that
		// doesn't exist in the PATH search; try using it directly.
		tmuxBin = tmux.TmuxBin
	}
	return syscall.Exec(tmuxBin, []string{"tmux", "attach-session", "-t", session}, os.Environ())
}

func init() {
	dashboardCmd.Flags().Bool("popup", false, "Run as ephemeral popup (spawned by C-w keybinding)")
	dashboardCmd.Flags().String("caller-session", "", "Tmux session name of the invoking client (for 'you are here' indicator)")
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

// ── legacy dashModel (for test compatibility) ──────────────────────────────────
//
// dashModel is kept as a thin wrapper around dashShared + mode-specific fields
// to preserve backward compatibility with existing tests in dashboard_test.go.
// New code should use popupModel or persistentModel directly.
//
// The popup bool here is used only by tests that construct dashModel directly.
// In production code, the mode is determined by the entry point (newPopupModel
// vs newPersistentModel), not by a field on the model.

type dashModel struct {
	dashShared
	client         string
	callerClient   string // immutable after init
	popup          bool
	currentSession string
	cursorActive   bool
}

func newDashModel(client string, popup bool) dashModel {
	currentSession := tmux.CallerSession()
	callerClient := tmux.CallerClient()
	m := dashModel{
		dashShared: dashShared{
			loading: true,
		},
		client:         client,
		callerClient:   callerClient,
		popup:          popup,
		currentSession: currentSession,
		cursorActive:   popup,
	}
	m.displayed = m.sessions
	return m
}

func (m dashModel) Init() tea.Cmd {
	return tea.Batch(fetchSessionsFromDB, fetchGitHubStats, ghTick())
}

func (m dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case RefreshMsg:
		return m, fetchSessionsFromDB

	case dashStatusMsg:
		m.statusMsg = string(msg)
		return m, nil

	case ghTickMsg:
		return m, tea.Batch(fetchGitHubStats, ghTick())

	case githubStatsMsg:
		if !msg.err {
			m.ghOpenPRs = msg.openPRs
		}
		m.ghLoaded = true

	case cursorTimeoutMsg:
		if !m.popup && !m.filterActive {
			m.cursorActive = false
		}

	case tea.BlurMsg:
		if !m.popup && !m.filterActive {
			m.cursorActive = false
		}

	case tea.FocusMsg:
		// Focus regained — cursor stays hidden until user presses j/k.

	case sessionsMsg:
		m.loading = false
		if msg.sessions != nil {
			m.sessions = msg.sessions
		}
		if msg.gitStats != nil {
			m.gitStats = msg.gitStats
		}
		needsSnap := !m.cursorInitialised && !m.filterActive
		if !m.cursorInitialised {
			m.cursorInitialised = true
		}
		m.dashShared = dashRefilterShared(m.dashShared)
		if needsSnap {
			for i, s := range m.displayed {
				if s.Name == m.currentSession {
					m.cursor = i
					break
				}
			}
		}

	case tea.KeyMsg:
		m.statusMsg = ""
		if m.filterActive {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit

			case "esc":
				m.filterActive = false
				m.filterText = ""
				m.dashShared = dashRefilterShared(m.dashShared)
				return m, nil

			case "enter":
				if len(m.displayed) == 0 {
					return m, nil
				}
				selected := m.displayed[m.cursor]
				m.filterActive = false
				m.filterText = ""
				target := legacyDashSwitchTarget(m.popup, m.client, m.callerClient)
				return m, func() tea.Msg {
					if errMsg := ensureSessionAndSwitch(selected.Name, target); errMsg != "" {
						return dashStatusMsg(errMsg)
					}
					return tea.QuitMsg{}
				}

			case "backspace", "ctrl+h":
				if len(m.filterText) > 0 {
					runes := []rune(m.filterText)
					m.filterText = string(runes[:len(runes)-1])
					m.dashShared = dashRefilterShared(m.dashShared)
				}

			case "j", "down":
				if m.cursor < len(m.displayed)-1 {
					m.cursor++
				}

			case "k", "up":
				if m.cursor > 0 {
					m.cursor--
				}

			default:
				if msg.Type == tea.KeyRunes {
					m.filterText += msg.String()
					m.dashShared = dashRefilterShared(m.dashShared)
				}
			}
			return m, nil
		}

		switch msg.String() {
		case "q", "esc":
			if m.popup {
				return m, tea.Quit
			}
			return m, tea.Sequence(
				func() tea.Msg {
					if m.callerClient != "" {
						_ = tmux.SwitchClient(m.callerClient, m.currentSession)
					}
					return nil
				},
				tea.Quit,
			)

		case "/":
			m.filterActive = true
			m.filterText = ""
			m.cursorActive = true
			m.dashShared = dashRefilterShared(m.dashShared)
			return m, nil

		case "j", "down":
			if !m.cursorActive {
				m.cursorActive = true
				return m, cursorTimeoutCmd()
			}
			if m.cursor < len(m.displayed)-1 {
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
				if !m.popup {
					m.cursorActive = true
					return m, cursorTimeoutCmd()
				}
			}
			if len(m.displayed) == 0 {
				return m, nil
			}
			selected := m.displayed[m.cursor]
			target := legacyDashSwitchTarget(m.popup, m.client, m.callerClient)
			return m, func() tea.Msg {
				if errMsg := ensureSessionAndSwitch(selected.Name, target); errMsg != "" {
					return dashStatusMsg(errMsg)
				}
				return tea.QuitMsg{}
			}
		}
	}
	return m, nil
}

func (m dashModel) View() string {
	return dashView(m.dashShared, m.currentSession, m.cursorActive)
}

// legacyDashSwitchTarget is the switch-target helper used by dashModel (the
// legacy test-compatibility model). It mirrors the original dashSwitchTarget
// signature with the popup bool parameter.
func legacyDashSwitchTarget(popup bool, client, callerClient string) string {
	if !popup && callerClient != "" {
		return callerClient
	}
	return client
}
