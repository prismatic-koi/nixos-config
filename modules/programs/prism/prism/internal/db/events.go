package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WriteEvent inserts an event row into agent_events and, when a matching
// agent_status row exists for e.SessionName, bumps last_seen to
// MAX(last_seen, e.CreatedAt) in the same transaction. Writing an event for
// an unknown session_name (no agent_status row) is not an error — the event
// is still recorded and no last_seen update is attempted.
func (d *DB) WriteEvent(e Event) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	createdAt := e.CreatedAt.UnixMilli()

	// Second redaction control (issue #2589). The harness redacts before it
	// writes to the socket; this covers every other producer, including a
	// harness with no redactor of its own. See redact.go.
	e.Payload = d.redactPayload(e.Payload)

	// Resolve the active account name at write time, not at scrape time
	// (issue #2714). See account_name.go for why. Never NULL on a new row.
	accountName := d.resolveAccountName()

	// Resolve the active profile the same way (issue #2768). See
	// profile_name.go for why capture happens here rather than at scrape time,
	// and why a coordinator — which has no spawn_inputs row — needs it. Never
	// NULL on a new row.
	profileName := d.resolveProfileName()

	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("db: write event: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const insertQ = `
INSERT INTO agent_events (id, session_name, repo, worktree, harness_session_id, type, payload, created_at, instance_id, account_name, profile_name)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.Exec(insertQ, e.ID, e.SessionName, e.Repo, e.Worktree, e.HarnessSessionID, e.Type, e.Payload, createdAt, e.InstanceID, accountName, profileName); err != nil {
		return fmt.Errorf("db: write event: insert: %w", err)
	}

	// Bump last_seen only when a matching agent_status row exists. The MAX
	// guard ensures we never move last_seen backward (e.g. for out-of-order
	// event replays or backfill writes with old timestamps).
	const updateQ = `
UPDATE agent_status
   SET last_seen = MAX(last_seen, ?)
 WHERE session_name = ?`
	if _, err := tx.Exec(updateQ, createdAt, e.SessionName); err != nil {
		return fmt.Errorf("db: write event: update last_seen: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: write event: commit: %w", err)
	}
	return nil
}

// QueryEvents returns up to limit events for the given session, ordered by
// created_at ASC. before and after are event IDs used for cursor-based
// pagination — pass nil for open-ended queries. types filters by event type;
// pass nil to return all types.
//
// When limit > 0 and neither before nor after is set (a plain "last N" query),
// the most-recent N events are returned (newest first in the DB query, then
// reversed to produce chronological ASC order in the result).
// When after is set (forward pagination from a cursor), events are fetched
// ASC from that point. When before is set (backward pagination), events are
// fetched DESC up to the cursor then reversed to chronological order.
func (d *DB) QueryEvents(sessionName string, limit int, before, after *string, types []string) ([]Event, error) {
	args := []any{sessionName}
	var conditions []string
	conditions = append(conditions, "session_name = ?")

	if after != nil {
		var afterTS int64
		err := d.conn.QueryRow(
			"SELECT created_at FROM agent_events WHERE id = ?", *after,
		).Scan(&afterTS)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("QueryEvents: cursor event %q not found", *after)
		}
		if err != nil {
			return nil, fmt.Errorf("db: resolve after cursor: %w", err)
		}
		conditions = append(conditions, "created_at > ?")
		args = append(args, afterTS)
	}

	if before != nil {
		var beforeTS int64
		err := d.conn.QueryRow(
			"SELECT created_at FROM agent_events WHERE id = ?", *before,
		).Scan(&beforeTS)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("QueryEvents: cursor event %q not found", *before)
		}
		if err != nil {
			return nil, fmt.Errorf("db: resolve before cursor: %w", err)
		}
		conditions = append(conditions, "created_at < ?")
		args = append(args, beforeTS)
	}

	if len(types) > 0 {
		placeholders := make([]string, len(types))
		for i, t := range types {
			placeholders[i] = "?"
			args = append(args, t)
		}
		conditions = append(conditions, "type IN ("+strings.Join(placeholders, ",")+")")
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	// Choose ordering strategy:
	//   - "last N" (no cursor): fetch newest N rows with DESC, then reverse.
	//   - forward pagination (after cursor): fetch ASC from cursor.
	//   - backward pagination (before cursor): fetch DESC up to cursor, then reverse.
	//   - no limit: fetch all ASC.
	reverseResult := false
	orderDir := "ASC"
	if limit > 0 && after == nil {
		// Covers both the plain "last N" case and the --before cursor case.
		orderDir = "DESC"
		reverseResult = true
	}

	q := "SELECT id, session_name, repo, worktree, harness_session_id, type, payload, created_at, instance_id FROM agent_events" +
		where + " ORDER BY created_at " + orderDir
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.HarnessSessionID, &e.Type, &e.Payload, &createdAt, &e.InstanceID); err != nil {
			return nil, fmt.Errorf("db: scan event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate events: %w", err)
	}

	if reverseResult {
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
	}

	return events, nil
}

// QueryEventsByMessageIDs returns all events for sessionName whose payload
// joins back to one of the provided assistant-turn message IDs. Only events
// of the specified types are returned; pass nil for types to return all types.
// Results are ordered by created_at ASC.
//
// This is used by checkin's secondary query to fetch tool_call, tool_result,
// permission_ask, permission_denied, and thinking events that belong to a set
// of assistant-message turns retrieved by the primary query.
//
// Join field by event type (#1787):
//
//   - tool_call / tool_result: matched on `$.parentMessageId`. The pi prism
//     extension stamps this field with the in-flight assistant messageId on
//     every tool frame; pre-#1787 these rows had no parent-link field at all
//     and were silently dropped by the previous `$.messageId` pushdown.
//   - permission_ask / permission_denied / thinking: matched on `$.messageId`
//     (the field the plugin has always emitted for these types).
//
// To keep the API a single round-trip with one IN clause, the SQL matches a
// row when *either* JSON path resolves to a value in the provided id set.
// COALESCE picks whichever path is populated for a given row — since no
// event type emits both fields, the two paths are mutually exclusive in
// practice, so the COALESCE never has to choose between competing values.
func (d *DB) QueryEventsByMessageIDs(sessionName string, messageIDs []string, types []string) ([]Event, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	args := []any{sessionName}
	conditions := []string{"session_name = ?"}

	// Build the IN clause for messageIds using JSON_EXTRACT.
	idPlaceholders := make([]string, len(messageIDs))
	for i, id := range messageIDs {
		idPlaceholders[i] = "?"
		args = append(args, id)
	}
	conditions = append(conditions,
		"COALESCE(JSON_EXTRACT(payload, '$.parentMessageId'), JSON_EXTRACT(payload, '$.messageId')) IN ("+
			strings.Join(idPlaceholders, ",")+")")

	if len(types) > 0 {
		typePlaceholders := make([]string, len(types))
		for i, t := range types {
			typePlaceholders[i] = "?"
			args = append(args, t)
		}
		conditions = append(conditions, "type IN ("+strings.Join(typePlaceholders, ",")+")")
	}

	q := "SELECT id, session_name, repo, worktree, harness_session_id, type, payload, created_at, instance_id FROM agent_events" +
		" WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY created_at ASC"

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query events by message IDs: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.HarnessSessionID, &e.Type, &e.Payload, &createdAt, &e.InstanceID); err != nil {
			return nil, fmt.Errorf("db: scan event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate events by message IDs: %w", err)
	}
	return events, nil
}

// AllSessionEvents returns all events for a session, ordered by created_at ASC.
// Unlike QueryEvents, this has no limit — it returns the full event history.
func (d *DB) AllSessionEvents(sessionName string) ([]Event, error) {
	return d.QueryEvents(sessionName, 0, nil, nil, nil)
}

// EventsSince returns all events across all sessions created after sinceMs
// (Unix milliseconds), ordered by created_at ASC. Used by `prism stats --days`.
func (d *DB) EventsSince(sinceMs int64) ([]Event, error) {
	const q = `
SELECT id, session_name, repo, worktree, harness_session_id, type, payload, created_at, instance_id
FROM agent_events
WHERE created_at >= ?
ORDER BY created_at ASC`
	rows, err := d.conn.Query(q, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("db: events since: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.HarnessSessionID, &e.Type, &e.Payload, &createdAt, &e.InstanceID); err != nil {
			return nil, fmt.Errorf("db: scan event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate events since: %w", err)
	}
	return events, nil
}

// QueryDoomLoopEvents returns doom_loop_detected events from agent_events,
// ordered by created_at DESC. Optional filters:
//   - sessionName: when non-empty, restrict to this session only
//   - sinceMs: when > 0, restrict to events created at or after this Unix ms timestamp
func (d *DB) QueryDoomLoopEvents(sessionName string, sinceMs int64) ([]Event, error) {
	args := []any{}
	conditions := []string{"type = 'doom_loop_detected'"}

	if sessionName != "" {
		conditions = append(conditions, "session_name = ?")
		args = append(args, sessionName)
	}

	if sinceMs > 0 {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, sinceMs)
	}

	where := " WHERE " + strings.Join(conditions, " AND ")

	q := "SELECT id, session_name, repo, worktree, harness_session_id, type, payload, created_at, instance_id FROM agent_events" +
		where + " ORDER BY created_at DESC"

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query doom loop events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.HarnessSessionID, &e.Type, &e.Payload, &createdAt, &e.InstanceID); err != nil {
			return nil, fmt.Errorf("db: scan doom loop event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate doom loop events: %w", err)
	}
	return events, nil
}

// QueryPermissionEvents returns permission_denied or permission_ask events from
// agent_events, ordered by created_at ASC. Optional filters:
//   - eventType: must be "permission_denied" or "permission_ask"
//   - sessionName: when non-empty, restrict to this session only
//   - sinceMs: when > 0, restrict to events created at or after this Unix ms timestamp
func (d *DB) QueryPermissionEvents(eventType, sessionName string, sinceMs int64) ([]Event, error) {
	args := []any{eventType}
	conditions := []string{"type = ?"}

	if sessionName != "" {
		conditions = append(conditions, "session_name = ?")
		args = append(args, sessionName)
	}

	if sinceMs > 0 {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, sinceMs)
	}

	where := " WHERE " + strings.Join(conditions, " AND ")

	q := "SELECT id, session_name, repo, worktree, harness_session_id, type, payload, created_at, instance_id FROM agent_events" +
		where + " ORDER BY created_at ASC"

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query permission events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.HarnessSessionID, &e.Type, &e.Payload, &createdAt, &e.InstanceID); err != nil {
			return nil, fmt.Errorf("db: scan permission event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate permission events: %w", err)
	}
	return events, nil
}

// QueryAuditEvents returns audit events from agent_events, ordered by
// created_at DESC. Optional filters:
//   - sessionName: when non-empty, restrict to this session only
//   - sinceMs: when > 0, restrict to events created at or after this Unix ms timestamp
//   - pattern: when non-empty, restrict to events whose payload command field
//     contains this substring (case-insensitive)
//   - limit: when > 0, return at most this many events (default 20 when both
//     limit==0 and sessionName=="")
//
// Note: audit events are subject to the same 90-day Prune() threshold as all
// other agent_events rows. For the forensic use-case described in issue #642,
// 90 days is sufficient, but audit events are not retained indefinitely.
func (d *DB) QueryAuditEvents(sessionName string, sinceMs int64, pattern string, limit int) ([]Event, error) {
	args := []any{}
	conditions := []string{"type = 'audit'"}

	if sessionName != "" {
		conditions = append(conditions, "session_name = ?")
		args = append(args, sessionName)
	}

	if sinceMs > 0 {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, sinceMs)
	}

	if pattern != "" {
		conditions = append(conditions, "LOWER(JSON_EXTRACT(payload, '$.command')) LIKE ?")
		args = append(args, "%"+strings.ToLower(pattern)+"%")
	}

	where := " WHERE " + strings.Join(conditions, " AND ")

	q := "SELECT id, session_name, repo, worktree, harness_session_id, type, payload, created_at, instance_id FROM agent_events" +
		where + " ORDER BY created_at DESC"

	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	} else if sessionName == "" {
		// Default: return the last 20 audit events when no session filter.
		q += " LIMIT 20"
	}

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query audit events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.HarnessSessionID, &e.Type, &e.Payload, &createdAt, &e.InstanceID); err != nil {
			return nil, fmt.Errorf("db: scan audit event: %w", err)
		}
		e.CreatedAt = time.UnixMilli(createdAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate audit events: %w", err)
	}
	return events, nil
}

// QuerySessionEventsSinceRowID returns all events for sessionName with
// rowid > sinceRowID, ordered by rowid ASC (monotonic DB insertion order).
// This is the backing query for a since_event_id replay protocol: each
// returned EventRow carries the rowid so the client can use it in a
// subsequent since_event_id field.
func (d *DB) QuerySessionEventsSinceRowID(sessionName string, sinceRowID int64) ([]EventRow, error) {
	const q = `
SELECT rowid, id, session_name, repo, worktree, harness_session_id,
       type, payload, created_at, instance_id
  FROM agent_events
 WHERE session_name = ? AND rowid > ?
 ORDER BY rowid ASC`
	rows, err := d.conn.Query(q, sessionName, sinceRowID)
	if err != nil {
		return nil, fmt.Errorf("db: query session events since rowid: %w", err)
	}
	defer rows.Close()

	var result []EventRow
	for rows.Next() {
		var er EventRow
		var createdAt int64
		if err := rows.Scan(
			&er.RowID, &er.Event.ID, &er.Event.SessionName, &er.Event.Repo,
			&er.Event.Worktree, &er.Event.HarnessSessionID,
			&er.Event.Type, &er.Event.Payload, &createdAt, &er.Event.InstanceID,
		); err != nil {
			return nil, fmt.Errorf("db: scan event row: %w", err)
		}
		er.Event.CreatedAt = time.UnixMilli(createdAt)
		result = append(result, er)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate session events since rowid: %w", err)
	}
	return result, nil
}

// MaxSessionEventRowID returns the maximum rowid in agent_events for the
// given sessionName, or 0 if no events exist. Used by the client IPC socket to
// snapshot the high-water mark before a replay query so that events arriving
// during replay can be deduplicated (§4.3 design doc — the since_event_id race
// avoidance strategy).
func (d *DB) MaxSessionEventRowID(sessionName string) (int64, error) {
	const q = `SELECT COALESCE(MAX(rowid), 0) FROM agent_events WHERE session_name = ?`
	var max int64
	if err := d.conn.QueryRow(q, sessionName).Scan(&max); err != nil {
		return 0, fmt.Errorf("db: max session event rowid: %w", err)
	}
	return max, nil
}

// QuerySessionEventsBeforeRowID returns up to `limit` events for sessionName
// with rowid < beforeRowID, ordered by rowid ASC. When beforeRowID == 0 the
// query returns the most recent `limit` events for the session — i.e. the
// tail of history — which is useful when the caller has not yet seen any
// event for the session and wants a starting window.
//
// Pagination protocol: when a scrollback-style caller scrolls past the top
// of its in-memory window, it requests another page of older events using
// the rowid of the previously-top event as `beforeRowID`. The result is
// returned in ASC order so the caller can prepend it directly without
// re-sorting; the caller is responsible for detecting head-of-history
// (zero rows returned) and suppressing further requests.
func (d *DB) QuerySessionEventsBeforeRowID(sessionName string, beforeRowID int64, limit int) ([]EventRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	// Two-step query: select the last `limit` rows in DESC order using the
	// `<` predicate, then re-order ASC in the outer select. SQLite's query
	// planner uses the implicit rowid index for both the WHERE filter and
	// the ORDER BY so this is O(limit) on a session of any size.
	//
	// When beforeRowID == 0 the inner WHERE collapses to `session_name = ?`
	// alone and we get the tail of history — same `limit`/ORDER semantics.
	const qWithBefore = `
SELECT rowid, id, session_name, repo, worktree, harness_session_id,
       type, payload, created_at, instance_id
  FROM (
    SELECT rowid, id, session_name, repo, worktree, harness_session_id,
           type, payload, created_at, instance_id
      FROM agent_events
     WHERE session_name = ? AND rowid < ?
     ORDER BY rowid DESC
     LIMIT ?
  )
 ORDER BY rowid ASC`
	const qNoBefore = `
SELECT rowid, id, session_name, repo, worktree, harness_session_id,
       type, payload, created_at, instance_id
  FROM (
    SELECT rowid, id, session_name, repo, worktree, harness_session_id,
           type, payload, created_at, instance_id
      FROM agent_events
     WHERE session_name = ?
     ORDER BY rowid DESC
     LIMIT ?
  )
 ORDER BY rowid ASC`

	var (
		rows *sql.Rows
		err  error
	)
	if beforeRowID > 0 {
		rows, err = d.conn.Query(qWithBefore, sessionName, beforeRowID, limit)
	} else {
		rows, err = d.conn.Query(qNoBefore, sessionName, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("db: query session events before rowid: %w", err)
	}
	defer rows.Close()

	var result []EventRow
	for rows.Next() {
		var er EventRow
		var createdAt int64
		if err := rows.Scan(
			&er.RowID, &er.Event.ID, &er.Event.SessionName, &er.Event.Repo,
			&er.Event.Worktree, &er.Event.HarnessSessionID,
			&er.Event.Type, &er.Event.Payload, &createdAt, &er.Event.InstanceID,
		); err != nil {
			return nil, fmt.Errorf("db: scan event row: %w", err)
		}
		er.Event.CreatedAt = time.UnixMilli(createdAt)
		result = append(result, er)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate session events before rowid: %w", err)
	}
	return result, nil
}

// EventRow wraps an Event together with its SQLite rowid. The rowid is a
// monotonically increasing integer that clients use as since_event_id for
// replay requests.
type EventRow struct {
	RowID int64
	Event Event
}

// WriteEventReturningRowID is identical to WriteEvent but also returns the
// SQLite rowid of the newly inserted agent_events row. The rowid is used by
// the client IPC socket (D-6) as the monotonic event_id for since_event_id
// replay and fan-out ordering.
func (d *DB) WriteEventReturningRowID(e Event) (int64, error) {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	createdAt := e.CreatedAt.UnixMilli()

	// Second redaction control (issue #2589) — see WriteEvent.
	e.Payload = d.redactPayload(e.Payload)

	// Write-time account and profile resolution — see WriteEvent,
	// account_name.go, and profile_name.go.
	accountName := d.resolveAccountName()
	profileName := d.resolveProfileName()

	tx, err := d.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("db: write event: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const insertQ = `
INSERT INTO agent_events (id, session_name, repo, worktree, harness_session_id, type, payload, created_at, instance_id, account_name, profile_name)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := tx.Exec(insertQ, e.ID, e.SessionName, e.Repo, e.Worktree, e.HarnessSessionID, e.Type, e.Payload, createdAt, e.InstanceID, accountName, profileName)
	if err != nil {
		return 0, fmt.Errorf("db: write event: insert: %w", err)
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db: write event: last insert id: %w", err)
	}

	const updateQ = `
UPDATE agent_status
   SET last_seen = MAX(last_seen, ?)
 WHERE session_name = ?`
	if _, err := tx.Exec(updateQ, createdAt, e.SessionName); err != nil {
		return 0, fmt.Errorf("db: write event: update last_seen: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("db: write event: commit: %w", err)
	}
	return rowID, nil
}
