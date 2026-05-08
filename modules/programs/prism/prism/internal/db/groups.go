package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// terminalStates is the set of agent states that indicate a session has stopped
// working and will not make further progress.
//
// Note: this includes "deleted" (cleaned up mid-run) intentionally — a deleted
// session will never complete, so it counts as terminal for GroupCompleted
// purposes.
//
// Note: this deliberately EXCLUDES "interrupted". An interrupted session may
// resume — the user can redirect it via `prism prompt <agent-session>` and the
// agent will continue toward a terminal state (#1495). Treating "interrupted"
// as terminal here would close out a review group the moment the user reaches
// for Esc to redirect a review agent, contaminating the review-complete prompt
// with a false-error verdict before the redirection has a chance to take
// effect. The user's escape hatch for genuinely abandoning an interrupted
// agent is `prism cleanup --yes --session <agent>`, which transitions the
// session to "deleted" — and "deleted" IS terminal here.
//
// This differs from review.go's isTerminalState, which omits "deleted" because
// prism review uses separate cleanup detection logic.
var terminalStates = []string{"finished", "error", "deleted"}

// isTerminalState reports whether state is a terminal agent state.
func isTerminalState(state string) bool {
	for _, s := range terminalStates {
		if state == s {
			return true
		}
	}
	return false
}

// RegisterGroup inserts a new row into session_groups and returns the generated
// group_id. parent_session identifies the session that owns this group (e.g.
// the worker running `prism review`).
func (d *DB) RegisterGroup(parentSession string) (string, error) {
	groupID := uuid.New().String()
	const q = `INSERT INTO session_groups (group_id, parent_session) VALUES (?, ?)`
	if _, err := d.conn.Exec(q, groupID, parentSession); err != nil {
		return "", fmt.Errorf("db: register group: %w", err)
	}
	return groupID, nil
}

// GroupCompleted reports whether every agent_status row with this group_id has
// reached a terminal state. A row is considered terminal when EITHER:
//
//   - its `state` is in terminalStates (finished, error, deleted), OR
//   - its `ended_at` is non-NULL (the session row has been closed by
//     `prism cleanup`, `prism reset`, or any other lifecycle path that
//     calls SetEnded / MarkAllEnded).
//
// The `ended_at` arm is what makes the user's escape hatch work. When the
// user runs `prism cleanup --yes --session <interrupted-agent>` to abandon
// an interrupted review agent, the cleanup path:
//
//   1. SIGTERMs the sidecar, which writes state="interrupted".
//   2. Calls SetEnded(session), which sets ended_at but leaves state alone.
//
// Without the `ended_at` arm here, an interrupted-then-cleaned-up agent's
// row would have state="interrupted" forever, which is no longer terminal
// per terminalStates (#1495), and GroupCompleted would never return true.
// The review monitor would then spin until its overall safety timeout.
// Treating `ended_at IS NOT NULL` as terminal closes that gap for any path
// that ends a session row without rewriting state — not just `prism cleanup`,
// but also `prism reset`'s MarkAllEnded.
//
// Returns (true, nil) when all members are terminal (including the case where
// there are no members yet — caller should guard against that if needed).
// Returns (false, nil) when at least one member is still running.
// Returns (false, err) on a database error.
func (d *DB) GroupCompleted(groupID string) (bool, error) {
	// Build the NOT IN list for terminal states.
	placeholders := make([]string, len(terminalStates))
	args := make([]any, 0, 1+len(terminalStates))
	args = append(args, groupID)
	for i, s := range terminalStates {
		placeholders[i] = "?"
		args = append(args, s)
	}
	q := `SELECT COUNT(*) FROM agent_status
          WHERE group_id = ?
            AND state NOT IN (` + strings.Join(placeholders, ",") + `)
            AND ended_at IS NULL`
	var nonTerminalCount int
	if err := d.conn.QueryRow(q, args...).Scan(&nonTerminalCount); err != nil {
		return false, fmt.Errorf("db: group completed: %w", err)
	}
	return nonTerminalCount == 0, nil
}

// GroupResults returns the terminal state and last assistant message for every
// member of the group, keyed by session_name. It is intended for use after
// GroupCompleted returns true. Members that are still active are included but
// their State may be non-terminal.
//
// Rows whose `ended_at` is non-NULL are intentionally EXCLUDED from the
// returned map. This is what makes the user's escape hatch flow correctly
// through `buildMonitorResults`'s missing-session branch (#1495): when the
// user runs `prism cleanup --yes --session <interrupted-agent>` to abandon
// an interrupted review agent, the cleanup path sets ended_at without
// rewriting state. By dropping ended rows here, the cleaned-up agent
// surfaces as "session not found in group" — IsError=true with the
// pre-existing missing-session message — and the remaining agents are
// reported normally. (For any race in which a normally-finished agent has
// ended_at set before the monitor reads results, that agent's verdict is
// also dropped; this is rare and acceptable, since the user must have
// initiated cleanup mid-review.)
//
// LastMessage is populated from the most recent msg_assistant event payload
// for each session. It is empty when no such event has been recorded.
func (d *DB) GroupResults(groupID string) (map[string]GroupMemberResult, error) {
	// Fetch each member's session_name, state, and root_agent_name.
	// Exclude rows that have been ended (ended_at IS NOT NULL) — see comment above.
	const statusQ = `
SELECT session_name, state, COALESCE(root_agent_name, '')
FROM agent_status
WHERE group_id = ? AND ended_at IS NULL`
	rows, err := d.conn.Query(statusQ, groupID)
	if err != nil {
		return nil, fmt.Errorf("db: group results: query statuses: %w", err)
	}
	defer rows.Close()

	results := make(map[string]GroupMemberResult)
	for rows.Next() {
		var r GroupMemberResult
		if err := rows.Scan(&r.SessionName, &r.State, &r.RootAgent); err != nil {
			return nil, fmt.Errorf("db: group results: scan status: %w", err)
		}
		results[r.SessionName] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: group results: iterate statuses: %w", err)
	}

	// For each member, fetch the last msg_assistant event payload.
	for name, r := range results {
		const msgQ = `
SELECT payload FROM agent_events
WHERE session_name = ? AND type = 'msg_assistant'
ORDER BY created_at DESC, rowid DESC
LIMIT 1`
		var payload string
		err := d.conn.QueryRow(msgQ, name).Scan(&payload)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("db: group results: last message for %q: %w", name, err)
		}
		r.LastMessage = payload

		// Fetch the startup_error event reason when present. This is written by
		// writeStartupError in the sidecar when WaitHealthy or CreateSession
		// fails, allowing the review monitor to distinguish a no-start failure
		// from a mid-run crash (#1222).
		const startupErrQ = `
SELECT payload FROM agent_events
WHERE session_name = ? AND type = 'startup_error'
ORDER BY created_at DESC, rowid DESC
LIMIT 1`
		var startupErrPayload string
		seErr := d.conn.QueryRow(startupErrQ, name).Scan(&startupErrPayload)
		if seErr == nil && startupErrPayload != "" {
			// Extract the "reason" field from the JSON payload.
			var p struct {
				Reason string `json:"reason"`
			}
			if jsonErr := json.Unmarshal([]byte(startupErrPayload), &p); jsonErr == nil && p.Reason != "" {
				r.StartupError = p.Reason
			} else {
				r.StartupError = startupErrPayload
			}
		}

		results[name] = r
	}

	return results, nil
}

// IsGroupMember returns true when sessionName has a non-NULL group_id in
// agent_status (i.e. it belongs to a session group). Returns false for
// pre-migration rows where group_id is NULL.
func (d *DB) IsGroupMember(sessionName string) (bool, error) {
	var groupID sql.NullString
	const q = `SELECT group_id FROM agent_status WHERE session_name = ?`
	if err := d.conn.QueryRow(q, sessionName).Scan(&groupID); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("db: is group member: %w", err)
	}
	return groupID.Valid && groupID.String != "", nil
}

// HasReviewGroup returns true when sessionName is the parent_session of at
// least one row in session_groups. This is the DB-backed way to detect whether
// a session has spawned a review group (replacing the "~review" name heuristic).
func (d *DB) HasReviewGroup(parentSession string) (bool, error) {
	var count int
	const q = `SELECT COUNT(*) FROM session_groups WHERE parent_session = ?`
	if err := d.conn.QueryRow(q, parentSession).Scan(&count); err != nil {
		return false, fmt.Errorf("db: has review group: %w", err)
	}
	return count > 0, nil
}

// AllGroupParents returns a map of group_id → parent_session for all rows in
// session_groups. This is the efficient batch counterpart to ParentSessionFor:
// callers that need parent attribution for a large set of sessions can fetch the
// whole map in one query rather than issuing N individual lookups.
//
// The returned map only contains groups registered in session_groups; sessions
// whose group_id is NULL (pre-migration rows) or whose group_id has no matching
// session_groups row are absent from the map (callers should fall back to the
// name heuristic for those).
func (d *DB) AllGroupParents() (map[string]string, error) {
	const q = `SELECT group_id, parent_session FROM session_groups`
	rows, err := d.conn.Query(q)
	if err != nil {
		return nil, fmt.Errorf("db: all group parents: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var groupID, parent string
		if err := rows.Scan(&groupID, &parent); err != nil {
			return nil, fmt.Errorf("db: all group parents: scan: %w", err)
		}
		result[groupID] = parent
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: all group parents: iterate: %w", err)
	}
	return result, nil
}

// ParentSessionFor returns the authoritative parent session name for the given
// session. It is the single named source of truth for parent attribution,
// used by both the dashboard (via AllGroupParents + StatusToAgentSession) and
// prism list-sessions (via AllGroupParents in the renderSessionTable sort key).
//
// Resolution order:
//  1. DB-backed (post-migration): looks up session_groups.parent_session via
//     agent_status.group_id. This is the most reliable source — it records the
//     actual caller at spawn time.
//  2. Name-heuristic fallback (pre-migration rows where group_id IS NULL):
//     strips the "~…" suffix from the session name and returns the prefix as
//     the parent name. e.g. "nixos-config@main~review-1-review-code" → parent
//     is "nixos-config@main".
//
// Returns "" when no parent can be determined (top-level sessions, sessions
// with no group_id and no "~" in the branch component, or DB errors).
func (d *DB) ParentSessionFor(sessionName string) string {
	// Step 1: try DB-backed group_id → session_groups.parent_session.
	const q = `
SELECT sg.parent_session
FROM agent_status AS a
JOIN session_groups AS sg ON a.group_id = sg.group_id
WHERE a.session_name = ?`
	var parent string
	err := d.conn.QueryRow(q, sessionName).Scan(&parent)
	if err == nil && parent != "" {
		return parent
	}
	// err == sql.ErrNoRows or group_id IS NULL: fall through to name heuristic.

	// Step 2: name-heuristic fallback — strip the "~…" suffix from the branch
	// component.  Session names are of the form "repo@branch~suffix" where
	// "~suffix" marks a depth-2 review session.  The parent is "repo@branch".
	if idx := strings.Index(sessionName, "@"); idx >= 0 {
		branch := sessionName[idx+1:] // e.g. "main~review-1-review-code"
		if tildeIdx := strings.Index(branch, "~"); tildeIdx >= 0 {
			return sessionName[:idx] + "@" + branch[:tildeIdx]
		}
	}
	return ""
}

// GroupMembersForParent returns all agent_status rows whose group_id belongs
// to a session_groups row with parent_session = parentSession.
func (d *DB) GroupMembersForParent(parentSession string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id
FROM agent_status
WHERE group_id IN (SELECT group_id FROM session_groups WHERE parent_session = ?)`
	return d.queryStatuses(q, parentSession)
}
