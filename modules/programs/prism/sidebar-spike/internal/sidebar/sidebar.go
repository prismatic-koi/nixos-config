package sidebar

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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

// Flatten walks the tree honouring Repo.Expanded and
// Session.ExpandedReviews. Review subsessions are hidden by default
// (mirroring `prism sessions list` without `--all`); the parent
// session row carries a `(N reviews)` badge when collapsed.
func Flatten(t *model.Tree) []Row {
	var rows []Row
	for ri, repo := range t.Repos {
		rows = append(rows, Row{Kind: RowRepo, RepoIdx: ri, SessionIdx: -1, SubIdx: -1})
		if !repo.Expanded {
			continue
		}
		for si, sess := range repo.Sessions {
			rows = append(rows, Row{Kind: RowSession, RepoIdx: ri, SessionIdx: si, SubIdx: -1})
			if !sess.ExpandedReviews {
				continue
			}
			for sui := range sess.Subsessions {
				rows = append(rows, Row{Kind: RowSubsession, RepoIdx: ri, SessionIdx: si, SubIdx: sui})
			}
		}
	}
	return rows
}

// CountSessions returns the number of non-review sessions across all
// repos in the tree. This is the value shown in the header — review
// subsessions are deliberately excluded so the number doesn't shift
// when the user expands/collapses a review group (see v2 notes in
// design-notes.md).
func CountSessions(t *model.Tree) int {
	n := 0
	for _, repo := range t.Repos {
		n += len(repo.Sessions)
	}
	return n
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

// SelectedRepo returns the Repo containing the cursor's current row,
// or nil if the tree is empty.
func (m Model) SelectedRepo() *model.Repo {
	row, ok := m.Selected()
	if !ok {
		return nil
	}
	return m.Tree.Repos[row.RepoIdx]
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

// MoveLeft has three behaviours depending on cursor position:
//
//   - Repo header, expanded: collapse the repo.
//   - Session row, reviews expanded: collapse the review group.
//   - Anywhere else: walk back up to the row's repo header.
//
// This mirrors the file-explorer collapse pattern: ← always "goes
// outward" toward the root.
func (m *Model) MoveLeft() {
	row, ok := m.Selected()
	if !ok {
		return
	}
	repo := m.Tree.Repos[row.RepoIdx]
	switch row.Kind {
	case RowRepo:
		if repo.Expanded {
			repo.Expanded = false
			return
		}
	case RowSession:
		sess := repo.Sessions[row.SessionIdx]
		if sess.ExpandedReviews {
			sess.ExpandedReviews = false
			return
		}
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

// MoveRight has three behaviours, symmetric with MoveLeft:
//
//   - Repo header, collapsed: expand the repo.
//   - Repo header, expanded: step into the first child session.
//   - Session row with reviews, collapsed: expand the review group.
//   - Session row with reviews, expanded: step into the first review.
func (m *Model) MoveRight() {
	row, ok := m.Selected()
	if !ok {
		return
	}
	repo := m.Tree.Repos[row.RepoIdx]
	switch row.Kind {
	case RowRepo:
		if !repo.Expanded {
			repo.Expanded = true
			return
		}
		if len(repo.Sessions) > 0 {
			m.MoveDown()
		}
	case RowSession:
		sess := repo.Sessions[row.SessionIdx]
		if len(sess.Subsessions) == 0 {
			return
		}
		if !sess.ExpandedReviews {
			sess.ExpandedReviews = true
			return
		}
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

// View renders the sidebar at the supplied (width, height). v2 fixes
// the v1 header-disappears bug by composing the three components
// (header, scrollable body, footer) with lipgloss.JoinVertical and
// allocating each a fixed height. The body gets the remainder; any
// rows that don't fit are clipped from the bottom and the cursor's
// row is auto-scrolled into view.
//
// width defaults to Width when zero; values smaller than Width are
// respected so the popover host can render a narrow variant.
func (m Model) View(width, height int) string {
	if width <= 0 {
		width = Width
	}
	contentWidth := width - 1 // reserve one column for the right border

	rows := Flatten(m.Tree)

	// Header: pinned, never scrolled. One-cell left margin so the
	// text doesn't sit flush against the frame border.
	headerText := fmt.Sprintf(" prism · %d sessions", CountSessions(m.Tree))
	header := headerStyle().Render(truncateOrPad(headerText, contentWidth))
	divider := dividerStyle().Render(strings.Repeat("─", contentWidth))

	// Footer: pinned, never scrolled. Same one-cell left margin.
	footerText := " ↑↓ nav  ←→ collapse  ⏎ select  ⇥ pane  q quit"
	if ansi.StringWidth(footerText) > contentWidth {
		footerText = " ↑↓ ←→ ⏎ ⇥ q"
	}
	footer := footerStyle().Render(truncateOrPad(footerText, contentWidth))

	// Reserve heights: header 1, divider 1, footer 1. Body gets the
	// rest. If height is too small to fit even the chrome, drop the
	// body and let the frame clip — better than panicking.
	bodyHeight := height - 3
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	// Auto-scroll: ensure the cursor's row is within [scroll,
	// scroll+bodyHeight). v1 had no scrolling, which is what made
	// long trees fall off the bottom.
	scroll := 0
	if m.Cursor >= bodyHeight {
		scroll = m.Cursor - bodyHeight + 1
	}
	end := scroll + bodyHeight
	if end > len(rows) {
		end = len(rows)
	}

	var lines []string
	for i := scroll; i < end; i++ {
		selected := i == m.Cursor
		lines = append(lines, renderRow(m.Tree, rows[i], selected, contentWidth))
	}
	// Pad the body to exactly bodyHeight rows so the footer always
	// sits flush at the bottom.
	for len(lines) < bodyHeight {
		lines = append(lines, strings.Repeat(" ", contentWidth))
	}
	body := strings.Join(lines, "\n")

	composed := lipgloss.JoinVertical(lipgloss.Left, header, divider, body, footer)
	return frameStyle().Width(width).Height(height).Render(composed)
}

// renderRow renders a single row, hard-truncated to width. The three
// shapes:
//
//   - RowRepo:     "▾ nixos-config"
//   - RowSession:  " ├─ ●  @main      active"
//   - RowSubsession: " │  ├─ ⊙ ~1-code"
//
// Session rows whose Subsessions are collapsed carry a trailing
// "(N reviews)" badge.
func renderRow(t *model.Tree, row Row, selected bool, width int) string {
	repo := t.Repos[row.RepoIdx]
	switch row.Kind {
	case RowRepo:
		arrow := "▸"
		if repo.Expanded {
			arrow = "▾"
		}
		text := fmt.Sprintf(" %s %s", arrow, repo.Name)
		return repoHeaderStyle(selected).Render(truncateOrPad(text, width))

	case RowSession:
		sess := repo.Sessions[row.SessionIdx]
		isLast := row.SessionIdx == len(repo.Sessions)-1
		prefix := " ├─ "
		if isLast {
			prefix = " └─ "
		}
		return renderSessionRow(prefix, sess, selected, width)

	case RowSubsession:
		sess := repo.Sessions[row.SessionIdx]
		sub := sess.Subsessions[row.SubIdx]
		parentIsLast := row.SessionIdx == len(repo.Sessions)-1
		// When the parent session is the last child of the repo, drop
		// the trunk to match the quieter look on trailing clusters.
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
		return renderSubsessionRow(prefix, sub, selected, width)
	}
	return strings.Repeat(" ", width)
}

// renderSessionRow renders a session row: prefix + glyph + name +
// optional review-count badge + (when there's room) state label.
// Hard-truncated to width to fix the v1 wrap-bleed bug.
func renderSessionRow(prefix string, s *model.Session, selected bool, width int) string {
	v := stateVisuals[s.State]
	glyph := glyphStyle(s.State).Render(v.glyph)
	name := nameStyle(s.State, selected).Render(s.Name)

	left := fmt.Sprintf("%s%s %s", prefix, glyph, name)

	// Badge: only when reviews are collapsed and there are any.
	badge := ""
	if !s.ExpandedReviews && len(s.Subsessions) > 0 {
		badge = dimStyle().Render(fmt.Sprintf(" (%d rev)", len(s.Subsessions)))
	}

	// State label is rendered to the right when there's room, after
	// the badge. Drop it before dropping the name.
	stateLabel := dimStyle().Render(s.State.String())
	return composeRowWithRightLabel(left, badge, stateLabel, width)
}

// renderSubsessionRow is the same shape as renderSessionRow but
// renders a shortened name (Issue 1a: drop the redundant
// `review-N-review-` prefix).
func renderSubsessionRow(prefix string, s *model.Session, selected bool, width int) string {
	v := stateVisuals[s.State]
	glyph := glyphStyle(s.State).Render(v.glyph)
	short := shortReviewName(s.Name)
	name := nameStyle(s.State, selected).Render(short)

	left := fmt.Sprintf("%s%s %s", prefix, glyph, name)
	stateLabel := dimStyle().Render(s.State.String())
	return composeRowWithRightLabel(left, "", stateLabel, width)
}

// Topbar renders the narrow-mode single-row identity strip. Exposed
// here so the visual vocabulary (topbarStyle / topbarHintStyle)
// stays in this package. width is the total terminal width.
//
// Shape: "<repo>/<session> · <state>" left-aligned, with a
// dim trailing hint right-anchored.
func Topbar(m Model, width int, popoverOpen bool) string {
	repo := m.SelectedRepo()
	sess := m.SelectedSession()

	var identity string
	switch {
	case repo == nil:
		identity = "(no session)"
	case sess == nil:
		identity = repo.Name
	default:
		identity = fmt.Sprintf("%s/%s · %s", repo.Name, sess.Name, sess.State)
	}

	hint := "^B switch"
	if popoverOpen {
		hint = "esc close"
	}
	hint = " " + hint + " "

	identityW := width - ansi.StringWidth(hint)
	if identityW < 4 {
		identityW = 4
	}
	identityCell := topbarStyle().Render(truncateOrPad(" "+identity, identityW))
	hintCell := topbarHintStyle().Render(hint)
	return lipgloss.JoinHorizontal(lipgloss.Top, identityCell, hintCell)
}

// reviewNameRE matches `~review-<cycle>-<agent>` and captures the
// cycle number and the agent component. Anything that doesn't match
// is returned unchanged.
var reviewNameRE = regexp.MustCompile(`^~review-(\d+)-(.+)$`)

// shortReviewName rewrites `~review-N-<agent>` as `~N-<agent>`.
// Real prism review subsession names follow this convention exactly
// (see e.g. `prism sessions list` output for any active review
// group), so the rewrite is unambiguous. Non-review names pass
// through unchanged.
func shortReviewName(full string) string {
	m := reviewNameRE.FindStringSubmatch(full)
	if m == nil {
		return full
	}
	// Strip a leading `review-` from the agent component too, when
	// present: `review-code` → `code`. Eliminates the double-review.
	agent := strings.TrimPrefix(m[2], "review-")
	return fmt.Sprintf("~%s-%s", m[1], agent)
}

// composeRowWithRightLabel takes a left-aligned content string, an
// optional inline-trailing badge, and an optional right-aligned label,
// and renders the row at exactly width display cells with the label
// dropped first when space is tight.
func composeRowWithRightLabel(left, badge, label string, width int) string {
	leftW := ansi.StringWidth(left)
	badgeW := ansi.StringWidth(badge)
	labelW := ansi.StringWidth(label)

	// Best case: left + badge + gap + label all fit.
	if leftW+badgeW+1+labelW <= width {
		gap := width - leftW - badgeW - labelW
		return left + badge + strings.Repeat(" ", gap) + label
	}
	// Drop label, keep badge.
	if leftW+badgeW <= width {
		gap := width - leftW - badgeW
		return left + badge + strings.Repeat(" ", gap)
	}
	// Drop badge too — truncate the left content with ellipsis.
	return ansi.Truncate(left, width, "…")
}

// truncateOrPad is the workhorse for header / footer / repo-header
// rows: ensures the result is exactly width display cells wide,
// truncating with `…` when the input is too long and padding with
// spaces when it is too short. ANSI-aware via x/ansi.
func truncateOrPad(s string, width int) string {
	w := ansi.StringWidth(s)
	switch {
	case w == width:
		return s
	case w < width:
		return s + strings.Repeat(" ", width-w)
	default:
		return ansi.Truncate(s, width, "…")
	}
}
