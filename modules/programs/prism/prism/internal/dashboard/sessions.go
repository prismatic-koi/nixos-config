package dashboard

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// AgentSession is the dashboard's view of a session. It is derived from
// db.Status (the authoritative source) with client attachment count added from
// a tmux query.
type AgentSession struct {
	Name         string
	AgentState   string // active | waiting | finished | compacting | error | idle | ""
	AgentPath    string // worktree path — used for git diff stats
	AgentTitle   string // current session title from agent_status.title
	AgentName    string // coordinator | worker | "" — from agent_status.agent_name
	ModelID      string // model identifier from agent_status.model_id
	Harness      string // harness name from agent_status.harness, defaults to "opencode"
	OpencodePort *int   // allocated port from agent_status.opencode_port, nil when unset
	ClientCount  int    // tmux clients currently attached (best-effort, 0 on error)
}

// StatusToAgentSession converts a db.Status into an AgentSession.
// clientCounts is a map from session name → client count (from tmux).
func StatusToAgentSession(s db.Status, clientCounts map[string]int) AgentSession {
	title := ""
	if s.Title != nil {
		title = *s.Title
	}
	agentName := ""
	if s.AgentName != nil {
		agentName = *s.AgentName
	}
	modelID := ""
	if s.ModelID != nil {
		modelID = *s.ModelID
	}
	harness := "opencode"
	if s.Harness != nil && *s.Harness != "" {
		harness = *s.Harness
	}
	return AgentSession{
		Name:         s.SessionName,
		AgentState:   s.State,
		AgentPath:    s.Worktree,
		AgentTitle:   title,
		AgentName:    agentName,
		ModelID:      modelID,
		Harness:      harness,
		OpencodePort: s.OpencodePort,
		ClientCount:  clientCounts[s.SessionName],
	}
}

// SessionRepo extracts the repo prefix from a session name.
// Session names are of the form "repo@branch"; for names without "@" the whole
// name is used as the repo key (handles scratchpad, prism-dashboard, etc.).
func SessionRepo(name string) string {
	if idx := strings.Index(name, "@"); idx >= 0 {
		return name[:idx]
	}
	return name
}

// SessionBranch extracts the branch suffix from a session name (the part after
// the first "@"). Returns the full name when there is no "@".
func SessionBranch(name string) string {
	if idx := strings.Index(name, "@"); idx >= 0 {
		return name[idx:] // keeps the "@" prefix, e.g. "@main"
	}
	return name
}

// IsDepth2Session returns true when the session name contains a "~" in the
// branch component (after "@"), indicating it is a depth-2 review child.
// Example: "nixos-config@feature~review-1" → true.
func IsDepth2Session(name string) bool {
	branch := SessionBranch(name)
	// branch is "@feature~review-1" or the full name when no "@".
	// Strip the leading "@" for the tilde check.
	if len(branch) > 0 && branch[0] == '@' {
		branch = branch[1:]
	}
	return strings.Contains(branch, "~")
}

// Depth2ParentBranch returns the branch component of the parent session for a
// depth-2 review session. Given "nixos-config@feature~review-1" it returns
// "@feature". Returns "" when the session is not a depth-2 session.
func Depth2ParentBranch(name string) string {
	branch := SessionBranch(name)
	if len(branch) == 0 || branch[0] != '@' {
		return ""
	}
	inner := branch[1:] // strip leading "@"
	if idx := strings.Index(inner, "~"); idx >= 0 {
		return "@" + inner[:idx]
	}
	return ""
}

// Depth2Label returns the display label for a depth-2 review session.
// Given "nixos-config@feature~review-1" it returns "~review-1".
// Returns "" when not a depth-2 session.
func Depth2Label(name string) string {
	branch := SessionBranch(name)
	if len(branch) == 0 || branch[0] != '@' {
		return ""
	}
	inner := branch[1:] // strip leading "@"
	if idx := strings.Index(inner, "~"); idx >= 0 {
		return inner[idx:] // e.g. "~review-1"
	}
	return ""
}

// SortDisplayed sorts a session slice in-place to match the flat visual render
// order: alphabetical by repo name, @main first within each repo, then other
// branches alphabetically, with depth-2 review sessions (containing ~)
// sorted immediately after their parent branch. Uses insertion sort
// (no stdlib import needed for small N).
func SortDisplayed(ss []AgentSession) {
	// sessionKey returns a sort key for a session:
	//   - Plain sessions (no @): "repo\x00<name>"  — sorts first within repo
	//   - @main sessions:        "repo\x00<name>"  — sorts first within repo
	//   - Branch sessions:       "repo\x01<branch>\x00"  — sorts after @main
	//   - Depth-2 review:        "repo\x01<parent-branch>\x00~<label>"
	//     — sorts immediately after the parent branch
	sessionKey := func(s AgentSession) string {
		repo := SessionRepo(s.Name)
		branch := SessionBranch(s.Name)
		if branch == s.Name || branch == "@main" {
			// No "@" (plain session) or @main — sorts first within repo.
			return repo + "\x00" + s.Name
		}
		// Depth-2 session: sort directly after its parent branch.
		if IsDepth2Session(s.Name) {
			parentBranch := Depth2ParentBranch(s.Name)
			label := Depth2Label(s.Name)
			return repo + "\x01" + parentBranch + "\x00" + label
		}
		// Regular branch session.
		return repo + "\x01" + branch + "\x00"
	}
	for i := 1; i < len(ss); i++ {
		key := ss[i]
		keyStr := sessionKey(key)
		j := i - 1
		for j >= 0 && sessionKey(ss[j]) > keyStr {
			ss[j+1] = ss[j]
			j--
		}
		ss[j+1] = key
	}
}

// agentTypeLabel returns a display label for the agent_name value.
func agentTypeLabel(agentName string) string {
	switch agentName {
	case "coordinator":
		return "coordinator"
	case "worker":
		return "worker"
	default:
		return ""
	}
}

// SessionColumnWidth computes the session column width (sessionW) from the
// actual rendered widths of all displayed sessions. It scans the session names
// and their tree prefixes to find the minimum sessionW that accommodates every
// row without truncation, then clamps the result to [sessionWMin, sessionWCap].
//
// Width accounting (treePrefixW = 10 is the fixed prefix slot in the view):
//
//	totalSessionW = treePrefixW + sessionW
//
// Each row type contributes to sessionW as follows:
//   - Top-level rows:     sessionW ≥ len(name) − treePrefixW
//   - Depth-1 child rows: sessionW ≥ d1PrefixLen + len(branch) − treePrefixW
//     (d1PrefixLen = 6: "  ├── " or "  └── ")
//   - Depth-2 child rows: sessionW ≥ d2PrefixLen + len(label) − treePrefixW
//     (d2PrefixLen = 10: "  │   ├── " or "  │   └── ", so the offset is 0)
//
// Constants are defined here as unexported values to avoid import cycles;
// the view's own sessionWCap constant must stay in sync.
func SessionColumnWidth(sessions []AgentSession) int {
	const sessionWMin = 7  // len("session") — never truncate the column header
	const sessionWCap = 40 // must match sessionWCap in view.go
	const treePrefixW = 10 // must match treePrefixW in view.go
	const d1PrefixLen = 6  // "  ├── " or "  └── "
	const d2PrefixLen = 10 // "  │   ├── " or "  │   └── "

	// isTopLevel mirrors view.go's inline isTopLevel closure.
	isTopLevelSession := func(name string) bool {
		branch := SessionBranch(name)
		return branch == name || branch == "@main"
	}

	maxW := 0
	for _, s := range sessions {
		var needed int
		if IsDepth2Session(s.Name) {
			// Depth-2: d2PrefixLen + len(label) - treePrefixW = len(label)
			label := Depth2Label(s.Name)
			needed = d2PrefixLen + utf8.RuneCountInString(label) - treePrefixW
		} else if isTopLevelSession(s.Name) {
			// Top-level: len(name) - treePrefixW
			needed = utf8.RuneCountInString(s.Name) - treePrefixW
		} else {
			// Depth-1 child: d1PrefixLen + len(branch) - treePrefixW
			branch := SessionBranch(s.Name)
			needed = d1PrefixLen + utf8.RuneCountInString(branch) - treePrefixW
		}
		if needed > maxW {
			maxW = needed
		}
	}

	if maxW < sessionWMin {
		return sessionWMin
	}
	if maxW > sessionWCap {
		return sessionWCap
	}
	return maxW
}

// FilterAgentSessions removes internal sessions (scratchpad, prism-dashboard)
// from the slice.
func FilterAgentSessions(all []AgentSession) []AgentSession {
	var out []AgentSession
	for _, s := range all {
		if s.Name == "scratchpad" || s.Name == "prism-dashboard" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// TmuxClientCounts returns a map of session name → number of attached clients.
// Returns an empty map on error (attachment count is best-effort).
func TmuxClientCounts() map[string]int {
	counts := map[string]int{}
	out, err := tmux.Run("list-clients", "-F", "#{session_name}")
	if err != nil {
		return counts
	}
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name != "" {
			counts[name]++
		}
	}
	return counts
}

// RenderSessionRow renders a single session row (selected or unselected).
// treePrefix is the ASCII tree connector string (e.g. "  ├── ") for child rows;
// pass "" for top-level rows. Top-level rows display the full session name
// padded to treePrefixW+sessionW; child rows display the tree prefix plus the
// branch name padded to the same total width.
func RenderSessionRow(
	d Shared,
	s AgentSession,
	cursorIdx int,
	treePrefix string,
	currentSession string,
	cursorActive bool,
	styleDim, styleIns, styleDel, styleFg, styleAgentType lipgloss.Style,
	sessionW, agentTypeW, stateW, statW, statWCompact, titleW, modelW, harnessW int,
	showType, showHarness, showModel, showStat bool,
) string {
	isHere := s.Name == currentSession
	isSelected := cursorIdx == d.Cursor

	// dot: ◆ = you are here, ● = someone else attached, space = unattached
	var dot string
	switch {
	case isHere:
		dot = "◆ "
	case s.ClientCount > 0:
		dot = "● "
	default:
		dot = "  "
	}

	// treePrefixW is 10 runes (matches view.go constant). The total session
	// display area is always treePrefixW+sessionW runes wide to keep columns
	// aligned across all row depths.
	const treePrefixW = 10

	// Build the session display area (treePrefixW+sessionW total width):
	// - Top-level (treePrefix=""): full session name padded to treePrefixW+sessionW.
	// - Child (treePrefix non-empty): prefix used tight (as-is), branch field
	//   absorbs the spare width so the total remains treePrefixW+sessionW.
	var sessionArea string
	totalSessionW := treePrefixW + sessionW
	if treePrefix == "" {
		// Top-level row: full session name padded to totalSessionW.
		name := s.Name
		if utf8.RuneCountInString(name) > totalSessionW {
			name = string([]rune(name)[:totalSessionW-1]) + "…"
		}
		sessionArea = fmt.Sprintf("%-*s", totalSessionW, name)
	} else {
		// Child row: use prefix as-is; give spare width to the branch field so
		// the total (runeCount + branch field) equals totalSessionW.
		runeCount := utf8.RuneCountInString(treePrefix)
		// For depth-2 sessions, display only the ~review-N label rather than
		// the full @feature~review-N branch string.
		var branch string
		if label := Depth2Label(s.Name); label != "" {
			branch = label
		} else {
			branch = SessionBranch(s.Name)
		}
		branchW := totalSessionW - runeCount
		if branchW <= 0 {
			branch = ""
		} else if utf8.RuneCountInString(branch) > branchW {
			branch = string([]rune(branch)[:branchW-1]) + "…"
		}
		sessionArea = treePrefix + fmt.Sprintf("%-*s", totalSessionW-runeCount, branch)
	}

	agentLabel := agentTypeLabel(s.AgentName)
	paddedAgentLabel := fmt.Sprintf("%-*s", agentTypeW, agentLabel)

	harnessLabel := s.Harness
	paddedHarnessLabel := fmt.Sprintf("%-*s", harnessW, harnessLabel)

	modelLabel := s.ModelID
	if utf8.RuneCountInString(modelLabel) > modelW {
		modelLabel = string([]rune(modelLabel)[:modelW-1]) + "…"
	}
	paddedModelLabel := fmt.Sprintf("%-*s", modelW, modelLabel)

	result := d.GitStats[s.AgentPath]
	var statPlain string
	if s.AgentPath == "" || result.Ok && result.Stat.Files == 0 {
		statPlain = "—"
	} else if !result.Ok {
		statPlain = "?"
	} else if statW == statWCompact {
		statPlain = fmt.Sprintf("+%d -%d", result.Stat.Insertions, result.Stat.Deletions)
	} else {
		fileWord := "files"
		if result.Stat.Files == 1 {
			fileWord = "file "
		}
		statPlain = fmt.Sprintf("%d %s +%d -%d", result.Stat.Files, fileWord, result.Stat.Insertions, result.Stat.Deletions)
	}

	title := s.AgentTitle

	// portLabel is the compact port display shown when a port is allocated.
	// It is rendered at the start of the title column, before the title text.
	portLabel := ""
	if s.OpencodePort != nil {
		portLabel = fmt.Sprintf(":%d", *s.OpencodePort)
	}

	// When a portLabel is present, reserve its width (plus one space separator)
	// from the title budget so the title still fits within titleW.
	// Only show the port label if at least 5 characters remain for the title
	// after the reservation; otherwise suppress it to avoid overflow.
	titleAvail := titleW
	if portLabel != "" && titleAvail >= 5 {
		reserved := utf8.RuneCountInString(portLabel) + 1 // +1 for the separating space
		if titleAvail-reserved >= 5 {
			titleAvail -= reserved
		} else {
			// Not enough room for the port label and a usable title; suppress it.
			portLabel = ""
		}
	}
	if titleAvail >= 5 && utf8.RuneCountInString(title) > titleAvail {
		title = string([]rune(title)[:titleAvail-1]) + "…"
	}

	if isSelected && cursorActive {
		// Bar colour: state colour for active states, primary for idle/finished.
		barBg := lipgloss.Color(ColorPrimary)
		switch agent.AgentState(s.AgentState) {
		case agent.StateActive, agent.StateWaiting, agent.StateCompacting, agent.StateError, agent.StateInterrupted:
			if c, ok := stateStyle(s.AgentState).GetForeground().(lipgloss.Color); ok {
				barBg = c
			}
		}

		plain := fmt.Sprintf(" %s%s", dot, sessionArea)
		if showType {
			plain += fmt.Sprintf("  %-*s", agentTypeW, agentLabel)
		}
		if showHarness {
			plain += fmt.Sprintf("  %-*s", harnessW, harnessLabel)
		}
		if showModel {
			plain += fmt.Sprintf("  %-*s", modelW, modelLabel)
		}
		plain += fmt.Sprintf("  %-*s", stateW, stateLabel(s.AgentState))
		if showStat {
			plain += fmt.Sprintf("  %-*s", statW, statPlain)
		}
		if titleW >= 5 {
			titleSection := ""
			if portLabel != "" && title != "" {
				titleSection = portLabel + " " + title
			} else if portLabel != "" {
				titleSection = portLabel
			} else {
				titleSection = title
			}
			plain += fmt.Sprintf("  %s", titleSection)
		}
		row := lipgloss.NewStyle().
			Foreground(lipgloss.Color(ColorBg0)).
			Background(barBg).
			Bold(true).
			Width(d.Width).
			Render(plain)
		return row + "\n"
	}

	// Unselected: coloured state + coloured diff + dimmed agent type, normal fg for the rest.
	stateStr := lipgloss.NewStyle().
		Foreground(stateStyle(s.AgentState).GetForeground()).
		Render(fmt.Sprintf("%-*s", stateW, stateLabel(s.AgentState)))

	agentTypeStr := styleAgentType.Render(paddedAgentLabel)
	harnessStr := styleDim.Render(paddedHarnessLabel)
	modelStr := styleDim.Render(paddedModelLabel)

	var statStr string
	if showStat {
		if s.AgentPath == "" || result.Ok && result.Stat.Files == 0 {
			statStr = styleDim.Render(fmt.Sprintf("%-*s", statW, "—"))
		} else if !result.Ok {
			statStr = styleDim.Render(fmt.Sprintf("%-*s", statW, "?"))
		} else if statW == statWCompact {
			coloured := styleIns.Render(fmt.Sprintf("+%d", result.Stat.Insertions)) +
				" " + styleDel.Render(fmt.Sprintf("-%d", result.Stat.Deletions))
			rawLen := len(fmt.Sprintf("+%d -%d", result.Stat.Insertions, result.Stat.Deletions))
			pad := statW - rawLen
			if pad < 0 {
				pad = 0
			}
			statStr = coloured + strings.Repeat(" ", pad)
		} else {
			fileWord := "files"
			if result.Stat.Files == 1 {
				fileWord = "file "
			}
			plain := fmt.Sprintf("%d %s ", result.Stat.Files, fileWord)
			coloured := styleIns.Render(fmt.Sprintf("+%d", result.Stat.Insertions)) +
				" " + styleDel.Render(fmt.Sprintf("-%d", result.Stat.Deletions))
			rawStat := fmt.Sprintf("%d %s +%d -%d", result.Stat.Files, fileWord, result.Stat.Insertions, result.Stat.Deletions)
			pad := statW - len(rawStat)
			if pad < 0 {
				pad = 0
			}
			statStr = styleFg.Render(plain) + coloured + strings.Repeat(" ", pad)
		}
	}

	prefix := styleFg.Render(fmt.Sprintf(" %s%s", dot, sessionArea))
	row := prefix
	if showType {
		row += styleFg.Render("  ") + agentTypeStr
	}
	if showHarness {
		row += styleFg.Render("  ") + harnessStr
	}
	if showModel {
		row += styleFg.Render("  ") + modelStr
	}
	row += styleFg.Render("  ") + stateStr
	if showStat {
		row += styleFg.Render("  ") + statStr
	}
	if titleW >= 5 {
		if portLabel != "" && title != "" {
			row += styleDim.Render("  "+portLabel) + styleDim.Render(" "+title)
		} else if portLabel != "" {
			row += styleDim.Render("  " + portLabel)
		} else if title != "" {
			row += styleDim.Render("  " + title)
		}
	}
	return row + "\n"
}
