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

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/sidebar-spike/internal/mockdata"
	"github.com/prismatic-koi/nixos-config/modules/programs/prism/sidebar-spike/internal/model"
	"github.com/prismatic-koi/nixos-config/modules/programs/prism/sidebar-spike/internal/sidebar"
)

// tickInterval is the wall-clock interval the animation engine fires
// at. State transitions are evaluated against the elapsed time on
// each tick.
const tickInterval = 200 * time.Millisecond

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

func (m rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			m.sidebar.MoveUp()
		case "down", "j":
			m.sidebar.MoveDown()
		case "left", "h":
			m.sidebar.MoveLeft()
		case "right", "l":
			m.sidebar.MoveRight()
		case "enter":
			// Enter just confirms the current selection — the right
			// pane already reflects what the cursor is on, so there
			// is no separate "selected session" to track in v1.
		case "tab":
			m.sidebar.CycleNextPane()
		case "shift+tab":
			m.sidebar.CyclePrevPane()
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
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
		// Reset the tree by rebuilding from the fixture. Cheap, and
		// simpler than tracking per-session original states.
		fresh := mockdata.ByName(m.fixture.Name)
		// Preserve the user's expand/collapse choices and pane
		// cursor across the loop — it's jarring to have the UI
		// snap back to defaults mid-observation. Walk both trees in
		// parallel and copy across.
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

// copyUserChoices preserves Repo.Expanded and Session.ActivePane from
// src into dst on a name-match basis. Anything not present in dst is
// silently dropped.
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
		// Pre-windowsize message; render nothing rather than a
		// zero-sized lipgloss box.
		return "initialising…"
	}

	// Left: sidebar at fixed width.
	left := m.sidebar.View(m.height)

	// Right: placeholder for the selected session's active pane.
	right := renderRightPane(m.sidebar, m.width-sidebar.Width, m.height)

	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
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
