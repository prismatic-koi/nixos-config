package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// scanSession scans a sessions row from the given scanner into a Session value.
func scanSession(s scanner) (*Session, error) {
	var sess Session
	var startedAt int64
	var endedAt sql.NullInt64
	var agentRole sql.NullString
	var rootAgentName sql.NullString
	var harnessSessionID sql.NullString
	var groupID sql.NullString
	var endState sql.NullString
	var archivePath sql.NullString
	var prismVersion sql.NullString

	err := s.Scan(
		&sess.InstanceID, &sess.SessionName, &agentRole, &rootAgentName,
		&sess.Repo, &sess.Worktree, &sess.Harness, &harnessSessionID, &groupID,
		&startedAt, &endedAt, &endState, &archivePath, &prismVersion,
	)
	if err != nil {
		return nil, err
	}
	sess.StartedAt = time.UnixMilli(startedAt)
	if endedAt.Valid {
		t := time.UnixMilli(endedAt.Int64)
		sess.EndedAt = &t
	}
	if agentRole.Valid {
		sess.AgentRole = &agentRole.String
	}
	if rootAgentName.Valid {
		sess.RootAgentName = &rootAgentName.String
	}
	if harnessSessionID.Valid {
		sess.HarnessSessionID = &harnessSessionID.String
	}
	if groupID.Valid {
		sess.GroupID = &groupID.String
	}
	if endState.Valid {
		sess.EndState = &endState.String
	}
	if archivePath.Valid {
		sess.ArchivePath = &archivePath.String
	}
	if prismVersion.Valid {
		sess.PrismVersion = &prismVersion.String
	}
	return &sess, nil
}

const sessionsSelectCols = `
SELECT instance_id, session_name, agent_role, root_agent_name,
       repo, worktree, harness, harness_session_id, group_id,
       started_at, ended_at, end_state, archive_path, prism_version
  FROM sessions`

// querySessions is a helper that runs a SELECT on sessions and scans rows.
func (d *DB) querySessions(q string, args ...any) ([]Session, error) {
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query sessions: %w", err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan session: %w", err)
		}
		sessions = append(sessions, *sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate sessions: %w", err)
	}
	return sessions, nil
}

// InsertSession inserts a new row into the sessions table for the given
// incarnation. Called from the tmux-session-start handler immediately after
// the instance_id is minted (or confirmed) for the session.
//
// Fields not yet known at session-start time (ended_at, end_state,
// archive_path) are stored as NULL. The harness field defaults to "pi"
// when empty. Inserting a duplicate instance_id is a no-op (INSERT OR IGNORE).
func (d *DB) InsertSession(s Session) error {
	if s.Harness == "" {
		s.Harness = "pi"
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = time.Now()
	}
	startedAt := s.StartedAt.UnixMilli()
	const q = `
INSERT OR IGNORE INTO sessions
  (instance_id, session_name, agent_role, root_agent_name, repo, worktree,
   harness, harness_session_id, group_id, started_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := d.conn.Exec(q,
		s.InstanceID, s.SessionName, s.AgentRole, s.RootAgentName,
		s.Repo, s.Worktree, s.Harness, s.HarnessSessionID, s.GroupID,
		startedAt,
	)
	if err != nil {
		return fmt.Errorf("db: insert session: %w", err)
	}
	return nil
}

// UpdateSessionEnded sets ended_at and end_state on the sessions row for
// instanceID. Called during prism cleanup to record when and how the session
// ended. archive_path remains NULL in this PR (populated in #997).
//
// It is a no-op when no row exists for instanceID (returns nil).
func (d *DB) UpdateSessionEnded(instanceID, endState string) error {
	now := time.Now().UnixMilli()
	const q = `
UPDATE sessions
   SET ended_at  = ?,
       end_state = ?
 WHERE instance_id = ?`
	_, err := d.conn.Exec(q, now, endState, instanceID)
	if err != nil {
		return fmt.Errorf("db: update session ended: %w", err)
	}
	return nil
}

// UpdateSessionArchivePath sets archive_path on the sessions row for
// instanceID. Called during prism cleanup after the archive copy completes
// successfully. It is a no-op when no row exists for instanceID (returns nil).
func (d *DB) UpdateSessionArchivePath(instanceID, archivePath string) error {
	const q = `UPDATE sessions SET archive_path = ? WHERE instance_id = ?`
	_, err := d.conn.Exec(q, archivePath, instanceID)
	if err != nil {
		return fmt.Errorf("db: update session archive path: %w", err)
	}
	return nil
}

// AllSessions returns all rows in the sessions table, ordered by started_at DESC.
func (d *DB) AllSessions() ([]Session, error) {
	return d.querySessions(sessionsSelectCols + ` ORDER BY started_at DESC`)
}

// SessionsForRepo returns all sessions rows for the given repo, ordered by
// started_at DESC.
func (d *DB) SessionsForRepo(repo string) ([]Session, error) {
	return d.querySessions(sessionsSelectCols+` WHERE repo = ? ORDER BY started_at DESC`, repo)
}

// SessionsSince returns all sessions rows where started_at >= sinceMs (Unix
// milliseconds), ordered by started_at DESC.
func (d *DB) SessionsSince(sinceMs int64) ([]Session, error) {
	return d.querySessions(sessionsSelectCols+` WHERE started_at >= ? ORDER BY started_at DESC`, sinceMs)
}

// SessionsForRepoSince returns all sessions rows for repo where started_at >=
// sinceMs, ordered by started_at DESC.
func (d *DB) SessionsForRepoSince(repo string, sinceMs int64) ([]Session, error) {
	return d.querySessions(sessionsSelectCols+` WHERE repo = ? AND started_at >= ? ORDER BY started_at DESC`, repo, sinceMs)
}

// SessionByInstanceID returns the sessions row with the given instance_id, or
// nil when not found.
func (d *DB) SessionByInstanceID(instanceID string) (*Session, error) {
	row := d.conn.QueryRow(sessionsSelectCols+` WHERE instance_id = ?`, instanceID)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: session by instance_id: %w", err)
	}
	return sess, nil
}

// SessionsByInstanceIDPrefix returns all sessions rows whose instance_id starts
// with the given prefix. Used for short-form UUID lookup.
// The prefix is escaped for SQL LIKE metacharacters (`%`, `_`, `\`) so that
// literal characters in the prefix are matched exactly, not as wildcards.
func (d *DB) SessionsByInstanceIDPrefix(prefix string) ([]Session, error) {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return d.querySessions(sessionsSelectCols+` WHERE instance_id LIKE ? ESCAPE '\' ORDER BY started_at DESC`, escaped+"%")
}

// MostRecentSessionForName returns the most recent sessions row whose
// session_name equals name (ordered by started_at DESC, take first), or nil.
func (d *DB) MostRecentSessionForName(name string) (*Session, error) {
	row := d.conn.QueryRow(sessionsSelectCols+` WHERE session_name = ? ORDER BY started_at DESC LIMIT 1`, name)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: most recent session for name: %w", err)
	}
	return sess, nil
}

// SessionsByName returns all sessions rows where session_name = name, ordered
// by started_at DESC.
func (d *DB) SessionsByName(name string) ([]Session, error) {
	return d.querySessions(sessionsSelectCols+` WHERE session_name = ? ORDER BY started_at DESC`, name)
}

// SessionsByNamePattern returns all sessions rows whose session_name ends with
// the given suffix (LIKE '%<suffix>' pattern), ordered by started_at DESC.
// The suffix is escaped for SQL LIKE metacharacters.
func (d *DB) SessionsByNamePattern(suffix string) ([]Session, error) {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(suffix)
	return d.querySessions(sessionsSelectCols+` WHERE session_name LIKE ? ESCAPE '\' ORDER BY started_at DESC`, "%"+escaped)
}

// SessionTurnTokens returns per-turn token data for the given instance_id.
func (d *DB) SessionTurnTokens(instanceID string) ([]TokenTurn, error) {
	const q = `
SELECT COALESCE(JSON_EXTRACT(payload, '$.model'), ''),
       COALESCE(JSON_EXTRACT(payload, '$.inputTokens'), 0),
       COALESCE(JSON_EXTRACT(payload, '$.outputTokens'), 0),
       COALESCE(JSON_EXTRACT(payload, '$.cacheReadTokens'), 0),
       COALESCE(JSON_EXTRACT(payload, '$.cacheWriteTokens'), 0),
       COALESCE(JSON_EXTRACT(payload, '$.cost'), 0.0)
  FROM agent_events
 WHERE instance_id = ?
   AND type = 'msg_assistant'
 ORDER BY created_at ASC`
	rows, err := d.conn.Query(q, instanceID)
	if err != nil {
		return nil, fmt.Errorf("db: session turn tokens: %w", err)
	}
	defer rows.Close()

	var turns []TokenTurn
	for rows.Next() {
		var t TokenTurn
		if scanErr := rows.Scan(&t.Model, &t.Input, &t.Output, &t.CacheRead, &t.CacheWrite, &t.EventCost); scanErr != nil {
			return nil, fmt.Errorf("db: session turn tokens: scan: %w", scanErr)
		}
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: session turn tokens: iterate: %w", err)
	}
	return turns, nil
}

// WriteSpawnOutcome computes all aggregated columns for the given instanceID
// from agent_events and sessions, then upserts a spawn_outcome row. The write
// is idempotent (INSERT OR REPLACE). Calling it twice produces the same result.
//
// The function does not return an error when the sessions row does not exist
// (e.g. for pre-migration instances). In that case it is a silent no-op.
//
// Review-group verdict is rolled up from GroupResults when a review group
// exists for the session. PR number and merge timestamp come from
// pending_merges (merge-queue path only).
func (d *DB) WriteSpawnOutcome(instanceID string) error {
	// Fetch the sessions row; skip if not found.
	sess, err := d.SessionByInstanceID(instanceID)
	if err != nil {
		return fmt.Errorf("db: write spawn outcome: fetch session: %w", err)
	}
	if sess == nil {
		return nil // pre-migration or unknown instance — silent no-op
	}

	now := time.Now().UnixMilli()
	out := SpawnOutcome{
		InstanceID:    instanceID,
		ComputedAt:    now,
		SchemaVersion: 1,
	}

	// --- Process-level ---

	out.EndState = sess.EndState
	if sess.EndedAt != nil && sess.StartedAt.UnixMilli() > 0 {
		dur := sess.EndedAt.Sub(sess.StartedAt).Milliseconds()
		out.DurationMs = &dur
		if sess.EndState != nil && *sess.EndState == "finished" {
			out.TimeToFinishedMs = &dur
		}
	}

	// Aggregate process-level counts from agent_events in a single query.
	const aggQ = `
SELECT
    COALESCE(SUM(CASE WHEN type = 'state_change'       AND JSON_EXTRACT(payload,'$.state') = 'interrupted' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'compaction'                                                              THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'error'                                                                   THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'permission_ask'                                                          THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'permission_denied'                                                       THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'doom_loop_detected'                                                      THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'tool_call'                                                               THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'tool_error'                                                              THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'msg_assistant'                                                           THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'msg_assistant' THEN COALESCE(JSON_EXTRACT(payload,'$.inputTokens'),   0) ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'msg_assistant' THEN COALESCE(JSON_EXTRACT(payload,'$.outputTokens'),  0) ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'msg_assistant' THEN COALESCE(JSON_EXTRACT(payload,'$.cacheReadTokens'), 0) ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'msg_assistant' THEN COALESCE(JSON_EXTRACT(payload,'$.cacheWriteTokens'),0) ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN type = 'msg_assistant' THEN COALESCE(JSON_EXTRACT(payload,'$.cost'),          0) ELSE 0.0 END), 0.0),
    MIN(created_at)
FROM agent_events
WHERE instance_id = ?`

	row := d.conn.QueryRow(aggQ, instanceID)
	var minCreatedAt *int64
	if scanErr := row.Scan(
		&out.InterruptedCount, &out.CompactionCount, &out.ErrorEventCount,
		&out.PermissionAskCount, &out.PermissionDeniedCount, &out.DoomLoopCount,
		&out.ToolCallCount, &out.ToolErrorCount, &out.MsgAssistantCount,
		&out.TokensInputTotal, &out.TokensOutputTotal,
		&out.TokensCacheReadTotal, &out.TokensCacheWriteTotal,
		&out.CostUSDTotal,
		&minCreatedAt,
	); scanErr != nil {
		return fmt.Errorf("db: write spawn outcome: aggregate events: %w", scanErr)
	}

	// time_to_first_event_ms: min(event.created_at) − session.started_at
	if minCreatedAt != nil && sess.StartedAt.UnixMilli() > 0 {
		ttfe := *minCreatedAt - sess.StartedAt.UnixMilli()
		if ttfe >= 0 {
			out.TimeToFirstEventMs = &ttfe
		}
	}

	// --- Agent-level: PR from pending_merges ---
	const prQ = `
SELECT pr, merged_at FROM pending_merges
 WHERE instance_id = ? AND status IN ('watching','merged')
 ORDER BY queued_at ASC LIMIT 1`
	prRow := d.conn.QueryRow(prQ, instanceID)
	var prNum int
	var prMergedAt *int64
	if scanErr := prRow.Scan(&prNum, &prMergedAt); scanErr == nil {
		out.PRNumber = &prNum
		out.PRMergedAt = prMergedAt
	}

	// --- Agent-level: review-group verdict ---
	// Look up whether a review group exists for this session.
	const revGroupQ = `
SELECT group_id FROM session_groups
 WHERE parent_session = (SELECT session_name FROM sessions WHERE instance_id = ?)
 LIMIT 1`
	var reviewGroupID string
	if scanErr := d.conn.QueryRow(revGroupQ, instanceID).Scan(&reviewGroupID); scanErr == nil && reviewGroupID != "" {
		out.ReviewGroupID = &reviewGroupID

		// Roll up verdicts from group members.
		members, revErr := d.GroupResults(reviewGroupID)
		if revErr == nil && len(members) > 0 {
			var passCount, failCount, noneCount int
			for _, m := range members {
				// Import review package would create a cycle; inline the check here.
				lower := strings.ToLower(m.LastMessage)
				if strings.Contains(lower, "<verdict>pass</verdict>") {
					passCount++
				} else if strings.Contains(lower, "<verdict>fail</verdict>") {
					failCount++
				} else {
					noneCount++
				}
			}
			out.ReviewPassCount = &passCount
			out.ReviewFailCount = &failCount
			out.ReviewNoneCount = &noneCount

			var verdict string
			switch {
			case failCount == 0 && noneCount == 0 && passCount > 0:
				verdict = "pass"
			case passCount == 0 && noneCount == 0 && failCount > 0:
				verdict = "fail"
			case passCount > 0 && failCount > 0:
				verdict = "mixed"
			default:
				verdict = "mixed"
			}
			out.ReviewVerdict = &verdict
		}
	}

	// --- INSERT OR REPLACE ---
	const insertQ = `
INSERT OR REPLACE INTO spawn_outcome (
    instance_id,
    end_state, exit_code, duration_ms,
    interrupted_count, compaction_count, error_event_count,
    permission_ask_count, permission_denied_count, doom_loop_count,
    pr_number, pr_merged_at,
    review_group_id, review_verdict,
    review_pass_count, review_fail_count, review_none_count,
    rubric_verdict, rubric_score, rubric_breakdown, rubric_grader,
    tokens_input_total, tokens_output_total,
    tokens_cache_read_total, tokens_cache_write_total,
    cost_usd_total, tool_call_count, tool_error_count,
    msg_assistant_count, time_to_first_event_ms, time_to_finished_ms,
    computed_at, schema_version
) VALUES (
    ?,
    ?, ?, ?,
    ?, ?, ?,
    ?, ?, ?,
    ?, ?,
    ?, ?,
    ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?,
    ?, ?,
    ?, ?, ?,
    ?, ?, ?,
    ?, ?
)`
	_, err = d.conn.Exec(insertQ,
		out.InstanceID,
		out.EndState, out.ExitCode, out.DurationMs,
		out.InterruptedCount, out.CompactionCount, out.ErrorEventCount,
		out.PermissionAskCount, out.PermissionDeniedCount, out.DoomLoopCount,
		out.PRNumber, out.PRMergedAt,
		out.ReviewGroupID, out.ReviewVerdict,
		out.ReviewPassCount, out.ReviewFailCount, out.ReviewNoneCount,
		out.RubricVerdict, out.RubricScore, out.RubricBreakdown, out.RubricGrader,
		out.TokensInputTotal, out.TokensOutputTotal,
		out.TokensCacheReadTotal, out.TokensCacheWriteTotal,
		out.CostUSDTotal, out.ToolCallCount, out.ToolErrorCount,
		out.MsgAssistantCount, out.TimeToFirstEventMs, out.TimeToFinishedMs,
		out.ComputedAt, out.SchemaVersion,
	)
	if err != nil {
		return fmt.Errorf("db: write spawn outcome: insert: %w", err)
	}
	return nil
}

// SpawnOutcomeByInstanceID returns the spawn_outcome row for instanceID, or nil
// when not found.
func (d *DB) SpawnOutcomeByInstanceID(instanceID string) (*SpawnOutcome, error) {
	const q = `
SELECT
    instance_id,
    end_state, exit_code, duration_ms,
    interrupted_count, compaction_count, error_event_count,
    permission_ask_count, permission_denied_count, doom_loop_count,
    pr_number, pr_merged_at,
    review_group_id, review_verdict,
    review_pass_count, review_fail_count, review_none_count,
    rubric_verdict, rubric_score, rubric_breakdown, rubric_grader,
    tokens_input_total, tokens_output_total,
    tokens_cache_read_total, tokens_cache_write_total,
    cost_usd_total, tool_call_count, tool_error_count,
    msg_assistant_count, time_to_first_event_ms, time_to_finished_ms,
    computed_at, schema_version
FROM spawn_outcome
WHERE instance_id = ?`
	row := d.conn.QueryRow(q, instanceID)
	out, err := scanSpawnOutcome(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: spawn outcome by instance_id: %w", err)
	}
	return out, nil
}

// GroupByRow holds aggregated spawn_outcome metrics for a single group-by value.
// The GroupValue field is the value of the group-by column (e.g. harness_flag);
// an empty string means the column was NULL for all sessions in that group.
type GroupByRow struct {
	GroupValue string // "" when the column is NULL (rendered as "(none)")

	// Counts
	SessionCount int

	// Aggregate sums from spawn_outcome
	TokensInputTotal      int64
	TokensOutputTotal     int64
	TokensCacheReadTotal  int64
	TokensCacheWriteTotal int64
	CostUSDTotal          float64
	ToolCallCount         int64
	MsgAssistantCount     int64
	DoomLoopCount         int64
	PermissionDeniedCount int64
	ErrorEventCount       int64

	// Average duration (ms) over sessions with a non-NULL duration_ms
	AvgDurationMs *float64
}

// validGroupByAxes lists the allowed axis names for --group-by.
var validGroupByAxes = map[string]string{
	"harness": "si.harness_flag",
	"profile": "si.profile_name",
	"variant": "si.variant_flag",
	"model":   "si.model_flag",
}

// SpawnOutcomeGroupBy queries the spawn_inputs ⋈ spawn_outcome join and
// returns per-group aggregated rows for the given axis. The axis must be one
// of "harness", "profile", "variant", or "model". Sessions that have no
// spawn_inputs row are excluded (the JOIN is INNER).
//
// An optional cutoff in Unix milliseconds (sinceMs > 0) filters sessions to
// those whose sessions.started_at >= sinceMs — allowing the caller to apply
// existing --days or --since filters before grouping.
func (d *DB) SpawnOutcomeGroupBy(axis string, sinceMs int64) ([]GroupByRow, error) {
	col, ok := validGroupByAxes[axis]
	if !ok {
		return nil, fmt.Errorf("unknown group-by axis %q; valid axes: harness, profile, variant, model", axis)
	}

	// Build the WHERE clause for the optional time filter.
	whereParts := []string{}
	var args []any
	if sinceMs > 0 {
		whereParts = append(whereParts, "s.started_at >= ?")
		args = append(args, sinceMs)
	}

	whereClause := ""
	if len(whereParts) > 0 {
		whereClause = "WHERE " + strings.Join(whereParts, " AND ")
	}

	q := fmt.Sprintf(`
SELECT
    COALESCE(%s, '') AS group_value,
    COUNT(*)         AS session_count,
    COALESCE(SUM(so.tokens_input_total), 0),
    COALESCE(SUM(so.tokens_output_total), 0),
    COALESCE(SUM(so.tokens_cache_read_total), 0),
    COALESCE(SUM(so.tokens_cache_write_total), 0),
    COALESCE(SUM(so.cost_usd_total), 0.0),
    COALESCE(SUM(so.tool_call_count), 0),
    COALESCE(SUM(so.msg_assistant_count), 0),
    COALESCE(SUM(so.doom_loop_count), 0),
    COALESCE(SUM(so.permission_denied_count), 0),
    COALESCE(SUM(so.error_event_count), 0),
    AVG(CASE WHEN so.duration_ms IS NOT NULL THEN so.duration_ms ELSE NULL END)
FROM sessions s
INNER JOIN spawn_inputs  si ON si.instance_id = s.instance_id
INNER JOIN spawn_outcome so ON so.instance_id = s.instance_id
%s
GROUP BY group_value
ORDER BY session_count DESC, group_value ASC`, col, whereClause)

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: group_by %s: %w", axis, err)
	}
	defer rows.Close()

	var result []GroupByRow
	for rows.Next() {
		var r GroupByRow
		if scanErr := rows.Scan(
			&r.GroupValue,
			&r.SessionCount,
			&r.TokensInputTotal,
			&r.TokensOutputTotal,
			&r.TokensCacheReadTotal,
			&r.TokensCacheWriteTotal,
			&r.CostUSDTotal,
			&r.ToolCallCount,
			&r.MsgAssistantCount,
			&r.DoomLoopCount,
			&r.PermissionDeniedCount,
			&r.ErrorEventCount,
			&r.AvgDurationMs,
		); scanErr != nil {
			return nil, fmt.Errorf("db: group_by %s: scan: %w", axis, scanErr)
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: group_by %s: iterate: %w", axis, err)
	}
	return result, nil
}

// scanSpawnOutcome scans a *sql.Row into a SpawnOutcome.
func scanSpawnOutcome(row *sql.Row) (*SpawnOutcome, error) {
	var out SpawnOutcome
	err := row.Scan(
		&out.InstanceID,
		&out.EndState, &out.ExitCode, &out.DurationMs,
		&out.InterruptedCount, &out.CompactionCount, &out.ErrorEventCount,
		&out.PermissionAskCount, &out.PermissionDeniedCount, &out.DoomLoopCount,
		&out.PRNumber, &out.PRMergedAt,
		&out.ReviewGroupID, &out.ReviewVerdict,
		&out.ReviewPassCount, &out.ReviewFailCount, &out.ReviewNoneCount,
		&out.RubricVerdict, &out.RubricScore, &out.RubricBreakdown, &out.RubricGrader,
		&out.TokensInputTotal, &out.TokensOutputTotal,
		&out.TokensCacheReadTotal, &out.TokensCacheWriteTotal,
		&out.CostUSDTotal, &out.ToolCallCount, &out.ToolErrorCount,
		&out.MsgAssistantCount, &out.TimeToFirstEventMs, &out.TimeToFinishedMs,
		&out.ComputedAt, &out.SchemaVersion,
	)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
