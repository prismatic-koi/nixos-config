package render

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// Model is the bubbletea root model for the multiplexer's UI. One
// process; one Model. It carries the cursor, expand/collapse maps, and
// window dimensions; the data (sessions, panes, state, PTY content)
// flows through the *SessionTree + the two optional providers.
//
// Construct with New. The zero Model is NOT ready for use — the
// expansion maps are nil and would panic on the first key press.
type Model struct {
	// Tree is the session-tree data source. Must be non-nil. The renderer
	// only reads through the SessionTree's accessor methods, so external
	// mutations (server adding a session, sidecar publishing a state
	// change) are picked up on the next View() call without any
	// subscription wiring.
	Tree *pane.SessionTree

	// States resolves a session ID to a State. Optional — a nil States
	// reports every session as StateIdle.
	States StateProvider

	// Hosts resolves a (sessionID, paneName) pair to a *vt.Host. Optional
	// — a nil Hosts renders the "(no PTY)" placeholder in the active
	// pane area.
	Hosts HostProvider

	// cursor is the row index into flatten() of the current selection.
	cursor int

	// repoExp keys are repo names. true = expanded, false = collapsed.
	// Missing entries default to true (repos open) per §3.1.
	repoExp map[string]bool

	// sessExp keys are top-level session IDs. true = reviews expanded,
	// false = reviews collapsed. Missing entries default to false (the
	// §3.1 "reviews collapsed by default" rule).
	sessExp map[string]bool

	// width and height are the most recent tea.WindowSizeMsg dimensions.
	// Zero until the first message; View renders an "initialising…"
	// placeholder while both are zero.
	width  int
	height int

	// popoverOpen is the narrow-mode popover state. Always false in wide
	// mode (the sidebar is always visible).
	popoverOpen bool
}

// Option is the functional-options shape used by New. Keeps the
// constructor stable as the provider surface area grows.
type Option func(*Model)

// WithStates wires a StateProvider.
func WithStates(p StateProvider) Option { return func(m *Model) { m.States = p } }

// WithHosts wires a HostProvider.
func WithHosts(p HostProvider) Option { return func(m *Model) { m.Hosts = p } }

// WithSize seeds an initial (width, height) before the first
// tea.WindowSizeMsg arrives — useful for tests that bypass the
// bubbletea program loop and call View() directly.
func WithSize(width, height int) Option {
	return func(m *Model) { m.width = width; m.height = height }
}

// WithRepoExpanded forces a repo's expanded state at construction. The
// renderer's default is "expanded", so this is mostly useful for tests
// that want to render a collapsed-repo fixture.
func WithRepoExpanded(repo string, expanded bool) Option {
	return func(m *Model) {
		if m.repoExp == nil {
			m.repoExp = make(map[string]bool)
		}
		m.repoExp[repo] = expanded
	}
}

// WithReviewsExpanded forces a session's review-group expanded state at
// construction. The renderer's default is "collapsed", so this is the
// hook for "spawn the UI with one review group already open" flows
// (most often used by tests).
func WithReviewsExpanded(sessionID string, expanded bool) Option {
	return func(m *Model) {
		if m.sessExp == nil {
			m.sessExp = make(map[string]bool)
		}
		m.sessExp[sessionID] = expanded
	}
}

// New constructs a Model around a *SessionTree. Tree must not be nil —
// construct one with pane.New() if you don't have a real one to hand.
//
// Initial selection is the first row of the flattened tree (typically
// the first repo header), matching the spike's behaviour. Callers
// wanting a specific initial selection should call MoveDown / select
// after construction.
func New(tree *pane.SessionTree, opts ...Option) *Model {
	m := &Model{
		Tree:    tree,
		repoExp: make(map[string]bool),
		sessExp: make(map[string]bool),
	}
	for _, o := range opts {
		o(m)
	}
	if m.repoExp == nil {
		m.repoExp = make(map[string]bool)
	}
	if m.sessExp == nil {
		m.sessExp = make(map[string]bool)
	}
	return m
}

// repoExpanded reports whether the named repo is currently expanded. The
// default is true — §3.1's tree examples show repos open.
func (m *Model) repoExpanded(repo string) bool {
	v, ok := m.repoExp[repo]
	if !ok {
		return true
	}
	return v
}

// reviewsExpanded reports whether the named session's review subsessions
// are currently shown. The default is false per §3.1 — reviews are
// collapsed by default; the `(N rev)` badge advertises the count while
// the user has not expanded them.
func (m *Model) reviewsExpanded(sessionID string) bool {
	v, ok := m.sessExp[sessionID]
	if !ok {
		return false
	}
	return v
}

// selectedRow returns the Row currently under the cursor and true, or
// the zero Row and false if the tree is empty / the cursor is out of
// range.
func (m *Model) selectedRow() (Row, bool) {
	rows := m.flatten()
	if len(rows) == 0 {
		return Row{}, false
	}
	if m.cursor < 0 || m.cursor >= len(rows) {
		return Row{}, false
	}
	return rows[m.cursor], true
}

// SelectedSessionID returns the session ID the cursor currently
// identifies, resolving up a level if the cursor is on a repo header
// (matches the spike's SelectedSession behaviour). Returns the empty
// string if no session is resolvable (empty tree, or repo with no
// sessions).
func (m *Model) SelectedSessionID() string {
	row, ok := m.selectedRow()
	if !ok || m.Tree == nil {
		return ""
	}
	switch row.Kind {
	case RowRepo:
		ids := m.Tree.RepoSessions(row.Repo)
		if len(ids) == 0 {
			return ""
		}
		return ids[0]
	case RowSession, RowReview:
		return row.SessionID
	}
	return ""
}

// CursorRow returns the current cursor index — useful for tests that
// want to assert navigation without relying on view output.
func (m *Model) CursorRow() int { return m.cursor }

// IsNarrow reports whether the current terminal width should use the
// mobile-shape layout per §3.1 (< 80 cols).
func (m *Model) IsNarrow() bool {
	return m.width > 0 && m.width < NarrowWidthThreshold
}

// PopoverOpen reports whether the narrow-mode popover is currently
// visible. Always false in wide mode.
func (m *Model) PopoverOpen() bool { return m.popoverOpen }

// Init is part of tea.Model — no startup work needed.
func (m *Model) Init() tea.Cmd { return nil }

// Update applies a tea.Msg to the model. Returns m as tea.Model so
// callers can use this both as a top-level program model and as a
// component.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Close the popover if the terminal grew back into wide
		// territory — leaving it open would be confusing once the
		// inline sidebar is visible again.
		if !m.IsNarrow() {
			m.popoverOpen = false
		}
		return m, nil
	}
	return m, nil
}

// handleKey is the §3.1 canonical keyboard surface, implemented
// verbatim:
//
//	↑ / k          MoveUp
//	↓ / j          MoveDown
//	← / h          collapse / walk outward
//	→ / l          expand / step inward
//	Enter          select session; in narrow mode also dismiss popover
//	Tab / Shift+T  cycle the selected session's active pane
//	Ctrl-B         toggle popover (narrow only; no-op in wide mode)
//	Esc            dismiss popover (narrow only)
//	q / Ctrl-C     quit
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "ctrl+b":
		if m.IsNarrow() {
			m.popoverOpen = !m.popoverOpen
		}
		return m, nil
	case "esc":
		// Narrow-mode dismiss. Wide mode has no popover, so esc is inert
		// — matches the §3.1 table ("narrow mode only").
		if m.popoverOpen {
			m.popoverOpen = false
		}
		return m, nil
	case "up", "k":
		m.moveUp()
	case "down", "j":
		m.moveDown()
	case "left", "h":
		m.moveLeft()
	case "right", "l":
		m.moveRight()
	case "enter":
		// Enter on a session row commits the selection; in narrow mode
		// it also dismisses the popover per §3.1 ("Enter selects and
		// dismisses").
		m.activateSelection()
		if m.popoverOpen {
			m.popoverOpen = false
		}
	case "tab":
		m.cycleNextPane()
	case "shift+tab":
		m.cyclePrevPane()
	}
	return m, nil
}

func (m *Model) moveUp() {
	if m.cursor > 0 {
		m.cursor--
	}
}

func (m *Model) moveDown() {
	rows := m.flatten()
	if m.cursor < len(rows)-1 {
		m.cursor++
	}
}

// moveLeft implements the §3.1 "collapse-expand-then-walk" rule going
// outward:
//
//   - On an expanded repo header: collapse the repo.
//   - On a session row with reviews expanded: collapse the review group.
//   - Anywhere else: walk back up to the row's repo header.
func (m *Model) moveLeft() {
	row, ok := m.selectedRow()
	if !ok {
		return
	}
	switch row.Kind {
	case RowRepo:
		if m.repoExpanded(row.Repo) {
			m.repoExp[row.Repo] = false
			return
		}
	case RowSession:
		if m.reviewsExpanded(row.SessionID) {
			m.sessExp[row.SessionID] = false
			return
		}
	}
	// Walk back to the row's repo header.
	for m.cursor > 0 {
		m.cursor--
		r, _ := m.selectedRow()
		if r.Kind == RowRepo {
			return
		}
	}
}

// moveRight implements the §3.1 collapse-expand-then-walk rule going
// inward:
//
//   - On a collapsed repo header: expand the repo.
//   - On an expanded repo header: step into the first child session.
//   - On a session row with reviews collapsed: expand the reviews.
//   - On a session row with reviews expanded: step into the first
//     review.
func (m *Model) moveRight() {
	row, ok := m.selectedRow()
	if !ok {
		return
	}
	switch row.Kind {
	case RowRepo:
		if !m.repoExpanded(row.Repo) {
			m.repoExp[row.Repo] = true
			return
		}
		if len(m.Tree.RepoSessions(row.Repo)) > 0 {
			m.moveDown()
		}
	case RowSession:
		children := m.Tree.Children(row.SessionID)
		if len(children) == 0 {
			return
		}
		if !m.reviewsExpanded(row.SessionID) {
			m.sessExp[row.SessionID] = true
			return
		}
		m.moveDown()
	}
}

// activateSelection sets the tree's ActiveSession to the currently
// selected session. No-op if the cursor doesn't resolve to a session
// (empty tree, repo with no sessions, etc.). Errors are swallowed —
// ActivateSession only fails on lookup mismatches we've already filtered
// out by walking through Tree accessors.
func (m *Model) activateSelection() {
	id := m.SelectedSessionID()
	if id == "" {
		return
	}
	_ = m.Tree.ActivateSession(id)
}

// cycleNextPane / cyclePrevPane cycle the selected session's active
// pane — the "inner ring" the §3.1 keyboard table calls out under Tab /
// Shift-Tab.
func (m *Model) cycleNextPane() {
	id := m.SelectedSessionID()
	if id == "" {
		return
	}
	_, _ = m.Tree.NextPane(id)
}

func (m *Model) cyclePrevPane() {
	id := m.SelectedSessionID()
	if id == "" {
		return
	}
	_, _ = m.Tree.PrevPane(id)
}

// View renders the current model to a string. Returns "initialising…"
// until the first tea.WindowSizeMsg has been received — bubbletea
// programs receive this synchronously at startup, so this state lasts
// at most one tick.
func (m *Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "initialising…"
	}
	if m.IsNarrow() {
		return m.viewNarrow()
	}
	return m.viewWide()
}

// viewWide composes the §3.1 split layout: sidebar at SidebarWidth on
// the left, active pane filling the rest. Joined via
// lipgloss.JoinHorizontal so the two halves align row-for-row.
func (m *Model) viewWide() string {
	left := m.renderSidebar(SidebarWidth, m.height)
	rightWidth := m.width - SidebarWidth
	if rightWidth < 1 {
		rightWidth = 1
	}
	right := m.renderActivePane(rightWidth, m.height)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// viewNarrow renders the §3.1 mobile-shape layout: 1-row topbar,
// full-width pane underneath, and (when popoverOpen) a sidebar overlay
// composited on top.
func (m *Model) viewNarrow() string {
	topbar := m.renderTopbar(m.width)
	paneHeight := m.height - 1
	if paneHeight < 1 {
		paneHeight = 1
	}
	pane := m.renderActivePane(m.width, paneHeight)
	base := lipgloss.JoinVertical(lipgloss.Left, topbar, pane)

	if !m.popoverOpen {
		return base
	}

	// Popover width is sidebar.Width + 6, capped at terminal_width - 4
	// (§3.1). Capped width never drops below SidebarWidth — the chrome
	// itself wouldn't fit.
	popoverW := SidebarWidth + NarrowPopoverExtra
	if popoverW > m.width-NarrowPopoverInset {
		popoverW = m.width - NarrowPopoverInset
	}
	if popoverW < SidebarWidth {
		popoverW = SidebarWidth
	}
	popoverH := m.height - 2
	if popoverH < 4 {
		popoverH = 4
	}
	overlay := m.renderSidebar(popoverW, popoverH)
	// Top-left anchor with one-row top offset so the topbar stays
	// visible above the overlay. lipgloss doesn't ship a Z-index
	// composer, so we splice the overlay into the base string ourselves.
	return overlayAt(base, overlay, 1, 0)
}
