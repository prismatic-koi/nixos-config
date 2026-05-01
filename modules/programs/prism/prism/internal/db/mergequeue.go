package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// isMergeTerminal returns true when status is a terminal state.
func isMergeTerminal(status string) bool {
	switch status {
	case "merged", "failed", "cancelled", "abandoned":
		return true
	}
	return false
}

// EnqueueMerge inserts a new pending_merges row with status = 'watching'.
// queue_position is set to the current Unix millisecond timestamp (monotone
// insertion order). If a non-terminal row already exists for pr, the existing
// row is returned unchanged (idempotent success). A terminal row (merged,
// failed, cancelled, abandoned) is treated as gone — a fresh row is inserted.
// title is the PR title stored for display in `prism merges list`; pass nil
// if the title is not known at enqueue time.
// Returns the resulting row (existing or newly inserted).
func (d *DB) EnqueueMerge(pr int, sessionName, instanceID string, title *string) (*PendingMerge, error) {
	// Check for an existing non-terminal row.
	existing, err := d.PendingMergeByPR(pr)
	if err != nil {
		return nil, fmt.Errorf("db: enqueue merge: check existing: %w", err)
	}
	if existing != nil && !isMergeTerminal(existing.Status) {
		// Non-terminal row exists — idempotent success.
		return existing, nil
	}

	now := time.Now().UnixMilli()
	const q = `
INSERT INTO pending_merges (pr, session_name, instance_id, queue_position, status, title, queued_at)
VALUES (?, ?, ?, ?, 'watching', ?, ?)
ON CONFLICT(pr) DO UPDATE SET
  session_name    = excluded.session_name,
  instance_id     = excluded.instance_id,
  queue_position  = excluded.queue_position,
  status          = 'watching',
  title           = excluded.title,
  error           = NULL,
  queued_at       = excluded.queued_at,
  last_checked_at = NULL,
  merged_at       = NULL,
  ended_at        = NULL`
	if _, err := d.conn.Exec(q, pr, sessionName, instanceID, now, title, now); err != nil {
		return nil, fmt.Errorf("db: enqueue merge: insert: %w", err)
	}
	row, err := d.PendingMergeByPR(pr)
	if err != nil {
		return nil, fmt.Errorf("db: enqueue merge: refetch: %w", err)
	}
	return row, nil
}

// PendingMergeByPR returns the pending_merges row for pr, or nil if not found.
func (d *DB) PendingMergeByPR(pr int) (*PendingMerge, error) {
	const q = `
SELECT pr, session_name, instance_id, queue_position, status, title, error,
       queued_at, last_checked_at, merged_at, ended_at
  FROM pending_merges
 WHERE pr = ?`
	row := d.conn.QueryRow(q, pr)
	m, err := scanPendingMerge(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: pending merge by pr: %w", err)
	}
	return m, nil
}

// MergeQueueHead returns the head of the merge queue for the given
// sessionName: the watching row with the lowest queue_position. Returns nil
// when the queue is empty.
//
// Filtering by session_name rather than instance_id ensures the watcher picks
// up rows regardless of which instance_id was written at enqueue time (e.g.
// when prism merge mints a fresh UUID on the fly). Incarnation isolation —
// preventing stale rows from a previous sidecar from being re-processed — is
// handled by AbandonWatchingMerges on sidecar shutdown, which sets rows to
// 'abandoned' before the new sidecar starts.
func (d *DB) MergeQueueHead(sessionName string) (*PendingMerge, error) {
	const q = `
SELECT pr, session_name, instance_id, queue_position, status, title, error,
       queued_at, last_checked_at, merged_at, ended_at
  FROM pending_merges
 WHERE session_name = ? AND status = 'watching'
 ORDER BY queue_position ASC
 LIMIT 1`
	row := d.conn.QueryRow(q, sessionName)
	m, err := scanPendingMerge(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: merge queue head: %w", err)
	}
	return m, nil
}

// UpdateMergeLastChecked sets last_checked_at to now for the given pr.
func (d *DB) UpdateMergeLastChecked(pr int) error {
	now := time.Now().UnixMilli()
	_, err := d.conn.Exec(
		"UPDATE pending_merges SET last_checked_at = ? WHERE pr = ?",
		now, pr,
	)
	if err != nil {
		return fmt.Errorf("db: update merge last checked: %w", err)
	}
	return nil
}

// TerminateMerge transitions a pending_merges row to a terminal status.
// status must be one of 'merged', 'failed', 'cancelled', or 'abandoned'.
// errMsg is stored in the error column; pass "" for no error.
// merged_at is populated when status = 'merged'; ended_at is always set.
func (d *DB) TerminateMerge(pr int, status, errMsg string) error {
	now := time.Now().UnixMilli()
	var errPtr *string
	if errMsg != "" {
		errPtr = &errMsg
	}
	var mergedAt *int64
	if status == "merged" {
		mergedAt = &now
	}
	const q = `
UPDATE pending_merges
   SET status   = ?,
       error    = ?,
       merged_at = ?,
       ended_at  = ?
 WHERE pr = ?`
	_, err := d.conn.Exec(q, status, errPtr, mergedAt, now, pr)
	if err != nil {
		return fmt.Errorf("db: terminate merge: %w", err)
	}
	return nil
}

// AbandonWatchingMerges transitions all watching rows for instanceID to
// 'abandoned' with error = 'coordinator session ended'. Called on sidecar
// shutdown so that a new coordinator never inherits stale watching rows.
func (d *DB) AbandonWatchingMerges(instanceID string) error {
	now := time.Now().UnixMilli()
	const q = `
UPDATE pending_merges
   SET status   = 'abandoned',
       error    = 'coordinator session ended',
       ended_at = ?
 WHERE instance_id = ? AND status = 'watching'`
	_, err := d.conn.Exec(q, now, instanceID)
	if err != nil {
		return fmt.Errorf("db: abandon watching merges: %w", err)
	}
	return nil
}

// CancelMerge transitions a watching row owned by instanceID to 'cancelled'.
// Returns (true, nil) when the row was cancelled; (false, nil) when the row
// does not exist, is already terminal, or is owned by a different instanceID.
func (d *DB) CancelMerge(pr int, instanceID string) (bool, error) {
	now := time.Now().UnixMilli()
	const q = `
UPDATE pending_merges
   SET status   = 'cancelled',
       ended_at = ?
 WHERE pr = ? AND instance_id = ? AND status = 'watching'`
	res, err := d.conn.Exec(q, now, pr, instanceID)
	if err != nil {
		return false, fmt.Errorf("db: cancel merge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("db: cancel merge: rows affected: %w", err)
	}
	return n > 0, nil
}

// MergeQueueForInstance returns merge queue rows for instanceID, optionally
// filtered by status. Rows are sorted by queue_position ascending.
// filter values: "watching", "failed", "all", "abandoned" (special: cross-instance).
//
//   - "" or "watching" → only watching rows for instanceID
//   - "failed"         → failed rows for instanceID
//   - "all"            → all non-abandoned rows for instanceID from the last 7 days
//   - "abandoned"      → abandoned rows for the same session_name but a different
//     instanceID (previous coordinator incarnations)
func (d *DB) MergeQueueForInstance(instanceID, sessionName, filter string) ([]PendingMerge, error) {
	var (
		q    string
		args []any
	)
	sevenDaysAgo := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()

	switch filter {
	case "failed":
		q = `SELECT pr, session_name, instance_id, queue_position, status, title, error,
		            queued_at, last_checked_at, merged_at, ended_at
		       FROM pending_merges
		      WHERE instance_id = ? AND status = 'failed'
		      ORDER BY queue_position ASC`
		args = []any{instanceID}
	case "abandoned":
		q = `SELECT pr, session_name, instance_id, queue_position, status, title, error,
		            queued_at, last_checked_at, merged_at, ended_at
		       FROM pending_merges
		      WHERE session_name = ? AND instance_id != ? AND status = 'abandoned'
		      ORDER BY queue_position ASC`
		args = []any{sessionName, instanceID}
	case "all":
		// Include all terminal states (merged, cancelled, failed, abandoned) plus
		// watching, from the last 7 days, scoped to this instanceID. Per AC:
		// "includes terminal states (merged, cancelled, failed, abandoned) from
		// the last 7 days."
		q = `SELECT pr, session_name, instance_id, queue_position, status, title, error,
		            queued_at, last_checked_at, merged_at, ended_at
		       FROM pending_merges
		      WHERE instance_id = ? AND queued_at >= ?
		      ORDER BY queue_position ASC`
		args = []any{instanceID, sevenDaysAgo}
	default:
		// Default: only watching rows for this instanceID.
		q = `SELECT pr, session_name, instance_id, queue_position, status, title, error,
		            queued_at, last_checked_at, merged_at, ended_at
		       FROM pending_merges
		      WHERE instance_id = ? AND status = 'watching'
		      ORDER BY queue_position ASC`
		args = []any{instanceID}
	}

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: merge queue for instance: %w", err)
	}
	defer rows.Close()

	var merges []PendingMerge
	for rows.Next() {
		m, scanErr := scanPendingMergeRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("db: merge queue for instance: scan: %w", scanErr)
		}
		merges = append(merges, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: merge queue for instance: iterate: %w", err)
	}
	return merges, nil
}

// scanPendingMerge scans a single *sql.Row into a PendingMerge.
func scanPendingMerge(row *sql.Row) (*PendingMerge, error) {
	var m PendingMerge
	var (
		queuedAtMs      int64
		lastCheckedAtMs *int64
		mergedAtMs      *int64
		endedAtMs       *int64
	)
	err := row.Scan(
		&m.PR, &m.SessionName, &m.InstanceID, &m.QueuePosition, &m.Status,
		&m.Title, &m.Error, &queuedAtMs, &lastCheckedAtMs, &mergedAtMs, &endedAtMs,
	)
	if err != nil {
		return nil, err
	}
	m.QueuedAt = time.UnixMilli(queuedAtMs)
	if lastCheckedAtMs != nil {
		t := time.UnixMilli(*lastCheckedAtMs)
		m.LastCheckedAt = &t
	}
	if mergedAtMs != nil {
		t := time.UnixMilli(*mergedAtMs)
		m.MergedAt = &t
	}
	if endedAtMs != nil {
		t := time.UnixMilli(*endedAtMs)
		m.EndedAt = &t
	}
	return &m, nil
}

// scanPendingMergeRow scans a *sql.Rows (multi-row scanner) into a PendingMerge.
func scanPendingMergeRow(rows *sql.Rows) (PendingMerge, error) {
	var m PendingMerge
	var (
		queuedAtMs      int64
		lastCheckedAtMs *int64
		mergedAtMs      *int64
		endedAtMs       *int64
	)
	err := rows.Scan(
		&m.PR, &m.SessionName, &m.InstanceID, &m.QueuePosition, &m.Status,
		&m.Title, &m.Error, &queuedAtMs, &lastCheckedAtMs, &mergedAtMs, &endedAtMs,
	)
	if err != nil {
		return m, err
	}
	m.QueuedAt = time.UnixMilli(queuedAtMs)
	if lastCheckedAtMs != nil {
		t := time.UnixMilli(*lastCheckedAtMs)
		m.LastCheckedAt = &t
	}
	if mergedAtMs != nil {
		t := time.UnixMilli(*mergedAtMs)
		m.MergedAt = &t
	}
	if endedAtMs != nil {
		t := time.UnixMilli(*endedAtMs)
		m.EndedAt = &t
	}
	return m, nil
}
