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
//
// When s.ReviewChildSummaries is non-empty, the row also renders per-agent
// verdict labels ("code:P  context:·  goal:P  qa:◌  sec:F") after the state
// column, in alphabetical order by short label. If the available width budget
// cannot accommodate the labels, they are suppressed entirely and the row
// falls back to session + state only. See #1802.
// profileW is the rendered width of the profile column (0 = hidden by the
// narrow-terminal fallback, see DashView). Review group rows are virtual
// (not a real spawned session), so the profile cell, when shown, is always
// blank — the profile tier belongs to the per-agent child sessions, which
// render via RenderSessionRow and show their own profile there (issue #2640).
func RenderReviewGroupRow(
	d Shared,
	s AgentSession,
	cursorIdx int,
	treePrefix string,
	expanded bool,
	cursorActive bool,
	styleDim, styleFg lipgloss.Style,
	sessionW, stateW, profileW int,
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

	// Compute the per-agent verdict labels if per-child summaries are
	// populated. The width budget is whatever is left of d.Width after the
	// leading session area + state column + the fixed gaps. We deliberately
	// budget only against d.Width, not the other columns rendered by leaf
	// rows — the group row's trailing area is independent of the leaf rows'
	// type / model / harness / stat / title columns; it just consumes
	// whatever horizontal space remains.
	var labelsStr string
	var summaryRenderMode summaryMode
	if len(s.ReviewChildSummaries) > 0 && d.Width > 0 {
		// Bytes consumed so far in the plain (un-styled) layout:
		//   leading space (1) + dot (2) + sessionArea (totalSessionW) +
		//   gap (2) + state label (stateW) + gap (2) = consumed.
		profileGap := 0
		if profileW > 0 {
			profileGap = profileW + 2
		}
		consumed := 1 + 2 + totalSessionW + 2 + stateW + profileGap + 2
		budget := d.Width - consumed
		if budget < 0 {
			budget = 0
		}
		labelsStr, _, summaryRenderMode = RenderReviewSummary(s.ReviewChildSummaries, budget)
	}

	if isSelected && cursorActive {
		barBg := lipgloss.Color(ColorSecondary)
		if c, ok := stateStyle(s.AgentState).GetForeground().(lipgloss.Color); ok {
			barBg = c
		}
		plain := fmt.Sprintf(" %s%s  %-*s", dot, sessionArea, stateW, stateLabel(s.AgentState))
		if profileW > 0 {
			plain += fmt.Sprintf("  %-*s", profileW, "")
		}
		if labelsStr != "" {
			// The selected-row bar uses lipgloss.Width(...).Render(plain),
			// which would strip per-letter colours under the bar. Render the
			// trailing segment as plain text (no per-letter colour) when the
			// row is selected so the bar bg stays uniform and the letters
			// stay readable in foreground/background contrast. The mode
			// chosen by RenderReviewSummary is reused here so the plain
			// mirror has the same width footprint as the coloured form.
			plain += "  " + plainSummaryForBudget(s.ReviewChildSummaries, summaryRenderMode)
		}
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
	if profileW > 0 {
		row += styleFg.Render(fmt.Sprintf("  %-*s", profileW, ""))
	}
	if labelsStr != "" {
		row += styleFg.Render("  ") + labelsStr
	}
	return row + "\n"
}

// plainSummaryForBudget renders the per-agent trailing segment as a plain
// (unstyled) string for use inside the selected-row bar, where lipgloss's
// Width().Render would interact poorly with per-letter colours. The `mode`
// argument selects which rendering tier to emit — it must match the mode
// chosen by RenderReviewSummary so the plain mirror has the same width
// footprint as the coloured form on the unselected render. See #1812.
func plainSummaryForBudget(summaries []ReviewChildSummary, mode summaryMode) string {
	if len(summaries) == 0 || mode == summaryNone {
		return ""
	}
	var b strings.Builder
	for i, sm := range summaries {
		if i > 0 {
			b.WriteString("  ")
		}
		if mode == summaryFull {
			b.WriteString(sm.AgentShortName)
			b.WriteString(":")
		}
		b.WriteString(letterForVerdict(sm.Verdict))
	}
	return b.String()
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
	styleFg := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorForeground))
	stylePrompt := lipgloss.NewStyle().Foreground(lipgloss.Color(ColorPrimary)).Bold(true)

	// ── column widths ────────────────────────────────────────────────────────
	// Tree prefix slot for child rows.
	//   Depth-1 prefixes: "  ├── " or "  └── " (6 runes; branch field absorbs spare 4)
	//   Depth-2 prefixes: "  │   ├── " or "  │   └── " (exactly treePrefixW=10 runes)
	// treePrefixW=10 sets the total reserved width (prefix + branch) so column
	// alignment is preserved across all depths.
	const treePrefixW = 10
	const stateW = 10
	const dotW = 2

	// profileW is the width of the profile-tier column, sized to the longest
	// valid tier name ("standard", 8 runes). Unlike stateW, profileW is NOT
	// part of fixedCore: it competes with titleW for the leftover width after
	// session+state, and is dropped first (see showProfile below) so that a
	// narrow terminal degrades to session+state only, same as it did before
	// this column existed (issue #2640) — the fallback the title column
	// already relied on. stateW stays in fixedCore and is never truncated.
	const profileW = 8

	// fixedCore is the non-negotiable fixed overhead: leading space + dot +
	// treePrefixW + gap-before-state + stateW.
	const fixedCore = 1 + dotW + treePrefixW + 2 + stateW

	// Derive session column width from the longest rendered session name
	// across all displayed entries, clamped to [7, 40].
	sessionW := SessionColumnWidth(d.Displayed)

	// Layout: " " + dot(2) + treePrefixW + sessionW + 2 + stateW + 2 + [profileW + 2] + titleW.
	// available is the width left after session+state; profile takes a fixed
	// slice of it (dropped when there isn't room), and titleW absorbs
	// whatever remains — so the profile column takes its width from title,
	// not from the fixed core (see the design-tension note in the PR body).
	available := d.Width - fixedCore - sessionW - 2
	if available < 0 {
		available = 0
	}
	showProfile := available >= profileW
	titleW := available
	if showProfile {
		titleW = available - profileW - 2
	}
	if titleW < 0 {
		titleW = 0
	}
	renderedProfileW := 0
	if showProfile {
		renderedProfileW = profileW
	}

	var sb strings.Builder

	// ── header: stats left, art right ───────────────────────────────────────
	sb.WriteString(RenderHeader(d, styleDim))

	// Rainbow separator between header and column headers.
	sb.WriteString(RainbowLineWidth(strings.Repeat("─", d.Width), d.Width))
	sb.WriteString("\n")

	// Column header: top-level rows have no tree prefix gap, so session column
	// spans treePrefixW+sessionW total (same total width as all data rows).
	header := fmt.Sprintf(" %-*s%-*s",
		dotW, "",
		treePrefixW+sessionW, "session",
	)
	header += fmt.Sprintf("  %-*s", stateW, "state")
	if showProfile {
		header += fmt.Sprintf("  %-*s", profileW, "profile")
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
			sb.WriteString(RenderSessionRow(d, s, i, "" /*treePrefix*/, currentSession, cursorActive, styleDim, styleFg, sessionW, stateW, renderedProfileW, titleW))
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
				sb.WriteString(RenderReviewGroupRow(d, s, i, treePrefix, expanded, cursorActive, styleDim, styleFg, sessionW, stateW, renderedProfileW))
			} else {
				sb.WriteString(RenderSessionRow(d, s, i, treePrefix, currentSession, cursorActive, styleDim, styleFg, sessionW, stateW, renderedProfileW, titleW))
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
