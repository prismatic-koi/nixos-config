package db

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// IrisSessionRow holds the columns from the sessions table that iris needs
// for its restore path. Returned by IrisSessionsToRestore.
type IrisSessionRow struct {
	InstanceID       string
	SessionName      string
	Worktree         string
	Role             string // from agent_role
	HarnessSessionID string // pi session UUID, used to find the JSONL file
	IrisState        string // "spawning" or "active"
	StartedAt        time.Time
}

// IrisSessionsToRestore returns all sessions in the iris DB that were in
// "spawning", "active", or "waiting" state when the daemon died (i.e. sessions
// that have a non-terminal iris_state). These are the candidates for orphan
// detection and re-spawn.
//
// The harness column is used to restrict to iris-managed sessions (harness='pi').
// Sessions with end_state IS NOT NULL (finished/error) are excluded.
//
// Sessions in "waiting" state (paused for user input at crash time, issue
// #1701) are restored via the same path as "active": pi is re-spawned with
// the previous JSONL session file. Restored sessions transition through
// StateSpawning → StateActive on re-handshake; if pi is still paused for
// input, the extension will emit state_change="waiting" again on the next
// pause and the session converges back to waiting.
func (d *DB) IrisSessionsToRestore() ([]IrisSessionRow, error) {
	const q = `
SELECT instance_id, session_name, COALESCE(worktree, ''), COALESCE(agent_role, ''),
       COALESCE(harness_session_id, ''), COALESCE(iris_state, ''), started_at
  FROM sessions
 WHERE harness = 'pi'
   AND end_state IS NULL
   AND iris_state IN ('spawning', 'active', 'waiting')
 ORDER BY started_at ASC`
	rows, err := d.conn.Query(q)
	if err != nil {
		return nil, fmt.Errorf("db: iris sessions to restore: %w", err)
	}
	defer rows.Close()

	var result []IrisSessionRow
	for rows.Next() {
		var r IrisSessionRow
		var startedAtMs int64
		if scanErr := rows.Scan(&r.InstanceID, &r.SessionName, &r.Worktree,
			&r.Role, &r.HarnessSessionID, &r.IrisState, &startedAtMs); scanErr != nil {
			return nil, fmt.Errorf("db: iris sessions to restore: scan: %w", scanErr)
		}
		r.StartedAt = time.UnixMilli(startedAtMs)
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iris sessions to restore: iterate: %w", err)
	}
	return result, nil
}

// IrisUpdateSessionState sets the iris_state column on the sessions row for
// instanceID. Called by the iris supervisor on each state transition.
//
// It is a no-op when no row exists for instanceID (returns nil).
func (d *DB) IrisUpdateSessionState(instanceID, irisState string) error {
	const q = `UPDATE sessions SET iris_state = ? WHERE instance_id = ?`
	_, err := d.conn.Exec(q, irisState, instanceID)
	if err != nil {
		return fmt.Errorf("db: iris update session state: %w", err)
	}
	return nil
}

// IrisOrphanedToolCallID holds the tool call ID and session context for a
// tool_call event that has no matching tool_result event.
type IrisOrphanedToolCallID struct {
	ToolCallID  string
	SessionName string
	Worktree    string
	InstanceID  string // may be "" when not set on the event
}

// IrisOrphanedToolCalls returns all tool_call events for the given session
// instance that have no matching tool_result event. A "match" is defined as a
// tool_result event whose JSON payload contains the same tool call id.
//
// This is the orphan detector for D-9. It is called on daemon restart for each
// session in the "active" state.
func (d *DB) IrisOrphanedToolCalls(instanceID string) ([]IrisOrphanedToolCallID, error) {
	// Fetch all tool_call events for this instance with their id field.
	const q = `
SELECT JSON_EXTRACT(payload, '$.id') AS tool_call_id,
       session_name, COALESCE(worktree, ''), COALESCE(instance_id, '')
  FROM agent_events
 WHERE type = 'tool_call'
   AND instance_id = ?
   AND JSON_EXTRACT(payload, '$.id') IS NOT NULL
 ORDER BY created_at ASC`
	rows, err := d.conn.Query(q, instanceID)
	if err != nil {
		return nil, fmt.Errorf("db: iris orphaned tool calls: %w", err)
	}
	defer rows.Close()

	type candidateRow struct {
		toolCallID  string
		sessionName string
		worktree    string
		instanceID  string
	}
	var candidates []candidateRow
	for rows.Next() {
		var c candidateRow
		if scanErr := rows.Scan(&c.toolCallID, &c.sessionName, &c.worktree, &c.instanceID); scanErr != nil {
			return nil, fmt.Errorf("db: iris orphaned tool calls: scan: %w", scanErr)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iris orphaned tool calls: iterate: %w", err)
	}

	// For each candidate, check whether a tool_result with the same id exists.
	var orphans []IrisOrphanedToolCallID
	for _, c := range candidates {
		var count int
		const checkQ = `
SELECT COUNT(*) FROM agent_events
 WHERE type = 'tool_result'
   AND instance_id = ?
   AND JSON_EXTRACT(payload, '$.id') = ?`
		if err := d.conn.QueryRow(checkQ, instanceID, c.toolCallID).Scan(&count); err != nil {
			return nil, fmt.Errorf("db: iris orphaned tool calls: check result for %q: %w", c.toolCallID, err)
		}
		if count == 0 {
			orphans = append(orphans, IrisOrphanedToolCallID{
				ToolCallID:  c.toolCallID,
				SessionName: c.sessionName,
				Worktree:    c.worktree,
				InstanceID:  c.instanceID,
			})
		}
	}
	return orphans, nil
}

// IrisUpdateHarnessSessionID sets the harness_session_id column on the
// sessions row identified by instanceID. Called by the iris harness socket
// handler when the session_status frame from pi delivers the session UUID.
//
// Unlike UpdateHarnessSessionID (which operates on session_name and also
// writes to agent_status), this variant operates on instance_id and writes
// only to the sessions table — appropriate for iris daemon sessions which
// do not have an agent_status row.
func (d *DB) IrisUpdateHarnessSessionID(instanceID, harnessSessionID string) error {
	const q = `UPDATE sessions SET harness_session_id = ? WHERE instance_id = ?`
	_, err := d.conn.Exec(q, harnessSessionID, instanceID)
	if err != nil {
		return fmt.Errorf("db: iris update harness session id: %w", err)
	}
	return nil
}

// IrisSyntheticToolResult writes a synthetic tool_result event for an orphaned
// tool_call. The event payload has synthetic=true so downstream tooling can
// distinguish it from a genuine failure.
//
// Returns the DB row ID and the serialised payload string so the caller can
// publish the event to the client fan-out (D-6) without a second DB read.
// The event is associated with the given sessionName and instanceID (which may
// be "" when not set).
func (d *DB) IrisSyntheticToolResult(sessionName, worktree, toolCallID, instanceID string) (rowID int64, payload string, err error) {
	payloadMap := map[string]any{
		"type":      "tool_result",
		"id":        toolCallID,
		"success":   false,
		"isError":   true,
		"output":    "daemon restarted mid-call; tool result lost",
		"synthetic": true,
	}

	payloadBytes, merr := json.Marshal(payloadMap)
	if merr != nil {
		return 0, "", fmt.Errorf("db: iris synthetic tool result: marshal payload: %w", merr)
	}
	payload = string(payloadBytes)

	var iidPtr *string
	if instanceID != "" {
		iidPtr = &instanceID
	}

	event := Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Worktree:    worktree,
		Type:        "tool_result",
		Payload:     payload,
		CreatedAt:   time.Now(),
		InstanceID:  iidPtr,
	}
	rowID, err = d.WriteEventReturningRowID(event)
	return rowID, payload, err
}
