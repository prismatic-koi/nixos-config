// Package dashboard implements the iris live multi-session dashboard TUI
// (issue #1703).
//
// This is the iris analogue of `prism dashboard`. Where `iris tui` is a
// per-session-focused view (session list + event stream + prompt), the
// dashboard is the read-only "what's happening across my whole work" view:
// every daemon-known iris session in one screen, with state, role,
// worktree, uptime, and a recent-activity indicator.
//
// State source:
//   - sessions_snapshot (initial + periodic refresh) — authoritative list.
//   - session_spawned (push) — new session appears immediately on the
//     client that triggered the spawn; cross-client appearance happens
//     within one refresh tick.
//   - session_state (push, per subscribed session) — live state colour.
//   - session_event (push, per subscribed session) — drives the recent
//     activity column.
//
// The dashboard subscribes to every visible session so state and event
// frames flow without needing an explicit "subscribe to all" frame. Disposal
// of subscriptions for sessions that drop off the snapshot is handled in
// reconcileSessions.
//
// The dashboard never reads iris.db directly. All state access goes through
// the daemon socket, mirroring the discipline asserted in the iris TUI's
// TestNoDBImport.
package dashboard

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/tui"
)

// DashSession is the canonical persistent dashboard tmux session name.
// Mirrors prism's DashSession naming (s/prism/iris/) so the keybinding
// recipe is structurally identical.
const DashSession = "iris-dashboard"

// refreshInterval is how often the dashboard issues a sessions_list to
// reconcile against the daemon's authoritative session set. A 1s cadence
// satisfies the "row disappears within ~1 second" cleanup AC while still
// being cheap (sessions_snapshot is an in-memory dump).
const refreshInterval = 1 * time.Second

// Mode distinguishes popup (one-shot, q/esc quits) from persistent (long-lived
// session running `iris dashboard`). Both share the same Model — the only
// behavioural difference is what `q`/`esc` does.
type Mode int

const (
	// ModePopup is the ephemeral popup mode (spawned by the C-q binding via
	// `tmux display-popup -E`). q/esc quits the program and the popup
	// closes.
	ModePopup Mode = iota
	// ModePersistent is the long-lived mode that runs inside the
	// `iris-dashboard` tmux session. q/esc still quits — the tmux binding
	// recipe handles re-entry — but in practice users leave it open.
	ModePersistent
)

// sessionRow is the rendered, in-memory state for a single dashboard row.
type sessionRow struct {
	snap         iris.SessionSnapshot
	lastEventAt  time.Time
	lastEventTxt string
	subscribed   bool
}

// tickMsg is the periodic refresh trigger.
type tickMsg time.Time

// tick returns a Cmd that fires a tickMsg after refreshInterval.
func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Model is the bubbletea model for the iris dashboard.
//
// Exported fields are intentionally minimal — the dashboard surface is
// read-only, so test access is via the export_test.go shims in the same
// package, not via field exposure.
type Model struct {
	client *tui.DaemonClient
	mode   Mode

	// callerSession is the tmux session name passed via --caller-session.
	// When non-empty, the row whose Name matches is decorated with a
	// "you are here" indicator (◆ prefix), matching prism's treatment.
	callerSession string

	// Connection state.
	connected    bool
	connectError string
	reconnecting bool

	// Terminal dimensions.
	width  int
	height int

	// Sessions, keyed by name for O(1) reconcile and update.
	sessions map[string]*sessionRow
	// order is the rendered row order, recomputed on every reconcile.
	order []string

	// Theme, loaded from internal/config so the dashboard tracks the same
	// palette as the tmuxstatus segment (#1672) — a single source of truth
	// for "what colour is `active`".
	theme palette
}

// palette holds the resolved theme colours used by the dashboard. Loaded once
// at NewModel time from internal/config so tests can assert against fixed
// values without needing the config file on disk (config.Load returns
// gruvbox-dark defaults when no file is present).
type palette struct {
	primary    string // separator / accent
	secondary  string // dim
	purple     string // active / spawning
	yellow     string // waiting
	green      string // finished
	red        string // error
	blue       string // role accents
	foreground string
	bg0        string
}

func loadPalette() palette {
	cfg := config.Load()
	return palette{
		primary:    cfg.ColorPrimary,
		secondary:  cfg.ColorSecondary,
		purple:     cfg.ColorPurple,
		yellow:     cfg.ColorYellow,
		green:      cfg.ColorGreen,
		red:        cfg.ColorRed,
		blue:       cfg.ColorBlue,
		foreground: cfg.ColorForeground,
		bg0:        cfg.ColorBg0,
	}
}

// NewModel constructs a dashboard model. client is the (already-configured)
// DaemonClient — the caller is responsible for SetProgram/Connect plumbing.
// mode and callerSession come from the cobra flags.
func NewModel(client *tui.DaemonClient, mode Mode, callerSession string) Model {
	return Model{
		client:        client,
		mode:          mode,
		callerSession: callerSession,
		sessions:      make(map[string]*sessionRow),
		theme:         loadPalette(),
	}
}

// Init starts the periodic refresh tick. The DaemonClient already issues an
// initial sessions_list on connect, so no startup Cmd is needed beyond the
// tick.
func (m Model) Init() tea.Cmd {
	return tick()
}

// Update is the bubbletea reducer. Frame handling lives in handleDaemonFrame
// to keep the switch arm short.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tui.ConnectedMsg:
		m.connected = true
		m.reconnecting = false
		m.connectError = ""
		return m, nil

	case tui.DisconnectedMsg:
		m.connected = false
		if msg.Err != nil {
			m.connectError = msg.Err.Error()
		}
		m.reconnecting = true
		return m, nil

	case tui.DaemonFrame:
		return m.handleDaemonFrame(msg)

	case tickMsg:
		// Periodic refresh: ask the daemon for the authoritative session
		// list. The handler in handleDaemonFrame will reconcile (additions,
		// removals, subscription cleanup) when the snapshot arrives.
		cmd := func() tea.Msg {
			_ = m.client.SendSessionsList()
			return nil
		}
		return m, tea.Batch(cmd, tick())

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q", "esc":
		// Both modes quit on q/esc. In popup mode this closes the popup
		// frame (display-popup -E). In persistent mode the tmux binding
		// recipe (D key) re-enters by switching back to the dashboard
		// session, so a stray q simply exits the bubbletea program — the
		// session itself remains and can be re-entered.
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) handleDaemonFrame(msg tui.DaemonFrame) (tea.Model, tea.Cmd) {
	switch msg.RawType {

	case iris.DaemonFrameSessionsSnapshot:
		if msg.Snapshot == nil {
			return m, nil
		}
		return m.reconcileSessions(msg.Snapshot.Sessions)

	case iris.DaemonFrameSessionSpawned:
		if msg.Spawned == nil || msg.Spawned.Name == "" {
			return m, nil
		}
		// session_spawned is broadcast only to the spawning client today,
		// but handle it eagerly anyway — it lets the originating client
		// see the new row sub-tick rather than waiting up to refreshInterval.
		// The next sessions_snapshot will reconcile cross-client.
		var snap iris.SessionSnapshot
		if msg.Spawned.Session != nil {
			snap = *msg.Spawned.Session
		} else {
			snap = iris.SessionSnapshot{
				Name:       msg.Spawned.Name,
				InstanceID: msg.Spawned.InstanceID,
			}
		}
		return m.applySnapshotAdd(snap)

	case iris.DaemonFrameSessionState:
		if msg.State == nil {
			return m, nil
		}
		if row, ok := m.sessions[msg.State.SessionName]; ok {
			row.snap.State = msg.State.State
		}
		return m, nil

	case iris.DaemonFrameSessionEvent:
		if msg.Event == nil {
			return m, nil
		}
		if row, ok := m.sessions[msg.Event.SessionName]; ok {
			row.lastEventAt = time.Now()
			row.lastEventTxt = activityLabel(msg.Event.EventType)
		}
		return m, nil
	}

	return m, nil
}

// applySnapshotAdd inserts or updates a single row from a session_spawned
// frame and (re)subscribes to its event stream so live state/event frames
// flow. Order is recomputed via sortOrder so the new row appears in the
// canonical position.
func (m Model) applySnapshotAdd(snap iris.SessionSnapshot) (tea.Model, tea.Cmd) {
	if existing, ok := m.sessions[snap.Name]; ok {
		// Merge: preserve our liveness tracking but take the new snapshot.
		existing.snap = snap
		return m, nil
	}
	row := &sessionRow{snap: snap}
	m.sessions[snap.Name] = row
	m.order = sortOrder(m.sessions)
	if !row.subscribed {
		row.subscribed = true
		name := snap.Name
		return m, func() tea.Msg {
			_ = m.client.SendSessionSubscribe(name, 0)
			return nil
		}
	}
	return m, nil
}

// reconcileSessions takes an authoritative sessions_snapshot and brings the
// in-memory map into agreement: adds new rows (subscribing to each so live
// frames flow), removes rows that are no longer present (unsubscribing), and
// updates the snapshot fields on rows that persist. Returns a Cmd that
// dispatches all the subscribe/unsubscribe writes in parallel.
func (m Model) reconcileSessions(snaps []iris.SessionSnapshot) (tea.Model, tea.Cmd) {
	seen := make(map[string]bool, len(snaps))
	var subscribes []string
	for _, s := range snaps {
		seen[s.Name] = true
		if existing, ok := m.sessions[s.Name]; ok {
			// Preserve liveness tracking; refresh the snapshot fields.
			existing.snap = s
			continue
		}
		row := &sessionRow{snap: s, subscribed: true}
		m.sessions[s.Name] = row
		subscribes = append(subscribes, s.Name)
	}
	// Removals: any in-memory row whose name is no longer in the snapshot.
	var unsubscribes []string
	for name := range m.sessions {
		if !seen[name] {
			unsubscribes = append(unsubscribes, name)
			delete(m.sessions, name)
		}
	}
	m.order = sortOrder(m.sessions)

	if len(subscribes) == 0 && len(unsubscribes) == 0 {
		return m, nil
	}
	client := m.client
	cmd := func() tea.Msg {
		for _, n := range subscribes {
			_ = client.SendSessionSubscribe(n, 0)
		}
		for _, n := range unsubscribes {
			_ = client.SendSessionUnsubscribe(n)
		}
		return nil
	}
	return m, cmd
}

// sortOrder returns a stable ordering for the row keys: by StartedAt
// descending (newest first), with name as the tiebreaker. Empty StartedAt
// sorts last (treats unstarted/restored rows as oldest).
func sortOrder(sessions map[string]*sessionRow) []string {
	names := make([]string, 0, len(sessions))
	for n := range sessions {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := sessions[names[i]], sessions[names[j]]
		ti, _ := time.Parse(time.RFC3339, ri.snap.StartedAt)
		tj, _ := time.Parse(time.RFC3339, rj.snap.StartedAt)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return names[i] < names[j]
	})
	return names
}

// activityLabel turns an event type into a short activity blurb for the
// recent-activity column. Mirrors the verb shapes used by the prism
// dashboard's activity indicator.
func activityLabel(eventType string) string {
	switch eventType {
	case "msg_assistant", "msg_assistant_body":
		return "assistant turn"
	case "msg_user", "msg_user_body":
		return "user prompt"
	case "tool_call":
		return "tool call"
	case "tool_result":
		return "tool result"
	case "permission_ask":
		return "permission ask"
	case "permission_denied":
		return "permission denied"
	case "state_change":
		return "state change"
	case "":
		return ""
	default:
		return eventType
	}
}

// ── view ───────────────────────────────────────────────────────────────────

// View renders the dashboard. Layout, top to bottom:
//
//	header line     "iris dashboard — N sessions"
//	separator
//	column headers  "  SESSION  STATE  ROLE  WORKTREE  UPTIME  ACTIVITY"
//	rows…
//	footer hint     "q/esc to quit"
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}

	if !m.connected {
		return m.viewDisconnected()
	}

	var sb strings.Builder
	sb.WriteString(m.viewHeader())
	sb.WriteString("\n")
	sb.WriteString(m.styleDim().Render(strings.Repeat("─", max(0, m.width))))
	sb.WriteString("\n")

	if len(m.order) == 0 {
		sb.WriteString("\n")
		sb.WriteString(m.styleDim().Render("  no iris sessions yet — spawn one with `iris spawn --worktree <path>`."))
		sb.WriteString("\n")
	} else {
		sb.WriteString(m.viewColumnHeader())
		sb.WriteString("\n")
		for _, name := range m.order {
			row := m.sessions[name]
			sb.WriteString(m.viewRow(row))
			sb.WriteString("\n")
		}
	}

	// Footer.
	sb.WriteString("\n")
	footer := "q/esc to quit"
	if m.mode == ModePersistent {
		footer = "q/esc to quit  (re-enter via prefix+I)"
	}
	sb.WriteString(m.styleDim().Render("  " + footer))
	return sb.String()
}

// viewDisconnected renders the daemon-down overlay. The hint matches the
// "iris daemon not running" wording used by `iris sessions list` for
// consistent operator instructions.
func (m Model) viewDisconnected() string {
	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(m.styleError().Render("  ✗ iris daemon not connected"))
	sb.WriteString("\n\n")
	if m.connectError != "" {
		sb.WriteString(m.styleDim().Render("  " + m.connectError))
		sb.WriteString("\n")
	}
	if m.reconnecting {
		sb.WriteString(m.styleDim().Render("  Reconnecting…"))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(m.styleDim().Render("  Start the daemon: systemctl --user start iris"))
	sb.WriteString("\n")
	sb.WriteString(m.styleDim().Render("  Press q or ctrl+c to quit."))
	return sb.String()
}

// viewHeader renders the title bar with the session count.
func (m Model) viewHeader() string {
	title := fmt.Sprintf("  iris dashboard — %d session%s", len(m.order), plural(len(m.order)))
	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color(m.theme.primary)).
		Bold(true)
	return style.Render(title)
}

// Column widths. The dashboard is full-screen and every column is fixed-width
// so columns line up cleanly across rows (parity with prism's dashboard).
const (
	colSessionW  = 36
	colStateW    = 10
	colRoleW     = 12
	colWorktreeW = 24
	colUptimeW   = 8
	colActivityW = 28
)

func (m Model) viewColumnHeader() string {
	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color(m.theme.foreground)).
		Bold(true)
	header := fmt.Sprintf("  %-*s %-*s %-*s %-*s %-*s %-*s",
		colSessionW, "SESSION",
		colStateW, "STATE",
		colRoleW, "ROLE",
		colWorktreeW, "WORKTREE",
		colUptimeW, "UPTIME",
		colActivityW, "ACTIVITY",
	)
	return style.Render(header)
}

// viewRow renders a single dashboard row. Layout:
//
//	"<here> SESSION  STATE  ROLE  WORKTREE  UPTIME  ACTIVITY"
//
// The "here" prefix is "◆ " when row.snap.Name == m.callerSession (matches
// prism's "you are here" indicator); otherwise "  " (two spaces) for column
// alignment.
func (m Model) viewRow(row *sessionRow) string {
	here := "  "
	if m.callerSession != "" && row.snap.Name == m.callerSession {
		here = lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.yellow)).Bold(true).Render("◆ ")
	}

	name := truncCols(row.snap.Name, colSessionW)
	state := stateLabel(row.snap.State)
	statePadded := padCols(state, colStateW)
	stateColoured := m.styleForState(row.snap.State).Render(statePadded)

	role := truncCols(row.snap.Role, colRoleW)
	worktree := truncCols(filepath.Base(row.snap.Worktree), colWorktreeW)
	uptime := padCols(formatUptime(row.snap.StartedAt), colUptimeW)
	activity := truncCols(formatActivity(row.lastEventAt, row.lastEventTxt), colActivityW)

	// Assemble: name  state  role  worktree  uptime  activity
	return fmt.Sprintf("%s%-*s %s %-*s %-*s %s %-*s",
		here,
		colSessionW, name,
		stateColoured,
		colRoleW, role,
		colWorktreeW, worktree,
		uptime,
		colActivityW, activity,
	)
}

// stateLabel returns the human-readable state name. Unknown states fall
// through unchanged so operators can still see them in the dashboard.
func stateLabel(state string) string {
	switch state {
	case "active":
		return "active"
	case "spawning":
		return "spawning"
	case "waiting":
		return "waiting"
	case "idle":
		return "idle"
	case "finished":
		return "finished"
	case "error":
		return "error"
	case "":
		return "—"
	default:
		return state
	}
}

// styleForState returns a lipgloss style with the colour appropriate to the
// state. The mapping mirrors the tmuxstatus segment colours so the
// dashboard, the status-right segment, and (later) any other iris surfaces
// all use one palette per state — when a user themes "active" purple in
// ~/.config/prism/config.json, every iris surface respects it.
func (m Model) styleForState(state string) lipgloss.Style {
	colour := m.theme.secondary
	switch state {
	case "active", "spawning":
		colour = m.theme.purple
	case "waiting":
		colour = m.theme.yellow
	case "finished":
		colour = m.theme.green
	case "error":
		colour = m.theme.red
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(colour))
}

func (m Model) styleDim() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.secondary))
}

func (m Model) styleError() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.red)).Bold(true)
}

// formatUptime returns a short uptime string (≤ 7 chars) given an RFC3339
// timestamp. Empty input → "—".
func formatUptime(s string) string {
	if s == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "—"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// formatActivity renders the recent-activity column entry. Empty when no
// event has been observed for this row yet (avoids implying "0s ago").
func formatActivity(at time.Time, label string) string {
	if at.IsZero() || label == "" {
		return ""
	}
	return fmt.Sprintf("%s at %s", label, at.Format("15:04:05"))
}

// truncCols truncates s to at most w display columns, appending "…" if it
// had to cut. Display width is approximated as rune count (the dashboard
// renders ASCII session names, role tags, basenames, and HH:MM:SS — full
// CJK/grapheme width measurement isn't worth the extra dependency here).
func truncCols(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	runes := []rune(s)
	return string(runes[:w-1]) + "…"
}

// padCols pads s on the right with spaces to exactly w display columns.
// Truncates with truncCols if s is wider.
func padCols(s string, w int) string {
	if utf8.RuneCountInString(s) > w {
		return truncCols(s, w)
	}
	return s + strings.Repeat(" ", w-utf8.RuneCountInString(s))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
