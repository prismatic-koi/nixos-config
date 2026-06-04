package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
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
	var parentSession sql.NullString

	err := s.Scan(
		&sess.InstanceID, &sess.SessionName, &agentRole, &rootAgentName,
		&sess.Repo, &sess.Worktree, &sess.Harness, &harnessSessionID, &groupID,
		&startedAt, &endedAt, &endState, &archivePath, &prismVersion, &parentSession,
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
	if parentSession.Valid {
		sess.ParentSession = &parentSession.String
	}
	return &sess, nil
}

const sessionsSelectCols = `
SELECT instance_id, session_name, agent_role, root_agent_name,
       repo, worktree, harness, harness_session_id, group_id,
       started_at, ended_at, end_state, archive_path, prism_version, parent_session
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
   harness, harness_session_id, group_id, started_at, parent_session)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := d.conn.Exec(q,
		s.InstanceID, s.SessionName, s.AgentRole, s.RootAgentName,
		s.Repo, s.Worktree, s.Harness, s.HarnessSessionID, s.GroupID,
		startedAt, s.ParentSession,
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

// SessionsByNamePatternAndRepo returns all sessions rows whose session_name ends
// with the given suffix AND whose repo equals the given repo, ordered by
// started_at DESC. Used by lookupWorkerArchivePath to scope the PR-number suffix
// match to a single repository, avoiding false matches when two repos share the
// same PR number.
func (d *DB) SessionsByNamePatternAndRepo(suffix, repo string) ([]Session, error) {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(suffix)
	return d.querySessions(sessionsSelectCols+` WHERE session_name LIKE ? ESCAPE '\' AND repo = ? ORDER BY started_at DESC`, "%"+escaped, repo)
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

// computeSpawnOutcome aggregates every spawn_outcome column for instanceID from
// agent_events and the sessions / agent_status rows, returning the computed
// *SpawnOutcome WITHOUT persisting it. It returns (nil, nil) when no sessions
// row exists (pre-migration or unknown instance).
//
// This is the single canonical aggregation, shared by two callers so their
// values can never drift:
//
//   - WriteSpawnOutcome persists the result (INSERT OR REPLACE) — the
//     cleanup-path writer (cmd/cleanup.go).
//   - SpawnOutcomeForCompare returns it directly at read time for a terminal
//     session that has not been cleaned up yet, so `prism stats compare` shows
//     full data between the terminal transition and cleanup (issue #2102).
//
// Because both paths run the identical agent_events aggregation query, the
// event-derived axes (token / cost / tool / msg counts) agree byte-for-byte:
// there is no double-counting and no missed delta when cleanup later overwrites
// the row with the same numbers.
//
// Review-group verdict is rolled up from GroupResults when a review group
// exists for the session. PR number and merge timestamp come from
// pending_merges (merge-queue path only).
func (d *DB) computeSpawnOutcome(instanceID string) (*SpawnOutcome, error) {
	// Fetch the sessions row; skip if not found.
	sess, err := d.SessionByInstanceID(instanceID)
	if err != nil {
		return nil, fmt.Errorf("db: compute spawn outcome: fetch session: %w", err)
	}
	if sess == nil {
		return nil, nil // pre-migration or unknown instance — silent no-op
	}

	now := time.Now().UnixMilli()
	out := SpawnOutcome{
		InstanceID:    instanceID,
		ComputedAt:    now,
		SchemaVersion: 1,
	}

	// --- Process-level: end_state ---
	//
	// sessions.end_state is only stamped at cleanup (UpdateSessionEnded). Between
	// the agent reaching a terminal state and cleanup running it is NULL, so fall
	// back to the live agent_status.state when that is terminal. The cleanup path
	// is unchanged (sess.EndState is already set there); only the read-time compute
	// uses the fallback to surface the real terminal state pre-cleanup.
	out.EndState = sess.EndState
	if out.EndState == nil {
		if st, terminal, stErr := d.agentTerminalState(instanceID); stErr == nil && terminal {
			s := string(st)
			out.EndState = &s
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
    MIN(created_at),
    MAX(created_at)
FROM agent_events
WHERE instance_id = ?`

	row := d.conn.QueryRow(aggQ, instanceID)
	var minCreatedAt, maxCreatedAt *int64
	if scanErr := row.Scan(
		&out.InterruptedCount, &out.CompactionCount, &out.ErrorEventCount,
		&out.PermissionAskCount, &out.PermissionDeniedCount, &out.DoomLoopCount,
		&out.ToolCallCount, &out.ToolErrorCount, &out.MsgAssistantCount,
		&out.TokensInputTotal, &out.TokensOutputTotal,
		&out.TokensCacheReadTotal, &out.TokensCacheWriteTotal,
		&out.CostUSDTotal,
		&minCreatedAt, &maxCreatedAt,
	); scanErr != nil {
		return nil, fmt.Errorf("db: compute spawn outcome: aggregate events: %w", scanErr)
	}

	// time_to_first_event_ms: min(event.created_at) − session.started_at
	if minCreatedAt != nil && sess.StartedAt.UnixMilli() > 0 {
		ttfe := *minCreatedAt - sess.StartedAt.UnixMilli()
		if ttfe >= 0 {
			out.TimeToFirstEventMs = &ttfe
		}
	}

	// --- Process-level: duration_ms / time_to_finished_ms ---
	//
	// Prefer the cleanup-stamped sessions.ended_at (the post-cleanup value). Before
	// cleanup runs that column is NULL, so fall back to the last agent_event
	// timestamp — a point that does not move once the agent stops emitting events —
	// so duration is populated on the pre-cleanup compare surface too.
	var endedAtMs int64
	hasEnded := false
	switch {
	case sess.EndedAt != nil:
		endedAtMs = sess.EndedAt.UnixMilli()
		hasEnded = true
	case maxCreatedAt != nil:
		endedAtMs = *maxCreatedAt
		hasEnded = true
	}
	if hasEnded && sess.StartedAt.UnixMilli() > 0 {
		dur := endedAtMs - sess.StartedAt.UnixMilli()
		if dur >= 0 {
			out.DurationMs = &dur
			if out.EndState != nil && *out.EndState == "finished" {
				out.TimeToFinishedMs = &dur
			}
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

	return &out, nil
}

// WriteSpawnOutcome computes the aggregated spawn_outcome columns for
// instanceID (via computeSpawnOutcome) and upserts the row. The write is
// idempotent (INSERT OR REPLACE): calling it twice — or after the read-time
// compute (SpawnOutcomeForCompare) has already surfaced the same numbers —
// produces the identical row.
//
// It is a silent no-op when no sessions row exists (pre-migration / unknown
// instance). Called from cmd/cleanup.go after UpdateSessionEnded stamps the
// terminal end_state.
func (d *DB) WriteSpawnOutcome(instanceID string) error {
	out, err := d.computeSpawnOutcome(instanceID)
	if err != nil {
		return err
	}
	if out == nil {
		return nil // pre-migration or unknown instance — silent no-op
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
	if _, execErr := d.conn.Exec(insertQ,
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
	); execErr != nil {
		return fmt.Errorf("db: write spawn outcome: insert: %w", execErr)
	}
	return nil
}

// agentTerminalState returns the current agent_status.state for instanceID and
// whether it is one of the terminal agent states (finished / error /
// interrupted). It returns ("", false, nil) when no agent_status row exists.
//
// deleted is intentionally excluded: a deleted session has been cleaned up, so
// its spawn_outcome row already exists (or its events were purged) and the
// on-the-fly compute path does not apply.
func (d *DB) agentTerminalState(instanceID string) (agent.AgentState, bool, error) {
	var state string
	err := d.conn.QueryRow(
		`SELECT state FROM agent_status WHERE instance_id = ? ORDER BY last_seen DESC LIMIT 1`,
		instanceID,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("db: agent terminal state for %q: %w", instanceID, err)
	}
	st := agent.AgentState(state)
	switch st {
	case agent.StateFinished, agent.StateError, agent.StateInterrupted:
		return st, true, nil
	}
	return st, false, nil
}

// SpawnOutcomeForCompare returns the spawn_outcome data for instanceID for the
// `prism stats compare` read path (issue #2102). It prefers the persisted
// spawn_outcome row (present post-cleanup). When that row is absent it computes
// the aggregates on the fly from agent_events — but only when the session has
// reached a terminal agent state, so an in-progress (active / idle / ...)
// session still reports "—" for its aggregate axes rather than partial
// mid-flight data.
//
// Returns (nil, nil) when there is no persisted row and the session is not
// terminal.
func (d *DB) SpawnOutcomeForCompare(instanceID string) (*SpawnOutcome, error) {
	out, err := d.SpawnOutcomeByInstanceID(instanceID)
	if err != nil {
		return nil, err
	}
	if out != nil {
		return out, nil
	}
	_, terminal, err := d.agentTerminalState(instanceID)
	if err != nil {
		return nil, err
	}
	if !terminal {
		return nil, nil
	}
	return d.computeSpawnOutcome(instanceID)
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
