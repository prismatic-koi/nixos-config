package dashboard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// DashView is the shared rendering function for both popup and persistent modes.
// currentSession is used to show the "you are here" ◆ indicator.
// cursorActive controls whether the selection bar is shown.
func DashView(d Shared, currentSession string, cursorActive bool) string {
	if d.Width == 0 {
		// Before WindowSizeMsg: render a minimal skeleton so the first frame
		// is never blank. Use a fixed width so the output is deterministic.
		return SkeletonView(80)
	}

	if d.Loading {
		return SkeletonView(d.Width)
	}

	// fixedCore (defined below) is the irreducible column overhead. At widths
	// below fixedCore+1, the session header word "session" (7 chars) overflows
	// its 6-char slot when sessionW=0, so render a skeleton instead.
	const minUsableWidth = 26 // fixedCore+1
	if d.Width < minUsableWidth {
		return SkeletonView(d.Width)
	}

	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))
	styleHeader := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)
	styleIns := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorGreen))
	styleDel := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorRed))
	styleFg := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorForeground))
	stylePrompt := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)
	styleAgentType := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorSecondary))

	// ── column widths ────────────────────────────────────────────────────────
	// Tree prefix slot for child rows.
	//   Depth-1 prefixes: "  ├────── " or "  └────── " (exactly treePrefixW)
	//   Depth-2 prefixes: "  │   ├── " or "  │   └── " (exactly treePrefixW)
	// treePrefixW=10 accommodates the widest depth-2 connector without overflow.
	// Both depth-1 and depth-2 prefixes are exactly 10 runes; no padding is added.
	const treePrefixW = 10
	const agentTypeW = 12 // "coordinator " or "worker      " or "            "
	const stateW = 10
	const dotW = 2
	const sessionWStart = 20 // starting width; grows as columns are dropped
	const sessionWCap = 40   // maximum session width before the rest goes to title
	const statWFull = 22     // "2 files +122 -14"
	const statWCompact = 10  // "+122 -14"
	const modelWFull = 22    // e.g. "claude-sonnet-4-6    "

	// fixedCore is the non-negotiable fixed overhead: leading space + dot +
	// treePrefixW + gap-before-state + stateW.
	const fixedCore = 1 + dotW + treePrefixW + 2 + stateW

	showType := true
	showModel := true
	showStat := true
	statW := statWFull
	sessionW := sessionWStart

	// usedWidth returns the width of all non-title columns at their current settings.
	usedWidth := func() int {
		w := fixedCore + sessionW
		if showType {
			w += agentTypeW + 2
		}
		if showModel {
			w += modelWFull + 2
		}
		if showStat {
			w += statW + 2
		}
		return w
	}

	// calcTitleW returns the space available for the title column content
	// (excluding the 2-char gap before it).
	calcTitleW := func() int {
		return d.Width - usedWidth() - 2
	}

	// growSession offers surplus space to sessionW (up to sessionWCap) before
	// allocating the rest to titleW.
	growSession := func(tw int) int {
		if tw > 2 && sessionW < sessionWCap {
			gain := min(tw-2, sessionWCap-sessionW)
			sessionW += gain
			tw -= gain
		}
		return tw
	}

	// Start with the widest layout and shed columns in order until the layout fits.
	titleW := growSession(calcTitleW())

	if titleW < 0 {
		// Compact stat column first.
		statW = statWCompact
		titleW = growSession(calcTitleW())
	}
	if titleW < 0 {
		// Drop model.
		showModel = false
		titleW = growSession(calcTitleW())
	}
	if titleW < 0 {
		// Drop stat entirely.
		showStat = false
		titleW = growSession(calcTitleW())
	}
	if titleW < 0 {
		// Drop type.  After this only session + state remain; grow session to
		// fill all available space, clamped to 0 on extremely narrow terminals.
		showType = false
		avail := d.Width - fixedCore
		if avail > 0 {
			sessionW = avail
		} else {
			sessionW = 0
		}
		titleW = 0
	}

	var sb strings.Builder

	// ── header: stats left, art right ───────────────────────────────────────
	sb.WriteString(RenderHeader(d, styleDim, styleIns, styleDel))

	// Rainbow separator between header and column headers.
	sb.WriteString(RainbowLineWidth(strings.Repeat("─", d.Width), d.Width))
	sb.WriteString("\n")

	changesHeader := "changes"
	if statW == statWCompact {
		changesHeader = "+/-"
	}
	// Column header: top-level rows have no tree prefix gap, so session column
	// spans treePrefixW+sessionW total (same total width as all data rows).
	header := fmt.Sprintf(" %-*s%-*s",
		dotW, "",
		treePrefixW+sessionW, "session",
	)
	if showType {
		header += fmt.Sprintf("  %-*s", agentTypeW, "type")
	}
	if showModel {
		header += fmt.Sprintf("  %-*s", modelWFull, "model")
	}
	header += fmt.Sprintf("  %-*s", stateW, "state")
	if showStat {
		header += fmt.Sprintf("  %-*s", statW, changesHeader)
	}
	if titleW >= 5 {
		header += fmt.Sprintf("  %-*s", titleW, "title")
	}
	sb.WriteString(styleHeader.Render(header))
	sb.WriteString("\n\n")

	sessions := d.Displayed
	if len(sessions) == 0 {
		if d.FilterActive {
			sb.WriteString(styleDim.Render("  no matches"))
		} else {
			sb.WriteString(styleDim.Render("  no active sessions"))
		}
		sb.WriteString("\n")
	} else if d.FilterActive {
		// Flat list while filter is active (no grouping — easier to scan).
		for i, s := range sessions {
			sb.WriteString(RenderSessionRow(d, s, i, "" /*treePrefix*/, currentSession, cursorActive, styleDim, styleIns, styleDel, styleFg, styleAgentType, sessionW, agentTypeW, stateW, statW, statWCompact, titleW, modelWFull, showType, showModel, showStat))
		}
	} else {
		// Flat view with inline child detection via look-ahead.
		isTopLevel := func(name string) bool {
			branch := SessionBranch(name)
			return branch == name || branch == "@main"
		}
		isDepth1Child := func(name string) bool {
			return !isTopLevel(name) && !IsDepth2Session(name)
		}
		// groupHasTopLevel returns true if the contiguous run of same-repo
		// sessions that includes sessions[i] contains at least one top-level row.
		groupHasTopLevel := func(i int) bool {
			thisRepo := SessionRepo(sessions[i].Name)
			// Walk back to the start of this repo's run.
			start := i
			for start > 0 && SessionRepo(sessions[start-1].Name) == thisRepo {
				start--
			}
			for k := start; k < len(sessions) && SessionRepo(sessions[k].Name) == thisRepo; k++ {
				if isTopLevel(sessions[k].Name) {
					return true
				}
			}
			return false
		}
		for i, s := range sessions {
			isD1Child := isDepth1Child(s.Name) && groupHasTopLevel(i)
			isD2Child := IsDepth2Session(s.Name)
			var treePrefix string
			if isD2Child {
				// Depth-2 review session: "  │   ├── " or "  │   └── "
				// Look ahead within the same parent branch to determine if last sibling.
				parentBranch := Depth2ParentBranch(s.Name)
				isLastD2 := true
				for j := i + 1; j < len(sessions); j++ {
					next := sessions[j]
					if SessionRepo(next.Name) != SessionRepo(s.Name) {
						break
					}
					if IsDepth2Session(next.Name) && Depth2ParentBranch(next.Name) == parentBranch {
						isLastD2 = false
						break
					}
					// If we hit a non-depth-2 session in the same repo, stop.
					if !IsDepth2Session(next.Name) {
						break
					}
				}
				if isLastD2 {
					treePrefix = "  │   └── "
				} else {
					treePrefix = "  │   ├── "
				}
			} else if isD1Child {
				// Look ahead to determine if this is the last depth-1 child in the group.
				isLastChild := true
				thisRepo := SessionRepo(s.Name)
				for j := i + 1; j < len(sessions); j++ {
					if SessionRepo(sessions[j].Name) != thisRepo {
						break
					}
					if isDepth1Child(sessions[j].Name) || IsDepth2Session(sessions[j].Name) {
						isLastChild = false
						break
					}
				}
				if isLastChild {
					treePrefix = "  └────── "
				} else {
					treePrefix = "  ├────── "
				}
			}
			sb.WriteString(RenderSessionRow(d, s, i, treePrefix, currentSession, cursorActive, styleDim, styleIns, styleDel, styleFg, styleAgentType, sessionW, agentTypeW, stateW, statW, statWCompact, titleW, modelWFull, showType, showModel, showStat))
		}
	}

	// Filter prompt or help hint at the bottom.
	if d.FilterActive {
		sb.WriteString("\n")
		sb.WriteString(stylePrompt.Render(" / "))
		sb.WriteString(d.FilterText)
		sb.WriteString(styleDim.Render("█"))
		sb.WriteString("\n")
	} else if d.StatusMsg != "" {
		sb.WriteString("\n")
		styleErr := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorRed))
		sb.WriteString(styleErr.Render("  " + d.StatusMsg))
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n")
		sb.WriteString(styleDim.Render("  / filter  ↑↓/jk navigate  enter select  q quit"))
		sb.WriteString("\n")
	}

	return sb.String()
}

// ── math helpers ─────────────────────────────────────────────────────────────

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
