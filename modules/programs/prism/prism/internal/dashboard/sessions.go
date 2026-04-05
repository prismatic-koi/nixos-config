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
	return AgentSession{
		Name:         s.SessionName,
		AgentState:   s.State,
		AgentPath:    s.Worktree,
		AgentTitle:   title,
		AgentName:    agentName,
		ModelID:      modelID,
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

// SortDisplayed sorts a session slice in-place to match the flat visual render
// order: alphabetical by repo name, @main first within each repo, then other
// branches alphabetically. Uses insertion sort (no stdlib import needed for
// small N).
func SortDisplayed(ss []AgentSession) {
	// sessionKey returns a sort key for a session: "repo\x00" for @main and
	// sessions without @, so they sort before any branch, or "repo\x01branch"
	// for other worktree branches.
	sessionKey := func(s AgentSession) string {
		repo := SessionRepo(s.Name)
		branch := SessionBranch(s.Name)
		if branch == s.Name || branch == "@main" {
			// No "@" (plain session) or @main — sorts first within repo.
			return repo + "\x00" + s.Name
		}
		return repo + "\x01" + branch
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
	sessionW, agentTypeW, stateW, statW, statWCompact, titleW, modelW int,
	showType, showModel, showStat bool,
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

	// treePrefixW is always 6 runes; pad treePrefix to that width using rune
	// count (not byte count) since tree connector chars are multi-byte in UTF-8.
	const treePrefixW = 6

	// Build the session display area (treePrefixW+sessionW total width):
	// - Top-level (treePrefix=""): full session name padded to treePrefixW+sessionW.
	// - Child (treePrefix non-empty): tree prefix (6 runes) + branch name padded to sessionW.
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
		// Child row: pad treePrefix to treePrefixW then branch name padded to sessionW.
		paddedPrefix := treePrefix
		runeCount := utf8.RuneCountInString(paddedPrefix)
		if runeCount < treePrefixW {
			paddedPrefix += strings.Repeat(" ", treePrefixW-runeCount)
		}
		branch := SessionBranch(s.Name)
		if sessionW == 0 {
			branch = ""
		} else if utf8.RuneCountInString(branch) > sessionW {
			branch = string([]rune(branch)[:sessionW-1]) + "…"
		}
		sessionArea = paddedPrefix + fmt.Sprintf("%-*s", sessionW, branch)
	}

	agentLabel := agentTypeLabel(s.AgentName)
	paddedAgentLabel := fmt.Sprintf("%-*s", agentTypeW, agentLabel)

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
	titleAvail := titleW
	if portLabel != "" && titleAvail >= 5 {
		reserved := utf8.RuneCountInString(portLabel) + 1 // +1 for the separating space
		titleAvail -= reserved
		if titleAvail < 0 {
			titleAvail = 0
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
