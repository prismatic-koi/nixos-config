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
// This file defines shared data types, message types, and shared update logic.
// The shared view function (DashView) lives in view.go.
package dashboard

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ── message types ─────────────────────────────────────────────────────────────

// RefreshMsg is sent by the sentinel watcher goroutine to trigger a DB re-fetch.
type RefreshMsg struct{}

// DashStatusMsg carries a transient status/error message to display in the dashboard.
type DashStatusMsg string

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

// SessionsMsg carries a fresh sessions list from the DB poller.
type SessionsMsg struct {
	Sessions []AgentSession
}

// GhTickMsg is sent on the 60-second GitHub stats refresh timer.
type GhTickMsg time.Time

// SessionSyncTickMsg is sent on the 10-second session-list sync timer.
// Receiving it triggers a full FetchSessionsFromDB to ensure the persistent
// dashboard's session list converges with DB state (handles spawned or cleaned-up
// sessions that are not covered by push events).
type SessionSyncTickMsg time.Time

// CursorTimeoutMsg is sent when the cursor auto-hide timeout fires.
type CursorTimeoutMsg struct{}

// PushEventMsg is sent by the socket listener goroutine when a sidecar pushes
// a state-change event to the dashboard socket. It carries only the session
// name, new state, and (optionally) title — no DB round-trip required.
type PushEventMsg struct {
	Session string
	State   string
	Title   string
}

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
	// It includes virtual review-round group rows (IsReviewGroup=true) and
	// excludes per-agent children when their group is collapsed.
	Displayed []AgentSession
	// StatusMsg is a transient error/info line shown at the bottom of the view.
	StatusMsg string
	// CollapsedGroups tracks the expand/collapse state of review-round group rows.
	// Key: group key (e.g. "nixos-config@feature~review-1").
	// Value: true = expanded, false/absent = collapsed.
	// This map is NOT persisted across picker/dashboard invocations; it starts
	// empty (all groups collapsed) on every open.
	CollapsedGroups map[string]bool
}

// ApplySessionsMsg updates shared state when a SessionsMsg arrives.
// snapSession is the session name to snap the cursor to on first load
// (pass the currentSession value from the mode-specific model).
// Returns the updated Shared and whether a snap was performed.
//
// The nil-vs-empty distinction on msg.Sessions is the wire contract between
// FetchSessionsFromDB and this method:
//
//   - nil          → DB error (openDB or AllActiveStatus failed). Preserve
//     the last-known session list so the dashboard does not flash empty on
//     a transient failure.
//   - empty slice  → successful fetch returned zero non-meta sessions. Clear
//     the displayed list so the dashboard reflects reality (no ghost rows).
//   - non-empty    → normal update; replace the list.
//
// FilterAgentSessions is responsible for upholding the empty-slice half of
// the contract: it always returns a non-nil slice. See issue #1859.
func (d Shared) ApplySessionsMsg(msg SessionsMsg, snapSession string) (Shared, bool) {
	d.Loading = false
	if msg.Sessions != nil {
		d.Sessions = msg.Sessions
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
// fuzzy filter (if any), then builds the display rows (inserting virtual
// review-round group rows and applying collapse state). It also clamps the
// cursor so it never points out of bounds. It returns the updated Shared.
func RefilterShared(d Shared) Shared {
	// Ensure CollapsedGroups is initialised.
	if d.CollapsedGroups == nil {
		d.CollapsedGroups = map[string]bool{}
	}

	// Step 1: Apply fuzzy filter to real sessions.
	var filtered []AgentSession
	if !d.FilterActive || d.FilterText == "" {
		filtered = make([]AgentSession, len(d.Sessions))
		copy(filtered, d.Sessions)
	} else {
		for _, s := range d.Sessions {
			// In filter mode, match against the full session name.
			// Per-agent sessions are matched individually; the group row
			// is synthesised by BuildDisplayRows when needed.
			if fuzzyMatch(s.Name, d.FilterText) {
				filtered = append(filtered, s)
			}
		}
	}

	// Step 2: Sort to match visual render order.
	SortDisplayed(filtered)

	// Step 3: Build display rows.
	//
	// In filter mode with text: we display a flat list (no grouping — easier to
	// scan), but we still run BuildDisplayRows with the filter text so that
	// auto-expand state is persisted to CollapsedGroups. This ensures that when
	// the filter is cleared, groups that had matching children stay expanded.
	//
	// In non-filter mode (or empty filter): use the full grouped rendering with
	// virtual review-round group rows and collapse state applied.
	if d.FilterActive && d.FilterText != "" {
		// Run BuildDisplayRows to detect and persist auto-expanded groups,
		// but display the flat filtered list rather than the grouped rows.
		_, autoExpanded := BuildDisplayRows(d.Sessions, d.CollapsedGroups, d.FilterText)
		for k := range autoExpanded {
			d.CollapsedGroups[k] = true
		}
		d.Displayed = filtered
	} else {
		// Build display rows with collapse logic.
		filterText := ""
		if d.FilterActive {
			filterText = d.FilterText
		}
		rows, autoExpanded := BuildDisplayRows(filtered, d.CollapsedGroups, filterText)
		// Persist any auto-expanded groups so they remain expanded after the
		// filter text changes within the same session.
		for k := range autoExpanded {
			d.CollapsedGroups[k] = true
		}
		d.Displayed = rows
	}

	if d.Cursor >= len(d.Displayed) {
		d.Cursor = max(0, len(d.Displayed)-1)
	}
	return d
}

// ToggleReviewGroup flips the expand/collapse state for a review-round group
// row identified by its group key. Returns the updated Shared.
func ToggleReviewGroup(d Shared, groupKey string) Shared {
	if d.CollapsedGroups == nil {
		d.CollapsedGroups = map[string]bool{}
	}
	d.CollapsedGroups[groupKey] = !d.CollapsedGroups[groupKey]
	return RefilterShared(d)
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

// applyPushEvent updates d in response to a PushEventMsg: it finds the session
// named msg.Session in d.Sessions and updates its AgentState (and AgentTitle if
// non-empty) in-place. If the session is not found, d is returned unchanged
// (no crash). After updating, RefilterShared is called to propagate the change
// to Displayed.
func applyPushEvent(d Shared, msg PushEventMsg) Shared {
	found := false
	for i, s := range d.Sessions {
		if s.Name == msg.Session {
			d.Sessions[i].AgentState = msg.State
			if msg.Title != "" {
				d.Sessions[i].AgentTitle = msg.Title
			}
			found = true
			break
		}
	}
	if !found {
		// Unknown session — ignore gracefully.
		return d
	}
	return RefilterShared(d)
}

// ── db helper ─────────────────────────────────────────────────────────────────

// openDB is a package-level function pointer so tests can redirect it.
// defaultOpenDB is defined in db.go (a separate file in this package).
var openDB = defaultOpenDB

// ── tmux client helper ────────────────────────────────────────────────────────

// CurrentClientFunc is a package-level function pointer that returns the name
// of the tmux client that most recently interacted with the current pane. It
// defaults to tmux.CurrentClient but can be overridden in tests (where no real
// tmux client is attached to the test server's pane) to inject a known client.
var CurrentClientFunc = tmux.CurrentClient

// resolveDashClientFunc resolves the tmux client currently viewing the
// persistent dashboard session. It defaults to tmux.ClientForSession(DashSession),
// which is sound from the pane-resident dashboard process - unlike
// CurrentClientFunc (display-message), which can leak a client attached to a
// different session or return an empty string. It is a package var so tests can
// inject a known client. See issue #2522 (defect 2).
var resolveDashClientFunc = func() (string, error) { return tmux.ClientForSession(DashSession) }

// switchSessionFunc ensures the named session exists and switches the given
// client to it. It defaults to ensureSessionAndSwitch and is a package var so
// tests can intercept the switch without invoking real tmux. See issue #2522.
var switchSessionFunc = ensureSessionAndSwitch
