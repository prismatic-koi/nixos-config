package iris

import (
	"time"
)

// SessionState represents the lifecycle state of an iris session.
// Transitions follow the sequence spawning → active → finished (clean) or
// spawning → active → error (crash/restart exhausted).
type SessionState string

const (
	// StateSpawning means the session record has been created but the pi child
	// has not yet signalled readiness (no hello frame received).
	StateSpawning SessionState = "spawning"

	// StateActive means the pi child is running and the harness socket
	// handshake has completed.
	StateActive SessionState = "active"

	// StateWaiting means the pi child has paused for user input — typically
	// when an assistant turn has finished and the next user message has not
	// arrived. Emitted by the pi extension as a state_change frame on the
	// harness socket and surfaced so coordinators (e.g. `iris prompt`'s
	// waiting-state guard, #1689) can distinguish an idle-but-attentive
	// session from one that is still working. Use the literal lowercase
	// string "waiting" — matches prism's agent.StateWaiting; do not drift.
	StateWaiting SessionState = "waiting"

	// StateFinished means the session ended cleanly (session_shutdown frame
	// received and pi exited with code 0).
	StateFinished SessionState = "finished"

	// StateError means the session ended uncleanly (non-zero exit that
	// exhausted the restart policy, or unrecoverable protocol error).
	StateError SessionState = "error"
)

// DefaultRestartThreshold is the maximum number of consecutive non-zero exits
// before the supervisor stops restarting and transitions to StateError.
// Matches the sidecar's DefaultSidecarCircuitBreakerThreshold = 3.
const DefaultRestartThreshold = 3

// RestartBackoff returns the backoff delay for the n-th restart attempt
// (1-indexed). Simple exponential backoff: 1s, 2s, 4s.
func RestartBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 1 * time.Second
	case attempt == 2:
		return 2 * time.Second
	default:
		return 4 * time.Second
	}
}

// SessionRecord holds the in-memory state for a managed iris session.
// It mirrors what would be written to the iris DB but kept in memory for
// the supervisor to reference.
type SessionRecord struct {
	// InstanceID is a UUID that uniquely identifies this session incarnation.
	InstanceID string
	// SessionName is the logical name (e.g. "nixos-config@my-branch").
	SessionName string
	// Worktree is the absolute path to the git worktree the pi child operates on.
	Worktree string
	// Role is the agent role (e.g. "worker", "coordinator").
	Role string
	// State is the current lifecycle state.
	State SessionState
	// HarnessSockPath is the absolute path to this session's harness socket.
	HarnessSockPath string
	// RestartCount is the number of consecutive non-zero exits seen so far.
	RestartCount int
	// RestartThreshold is the maximum before the circuit breaker opens.
	RestartThreshold int
	// StartedAt is when this incarnation was created.
	StartedAt time.Time
	// PiSessionPath is the full path to the pi JSONL session file, stored for
	// daemon-restart continuation (§8.2 of the design doc, Q5 of
	// pi-rpc-interface.md). Populated from the session_status frame.
	PiSessionPath string
	// BareRoot is the bare git repository root for this session (the directory
	// containing the .bare subdirectory). Used by the bash sandbox to derive
	// the GitHub account for the 4-PAT GITHUB_TOKEN selection. May be empty
	// when the worktree is not associated with a known bare repo — in that
	// case the bash sandbox falls back to the host GITHUB_TOKEN.
	BareRoot string
	// cleanExit is set to true when a session_shutdown frame is received
	// before the pi child exits — used by the supervisor to distinguish clean
	// from unclean exits.
	cleanExit bool
}
