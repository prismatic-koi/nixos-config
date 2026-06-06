package render

import "github.com/charmbracelet/lipgloss"

// SidebarWidth is the fixed 32-column sidebar width codified in §3.1.
// Resize is explicitly deferred to a post-MVP follow-up.
const SidebarWidth = 32

// NarrowWidthThreshold is the terminal-width cutoff (in columns) below
// which the wide split-pane layout gives way to the mobile-shape layout
// described in §3.1. At total width 80 a 32-col sidebar leaves 48 cols
// for the pane — too narrow for most code; the crossover where a
// full-width pane clearly beats the split sits here.
const NarrowWidthThreshold = 80

// NarrowPopoverExtra is the +6 column extra-width the narrow-mode popover
// gets over SidebarWidth per §3.1 ("Popover width is sidebar.Width + 6
// columns").
const NarrowPopoverExtra = 6

// NarrowPopoverInset is the right-edge inset that keeps the popover from
// reaching the terminal's right edge — §3.1: "capped at terminal_width - 4
// so the pane background peeks through on the right edge".
const NarrowPopoverInset = 4

// Tailwind hex values verbatim from the §3.1 state-to-colour table. Keep
// them as named constants so tests can assert against the same string the
// renderer emits.
const (
	colourGreen400  = "#4ade80" // active
	colourGrey500   = "#71717a" // idle
	colourYellow400 = "#facc15" // waiting
	colourBlue400   = "#60a5fa" // reviewing
	colourRed400    = "#f87171" // escalated
	colourGrey600   = "#52525b" // finished (strikethrough)

	colourZinc50  = "#fafafa" // selection fg / header
	colourZinc200 = "#e4e4e7" // session name default
	colourZinc400 = "#a1a1aa" // repo header
	colourZinc700 = "#3f3f46" // selection bg
	colourZinc800 = "#27272a" // divider / sidebar border
	colourZinc900 = "#18181b" // topbar bg
)

// stateVisual is the (glyph, colour, strikethrough?) triple for each
// prism state — the source of truth for the §3.1 mapping. Indexed by
// State.
type stateVisual struct {
	glyph         string
	colour        lipgloss.Color
	strikethrough bool
}

// stateVisuals codifies the §3.1 state-to-glyph-and-colour table. The
// values here are the only place hex codes appear; the rest of the
// renderer reads from this map so a future revision lands in one spot.
//
// `escalated` is the one deliberate break from the circle family per §3.1
// — a triangle reads distinctly at a glance, which is the right cue for
// the state that demands user attention.
//
// `finished` uses the same `●` glyph as active but with strikethrough and
// a darker grey, matching the §3.1 "*strikethrough*" annotation.
var stateVisuals = map[State]stateVisual{
	StateActive:    {glyph: "●", colour: lipgloss.Color(colourGreen400)},
	StateIdle:      {glyph: "○", colour: lipgloss.Color(colourGrey500)},
	StateWaiting:   {glyph: "◐", colour: lipgloss.Color(colourYellow400)},
	StateReviewing: {glyph: "◑", colour: lipgloss.Color(colourBlue400)},
	StateEscalated: {glyph: "▲", colour: lipgloss.Color(colourRed400)},
	StateFinished:  {glyph: "●", colour: lipgloss.Color(colourGrey600), strikethrough: true},
}

// visualFor returns the codified stateVisual for s, falling back to the
// idle entry when s is out of range (keeps the renderer total — a bad
// State value renders as idle rather than panicking).
func visualFor(s State) stateVisual {
	if v, ok := stateVisuals[s]; ok {
		return v
	}
	return stateVisuals[StateIdle]
}

// glyphStyle returns the lipgloss.Style for a state's glyph cell. The
// glyph keeps its own colour even when the row is selected (§3.1: "The
// state glyph keeps its own colour; only the session name and the
// right-aligned state label adopt the selection treatment").
func glyphStyle(s State) lipgloss.Style {
	v := visualFor(s)
	return lipgloss.NewStyle().Foreground(v.colour).Bold(true)
}

// nameStyle returns the style for the session name cell. Selected rows
// invert to the §3.1 selection highlight (zinc-700 bg, zinc-50 fg, bold);
// finished sessions strike through their name (§3.1).
func nameStyle(s State, selected bool) lipgloss.Style {
	v := visualFor(s)
	style := lipgloss.NewStyle()
	if v.strikethrough {
		style = style.Foreground(v.colour).Strikethrough(true)
	} else {
		style = style.Foreground(lipgloss.Color(colourZinc200))
	}
	if selected {
		style = style.
			Foreground(lipgloss.Color(colourZinc50)).
			Background(lipgloss.Color(colourZinc700)).
			Bold(true)
	}
	return style
}

// dimStyle is the grey used for the right-aligned state label, the
// `(N rev)` review badge, and the footer hint.
func dimStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(colourGrey500))
}

// repoHeaderStyle is the style for the `▾ nixos-config` / `▸ home-ops`
// rows. Repo headers participate in selection so `←` / `→` can collapse
// and expand the cluster (§3.1).
func repoHeaderStyle(selected bool) lipgloss.Style {
	s := lipgloss.NewStyle().
		Foreground(lipgloss.Color(colourZinc400)).
		Bold(true)
	if selected {
		s = s.
			Foreground(lipgloss.Color(colourZinc50)).
			Background(lipgloss.Color(colourZinc700))
	}
	return s
}

// frameStyle wraps the sidebar with a single-cell right-edge border.
// Used identically by the wide-mode sidebar and the narrow-mode popover;
// the caller sets Width / Height so the same style serves both.
func frameStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderRight(true).
		BorderForeground(lipgloss.Color(colourZinc800))
}

// dividerStyle is the colour of the horizontal rule that sits between
// the pinned header and the scrollable body.
func dividerStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(colourZinc800))
}

// headerStyle is the §3.1 "prism · N sessions" header row.
func headerStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(colourZinc50)).
		Bold(true)
}

// footerStyle is the §3.1 keymap-hint footer.
func footerStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(colourGrey500))
}

// topbarStyle is the narrow-mode 1-row identity strip. Dim background per
// §3.1 ("Dim background").
func topbarStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(colourZinc200)).
		Background(lipgloss.Color(colourZinc900))
}

// topbarHintStyle is the right-anchored `^B switch` hint on the topbar.
// Dimmer than the identity so the session identity reads first.
func topbarHintStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(colourZinc400)).
		Background(lipgloss.Color(colourZinc900))
}

// paneStyle is the active pane area's outer style — used to render the
// "(no PTY)" placeholder when no Host is wired. The real PTY content path
// (renderActivePane) does its own positioning and does not pipe through
// this style.
func paneStyle(width, height int) lipgloss.Style {
	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		Padding(0, 1).
		Foreground(lipgloss.Color(colourZinc400))
}

// paneHintStyle dims the "(no PTY for <pane>)" placeholder text so it
// reads as chrome rather than content.
func paneHintStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(colourGrey600)).
		Italic(true)
}
