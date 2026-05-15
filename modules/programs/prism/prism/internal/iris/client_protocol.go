package iris

// client_protocol.go — wire-protocol frame types for the iris client IPC socket.
//
// The client IPC socket exposes the daemon's session management surface to
// user-facing clients: TUI, CLI, future web UI (§4.2 of daemon-mode-design.md).
//
// Framing: JSON-line (one JSON object per '\n'-terminated line), identical to
// the harness socket protocol and pi-wire-protocol.md §3. Both sides must
// tolerate unknown "type" values by logging and skipping them.
//
// Client → daemon frames:
//
//	sessions_list, session_subscribe, session_unsubscribe,
//	session_spawn, session_kill, prompt_deliver, ping
//
// Daemon → client frames:
//
//	sessions_snapshot, session_event, session_state, session_spawned,
//	error, pong

// ---------------------------------------------------------------------------
// Client → daemon request frames
// ---------------------------------------------------------------------------

// ClientSessionsListFrame requests a snapshot of all active sessions.
//
//	{"type": "sessions_list"}
type ClientSessionsListFrame struct {
	Type string `json:"type"` // "sessions_list"
}

// ClientSessionSubscribeFrame subscribes the client to a session's event
// stream. When SinceEventID is non-zero, the daemon first replays events
// with rowid > SinceEventID from the DB, then switches to live mode.
//
//	{"type": "session_subscribe", "name": "<session>"}
//	{"type": "session_subscribe", "name": "<session>", "since_event_id": 42}
type ClientSessionSubscribeFrame struct {
	Type         string `json:"type"` // "session_subscribe"
	Name         string `json:"name"`
	SinceEventID int64  `json:"since_event_id,omitempty"` // 0 means no replay
}

// ClientSessionUnsubscribeFrame cancels a subscription to a session's event
// stream. The connection stays open; other subscriptions or requests continue.
//
//	{"type": "session_unsubscribe", "name": "<session>"}
type ClientSessionUnsubscribeFrame struct {
	Type string `json:"type"` // "session_unsubscribe"
	Name string `json:"name"`
}

// ClientSessionSpawnFrame requests the daemon to spawn a new pi session.
//
//	{"type": "session_spawn", "worktree": "/abs/path", "role": "worker"}
//	{"type": "session_spawn", "worktree": "...", "role": "coordinator", "config_overrides": {...}}
type ClientSessionSpawnFrame struct {
	Type            string         `json:"type"` // "session_spawn"
	Worktree        string         `json:"worktree"`
	Role            string         `json:"role"`
	ConfigOverrides map[string]any `json:"config_overrides,omitempty"`
}

// ClientSessionKillFrame requests the daemon to kill a named session.
//
//	{"type": "session_kill", "name": "<session>"}
type ClientSessionKillFrame struct {
	Type string `json:"type"` // "session_kill"
	Name string `json:"name"`
}

// ClientPromptDeliverFrame delivers a prompt to a named session.
//
//	{"type": "prompt_deliver", "name": "<session>", "text": "..."}
//	{"type": "prompt_deliver", "name": "<session>", "text": "...", "deliver_as": "steer", "images": ["base64..."]}
type ClientPromptDeliverFrame struct {
	Type      string   `json:"type"` // "prompt_deliver"
	Name      string   `json:"name"`
	Text      string   `json:"text"`
	DeliverAs string   `json:"deliver_as,omitempty"` // "prompt", "steer", "follow_up"; default "prompt"
	Images    []string `json:"images,omitempty"`
}

// ClientPingFrame is a keepalive probe. The daemon responds with pong.
//
//	{"type": "ping"}
type ClientPingFrame struct {
	Type string `json:"type"` // "ping"
}

// ---------------------------------------------------------------------------
// Daemon → client response frames
// ---------------------------------------------------------------------------

// DaemonSessionsSnapshotFrame is the response to sessions_list.
// Sessions contains the list of known iris sessions with their current state.
//
//	{"type": "sessions_snapshot", "sessions": [...]}
type DaemonSessionsSnapshotFrame struct {
	Type     string            `json:"type"` // "sessions_snapshot"
	Sessions []SessionSnapshot `json:"sessions"`
}

// SessionSnapshot is a single session entry in a sessions_snapshot frame.
// Fields are a subset of the iris SessionRecord, sufficient for the TUI.
type SessionSnapshot struct {
	// Name is the logical session name (e.g. "nixos-config@my-branch").
	Name string `json:"name"`
	// InstanceID is the UUID for this session incarnation.
	InstanceID string `json:"instance_id"`
	// State is the lifecycle state ("spawning", "active", "finished", "error").
	State string `json:"state"`
	// Role is the agent role ("worker", "coordinator", etc.).
	Role string `json:"role"`
	// Worktree is the absolute path to the session's git worktree.
	Worktree string `json:"worktree"`
	// StartedAt is the RFC3339 timestamp when this session was created.
	StartedAt string `json:"started_at"`
}

// DaemonSessionEventFrame wraps a raw event payload from the subscribed
// session's event log. The Payload field is the verbatim JSON from the DB's
// agent_events.payload column. Clients use EventType and RowID for ordering
// and replay logic.
//
//	{"type": "session_event", "session_name": "...", "row_id": 42, "event_type": "tool_call", "payload": {...}}
type DaemonSessionEventFrame struct {
	Type        string `json:"type"`         // "session_event"
	SessionName string `json:"session_name"` // which session emitted this
	RowID       int64  `json:"row_id"`       // monotonic DB row ID; used for since_event_id replay
	EventType   string `json:"event_type"`   // agent_events.type field
	Payload     string `json:"payload"`      // verbatim agent_events.payload JSON
}

// DaemonSessionStateFrame notifies the client of a state transition for a
// subscribed session.
//
//	{"type": "session_state", "session_name": "...", "state": "active"}
type DaemonSessionStateFrame struct {
	Type        string `json:"type"`         // "session_state"
	SessionName string `json:"session_name"`
	State       string `json:"state"` // one of the SessionState constants
}

// DaemonSessionSpawnedFrame acknowledges a successful session_spawn.
//
//	{"type": "session_spawned", "name": "...", "instance_id": "..."}
type DaemonSessionSpawnedFrame struct {
	Type       string `json:"type"`        // "session_spawned"
	Name       string `json:"name"`        // the new session's logical name
	InstanceID string `json:"instance_id"` // the new session's instance UUID
}

// DaemonErrorFrame is an error response to a client request.
//
//	{"type": "error", "request_type": "session_subscribe", "message": "session not found"}
type DaemonErrorFrame struct {
	Type        string `json:"type"`         // "error"
	RequestType string `json:"request_type"` // the frame type that caused the error
	Message     string `json:"message"`
}

// DaemonPongFrame is the keepalive response to a ping.
//
//	{"type": "pong"}
type DaemonPongFrame struct {
	Type string `json:"type"` // "pong"
}

// Client-socket frame type constants.
const (
	ClientFrameSessionsList        = "sessions_list"
	ClientFrameSessionSubscribe    = "session_subscribe"
	ClientFrameSessionUnsubscribe  = "session_unsubscribe"
	ClientFrameSessionSpawn        = "session_spawn"
	ClientFrameSessionKill         = "session_kill"
	ClientFramePromptDeliver       = "prompt_deliver"
	ClientFramePing                = "ping"
	DaemonFrameSessionsSnapshot    = "sessions_snapshot"
	DaemonFrameSessionEvent        = "session_event"
	DaemonFrameSessionState        = "session_state"
	DaemonFrameSessionSpawned      = "session_spawned"
	DaemonFrameError               = "error"
	DaemonFramePong                = "pong"
)
