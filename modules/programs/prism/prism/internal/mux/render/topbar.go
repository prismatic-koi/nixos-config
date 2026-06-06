package render

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// renderTopbar renders the §3.1 narrow-mode single-row identity strip:
//
//	<repo>/<session> · <state>          ^B switch
//
// `^B switch` is replaced with `esc close` when the popover is open so
// the hint surface reflects the current foreground keystroke. Dim
// background per §3.1.
func (m *Model) renderTopbar(width int) string {
	identity := m.topbarIdentity()
	hint := "^B switch"
	if m.popoverOpen {
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

// topbarIdentity formats the `<repo>/<session> · <state>` left half of
// the topbar. Walks the current selection: a repo-header selection
// resolves down to the first child session (matches the wide sidebar's
// SelectedSession behaviour), an empty tree reports "(no session)".
func (m *Model) topbarIdentity() string {
	row, ok := m.selectedRow()
	if !ok {
		return "(no session)"
	}
	switch row.Kind {
	case RowRepo:
		ids := m.Tree.RepoSessions(row.Repo)
		if len(ids) == 0 {
			return row.Repo
		}
		sess, ok := m.Tree.Session(ids[0])
		if !ok {
			return row.Repo
		}
		state := m.sessionState(sess.ID)
		return fmt.Sprintf("%s/%s · %s", row.Repo, sessionDisplayName(sess), state)
	case RowSession:
		sess, ok := m.Tree.Session(row.SessionID)
		if !ok {
			return row.Repo
		}
		state := m.sessionState(sess.ID)
		return fmt.Sprintf("%s/%s · %s", row.Repo, sessionDisplayName(sess), state)
	case RowReview:
		sub, ok := m.Tree.Session(row.SessionID)
		if !ok {
			return row.Repo
		}
		state := m.sessionState(sub.ID)
		return fmt.Sprintf("%s/%s · %s", row.Repo, reviewDisplayName(sub.ID, sub.ParentID), state)
	}
	return "(no session)"
}
