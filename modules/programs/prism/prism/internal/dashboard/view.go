package dashboard

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// RenderReviewGroupRow renders a virtual review-round group row (collapsed or
// expanded). treePrefix is the depth-1 connector ("  ├── " or "  └── ").
// expanded controls whether the ▼ (expanded) or ▶ (collapsed) indicator is shown.
func RenderReviewGroupRow(
	d Shared,
	s AgentSession,
	cursorIdx int,
	treePrefix string,
	expanded bool,
	cursorActive bool,
	styleDim, styleFg, styleAgentType lipgloss.Style,
	sessionW, stateW int,
) string {
	isSelected := cursorIdx == d.Cursor

	// Group rows have no "you are here" dot — they are not real sessions.
	const dot = "  "

	const treePrefixW = 10
	totalSessionW := treePrefixW + sessionW

	// Indicator: ▼ expanded, ▶ collapsed.
	indicator := "▶ "
	if expanded {
		indicator = "▼ "
	}

	// Display the group label (e.g. "~review-1") with the indicator prepended.
	label := Depth2Label(s.Name)
	if label == "" {
		label = SessionBranch(s.Name)
	}
	content := indicator + label

	// Build session area: treePrefix (6 runes) + content padded to fill remainder.
	runeCount := utf8.RuneCountInString(treePrefix)
	branchW := totalSessionW - runeCount
	if branchW <= 0 {
		content = ""
	} else if utf8.RuneCountInString(content) > branchW {
		content = string([]rune(content)[:branchW-1]) + "…"
	}
	sessionArea := treePrefix + fmt.Sprintf("%-*s", totalSessionW-runeCount, content)

	if isSelected && cursorActive {
		barBg := lipgloss.Color(ColorSecondary)
		if c, ok := stateStyle(s.AgentState).GetForeground().(lipgloss.Color); ok {
			barBg = c
		}
		plain := fmt.Sprintf(" %s%s  %-*s", dot, sessionArea, stateW, stateLabel(s.AgentState))
		row := lipgloss.NewStyle().
			Foreground(lipgloss.Color(ColorBg0)).
			Background(barBg).
			Bold(true).
			Width(d.Width).
			Render(plain)
		return row + "\n"
	}

	stateStr := lipgloss.NewStyle().
		Foreground(stateStyle(s.AgentState).GetForeground()).
		Render(fmt.Sprintf("%-*s", stateW, stateLabel(s.AgentState)))

	// Split the styling: tree connector in styleFg (matching leaf rows),
	// indicator + label in styleDim (blue, so the group row stays visually distinct).
	treePart := styleFg.Render(fmt.Sprintf(" %s%s", dot, treePrefix))
	labelPart := styleDim.Render(fmt.Sprintf("%-*s", totalSessionW-runeCount, content))
	prefix := treePart + labelPart
	row := prefix + styleFg.Render("  ") + stateStr
	return row + "\n"
}

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
	//   Depth-1 prefixes: "  ├── " or "  └── " (6 runes; branch field absorbs spare 4)
	//   Depth-2 prefixes: "  │   ├── " or "  │   └── " (exactly treePrefixW=10 runes)
	// treePrefixW=10 sets the total reserved width (prefix + branch) so column
	// alignment is preserved across all depths.
	const treePrefixW = 10
	const agentTypeW = 15 // widest: "review-security" (15 chars); other types right-pad.
	const stateW = 10
	const dotW = 2
	const sessionWCap = 40  // maximum session width before the rest goes to title
	const statWFull = 22    // "2 files +122 -14"
	const statWCompact = 10 // "+122 -14"
	const modelWFull = 22   // e.g. "claude-sonnet-4-6    "
	const harnessW = 10     // "opencode  " or future harness names

	// fixedCore is the non-negotiable fixed overhead: leading space + dot +
	// treePrefixW + gap-before-state + stateW.
	const fixedCore = 1 + dotW + treePrefixW + 2 + stateW

	showType := true
	showModel := true
	showHarness := true
	showStat := true
	statW := statWFull
	// Derive session column width from the longest rendered session name
	// across all displayed entries, clamped to [7, 40]. This allows the
	// column to be narrower than the old hardcoded 20-char floor when all
	// session names are short, while still never truncating the "session"
	// header word and respecting the existing cap.
	sessionW := SessionColumnWidth(d.Displayed)

	// usedWidth returns the width of all non-title columns at their current settings.
	usedWidth := func() int {
		w := fixedCore + sessionW
		if showType {
			w += agentTypeW + 2
		}
		if showHarness {
			w += harnessW + 2
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
		// Drop harness.
		showHarness = false
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
	if showHarness {
		header += fmt.Sprintf("  %-*s", harnessW, "harness")
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
			sb.WriteString(RenderSessionRow(d, s, i, "" /*treePrefix*/, currentSession, cursorActive, styleDim, styleIns, styleDel, styleFg, styleAgentType, sessionW, agentTypeW, stateW, statW, statWCompact, titleW, modelWFull, harnessW, showType, showHarness, showModel, showStat))
		}
	} else {
		// Flat view with inline child detection via look-ahead.
		// The Displayed list (built by BuildDisplayRows) may include:
		//   - Top-level rows (repo@main or plain session names)
		//   - Depth-1 child rows (repo@branch, non-review)
		//   - Virtual review-round group rows (IsReviewGroup=true, displayed as depth-1)
		//   - Depth-2 per-agent child rows (IsDepth2Session=true, visible only when group is expanded)
		isTopLevel := func(s AgentSession) bool {
			if s.IsReviewGroup {
				return false
			}
			branch := SessionBranch(s.Name)
			return branch == s.Name || branch == "@main"
		}
		isDepth1Child := func(s AgentSession) bool {
			return !isTopLevel(s) && !IsDepth2Session(s.Name) && !s.IsReviewGroup
		}
		// isDepth1Like returns true for rows that render as depth-1: review group
		// rows and regular depth-1 children.
		isDepth1Like := func(s AgentSession) bool {
			return s.IsReviewGroup || isDepth1Child(s)
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
				if isTopLevel(sessions[k]) {
					return true
				}
			}
			return false
		}
		for i, s := range sessions {
			isD2Child := IsDepth2Session(s.Name) && !s.IsReviewGroup
			isReviewGrp := s.IsReviewGroup
			isD1Child := isDepth1Child(s) && groupHasTopLevel(i)
			var treePrefix string
			if isD2Child {
				// Depth-2 per-agent child (expanded group): "  │   ├── " or "  │   └── "
				// Look ahead within the same group (same ReviewRoundKey) to determine
				// if this is the last sibling.
				thisGroupKey := ReviewRoundKey(s.Name)
				isLastD2 := true
				for j := i + 1; j < len(sessions); j++ {
					next := sessions[j]
					if SessionRepo(next.Name) != SessionRepo(s.Name) {
						break
					}
					if IsDepth2Session(next.Name) && !next.IsReviewGroup && ReviewRoundKey(next.Name) == thisGroupKey {
						isLastD2 = false
						break
					}
					// Stop if we hit a non-depth-2 session or a different group.
					if !IsDepth2Session(next.Name) || next.IsReviewGroup {
						break
					}
				}
				if isLastD2 {
					treePrefix = "  │   └── "
				} else {
					treePrefix = "  │   ├── "
				}
			} else if isReviewGrp || isD1Child {
				// Review group row or regular depth-1 child: "  ├── " or "  └── "
				// Look ahead to determine if this is the last depth-1-like child in the group.
				isLastChild := true
				thisRepo := SessionRepo(s.Name)
				for j := i + 1; j < len(sessions); j++ {
					next := sessions[j]
					if SessionRepo(next.Name) != thisRepo {
						break
					}
					if isDepth1Like(next) || IsDepth2Session(next.Name) {
						isLastChild = false
						break
					}
				}
				if isLastChild {
					treePrefix = "  └── "
				} else {
					treePrefix = "  ├── "
				}
			}
			// For review group rows, use specialised renderer.
			if isReviewGrp {
				expanded := d.CollapsedGroups[s.Name]
				sb.WriteString(RenderReviewGroupRow(d, s, i, treePrefix, expanded, cursorActive, styleDim, styleFg, styleAgentType, sessionW, stateW))
			} else {
				sb.WriteString(RenderSessionRow(d, s, i, treePrefix, currentSession, cursorActive, styleDim, styleIns, styleDel, styleFg, styleAgentType, sessionW, agentTypeW, stateW, statW, statWCompact, titleW, modelWFull, harnessW, showType, showHarness, showModel, showStat))
			}
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
