package dashboard

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// AgentSession is the dashboard's view of a session. It is derived from
// db.Status (the authoritative source) with client attachment count added from
// a tmux query.
//
// Review-round group rows are virtual: they do not correspond to a real tmux
// session but act as expand/collapse placeholders for a set of per-agent child
// sessions. IsReviewGroup is set to true for these rows; Name holds the full
// virtual session key (e.g. "nixos-config@feature~review-1") and AgentState
// holds the escalated state across the children.
type AgentSession struct {
	Name        string
	AgentState  string  // active | waiting | finished | compacting | error | idle | ""
	AgentPath   string  // worktree path — used for git diff stats
	AgentTitle  string  // current session title from agent_status.title
	AgentName   string  // coordinator | worker | "" — from agent_status.agent_name
	ModelID     string  // model identifier from agent_status.model_id
	Harness     string  // harness name from agent_status.harness, defaults to "opencode"
	HarnessPort *int    // allocated port from agent_status.harness_port, nil when unset
	ClientCount int     // tmux clients currently attached (best-effort, 0 on error)
	GroupID     *string // from agent_status.group_id; non-nil when session belongs to a review group
	// IsReviewGroup marks a virtual ~review-N group row (not a real session).
	// Selecting this row in the picker toggles expand/collapse rather than switching.
	IsReviewGroup bool
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
		Name:        s.SessionName,
		AgentState:  s.State,
		AgentPath:   s.Worktree,
		AgentTitle:  title,
		AgentName:   agentName,
		ModelID:     modelID,
		Harness:     harness,
		HarnessPort: s.HarnessPort,
		ClientCount: clientCounts[s.SessionName],
		GroupID:     s.GroupID,
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

// ReviewRoundKey returns the review-round group key for a depth-2 per-agent
// session, e.g. for "nixos-config@feature~review-1-review-goal" it returns
// "nixos-config@feature~review-1". Returns "" when the session is not a
// per-agent review session.
//
// A per-agent session has a Depth2Label of the form "~review-N-<agent>" where N
// is a positive integer and <agent> contains at least one more dash-separated
// component.
func ReviewRoundKey(name string) string {
	label := Depth2Label(name)
	if label == "" {
		return ""
	}
	// label is e.g. "~review-1-review-goal"
	// We want to find the part up to and including "~review-N".
	// Strip leading "~review-"
	const prefix = "~review-"
	if !strings.HasPrefix(label, prefix) {
		return ""
	}
	rest := label[len(prefix):] // e.g. "1-review-goal"
	dashIdx := strings.Index(rest, "-")
	if dashIdx <= 0 {
		// No dash found after N: label is "~review-N" (pure round row, not per-agent).
		return ""
	}
	// Ensure the portion before the dash is a positive integer.
	nStr := rest[:dashIdx]
	allDigits := true
	for _, r := range nStr {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if !allDigits || nStr == "" {
		return ""
	}
	// Construct the group key: repo@branch~review-N
	// The label "~review-N" is everything up to the first dash in rest, inclusive.
	roundLabel := prefix + nStr // e.g. "~review-1"
	// Reconstruct: strip the Depth2Label from the full name and replace with roundLabel.
	// name = "nixos-config@feature~review-1-review-goal"
	// Depth2Label = "~review-1-review-goal"
	// We want: "nixos-config@feature~review-1"
	branch := SessionBranch(name) // e.g. "@feature~review-1-review-goal"
	if len(branch) == 0 || branch[0] != '@' {
		return ""
	}
	inner := branch[1:] // e.g. "feature~review-1-review-goal"
	tildeIdx := strings.Index(inner, "~")
	if tildeIdx < 0 {
		return ""
	}
	parentBranchInner := inner[:tildeIdx]              // e.g. "feature"
	repo := SessionRepo(name)                          // e.g. "nixos-config"
	return repo + "@" + parentBranchInner + roundLabel // e.g. "nixos-config@feature~review-1"
}

// EscalatedState returns the highest-priority state across a slice of states.
// Priority order (highest first): waiting > error > active > compacting >
// interrupted > finished > idle/empty.
//
// compacting is a first-class AgentState (see internal/agent) and ranks
// alongside active — it means the agent is doing background work and is not
// idle. It sits just below active so that a round with one active agent and
// one compacting agent shows "active".
func EscalatedState(states []string) string {
	priority := func(s string) int {
		switch s {
		case "waiting":
			return 7
		case "error":
			return 6
		case "active":
			return 5
		case "compacting":
			return 4
		case "interrupted":
			return 3
		case "finished":
			return 2
		default:
			return 1 // idle or ""
		}
	}
	best := ""
	bestP := 0
	for _, s := range states {
		p := priority(s)
		if p > bestP {
			bestP = p
			best = s
		}
	}
	return best
}

// BuildDisplayRows transforms a sorted list of AgentSessions into the list of
// rows to display, inserting virtual review-round group rows and hiding children
// when groups are collapsed.
//
// collapsedGroups maps a group key (e.g. "nixos-config@feature~review-1") to
// false (collapsed) or true (expanded). Absent keys default to collapsed.
//
// When a filter is active (filterText != ""), any collapsed group whose child
// matches the filter is auto-expanded (but the auto-expand state is not written
// back to collapsedGroups — callers must update that separately via
// AutoExpandForFilter).
//
// Returns the display row slice, and a map of group keys that were auto-expanded
// due to filter matching (so the caller can persist the expansion).
func BuildDisplayRows(sessions []AgentSession, collapsedGroups map[string]bool, filterText string) ([]AgentSession, map[string]bool) {
	autoExpanded := map[string]bool{}

	// First pass: group per-agent sessions by their ReviewRoundKey, preserving order.
	// We scan through sessions sequentially (they are pre-sorted by SortDisplayed).
	// Non-per-agent sessions are emitted as-is.
	// For per-agent sessions we emit a virtual group row followed by (optionally) the children.

	// We process sessions in order and track the current group key.
	var out []AgentSession
	var currentGroupKey string
	var currentChildren []AgentSession

	flush := func() {
		if currentGroupKey == "" || len(currentChildren) == 0 {
			return
		}
		// Determine expansion state.
		expanded := collapsedGroups[currentGroupKey] // false or absent = collapsed
		// If filter is active, auto-expand when any child matches.
		if filterText != "" && !expanded {
			for _, ch := range currentChildren {
				if fuzzyMatch(ch.Name, filterText) {
					expanded = true
					autoExpanded[currentGroupKey] = true
					break
				}
			}
		}
		// Build escalated state.
		states := make([]string, len(currentChildren))
		for i, ch := range currentChildren {
			states[i] = ch.AgentState
		}
		esc := EscalatedState(states)
		// Emit the virtual group row.
		// Use the first child's path/harness for context (or empty).
		groupRow := AgentSession{
			Name:          currentGroupKey,
			AgentState:    esc,
			AgentPath:     currentChildren[0].AgentPath,
			IsReviewGroup: true,
		}
		out = append(out, groupRow)
		if expanded {
			out = append(out, currentChildren...)
		}
		currentGroupKey = ""
		currentChildren = nil
	}

	for _, s := range sessions {
		rk := ReviewRoundKey(s.Name)
		if rk == "" {
			// Not a per-agent review session: flush any pending group, emit directly.
			flush()
			out = append(out, s)
			continue
		}
		if rk != currentGroupKey {
			// New group: flush the old one.
			flush()
			currentGroupKey = rk
		}
		currentChildren = append(currentChildren, s)
	}
	flush()

	return out, autoExpanded
}

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
//
// This is a pass-through: every agent type (coordinator, worker, review-goal,
// review-code, review-security, review-qa, review-context, ac, retro, and any
// future type) renders as its own name. An empty input yields an empty label.
// See #849 for the session-uniformity rationale behind the flat-allowlist
// removal.
func agentTypeLabel(agentName string) string {
	return agentName
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
//
//   - Review-round group rows (IsReviewGroup=true): rendered as depth-1 children
//     showing "▶ ~review-N" or "▼ ~review-N" after a 6-rune tree prefix.
//     The label is the Depth2Label of the group key (e.g. "~review-1").
//     needed = d1PrefixLen + 2 (indicator+space) + len(label) - treePrefixW.
//
//   - Depth-2 child rows: the display content is just the Depth2Label
//     (e.g. "~review-1-review-goal"), rendered after a 10-rune prefix that
//     exactly fills treePrefixW, so sessionW ≥ len(label).
//
//   - All other rows (top-level and depth-1 children): the maximum of
//     (a) the full session name length, for cases where the row renders without
//     a tree prefix — filter-active mode (all rows are flat) and orphaned
//     branch sessions whose group has no @main companion — and
//     (b) d1PrefixLen + len(branch), for the normal tree-view rendering where
//     only the branch suffix is shown after a 6-rune connector prefix.
//
//     Using the maximum of both formulas ensures correctness in all modes.
//
// Constants are defined here as unexported values to avoid import cycles;
// the view's own sessionWCap and treePrefixW constants must stay in sync.
func SessionColumnWidth(sessions []AgentSession) int {
	const sessionWMin = 7  // len("session") — never truncate the column header
	const sessionWCap = 40 // must match sessionWCap in view.go
	const treePrefixW = 10 // must match treePrefixW in view.go
	const d1PrefixLen = 6  // "  ├── " or "  └── "
	const d2PrefixLen = 10 // "  │   ├── " or "  │   └── "
	const indicatorW = 2   // "▶ " or "▼ " for group rows

	maxW := 0
	for _, s := range sessions {
		var needed int
		if s.IsReviewGroup {
			// Virtual review-round group row: rendered as depth-1 child with
			// an expand/collapse indicator prepended to the label.
			// Display: "  ├── ▶ ~review-N" (or "▼ ~review-N").
			// runeCount = d1PrefixLen(6) + indicatorW(2) + len(label).
			// totalSessionW must be ≥ runeCount, so:
			// needed = d1PrefixLen + indicatorW + len(label) - treePrefixW
			label := Depth2Label(s.Name) // e.g. "~review-1"
			if label == "" {
				// Fallback: use the part after the last "@".
				label = SessionBranch(s.Name)
			}
			needed = d1PrefixLen + indicatorW + utf8.RuneCountInString(label) - treePrefixW
		} else if IsDepth2Session(s.Name) {
			// Depth-2: d2PrefixLen + len(label) - treePrefixW = len(label).
			// The depth-2 prefix is exactly treePrefixW runes, so the offset
			// cancels out and only the label length contributes.
			label := Depth2Label(s.Name)
			needed = d2PrefixLen + utf8.RuneCountInString(label) - treePrefixW
		} else {
			// Top-level and depth-1 children.
			//
			// We take the maximum of two formulas:
			//
			// (a) Full-name formula: len(name) - treePrefixW
			//     Covers every case where the row renders WITHOUT a tree prefix:
			//     top-level rows always, filter-active mode (all rows are flat),
			//     and orphaned depth-1 branches whose group has no @main companion.
			//
			// (b) Tree-mode formula: d1PrefixLen + len(branch) - treePrefixW
			//     Covers the normal grouped view where a depth-1 child renders as
			//     "  ├── @branch" — only the branch suffix fills sessionW.
			//     Only applies when the session CAN be a depth-1 child, i.e.
			//     it has an "@" in the name and the branch is not "@main".
			//
			// Using max(a, b) is safe: for top-level / @main rows, formula (a)
			// is always ≥ formula (b) since len(repo) ≥ 0 with the repo+@ prefix
			// larger than d1PrefixLen - treePrefixW = -4.  For depth-1 children
			// with very short repo names (< 6 chars), (b) > (a), so (b) dominates.
			nameLen := utf8.RuneCountInString(s.Name)
			needed = nameLen - treePrefixW // formula (a)

			branch := SessionBranch(s.Name)
			canBeD1Child := branch != s.Name && branch != "@main"
			if canBeD1Child {
				treeModeChild := d1PrefixLen + utf8.RuneCountInString(branch) - treePrefixW
				if treeModeChild > needed {
					needed = treeModeChild // formula (b) dominates
				}
			}
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
		if session.IsMetaSession(s.Name) {
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
	if s.HarnessPort != nil {
		portLabel = fmt.Sprintf(":%d", *s.HarnessPort)
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
