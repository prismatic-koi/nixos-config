package main

// switch_tui.go — bubbletea picker model for `iris switch`.
//
// Two-stage flow:
//
//  1. switchPickerModel — fuzzy-filterable list of sessions plus the
//     synthetic "[+] spawn new session" top row. ↑/↓ navigate, type to
//     filter, Enter selects, Esc cancels.
//
//  2. switchInputModel — single-line text input used twice in the spawn
//     flow: once for the worktree path, once for the role. Enter confirms,
//     Esc cancels (returning the picker to stage 1).
//
// The picker runs the bubbletea program for the list, then if the user
// selected the spawn row it runs two more programs in sequence for the
// two inputs. Each `tea.NewProgram(...).Run()` blocks the calling
// goroutine, which is fine because `iris switch` is a short-lived CLI
// tool inside a tmux popup — no long-lived I/O happens here.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/iris"
)

// ---------------------------------------------------------------------------
// Result type returned from the picker
// ---------------------------------------------------------------------------

// pickerAction describes how runPicker ended.
type pickerAction int

const (
	pickerCancel   pickerAction = iota // Esc / Ctrl+C
	pickerExisting                     // User picked an existing session
	pickerSpawn                        // User picked [+] spawn new
)

// pickerResult carries the data the caller needs to act on the user's
// choice. Fields are zero-valued for actions that don't use them.
type pickerResult struct {
	// sessionName is set when action == pickerExisting; it's the session
	// to focus the TUI on.
	sessionName string

	// worktree, role are set when action == pickerSpawn; they're the
	// values the user typed in the two-prompt spawn flow.
	worktree string
	role     string
}

// ---------------------------------------------------------------------------
// Colours — match the iris TUI palette (model.go) for visual consistency.
// ---------------------------------------------------------------------------

const (
	switchColPrimary    = "#d79921" // yellow
	switchColSecondary  = "#928374" // grey
	switchColForeground = "#ebdbb2"
	switchColBg0        = "#282828"
	switchColRed        = "#cc241d"
	switchColGreen      = "#98971a"
	switchColBlue       = "#458588"
)

// ---------------------------------------------------------------------------
// Row model: one displayable entry in the picker
// ---------------------------------------------------------------------------

// pickerRow is one row in the picker list. `isSpawn` flags the synthetic
// top entry; for real sessions it holds the underlying snapshot.
type pickerRow struct {
	isSpawn bool
	snap    iris.SessionSnapshot
	// filterKey is the lowercased string used for fuzzy-matching.
	filterKey string
}

// display returns the single-line text used both for rendering and the
// fuzzy-match filter key.
//
// Columns (space-padded):
//
//	SESSION (12-char instance prefix)  STATE  ROLE  WORKTREE  UPTIME
//
// For the synthetic spawn row a friendly label is returned instead.
func (r pickerRow) display() string {
	if r.isSpawn {
		return "[+] spawn new session"
	}
	id := shortInstanceID(r.snap.InstanceID)
	state := padTrunc(r.snap.State, 8)
	role := padTrunc(r.snap.Role, 12)
	wt := padTrunc(worktreeBasename(r.snap.Worktree), 24)
	up := uptimeSince(r.snap.StartedAt)
	// Two-space separation so the columns are visually distinct even on
	// narrow popups.
	return fmt.Sprintf("%-12s  %-8s  %-12s  %-24s  %s", id, state, role, wt, up)
}

// buildRows constructs the canonical row order: the spawn row first,
// then one row per session in the daemon's order. The filterKey for
// each row is set to its lowercased display string + the session name
// so users can fuzzy-match on either the columnar prefix or the
// logical session name.
func buildRows(sessions []iris.SessionSnapshot) []pickerRow {
	rows := make([]pickerRow, 0, len(sessions)+1)
	rows = append(rows, pickerRow{isSpawn: true, filterKey: "[+] spawn new session"})
	for _, s := range sessions {
		r := pickerRow{snap: s}
		r.filterKey = strings.ToLower(r.display() + " " + s.Name)
		rows = append(rows, r)
	}
	return rows
}

// padTrunc pads s with spaces to exactly w columns, truncating with "…"
// if s is longer than w. Used to align the picker's columnar layout.
func padTrunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if len(s) > w {
		if w == 1 {
			return "…"
		}
		return s[:w-1] + "…"
	}
	return s + strings.Repeat(" ", w-len(s))
}

// ---------------------------------------------------------------------------
// switchPickerModel — bubbletea model for the row selector
// ---------------------------------------------------------------------------

// switchPickerModel is a fuzzy-filter list picker. It is intentionally
// simpler than prism's pickerModel: no project layout to negotiate, just
// a flat list of rows.
type switchPickerModel struct {
	rows     []pickerRow
	matched  []int // indices into rows that pass the filter
	filter   string
	cursor   int // index into matched
	width    int
	height   int
	chosen   *pickerRow // non-nil when the user pressed Enter
	cancelled bool      // true when the user pressed Esc/Ctrl+C
	errMsg   string     // inline error from a prior spawn attempt
}

func newSwitchPickerModel(rows []pickerRow) switchPickerModel {
	m := switchPickerModel{rows: rows}
	m.refilter()
	return m
}

// withError returns a copy of m with errMsg set. Used to re-enter the
// picker after a failed spawn so the user can retry or escape.
func (m switchPickerModel) withError(msg string) switchPickerModel {
	m.errMsg = msg
	m.chosen = nil
	m.cancelled = false
	return m
}

func (m switchPickerModel) Init() tea.Cmd { return nil }

func (m switchPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.cancelled = true
			return m, tea.Quit

		case "enter":
			if len(m.matched) > 0 {
				idx := m.matched[m.cursor]
				row := m.rows[idx]
				m.chosen = &row
			}
			return m, tea.Quit

		case "up", "ctrl+p", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "ctrl+n", "j":
			if m.cursor < len(m.matched)-1 {
				m.cursor++
			}
		case "backspace", "ctrl+h":
			if len(m.filter) > 0 {
				rs := []rune(m.filter)
				m.filter = string(rs[:len(rs)-1])
				m.refilter()
			}

		default:
			if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				m.filter += msg.String()
				m.refilter()
			}
		}
	}
	return m, nil
}

// refilter recomputes m.matched from m.filter using a fuzzy-substring
// match (every rune of filter must appear in order in filterKey).
func (m *switchPickerModel) refilter() {
	m.cursor = 0
	if m.filter == "" {
		m.matched = make([]int, len(m.rows))
		for i := range m.rows {
			m.matched[i] = i
		}
		return
	}
	pattern := strings.ToLower(m.filter)
	var out []int
	for i, r := range m.rows {
		if fuzzyContains(r.filterKey, pattern) {
			out = append(out, i)
		}
	}
	m.matched = out
}

// fuzzyContains returns true iff every rune in pattern appears in s in
// order (not necessarily contiguously). Case sensitivity is the caller's
// responsibility — both args should already be lowercased.
func fuzzyContains(s, pattern string) bool {
	si := 0
	sb := []byte(s)
	for _, r := range pattern {
		found := false
		for ; si < len(sb); si++ {
			if rune(sb[si]) == r {
				si++
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (m switchPickerModel) View() string {
	if m.width == 0 {
		return ""
	}

	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(switchColSecondary))
	stylePrompt := lipgloss.NewStyle().Foreground(lipgloss.Color(switchColPrimary)).Bold(true)
	styleHeader := lipgloss.NewStyle().Foreground(lipgloss.Color(switchColPrimary)).Bold(true)
	styleErr := lipgloss.NewStyle().Foreground(lipgloss.Color(switchColRed)).Bold(true)
	styleRowSelected := lipgloss.NewStyle().
		Foreground(lipgloss.Color(switchColBg0)).
		Background(lipgloss.Color(switchColPrimary)).
		Bold(true).
		Width(m.width)
	styleRowSpawn := lipgloss.NewStyle().
		Foreground(lipgloss.Color(switchColGreen)).
		Width(m.width)
	styleRowSpawnSelected := lipgloss.NewStyle().
		Foreground(lipgloss.Color(switchColBg0)).
		Background(lipgloss.Color(switchColGreen)).
		Bold(true).
		Width(m.width)
	styleRowNormal := lipgloss.NewStyle().
		Foreground(lipgloss.Color(switchColForeground)).
		Width(m.width)

	var sb strings.Builder

	// Banner + filter input.
	sb.WriteString("\n")
	sb.WriteString(stylePrompt.Render(" iris switch "))
	sb.WriteString(styleDim.Render(" — pick a session, or [+] spawn"))
	sb.WriteString("\n")
	sb.WriteString(stylePrompt.Render(" >> "))
	sb.WriteString(m.filter)
	sb.WriteString(styleDim.Render("█"))
	sb.WriteString("\n")

	// Inline error (sticky from previous spawn failure).
	if m.errMsg != "" {
		sb.WriteString(styleErr.Render(" ⚠ " + m.errMsg))
		sb.WriteString("\n")
	}

	// Column header — only meaningful when there are real sessions.
	hasSessions := false
	for _, r := range m.rows {
		if !r.isSpawn {
			hasSessions = true
			break
		}
	}
	if hasSessions {
		header := fmt.Sprintf(" %-12s  %-8s  %-12s  %-24s  %s",
			"SESSION", "STATE", "ROLE", "WORKTREE", "UPTIME")
		sb.WriteString(styleHeader.Render(header))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// Visible window of matched rows.
	maxVisible := m.height - 8
	if maxVisible < 1 {
		maxVisible = 8
	}
	start := 0
	if m.cursor >= maxVisible {
		start = m.cursor - maxVisible + 1
	}
	end := start + maxVisible
	if end > len(m.matched) {
		end = len(m.matched)
	}

	for i := start; i < end; i++ {
		row := m.rows[m.matched[i]]
		text := " " + row.display()
		var rendered string
		switch {
		case i == m.cursor && row.isSpawn:
			rendered = styleRowSpawnSelected.Render(text)
		case i == m.cursor:
			rendered = styleRowSelected.Render(text)
		case row.isSpawn:
			rendered = styleRowSpawn.Render(text)
		default:
			rendered = styleRowNormal.Render(text)
		}
		sb.WriteString(rendered + "\n")
	}

	if len(m.matched) == 0 {
		sb.WriteString(styleDim.Render(" no matches"))
		sb.WriteString("\n")
	}

	sb.WriteString("\n")
	sb.WriteString(styleDim.Render(" ↑/↓ navigate  enter select  esc cancel  type to filter"))
	sb.WriteString("\n")
	return sb.String()
}

// ---------------------------------------------------------------------------
// switchInputModel — single-line text input for the spawn flow
// ---------------------------------------------------------------------------

// switchInputModel is a minimal single-line text input. It is reused for
// the worktree prompt and the role prompt in the spawn flow.
type switchInputModel struct {
	prompt    string
	runes     []rune
	cursor    int
	done      bool
	cancelled bool
	width     int
}

func newSwitchInputModel(prompt, initial string) switchInputModel {
	rs := []rune(initial)
	return switchInputModel{
		prompt: prompt,
		runes:  rs,
		cursor: len(rs),
	}
}

func (m switchInputModel) value() string { return string(m.runes) }

func (m switchInputModel) Init() tea.Cmd { return nil }

func (m switchInputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.cancelled = true
			return m, tea.Quit
		case "enter":
			m.done = true
			return m, tea.Quit
		case "left", "ctrl+b":
			if m.cursor > 0 {
				m.cursor--
			}
		case "right", "ctrl+f":
			if m.cursor < len(m.runes) {
				m.cursor++
			}
		case "home", "ctrl+a":
			m.cursor = 0
		case "end", "ctrl+e":
			m.cursor = len(m.runes)
		case "backspace", "ctrl+h":
			if m.cursor > 0 {
				m.runes = append(m.runes[:m.cursor-1], m.runes[m.cursor:]...)
				m.cursor--
			}
		case "delete", "ctrl+d":
			if m.cursor < len(m.runes) {
				m.runes = append(m.runes[:m.cursor], m.runes[m.cursor+1:]...)
			}
		default:
			if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				ins := []rune(msg.String())
				m.runes = append(m.runes[:m.cursor], append(ins, m.runes[m.cursor:]...)...)
				m.cursor += len(ins)
			}
		}
	}
	return m, nil
}

func (m switchInputModel) View() string {
	styleDim := lipgloss.NewStyle().Foreground(lipgloss.Color(switchColSecondary))
	stylePrompt := lipgloss.NewStyle().Foreground(lipgloss.Color(switchColPrimary)).Bold(true)
	styleCaret := lipgloss.NewStyle().
		Foreground(lipgloss.Color(switchColBg0)).
		Background(lipgloss.Color(switchColPrimary))

	before := string(m.runes[:m.cursor])
	after := string(m.runes[m.cursor:])
	var caret string
	if len(after) > 0 {
		caretRunes := []rune(after)
		caret = styleCaret.Render(string(caretRunes[0]))
		after = string(caretRunes[1:])
	} else {
		caret = styleDim.Render("█")
	}

	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(stylePrompt.Render(" " + m.prompt))
	sb.WriteString("\n ")
	sb.WriteString(before)
	sb.WriteString(caret)
	sb.WriteString(after)
	sb.WriteString("\n\n")
	sb.WriteString(styleDim.Render(" enter confirm  esc cancel"))
	sb.WriteString("\n")
	return sb.String()
}

// promptSwitchInput is the convenience wrapper that runs a bubbletea
// program for a single switchInputModel and returns the typed value
// plus a "cancelled" bool. On cancellation the returned value is "".
func promptSwitchInput(prompt, initial string) (value string, cancelled bool) {
	m := newSwitchInputModel(prompt, initial)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithoutBracketedPaste())
	result, err := p.Run()
	if err != nil {
		return "", true
	}
	final, ok := result.(switchInputModel)
	if !ok {
		return "", true
	}
	if final.cancelled || !final.done {
		return "", true
	}
	return final.value(), false
}

// ---------------------------------------------------------------------------
// runPicker — orchestrate the two-stage UI
// ---------------------------------------------------------------------------

// runPickerWith runs the bubbletea picker (and the two-prompt spawn
// flow if the user chooses `[+] spawn new`). Returns the chosen result
// and the action taken.
//
// On a spawn submission, the caller is responsible for actually sending
// the session_spawn frame; runPickerWith only collects the worktree and
// role. If the daemon then returns an error, the caller should re-invoke
// runPickerWith with the error message in errMsg — it is shown as a
// sticky banner on the first render and cleared on navigation.
func runPickerWith(sessions []iris.SessionSnapshot, defaultWorktree, errMsg string) (pickerResult, pickerAction) {
	rows := buildRows(sessions)
	pm := newSwitchPickerModel(rows)
	if errMsg != "" {
		pm = pm.withError(errMsg)
	}

	p := tea.NewProgram(pm, tea.WithAltScreen())
	finalAny, err := p.Run()
	if err != nil {
		return pickerResult{}, pickerCancel
	}
	final, ok := finalAny.(switchPickerModel)
	if !ok {
		return pickerResult{}, pickerCancel
	}
	if final.cancelled || final.chosen == nil {
		return pickerResult{}, pickerCancel
	}

	if !final.chosen.isSpawn {
		return pickerResult{sessionName: final.chosen.snap.Name}, pickerExisting
	}

	// Spawn flow: prompt for worktree, then role.
	wt, cancelled := promptSwitchInput("worktree:", defaultWorktree)
	if cancelled {
		// Re-enter the picker so the user can try again or escape.
		return runPickerWith(sessions, defaultWorktree, "")
	}
	defaultRole := iris.ResolveAgent(wt, "")
	if defaultRole == "" {
		defaultRole = "worker"
	}
	role, cancelled := promptSwitchInput("role:", defaultRole)
	if cancelled {
		return runPickerWith(sessions, defaultWorktree, "")
	}
	if role == "" {
		role = defaultRole
	}
	return pickerResult{worktree: wt, role: role}, pickerSpawn
}
