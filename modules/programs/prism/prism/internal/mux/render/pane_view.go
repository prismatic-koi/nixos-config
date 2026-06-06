package render

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/prismatic-koi/prism/internal/mux/vt"
)

// renderActivePane is the right-hand split-pane content. Resolves the
// currently-selected session + active pane to a *vt.Host via the
// HostProvider and renders its frame; when no host is available, draws
// the "(no PTY)" placeholder. Always exactly (width, height) cells.
func (m *Model) renderActivePane(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}

	row, ok := m.selectedRow()
	if !ok || m.Tree == nil {
		return placeholderPane(width, height, "(no session)")
	}

	var sessID, paneName string
	switch row.Kind {
	case RowRepo:
		ids := m.Tree.RepoSessions(row.Repo)
		if len(ids) == 0 {
			return placeholderPane(width, height, "(repo has no sessions)")
		}
		sess, ok := m.Tree.Session(ids[0])
		if !ok {
			return placeholderPane(width, height, "(no session)")
		}
		sessID = sess.ID
		paneName = sess.ActivePane
	case RowSession, RowReview:
		sess, ok := m.Tree.Session(row.SessionID)
		if !ok {
			return placeholderPane(width, height, "(no session)")
		}
		sessID = sess.ID
		paneName = sess.ActivePane
	}

	if m.Hosts == nil {
		return placeholderPane(width, height, fmt.Sprintf("(no PTY for %s)", displayLabel(sessID, paneName)))
	}
	host := m.Hosts.Host(sessID, paneName)
	if host == nil {
		return placeholderPane(width, height, fmt.Sprintf("(no PTY for %s)", displayLabel(sessID, paneName)))
	}
	return renderHostFrame(host, width, height)
}

// renderHostFrame turns vt.Host.RenderRows() output into a (width,
// height) string. Each emulator row gets truncated/padded to width; rows
// past the emulator's height are filled with spaces. Per the vt package
// docstring on RenderRows, this is the live-replay primitive — the
// caller is expected to do its own padding, which we do here.
func renderHostFrame(host *vt.Host, width, height int) string {
	cols, rows := host.Size()
	if cols == 0 || rows == 0 {
		return placeholderPane(width, height, "(host not sized)")
	}
	rendered := host.RenderRows()
	lines := make([]string, height)
	for i := 0; i < height; i++ {
		var raw string
		if i < len(rendered) {
			raw = rendered[i]
		}
		lines[i] = truncateOrPad(raw, width)
	}
	return strings.Join(lines, "\n")
}

// placeholderPane fills the pane area with hint text — centred
// horizontally on a single line, vertically centred. Used whenever a
// real host isn't available. Always exactly (width × height) cells so
// composition with the sidebar via lipgloss.JoinHorizontal aligns.
func placeholderPane(width, height int, message string) string {
	style := paneStyle(width, height)
	if width < 4 || height < 1 {
		return style.Render("")
	}
	hint := paneHintStyle().Render(message)
	// Centre the hint roughly mid-height. paneStyle adds 1-col horizontal
	// padding via the lipgloss padding API, so we don't pad here.
	hintWidth := ansi.StringWidth(message)
	innerWidth := width - 2 // account for paneStyle's Padding(0, 1)
	if innerWidth < 1 {
		innerWidth = 1
	}
	leftPad := (innerWidth - hintWidth) / 2
	if leftPad < 0 {
		leftPad = 0
	}
	centred := strings.Repeat(" ", leftPad) + hint
	// Vertical centring: build (height - paneStyle padding) lines with
	// the hint on the middle row.
	innerHeight := height
	if innerHeight < 1 {
		innerHeight = 1
	}
	mid := innerHeight / 2
	lines := make([]string, innerHeight)
	for i := range lines {
		if i == mid {
			lines[i] = centred
		} else {
			lines[i] = ""
		}
	}
	return style.Render(strings.Join(lines, "\n"))
}

// displayLabel composes a "<session>:<pane>" summary for the placeholder
// hint. Empty pane or session IDs are gracefully omitted.
func displayLabel(sessionID, paneName string) string {
	switch {
	case sessionID == "" && paneName == "":
		return "selection"
	case paneName == "":
		return sessionID
	case sessionID == "":
		return paneName
	default:
		return sessionID + ":" + paneName
	}
}

// lipglossPlaceholderWidth returns the value lipgloss reports for the
// rendered placeholder pane — handy for tests asserting the pane is
// exactly (width, height) cells. Kept here so the test does not have to
// reach into lipgloss internals.
func lipglossPlaceholderWidth(s string) int {
	maxw := 0
	for _, line := range strings.Split(s, "\n") {
		if w := lipgloss.Width(line); w > maxw {
			maxw = w
		}
	}
	return maxw
}
