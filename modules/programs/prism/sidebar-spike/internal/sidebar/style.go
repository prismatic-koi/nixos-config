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

// frameStyle wraps the sidebar with a right-edge separator at the
// fixed Width.
func frameStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Width(Width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderRight(true).
		BorderForeground(lipgloss.Color("#27272a"))
}

// headerStyle is used for the sticky top row showing the spike's name
// and the visible-session count.
func headerStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#fafafa")).
		Bold(true).
		Padding(0, 1)
}

// footerStyle is used for the keymap hint at the bottom of the
// sidebar.
func footerStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#71717a")).
		Padding(0, 1)
}
