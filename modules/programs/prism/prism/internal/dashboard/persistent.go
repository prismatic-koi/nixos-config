package dashboard

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ── persistent dashboard model ────────────────────────────────────────────────

// PersistentModel is the Bubble Tea model for the long-running persistent
// dashboard session (`prism-dashboard`).
//
// Lifecycle: the process runs indefinitely. Pressing q/esc switches the current
// client back to its previous session (switch-client -l) and returns to passive
// watch mode. The session is kept alive by the tmux session itself, not by a
// restart loop.
//
// Focus management: Client is the only client identity needed — it is the
// client currently viewing the persistent dashboard. q/esc and Enter both
// operate on Client (no caller state required).
type PersistentModel struct {
	Shared
	Client         string // tmux client of the process (from CurrentClient())
	CurrentSession string // caller's session (from --caller-session flag; for "you are here")
	CursorActive   bool   // false = passive watch; true = selection mode
}

// NewPersistentModel constructs a new persistent dashboard model.
func NewPersistentModel(client, callerSession string) PersistentModel {
	m := PersistentModel{
		Shared: Shared{
			Loading: true,
		},
		Client:         client,
		CurrentSession: callerSession,
		CursorActive:   false, // persistent starts in passive watch mode
	}
	m.Displayed = m.Sessions // empty at init; populated on first SessionsMsg
	return m
}

func (m PersistentModel) Init() tea.Cmd {
	// FetchSessionsFromDB populates the initial session list.
	// SessionSyncTick schedules a 10-second periodic full re-fetch from the DB so
	// that sessions spawned or cleaned up after Init are reflected without manual
	// refresh. This complements push events, which only update existing sessions.
	return tea.Batch(FetchSessionsFromDB, FetchGitHubStats, GhTick(), SessionSyncTick())
}

func (m PersistentModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height

	case RefreshMsg:
		return m, FetchSessionsFromDB

	case SessionSyncTickMsg:
		// 10-second full session-list re-sync from the DB. This catches sessions
		// that were spawned or cleaned up since the last full refresh — changes
		// that push events cannot communicate (they only update existing sessions).
		// On success the resulting SessionsMsg replaces the session list exactly
		// as a manual refresh (RefreshMsg) would. On DB error FetchSessionsFromDB
		// returns an empty SessionsMsg; ApplySessionsMsg guards against clearing
		// the list when Sessions is nil, so the last-known state is retained.
		return m, tea.Batch(FetchSessionsFromDB, SessionSyncTick())

	case PushEventMsg:
		// A sidecar pushed a state-change event directly to the dashboard socket.
		// Update the named session in-memory and re-render immediately — no DB
		// round-trip, no git stat wait.
		m.Shared = applyPushEvent(m.Shared, msg)
		return m, nil

	case DashStatusMsg:
		m.StatusMsg = string(msg)
		return m, nil

	case GhTickMsg:
		return m, tea.Batch(FetchGitHubStats, GhTick())

	case GithubStatsMsg:
		if !msg.Err {
			m.GhOpenPRs = msg.OpenPRs
		}
		m.GhLoaded = true

	case CursorTimeoutMsg:
		// Do not deactivate the cursor while the filter is open — the selection
		// bar must stay visible for the entire filter session.
		if !m.FilterActive {
			m.CursorActive = false
		}

	case tea.BlurMsg:
		// Do not deactivate the cursor while the filter is open — the user
		// must be able to see which session Enter will select at all times.
		if !m.FilterActive {
			m.CursorActive = false
		}
		// Client detached — clear Client and CurrentSession unconditionally,
		// even if FilterActive is true. The detached client is truly gone: any
		// pending action that uses Client will hit the belt-and-suspenders
		// tmux.CurrentClient() inline fallback and still work correctly.
		// Clearing eagerly avoids using a stale client identity for a new visitor.
		m.Client = ""
		m.CurrentSession = ""

	case tea.FocusMsg:
		// A tmux client just attached to this session. Query the client name
		// asynchronously so Enter/q/esc have the right target. Clear any stale
		// client and session state from the previous visitor while the async
		// query is in-flight; they will be repopulated by FocusClientMsg.
		m.Client = ""
		m.CurrentSession = ""
		return m, fetchCurrentClient

	case FocusClientMsg:
		// Received the client name (and originating session) queried after focus
		// was gained.
		if msg.Client != "" {
			m.Client = msg.Client
		}
		// CurrentSession is empty for clients arriving via prefix+D because
		// ClientSession returns prism-dashboard (they are already here); in that
		// case the ◆ indicator stays blank, which is intentional.
		m.CurrentSession = msg.CurrentSession

	case SessionsMsg:
		m.Shared, _ = m.Shared.ApplySessionsMsg(msg, m.CurrentSession)

	case tea.KeyMsg:
		m.StatusMsg = ""

		// In filter mode most keys are consumed by the filter input.
		if m.FilterActive {
			var exitFilter bool
			var cmd tea.Cmd
			m.Shared, exitFilter, cmd = m.Shared.HandleFilterKey(msg)
			if exitFilter {
				// Confirm selection: switch the viewing client to the selected
				// session and return to passive watch mode. The process does NOT
				// quit — the dashboard stays alive for the next visitor.
				selected := m.Displayed[m.Cursor]
				// If the cursor is on a review-round group row (e.g. when filter
				// is empty and the cursor happened to be on one), toggle it
				// instead of trying to switch to a non-existent session.
				if selected.IsReviewGroup {
					m.Shared = ToggleReviewGroup(m.Shared, selected.Name)
					return m, nil
				}
				m.CursorActive = false
				return m, switchToSessionCmd(selected.Name)
			}
			return m, cmd
		}

		// Normal (non-filter) key handling.
		switch msg.String() {
		case "q", "esc":
			// Persistent mode: switch the current client back to its last session
			// (switch-client -l), then deactivate the cursor (return to passive
			// watch mode). Using -l avoids any stale caller state — it always
			// returns to wherever the client was before it switched here.
			// The process does NOT quit — the session stays alive.
			//
			// Always query the current client at switch time (see Enter handler
			// comment for rationale).
			m.CursorActive = false
			return m, func() tea.Msg {
				// Resolve the viewing client deterministically from the
				// dashboard session's client list, not display-message, which
				// is unsound from this pane-resident process (issue #2522,
				// defect 2).
				client, _ := resolveDashClientFunc()
				if client != "" {
					_ = tmux.SwitchClientLast(client)
				}
				return nil
			}

		case "/":
			// Activate inline fuzzy filter.
			m.FilterActive = true
			m.FilterText = ""
			m.CursorActive = true
			m.Shared = RefilterShared(m.Shared)
			return m, nil

		case "j", "down":
			if !m.CursorActive {
				// First keypress just activates the cursor without moving.
				m.CursorActive = true
				return m, CursorTimeoutCmd()
			}
			if m.Cursor < len(m.Displayed)-1 {
				m.Cursor++
			}
			return m, CursorTimeoutCmd()

		case "k", "up":
			if !m.CursorActive {
				m.CursorActive = true
				return m, CursorTimeoutCmd()
			}
			if m.Cursor > 0 {
				m.Cursor--
			}
			return m, CursorTimeoutCmd()

		case "enter":
			if len(m.Displayed) == 0 {
				return m, nil
			}
			selected := m.Displayed[m.Cursor]
			// Enter on a review-round group row toggles expand/collapse instead
			// of switching to a session. Activate the cursor so the change is
			// visible and keep the dashboard open.
			if selected.IsReviewGroup {
				m.CursorActive = true
				m.Shared = ToggleReviewGroup(m.Shared, selected.Name)
				return m, CursorTimeoutCmd()
			}
			// Enter on a live session row switches on the FIRST keypress - no
			// cursor-activation step is required (issue #2522, defect 1). The
			// passive-watch cursor is kept for j/k, which still activate before
			// they move, but Enter must act immediately on the highlighted row.
			m.CursorActive = false
			return m, switchToSessionCmd(selected.Name)
		}
	}
	return m, nil
}

func (m PersistentModel) View() string {
	return DashView(m.Shared, m.CurrentSession, m.CursorActive)
}

// switchToSessionCmd returns a tea.Cmd that resolves the client viewing the
// persistent dashboard and switches it to sessionName. It resolves the client
// deterministically via resolveDashClientFunc (issue #2522, defect 2) rather
// than display-message, which can return a client on another session or an
// empty string from this pane-resident process. When no client is attached it
// returns a visible status message instead of a silent no-op.
func switchToSessionCmd(sessionName string) tea.Cmd {
	return func() tea.Msg {
		client, _ := resolveDashClientFunc()
		if client == "" {
			return DashStatusMsg("no client is attached to the dashboard - cannot switch")
		}
		if errMsg := switchSessionFunc(sessionName, client); errMsg != "" {
			return DashStatusMsg(errMsg)
		}
		return nil
	}
}

// fetchCurrentClient queries tmux for the currently-attached client name and
// the session that client was last in (before switching to the dashboard).
// Returns a FocusClientMsg. Called as a tea.Cmd from the FocusMsg handler in
// PersistentModel so the I/O runs off the main update loop.
func fetchCurrentClient() tea.Msg {
	client, _ := CurrentClientFunc()
	// Also fetch the session the client came from so the "you are here" ◆
	// indicator can be shown for the visiting client.
	var currentSession string
	if client != "" {
		currentSession, _ = tmux.ClientSession(client)
		// ClientSession returns the session the client is *currently* in,
		// which at this point is already prism-dashboard. We need the previous
		// session. Use SwitchClientLast to peek is not possible without side
		// effects, so leave currentSession empty — the ◆ indicator is not
		// supported in persistent mode (the user is already in the dashboard).
		if currentSession == DashSession {
			currentSession = ""
		}
	}
	return FocusClientMsg{Client: client, CurrentSession: currentSession}
}

// DashSession is the name of the persistent dashboard tmux session.
const DashSession = "prism-dashboard"
