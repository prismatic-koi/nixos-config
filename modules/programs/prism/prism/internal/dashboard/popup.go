package dashboard

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ── popup dashboard model ─────────────────────────────────────────────────────

// PopupModel is the Bubble Tea model for the ephemeral popup dashboard
// (spawned by C-w via `tmux display-popup -E`).
//
// Lifecycle: the popup is short-lived. Pressing q/esc causes tea.Quit, which
// terminates the process; display-popup -E then closes the popup frame.
//
// Focus management: the popup runs inside the caller's own tmux client, so
// Client is always the right switch-client target. No global tmux option
// reads are required.
type PopupModel struct {
	Shared
	Client         string // tmux client running this popup (from CurrentClient())
	CurrentSession string // caller's current session (from --caller-session flag)
	CursorActive   bool   // always true in popup mode
}

// NewPopupModel constructs a new popup dashboard model.
func NewPopupModel(client, callerSession string) PopupModel {
	m := PopupModel{
		Shared: Shared{
			Loading: true,
		},
		Client:         client,
		CurrentSession: callerSession,
		CursorActive:   true, // popup always shows cursor
	}
	m.Displayed = m.Sessions // empty at init; populated on first SessionsMsg
	return m
}

func (m PopupModel) Init() tea.Cmd {
	return tea.Batch(FetchSessionsFromDB, FetchGitHubStats, GhTick())
}

func (m PopupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height

	case RefreshMsg:
		return m, FetchSessionsFromDB

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

	case SessionsMsg:
		m.Shared, _ = m.Shared.ApplySessionsMsg(msg, m.CurrentSession)

	case CursorTimeoutMsg:
		// Popup mode always shows the cursor (cursorActive is always true);
		// cursor-timeout messages are no-ops here, but handled explicitly to
		// self-document the intent.

	case tea.KeyMsg:
		m.StatusMsg = ""

		// In filter mode most keys are consumed by the filter input.
		if m.FilterActive {
			var exitFilter bool
			var cmd tea.Cmd
			m.Shared, exitFilter, cmd = m.Shared.HandleFilterKey(msg)
			if exitFilter {
				// Confirm selection: switch to highlighted session.
				selected := m.Displayed[m.Cursor]
				target := m.SwitchTarget()
				return m, func() tea.Msg {
					if errMsg := ensureSessionAndSwitch(selected.Name, target); errMsg != "" {
						return DashStatusMsg(errMsg)
					}
					return tea.QuitMsg{}
				}
			}
			return m, cmd
		}

		// Normal (non-filter) key handling.
		switch msg.String() {
		case "q", "esc":
			// Popup mode: just quit — display-popup -E closes the frame.
			return m, tea.Quit

		case "/":
			m.FilterActive = true
			m.FilterText = ""
			m.CursorActive = true
			m.Shared = RefilterShared(m.Shared)
			return m, nil

		case "j", "down":
			m.CursorActive = true
			if m.Cursor < len(m.Displayed)-1 {
				m.Cursor++
			}
			return m, nil

		case "k", "up":
			m.CursorActive = true
			if m.Cursor > 0 {
				m.Cursor--
			}
			return m, nil

		case "enter":
			if len(m.Displayed) == 0 {
				return m, nil
			}
			selected := m.Displayed[m.Cursor]
			target := m.SwitchTarget()
			return m, func() tea.Msg {
				if errMsg := ensureSessionAndSwitch(selected.Name, target); errMsg != "" {
					return DashStatusMsg(errMsg)
				}
				return tea.QuitMsg{}
			}
		}
	}
	return m, nil
}

// SwitchTarget returns the tmux client that should receive switch-client when
// the user selects a session. In popup mode the popup process runs inside the
// caller's own client, so Client is the right target.
func (m PopupModel) SwitchTarget() string {
	return m.Client
}

func (m PopupModel) View() string {
	return DashView(m.Shared, m.CurrentSession, m.CursorActive)
}

// ── session switch helpers ────────────────────────────────────────────────────

// ensureSessionAndSwitch checks whether the named tmux session exists.
// If it does, it selects the agent window and switches the given client to it.
// If it does not, it looks up the worktree path from the DB; if the directory
// exists on disk it recreates a bare shell session there, then switches.
// Returns a non-empty error string on failure; returns "" on success.
func ensureSessionAndSwitch(sessionName, target string) string {
	if tmux.HasSession(sessionName) {
		_ = tmux.SelectAgentWindow(sessionName)
		if target != "" {
			if err := tmux.SwitchClient(target, sessionName); err != nil {
				return fmt.Sprintf("switch failed: %v", err)
			}
		}
		return ""
	}

	worktreePath := worktreePathFromDB(sessionName)
	if worktreePath == "" {
		return fmt.Sprintf("session %q has no worktree record — use prism cleanup to remove it", sessionName)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		return fmt.Sprintf("worktree directory not found: %s", worktreePath)
	}

	if err := session.Create(sessionName, worktreePath, session.Opts{Layout: session.LayoutBare}); err != nil {
		return fmt.Sprintf("could not recreate session: %v", err)
	}

	if target != "" {
		if err := tmux.SwitchClient(target, sessionName); err != nil {
			return fmt.Sprintf("switch failed: %v", err)
		}
	}
	return ""
}

// worktreePathFromDB looks up the worktree path for a session from the DB only.
// Returns "" if not found or on error.
func worktreePathFromDB(sessionName string) string {
	d, err := openDB()
	if err != nil {
		return ""
	}
	defer d.Close()
	status, err := d.CurrentStatus(sessionName)
	if err != nil || status == nil {
		return ""
	}
	return status.Worktree
}
