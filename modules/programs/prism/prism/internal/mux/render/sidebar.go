package render

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// RowKind discriminates the three kinds of rows the sidebar can render.
// Repo headers participate in selection so `←` / `→` can collapse and
// expand the cluster; session and review subsession rows participate so
// `Enter` selects the active pane host (§3.1 keyboard surface).
type RowKind int

const (
	// RowRepo is a repo cluster header — `▾ nixos-config` / `▸ home-ops`.
	RowRepo RowKind = iota
	// RowSession is a top-level session row — `├─ ○  @main  idle`.
	RowSession
	// RowReview is a review subsession row — `│  ├─ ●  ~1-code  active`.
	RowReview
)

// Row identifies one rendered row in the flattened tree. The fields
// double as a content address: (Repo) for RowRepo, (Repo, SessionID) for
// RowSession, and (Repo, SessionID) where SessionID is the review
// subsession's own ID for RowReview.
//
// The parent of a review row is recoverable from pane.Session.ParentID;
// the renderer reads it through the SessionTree when it needs to format
// the trunk character.
type Row struct {
	Kind      RowKind
	Repo      string
	SessionID string
}

// flatten walks the tree honouring repoExpanded / sessionExpanded. The
// repo-expanded default is true (repos open) and the session-reviews
// default is false (reviews collapsed) per §3.1: "Review subsessions are
// collapsed by default".
func (m *Model) flatten() []Row {
	if m.Tree == nil {
		return nil
	}
	var rows []Row
	for _, repo := range m.Tree.Repos() {
		rows = append(rows, Row{Kind: RowRepo, Repo: repo})
		if !m.repoExpanded(repo) {
			continue
		}
		for _, sid := range m.Tree.RepoSessions(repo) {
			rows = append(rows, Row{Kind: RowSession, Repo: repo, SessionID: sid})
			if !m.reviewsExpanded(sid) {
				continue
			}
			for _, cid := range m.Tree.Children(sid) {
				rows = append(rows, Row{Kind: RowReview, Repo: repo, SessionID: cid})
			}
		}
	}
	return rows
}

// countTopLevelSessions returns the value shown in the §3.1 header:
// "prism · N sessions" where N counts non-review sessions across all
// repos. Review subsessions are deliberately excluded so the number does
// not shift when the user expands or collapses a review group.
func (m *Model) countTopLevelSessions() int {
	if m.Tree == nil {
		return 0
	}
	n := 0
	for _, repo := range m.Tree.Repos() {
		n += len(m.Tree.RepoSessions(repo))
	}
	return n
}

// renderSidebar renders the sidebar at (width, height). The composition
// is header / divider / scrollable body / footer, joined by
// lipgloss.JoinVertical so the chrome cannot be pushed off the top by
// overflowing content. Body is auto-scrolled to keep the cursor in view.
//
// The width argument is honoured so the narrow-mode popover can render a
// variant at SidebarWidth + 6. The right border lives outside this width;
// callers wanting a borderless variant should subtract one column from
// the requested width before calling.
func (m *Model) renderSidebar(width, height int) string {
	if width <= 0 {
		width = SidebarWidth
	}
	if height <= 0 {
		height = 1
	}
	// The frame renders a one-column right border, so the content body
	// occupies width-1 cells. Anything wider would clip into the border.
	contentWidth := width - 1
	if contentWidth < 1 {
		contentWidth = 1
	}

	rows := m.flatten()

	// Header — pinned, never scrolled. One-cell left margin so the text
	// doesn't sit flush against the frame border.
	headerText := fmt.Sprintf(" prism · %d sessions", m.countTopLevelSessions())
	header := headerStyle().Render(truncateOrPad(headerText, contentWidth))
	divider := dividerStyle().Render(strings.Repeat("─", contentWidth))

	// Footer — pinned, never scrolled. Glyph-only truncation when the
	// sidebar is too narrow for the full hint, per §3.1.
	footerText := " ↑↓ nav  ←→ collapse  ⏎ select  ⇥ pane  q quit"
	if ansi.StringWidth(footerText) > contentWidth {
		footerText = " ↑↓ ←→ ⏎ ⇥ q"
	}
	footer := footerStyle().Render(truncateOrPad(footerText, contentWidth))

	// Body height: reserve 3 rows for chrome (header, divider, footer).
	// If the host gives us less than 4 rows total, the body collapses to
	// a single row and the frame clips — better than panicking.
	bodyHeight := height - 3
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	// Auto-scroll: keep the cursor inside the visible window. The first
	// frame after a window-size change can have a cursor past the end of
	// the rows slice; clamp here so the View doesn't panic.
	cursor := m.cursor
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(rows) {
		cursor = len(rows) - 1
	}
	scroll := 0
	if cursor >= bodyHeight {
		scroll = cursor - bodyHeight + 1
	}
	end := scroll + bodyHeight
	if end > len(rows) {
		end = len(rows)
	}

	var lines []string
	for i := scroll; i < end; i++ {
		lines = append(lines, m.renderRow(rows[i], i == cursor, contentWidth))
	}
	// Pad the body to bodyHeight so the footer is flush at the bottom
	// regardless of how many rows are visible.
	for len(lines) < bodyHeight {
		lines = append(lines, strings.Repeat(" ", contentWidth))
	}
	body := strings.Join(lines, "\n")

	composed := lipgloss.JoinVertical(lipgloss.Left, header, divider, body, footer)
	// frameStyle's Width is the INNER width (lipgloss adds the border on
	// top), so passing contentWidth here produces total visible width of
	// contentWidth + 1 border column = the caller's requested width.
	return frameStyle().Width(contentWidth).Height(height).Render(composed)
}

// renderRow renders a single row hard-truncated to contentWidth using
// the ANSI-aware truncator. Switches on Kind so each row type can pick up
// its own prefix and badge logic.
func (m *Model) renderRow(row Row, selected bool, contentWidth int) string {
	switch row.Kind {
	case RowRepo:
		return m.renderRepoRow(row, selected, contentWidth)
	case RowSession:
		return m.renderSessionRow(row, selected, contentWidth)
	case RowReview:
		return m.renderReviewRow(row, selected, contentWidth)
	}
	return strings.Repeat(" ", contentWidth)
}

func (m *Model) renderRepoRow(row Row, selected bool, contentWidth int) string {
	arrow := "▸"
	if m.repoExpanded(row.Repo) {
		arrow = "▾"
	}
	text := fmt.Sprintf(" %s %s", arrow, row.Repo)
	return repoHeaderStyle(selected).Render(truncateOrPad(text, contentWidth))
}

func (m *Model) renderSessionRow(row Row, selected bool, contentWidth int) string {
	sess, ok := m.Tree.Session(row.SessionID)
	if !ok {
		return strings.Repeat(" ", contentWidth)
	}
	state := m.sessionState(row.SessionID)

	// Tree prefix: `├─` for mid-children, `└─` for the trailing one.
	ids := m.Tree.RepoSessions(row.Repo)
	isLast := len(ids) > 0 && ids[len(ids)-1] == row.SessionID
	prefix := " ├─ "
	if isLast {
		prefix = " └─ "
	}

	glyph := glyphStyle(state).Render(visualFor(state).glyph)
	displayName := sessionDisplayName(sess)
	name := nameStyle(state, selected).Render(displayName)
	left := fmt.Sprintf("%s%s %s", prefix, glyph, name)

	// `(N rev)` review badge — only when the row's reviews are collapsed
	// AND the session has reviews. The badge disappears when expanded
	// (§3.1).
	badge := ""
	children := m.Tree.Children(row.SessionID)
	if !m.reviewsExpanded(row.SessionID) && len(children) > 0 {
		badge = dimStyle().Render(fmt.Sprintf(" (%d rev)", len(children)))
	}

	stateLabel := dimStyle().Render(state.String())
	return composeRowWithRightLabel(left, badge, stateLabel, contentWidth)
}

func (m *Model) renderReviewRow(row Row, selected bool, contentWidth int) string {
	sub, ok := m.Tree.Session(row.SessionID)
	if !ok {
		return strings.Repeat(" ", contentWidth)
	}
	state := m.sessionState(row.SessionID)

	// Trunk character: `│` while the parent session is not the trailing
	// one in the cluster, four spaces (the quieter look) when it is.
	parentIDs := m.Tree.RepoSessions(row.Repo)
	parentIsLast := len(parentIDs) > 0 && parentIDs[len(parentIDs)-1] == sub.ParentID
	trunk := " │  "
	if parentIsLast {
		trunk = "    "
	}

	// Branch character: `├─` for mid-children, `└─` for the trailing
	// review.
	siblings := m.Tree.Children(sub.ParentID)
	isLastSub := len(siblings) > 0 && siblings[len(siblings)-1] == row.SessionID
	branch := "├─ "
	if isLastSub {
		branch = "└─ "
	}

	glyph := glyphStyle(state).Render(visualFor(state).glyph)
	displayName := reviewDisplayName(sub.ID, sub.ParentID)
	name := nameStyle(state, selected).Render(displayName)
	left := fmt.Sprintf("%s%s%s %s", trunk, branch, glyph, name)

	stateLabel := dimStyle().Render(state.String())
	return composeRowWithRightLabel(left, "", stateLabel, contentWidth)
}

// sessionState resolves the State for a session via the configured
// StateProvider, falling back to StateIdle when no provider is wired.
func (m *Model) sessionState(id string) State {
	if m.States == nil {
		return StateIdle
	}
	return m.States.State(id)
}

// sessionDisplayName extracts the user-visible suffix from a top-level
// session ID. Prism's naming convention is "<repo>@<branch>" (per the
// pane package's docstring), so the @-and-after portion is what reads on
// the sidebar; the repo prefix is already implied by the cluster header.
// IDs without an `@` pass through unchanged.
func sessionDisplayName(s pane.Session) string {
	if i := strings.IndexByte(s.ID, '@'); i >= 0 {
		return s.ID[i:]
	}
	return s.ID
}

// reviewNameRE matches `~review-<cycle>-<agent>` and captures the cycle
// number and the agent component. Anything that doesn't match passes
// through unchanged.
var reviewNameRE = regexp.MustCompile(`^~review-(\d+)-(.+)$`)

// reviewDisplayName rewrites `<parent>~review-N-<agent>` as `~N-<agent>`
// per §3.1: "Review subsession names are rewritten at render time: the
// real name `~review-N-<agent>` (as it appears in
// `agent_status.session_name`) renders as `~N-<agent>` inside the
// parent's tree context. The redundant `review-` doubling is stripped".
func reviewDisplayName(id, parentID string) string {
	suffix := strings.TrimPrefix(id, parentID)
	matches := reviewNameRE.FindStringSubmatch(suffix)
	if matches == nil {
		return suffix
	}
	// Strip a leading `review-` from the agent component too, when
	// present: `review-code` → `code`. Eliminates the double-review.
	agent := strings.TrimPrefix(matches[2], "review-")
	return fmt.Sprintf("~%s-%s", matches[1], agent)
}

// composeRowWithRightLabel lays out a row with an optional inline badge
// and an optional right-aligned label, applying the §3.1 drop order when
// space is tight: state label first, badge second, then ellipsis-truncate
// the left content.
func composeRowWithRightLabel(left, badge, label string, width int) string {
	leftW := ansi.StringWidth(left)
	badgeW := ansi.StringWidth(badge)
	labelW := ansi.StringWidth(label)

	// Best case: left + badge + at-least-one-space-gap + label all fit.
	if leftW+badgeW+1+labelW <= width {
		gap := width - leftW - badgeW - labelW
		return left + badge + strings.Repeat(" ", gap) + label
	}
	// Drop the state label, keep the badge.
	if leftW+badgeW <= width {
		gap := width - leftW - badgeW
		return left + badge + strings.Repeat(" ", gap)
	}
	// Drop the badge too, ellipsis-truncate the left content.
	return ansi.Truncate(left, width, "…")
}

// truncateOrPad is the workhorse for header / footer / repo-header rows:
// pads with spaces when the input is too short, ellipsis-truncates when
// it is too long. ANSI-aware via x/ansi so lipgloss-coloured strings
// truncate at display-cell boundaries, not byte boundaries.
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
