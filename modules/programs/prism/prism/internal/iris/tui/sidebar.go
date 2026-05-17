package tui

// sidebar.go — left-pane (session list) rendering.
//
// Layout per session row (issue #1771, design doc child 6):
//
//	"<g> <name>                  <state>  <role>  HH:MM"
//	"   ↳ <one-line preview…>"                        (optional)
//
// where:
//
//   - <g> is a one-column glyph indicator: "*" when the session is in the
//     `waiting` state (paused for the next user prompt — the iris equivalent
//     of "needs operator attention"), and a space otherwise. The glyph
//     supplements the existing yellow colouring on the state label so the
//     waiting cue is not colour-only — important for low-vision operators
//     and for terminals that down-render bold/colour cells.
//
//   - HH:MM is the wall-clock time at which the most recent agent_events
//     row for this session arrived at the TUI. The daemon's event frame
//     does not currently expose the DB `created_at` on the wire, so the
//     TUI uses time.Now() at frame-arrival as a best-effort proxy. This
//     matches the convention already used by internal/iris/narrative,
//     which renders narrative-line timestamps with time.Now() for the
//     same reason. "-" is shown for sessions that have produced no events
//     since the TUI subscribed (newly-spawned, just-restored, or no
//     activity yet) so the timestamp column stays vertically aligned
//     instead of leaving a ragged blank.
//
//   - The optional preview line is shown only when at least one
//     msg_assistant event has been observed for the session. It is dim,
//     truncated to fit the inner pane width, and indented under the
//     session name. When no msg_assistant has arrived (today's state,
//     pre-#1764) the preview line is omitted entirely and the sidebar
//     renders exactly as it did before this PR — important because
//     #1764 is still in flight and `msg_assistant` events do not yet
//     reach the TUI in the field.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/payload"
)

// waitingGlyph is the one-column indicator prefixed to rows whose session
// is in the `waiting` state. Picked as "*" rather than "!" or "●" because
// it is the most universally rendered glyph across terminal fonts and is
// already an established "needs attention" cue in many TUIs (mutt, git
// status, etc).
const waitingGlyph = "*"

// timeColWidth is the fixed width of the timestamp column ("HH:MM" or "-",
// right-padded). 5 columns covers "HH:MM" exactly; "-" is right-justified
// within that width.
const timeColWidth = 5

// noEventTimePlaceholder is rendered in the timestamp column when no
// agent_events have been observed for a session yet. A literal dash keeps
// the column visually aligned with the HH:MM rows above and below.
const noEventTimePlaceholder = "-"

// formatSidebarTime returns the HH:MM timestamp string for a sidebar row,
// or noEventTimePlaceholder when t is the zero value (no events yet).
func formatSidebarTime(t time.Time) string {
	if t.IsZero() {
		return noEventTimePlaceholder
	}
	return t.Format("15:04")
}

// styleSidebarTime renders the timestamp column with a dim/secondary
// foreground so the eye is drawn to the session name and state, not the
// timestamp. The string is right-padded to timeColWidth.
func styleSidebarTime(t time.Time) string {
	s := formatSidebarTime(t)
	// Right-align inside the column so single-character "-" sits where the
	// minute digits sit on populated rows.
	if len(s) < timeColWidth {
		s = strings.Repeat(" ", timeColWidth-len(s)) + s
	}
	return styleDim.Render(s)
}

// previewIndent is the leading whitespace for the optional second-line
// assistant-message preview. Two spaces aligns the preview under the
// session name (past the glyph column + its trailing space).
const previewIndent = "  ↳ "

// extractAssistantPreview parses a msg_assistant payload JSON and returns
// the first line of the assistant's text, suitable for use as a sidebar
// preview. Empty string on parse failure or empty text — callers treat
// "" as "do not update the preview".
func extractAssistantPreview(payloadJSON string) string {
	var p payload.MsgAssistant
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return ""
	}
	text := strings.TrimSpace(p.Text)
	if text == "" {
		return ""
	}
	// Collapse to first non-empty line; the sidebar has at most one row of
	// preview real estate, so multi-line replies would render as a single
	// concatenated mess otherwise.
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// extractAssistantModelCost parses a msg_assistant payload and returns
// (model, cost). Both are zero-valued when the payload doesn't parse or
// doesn't include them; callers use those to mean "do not update". Used
// by handleDaemonFrame to keep the status-line strip (#1767) populated
// without re-decoding the payload in the renderer.
func extractAssistantModelCost(payloadJSON string) (string, float64) {
	var p payload.MsgAssistant
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return "", 0
	}
	return p.Model, p.Cost
}

// previewIndentWidth is the visible column width of previewIndent. Hard
// coded because previewIndent contains a multi-byte rune ("↳") and
// len(previewIndent) returns the byte count, not the display width.
const previewIndentWidth = 4 // two spaces + "↳" (1 col) + one space

// renderPreviewLine renders the optional second-line assistant-message
// preview, truncated to fit innerW. Returns "" when no preview should be
// shown (preview is empty or innerW too small for any content).
func renderPreviewLine(preview string, innerW int) string {
	if preview == "" {
		return ""
	}
	// Reserve indent; if there's no room for at least a couple of preview
	// columns after the indent, skip the line entirely rather than render a
	// useless lone "↳".
	avail := innerW - previewIndentWidth
	if avail < 4 {
		return ""
	}
	body := truncate(preview, avail)
	line := previewIndent + body
	return styleDim.Render(padRight(line, innerW))
}

// renderSessionRow renders the primary one-line row for a single session.
// selected controls whether the row uses the styleSelected highlight
// (cursor row) or styleNormal. innerW is the renderable width inside the
// pane border.
//
// Column layout (no inter-column separator between glyph and name — the
// glyph runs straight into the name, which both saves one column for
// narrow terminals and reads naturally because the glyph is either "*"
// or a space):
//
//	glyph(1) + name(nameW) + sep(1) + state(8) + sep(1) + role(6) +
//	sep(1) + time(timeColWidth)
//	= nameW + 18 + timeColWidth
//
// At the standard test width (innerW=40, timeColWidth=5) this gives
// nameW=17 — exactly enough for "nixos-config@main". On narrower
// terminals the name is truncated with an ellipsis (clamped to a min of
// 8 columns); the state, role and timestamp still render in full. We do
// NOT widen minLeftWidth from the current 28 — doing so would steal
// columns from the right (events) pane in 80-col terminals.
func renderSessionRow(si sessionItem, innerW int, selected bool) string {
	nameW := innerW - (1 + 1 + 8 + 1 + 6 + 1 + timeColWidth)
	if nameW < 8 {
		nameW = 8
	}

	name := truncate(si.snap.Name, nameW)
	role := truncate(si.snap.Role, 6)

	if selected {
		// On the cursor row the entire line is rendered with the
		// selected style (yellow background). Embedding the per-fragment
		// ANSI escapes used on non-selected rows (yellow glyph, coloured
		// state label, dim timestamp) inside that yellow background
		// would either clash visually or break selection contrast — the
		// glyph would disappear into the background, the dim timestamp
		// would be unreadable. So we rebuild the row as plain text and
		// let styleSelected paint the whole span uniformly.
		plain := fmt.Sprintf("%s%-*s %-8s %-6s %s",
			plainGlyph(si.snap.State),
			nameW, name,
			plainState(si.snap.State),
			role,
			plainSidebarTime(si.lastEventAt),
		)
		if pad := innerW - displayWidth(plain); pad > 0 {
			plain += strings.Repeat(" ", pad)
		}
		return styleSelected.Render(plain)
	}

	glyph := " "
	if si.snap.State == "waiting" {
		glyph = styleYellow.Render(waitingGlyph)
	}
	state := stateLabel(si.snap.State)
	ts := styleSidebarTime(si.lastEventAt)

	// The glyph, state and timestamp fragments contain ANSI escapes
	// that inflate their byte length; fmt's %-*s padding operates on
	// bytes, not display columns, so we cannot rely on it for those
	// fields. Build the row by concatenation and pad the final result
	// based on the visible (ANSI-stripped) width via lipgloss.Width.
	line := glyph +
		fmt.Sprintf("%-*s ", nameW, name) +
		state + " " +
		fmt.Sprintf("%-6s ", role) +
		ts
	if pad := innerW - lipgloss.Width(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return styleNormal.Render(line)
}

// plainSidebarTime is the un-styled, right-aligned timestamp string used
// on the cursor row. Mirrors styleSidebarTime() but without the dim ANSI
// wrapper so the selection background applies uniformly.
func plainSidebarTime(t time.Time) string {
	s := formatSidebarTime(t)
	if len(s) < timeColWidth {
		s = strings.Repeat(" ", timeColWidth-len(s)) + s
	}
	return s
}

// plainGlyph returns the un-styled glyph used for the row prefix. Used
// when the cursor row needs the selection background to cover everything
// uniformly without inner colour escapes.
func plainGlyph(state string) string {
	if state == "waiting" {
		return waitingGlyph
	}
	return " "
}

// plainState returns the un-styled, fixed-width state string used for the
// cursor row. Mirrors the widths in stateLabel() so column alignment is
// identical between the highlighted row and the normal rows.
func plainState(state string) string {
	switch state {
	case "active":
		return "active  "
	case "waiting":
		return "waiting "
	case "spawning":
		return "spawning"
	case "finished":
		return "finished"
	case "error":
		return "error   "
	default:
		return padRight(state, 8)
	}
}

// renderPreviewRow renders the indented dim preview row for a session, or
// returns "" if no preview should be drawn. selected controls whether the
// row picks up the selection background.
func renderPreviewRow(si sessionItem, innerW int, selected bool) string {
	line := renderPreviewLine(si.lastAssistantPreview, innerW)
	if line == "" {
		return ""
	}
	if !selected {
		return line
	}
	// For the selected row we want the highlight to extend across the
	// preview line as well, so the user sees the whole session block as a
	// single selection. Re-render in plain text under the selected style.
	avail := innerW - previewIndentWidth
	body := truncate(si.lastAssistantPreview, avail)
	plain := previewIndent + body
	if pad := innerW - displayWidth(plain); pad > 0 {
		plain += strings.Repeat(" ", pad)
	}
	return styleSelected.Render(plain)
}
