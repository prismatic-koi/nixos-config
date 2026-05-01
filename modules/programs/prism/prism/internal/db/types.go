package db

import "time"

// Event represents a row in the agent_events table.
type Event struct {
	ID               string
	SessionName      string
	Repo             string
	Worktree         string
	HarnessSessionID *string
	// InstanceID links this event to a row in the sessions table.
	// NULL for legacy events and for call sites that do not have a known
	// instance_id (e.g. the pane-died hook before a session_start row exists).
	InstanceID *string
	Type       string
	Payload    string // raw JSON
	CreatedAt  time.Time
}

// Status represents a row in the agent_status table.
type Status struct {
	SessionName      string
	Repo             string
	Worktree         string
	State            string
	Title            *string
	AgentName        *string
	ModelID          *string
	RootAgentName    *string
	RootModelID      *string
	IsolationMode    string // "podman", "bwrap", or "host"; "" means not recorded (back-compat)
	InstanceID       *string
	LastSeen         time.Time
	EndedAt          *time.Time
	Harness          *string
	HarnessSessionID *string
	HarnessPort      *int
	// GroupID is the session_groups.group_id this session belongs to, or nil
	// when this session is not part of a group. Populated by SpawnSession
	// when opts.GroupID is non-empty (see #849 §3.1 and #859).
	GroupID *string
}

// BusMessage represents a row in the bus_messages table.
type BusMessage struct {
	ID           string
	FromSession  string
	ToSession    string
	ToInstanceID *string
	Repo         string
	Text         string
	Urgency      string
	SentAt       time.Time
	DeliveredAt  *time.Time
	FailedAt     *time.Time
}

// Session represents a row in the sessions table.
// Each row is immutable per incarnation: it is inserted on session start
// and updated only on session cleanup (ended_at, end_state).
type Session struct {
	InstanceID       string
	SessionName      string
	AgentRole        *string
	RootAgentName    *string
	Repo             string
	Worktree         string
	Harness          string
	HarnessSessionID *string
	GroupID          *string
	StartedAt        time.Time
	EndedAt          *time.Time
	EndState         *string
	ArchivePath      *string
	PrismVersion     *string
}

// GroupMemberResult holds the terminal state and last assistant message for a
// single member of a session group. Used by GroupResults to aggregate outcomes.
type GroupMemberResult struct {
	SessionName string
	RootAgent   string // from root_agent_name; empty when not set
	State       string // terminal state: finished / interrupted / error / deleted
	LastMessage  string // last assistant turn from agent_events; empty when none
	StartupError string // reason from startup_error event; empty when not a no-start failure
}

// TokenTurn holds per-turn token and cost data for a single msg_assistant event.
// Used by SessionTurnTokens to return per-turn data for cost calculation.
type TokenTurn struct {
	Model      string
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
	EventCost  float64
}

// PendingMerge represents a row in the pending_merges table.
type PendingMerge struct {
	PR            int
	SessionName   string
	InstanceID    string
	QueuePosition int64
	Status        string // 'watching' | 'merged' | 'failed' | 'cancelled' | 'abandoned'
	Title         *string
	Error         *string
	QueuedAt      time.Time
	LastCheckedAt *time.Time
	MergedAt      *time.Time
	EndedAt       *time.Time
}

// SpawnOutcome represents a row in the spawn_outcome table. All pointer fields
// are NULL-able; zero-value pointers indicate the signal was not available for
// this run (e.g. exit_code is always nil because the sidecar does not capture
// it today).
type SpawnOutcome struct {
	InstanceID string

	// Process-level
	EndState              *string
	ExitCode              *int
	DurationMs            *int64
	InterruptedCount      int
	CompactionCount       int
	ErrorEventCount       int
	PermissionAskCount    int
	PermissionDeniedCount int
	DoomLoopCount         int

	// Agent-level
	PRNumber        *int
	PRMergedAt      *int64 // ms epoch
	ReviewGroupID   *string
	ReviewVerdict   *string // "pass" | "fail" | "mixed" | nil
	ReviewPassCount *int
	ReviewFailCount *int
	ReviewNoneCount *int

	// Rubric-level (reserved; all nil until a grader mechanism lands)
	RubricVerdict   *string
	RubricScore     *float64
	RubricBreakdown *string // JSON
	RubricGrader    *string

	// Per-axis aggregations
	TokensInputTotal      int64
	TokensOutputTotal     int64
	TokensCacheReadTotal  int64
	TokensCacheWriteTotal int64
	CostUSDTotal          float64
	ToolCallCount         int
	ToolErrorCount        int
	MsgAssistantCount     int
	TimeToFirstEventMs    *int64
	TimeToFinishedMs      *int64

	// Audit
	ComputedAt    int64
	SchemaVersion int
}
