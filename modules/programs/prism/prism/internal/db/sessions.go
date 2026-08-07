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

// ComputeSpawnOutcome computes all aggregated columns for the given
// instanceID from agent_events, sessions, pending_merges, and any
// review-group rollup, and returns a fully-populated *SpawnOutcome
// without persisting it.
//
// It is the canonical aggregation pass shared by:
//
//   - WriteSpawnOutcome (the persist path called from `prism cleanup`)
//   - prism stats compare (the read path that needs the same data shape
//     between terminal-state transition and cleanup, issue #2102)
//
// Returns (nil, nil) when the sessions row does not exist (pre-migration
// or unknown instance) so callers can treat the result as a silent no-op.
//
// Both call sites must use this helper to guarantee byte-for-byte
// identical aggregates — the AC for issue #2102 requires that the
// on-the-fly compute path and the cleanup write path agree.
func (d *DB) ComputeSpawnOutcome(instanceID string) (*SpawnOutcome, error) {
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
		return nil, fmt.Errorf("db: compute spawn outcome: aggregate events: %w", scanErr)
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
		//
		// GroupResultsAll, not GroupResults (#2649): this is a historical read.
		// It runs at parent-cleanup time and from `prism stats compare`, both
		// after the round is over, and review agents are released 15 minutes
		// after their round is delivered — which stamps ended_at, the column
		// GroupResults filters on. Through the narrow read this roll-up saw an
		// empty map and left ReviewVerdict / ReviewPassCount / ReviewFailCount
		// nil for every round older than that window.
		//
		// The roll-up is a fallback — the #2110 dedicated write path persists
		// the verdict at review-complete time and the agent-level merge below
		// prefers it — so the loss was invisible wherever that path fired, and
		// total for exactly the sessions the fallback exists to serve.
		members, revErr := d.GroupResultsAll(reviewGroupID)
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

	// --- Agent-level: prefer persisted spawn_outcome values when present ---
	//
	// Issue #2110 added dedicated write paths for the agent-level columns
	// (pr_number from the worker's `gh pr create`, pr_merged_at from the
	// merge-queue watcher, review_verdict/review_pass_count/review_fail_count
	// from the review-complete handler). When a write path has already fired,
	// the persisted value is the authoritative one for the latest-round-wins
	// semantics required by the AC. The pending_merges and GroupResults
	// fallbacks above remain in place for sessions whose write paths never
	// fired (historical sessions completed before #2110 landed).
	if existing, exErr := d.SpawnOutcomeByInstanceID(instanceID); exErr == nil && existing != nil {
		if existing.PRNumber != nil {
			out.PRNumber = existing.PRNumber
		}
		if existing.PRMergedAt != nil {
			out.PRMergedAt = existing.PRMergedAt
		}
		if existing.ReviewVerdict != nil {
			out.ReviewVerdict = existing.ReviewVerdict
			out.ReviewPassCount = existing.ReviewPassCount
			out.ReviewFailCount = existing.ReviewFailCount
			// ReviewNoneCount is computed-only; leave whatever ComputeSpawnOutcome
			// derived above. The write path only persists the three AC-named
			// columns; the none-count remains a derived signal.
		}
	}

	return &out, nil
}

// WriteSpawnOutcome computes all aggregated columns for the given instanceID
// from agent_events and sessions, then upserts a spawn_outcome row. The write
// is idempotent (INSERT OR REPLACE). Calling it twice produces the same result.
//
// The function does not return an error when the sessions row does not exist
// (e.g. for pre-migration instances). In that case it is a silent no-op.
//
// Review-group verdict is rolled up from GroupResultsAll when a review group
// exists for the session — the wide read, because by the time this runs the
// round is over and its member rows are closed (#2649). PR number and merge
// timestamp come from pending_merges (merge-queue path only).
//
// Implementation note: this function delegates to ComputeSpawnOutcome to
// produce the in-memory aggregate, then performs the INSERT OR REPLACE.
// The two call sites (write-time via cleanup, read-time via
// `prism stats compare` for terminal-but-not-cleaned-up sessions) share the
// aggregation pass so the produced rows agree byte-for-byte (issue #2102).
func (d *DB) WriteSpawnOutcome(instanceID string) error {
	out, err := d.ComputeSpawnOutcome(instanceID)
	if err != nil {
		return fmt.Errorf("db: write spawn outcome: %w", err)
	}
	if out == nil {
		return nil // sessions row missing — silent no-op
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

// WriteSpawnOutcomeCascade writes a spawn_outcome row for sessionName and for
// every review-agent child session whose session_name matches the pattern
// "<sessionName>~review-%" (issue #2591). WriteSpawnOutcome is called from
// four sites in cmd/cleanup.go, and until this cascade existed each call only
// ever wrote a row for the parent named on the command line — review-agent
// children never got one (measured coverage: 1.2% of ~review-% sessions).
//
// This mirrors the cascade SetEnded already performs for ended_at: it
// resolves every distinct instance_id among rows where
// session_name = sessionName OR session_name LIKE '<sessionName>~review-%',
// then calls WriteSpawnOutcome for each one found. Both the parent lookup and
// the children lookup go through the same query, so a parent with no rows in
// agent_status (e.g. never spawned via prism, or already purged) simply
// contributes no instance_id and is silently skipped — WriteSpawnOutcome's
// own pre-migration tolerance is preserved.
//
// The session name is escaped for SQL LIKE wildcards before being used as a
// pattern prefix, matching the convention used by SetEnded and
// ClearHarnessSessionID.
//
// Each resolved instance_id is written independently via WriteSpawnOutcome,
// so one child's failure does not prevent the others (or the parent) from
// being written. The first error encountered, if any, is returned after all
// writes have been attempted.
func (d *DB) WriteSpawnOutcomeCascade(sessionName string) error {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(sessionName)
	const q = `
SELECT DISTINCT instance_id
FROM   agent_status
WHERE  (session_name = ? OR session_name LIKE ? || '~review-%' ESCAPE '\')
  AND  instance_id IS NOT NULL`
	rows, err := d.conn.Query(q, sessionName, escaped)
	if err != nil {
		return fmt.Errorf("db: write spawn outcome cascade: query instance ids: %w", err)
	}

	var instanceIDs []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			return fmt.Errorf("db: write spawn outcome cascade: scan instance id: %w", scanErr)
		}
		instanceIDs = append(instanceIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("db: write spawn outcome cascade: iterate instance ids: %w", err)
	}
	rows.Close()

	var firstErr error
	for _, instanceID := range instanceIDs {
		if writeErr := d.WriteSpawnOutcome(instanceID); writeErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("db: write spawn outcome cascade: instance %s: %w", instanceID, writeErr)
		}
	}
	return firstErr
}

// UpdateSpawnOutcomePR records the PR number opened by the worker session
// identified by instanceID. Issue #2110: pr_number was previously read by the
// `prism stats compare` renderer but never written by any code path. This is
// the worker-side capture write trigger: the sidecar calls it the moment it
// observes a successful `gh pr create` invocation in its event stream.
//
// The write is a partial UPSERT — it inserts a minimal row with pr_number set
// when no spawn_outcome row yet exists for instanceID, and updates only
// pr_number and computed_at when one does. Other columns are preserved. This
// allows the write to fire at the natural event boundary (PR creation) without
// racing the cleanup-time WriteSpawnOutcome overwrite.
//
// Returns a silent no-op when no sessions row exists for instanceID. The FK
// constraint on spawn_outcome.instance_id would otherwise reject the insert;
// catching it as a no-op matches WriteSpawnOutcome's pre-migration tolerance.
func (d *DB) UpdateSpawnOutcomePR(instanceID string, prNumber int) error {
	if instanceID == "" {
		return nil
	}
	sess, err := d.SessionByInstanceID(instanceID)
	if err != nil {
		return fmt.Errorf("db: update spawn outcome pr: lookup session: %w", err)
	}
	if sess == nil {
		return nil // pre-migration or unknown instance — silent no-op
	}
	now := time.Now().UnixMilli()
	const q = `
INSERT INTO spawn_outcome (instance_id, pr_number, computed_at, schema_version)
VALUES (?, ?, ?, 1)
ON CONFLICT(instance_id) DO UPDATE SET
    pr_number   = excluded.pr_number,
    computed_at = excluded.computed_at`
	if _, err := d.conn.Exec(q, instanceID, prNumber, now); err != nil {
		return fmt.Errorf("db: update spawn outcome pr: %w", err)
	}
	return nil
}

// UpdateSpawnOutcomePRMergedAt records the wall-clock time the worker
// session's PR was merged. Issue #2110: this is the merge-queue watcher's
// write trigger — the watcher calls it from succeedAndNotify, persisting the
// same timestamp it already passes to TerminateMerge on the pending_merges
// row. The persistence happens BEFORE the merge notification fires so a write
// error cannot delay or skip the notification.
//
// mergedAtMs is the Unix-milliseconds timestamp. Like UpdateSpawnOutcomePR
// this is a partial UPSERT — other columns are preserved.
//
// Returns a silent no-op when no sessions row exists for instanceID.
func (d *DB) UpdateSpawnOutcomePRMergedAt(instanceID string, mergedAtMs int64) error {
	if instanceID == "" {
		return nil
	}
	sess, err := d.SessionByInstanceID(instanceID)
	if err != nil {
		return fmt.Errorf("db: update spawn outcome pr_merged_at: lookup session: %w", err)
	}
	if sess == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	const q = `
INSERT INTO spawn_outcome (instance_id, pr_merged_at, computed_at, schema_version)
VALUES (?, ?, ?, 1)
ON CONFLICT(instance_id) DO UPDATE SET
    pr_merged_at = excluded.pr_merged_at,
    computed_at  = excluded.computed_at`
	if _, err := d.conn.Exec(q, instanceID, mergedAtMs, now); err != nil {
		return fmt.Errorf("db: update spawn outcome pr_merged_at: %w", err)
	}
	return nil
}

// UpdateSpawnOutcomeReviewResult records the latest-round verdict and per-
// agent counts for the worker session's review. Issue #2110: this is the
// review-complete handler's write trigger — the monitor calls it the moment
// it has the aggregated verdicts in hand (same site that builds the prompt
// body), persisting all three columns in a single transaction.
//
// Latest-round-wins semantics. Each MonitorFunc invocation represents a
// single review round; this UPSERT overwrites the previous round's values
// rather than accumulating. A worker that runs round 1 (3 PASS, 2 FAIL) then
// round 2 (5 PASS, 0 FAIL) ends with review_pass_count=5, review_fail_count=0,
// review_verdict="pass" — its actual ship state, not a historical sum.
//
// verdict is "pass" when all reviewers passed, "fail" when any reviewer failed.
// passCount/failCount reflect the agents whose LastMessage carried a
// parseable `<verdict>PASS</verdict>` / `<verdict>FAIL</verdict>` marker for
// this round; agents without a parseable verdict (infrastructure failures,
// truncated output) count toward failCount when verdict=="fail" and toward
// neither when verdict=="pass" (which is unreachable in that case).
//
// Like the other partial writers this UPSERT is a no-op when no sessions row
// exists for instanceID.
func (d *DB) UpdateSpawnOutcomeReviewResult(instanceID, verdict string, passCount, failCount int) error {
	if instanceID == "" {
		return nil
	}
	sess, err := d.SessionByInstanceID(instanceID)
	if err != nil {
		return fmt.Errorf("db: update spawn outcome review result: lookup session: %w", err)
	}
	if sess == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	const q = `
INSERT INTO spawn_outcome (instance_id, review_verdict, review_pass_count, review_fail_count, computed_at, schema_version)
VALUES (?, ?, ?, ?, ?, 1)
ON CONFLICT(instance_id) DO UPDATE SET
    review_verdict    = excluded.review_verdict,
    review_pass_count = excluded.review_pass_count,
    review_fail_count = excluded.review_fail_count,
    computed_at       = excluded.computed_at`
	if _, err := d.conn.Exec(q, instanceID, verdict, passCount, failCount, now); err != nil {
		return fmt.Errorf("db: update spawn outcome review result: %w", err)
	}
	return nil
}

// InstanceIDForPRNumber returns the instance_id of the worker session whose
// spawn_outcome row carries the given pr_number, or ("", nil) when none is
// found. Used by the merge-queue watcher (issue #2110) to locate the worker
// session that opened the PR, so it can persist pr_merged_at on the same row
// whose pr_number it sees.
//
// When multiple rows match (shouldn't happen in practice — PR numbers are
// globally unique within a repo and the worker-side capture only writes once),
// the most recently-computed row wins. The query is scoped to spawn_outcome
// only; cross-repo PR collisions are impossible because PR numbers are
// assigned per-repo and the merge-queue watcher's repo binding already
// narrows the scope at the call site.
func (d *DB) InstanceIDForPRNumber(prNumber int) (string, error) {
	const q = `
SELECT instance_id FROM spawn_outcome
 WHERE pr_number = ?
 ORDER BY computed_at DESC
 LIMIT 1`
	var iid string
	err := d.conn.QueryRow(q, prNumber).Scan(&iid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: instance for pr_number %d: %w", prNumber, err)
	}
	return iid, nil
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
