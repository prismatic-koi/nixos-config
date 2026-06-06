// sidebar-spike is a non-functional UI mockup of the herdr-shape
// sidebar planned for the prism multiplexer programme (issue #2147,
// child #2148). It exists so the visual design of the sidebar can be
// refined interactively before planned PR #3 (`internal/mux/render/`)
// implements against it.
//
// What is real:
//   - the bubbletea render loop
//   - keyboard input handling
//   - the state-to-glyph + state-to-colour mapping under refinement
//
// What is mocked:
//   - PTYs (none; the right pane is a static placeholder)
//   - session data (fixture from internal/mockdata)
//   - bus subscription (a wall-clock ticker drives scripted state
//     transitions from the fixture)
//
// Surface: a single `nix run .#sidebar-spike` invocation. Optional
// `--fixture <name>` to swap between fixtures.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/sidebar-spike/internal/mockdata"
	"github.com/prismatic-koi/nixos-config/modules/programs/prism/sidebar-spike/internal/model"
	"github.com/prismatic-koi/nixos-config/modules/programs/prism/sidebar-spike/internal/sidebar"
)

// tickInterval is the wall-clock interval the animation engine fires
// at. State transitions are evaluated against the elapsed time on
// each tick.
const tickInterval = 200 * time.Millisecond

// narrowWidthThreshold is the terminal-width cutoff below which the
// UI switches to mobile-style narrow layout: no split-pane sidebar,
// instead a top status bar and a popover triggered by Ctrl-B.
//
// Rationale for 80: at total width 80 a 32-col sidebar leaves 48
// cols for the right pane, which is too narrow for most code. The
// crossover where a full-width pane is clearly better than the split
// sits around this width.
//
// Revisable. See design-notes.md.
const narrowWidthThreshold = 80

// narrowPopoverWidth is the width of the sidebar popover when it is
// open in narrow mode. Capped at min(sidebar.Width+6, terminalWidth-4)
// so the popover always has some background of the right pane
// peeking through the right edge.
const narrowPopoverWidth = sidebar.Width + 6

// tickMsg is delivered by the animation engine on each tick.
type tickMsg time.Time

// tick returns a tea.Cmd that fires a tickMsg after tickInterval.
func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// rootModel is the top-level bubbletea model — owns the tree, the
// sidebar component, the wall-clock-elapsed counter the transitions
// drive against, and the window dimensions.
type rootModel struct {
	sidebar sidebar.Model
	fixture mockdata.Fixture

	startedAt   time.Time
	transitions []model.Transition // copy we mutate as we apply them
	loopAfter   time.Duration

	width  int
	height int

	// popoverOpen is true when the user has pressed Ctrl-B in narrow
	// mode. The sidebar then renders as an overlay on top of the
	// active pane. Always false in wide mode (the sidebar is always
	// visible there).
	popoverOpen bool
}

func newRootModel(fix mockdata.Fixture) rootModel {
	return rootModel{
		sidebar: sidebar.Model{
			Tree:   fix.Tree,
			Cursor: 0,
		},
		fixture:     fix,
		startedAt:   time.Now(),
		transitions: append([]model.Transition(nil), fix.Transitions...),
		loopAfter:   fix.LoopAfter,
	}
}

func (m rootModel) Init() tea.Cmd {
	return tick()
}

// isNarrow returns true when the current terminal width should use
// the mobile layout.
func (m rootModel) isNarrow() bool {
	return m.width > 0 && m.width < narrowWidthThreshold
}

func (m rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "ctrl+b":
			// In narrow mode, Ctrl-B toggles the sidebar popover.
			// In wide mode it is a no-op (the sidebar is always
			// visible). Keystroke mirrors tmux's prefix convention,
			// revisable in iteration.
			if m.isNarrow() {
				m.popoverOpen = !m.popoverOpen
			}
			return m, nil
		case "esc":
			// Closes the popover without making a selection.
			if m.popoverOpen {
				m.popoverOpen = false
			}
			return m, nil
		case "up", "k":
			m.sidebar.MoveUp()
		case "down", "j":
			m.sidebar.MoveDown()
		case "left", "h":
			m.sidebar.MoveLeft()
		case "right", "l":
			m.sidebar.MoveRight()
		case "enter":
			// Enter confirms the current selection. In narrow mode
			// with the popover open it also closes the popover —
			// matches herdr's mobile pattern where tapping a row
			// dismisses the switcher.
			if m.popoverOpen {
				m.popoverOpen = false
			}
		case "tab":
			m.sidebar.CycleNextPane()
		case "shift+tab":
			m.sidebar.CyclePrevPane()
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Close the popover if the terminal grew back into wide
		// territory — leaving it open would be confusing once the
		// inline sidebar is visible again.
		if !m.isNarrow() {
			m.popoverOpen = false
		}
		return m, nil

	case tickMsg:
		m.applyDueTransitions()
		return m, tick()
	}
	return m, nil
}

// applyDueTransitions walks the transition timeline and applies every
// transition whose At <= elapsed, then resets the elapsed clock when
// loopAfter is reached so the animation keeps cycling.
func (m *rootModel) applyDueTransitions() {
	elapsed := time.Since(m.startedAt)
	if m.loopAfter > 0 && elapsed > m.loopAfter {
		// Restart the loop: reset the tree to its initial state and
		// reseed the transition list from the fixture.
		m.startedAt = time.Now()
		m.transitions = append([]model.Transition(nil), m.fixture.Transitions...)
		fresh := mockdata.ByName(m.fixture.Name)
		// Preserve the user's expand/collapse choices and pane
		// cursor across the loop — it's jarring to have the UI
		// snap back to defaults mid-observation.
		copyUserChoices(m.sidebar.Tree, fresh.Tree)
		m.sidebar.Tree = fresh.Tree
		m.fixture = fresh
		return
	}

	remaining := m.transitions[:0]
	for _, t := range m.transitions {
		if t.At > elapsed {
			remaining = append(remaining, t)
			continue
		}
		applyTransition(m.sidebar.Tree, t)
	}
	m.transitions = remaining
}

// copyUserChoices preserves Repo.Expanded, Session.ExpandedReviews
// and Session.ActivePane from src into dst on a name-match basis.
// Anything not present in dst is silently dropped.
func copyUserChoices(src, dst *model.Tree) {
	if src == nil || dst == nil {
		return
	}
	for _, sRepo := range src.Repos {
		for _, dRepo := range dst.Repos {
			if dRepo.Name != sRepo.Name {
				continue
			}
			dRepo.Expanded = sRepo.Expanded
			for _, sSess := range sRepo.Sessions {
				for _, dSess := range dRepo.Sessions {
					if dSess.Name == sSess.Name {
						dSess.ActivePane = sSess.ActivePane
						dSess.ExpandedReviews = sSess.ExpandedReviews
					}
				}
			}
		}
	}
}

// applyTransition resolves t.Target against the tree and updates the
// matched session's state. Unresolved targets are silently ignored —
// the fixture is hand-authored, but a typo shouldn't crash the spike.
func applyTransition(t *model.Tree, tr model.Transition) {
	parts := strings.Split(tr.Target, "/")
	if len(parts) < 2 {
		return
	}
	repoName, sessName := parts[0], parts[1]
	var subName string
	if len(parts) >= 3 {
		subName = parts[2]
	}
	for _, repo := range t.Repos {
		if repo.Name != repoName {
			continue
		}
		for _, sess := range repo.Sessions {
			if sess.Name != sessName {
				continue
			}
			if subName == "" {
				sess.State = tr.NewState
				return
			}
			for _, sub := range sess.Subsessions {
				if sub.Name == subName {
					sub.State = tr.NewState
					return
				}
			}
			return
		}
	}
}

func (m rootModel) View() string {
	if m.width == 0 || m.height == 0 {
		return "initialising…"
	}
	if m.isNarrow() {
		return m.viewNarrow()
	}
	return m.viewWide()
}

// viewWide is the v1 split-pane layout: sidebar at fixed width on the
// left, pane host on the right.
func (m rootModel) viewWide() string {
	left := m.sidebar.View(sidebar.Width, m.height)
	right := renderRightPane(m.sidebar, m.width-sidebar.Width, m.height)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// viewNarrow renders the mobile-shape layout: a thin status bar at
// the top showing current session identity, the active pane filling
// the rest of the screen, and an optional popover overlay holding the
// sidebar tree when the user has hit Ctrl-B.
//
// Modelled on herdr's mobile pattern (see herdr.dev/docs mobile
// screenshots); clean-room replication per the AGPL constraint in
// proposal §7.4.
func (m rootModel) viewNarrow() string {
	topbar := sidebar.Topbar(m.sidebar, m.width, m.popoverOpen)
	paneHeight := m.height - 1
	if paneHeight < 1 {
		paneHeight = 1
	}
	pane := renderRightPane(m.sidebar, m.width, paneHeight)
	base := lipgloss.JoinVertical(lipgloss.Left, topbar, pane)

	if !m.popoverOpen {
		return base
	}

	// Popover: composite the sidebar's standard View on top of the
	// pane background. Width is capped so the right edge of the pane
	// peeks through, signalling the popover is dismissible.
	popoverW := narrowPopoverWidth
	if popoverW > m.width-4 {
		popoverW = m.width - 4
	}
	if popoverW < sidebar.Width {
		popoverW = sidebar.Width
	}
	popoverH := m.height - 2
	if popoverH < 4 {
		popoverH = 4
	}
	overlay := m.sidebar.View(popoverW, popoverH)
	// Top-left anchor with one-row top offset so the topbar stays
	// visible above it. lipgloss doesn't ship a Z-index composer,
	// so we splice the overlay into the base string ourselves.
	return overlayAt(base, overlay, 1, 0)
}

// overlayAt splices an overlay block into base at (row, col), one row
// per source string-line, padding base lines to col with spaces. Used
// to render the narrow-mode popover on top of the pane background.
func overlayAt(base, overlay string, row, col int) string {
	baseLines := strings.Split(base, "\n")
	overlayLines := strings.Split(overlay, "\n")
	for i, oLine := range overlayLines {
		dst := row + i
		if dst >= len(baseLines) {
			break
		}
		oWidth := ansi.StringWidth(oLine)
		// Pad/truncate the base line so we land at the right column.
		left := ansi.Truncate(baseLines[dst], col, "")
		leftW := ansi.StringWidth(left)
		if leftW < col {
			left += strings.Repeat(" ", col-leftW)
		}
		// Trim the tail past the overlay so we don't double up.
		afterCol := col + oWidth
		baseLineW := ansi.StringWidth(baseLines[dst])
		var tail string
		if baseLineW > afterCol {
			// Skip past `afterCol` cells of the base line.
			tail = ansi.Cut(baseLines[dst], afterCol, baseLineW)
		}
		baseLines[dst] = left + oLine + tail
	}
	return strings.Join(baseLines, "\n")
}

// renderRightPane renders the [mock pi pane for session X] placeholder
// for the currently-selected session. The session name, the active
// pane, and the state are all shown so the parent UI demonstrates the
// shape of the real pane host without having to host a real PTY.
func renderRightPane(s sidebar.Model, width, height int) string {
	sess := s.SelectedSession()
	style := lipgloss.NewStyle().
		Width(width).
		Height(height).
		Padding(1, 2).
		Foreground(lipgloss.Color("#a1a1aa"))

	if sess == nil {
		return style.Render("(no session selected)")
	}

	activePane := "—"
	if len(sess.Panes) > 0 {
		activePane = string(sess.Panes[sess.ActivePane])
	}

	hint := lipgloss.NewStyle().Foreground(lipgloss.Color("#52525b")).Italic(true)

	lines := []string{
		lipgloss.NewStyle().Foreground(lipgloss.Color("#fafafa")).Bold(true).Render(
			fmt.Sprintf("[mock pi pane for %s]", sess.Name),
		),
		"",
		fmt.Sprintf("state:  %s", sess.State),
		fmt.Sprintf("active pane:  %s   (of %s)", activePane, paneList(sess.Panes)),
		"",
		hint.Render("This pane is intentionally non-functional. The"),
		hint.Render("sidebar-spike validates the visual layer only;"),
		hint.Render("the real pane host lives in planned PR #3."),
		"",
		hint.Render("Use ↑↓ to walk the sidebar, ←→ to collapse repos,"),
		hint.Render("Tab / Shift-Tab to cycle this session's panes."),
	}
	return style.Render(strings.Join(lines, "\n"))
}

func paneList(panes []model.Pane) string {
	parts := make([]string, len(panes))
	for i, p := range panes {
		parts[i] = string(p)
	}
	return strings.Join(parts, ", ")
}

func main() {
	var fixtureName string
	flag.StringVar(&fixtureName, "fixture", "default", "fixture name: default | minimal")
	flag.Parse()

	fix := mockdata.ByName(fixtureName)
	p := tea.NewProgram(newRootModel(fix), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "sidebar-spike: %v\n", err)
		os.Exit(1)
	}
}
