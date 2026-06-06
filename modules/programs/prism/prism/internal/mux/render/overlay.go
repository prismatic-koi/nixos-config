package render

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// overlayAt splices an overlay block into base at (row, col), one row
// per source string-line. Used by viewNarrow to render the §3.1 popover
// on top of the pane background — lipgloss does not ship a Z-index
// composer, so we do it by hand.
//
// The base line is padded with spaces to col before the overlay, then
// the tail past (col + overlay-width) is preserved so the popover
// appears as a window onto the underlying pane. Overlay rows that
// would land past the base's last line are dropped — the caller is
// expected to size the popover so this does not happen, but the bound
// keeps a stray off-by-one from panicking.
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
			tail = ansi.Cut(baseLines[dst], afterCol, baseLineW)
		}
		baseLines[dst] = left + oLine + tail
	}
	return strings.Join(baseLines, "\n")
}
