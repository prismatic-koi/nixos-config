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
//	session_spawn, session_kill, prompt_deliver, review_spawn,
//	escalation_deliver, ping
//
// Daemon → client frames:
//
//	sessions_snapshot, session_event, session_state, session_spawned,
//	session_killed, review_spawned, escalation_delivered, error, pong

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
//	{"type": "session_spawn", "worktree": "...", "role": "worker", "parent": "nixos-config@main"}
//
// Parent, when non-empty, names the session that is invoking `iris spawn`.
// The daemon records it on the new session's row (sessions.parent_session)
// so that the terminal-state notification path (issue #1700) can deliver a
// "Agent <name> has finished" prompt back to the parent. Empty means "no
// parent" (top-level spawn from a non-iris shell): no notification will be
// delivered when the child terminates.
type ClientSessionSpawnFrame struct {
	Type            string         `json:"type"` // "session_spawn"
	Worktree        string         `json:"worktree"`
	Role            string         `json:"role"`
	Parent          string         `json:"parent,omitempty"`
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

// ClientEscalationDeliverFrame is the worker-side wire frame for
// `iris escalate` (#1693). The worker sends this frame to the daemon; the
// daemon validates the calling session, resolves the coordinator target
// (auto-discovery or explicit --to), delivers the prompt body to the
// coordinator using the same path as prompt_deliver, and transitions the
// worker session to the "escalated" state.
//
//	{"type": "escalation_deliver", "from": "<worker>", "prompt": "..."}
//	{"type": "escalation_deliver", "from": "<worker>", "to": "<coord>", "prompt": "..."}
//
// When To is empty the daemon auto-discovers the coordinator by scanning
// in-memory active sessions for Role == "coordinator". Zero coordinators
// still transitions the worker to escalated (a self-marker is written; no
// prompt is delivered — the issue body documents this is the
// human-picks-up-via-tmux path). Multiple coordinators with no To set is
// rejected with an error listing the candidates.
//
// DeliveryID, when non-empty, is forwarded to the underlying prompt path
// so the coordinator's harness can dedup retries. The daemon mints one
// per call when the field is empty (#1695).
type ClientEscalationDeliverFrame struct {
	Type       string `json:"type"` // "escalation_deliver"
	From       string `json:"from"`
	To         string `json:"to,omitempty"`
	Prompt     string `json:"prompt"`
	DeliveryID string `json:"delivery_id,omitempty"`
}

// ClientPingFrame is a keepalive probe. The daemon responds with pong.
//
//	{"type": "ping"}
type ClientPingFrame struct {
	Type string `json:"type"` // "ping"
}

// ClientReviewSpawnFrame requests the daemon to spawn a review group for
// the named parent session. The daemon spawns one session per agent in
// AgentNames (one of the canonical iris.ReviewAgentNames), registers them
// under a shared group_id in session_groups, and begins watching the group
// for completion. When all members reach a terminal state the daemon
// delivers a single review-complete prompt to the parent session via the
// existing prompt_deliver path (issue #1694).
//
//	{"type":"review_spawn","parent":"...","pr_number":"...","agents":["review-goal",...],"timeout":"10m","delivery_id":"<uuid>"}
type ClientReviewSpawnFrame struct {
	Type string `json:"type"` // "review_spawn"
	// Parent is the session that invoked `iris review`. The review-complete
	// prompt is delivered here when the group completes.
	Parent string `json:"parent"`
	// PRNumber is the GitHub PR being reviewed. Recorded for observability;
	// the daemon does not use it for routing.
	PRNumber string `json:"pr_number"`
	// AgentNames is the subset of canonical review agents to spawn. Empty
	// means "all" (default 5). Names not in iris.ReviewAgentNames are
	// rejected with a daemon-side error frame.
	AgentNames []string `json:"agents,omitempty"`
	// Timeout is the per-agent timeout encoded as a Go duration string
	// (e.g. "10m"). Empty means "daemon default" (10m).
	Timeout string `json:"timeout,omitempty"`
	// DeliveryID is a UUID minted by the calling CLI. The daemon forwards
	// it on the review-complete delivery so a defensive double-fire from
	// the watcher is deduplicated at the receiving sidecar (issue #1695).
	// May be empty for ad-hoc callers; the daemon mints one in that case.
	DeliveryID string `json:"delivery_id,omitempty"`
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
// Fields are a subset of the iris SessionRecord, sufficient for the TUI and
// for the `iris sessions list` / `iris sessions status` CLI commands.
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
	// HarnessSessionID is the harness-side session identifier — for the pi
	// harness this is the full path to the pi JSONL session file (a stable
	// identifier suitable for cross-referencing pi-side logs). Populated
	// within milliseconds of session start, once the pi child emits its
	// first session_status frame (handled by the iris harness socket and
	// mirrored into the in-memory SessionRecord under the supervisor's
	// lock). The transient window between session_start and that first
	// frame is the only time this field is empty for a live session;
	// restored sessions have it populated from the DB before any client
	// can observe them.
	HarnessSessionID string `json:"harness_session_id,omitempty"`
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
	Type        string `json:"type"` // "session_state"
	SessionName string `json:"session_name"`
	State       string `json:"state"` // one of the SessionState constants
}

// DaemonSessionSpawnedFrame acknowledges a successful session_spawn and
// carries a snapshot of the newly-spawned session so the requesting client
// (and any future fan-out subscribers) can update its session list without
// re-issuing a sessions_list request.
//
// The Session field is optional for wire-format backwards compatibility:
// older daemons may emit the frame with only Name and InstanceID, in which
// case clients should treat the absence of Session as a signal to refresh
// via sessions_list. New daemons populate Session unconditionally.
//
//	{"type": "session_spawned", "name": "...", "instance_id": "...",
//	 "session": {"name": "...", "instance_id": "...", "state": "spawning",
//	              "role": "worker", "worktree": "...", "started_at": "..."}}
type DaemonSessionSpawnedFrame struct {
	Type       string           `json:"type"`              // "session_spawned"
	Name       string           `json:"name"`              // the new session's logical name
	InstanceID string           `json:"instance_id"`       // the new session's instance UUID
	Session    *SessionSnapshot `json:"session,omitempty"` // full snapshot of the new session (optional for backwards compat)
}

// DaemonEscalationDeliveredFrame acknowledges a successful
// escalation_deliver. The frame carries the resolved coordinator name
// (empty when no coordinator was found and the worker entered escalated
// state without delivery) and the delivery_id used at the prompt path so
// callers can correlate audit rows.
//
//	{"type": "escalation_delivered", "from": "<worker>", "to": "<coord>",
//	 "delivery_id": "...", "delivered": true}
//	{"type": "escalation_delivered", "from": "<worker>", "to": "",
//	 "delivery_id": "", "delivered": false}    // zero-coordinator branch
type DaemonEscalationDeliveredFrame struct {
	Type       string `json:"type"` // "escalation_delivered"
	From       string `json:"from"`
	To         string `json:"to,omitempty"`
	DeliveryID string `json:"delivery_id,omitempty"`
	Delivered  bool   `json:"delivered"`
}

// DaemonSessionKilledFrame acknowledges a successful session_kill. The
// State field reports the terminal state the session settled into — one of
// "finished" (clean SIGTERM exit), "error" (SIGKILL escalation), or
// "already_terminal" (the session was already in a terminal state and the
// kill was a no-op). Clients receive this frame whether the kill cancelled
// a live pi child or completed idempotently against an already-finished
// session.
//
//	{"type": "session_killed", "name": "...", "state": "finished"}
type DaemonSessionKilledFrame struct {
	Type  string `json:"type"`  // "session_killed"
	Name  string `json:"name"`  // session name from the request frame
	State string `json:"state"` // "finished" | "error" | "already_terminal"
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

// DaemonReviewSpawnedFrame acknowledges a successful review_spawn request.
// It carries the group_id, the round number (1-indexed, derived from the
// parent's prior `~review-N-<agent>` rows), and the per-agent session
// names so the caller can print a deterministic acknowledgement.
//
//	{"type":"review_spawned","group_id":"<uuid>","parent":"...","round":1,"members":[{"agent":"review-goal","session":"..."},...]}
type DaemonReviewSpawnedFrame struct {
	Type    string                    `json:"type"` // "review_spawned"
	GroupID string                    `json:"group_id"`
	Parent  string                    `json:"parent"`
	Round   int                       `json:"round"`
	Members []DaemonReviewGroupMember `json:"members"`
}

// DaemonReviewGroupMember is one entry in DaemonReviewSpawnedFrame.Members.
type DaemonReviewGroupMember struct {
	Agent       string `json:"agent"`        // role name, e.g. "review-goal"
	SessionName string `json:"session_name"` // full session name spawned for this agent
}

// Client-socket frame type constants.
const (
	ClientFrameSessionsList       = "sessions_list"
	ClientFrameSessionSubscribe   = "session_subscribe"
	ClientFrameSessionUnsubscribe = "session_unsubscribe"
	ClientFrameSessionSpawn       = "session_spawn"
	ClientFrameSessionKill        = "session_kill"
	ClientFramePromptDeliver       = "prompt_deliver"
	ClientFrameReviewSpawn         = "review_spawn"
	ClientFrameEscalationDeliver   = "escalation_deliver"
	ClientFramePing                = "ping"
	DaemonFrameSessionsSnapshot    = "sessions_snapshot"
	DaemonFrameSessionEvent        = "session_event"
	DaemonFrameSessionState        = "session_state"
	DaemonFrameSessionSpawned      = "session_spawned"
	DaemonFrameSessionKilled       = "session_killed"
	DaemonFrameReviewSpawned       = "review_spawned"
	DaemonFrameEscalationDelivered = "escalation_delivered"
	DaemonFrameError               = "error"
	DaemonFramePong                = "pong"
)
