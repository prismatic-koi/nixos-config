package dashboard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	colorful "github.com/lucasb-eyer/go-colorful"
	"github.com/prismatic-koi/prism/internal/agent"
)

// ── header art ────────────────────────────────────────────────────────────────

// artLines is the DSOTM-inspired prism header: small figlet "PRISM" with a
// triangle whose apex sits above the M and whose right face emits a short
// rainbow spectrum fan. The triangle left face merges with the figlet right edge.
var artLines = []string{
	`                      /\`,
	`                     /  \·───────────`,
	` ___  ___ ___ ___ __/ __ \·───────────`,
	`| _ \| _ \_ _/ __|  \/  | \·───────────`,
	`|  _/|   /| |\__ \ |\/| |  \·───────────`,
	`|_|  |_|_\___|___/_|  |_|   \·───────────`,
	`                /____________\`,
}

// artWidth is the width of the widest art line, used to normalise gradient positions.
const artWidth = 41

// artHeight is the number of lines in the art block.
const artHeight = 7

// rainbowAt returns the everforest spectrum colour at normalised position t ∈ [0,1].
// Uses the 5 available accent colours as stops: red → yellow → green → blue → purple.
func rainbowAt(t float64) colorful.Color {
	stops := []colorful.Color{
		mustHex(ColorRed),
		mustHex(ColorYellow),
		mustHex(ColorGreen),
		mustHex(ColorBlue),
		mustHex(ColorPurple),
	}
	scaled := t * float64(len(stops)-1)
	i := int(scaled)
	if i >= len(stops)-1 {
		return stops[len(stops)-1]
	}
	frac := scaled - float64(i)
	r1, g1, b1 := stops[i].R, stops[i].G, stops[i].B
	r2, g2, b2 := stops[i+1].R, stops[i+1].G, stops[i+1].B
	return colorful.Color{
		R: r1 + (r2-r1)*frac,
		G: g1 + (g2-g1)*frac,
		B: b1 + (b2-b1)*frac,
	}
}

func mustHex(h string) colorful.Color {
	c, err := colorful.Hex(h)
	if err != nil {
		return colorful.Color{}
	}
	return c
}

// RainbowLine renders a single art line with a left-to-right rainbow gradient
// normalised over artWidth columns.
func RainbowLine(line string) string {
	return RainbowLineWidth(line, artWidth)
}

// RainbowLineWidth renders a string with a rainbow gradient spread across
// totalWidth columns, so short strings like "PRISM" get the full spectrum.
func RainbowLineWidth(line string, totalWidth int) string {
	if totalWidth < 2 {
		totalWidth = 2
	}
	var sb strings.Builder
	col := 0
	for _, ch := range line {
		if ch == ' ' {
			sb.WriteRune(' ')
		} else {
			t := float64(col) / float64(totalWidth-1)
			if t > 1 {
				t = 1
			}
			c := rainbowAt(t)
			hex := fmt.Sprintf("#%02x%02x%02x",
				uint8(c.R*255+0.5),
				uint8(c.G*255+0.5),
				uint8(c.B*255+0.5),
			)
			sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render(string(ch)))
		}
		col++
	}
	return sb.String()
}

// RenderHeader composites the stats panel (left) with the art block (right)
// into a single header string that fills termWidth on each line.
// When the terminal is too short, a compact 2-line header is used instead.
func RenderHeader(d Shared, styleDim, styleIns, styleDel lipgloss.Style) string {
	// ── compute stats ────────────────────────────────────────────────────────
	var nActive, nWaiting, nIdle, nFinished, nInterrupted int
	var totalIns, totalDel int
	for _, s := range d.Sessions {
		result := d.GitStats[s.AgentPath]
		if result.Ok {
			totalIns += result.Stat.Insertions
			totalDel += result.Stat.Deletions
		}
		switch agent.AgentState(s.AgentState) {
		case agent.StateActive:
			nActive++
		case agent.StateWaiting:
			nWaiting++
		case agent.StateFinished:
			nFinished++
		case agent.StateInterrupted:
			nInterrupted++
		default:
			nIdle++
		}
	}

	var stateParts []string
	if nActive > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d active", nActive))
	}
	if nWaiting > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d waiting", nWaiting))
	}
	if nFinished > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d done", nFinished))
	}
	if nInterrupted > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d interrupted", nInterrupted))
	}
	if nIdle > 0 || len(stateParts) == 0 {
		stateParts = append(stateParts, fmt.Sprintf("%d idle", nIdle))
	}
	stateLine := strings.Join(stateParts, "  ")

	styleStatLabel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)
	styleStatDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	// padTo pads a rendered (ANSI) string to exactly w visible characters.
	padTo := func(s string, w int) string {
		vis := lipgloss.Width(s)
		if vis >= w {
			return s
		}
		return s + strings.Repeat(" ", w-vis)
	}

	wordmark := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true).Render("PRISM")
	const wordmarkW = 5

	// ── compact mode: terminal too short for full art block ──────────────────
	// Full header needs: artHeight + separator(1) + col-header+blank(2) + sessions + bottom-blank(1)
	fullHeaderNeeded := artHeight + 1 + 2 + len(d.Sessions) + 1
	if d.Height > 0 && d.Height < fullHeaderNeeded {
		// 2 lines: "N sessions  STATE_SUMMARY" left + PRISM right on line 1,
		// blank line 2 for breathing room.
		sessionCount := styleStatLabel.Render(fmt.Sprintf("%d sessions", len(d.Sessions)))
		stateStr := styleStatDim.Render(stateLine)
		leftContent := sessionCount + "  " + stateStr
		leftW := lipgloss.Width(leftContent)
		pad := d.Width - leftW - wordmarkW
		if pad < 1 {
			pad = 1
		}
		var sb strings.Builder
		sb.WriteString(leftContent)
		sb.WriteString(strings.Repeat(" ", pad))
		sb.WriteString(wordmark)
		sb.WriteString("\n\n")
		return sb.String()
	}

	// ── full mode ────────────────────────────────────────────────────────────
	var prLine string
	if !d.GhLoaded {
		prLine = "↑ …"
	} else {
		prLine = fmt.Sprintf("↑ %d open PRs", d.GhOpenPRs)
	}

	var changesLine string
	if totalIns == 0 && totalDel == 0 {
		changesLine = styleStatDim.Render("no changes")
	} else {
		changesLine = styleIns.Render(fmt.Sprintf("+%d", totalIns)) +
			"  " +
			styleDel.Render(fmt.Sprintf("-%d", totalDel))
	}

	prRendered := styleStatDim.Render(prLine)
	stateRendered := styleStatDim.Render(stateLine)
	sessionCountLine := styleStatLabel.Render(fmt.Sprintf("%d sessions", len(d.Sessions)))

	// Dynamically compute statsW from the actual visible widths of all stat
	// content lines, so that a long state line (e.g. all 5 status categories)
	// never overflows into the art column.
	const minStatsW = 20
	statsW := minStatsW
	for _, s := range []string{sessionCountLine, stateRendered, changesLine, prRendered} {
		if w := lipgloss.Width(s); w > statsW {
			statsW = w
		}
	}

	// 7 stat lines matching artHeight, each exactly statsW wide.
	// Lines 0-1: blank  2: sessions  3: states  4: changes  5: PRs  6: blank
	statLines := []string{
		strings.Repeat(" ", statsW),
		strings.Repeat(" ", statsW),
		padTo(sessionCountLine, statsW),
		padTo(stateRendered, statsW),
		padTo(changesLine, statsW),
		padTo(prRendered, statsW),
		strings.Repeat(" ", statsW),
	}

	// Only render art if there is enough horizontal room.
	showArt := d.Width >= statsW+artWidth

	var sb strings.Builder
	if showArt {
		middle := strings.Repeat(" ", d.Width-statsW-artWidth)
		for i, artLine := range artLines {
			stat := strings.Repeat(" ", statsW)
			if i < len(statLines) {
				stat = statLines[i]
			}
			sb.WriteString(stat)
			sb.WriteString(middle)
			sb.WriteString(RainbowLine(artLine))
			sb.WriteString("\n")
		}
	} else {
		// Narrow: stats lines with PRISM wordmark right-aligned on first line.
		for i, s := range statLines {
			if i == 0 {
				pad := d.Width - statsW - wordmarkW
				if pad < 0 {
					trimmed := s
					if len(trimmed) > d.Width-wordmarkW {
						trimmed = trimmed[:d.Width-wordmarkW]
					}
					sb.WriteString(trimmed)
					sb.WriteString(wordmark)
				} else {
					sb.WriteString(s)
					sb.WriteString(strings.Repeat(" ", pad))
					sb.WriteString(wordmark)
				}
			} else {
				sb.WriteString(s)
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// SkeletonView renders a minimal loading frame shown before the first DB fetch
// completes. This prevents the blank-frame-before-first-render bug.
func SkeletonView(width int) string {
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleHeader := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)

	var sb strings.Builder

	// Compact header: just the wordmark line.
	wordmark := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true).Render("PRISM")
	pad := width - 5
	if pad < 0 {
		pad = 0
	}
	sb.WriteString(strings.Repeat(" ", pad))
	sb.WriteString(wordmark)
	sb.WriteString("\n\n")

	// Separator.
	if width > 0 {
		sb.WriteString(RainbowLineWidth(strings.Repeat("─", width), width))
	}
	sb.WriteString("\n")

	// Column header.
	sb.WriteString(styleHeader.Render(" session"))
	sb.WriteString("\n\n")

	// Loading placeholder.
	sb.WriteString(styleDim.Render("  loading…"))
	sb.WriteString("\n")

	return sb.String()
}
