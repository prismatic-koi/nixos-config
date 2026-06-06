// Package sidebar implements the tree-render component under
// refinement in the sidebar-spike. style.go owns the visual vocabulary
// — glyphs, colours, padding — so iteration revisions on the visual
// design land in one file rather than scattered across the renderer.
package sidebar

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/sidebar-spike/internal/model"
)

// Width is the fixed sidebar width in v1 (per #2148 spec). Resize is
// deferred to the real render layer in planned PR #3.
const Width = 32

// stateVisual is the (glyph, colour) pair for each prism state. The
// initial values mirror the v1 strawman in the issue body; revisions
// during in-session iteration land here.
type stateVisual struct {
	glyph      string
	colour     lipgloss.Color
	strikethru bool
}

// stateVisuals is the source of truth for the state-to-glyph + colour
// mapping. Indexed by model.State.
var stateVisuals = map[model.State]stateVisual{
	model.StateActive:    {glyph: "●", colour: lipgloss.Color("#4ade80")},                    // green
	model.StateIdle:      {glyph: "○", colour: lipgloss.Color("#71717a")},                    // dim grey
	model.StateWaiting:   {glyph: "◐", colour: lipgloss.Color("#facc15")},                    // yellow
	model.StateReviewing: {glyph: "◑", colour: lipgloss.Color("#60a5fa")},                    // blue
	model.StateEscalated: {glyph: "▲", colour: lipgloss.Color("#f87171")},                    // red
	model.StateFinished:  {glyph: "●", colour: lipgloss.Color("#52525b"), strikethru: true}, // grey, strikethrough
}

// glyphStyle returns the lipgloss style for a state's glyph cell.
func glyphStyle(s model.State) lipgloss.Style {
	v := stateVisuals[s]
	return lipgloss.NewStyle().Foreground(v.colour).Bold(true)
}

// nameStyle returns the lipgloss style for the session name cell.
// Selected rows are inverted; finished sessions are dimmed +
// strikethrough.
func nameStyle(s model.State, selected bool) lipgloss.Style {
	v := stateVisuals[s]
	style := lipgloss.NewStyle()
	if v.strikethru {
		style = style.Foreground(v.colour).Strikethrough(true)
	} else {
		style = style.Foreground(lipgloss.Color("#e4e4e7"))
	}
	if selected {
		style = style.
			Foreground(lipgloss.Color("#fafafa")).
			Background(lipgloss.Color("#3f3f46")).
			Bold(true)
	}
	return style
}

// dimStyle is used for the state label rendered to the right of each
// row and for the branch-detail subtext.
func dimStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#71717a"))
}

// repoHeaderStyle is used for the repo cluster label.
func repoHeaderStyle(selected bool) lipgloss.Style {
	s := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#a1a1aa")).
		Bold(true)
	if selected {
		s = s.
			Foreground(lipgloss.Color("#fafafa")).
			Background(lipgloss.Color("#3f3f46"))
	}
	return s
}

// frameStyle wraps the sidebar with a right-edge separator. The
// caller sets Width / Height explicitly so the same style serves
// both the full sidebar (32 cols) and the narrow-mode popover
// (configurable width).
func frameStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderRight(true).
		BorderForeground(lipgloss.Color("#27272a"))
}

// dividerStyle is the colour of the horizontal rule that sits
// between the header and the scrollable body.
func dividerStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#27272a"))
}

// headerStyle is used for the pinned top row showing the spike's
// name and the non-review session count. v2: the header is composed
// separately from the body via lipgloss.JoinVertical so it can
// never be pushed off the top by overflowing content (the bug Ben
// caught in v1).
func headerStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#fafafa")).
		Bold(true)
}

// footerStyle is used for the pinned keymap hint at the bottom.
func footerStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#71717a"))
}

// topbarStyle is used by the narrow-mode top status bar that
// replaces the sidebar when the terminal is too narrow for a split
// layout. Mirrors herdr's mobile pattern: a single-row identity strip
// at the top with a `switch` hint anchored on the right.
func topbarStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#e4e4e7")).
		Background(lipgloss.Color("#18181b")).
		Padding(0, 1)
}

// topbarHintStyle is used for the trailing `^B switch` action label
// on the narrow-mode top bar. Dimmer so the session identity reads
// first.
func topbarHintStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#a1a1aa")).
		Background(lipgloss.Color("#18181b"))
}
