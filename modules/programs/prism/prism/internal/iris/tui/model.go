package tui

// model.go — bubbletea Model for the iris TUI.
//
// Layout (terminal full-screen):
//
//	┌─────────────────────────────────────────────────────────────┐
//	│  left pane (session list)  │  right pane (event stream)    │
//	│  name  state  role  time   │  narrative lines …            │
//	│  …                         │                               │
//	├─────────────────────────────┴───────────────────────────────┤
//	│  prompt: _                                                  │
//	└─────────────────────────────────────────────────────────────┘
//
// The TUI is a single bubbletea model (Model). All daemon I/O goes through
// the DaemonClient, which delivers messages via Program.Send(). The TUI
// reads NO state from the DB — every piece of state comes via the daemon socket.

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/narrative"
)

// --- Layout constants ---

const (
	leftPaneRatio   = 0.35 // fraction of total width for the session list
	bottomBarHeight = 3    // prompt box height in lines
	minLeftWidth    = 28
	minRightWidth   = 20
)

// --- Colour scheme (gruvbox-dark inspired, same as prism dashboard) ---

const (
	colPrimary    = "#d79921" // yellow
	colSecondary  = "#928374" // grey
	colGreen      = "#98971a"
	colBlue       = "#458588"
	colRed        = "#cc241d"
	colForeground = "#ebdbb2"
	colBg0        = "#282828"
	colBg1        = "#3c3836"
	colBg2        = "#504945"
)

// --- Styles ---

var (
	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color(colSecondary))

	styleHeader = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colPrimary)).
			Bold(true)

	styleSelected = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colBg0)).
			Background(lipgloss.Color(colPrimary)).
			Bold(true)

	styleNormal = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colForeground))

	styleDim = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colSecondary))

	styleError = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colRed))

	styleGreen = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colGreen))

	styleBlue = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colBlue))

	// styleYellow highlights sessions in `waiting` state — paused for the
	// next user prompt. Distinct from `active` (green) and `finished`
	// (dim) so operators can spot attention-needed sessions at a glance.
	styleYellow = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colPrimary)).
			Bold(true)

	stylePromptBox = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color(colBlue))
)

// --- Model ---

// sessionItem holds one session row in the left pane.
type sessionItem struct {
	snap iris.SessionSnapshot
}

// Model is the top-level bubbletea model for the iris TUI.
type Model struct {
	client *DaemonClient

	// Terminal dimensions.
	width  int
	height int

	// Connection state.
	connected    bool
	connectError string
	reconnecting bool

	// Session list (left pane).
	sessions []sessionItem
	cursor   int // selected row index

	// initialSession is the session name to focus on when the first
	// sessions_snapshot frame arrives. Empty means "use the daemon's
	// natural ordering and select the first row". Set by the --session
	// CLI flag (and consumed/cleared after first use).
	initialSession string

	// Subscribed session name (may differ from sessions[cursor].snap.Name
	// transiently during session switching).
	subscribedTo string

	// Event stream (right pane): rendered narrative lines, newest at bottom.
	eventLines []narrative.NarrativeLine
	// seenRowIDs deduplicates replayed vs live events.
	seenRowIDs map[int64]bool
	// toolCallByMsgID indexes the line slice position for open tool_call lines
	// so that arriving tool_result frames can append the result inline.
	toolCallByMsgID map[string]int

	// eventScroll is the number of lines scrolled up from the bottom (0 = live).
	eventScroll int

	// Prompt input (bottom).
	promptRunes  []rune
	promptCursor int // rune insert position
}

// NewModel creates the iris TUI model.
func NewModel(client *DaemonClient) Model {
	return Model{
		client:          client,
		seenRowIDs:      make(map[int64]bool),
		toolCallByMsgID: make(map[string]int),
	}
}

// NewModelFocused is like NewModel but pre-seeds an initial session name. On
// the first sessions_snapshot frame, if a session with that name is present
// the cursor is positioned on it (and the subscription targets it) instead
// of defaulting to row 0. Used by `iris tui --session <name>` so that the
// `iris switch` picker can hand the TUI a specific session to focus on.
func NewModelFocused(client *DaemonClient, initialSession string) Model {
	m := NewModel(client)
	m.initialSession = initialSession
	return m
}

func (m Model) Init() tea.Cmd {
	return nil
}

// --- Update ---

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case ConnectedMsg:
		m.connected = true
		m.reconnecting = false
		m.connectError = ""
		// The DaemonClient sends sessions_list immediately on connect;
		// no need to dispatch a redundant Cmd here.
		return m, nil

	case DisconnectedMsg:
		m.connected = false
		if msg.Err != nil {
			m.connectError = msg.Err.Error()
		}
		m.reconnecting = true
		return m, nil

	case DaemonFrame:
		return m.handleDaemonFrame(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m Model) handleDaemonFrame(msg DaemonFrame) (tea.Model, tea.Cmd) {
	switch msg.RawType {

	case iris.DaemonFrameSessionsSnapshot:
		if msg.Snapshot == nil {
			return m, nil
		}
		prevSelected := ""
		if m.cursor < len(m.sessions) {
			prevSelected = m.sessions[m.cursor].snap.Name
		}
		// On the first snapshot, an initialSession from --session takes
		// precedence over the previous-cursor heuristic. Consume the field
		// after applying it so later snapshots fall back to prevSelected.
		target := prevSelected
		if m.initialSession != "" {
			target = m.initialSession
			m.initialSession = ""
		}
		m.sessions = make([]sessionItem, len(msg.Snapshot.Sessions))
		for i, s := range msg.Snapshot.Sessions {
			m.sessions[i] = sessionItem{snap: s}
		}
		// Restore cursor position if the target session is present.
		m.cursor = 0
		for i, si := range m.sessions {
			if si.snap.Name == target {
				m.cursor = i
				break
			}
		}
		// Subscribe to the newly selected session if not already subscribed.
		if len(m.sessions) > 0 {
			name := m.sessions[m.cursor].snap.Name
			if name != m.subscribedTo {
				m.subscribedTo = name
				m.resetEventPane()
				return m, func() tea.Msg {
					_ = m.client.SendSessionSubscribe(name, 0)
					return nil
				}
			}
		}

	case iris.DaemonFrameSessionEvent:
		if msg.Event == nil {
			return m, nil
		}
		e := msg.Event
		if m.seenRowIDs[e.RowID] {
			return m, nil
		}
		m.seenRowIDs[e.RowID] = true

		lines := narrative.RenderEvent(e.RowID, e.EventType, e.Payload)
		for _, line := range lines {
			// For tool_result: find the matching tool_call and pair it.
			if e.EventType == "tool_result" && line.MessageID != "" {
				if idx, ok := m.toolCallByMsgID[line.MessageID]; ok {
					// Append result text to the tool_call line instead of
					// adding a separate line.
					if idx < len(m.eventLines) {
						existing := m.eventLines[idx]
						existing.IsPaired = true
						existing.ResultText = line.Text
						existing.Text = existing.Text + " " + line.Text
						m.eventLines[idx] = existing
					}
					continue
				}
			}
			pos := len(m.eventLines)
			m.eventLines = append(m.eventLines, line)
			// Index tool_call lines for later tool_result pairing.
			if e.EventType == "tool_call" && line.MessageID != "" {
				m.toolCallByMsgID[line.MessageID] = pos
			}
		}
		// Snap to bottom if the user is not scrolled up.
		if m.eventScroll == 0 {
			// (view already renders from bottom; nothing extra needed)
		}

	case iris.DaemonFrameSessionState:
		if msg.State == nil {
			return m, nil
		}
		// Update the in-memory session list state.
		for i, si := range m.sessions {
			if si.snap.Name == msg.State.SessionName {
				m.sessions[i].snap.State = msg.State.State
				break
			}
		}

	case iris.DaemonFrameSessionSpawned:
		// A new session has been spawned. Append it to the session list so
		// the user sees it without restarting the TUI.
		//
		// Malformed frames (missing payload or empty name) are skipped:
		// rendering an empty row would be worse than no-op, and a future
		// sessions_snapshot will reconcile the list anyway.
		if msg.Spawned == nil || msg.Spawned.Name == "" {
			return m, nil
		}
		spawned := msg.Spawned
		// Build a snapshot for the new row. Prefer the daemon-supplied
		// Session record (which carries state/role/worktree/started_at); fall
		// back to the minimal Name+InstanceID fields for forward compat with
		// older daemons that don't populate Session.
		var snap iris.SessionSnapshot
		if spawned.Session != nil {
			snap = *spawned.Session
		} else {
			snap = iris.SessionSnapshot{
				Name:       spawned.Name,
				InstanceID: spawned.InstanceID,
			}
		}
		// Dedupe defensively: if the session is already in the list (e.g.
		// the daemon emitted both a snapshot and a session_spawned for the
		// same incarnation), treat the frame as an update rather than a
		// duplicate-append.
		for i, si := range m.sessions {
			if si.snap.Name == snap.Name {
				m.sessions[i].snap = snap
				return m, nil
			}
		}
		// Append. sessions_snapshot itself is unsorted (it iterates the
		// supervisor map in cmd/iris/main.go), so appending is consistent
		// with the existing ordering — the new row simply appears at the
		// bottom of the list.
		wasEmpty := len(m.sessions) == 0
		m.sessions = append(m.sessions, sessionItem{snap: snap})
		// Auto-subscribe when the list transitions from empty to non-empty
		// and there is no existing subscription. This mirrors the
		// sessions_snapshot auto-subscribe path so a session spawned while
		// the TUI is open with an empty list becomes immediately usable
		// (prompt send is gated on subscribedTo != ""). We deliberately do
		// NOT switch subscription when the list was already non-empty —
		// that would steal focus from a session the user is actively
		// watching whenever a sibling spawns.
		if wasEmpty && m.subscribedTo == "" {
			m.cursor = 0
			name := snap.Name
			m.subscribedTo = name
			m.resetEventPane()
			return m, func() tea.Msg {
				_ = m.client.SendSessionSubscribe(name, 0)
				return nil
			}
		}

	case iris.DaemonFrameError:
		if msg.Error != nil {
			// Show error as a narrative line in the event pane.
			m.eventLines = append(m.eventLines, narrative.NarrativeLine{
				Text:      fmt.Sprintf("⚠ daemon error: %s", msg.Error.Message),
				EventType: "error",
			})
		}
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit

	case "up", "ctrl+p", "k":
		if m.cursor > 0 {
			m.cursor--
			return m, m.switchToSelected()
		}

	case "down", "ctrl+n", "j":
		if m.cursor < len(m.sessions)-1 {
			m.cursor++
			return m, m.switchToSelected()
		}

	case "pgup":
		m.eventScroll += (m.rightPaneHeight() - 2)
		return m, nil

	case "pgdown":
		m.eventScroll -= (m.rightPaneHeight() - 2)
		if m.eventScroll < 0 {
			m.eventScroll = 0
		}
		return m, nil

	case "enter":
		if len(m.promptRunes) > 0 && m.subscribedTo != "" {
			text := string(m.promptRunes)
			m.promptRunes = nil
			m.promptCursor = 0
			return m, func() tea.Msg {
				_ = m.client.SendPromptDeliver(m.subscribedTo, text)
				return nil
			}
		}

	case "backspace", "ctrl+h":
		if m.promptCursor > 0 {
			m.promptRunes = append(m.promptRunes[:m.promptCursor-1], m.promptRunes[m.promptCursor:]...)
			m.promptCursor--
		}

	case "left", "ctrl+b":
		if m.promptCursor > 0 {
			m.promptCursor--
		}

	case "right", "ctrl+f":
		if m.promptCursor < len(m.promptRunes) {
			m.promptCursor++
		}

	case "home", "ctrl+a":
		m.promptCursor = 0

	case "end", "ctrl+e":
		m.promptCursor = len(m.promptRunes)

	case "delete", "ctrl+d":
		if m.promptCursor < len(m.promptRunes) {
			m.promptRunes = append(m.promptRunes[:m.promptCursor], m.promptRunes[m.promptCursor+1:]...)
		}

	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			ins := []rune(msg.String())
			newRunes := make([]rune, len(m.promptRunes)+len(ins))
			copy(newRunes, m.promptRunes[:m.promptCursor])
			copy(newRunes[m.promptCursor:], ins)
			copy(newRunes[m.promptCursor+len(ins):], m.promptRunes[m.promptCursor:])
			m.promptRunes = newRunes
			m.promptCursor += len(ins)
		}
	}

	return m, nil
}

// switchToSelected unsubscribes from the previous session and subscribes to
// the newly selected one, clearing the event pane.
func (m *Model) switchToSelected() tea.Cmd {
	if m.cursor >= len(m.sessions) {
		return nil
	}
	newName := m.sessions[m.cursor].snap.Name
	if newName == m.subscribedTo {
		return nil
	}
	prev := m.subscribedTo
	m.subscribedTo = newName
	m.resetEventPane()

	return func() tea.Msg {
		if prev != "" {
			_ = m.client.SendSessionUnsubscribe(prev)
		}
		_ = m.client.SendSessionSubscribe(newName, 0)
		return nil
	}
}

// resetEventPane clears the event buffer and associated indexes.
func (m *Model) resetEventPane() {
	m.eventLines = nil
	m.seenRowIDs = make(map[int64]bool)
	m.toolCallByMsgID = make(map[string]int)
	m.eventScroll = 0
}

// --- View ---

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}

	// Disconnected overlay.
	if !m.connected {
		return m.viewDisconnected()
	}

	leftW, rightW := m.paneWidths()

	leftPane := m.viewLeftPane(leftW)
	rightPane := m.viewRightPane(rightW)

	body := lipgloss.JoinHorizontal(lipgloss.Top, leftPane, rightPane)
	prompt := m.viewPrompt(m.width)

	return lipgloss.JoinVertical(lipgloss.Left, body, prompt)
}

func (m Model) viewDisconnected() string {
	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(styleError.Render("  ✗ iris daemon not connected"))
	sb.WriteString("\n\n")
	if m.connectError != "" {
		sb.WriteString(styleDim.Render("  " + m.connectError))
		sb.WriteString("\n")
	}
	if m.reconnecting {
		sb.WriteString(styleDim.Render("  Reconnecting…"))
	}
	sb.WriteString("\n\n")
	sb.WriteString(styleDim.Render("  Make sure the daemon is running:  iris daemon"))
	sb.WriteString("\n")
	sb.WriteString(styleDim.Render("  Press q or ctrl+c to quit."))
	return sb.String()
}

func (m Model) paneWidths() (int, int) {
	leftW := int(float64(m.width) * leftPaneRatio)
	if leftW < minLeftWidth {
		leftW = minLeftWidth
	}
	rightW := m.width - leftW
	if rightW < minRightWidth {
		rightW = minRightWidth
		leftW = m.width - rightW
	}
	return leftW, rightW
}

func (m Model) leftPaneHeight() int {
	h := m.height - bottomBarHeight - 2 // minus prompt box and borders
	if h < 1 {
		h = 1
	}
	return h
}

func (m Model) rightPaneHeight() int {
	return m.leftPaneHeight()
}

// viewLeftPane renders the session list.
func (m Model) viewLeftPane(width int) string {
	innerW := width - 2 // border left+right
	paneH := m.leftPaneHeight()

	var rows []string

	// Header.
	header := styleHeader.Render(padRight("Sessions", innerW))
	rows = append(rows, header)
	rows = append(rows, styleDim.Render(strings.Repeat("─", innerW)))

	if len(m.sessions) == 0 {
		rows = append(rows, "")
		rows = append(rows, styleDim.Render(padRight("  no sessions", innerW)))
		rows = append(rows, "")
		rows = append(rows, styleDim.Render(padRight("  iris spawn --worktree <path>", innerW)))
	} else {
		// Column widths.
		nameW := innerW - 14 // leave room for state + role
		if nameW < 8 {
			nameW = 8
		}
		for i, si := range m.sessions {
			name := truncate(si.snap.Name, nameW)
			state := stateLabel(si.snap.State)
			role := truncate(si.snap.Role, 6)

			line := fmt.Sprintf(" %-*s %-8s %-6s", nameW, name, state, role)
			line = padRight(line, innerW)

			if i == m.cursor {
				rows = append(rows, styleSelected.Render(line))
			} else {
				rows = append(rows, styleNormal.Render(line))
			}
		}
	}

	// Pad to full height.
	for len(rows) < paneH {
		rows = append(rows, "")
	}
	rows = rows[:paneH]

	content := strings.Join(rows, "\n")
	return styleBorder.Width(innerW).Height(paneH).Render(content)
}

// viewRightPane renders the event stream for the subscribed session.
func (m Model) viewRightPane(width int) string {
	innerW := width - 2
	paneH := m.rightPaneHeight()

	var rows []string

	// Header.
	title := "Events"
	if m.subscribedTo != "" {
		title = "Events: " + m.subscribedTo
	}
	rows = append(rows, styleHeader.Render(padRight(truncate(title, innerW), innerW)))
	rows = append(rows, styleDim.Render(strings.Repeat("─", innerW)))

	contentH := paneH - 2 // minus header and separator
	if contentH < 1 {
		contentH = 1
	}

	if len(m.eventLines) == 0 {
		rows = append(rows, "")
		if m.subscribedTo != "" {
			rows = append(rows, styleDim.Render(padRight("  waiting for events…", innerW)))
		} else {
			rows = append(rows, styleDim.Render(padRight("  select a session to stream events", innerW)))
		}
	} else {
		// Render from the bottom up, respecting scroll offset.
		total := len(m.eventLines)
		end := total - m.eventScroll
		if end < 0 {
			end = 0
		}
		if end > total {
			end = total
		}
		start := end - contentH
		if start < 0 {
			start = 0
		}
		for _, line := range m.eventLines[start:end] {
			rendered := styleEventLine(line, innerW)
			rows = append(rows, rendered)
		}
	}

	// Scroll indicator.
	if m.eventScroll > 0 {
		rows = append(rows, styleDim.Render(
			padRight(fmt.Sprintf("  ↑ %d lines above (PgDn to scroll down)", m.eventScroll), innerW),
		))
	}

	// Pad to full height.
	for len(rows) < paneH {
		rows = append(rows, "")
	}
	rows = rows[:paneH]

	content := strings.Join(rows, "\n")
	return styleBorder.Width(innerW).Height(paneH).Render(content)
}

// styleEventLine applies colour to a NarrativeLine.
func styleEventLine(line narrative.NarrativeLine, width int) string {
	text := truncate(line.Text, width)
	switch line.EventType {
	case "state_change":
		return styleGreen.Render(padRight(text, width))
	case "msg_assistant", "msg_assistant_body":
		return styleNormal.Render(padRight(text, width))
	case "msg_user", "msg_user_body":
		return styleBlue.Render(padRight(text, width))
	case "tool_call":
		return styleDim.Render(padRight(text, width))
	case "tool_result":
		return styleDim.Render(padRight(text, width))
	case "permission_ask":
		return styleError.Render(padRight(text, width))
	case "permission_denied":
		return styleError.Render(padRight(text, width))
	case "error":
		return styleError.Render(padRight(text, width))
	default:
		return styleDim.Render(padRight(text, width))
	}
}

// viewPrompt renders the bottom prompt input.
func (m Model) viewPrompt(width int) string {
	innerW := width - 2
	if innerW < 1 {
		innerW = 1
	}

	var label string
	if m.subscribedTo != "" {
		label = fmt.Sprintf("prompt → %s: ", m.subscribedTo)
	} else {
		label = "prompt: "
	}

	labelStyle := styleHeader
	labelRendered := labelStyle.Render(label)

	// Cursor rendering.
	before := string(m.promptRunes[:m.promptCursor])
	after := string(m.promptRunes[m.promptCursor:])
	var caretStr string
	if len(after) > 0 {
		caretRunes := []rune(after)
		caretStr = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colBg0)).
			Background(lipgloss.Color(colPrimary)).
			Render(string(caretRunes[0]))
		after = string(caretRunes[1:])
	} else {
		caretStr = styleDim.Render("█")
	}

	inputLine := labelRendered + before + caretStr + after

	help := styleDim.Render("  ↑/↓ select session  enter send  pgup/pgdn scroll events  q quit")

	return stylePromptBox.Width(innerW).Render(inputLine + "\n" + help)
}

// --- Utilities ---

// padRight pads or truncates s to exactly w display columns.
func padRight(s string, w int) string {
	cols := displayWidth(s)
	if cols >= w {
		return s
	}
	return s + strings.Repeat(" ", w-cols)
}

// truncate truncates s to at most w display columns, appending "…" if needed.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	cols := displayWidth(s)
	if cols <= w {
		return s
	}
	// Trim runes until we fit.
	runes := []rune(s)
	result := runes
	for displayWidth(string(result)) > w-1 && len(result) > 0 {
		result = result[:len(result)-1]
	}
	return string(result) + "…"
}

// displayWidth returns the display column width of s (approximated as rune count
// for ASCII-heavy content; full unicode width would require a heavier dependency).
func displayWidth(s string) int {
	return utf8.RuneCountInString(s)
}

// stateLabel returns a short coloured state label.
func stateLabel(state string) string {
	switch state {
	case "active":
		return styleGreen.Render("active  ")
	case "waiting":
		return styleYellow.Render("waiting ")
	case "spawning":
		return styleBlue.Render("spawning")
	case "finished":
		return styleDim.Render("finished")
	case "error":
		return styleError.Render("error   ")
	default:
		return styleDim.Render(padRight(state, 8))
	}
}

// formatRelTime returns a short relative-time string (≤ 8 chars).
func formatRelTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Run starts the bubbletea program and connects the daemon client.
// It blocks until the user quits.
func Run(sockPath string) error {
	return RunFocused(sockPath, "")
}

// RunFocused is like Run but pre-selects a session by name on the first
// sessions_snapshot frame. Used by `iris tui --session <name>` so the
// context-switcher picker can hand off to the TUI focused on a specific
// session. An empty initialSession is equivalent to Run.
func RunFocused(sockPath, initialSession string) error {
	client := NewDaemonClient(sockPath)
	m := NewModelFocused(client, initialSession)

	opts := []tea.ProgramOption{
		tea.WithAltScreen(),
	}
	p := tea.NewProgram(m, opts...)
	client.SetProgram(p)

	// Connect asynchronously so the TUI can render the disconnected state
	// immediately while the dial is in progress.
	go client.Connect()

	_, err := p.Run()
	return err
}
