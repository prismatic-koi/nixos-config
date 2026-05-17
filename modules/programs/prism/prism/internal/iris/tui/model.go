package tui

// model.go — bubbletea Model for the iris TUI.
//
// Layout (terminal full-screen):
//
//	┌─────────────────────────────────────────────────────────────┐
//	│  left pane (session list)  │  right pane (event stream)    │
//	│  name  state  role  time   │  narrative lines …            │
//	│  …                         │                               │
//	├─────────────────────────────┴───────────────────────────────┤
//	│  prompt: _                                                  │
//	└─────────────────────────────────────────────────────────────┘
//
// The TUI is a single bubbletea model (Model). All daemon I/O goes through
// the DaemonClient, which delivers messages via Program.Send(). The TUI
// reads NO state from the DB — every piece of state comes via the daemon socket.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/narrative"
	"github.com/prismatic-koi/prism/internal/payload"
)

// --- Layout constants ---

const (
	leftPaneRatio   = 0.35 // fraction of total width for the session list
	bottomBarHeight = 3    // prompt box height in lines
	statusBarHeight = 1    // status-line strip between events and prompt (#1767)
	minLeftWidth    = 28
	minRightWidth   = 20
)

// --- Colour scheme (gruvbox-dark inspired, same as prism dashboard) ---

const (
	colPrimary    = "#d79921" // yellow
	colSecondary  = "#928374" // grey
	colGreen      = "#98971a"
	colBlue       = "#458588"
	colRed        = "#cc241d"
	colForeground = "#ebdbb2"
	colBg0        = "#282828"
	colBg1        = "#3c3836"
	colBg2        = "#504945"
)

// --- Styles ---

var (
	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color(colSecondary))

	styleHeader = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colPrimary)).
			Bold(true)

	styleSelected = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colBg0)).
			Background(lipgloss.Color(colPrimary)).
			Bold(true)

	styleNormal = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colForeground))

	styleDim = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colSecondary))

	styleError = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colRed))

	styleGreen = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colGreen))

	styleBlue = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colBlue))

	// styleYellow highlights sessions in `waiting` state — paused for the
	// next user prompt. Distinct from `active` (green) and `finished`
	// (dim) so operators can spot attention-needed sessions at a glance.
	styleYellow = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colPrimary)).
			Bold(true)

	stylePromptBox = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color(colBlue))

	// styleToolCall renders completed tool-call card headers (paired
	// tool_call + tool_result) and the legacy one-line card rows used
	// by fallback paths. Blue + bold keeps the header visually loud so
	// it reads as the start of a discrete tool invocation block.
	// Issue #1767 introduced this style for one-line cards; issue
	// #1769 extends it to multi-line cards (the header line continues
	// to carry the same treatment).
	styleToolCall = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colBlue)).
			Bold(true)

	// styleToolCallInFlight renders the header of an in-flight tool
	// call (tool_call seen, no matching tool_result yet) introduced
	// in issue #1769. Yellow + bold visually flags "this is running
	// right now" without using the red palette reserved for errors;
	// distinct from styleToolCall's blue so the operator can scan a
	// tall conversation and pick out which calls are still in flight.
	styleToolCallInFlight = lipgloss.NewStyle().
				Foreground(lipgloss.Color(colPrimary)).
				Bold(true)

	// styleToolCardArgs renders the args summary line of a tool
	// card. Dim so the args read as supplementary context to the
	// header (not competing with it for the operator's attention).
	styleToolCardArgs = lipgloss.NewStyle().
				Foreground(lipgloss.Color(colSecondary))

	// styleErrorProminent renders extension_error blocks. Bold red on
	// the dark background is the most attention-grabbing combination
	// available in the existing palette — extension errors are
	// fatal-class (#1757) so the design doc renderer table calls them
	// out as a "prominent error block". Distinct from styleError (red
	// foreground only, no bold) so an extension_error block is visually
	// louder than a routine permission_denied row.
	styleErrorProminent = lipgloss.NewStyle().
				Foreground(lipgloss.Color(colRed)).
				Bold(true)

	// styleStatusLine renders the bottom status-line strip (#1767) with
	// a contrasting background bar so the strip reads as a structural
	// divider between the event pane and the prompt rather than as
	// another line of conversation content. Foreground stays on the
	// foreground palette so the metadata (session · state · model ·
	// cost) is fully legible.
	styleStatusLine = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colForeground)).
			Background(lipgloss.Color(colBg1))

	// styleEscalation is the prominent treatment for session.escalated
	// rows in the conversation pane when the focused session is a
	// coordinator (issue #1772). Bold red foreground on the
	// yellow/highlight background mirrors the visual weight of
	// styleErrorProminent (extension errors) but with a different
	// background so the operator can distinguish "a worker hit an
	// extension error" from "a worker has called for help". The
	// styling is applied via a guarded path (m.focusedIsCoordinator)
	// so non-coordinator sessions render escalations with the standard
	// fallback treatment — the prominence is a coordinator-only signal.
	styleEscalation = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colRed)).
			Background(lipgloss.Color(colPrimary)).
			Bold(true)

	// styleMergeQueue is the prominent treatment for merge-queue
	// notification rows when the focused session is a coordinator.
	// Bold green foreground (the prism palette's "success" / "merged"
	// colour) on the bg1 background distinguishes a merge-queue row
	// from both styleEscalation (red on yellow) and ordinary msg_user
	// rows (blue, no background). The distinction is intentional even
	// for failure notifications ("PR #N CI failed") because the
	// overlay overall is "merge-queue news", and the operator's
	// reading flow is: spot the row → read the verb. We do not paint
	// failed-merge rows red here because the row's content already
	// names the failure, and styling the entire family one way keeps
	// the visual vocabulary minimal.
	styleMergeQueue = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colGreen)).
			Background(lipgloss.Color(colBg1)).
			Bold(true)
)

// --- Model ---

// sessionItem holds one session row in the left pane.
//
// lastEventAt and lastAssistantPreview are populated as session_event
// frames arrive on the daemon socket (see handleDaemonFrame) so the
// sidebar can show a per-session HH:MM "most recent activity" timestamp
// and (when msg_assistant events are flowing) a one-line preview of the
// most recent assistant reply. Both fields are zero/empty for sessions
// that have produced no events since the TUI subscribed; the renderer in
// sidebar.go handles those cases gracefully ("-" for the timestamp and
// no preview row at all).
type sessionItem struct {
	snap                 iris.SessionSnapshot
	lastEventAt          time.Time
	lastAssistantPreview string
	// lastModel is the most recently observed model identifier on a
	// msg_assistant event for this session — sourced from
	// payload.MsgAssistant.Model ("providerID/modelID"). Used by the
	// status-line strip (issue #1767) so the focused session shows its
	// active model without re-querying the DB. Empty string when no
	// msg_assistant event with a populated Model field has been seen.
	lastModel string
	// cumulativeCost is the running sum of payload.MsgAssistant.Cost
	// values observed for this session since the TUI subscribed.
	// Displayed by the status-line strip as the focused session's
	// running cost; zero when no Cost-bearing event has been seen.
	// Per-session (not per-turn) because the user wants to see total
	// spend at a glance — per-turn cost is available in checkin output.
	cumulativeCost float64
}

// focusArea identifies the currently focused interactive region. Tab
// rotates between the session list (left pane), the event stream (right
// pane, for scrolling), and the prompt (bottom). The prompt is the
// implicit default — pre-#1737 the prompt always swallowed typed runes.
// Now we surface focus explicitly so Tab can rotate it.
type focusArea int

const (
	focusPrompt   focusArea = iota // default: typing into the prompt
	focusSessions                  // navigation focused on the session list
	focusEvents                    // navigation focused on the event stream
)

// overlayKind enumerates which (if any) modal overlay is currently active.
// Only one overlay can be active at a time. overlayNone means the normal
// session-list + events + prompt layout is rendered.
type overlayKind int

const (
	overlayNone              overlayKind = iota
	overlayPicker                        // Ctrl+F: session picker / spawn-new
	overlaySpawnWorktree                 // step 2 of spawn flow: typing the worktree path
	overlaySpawnRole                     // step 3 of spawn flow: typing the role
	overlayDashboard                     // Ctrl+W: multi-session dashboard view
	overlayHelp                          // ?: keybindings help
	overlayCoordinatorEvents             // Ctrl+O: coordinator-only escalations + merge-queue events (#1772)
)

// Model is the top-level bubbletea model for the iris TUI.
type Model struct {
	client *DaemonClient

	// renderer is the per-event-type dispatch table used by
	// handleDaemonFrame to render session_event payloads into
	// NarrativeLines (issue #1767). Constructed once in NewModel; the
	// dispatch site does not switch on event type directly so child
	// PR 3 (streaming) and child PR 4 (tool-call cards) can plug into
	// this table without re-touching the model.
	renderer *eventRenderer

	// Terminal dimensions.
	width  int
	height int

	// Connection state.
	connected    bool
	connectError string
	reconnecting bool

	// Session list (left pane).
	sessions []sessionItem
	cursor   int // selected row index

	// initialSession is the session name to focus on when the first
	// sessions_snapshot frame arrives. Empty means "use the daemon's
	// natural ordering and select the first row". Set by the --session
	// CLI flag (and consumed/cleared after first use).
	initialSession string

	// Subscribed session name (may differ from sessions[cursor].snap.Name
	// transiently during session switching).
	subscribedTo string

	// Event stream (right pane): rendered narrative lines, newest at bottom.
	eventLines []narrative.NarrativeLine
	// seenRowIDs deduplicates replayed vs live events.
	seenRowIDs map[int64]bool
	// toolCallByMsgID indexes the line slice position for open tool_call lines
	// so that arriving tool_result frames can append the result inline.
	//
	// Pre-#1769 this was the single source of truth for tool_call ↔
	// tool_result pairing. Post-#1769 it is retained for any legacy
	// callers / tests that may still consult it; the canonical lookup
	// is now toolCardByMsgID below, which carries the full multi-line
	// card state required by the expand/collapse toggle.
	toolCallByMsgID map[string]int

	// toolCards is the ordered list of multi-line tool-call cards in
	// the current event pane (issue #1769, child 4 of the iris-tui
	// design). Each card owns a contiguous range of m.eventLines (see
	// toolCard.lineStart / .lineLen). The slice is append-only; on
	// resetEventPane it is cleared together with eventLines. The
	// ordering matches the order in which tool_call events arrived
	// for the subscribed session.
	toolCards []*toolCard

	// toolCardByMsgID is the lookup index for toolCards, keyed on
	// payload.ToolCall.MessageID (the same id used to pair a
	// tool_call with its tool_result). Inserted on tool_call arrival,
	// consumed on tool_result arrival, and consulted by the
	// expand/collapse toggle. Missing keys are not an error — a
	// tool_result with no matching card falls back to the legacy
	// indented-summary path.
	toolCardByMsgID map[string]*toolCard

	// expandedToolCards is the set of MessageIDs whose tool-card is
	// currently expanded (showing full args / full result). Adding
	// or removing a key triggers a card re-render via
	// rebuildToolCardLines. The set persists across event arrivals
	// per AC: "receiving a new event while a card is expanded does
	// not collapse it."
	expandedToolCards map[string]bool

	// eventScroll is the number of lines scrolled up from the bottom (0 = live).
	eventScroll int

	// Prompt input (bottom).
	promptRunes  []rune
	promptCursor int // rune insert position

	// In-TUI overlay state (issue #1737). overlay == overlayNone means no
	// overlay is active; all other values render a full-screen modal on top
	// of the normal view.
	overlay overlayKind
	// picker state — populated when overlay == overlayPicker.
	picker pickerState
	// spawn state — populated when overlay == overlaySpawnWorktree or
	// overlaySpawnRole. Carries the worktree typed in step 2 so step 3 can
	// recall it when the user confirms the role.
	spawn spawnState
	// errorMsg is a transient one-line error rendered in the picker overlay
	// (e.g. after a failed spawn attempt). Cleared on overlay dismissal.
	errorMsg string

	// focus is the currently focused interactive region (Tab rotates it).
	focus focusArea

	// coordinatorEvents is the in-memory accumulator for the
	// coordinator-events overlay (issue #1772 child 7). Populated by
	// handleDaemonFrame whenever a session.escalated event arrives, or
	// when a msg_user event's text matches
	// isMergeQueueNotificationText. The buffer is bounded to
	// coordinatorEventBufferCap; older entries are dropped from the
	// front when capacity is exceeded.
	//
	// Routing note (the daemon-side companion of this feature):
	// session.escalated rows are Publish()ed by
	// ClientSocket.writeSessionEscalatedEvent on BOTH the worker's
	// stream AND the target coordinator's stream (the secondary
	// Publish was added alongside this TUI work, also under #1772).
	// That means a TUI focused on the coordinator receives the event
	// in real time without subscribing to every worker. There is
	// still exactly one DB audit row per escalation — the second
	// Publish is delivery-only.
	//
	// Sourcing note (and the reason this is not a DB query): the TUI's
	// TestNoDBImport invariant forbids importing internal/db. The
	// design-doc guidance for this child suggests pulling history from
	// agent_events directly, but that would require a new daemon frame
	// to ferry the rows across the socket — out of scope for this PR.
	// In-memory accumulation gives correct behaviour for events that
	// arrive while the TUI is running; a future PR can add a daemon
	// frame for cross-session historic queries when one is needed for
	// long-running operator workflows. The bounded buffer is the
	// single point that limits memory exposure.
	coordinatorEvents []coordinatorEvent
}

// NewModel creates the iris TUI model.
func NewModel(client *DaemonClient) Model {
	return Model{
		client:            client,
		renderer:          newEventRenderer(),
		seenRowIDs:        make(map[int64]bool),
		toolCallByMsgID:   make(map[string]int),
		toolCardByMsgID:   make(map[string]*toolCard),
		expandedToolCards: make(map[string]bool),
	}
}

// NewModelFocused is like NewModel but pre-seeds an initial session name. On
// the first sessions_snapshot frame, if a session with that name is present
// the cursor is positioned on it (and the subscription targets it) instead
// of defaulting to row 0. Used by `iris tui --session <name>` so that the
// `iris switch` picker can hand the TUI a specific session to focus on.
func NewModelFocused(client *DaemonClient, initialSession string) Model {
	m := NewModel(client)
	m.initialSession = initialSession
	return m
}

func (m Model) Init() tea.Cmd {
	return nil
}

// --- Update ---

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case ConnectedMsg:
		m.connected = true
		m.reconnecting = false
		m.connectError = ""
		// The DaemonClient sends sessions_list immediately on connect;
		// no need to dispatch a redundant Cmd here.
		return m, nil

	case DisconnectedMsg:
		m.connected = false
		if msg.Err != nil {
			m.connectError = msg.Err.Error()
		}
		m.reconnecting = true
		return m, nil

	case DaemonFrame:
		return m.handleDaemonFrame(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m Model) handleDaemonFrame(msg DaemonFrame) (tea.Model, tea.Cmd) {
	switch msg.RawType {

	case iris.DaemonFrameSessionsSnapshot:
		if msg.Snapshot == nil {
			return m, nil
		}
		prevSelected := ""
		if m.cursor < len(m.sessions) {
			prevSelected = m.sessions[m.cursor].snap.Name
		}
		// On the first snapshot, an initialSession from --session takes
		// precedence over the previous-cursor heuristic. Consume the field
		// after applying it so later snapshots fall back to prevSelected.
		target := prevSelected
		if m.initialSession != "" {
			target = m.initialSession
			m.initialSession = ""
		}
		m.sessions = make([]sessionItem, len(msg.Snapshot.Sessions))
		for i, s := range msg.Snapshot.Sessions {
			m.sessions[i] = sessionItem{snap: s}
		}
		// Restore cursor position if the target session is present.
		m.cursor = 0
		for i, si := range m.sessions {
			if si.snap.Name == target {
				m.cursor = i
				break
			}
		}
		// Subscribe to the newly selected session if not already subscribed.
		if len(m.sessions) > 0 {
			name := m.sessions[m.cursor].snap.Name
			if name != m.subscribedTo {
				m.subscribedTo = name
				m.resetEventPane()
				return m, func() tea.Msg {
					_ = m.client.SendSessionSubscribe(name, 0)
					return nil
				}
			}
		}

	case iris.DaemonFrameSessionEvent:
		if msg.Event == nil {
			return m, nil
		}
		e := msg.Event
		// Sidebar bookkeeping (issue #1771 child 6): every session_event
		// arrival bumps the per-session last-event timestamp shown in the
		// sidebar's HH:MM column, regardless of whether the row_id has
		// been seen before. The de-dupe path below only suppresses
		// re-rendering into the conversation pane; the sidebar timestamp
		// should still tick when the daemon replays an event we already
		// have, because the "most recent activity" signal is about what
		// the daemon is sending us NOW, not about whether we'd already
		// drawn it. We also opportunistically capture the latest
		// msg_assistant text as a one-line preview when payload parsing
		// succeeds; failures are silent (the preview just doesn't update).
		now := time.Now()
		for i, si := range m.sessions {
			if si.snap.Name == e.SessionName {
				m.sessions[i].lastEventAt = now
				if e.EventType == "msg_assistant" {
					if preview := extractAssistantPreview(e.Payload); preview != "" {
						m.sessions[i].lastAssistantPreview = preview
					}
				}
				break
			}
		}
		if m.seenRowIDs[e.RowID] {
			return m, nil
		}

		// Capture model + accumulated cost for the status-line strip
		// (issue #1767) AFTER the dedupe check — we want
		// snapshot-replay overlapping with a live frame to keep the
		// sidebar timestamp ticking (above) but to NOT double-count
		// cost or thrash lastModel. The previous block keeps the
		// sidebar's "most recent activity" honest; this block treats
		// model/cost as content that only updates on truly novel rows.
		if e.EventType == "msg_assistant" {
			if model, cost := extractAssistantModelCost(e.Payload); model != "" || cost > 0 {
				for i, si := range m.sessions {
					if si.snap.Name == e.SessionName {
						if model != "" {
							m.sessions[i].lastModel = model
						}
						m.sessions[i].cumulativeCost += cost
						break
					}
				}
			}
		}

		// Tool-call card path (#1769): intercept tool_call and
		// tool_result BEFORE the generic dispatch loop. tool_call
		// produces a multi-line in-flight card recorded in m.toolCards;
		// tool_result mutates that card's lines in place to the
		// completed visual. Falls through to the generic path only when
		// no matching tool_call exists for a tool_result (legacy
		// indented-summary fallback per child 2).
		if e.EventType == evTypeToolCall {
			if card := m.installToolCallCard(e.RowID, e.Payload); card != nil {
				m.seenRowIDs[e.RowID] = true
				return m, nil
			}
			// Parse failed and installer returned nil — fall through
			// to the legacy dispatch path so the parse-error line still
			// renders. Drops through to the dispatch block below.
		}
		if e.EventType == evTypeToolResult {
			if handled := m.foldToolResultIntoCard(e.Payload); handled {
				m.seenRowIDs[e.RowID] = true
				return m, nil
			}
			// No matching card — fall through to the dispatcher which
			// produces the legacy indented one-liner. AC explicit:
			// "don't regress the orphan tool_result path".
		}

		lines := m.renderer.dispatch(e.RowID, e.EventType, e.Payload)

		// Coordinator-events accumulator (issue #1772 child 7).
		// session.escalated rows and msg_user rows that match the
		// merge-queue notification text are appended to the per-Model
		// coordinatorEvents buffer regardless of which session is
		// focused, so the coordinator-events overlay can list them. The
		// merge-queue case also re-labels the rendered NarrativeLine's
		// EventType to evTypeMergeQueueNotification so styleEventLine can
		// apply the distinct visual treatment when the focused session
		// is a coordinator. We do the re-label in place rather than via
		// a second renderer pass to keep the merge-queue handling
		// orthogonal to the dispatch table — the renderer.go handler
		// set stays focused on real agent_events.type values.
		m.accumulateCoordinatorEvent(e, lines)

		if len(lines) == 0 {
			// Suppressed (session_status, turn_start/end) or a handler
			// declined to render. Do NOT mark the row_id as seen —
			// future renderer extensions may decide to render an event
			// type that is currently suppressed, and the dedupe map
			// must not preempt them on replay. Suppression has no
			// per-event side effect beyond "do not append a line".
			return m, nil
		}
		m.seenRowIDs[e.RowID] = true
		for _, line := range lines {
			m.eventLines = append(m.eventLines, line)
		}
		// Snap to bottom if the user is not scrolled up.
		if m.eventScroll == 0 {
			// (view already renders from bottom; nothing extra needed)
		}

	case iris.DaemonFrameSessionState:
		if msg.State == nil {
			return m, nil
		}
		// Update the in-memory session list state.
		for i, si := range m.sessions {
			if si.snap.Name == msg.State.SessionName {
				m.sessions[i].snap.State = msg.State.State
				break
			}
		}

	case iris.DaemonFrameSessionSpawned:
		// A new session has been spawned. Append it to the session list so
		// the user sees it without restarting the TUI.
		//
		// Malformed frames (missing payload or empty name) are skipped:
		// rendering an empty row would be worse than no-op, and a future
		// sessions_snapshot will reconcile the list anyway.
		if msg.Spawned == nil || msg.Spawned.Name == "" {
			return m, nil
		}
		spawned := msg.Spawned
		// Build a snapshot for the new row. Prefer the daemon-supplied
		// Session record (which carries state/role/worktree/started_at); fall
		// back to the minimal Name+InstanceID fields for forward compat with
		// older daemons that don't populate Session.
		var snap iris.SessionSnapshot
		if spawned.Session != nil {
			snap = *spawned.Session
		} else {
			snap = iris.SessionSnapshot{
				Name:       spawned.Name,
				InstanceID: spawned.InstanceID,
			}
		}
		// Dedupe defensively: if the session is already in the list (e.g.
		// the daemon emitted both a snapshot and a session_spawned for the
		// same incarnation), treat the frame as an update rather than a
		// duplicate-append.
		for i, si := range m.sessions {
			if si.snap.Name == snap.Name {
				m.sessions[i].snap = snap
				return m, nil
			}
		}
		// Append. sessions_snapshot itself is unsorted (it iterates the
		// supervisor map in cmd/iris/main.go), so appending is consistent
		// with the existing ordering — the new row simply appears at the
		// bottom of the list.
		wasEmpty := len(m.sessions) == 0
		m.sessions = append(m.sessions, sessionItem{snap: snap})
		// Auto-subscribe when the list transitions from empty to non-empty
		// and there is no existing subscription. This mirrors the
		// sessions_snapshot auto-subscribe path so a session spawned while
		// the TUI is open with an empty list becomes immediately usable
		// (prompt send is gated on subscribedTo != ""). We deliberately do
		// NOT switch subscription when the list was already non-empty —
		// that would steal focus from a session the user is actively
		// watching whenever a sibling spawns.
		if wasEmpty && m.subscribedTo == "" {
			m.cursor = 0
			name := snap.Name
			m.subscribedTo = name
			m.resetEventPane()
			return m, func() tea.Msg {
				_ = m.client.SendSessionSubscribe(name, 0)
				return nil
			}
		}

	case iris.DaemonFrameError:
		if msg.Error != nil {
			// Show error as a narrative line in the event pane.
			m.eventLines = append(m.eventLines, narrative.NarrativeLine{
				Text:      fmt.Sprintf("⚠ daemon error: %s", msg.Error.Message),
				EventType: "error",
			})
		}
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Overlay routing: if an overlay is active it gets first refusal at
	// every keystroke. Quit (ctrl+c) still works because the overlay
	// handlers delegate it back to the main switch.
	if m.overlay != overlayNone {
		return m.handleOverlayKey(msg)
	}

	// Clear any transient errorMsg from a previous keystroke (e.g. a
	// C-o on a non-coordinator session, #1772) unless this very key is
	// the one that sets it. Without this, an error message would stick
	// around indefinitely until an overlay opened/closed it. We clear
	// BEFORE the switch so the per-case body can re-set errorMsg if it
	// needs to; clearing afterwards would wipe the case's own message.
	if msg.String() != "ctrl+o" {
		m.errorMsg = ""
	}

	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit

	// --- in-TUI overlay openers (issue #1737) ---
	//
	// These bindings fire when no overlay is active. Iris owns them
	// unconditionally — there is no longer a tmux popup binding to
	// coexist with (issue #1766).
	case "ctrl+f":
		// Open the in-TUI session picker overlay.
		m.openPicker()
		return m, nil

	case "ctrl+w":
		// Open the multi-session dashboard overlay.
		m.overlay = overlayDashboard
		return m, nil

	case "ctrl+o":
		// Open the coordinator-events overlay (issue #1772). We use
		// C-o rather than the issue's suggested C-e because C-e is
		// already bound to "end of line" in the prompt input (see the
		// `case "end", "ctrl+e"` arm below). C-o is mnemonic for
		// "overlay of c\u00f6ordinator events" and is otherwise unused by
		// either the main key handler or the overlay handlers.
		//
		// On a non-coordinator session this is a soft no-op: we set
		// errorMsg so the operator sees a clear "not applicable" line
		// rather than wondering why the keypress did nothing. The
		// errorMsg is rendered by viewPrompt's error path and cleared
		// the next time any overlay opens or closes.
		if !m.focusedIsCoordinator() {
			m.errorMsg = "C-o: coordinator-events overlay only applies to coordinator sessions"
			return m, nil
		}
		m.overlay = overlayCoordinatorEvents
		return m, nil

	case "?":
		// Only treat `?` as the help binding when the prompt is empty —
		// otherwise the user might want to type a literal question mark
		// into a prompt body. When the prompt is non-empty we must NOT
		// silently swallow the keystroke: we insert it as a literal rune,
		// matching the behaviour of every other printable character (the
		// `default` arm at the bottom of this switch). A previous version
		// of this code fell out of the case without inserting, dropping
		// the `?` — fixed here with an inline splice.
		if len(m.promptRunes) == 0 {
			m.overlay = overlayHelp
			return m, nil
		}
		ins := []rune{'?'}
		newRunes := make([]rune, len(m.promptRunes)+len(ins))
		copy(newRunes, m.promptRunes[:m.promptCursor])
		copy(newRunes[m.promptCursor:], ins)
		copy(newRunes[m.promptCursor+len(ins):], m.promptRunes[m.promptCursor:])
		m.promptRunes = newRunes
		m.promptCursor += len(ins)
		return m, nil

	case "esc":
		// No overlay open: Escape is a no-op. The issue spec is explicit
		// that Escape must NOT quit — only `q` and `ctrl+c` quit.
		return m, nil

	case "tab":
		// Two-mode tab binding (issue #1769 extends #1737):
		//
		// When focus is on the events pane AND at least one tool-call
		// card exists, tab expands/collapses the most recent tool
		// card. The spec says "Pressing `tab` while the conversation
		// pane has focus expands the currently-selected tool-call
		// card." We take "currently-selected" as the most recent card
		// per the issue's implementation-guidance hint (a single
		// current-card pointer is fine; child 5 may add full
		// selection).
		//
		// In every other case, tab continues to rotate focus prompt
		// → sessions → events → prompt as before — the rotation is
		// the only way to land focus on events in the first place,
		// so leaving it in place for the no-cards-yet case keeps the
		// pre-#1769 navigation working.
		if m.focus == focusEvents && m.toggleSelectedToolCard() {
			return m, nil
		}
		switch m.focus {
		case focusPrompt:
			m.focus = focusSessions
		case focusSessions:
			m.focus = focusEvents
		default:
			m.focus = focusPrompt
		}
		return m, nil

	case "ctrl+r":
		// Force-refresh: re-request the sessions snapshot from the daemon.
		return m, func() tea.Msg {
			_ = m.client.SendSessionsList()
			return nil
		}

	case "ctrl+l":
		// Clear-and-redraw. bubbletea repaints the full View() on every
		// Update, so issuing tea.ClearScreen here triggers a fresh paint
		// without losing model state.
		return m, tea.ClearScreen

	case "up", "ctrl+p", "k":
		if m.cursor > 0 {
			m.cursor--
			return m, m.switchToSelected()
		}

	case "down", "ctrl+n", "j":
		if m.cursor < len(m.sessions)-1 {
			m.cursor++
			return m, m.switchToSelected()
		}

	case "pgup":
		m.eventScroll += (m.rightPaneHeight() - 2)
		return m, nil

	case "pgdown":
		m.eventScroll -= (m.rightPaneHeight() - 2)
		if m.eventScroll < 0 {
			m.eventScroll = 0
		}
		return m, nil

	case "enter":
		if len(m.promptRunes) > 0 && m.subscribedTo != "" {
			text := string(m.promptRunes)
			m.promptRunes = nil
			m.promptCursor = 0
			return m, func() tea.Msg {
				_ = m.client.SendPromptDeliver(m.subscribedTo, text)
				return nil
			}
		}

	case "backspace", "ctrl+h":
		if m.promptCursor > 0 {
			m.promptRunes = append(m.promptRunes[:m.promptCursor-1], m.promptRunes[m.promptCursor:]...)
			m.promptCursor--
		}

	case "left", "ctrl+b":
		if m.promptCursor > 0 {
			m.promptCursor--
		}

	case "right":
		// Note: ctrl+f is now the picker-overlay binding (handled above),
		// no longer an alias for right-arrow in the prompt. Plain `right`
		// still moves the prompt cursor; users who need a single-rune
		// right-step can use the arrow key.
		if m.promptCursor < len(m.promptRunes) {
			m.promptCursor++
		}

	case "home", "ctrl+a":
		m.promptCursor = 0

	case "end", "ctrl+e":
		m.promptCursor = len(m.promptRunes)

	case "delete", "ctrl+d":
		if m.promptCursor < len(m.promptRunes) {
			m.promptRunes = append(m.promptRunes[:m.promptCursor], m.promptRunes[m.promptCursor+1:]...)
		}

	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			ins := []rune(msg.String())
			newRunes := make([]rune, len(m.promptRunes)+len(ins))
			copy(newRunes, m.promptRunes[:m.promptCursor])
			copy(newRunes[m.promptCursor:], ins)
			copy(newRunes[m.promptCursor+len(ins):], m.promptRunes[m.promptCursor:])
			m.promptRunes = newRunes
			m.promptCursor += len(ins)
		}
	}

	return m, nil
}

// switchToSelected unsubscribes from the previous session and subscribes to
// the newly selected one, clearing the event pane.
func (m *Model) switchToSelected() tea.Cmd {
	if m.cursor >= len(m.sessions) {
		return nil
	}
	newName := m.sessions[m.cursor].snap.Name
	if newName == m.subscribedTo {
		return nil
	}
	prev := m.subscribedTo
	m.subscribedTo = newName
	m.resetEventPane()

	return func() tea.Msg {
		if prev != "" {
			_ = m.client.SendSessionUnsubscribe(prev)
		}
		_ = m.client.SendSessionSubscribe(newName, 0)
		return nil
	}
}

// toolCard is the model-side state for a single multi-line tool-call
// card (#1769, child 4 of the iris-tui design). One card maps 1∶1 to a
// tool_call event keyed on payload.ToolCall.ID (the same id used by
// the matching tool_result — see issue #1783's wire-shape pivot).
// lineStart and lineLen index into m.eventLines so the card can be
// re-materialised in place when the result arrives or the operator
// toggles expand/collapse.
//
// The full args / result strings are stored on the card so the
// expanded view can show them without re-parsing the original event
// payload (the payload string itself is dropped after rendering).
//
// Field naming note: the local field is named `msgID` for backward
// readability — it holds payload.ToolCall.ID, which the extension
// emits as the JSON `id` field. The Model maps it via
// toolCardByMsgID; renaming the variable would churn this file
// without making the code clearer.
type toolCard struct {
	msgID     string
	rowID     int64
	tool      string
	args      json.RawMessage
	result    string
	paired    bool
	lineStart int
	lineLen   int
}

// installToolCallCard handles a fresh tool_call frame: parses the
// payload, builds the multi-line collapsed card, appends its lines to
// m.eventLines, and registers the card in toolCards / toolCardByMsgID.
// Returns the new card on success, nil on parse failure (in which
// case the caller falls back to the dispatcher's parse-error path).
//
// payload.ToolCall.ID may legitimately be empty (older pi versions,
// replay races). When empty we still install the card so the
// rendering is correct for the in-flight phase, but pairing on
// tool_result is impossible — the card stays in flight forever,
// visually flagging the missing pairing context.
func (m *Model) installToolCallCard(rowID int64, payloadJSON string) *toolCard {
	var p payload.ToolCall
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return nil
	}
	card := &toolCard{
		msgID:     p.ID,
		rowID:     rowID,
		tool:      p.Name,
		args:      p.Args,
		paired:    false,
		lineStart: len(m.eventLines),
	}
	expanded := m.expandedToolCards[p.ID]
	lines := buildToolCardLines(rowID, p.ID, p.Name, p.Args, "", false, expanded)
	card.lineLen = len(lines)
	m.eventLines = append(m.eventLines, lines...)
	m.toolCards = append(m.toolCards, card)
	if p.ID != "" {
		m.toolCardByMsgID[p.ID] = card
		// Maintain the legacy index pointing at the header line so any
		// remaining callers continue to find a tool_call line by id.
		m.toolCallByMsgID[p.ID] = card.lineStart
	}
	return card
}

// foldToolResultIntoCard handles a tool_result frame. Locates the
// matching tool-card by ID (post-#1783 wire shape: the extension's
// tool_result.id mirrors the tool_call.id), marks it paired, stores
// the result output, and rebuilds the card's lines in place. Returns
// true on success, false when no matching card exists (the caller
// then falls back to the dispatcher's legacy orphan-result path).
//
// On parse failure of the payload, returns false so the dispatcher
// can render whatever it can. A malformed tool_result must not
// orphan the in-flight card — future tool_result arrivals for the
// same id still have a chance to land cleanly.
func (m *Model) foldToolResultIntoCard(payloadJSON string) bool {
	var p payload.ToolResult
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return false
	}
	if p.ID == "" {
		return false
	}
	card, ok := m.toolCardByMsgID[p.ID]
	if !ok {
		return false
	}
	card.paired = true
	card.result = p.Output
	m.rebuildCardLines(card)
	return true
}

// rebuildCardLines replaces the line range owned by card with a
// freshly built block reflecting the card's current state (paired vs
// in-flight, expanded vs collapsed). When the new block has a
// different length than the previous block, lineStart of every later
// card is shifted by the delta so the index stays accurate.
//
// This is the single mutation point for card geometry — anything
// that changes a card's visual state goes through this function so
// the eventLines slice and the per-card index stay consistent.
func (m *Model) rebuildCardLines(card *toolCard) {
	expanded := false
	if card.msgID != "" {
		expanded = m.expandedToolCards[card.msgID]
	}
	newLines := buildToolCardLines(card.rowID, card.msgID, card.tool, card.args, card.result, card.paired, expanded)
	oldLen := card.lineLen
	newLen := len(newLines)

	end := card.lineStart + oldLen
	if end > len(m.eventLines) {
		end = len(m.eventLines)
	}
	// Splice: prefix + newLines + suffix.
	prefix := m.eventLines[:card.lineStart]
	suffix := append([]narrative.NarrativeLine(nil), m.eventLines[end:]...)
	rebuilt := make([]narrative.NarrativeLine, 0, len(prefix)+newLen+len(suffix))
	rebuilt = append(rebuilt, prefix...)
	rebuilt = append(rebuilt, newLines...)
	rebuilt = append(rebuilt, suffix...)
	m.eventLines = rebuilt

	card.lineLen = newLen

	// Shift later cards' lineStart by the delta.
	delta := newLen - oldLen
	if delta != 0 {
		for _, c := range m.toolCards {
			if c.lineStart > card.lineStart {
				c.lineStart += delta
				if c.msgID != "" {
					m.toolCallByMsgID[c.msgID] = c.lineStart
				}
			}
		}
	}
}

// toggleSelectedToolCard expand/collapses the "current" tool card.
// Per the issue's implementation guidance ("a single 'current card
// index' pointing at the most recent tool card is fine"), the current
// card is defined as the most recently installed tool_call. Returns
// true when a card existed and was toggled, false when no cards are
// present (e.g. focus on events with no tool_call seen yet).
//
// The expand/collapse state is keyed on MessageID so it survives
// later card-state transitions (paired arrival, tool_result fold).
// A card with empty msgID is not toggleable — the set key would
// collide with every other empty-id card; in practice every
// production tool_call carries a MessageID.
func (m *Model) toggleSelectedToolCard() bool {
	if len(m.toolCards) == 0 {
		return false
	}
	card := m.toolCards[len(m.toolCards)-1]
	if card.msgID == "" {
		return false
	}
	if m.expandedToolCards[card.msgID] {
		delete(m.expandedToolCards, card.msgID)
	} else {
		m.expandedToolCards[card.msgID] = true
	}
	m.rebuildCardLines(card)
	return true
}

// resetEventPane clears the event buffer and associated indexes.
//
// Includes the tool-card bookkeeping introduced in #1769: cards belong
// to a single subscribed session, so switching sessions wipes them
// alongside eventLines. expandedToolCards is keyed on MessageID and
// those ids do not survive a session switch either — a fresh
// subscription means a fresh set of cards.
func (m *Model) resetEventPane() {
	m.eventLines = nil
	m.seenRowIDs = make(map[int64]bool)
	m.toolCallByMsgID = make(map[string]int)
	m.toolCards = nil
	m.toolCardByMsgID = make(map[string]*toolCard)
	m.expandedToolCards = make(map[string]bool)
	m.eventScroll = 0
}

// --- View ---

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}

	// Disconnected overlay.
	if !m.connected {
		return m.viewDisconnected()
	}

	// Modal overlay: when one is active it replaces the entire view. The
	// underlying session-list / event-stream / prompt remains in the model
	// state — Escape restores it instantly.
	if m.overlay != overlayNone {
		return m.viewOverlay()
	}

	leftW, rightW := m.paneWidths()

	leftPane := m.viewLeftPane(leftW)
	rightPane := m.viewRightPane(rightW)

	body := lipgloss.JoinHorizontal(lipgloss.Top, leftPane, rightPane)
	// Status-line strip between the event pane and the prompt (issue
	// #1767). Spans the full terminal width — the sidebar takes the
	// left edge but the status line is per-program (focused-session
	// metadata), not per-pane. Placed above the prompt because the
	// design doc's ASCII layout puts it there: prompt is the bottom
	// affordance, status sits between events and prompt.
	status := m.viewStatusLine(m.width)
	prompt := m.viewPrompt(m.width)

	return lipgloss.JoinVertical(lipgloss.Left, body, status, prompt)
}

// focusedBorderStyle returns a border style tinted with the primary accent
// colour when this pane currently holds focus, and the default dim border
// otherwise. Threaded through viewLeftPane / viewRightPane / viewPrompt so
// Tab's focus rotation has an observable rendered effect (issue #1737 AC:
// "switch focus between session list and event-stream / prompt areas").
// Without this, m.focus would rotate silently and the AC would only be
// satisfied at the variable level. Input routing still goes to the prompt
// for typing and to the cursor/scroll keys regardless of focus — the
// border tint is the UX signal that Tab did something visible.
func (m Model) focusedBorderStyle(area focusArea) lipgloss.Style {
	if m.focus == area {
		return lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color(colPrimary))
	}
	return styleBorder
}

func (m Model) viewDisconnected() string {
	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(styleError.Render("  ✗ iris daemon not connected"))
	sb.WriteString("\n\n")
	if m.connectError != "" {
		sb.WriteString(styleDim.Render("  " + m.connectError))
		sb.WriteString("\n")
	}
	if m.reconnecting {
		sb.WriteString(styleDim.Render("  Reconnecting…"))
	}
	sb.WriteString("\n\n")
	sb.WriteString(styleDim.Render("  Make sure the daemon is running:  iris daemon"))
	sb.WriteString("\n")
	sb.WriteString(styleDim.Render("  Press q or ctrl+c to quit."))
	return sb.String()
}

func (m Model) paneWidths() (int, int) {
	leftW := int(float64(m.width) * leftPaneRatio)
	if leftW < minLeftWidth {
		leftW = minLeftWidth
	}
	rightW := m.width - leftW
	if rightW < minRightWidth {
		rightW = minRightWidth
		leftW = m.width - rightW
	}
	return leftW, rightW
}

func (m Model) leftPaneHeight() int {
	// Subtract the prompt box (bottomBarHeight, includes borders) AND the
	// status-line strip (statusBarHeight, single line, no border)
	// introduced in #1767. The -2 still accounts for the side-pane border
	// rows. Capped at >= 1 so a tiny terminal still renders something
	// rather than crashing on a zero-height pane.
	h := m.height - bottomBarHeight - statusBarHeight - 2
	if h < 1 {
		h = 1
	}
	return h
}

func (m Model) rightPaneHeight() int {
	return m.leftPaneHeight()
}

// viewLeftPane renders the session list.
func (m Model) viewLeftPane(width int) string {
	innerW := width - 2 // border left+right
	paneH := m.leftPaneHeight()

	var rows []string

	// Header. The title gets a focus marker ("▸") when this pane holds
	// focus so the rotation is readable on terminals that don't render
	// border-colour changes well (e.g. low-contrast themes).
	title := "Sessions"
	if m.focus == focusSessions {
		title = "▸ Sessions"
	}
	header := styleHeader.Render(padRight(title, innerW))
	rows = append(rows, header)
	rows = append(rows, styleDim.Render(strings.Repeat("─", innerW)))

	if len(m.sessions) == 0 {
		rows = append(rows, "")
		rows = append(rows, styleDim.Render(padRight("  no sessions", innerW)))
		rows = append(rows, "")
		rows = append(rows, styleDim.Render(padRight("  iris spawn --worktree <path>", innerW)))
	} else {
		for i, si := range m.sessions {
			selected := i == m.cursor
			rows = append(rows, renderSessionRow(si, innerW, selected))
			// Optional second-line preview of the most recent assistant
			// message. Only present when at least one msg_assistant event
			// has been observed for this session — today, with pi 0.72.1
			// still emitting the wrong event type (#1764), this branch
			// stays empty for every session and the sidebar looks exactly
			// as it did before this PR.
			if preview := renderPreviewRow(si, innerW, selected); preview != "" {
				rows = append(rows, preview)
			}
		}
	}

	// Pad to full height.
	for len(rows) < paneH {
		rows = append(rows, "")
	}
	rows = rows[:paneH]

	content := strings.Join(rows, "\n")
	return m.focusedBorderStyle(focusSessions).Width(innerW).Height(paneH).Render(content)
}

// viewRightPane renders the event stream for the subscribed session.
func (m Model) viewRightPane(width int) string {
	innerW := width - 2
	paneH := m.rightPaneHeight()

	var rows []string

	// Header. Same focus marker rule as the left pane.
	title := "Events"
	if m.subscribedTo != "" {
		title = "Events: " + m.subscribedTo
	}
	if m.focus == focusEvents {
		title = "▸ " + title
	}
	rows = append(rows, styleHeader.Render(padRight(truncate(title, innerW), innerW)))
	rows = append(rows, styleDim.Render(strings.Repeat("─", innerW)))

	contentH := paneH - 2 // minus header and separator
	if contentH < 1 {
		contentH = 1
	}

	if len(m.eventLines) == 0 {
		rows = append(rows, "")
		if m.subscribedTo != "" {
			rows = append(rows, styleDim.Render(padRight("  waiting for events…", innerW)))
		} else {
			rows = append(rows, styleDim.Render(padRight("  select a session to stream events", innerW)))
		}
	} else {
		// Render from the bottom up, respecting scroll offset.
		total := len(m.eventLines)
		end := total - m.eventScroll
		if end < 0 {
			end = 0
		}
		if end > total {
			end = total
		}
		start := end - contentH
		if start < 0 {
			start = 0
		}
		coord := m.focusedIsCoordinator()
		for _, line := range m.eventLines[start:end] {
			rendered := styleEventLine(line, innerW, coord)
			rows = append(rows, rendered)
		}
	}

	// Scroll indicator.
	if m.eventScroll > 0 {
		rows = append(rows, styleDim.Render(
			padRight(fmt.Sprintf("  ↑ %d lines above (PgDn to scroll down)", m.eventScroll), innerW),
		))
	}

	// Pad to full height.
	for len(rows) < paneH {
		rows = append(rows, "")
	}
	rows = rows[:paneH]

	content := strings.Join(rows, "\n")
	return m.focusedBorderStyle(focusEvents).Width(innerW).Height(paneH).Render(content)
}

// styleEventLine applies colour to a NarrativeLine, with per-event-type
// visual treatments per the design-doc renderer table (issue #1767):
//
//   - state_change            — dim single line (was green; design doc
//     calls for "Dim status line" so the pane reads as conversation
//     content first, state-changes second).
//   - msg_assistant + _body   — normal-foreground rich text.
//   - msg_user + _body        — blue (operator input visually
//     distinguished from assistant output).
//   - tool_call               — styleToolCall: blue + bold (legacy
//     one-line card path / orphan tool_result fallback).
//   - tool_card_header_done   — styleToolCall: blue + bold; header
//     line of a completed (paired) tool card.
//   - tool_card_header_inflight + tool_card_status_inflight —
//     styleToolCallInFlight: yellow + bold; visually flags "still
//     running" without using the red error palette.
//   - tool_card_args / _args_full / _status_done / _result_full —
//     styleToolCardArgs: dim; supplementary args/result rows beneath
//     the header line.
//   - tool_result             — dim with the leading-indent prefix
//     from the renderer (orphan-result fallback only; the paired
//     path renders via the card status line).
//   - extension_error + _body — prominent red-on-red block. Both lines
//     carry the same treatment so the header + message read as one
//     emergency unit. styleErrorProminent uses bold to break out of
//     the surrounding muted palette — extension errors are fatal-class
//     (#1757) and should not be possible to miss.
//   - permission_ask / permission_denied / error — red foreground
//     (pre-existing behaviour preserved).
//   - unknown types           — dim fallback (also pre-existing).
//
// The `coordinator` flag (issue #1772 child 7) gates the prominent
// rendering of session.escalated and merge-queue notification rows.
// On a non-coordinator session those rows fall through to their
// non-prominent treatments (default dim fallback for
// session.escalated; ordinary msg_user blue for merge-queue rows that
// have NOT been re-labelled). On a coordinator session the two
// signal classes get their dedicated bold/coloured/backgrounded
// styles so they read as urgent attention items.
func styleEventLine(line narrative.NarrativeLine, width int, coordinator bool) string {
	text := truncate(line.Text, width)
	switch line.EventType {
	case evTypeStateChange:
		return styleDim.Render(padRight(text, width))
	case evTypeMsgAssistant, evTypeMsgAssistant + "_body":
		return styleNormal.Render(padRight(text, width))
	case evTypeMsgUser, evTypeMsgUser + "_body":
		return styleBlue.Render(padRight(text, width))
	case evTypeToolCall:
		return styleToolCall.Render(padRight(text, width))
	case evTypeToolCardHeaderDone:
		return styleToolCall.Render(padRight(text, width))
	case evTypeToolCardHeaderInFlight, evTypeToolCardStatusInFlight:
		return styleToolCallInFlight.Render(padRight(text, width))
	case evTypeToolCardArgs, evTypeToolCardArgsFull, evTypeToolCardStatusDone, evTypeToolCardResultFull:
		return styleToolCardArgs.Render(padRight(text, width))
	case evTypeToolResult:
		return styleDim.Render(padRight(text, width))
	case evTypeExtensionError, evTypeExtensionErrorBody:
		return styleErrorProminent.Render(padRight(text, width))
	case evTypePermissionAsk:
		return styleError.Render(padRight(text, width))
	case evTypePermDenied:
		return styleError.Render(padRight(text, width))
	case evTypeSessionEscalated:
		if coordinator {
			return styleEscalation.Render(padRight(text, width))
		}
		// Non-coordinator sessions still see the row (a worker
		// viewing its own escalated event), just not the prominent
		// coordinator-attention treatment. Use styleError so the
		// row still reads as significant without the loud
		// background.
		return styleError.Render(padRight(text, width))
	case evTypeMergeQueueNotification:
		if coordinator {
			return styleMergeQueue.Render(padRight(text, width))
		}
		// Non-coordinator: should be unreachable because the
		// re-label only fires when accumulateCoordinatorEvent
		// recognises a merge-queue text on a session named
		// `<repo>@<main>`. Fall back to msg_user styling defensively
		// so an out-of-band frame does not crash the renderer.
		return styleBlue.Render(padRight(text, width))
	case "error":
		return styleError.Render(padRight(text, width))
	default:
		return styleDim.Render(padRight(text, width))
	}
}

// viewStatusLine renders the bottom status-line strip (#1767). Shows
// the focused session's state, model, and cumulative cost when
// available. When the focused session has no captured model (no
// msg_assistant with a Model field yet) and no cost, the strip
// degrades to just the session name + state — deliberately not
// rendering the literal string "<nil>" or "$0.00" placeholders, which
// were both review-time gotchas on previous TUI work. The strip is one
// row tall (statusBarHeight) with no border, separated from the panes
// above by the panes' own border and from the prompt below by the
// prompt's border.
func (m Model) viewStatusLine(width int) string {
	if width < 1 {
		return ""
	}
	// Find the focused session. We key off m.subscribedTo because the
	// status line follows the conversation pane's subject, not the
	// cursor (which can transiently differ during session switching).
	// When nothing is subscribed (empty list, pre-snapshot), render a
	// dim placeholder so the layout doesn't collapse.
	if m.subscribedTo == "" {
		return styleDim.Render(padRight("  no session selected", width))
	}
	var focused *sessionItem
	for i := range m.sessions {
		if m.sessions[i].snap.Name == m.subscribedTo {
			focused = &m.sessions[i]
			break
		}
	}
	if focused == nil {
		return styleDim.Render(padRight("  "+m.subscribedTo, width))
	}

	// Compose: "<name> · <state> · <model> · $<cost>". Each segment is
	// included only when it has a value, so the strip degrades from the
	// full four-segment form (active session, msg_assistant flowed) down
	// to two segments (session name + state) for newly-spawned sessions
	// that haven't produced any msg_assistant yet.
	parts := []string{focused.snap.Name}
	if s := focused.snap.State; s != "" {
		parts = append(parts, s)
	}
	if model := focused.lastModel; model != "" {
		parts = append(parts, model)
	}
	if cost := focused.cumulativeCost; cost > 0 {
		// 4 decimal places when the cost is sub-cent so a $0.0001
		// turn doesn't collapse to "$0.00"; 2 decimals otherwise so
		// the common case reads as "$1.23".
		var costStr string
		if cost < 0.01 {
			costStr = fmt.Sprintf("$%.4f", cost)
		} else {
			costStr = fmt.Sprintf("$%.2f", cost)
		}
		parts = append(parts, costStr)
	}
	line := "  " + strings.Join(parts, " · ")
	return styleStatusLine.Render(padRight(truncate(line, width), width))
}

// viewPrompt renders the bottom prompt input.
func (m Model) viewPrompt(width int) string {
	innerW := width - 2
	if innerW < 1 {
		innerW = 1
	}

	var label string
	if m.subscribedTo != "" {
		label = fmt.Sprintf("prompt → %s: ", m.subscribedTo)
	} else {
		label = "prompt: "
	}
	if m.focus == focusPrompt {
		label = "▸ " + label
	}

	labelStyle := styleHeader
	labelRendered := labelStyle.Render(label)

	// Cursor rendering.
	before := string(m.promptRunes[:m.promptCursor])
	after := string(m.promptRunes[m.promptCursor:])
	var caretStr string
	if len(after) > 0 {
		caretRunes := []rune(after)
		caretStr = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colBg0)).
			Background(lipgloss.Color(colPrimary)).
			Render(string(caretRunes[0]))
		after = string(caretRunes[1:])
	} else {
		caretStr = styleDim.Render("█")
	}

	inputLine := labelRendered + before + caretStr + after

	// When the model has a transient errorMsg (e.g. a C-o keypress on
	// a non-coordinator session — #1772), surface it in the help row
	// so the operator sees feedback without an additional overlay.
	// The styling matches the picker overlay's errorMsg row (warning
	// glyph + red foreground) for visual consistency.
	var help string
	if m.errorMsg != "" {
		help = styleError.Render("  ⚠ " + m.errorMsg)
	} else {
		help = styleDim.Render("  ↑/↓ session  enter send  pgup/pgdn scroll  C-f picker  C-w dash  ? help  q quit")
	}

	// Prompt box also gets a focus tint when Tab has parked focus on it.
	box := stylePromptBox
	if m.focus == focusPrompt {
		box = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color(colPrimary))
	}
	return box.Width(innerW).Render(inputLine + "\n" + help)
}

// --- Utilities ---

// padRight pads or truncates s to exactly w display columns.
func padRight(s string, w int) string {
	cols := displayWidth(s)
	if cols >= w {
		return s
	}
	return s + strings.Repeat(" ", w-cols)
}

// truncate truncates s to at most w display columns, appending "…" if needed.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	cols := displayWidth(s)
	if cols <= w {
		return s
	}
	// Trim runes until we fit.
	runes := []rune(s)
	result := runes
	for displayWidth(string(result)) > w-1 && len(result) > 0 {
		result = result[:len(result)-1]
	}
	return string(result) + "…"
}

// displayWidth returns the display column width of s (approximated as rune count
// for ASCII-heavy content; full unicode width would require a heavier dependency).
func displayWidth(s string) int {
	return utf8.RuneCountInString(s)
}

// stateLabel returns a short coloured state label.
func stateLabel(state string) string {
	switch state {
	case "active":
		return styleGreen.Render("active  ")
	case "waiting":
		return styleYellow.Render("waiting ")
	case "spawning":
		return styleBlue.Render("spawning")
	case "finished":
		return styleDim.Render("finished")
	case "error":
		return styleError.Render("error   ")
	default:
		return styleDim.Render(padRight(state, 8))
	}
}

// formatRelTime returns a short relative-time string (≤ 8 chars).
func formatRelTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Run starts the bubbletea program and connects the daemon client.
// It blocks until the user quits.
func Run(sockPath string) error {
	return RunFocused(sockPath, "")
}

// RunFocused is like Run but pre-selects a session by name on the first
// sessions_snapshot frame. Used by `iris tui --session <name>` so the
// context-switcher picker can hand off to the TUI focused on a specific
// session. An empty initialSession is equivalent to Run.
func RunFocused(sockPath, initialSession string) error {
	client := NewDaemonClient(sockPath)
	m := NewModelFocused(client, initialSession)

	opts := []tea.ProgramOption{
		tea.WithAltScreen(),
	}
	p := tea.NewProgram(m, opts...)
	client.SetProgram(p)

	// Connect asynchronously so the TUI can render the disconnected state
	// immediately while the dial is in progress.
	go client.Connect()

	_, err := p.Run()
	return err
}
