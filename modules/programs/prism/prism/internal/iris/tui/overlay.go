package tui

// overlay.go \u2014 in-TUI modal overlays for the iris TUI (issue #1737).
//
// The bubbletea Model in model.go is a single-pane-with-overlay design. This
// file adds four modal overlays that render *on top of* the normal session-
// list + event-stream + prompt layout:
//
//   overlayPicker         Ctrl+F  \u2014 session picker, mirrors `iris switch`.
//   overlaySpawnWorktree  step 2  \u2014 worktree-path input for `[+] spawn new`.
//   overlaySpawnRole      step 3  \u2014 role input for `[+] spawn new`.
//   overlayDashboard      Ctrl+W  \u2014 multi-session dashboard view.
//   overlayHelp           ?       \u2014 keybindings help.
//
// Only one overlay is active at a time (Model.overlay). When an overlay is
// active, handleOverlayKey gets first refusal at every keystroke; Escape
// always dismisses to overlayNone (per the issue's "Escape closes overlay,
// does NOT quit the TUI" AC). q and ctrl+c still quit unconditionally.
//
// # Coexistence with tmux popup bindings
//
// When iris runs under tmux, the popup bindings on `C-f` and `C-w` (see
// modules/programs/prism/tmux.nix:212 and :229) intercept those keystrokes
// at the tmux layer and the in-TUI handlers never fire. Note that those
// two specific popups are owned by prism (`prism switch` and `prism
// dashboard`), not iris — the dedicated iris tmux popups during the
// coexistence window are on `prefix+i` (iris switch), `C-q` (iris
// dashboard popup), and `prefix+I` (persistent iris-dashboard). Either
// way, when iris runs standalone the tmux bindings are inert and the
// in-TUI overlays take over. No conflict arises because tmux is the
// outer event source whichever runtime owns the popup.
//
// # Why not import internal/iris/dashboard?
//
// internal/iris/dashboard already imports internal/iris/tui (for the
// DaemonClient and the shared frame types). Reusing the dashboard.Model
// from here would create an import cycle. The overlay dashboard
// reimplements a minimal multi-session table using the same data the
// Model already has in m.sessions \u2014 no extra daemon traffic, no extra
// dependency.

import (
	"fmt"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/iris"
)

// resolveAgentForOverlay returns a sensible default role for the given
// worktree path, using the shared iris.ResolveAgent heuristic (parent
// has .bare marker → "coordinator" for `main`, "worker" for everything
// else; otherwise ""). Wrapped here so the spawn-flow code reads
// straightforwardly without inline iris.ResolveAgent calls.
func resolveAgentForOverlay(worktree string) string {
	return iris.ResolveAgent(worktree, "")
}

// ---------------------------------------------------------------------------
// Picker overlay state
// ---------------------------------------------------------------------------

// pickerRowKind distinguishes the synthetic `[+] spawn new` row from rows
// backed by a real session. We keep the kind as a small int rather than a
// bool so future row types (e.g. "[\u21bb] reload") slot in cleanly.
type pickerRowKind int

const (
	pickerRowSpawn    pickerRowKind = iota // synthetic "[+] spawn new session" row
	pickerRowExisting                      // backed by a daemon-known session
)

// pickerOverlayRow is one displayable entry in the in-TUI picker. For
// existing sessions we cache the snapshot's name only \u2014 we look the full
// snapshot back up by index when selection happens, so we don't have to
// worry about stale data drift if the daemon emits a snapshot mid-overlay.
type pickerOverlayRow struct {
	kind        pickerRowKind
	sessionName string // empty when kind == pickerRowSpawn
	display     string // pre-rendered single-line display
	filterKey   string // lowercased display + name for fuzzy match
}

// pickerState is the picker overlay's mutable state. Lives inside Model;
// reset by closeOverlay when the overlay dismisses.
type pickerState struct {
	rows    []pickerOverlayRow
	matched []int // indices into rows that survive the current filter
	filter  string
	cursor  int // index into matched
}

// spawnState carries data between the two-step spawn flow (worktree then
// role). When the user submits the role prompt we send a session_spawn
// frame to the daemon with both values. Reset by closeOverlay.
type spawnState struct {
	worktree     []rune
	worktreeCur  int
	role         []rune
	roleCur      int
	// defaultRole holds the ResolveAgent-derived role suggestion shown as
	// the placeholder. We pre-populate the role input with this value when
	// transitioning from worktree to role, mirroring `iris switch`.
	defaultRole string
}

// ---------------------------------------------------------------------------
// Opening / closing overlays
// ---------------------------------------------------------------------------

// openPicker builds the picker rows from the current session list and
// switches the model into the overlayPicker state. The picker is a
// read-only snapshot of the session list at the moment the overlay opened
// \u2014 if the daemon pushes a new session_spawned frame while the picker is
// open, the row appears next time the picker is reopened. This keeps the
// fuzzy-match cursor stable.
func (m *Model) openPicker() {
	rows := make([]pickerOverlayRow, 0, len(m.sessions)+1)
	rows = append(rows, pickerOverlayRow{
		kind:      pickerRowSpawn,
		display:   "[+] spawn new session",
		filterKey: "[+] spawn new session",
	})
	for _, si := range m.sessions {
		s := si.snap
		// Compact columnar display: NAME  STATE  ROLE  WORKTREE
		// We keep the same column widths as model.go's left pane so the
		// overlay visually matches the underlying session list.
		display := fmt.Sprintf("%-32s  %-8s  %-10s  %s",
			truncate(s.Name, 32),
			truncate(s.State, 8),
			truncate(s.Role, 10),
			truncate(filepath.Base(s.Worktree), 32),
		)
		rows = append(rows, pickerOverlayRow{
			kind:        pickerRowExisting,
			sessionName: s.Name,
			display:     display,
			filterKey:   strings.ToLower(display + " " + s.Name),
		})
	}
	m.picker = pickerState{rows: rows}
	m.picker.refilter()
	m.overlay = overlayPicker
	m.errorMsg = ""
}

// closeOverlay returns the model to the normal layout, clearing per-overlay
// scratch state. Used by Escape from every overlay.
func (m *Model) closeOverlay() {
	m.overlay = overlayNone
	m.picker = pickerState{}
	m.spawn = spawnState{}
	m.errorMsg = ""
}

// refilter recomputes picker.matched against the current filter string. An
// empty filter matches every row; otherwise we use the same in-order fuzzy
// substring test that `iris switch` uses (every rune of pattern must appear
// in filterKey in order).
func (p *pickerState) refilter() {
	p.cursor = 0
	if p.filter == "" {
		p.matched = make([]int, len(p.rows))
		for i := range p.rows {
			p.matched[i] = i
		}
		return
	}
	pat := strings.ToLower(p.filter)
	out := make([]int, 0, len(p.rows))
	for i, r := range p.rows {
		if overlayFuzzyContains(r.filterKey, pat) {
			out = append(out, i)
		}
	}
	p.matched = out
}

// overlayFuzzyContains is the same in-order substring fuzzy matcher used
// by `iris switch` (cmd/iris/switch_tui.go::fuzzyContains). Duplicated
// here rather than imported because the cmd/iris package already imports
// internal/iris/tui (this package) \u2014 a back-import would cycle.
func overlayFuzzyContains(s, pattern string) bool {
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

// ---------------------------------------------------------------------------
// Overlay key dispatch
// ---------------------------------------------------------------------------

// handleOverlayKey is called by handleKey when an overlay is active. It
// dispatches per overlay kind and consumes every keystroke (the caller does
// not fall through to the main key handler).
//
// Quit semantics: ctrl+c quits unconditionally from every overlay, matching
// the user's reasonable expectation that a "panic" interrupt always works.
// `q` is reserved for typing in the spawn flow's text inputs and the
// picker's filter; only the dashboard and help overlays treat `q` as quit.
func (m Model) handleOverlayKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.overlay {
	case overlayPicker:
		return m.handlePickerKey(msg)
	case overlaySpawnWorktree:
		return m.handleSpawnWorktreeKey(msg)
	case overlaySpawnRole:
		return m.handleSpawnRoleKey(msg)
	case overlayDashboard:
		return m.handleDashboardKey(msg)
	case overlayHelp:
		return m.handleHelpKey(msg)
	}
	return m, nil
}

// handlePickerKey processes keystrokes while the picker overlay is open.
// Esc dismisses; Enter selects (subscribes to the chosen session, or enters
// the spawn flow); up/down navigates; printable runes extend the filter.
func (m Model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.closeOverlay()
		return m, nil
	case "enter":
		if len(m.picker.matched) == 0 {
			return m, nil
		}
		row := m.picker.rows[m.picker.matched[m.picker.cursor]]
		if row.kind == pickerRowSpawn {
			// Transition to the worktree-input step. Seed the input with
			// a reasonable default (the current working directory falls
			// out as "" here \u2014 RunFocused does not pre-populate it; the
			// `iris switch` popup path handles that via its tmux popup
			// `-d "#{pane_current_path}"`. Leaving the default empty
			// preserves the user's typed value when they re-enter the
			// flow after a previous spawn failure.).
			m.overlay = overlaySpawnWorktree
			m.spawn = spawnState{
				worktree:    []rune{},
				worktreeCur: 0,
			}
			return m, nil
		}
		// Existing session: switch the subscription and close the overlay.
		// We locate the index in m.sessions so we can re-use
		// switchToSelected's unsubscribe/subscribe semantics.
		target := row.sessionName
		idx := -1
		for i, si := range m.sessions {
			if si.snap.Name == target {
				idx = i
				break
			}
		}
		m.closeOverlay()
		if idx < 0 {
			// The session vanished between overlay open and Enter \u2014
			// snapshot drift. Show an error rather than silently dropping
			// the selection so the user notices.
			m.errorMsg = "session no longer present: " + target
			return m, nil
		}
		m.cursor = idx
		return m, m.switchToSelected()
	case "up", "ctrl+p":
		if m.picker.cursor > 0 {
			m.picker.cursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if m.picker.cursor < len(m.picker.matched)-1 {
			m.picker.cursor++
		}
		return m, nil
	case "backspace", "ctrl+h":
		if len(m.picker.filter) > 0 {
			rs := []rune(m.picker.filter)
			m.picker.filter = string(rs[:len(rs)-1])
			m.picker.refilter()
		}
		return m, nil
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			m.picker.filter += msg.String()
			m.picker.refilter()
		}
	}
	return m, nil
}

// handleSpawnWorktreeKey processes keystrokes during the worktree-input
// step of the spawn flow. Enter advances to the role step (with the
// ResolveAgent-derived default pre-filled); Esc cancels back to the picker.
func (m Model) handleSpawnWorktreeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		// Cancel back to the picker so the user can retry or escape entirely.
		m.overlay = overlayPicker
		m.spawn = spawnState{}
		return m, nil
	case "enter":
		wt := strings.TrimSpace(string(m.spawn.worktree))
		if wt == "" {
			// Reject empty worktree at the UI layer \u2014 the daemon would
			// also reject it, but the inline feedback is faster.
			m.errorMsg = "worktree is required"
			return m, nil
		}
		// Advance to the role step. Derive a sensible default role via the
		// shared ResolveAgent helper; the user can overtype it.
		def := resolveAgentForOverlay(wt)
		runes := []rune(def)
		m.overlay = overlaySpawnRole
		m.spawn.role = runes
		m.spawn.roleCur = len(runes)
		m.spawn.defaultRole = def
		m.errorMsg = ""
		return m, nil
	case "left", "ctrl+b":
		if m.spawn.worktreeCur > 0 {
			m.spawn.worktreeCur--
		}
	case "right":
		if m.spawn.worktreeCur < len(m.spawn.worktree) {
			m.spawn.worktreeCur++
		}
	case "home", "ctrl+a":
		m.spawn.worktreeCur = 0
	case "end", "ctrl+e":
		m.spawn.worktreeCur = len(m.spawn.worktree)
	case "backspace", "ctrl+h":
		if m.spawn.worktreeCur > 0 {
			m.spawn.worktree = append(m.spawn.worktree[:m.spawn.worktreeCur-1], m.spawn.worktree[m.spawn.worktreeCur:]...)
			m.spawn.worktreeCur--
		}
	case "delete", "ctrl+d":
		if m.spawn.worktreeCur < len(m.spawn.worktree) {
			m.spawn.worktree = append(m.spawn.worktree[:m.spawn.worktreeCur], m.spawn.worktree[m.spawn.worktreeCur+1:]...)
		}
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			ins := []rune(msg.String())
			m.spawn.worktree = append(m.spawn.worktree[:m.spawn.worktreeCur], append(ins, m.spawn.worktree[m.spawn.worktreeCur:]...)...)
			m.spawn.worktreeCur += len(ins)
		}
	}
	return m, nil
}

// handleSpawnRoleKey processes keystrokes during the role-input step.
// Enter sends the session_spawn frame; Esc steps back to the worktree
// input so the user can edit it without re-entering the picker.
func (m Model) handleSpawnRoleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.overlay = overlaySpawnWorktree
		return m, nil
	case "enter":
		role := strings.TrimSpace(string(m.spawn.role))
		if role == "" {
			role = m.spawn.defaultRole
		}
		if role == "" {
			role = "worker"
		}
		worktree := strings.TrimSpace(string(m.spawn.worktree))
		// Send the spawn frame and close the overlay. The daemon's reply
		// arrives as a session_spawned DaemonFrame which model.go already
		// handles by appending the new row to the session list.
		m.closeOverlay()
		return m, func() tea.Msg {
			_ = m.client.SendSessionSpawn(worktree, role)
			return nil
		}
	case "left", "ctrl+b":
		if m.spawn.roleCur > 0 {
			m.spawn.roleCur--
		}
	case "right":
		if m.spawn.roleCur < len(m.spawn.role) {
			m.spawn.roleCur++
		}
	case "home", "ctrl+a":
		m.spawn.roleCur = 0
	case "end", "ctrl+e":
		m.spawn.roleCur = len(m.spawn.role)
	case "backspace", "ctrl+h":
		if m.spawn.roleCur > 0 {
			m.spawn.role = append(m.spawn.role[:m.spawn.roleCur-1], m.spawn.role[m.spawn.roleCur:]...)
			m.spawn.roleCur--
		}
	case "delete", "ctrl+d":
		if m.spawn.roleCur < len(m.spawn.role) {
			m.spawn.role = append(m.spawn.role[:m.spawn.roleCur], m.spawn.role[m.spawn.roleCur+1:]...)
		}
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			ins := []rune(msg.String())
			m.spawn.role = append(m.spawn.role[:m.spawn.roleCur], append(ins, m.spawn.role[m.spawn.roleCur:]...)...)
			m.spawn.roleCur += len(ins)
		}
	}
	return m, nil
}

// handleDashboardKey: q/esc dismiss the dashboard overlay. This mirrors
// the existing iris dashboard popup binding's q/esc-to-quit semantics so
// muscle memory carries over between in-tmux and standalone use.
func (m Model) handleDashboardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.closeOverlay()
	}
	return m, nil
}

// handleHelpKey: any key dismisses the help overlay. We are deliberately
// permissive here \u2014 the help screen is a transient reference, not an
// interactive surface.
func (m Model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "?", "enter":
		m.closeOverlay()
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Overlay rendering
// ---------------------------------------------------------------------------

// viewOverlay dispatches to the per-overlay renderer. Called from View()
// when m.overlay != overlayNone.
func (m Model) viewOverlay() string {
	switch m.overlay {
	case overlayPicker:
		return m.viewPickerOverlay()
	case overlaySpawnWorktree:
		return m.viewSpawnInputOverlay("worktree:", m.spawn.worktree, m.spawn.worktreeCur, "")
	case overlaySpawnRole:
		return m.viewSpawnInputOverlay("role:", m.spawn.role, m.spawn.roleCur, "default: "+m.spawn.defaultRole)
	case overlayDashboard:
		return m.viewDashboardOverlay()
	case overlayHelp:
		return m.viewHelpOverlay()
	}
	return ""
}

// viewPickerOverlay renders the picker as a full-screen modal. Layout
// follows `iris switch` so users see the same visual whether they hit C-f
// inside tmux (popup) or standalone (overlay).
func (m Model) viewPickerOverlay() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(styleHeader.Render(" iris picker "))
	sb.WriteString(styleDim.Render(" \u2014 pick a session, or [+] spawn"))
	sb.WriteString("\n")
	sb.WriteString(styleHeader.Render(" >> "))
	sb.WriteString(m.picker.filter)
	sb.WriteString(styleDim.Render("\u2588"))
	sb.WriteString("\n")
	if m.errorMsg != "" {
		sb.WriteString(styleError.Render(" \u26a0 " + m.errorMsg))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// Window of visible matches.
	maxVisible := m.height - 8
	if maxVisible < 1 {
		maxVisible = 8
	}
	start := 0
	if m.picker.cursor >= maxVisible {
		start = m.picker.cursor - maxVisible + 1
	}
	end := start + maxVisible
	if end > len(m.picker.matched) {
		end = len(m.picker.matched)
	}

	for i := start; i < end; i++ {
		row := m.picker.rows[m.picker.matched[i]]
		line := " " + row.display
		switch {
		case i == m.picker.cursor:
			sb.WriteString(styleSelected.Render(line))
		case row.kind == pickerRowSpawn:
			sb.WriteString(styleGreen.Render(line))
		default:
			sb.WriteString(styleNormal.Render(line))
		}
		sb.WriteString("\n")
	}
	if len(m.picker.matched) == 0 {
		sb.WriteString(styleDim.Render(" no matches"))
		sb.WriteString("\n")
	}

	sb.WriteString("\n")
	sb.WriteString(styleDim.Render(" \u2191/\u2193 navigate  enter select  esc close  type to filter"))
	return sb.String()
}

// viewSpawnInputOverlay renders a single-line text input prompt used for
// the two-step spawn flow. label is the field name; runes/cur are the
// current input buffer and cursor position; sub is an optional dim hint
// line below the input (e.g. "default: worker").
func (m Model) viewSpawnInputOverlay(label string, runes []rune, cur int, sub string) string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(styleHeader.Render(" iris spawn \u2014 " + label))
	sb.WriteString("\n\n ")

	before := string(runes[:cur])
	after := string(runes[cur:])
	var caret string
	if len(after) > 0 {
		caretRunes := []rune(after)
		caret = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colBg0)).
			Background(lipgloss.Color(colPrimary)).
			Render(string(caretRunes[0]))
		after = string(caretRunes[1:])
	} else {
		caret = styleDim.Render("\u2588")
	}
	sb.WriteString(before)
	sb.WriteString(caret)
	sb.WriteString(after)
	sb.WriteString("\n")
	if sub != "" {
		sb.WriteString(styleDim.Render(" " + sub))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(styleDim.Render(" enter confirm  esc back"))
	return sb.String()
}

// viewDashboardOverlay renders a compact multi-session table from the
// data the Model already has. Columns: NAME  STATE  ROLE  WORKTREE.
// This is intentionally a thin reimplementation of the
// internal/iris/dashboard package's view (see overlay.go header for why we
// can't import it).
func (m Model) viewDashboardOverlay() string {
	var sb strings.Builder
	sb.WriteString("\n")
	header := fmt.Sprintf(" iris dashboard \u2014 %d session", len(m.sessions))
	if len(m.sessions) != 1 {
		header += "s"
	}
	sb.WriteString(styleHeader.Render(header))
	sb.WriteString("\n")
	sb.WriteString(styleDim.Render(strings.Repeat("\u2500", maxInt(m.width-2, 1))))
	sb.WriteString("\n")
	if len(m.sessions) == 0 {
		sb.WriteString("\n")
		sb.WriteString(styleDim.Render("  no iris sessions yet \u2014 use C-f \u2192 [+] spawn new"))
		sb.WriteString("\n")
	} else {
		col := fmt.Sprintf(" %-36s  %-8s  %-12s  %s", "SESSION", "STATE", "ROLE", "WORKTREE")
		sb.WriteString(styleHeader.Render(col))
		sb.WriteString("\n")
		for _, si := range m.sessions {
			s := si.snap
			line := fmt.Sprintf(" %-36s  %-8s  %-12s  %s",
				truncate(s.Name, 36),
				truncate(s.State, 8),
				truncate(s.Role, 12),
				truncate(filepath.Base(s.Worktree), 32),
			)
			sb.WriteString(styleNormal.Render(line))
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")
	sb.WriteString(styleDim.Render("  q/esc close"))
	return sb.String()
}

// viewHelpOverlay renders the keybindings reference. Order matches the
// issue's required + should-have tables for easy cross-reference.
func (m Model) viewHelpOverlay() string {
	rows := [][2]string{
		{"C-f", "open picker overlay (session list + [+] spawn new)"},
		{"C-w", "open dashboard overlay (multi-session view)"},
		{"?", "show this help overlay"},
		{"Escape", "close any open overlay (does not quit)"},
		{"Tab", "rotate focus (prompt \u2192 sessions \u2192 events)"},
		{"C-r", "force-refresh sessions list from daemon"},
		{"C-l", "clear / redraw the screen"},
		{"\u2191/\u2193", "navigate session list"},
		{"PgUp/PgDn", "scroll event stream"},
		{"Enter", "send prompt (or pick row when overlay open)"},
		{"q, C-c", "quit the TUI"},
	}
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(styleHeader.Render(" iris TUI \u2014 keybindings "))
	sb.WriteString("\n\n")
	for _, kv := range rows {
		key := lipgloss.NewStyle().
			Foreground(lipgloss.Color(colPrimary)).
			Bold(true).
			Render(fmt.Sprintf(" %-12s", kv[0]))
		sb.WriteString(key)
		sb.WriteString(styleNormal.Render("  " + kv[1]))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(styleDim.Render(" press any key to close"))
	return sb.String()
}

// maxInt returns the larger of a and b. Inline helper rather than pulling
// in `golang.org/x/exp/constraints` or the Go 1.21 builtin for one call.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
