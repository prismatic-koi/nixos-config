package sidebar

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/sidebar-spike/internal/model"
)

// RowKind discriminates the three kinds of rows the sidebar can
// render. Repo headers participate in selection so left/right can
// collapse/expand the cluster; session and subsession rows participate
// so Enter selects the active pane host.
type RowKind int

const (
	RowRepo RowKind = iota
	RowSession
	RowSubsession
)

// Row is one rendered row in the flattened tree. The RepoIdx /
// SessionIdx / SubIdx fields address back into the tree so the
// selection cursor can identify what is currently focused without
// holding a stale pointer.
type Row struct {
	Kind       RowKind
	RepoIdx    int
	SessionIdx int
	SubIdx     int
}

// Flatten walks the tree honouring Repo.Expanded and returns the
// visible rows in render order. Returned indices are stable across
// state-only mutations of the tree (transitions don't reshape it).
func Flatten(t *model.Tree) []Row {
	var rows []Row
	for ri, repo := range t.Repos {
		rows = append(rows, Row{Kind: RowRepo, RepoIdx: ri, SessionIdx: -1, SubIdx: -1})
		if !repo.Expanded {
			continue
		}
		for si, sess := range repo.Sessions {
			rows = append(rows, Row{Kind: RowSession, RepoIdx: ri, SessionIdx: si, SubIdx: -1})
			for sui := range sess.Subsessions {
				rows = append(rows, Row{Kind: RowSubsession, RepoIdx: ri, SessionIdx: si, SubIdx: sui})
			}
		}
	}
	return rows
}

// Model is the bubbletea-friendly state carried by the sidebar
// component. It is a passive renderer — the parent program owns the
// tree and the cursor; the sidebar just turns them into a string.
type Model struct {
	Tree   *model.Tree
	Cursor int // index into Flatten(Tree)
}

// Selected returns the Row currently under the cursor, or a zero Row
// if the tree is empty.
func (m Model) Selected() (Row, bool) {
	rows := Flatten(m.Tree)
	if len(rows) == 0 || m.Cursor < 0 || m.Cursor >= len(rows) {
		return Row{}, false
	}
	return rows[m.Cursor], true
}

// SelectedSession returns the Session the cursor currently identifies,
// resolving up a level if the cursor is on a repo header. Returns nil
// if no session is resolvable (empty tree, or repo with no sessions).
//
// Used by the parent program to drive the right-pane placeholder.
func (m Model) SelectedSession() *model.Session {
	row, ok := m.Selected()
	if !ok {
		return nil
	}
	repo := m.Tree.Repos[row.RepoIdx]
	switch row.Kind {
	case RowRepo:
		if len(repo.Sessions) == 0 {
			return nil
		}
		return repo.Sessions[0]
	case RowSession:
		return repo.Sessions[row.SessionIdx]
	case RowSubsession:
		return repo.Sessions[row.SessionIdx].Subsessions[row.SubIdx]
	}
	return nil
}

// MoveUp / MoveDown / MoveLeft / MoveRight are the navigation API the
// parent program calls in response to arrow keys.

func (m *Model) MoveUp() {
	if m.Cursor > 0 {
		m.Cursor--
	}
}

func (m *Model) MoveDown() {
	if rows := Flatten(m.Tree); m.Cursor < len(rows)-1 {
		m.Cursor++
	}
}

// MoveLeft collapses the repo group the cursor is on (if it's a repo
// header) or moves the cursor up to its repo header otherwise.
// Mirrors the tree-collapse pattern from file explorers.
func (m *Model) MoveLeft() {
	row, ok := m.Selected()
	if !ok {
		return
	}
	repo := m.Tree.Repos[row.RepoIdx]
	if row.Kind == RowRepo && repo.Expanded {
		repo.Expanded = false
		// Cursor stays on the repo header; flatten will be shorter.
		return
	}
	// Walk back to the repo header.
	for m.Cursor > 0 {
		m.Cursor--
		r, _ := m.Selected()
		if r.Kind == RowRepo {
			return
		}
	}
}

// MoveRight expands the repo group the cursor is on, or descends into
// the first child if already expanded.
func (m *Model) MoveRight() {
	row, ok := m.Selected()
	if !ok {
		return
	}
	if row.Kind != RowRepo {
		// Already inside a repo — descend one level if there's a child.
		if row.Kind == RowSession {
			sess := m.Tree.Repos[row.RepoIdx].Sessions[row.SessionIdx]
			if len(sess.Subsessions) > 0 {
				m.MoveDown()
			}
		}
		return
	}
	repo := m.Tree.Repos[row.RepoIdx]
	if !repo.Expanded {
		repo.Expanded = true
		return
	}
	// Already expanded — step into the first session.
	if len(repo.Sessions) > 0 {
		m.MoveDown()
	}
}

// CycleNextPane / CyclePrevPane rotate the active pane on the
// currently-selected session.
func (m *Model) CycleNextPane() {
	s := m.SelectedSession()
	if s == nil || len(s.Panes) == 0 {
		return
	}
	s.ActivePane = (s.ActivePane + 1) % len(s.Panes)
}

func (m *Model) CyclePrevPane() {
	s := m.SelectedSession()
	if s == nil || len(s.Panes) == 0 {
		return
	}
	s.ActivePane = (s.ActivePane - 1 + len(s.Panes)) % len(s.Panes)
}

// View renders the sidebar to a string of fixed width. The parent
// program composes this with the right pane via lipgloss horizontal
// joining.
func (m Model) View(height int) string {
	rows := Flatten(m.Tree)

	var b strings.Builder

	// Header: short identity + visible session count. The visible
	// session count is a useful at-a-glance metric the herdr layout
	// inspired (workspace numbering) — adapted to prism's data shape.
	visibleSessions := 0
	for _, r := range rows {
		if r.Kind == RowSession || r.Kind == RowSubsession {
			visibleSessions++
		}
	}
	header := fmt.Sprintf("prism · %d sessions", visibleSessions)
	b.WriteString(headerStyle().Render(header))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", Width-1))
	b.WriteString("\n")

	for i, row := range rows {
		selected := i == m.Cursor
		b.WriteString(renderRow(m.Tree, row, selected))
		b.WriteString("\n")
	}

	// Footer keymap hint.
	footer := "↑↓ nav  ←→ collapse  ⏎ select  ⇥ pane  q quit"
	// Truncate to width.
	if lipgloss.Width(footer) > Width-2 {
		footer = "↑↓ ←→ ⏎ ⇥ q"
	}

	body := b.String()
	// Pad to (height - 2) lines so the footer sits at the bottom.
	bodyHeight := strings.Count(body, "\n")
	pad := height - bodyHeight - 2
	if pad > 0 {
		body += strings.Repeat("\n", pad)
	}
	body += footerStyle().Render(footer)

	return frameStyle().Height(height).Render(body)
}

// renderRow renders a single row to a width-fitted string. Three
// distinct shapes:
//
//   - RowRepo:     "▾ nixos-config" (▾ when expanded, ▸ when collapsed)
//   - RowSession:  " ├─ ●  @main      active"
//   - RowSubsession: " │  ├─ ⊙ ~review-1-review-code"
func renderRow(t *model.Tree, row Row, selected bool) string {
	repo := t.Repos[row.RepoIdx]
	switch row.Kind {
	case RowRepo:
		var arrow string
		if repo.Expanded {
			arrow = "▾"
		} else {
			arrow = "▸"
		}
		text := fmt.Sprintf(" %s %s", arrow, repo.Name)
		text = padOrTruncate(text, Width-1)
		return repoHeaderStyle(selected).Render(text)

	case RowSession:
		sess := repo.Sessions[row.SessionIdx]
		isLast := row.SessionIdx == len(repo.Sessions)-1 &&
			len(sess.Subsessions) == 0
		prefix := " ├─ "
		if isLast {
			prefix = " └─ "
		}
		return renderSessionRow(prefix, sess, selected)

	case RowSubsession:
		sess := repo.Sessions[row.SessionIdx]
		sub := sess.Subsessions[row.SubIdx]
		// Branch line continues if there's a sibling session after the
		// parent (rare in practice — review subsessions are usually
		// the trailing children of their parent).
		parentIsLast := row.SessionIdx == len(repo.Sessions)-1
		// When the parent session is the last child of the repo, drop
		// the trunk to match the quieter look on trailing clusters
		// (mirrors how herdr's sidebar reads).
		trunk := " │  "
		if parentIsLast {
			trunk = "    "
		}
		isLastSub := row.SubIdx == len(sess.Subsessions)-1
		branch := "├─ "
		if isLastSub {
			branch = "└─ "
		}
		prefix := trunk + branch
		return renderSessionRow(prefix, sub, selected)
	}
	return ""
}

// renderSessionRow renders a session/subsession row: prefix + glyph +
// name. The state label is rendered to the right when there's room.
func renderSessionRow(prefix string, s *model.Session, selected bool) string {
	v := stateVisuals[s.State]
	glyph := glyphStyle(s.State).Render(v.glyph)
	name := nameStyle(s.State, selected).Render(s.Name)

	// Compute remaining width to potentially right-align the state
	// label. Account for prefix width and glyph+space (2 cells) and
	// name width.
	left := fmt.Sprintf("%s%s %s", prefix, glyph, name)
	leftWidth := lipgloss.Width(left)
	if leftWidth >= Width-1 {
		return padOrTruncate(left, Width-1)
	}
	stateLabel := dimStyle().Render(s.State.String())
	stateWidth := lipgloss.Width(stateLabel)
	gap := Width - 1 - leftWidth - stateWidth
	if gap < 1 {
		// Not enough room for the label; drop it.
		return padOrTruncate(left, Width-1)
	}
	return left + strings.Repeat(" ", gap) + stateLabel
}

// padOrTruncate ensures the rendered string is exactly width display
// cells. Uses lipgloss.Width to handle ANSI-coloured strings correctly.
func padOrTruncate(s string, width int) string {
	w := lipgloss.Width(s)
	if w == width {
		return s
	}
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	// Truncation is tricky in the presence of ANSI; for v1 we just
	// fall back to the unchanged string if it overflows (the frame
	// style will clip it visually).
	return s
}
