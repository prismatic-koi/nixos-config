// Package dashboard implements the prism live-agent-status dashboard TUI.
//
// The dashboard has two modes:
//
//   - Popup (--popup, C-w): a short-lived TUI spawned inside a tmux
//     display-popup frame. Pressing q/esc quits the process, closing the popup.
//
//   - Persistent (prefix+D): a long-running session (prism-dashboard) that
//     stays alive indefinitely. Pressing q/esc switches the viewer back to
//     their previous session; the TUI remains active for the next visitor.
//
// This file defines shared data types, message types, and the shared view
// function used by both modes.
package dashboard

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/prismatic-koi/prism/internal/git"
)

// ── message types ─────────────────────────────────────────────────────────────

// RefreshMsg is sent by the sentinel watcher goroutine to trigger a DB re-fetch.
type RefreshMsg struct{}

// DashStatusMsg carries a transient status/error message to display in the dashboard.
type DashStatusMsg string

// GitStatResult holds the outcome of a git.Stat call for a single worktree.
// Ok is false when the git command failed; in that case Stat is zero and the
// caller should render "?" rather than "—".
type GitStatResult struct {
	Stat git.DiffStat
	Ok   bool
}

// FocusClientMsg is sent by the persistent model's FocusMsg handler after
// querying which tmux client just attached to the session. It updates m.client
// so that Enter and q/esc operate on the correct client even when the model was
// initialised without a client (detached new-session startup).
// CurrentSession is the session that client was in before switching here; it is
// used to restore the "you are here" ◆ indicator for the visiting client.
type FocusClientMsg struct {
	Client         string
	CurrentSession string
}

// SessionsMsg carries a fresh sessions list and git stats from the DB poller.
type SessionsMsg struct {
	Sessions []AgentSession
	GitStats map[string]GitStatResult // keyed by AgentPath
}

// GhTickMsg is sent on the 60-second GitHub stats refresh timer.
type GhTickMsg time.Time

// CursorTimeoutMsg is sent when the cursor auto-hide timeout fires.
type CursorTimeoutMsg struct{}

// GithubStatsMsg carries the result of a GitHub PR fetch.
type GithubStatsMsg struct {
	OpenPRs int
	Err     bool // true = fetch failed, keep showing previous value
}

// ── timing constants ──────────────────────────────────────────────────────────

// CursorTimeout is how long the cursor bar stays visible after the last keypress
// in persistent (non-popup) dashboard mode.
const CursorTimeout = 3 * time.Second

// CursorTimeoutCmd returns a tea.Cmd that fires CursorTimeoutMsg after CursorTimeout.
func CursorTimeoutCmd() tea.Cmd {
	return tea.Tick(CursorTimeout, func(time.Time) tea.Msg {
		return CursorTimeoutMsg{}
	})
}

// ── shared data model ─────────────────────────────────────────────────────────

// Shared is the data shared between popup and persistent dashboard modes.
// It contains only data-layer state: sessions, filter, cursor position, display
// geometry, and GitHub stats. It deliberately has no mode-specific fields
// (no popup bool, no callerClient, no currentSession, no inDashSession).
type Shared struct {
	Sessions          []AgentSession
	GitStats          map[string]GitStatResult // keyed by AgentPath; populated on SessionsMsg
	Cursor            int
	CursorInitialised bool // true once we've snapped cursor to currentSession
	Width             int
	Height            int
	GhOpenPRs         int
	GhLoaded          bool // false = still fetching, show "…"
	Loading           bool // true = first fetch not yet returned; show skeleton
	// filter mode: activated by '/', cancelled by esc/ctrl+c
	FilterActive bool
	FilterText   string
	// Displayed is the filtered (or full) sessions list used by View/cursor.
	Displayed []AgentSession
	// StatusMsg is a transient error/info line shown at the bottom of the view.
	StatusMsg string
}

// ApplySessionsMsg updates shared state when a SessionsMsg arrives.
// snapSession is the session name to snap the cursor to on first load
// (pass the currentSession value from the mode-specific model).
// Returns the updated Shared and whether a snap was performed.
func (d Shared) ApplySessionsMsg(msg SessionsMsg, snapSession string) (Shared, bool) {
	d.Loading = false
	if msg.Sessions != nil {
		d.Sessions = msg.Sessions
	}
	if msg.GitStats != nil {
		d.GitStats = msg.GitStats
	}
	needsSnap := !d.CursorInitialised && !d.FilterActive
	if !d.CursorInitialised {
		d.CursorInitialised = true
	}
	d = RefilterShared(d)
	if needsSnap {
		for i, s := range d.Displayed {
			if s.Name == snapSession {
				d.Cursor = i
				break
			}
		}
	}
	return d, needsSnap
}

// HandleFilterKey handles a key press in filter mode. Returns the updated
// Shared and the tea.Cmd to run. The exitFilter bool (true) signals that the
// filter was confirmed with Enter; the caller should switch sessions using the
// current cursor position.
func (d Shared) HandleFilterKey(msg tea.KeyMsg) (Shared, bool /* exitFilter */, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return d, false, tea.Quit

	case "esc":
		d.FilterActive = false
		d.FilterText = ""
		d = RefilterShared(d)
		return d, false, nil

	case "enter":
		if len(d.Displayed) == 0 {
			return d, false, nil
		}
		d.FilterActive = false
		d.FilterText = ""
		return d, true, nil

	case "backspace", "ctrl+h":
		if len(d.FilterText) > 0 {
			runes := []rune(d.FilterText)
			d.FilterText = string(runes[:len(runes)-1])
			d = RefilterShared(d)
		}

	case "j", "down":
		if d.Cursor < len(d.Displayed)-1 {
			d.Cursor++
		}

	case "k", "up":
		if d.Cursor > 0 {
			d.Cursor--
		}

	default:
		if msg.Type == tea.KeyRunes {
			d.FilterText += msg.String()
			d = RefilterShared(d)
		}
	}
	return d, false, nil
}

// RefilterShared recomputes d.Displayed from d.Sessions applying the active
// fuzzy filter (if any). It also clamps the cursor so it never points out of
// bounds. It returns the updated Shared.
func RefilterShared(d Shared) Shared {
	if !d.FilterActive || d.FilterText == "" {
		d.Displayed = make([]AgentSession, len(d.Sessions))
		copy(d.Displayed, d.Sessions)
	} else {
		var out []AgentSession
		for _, s := range d.Sessions {
			if fuzzyMatch(s.Name, d.FilterText) {
				out = append(out, s)
			}
		}
		d.Displayed = out
	}
	// Sort displayed to match visual render order so d.Cursor indexes
	// correctly: alphabetical by repo, @main first within each repo,
	// then other branches alphabetically.
	SortDisplayed(d.Displayed)
	if d.Cursor >= len(d.Displayed) {
		d.Cursor = max(0, len(d.Displayed)-1)
	}
	return d
}

// fuzzyMatch returns true if all runes in pattern appear in s in order.
func fuzzyMatch(s, pattern string) bool {
	si := 0
	sRunes := []rune(s)
	for _, p := range pattern {
		found := false
		for si < len(sRunes) {
			if sRunes[si] == p {
				si++
				found = true
				break
			}
			si++
		}
		if !found {
			return false
		}
	}
	return true
}

// ── shared view ───────────────────────────────────────────────────────────────

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
	const minUsableWidth = 22 // fixedCore+1
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
	// Tree prefix for worktree child rows: "  ├── " or "  └── " (6 chars).
	// Top-level rows use no prefix; their name is padded to treePrefixW+sessionW.
	const treePrefixW = 6
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
			isChild := !isTopLevel(s.Name) && groupHasTopLevel(i)
			var treePrefix string
			if isChild {
				// Look ahead to determine if this is the last child in the group.
				isLastChild := true
				thisRepo := SessionRepo(s.Name)
				for j := i + 1; j < len(sessions); j++ {
					if SessionRepo(sessions[j].Name) != thisRepo {
						break
					}
					if !isTopLevel(sessions[j].Name) {
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

// ── db helper ─────────────────────────────────────────────────────────────────

// openDB is a package-level function pointer so tests can redirect it.
// It is initialised in db.go (a separate file in this package).
var openDB = defaultOpenDB

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
