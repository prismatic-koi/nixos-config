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
// insertion order). If a non-terminal row already exists for (repo, pr), the
// existing row is returned unchanged (idempotent success). A terminal row
// (merged, failed, cancelled, abandoned) is treated as gone — a fresh row
// is inserted (via ON CONFLICT DO UPDATE).
//
// repo is the short repo slug this PR belongs to. It is stored on the row
// and used together with pr as the composite primary key, so that PR
// numbers can safely collide across repos sharing one prism.db. title is the
// PR title stored for display in `prism merges list`; pass nil if the title
// is not known at enqueue time.
//
// Returns the resulting row (existing or newly inserted).
func (d *DB) EnqueueMerge(pr int, repo, sessionName, instanceID string, title *string) (*PendingMerge, error) {
	// Check for an existing non-terminal row scoped to this (repo, pr).
	existing, err := d.PendingMergeByPR(pr, repo)
	if err != nil {
		return nil, fmt.Errorf("db: enqueue merge: check existing: %w", err)
	}
	if existing != nil && !isMergeTerminal(existing.Status) {
		// Non-terminal row exists — idempotent success.
		return existing, nil
	}

	now := time.Now().UnixMilli()
	const q = `
INSERT INTO pending_merges (repo, pr, session_name, instance_id, queue_position, status, title, queued_at)
VALUES (?, ?, ?, ?, ?, 'watching', ?, ?)
ON CONFLICT(repo, pr) DO UPDATE SET
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
	if _, err := d.conn.Exec(q, repo, pr, sessionName, instanceID, now, title, now); err != nil {
		return nil, fmt.Errorf("db: enqueue merge: insert: %w", err)
	}
	row, err := d.PendingMergeByPR(pr, repo)
	if err != nil {
		return nil, fmt.Errorf("db: enqueue merge: refetch: %w", err)
	}
	return row, nil
}

// PendingMergeByPR returns the pending_merges row for the given (pr, repo),
// or nil if not found. The composite primary key is (repo, pr), so callers
// MUST pass the caller's repo to avoid cross-repo PR-number collisions.
// Passing an empty repo only ever matches rows that failed the migration
// backfill (session_name contained no '@' at migration time). Production
// callers must always pass a non-empty repo.
func (d *DB) PendingMergeByPR(pr int, repo string) (*PendingMerge, error) {
	const q = `
SELECT repo, pr, session_name, instance_id, queue_position, status, title, error,
       queued_at, last_checked_at, merged_at, ended_at
  FROM pending_merges
 WHERE repo = ? AND pr = ?`
	row := d.conn.QueryRow(q, repo, pr)
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
// handled by EndIncarnation on sidecar shutdown, which sets rows to
// 'abandoned' before the new sidecar starts. prism restore and the
// stale-sidecar path of prism switch continue the session, so they give
// those rows back with ResumeMerges.
//
// This selector ignores instance_id, so two live sidecars of one session
// poll the same rows. A caller gives the rows to a new sidecar only after
// it proves that the old sidecar no longer runs (cmd/merge_queue_resume.go).
//
// session_name embeds the repo (`<repo>@<branch>` convention), so this
// selector remains implicitly repo-scoped without an explicit repo filter.
func (d *DB) MergeQueueHead(sessionName string) (*PendingMerge, error) {
	const q = `
SELECT repo, pr, session_name, instance_id, queue_position, status, title, error,
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

// UpdateMergeLastChecked sets last_checked_at to now for the given
// (pr, repo). repo is required so that the watcher's per-tick heartbeat
// only ever touches the row belonging to its own coordinator.
func (d *DB) UpdateMergeLastChecked(pr int, repo string) error {
	now := time.Now().UnixMilli()
	_, err := d.conn.Exec(
		"UPDATE pending_merges SET last_checked_at = ? WHERE repo = ? AND pr = ?",
		now, repo, pr,
	)
	if err != nil {
		return fmt.Errorf("db: update merge last checked: %w", err)
	}
	return nil
}

// TerminateMerge transitions a pending_merges row to a terminal status,
// scoped to (repo, pr). status must be one of 'merged', 'failed',
// 'cancelled', or 'abandoned'. errMsg is stored in the error column;
// pass "" for no error. merged_at is populated when status = 'merged';
// ended_at is always set.
//
// repo is required so that the watcher's terminal write only ever touches
// the row belonging to its own coordinator.
func (d *DB) TerminateMerge(pr int, repo, status, errMsg string) error {
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
 WHERE repo = ? AND pr = ?`
	_, err := d.conn.Exec(q, status, errPtr, mergedAt, now, repo, pr)
	if err != nil {
		return fmt.Errorf("db: terminate merge: %w", err)
	}
	return nil
}

// abandonedByStopError is the error of a row that the stop of its
// coordinator sidecar abandoned.
const abandonedByStopError = "coordinator session ended"

const abandonWatchingMergesSQL = `
UPDATE pending_merges
   SET status   = 'abandoned',
       error    = ?,
       ended_at = ?
 WHERE instance_id = ? AND status = 'watching'`

// AbandonWatchingMerges transitions all watching rows for instanceID to
// 'abandoned' with error = 'coordinator session ended', so that a new
// coordinator never inherits stale watching rows. The sidecar Shutdown does
// this through EndIncarnation. The sidecar cannot tell a restart from an
// end. When prism restore or prism switch restarts the coordinator
// session, it gives the rows back with ResumeMerges.
//
// Scoping by instance_id is already sufficient to prevent cross-repo
// spillover: instance_id is unique per coordinator and each coordinator
// only ever writes rows for its own repo.
func (d *DB) AbandonWatchingMerges(instanceID string) error {
	now := time.Now().UnixMilli()
	if _, err := d.conn.Exec(abandonWatchingMergesSQL, abandonedByStopError, now, instanceID); err != nil {
		return fmt.Errorf("db: abandon watching merges: %w", err)
	}
	return nil
}

// EndIncarnation abandons the watching rows of instanceID, as
// AbandonWatchingMerges does, and ends the sessions row of instanceID, as
// UpdateSessionEnded does. It does both in one transaction, with one time
// stamp, so that MergesAbandonedByStop can find the rows that this stop
// abandoned (#3175). The sidecar Shutdown calls it.
func (d *DB) EndIncarnation(instanceID, endState string) error {
	now := time.Now().UnixMilli()
	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("db: end incarnation: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(abandonWatchingMergesSQL, abandonedByStopError, now, instanceID); err != nil {
		return fmt.Errorf("db: end incarnation: abandon watching merges: %w", err)
	}
	if _, err := tx.Exec(updateSessionEndedSQL, now, endState, instanceID); err != nil {
		return fmt.Errorf("db: end incarnation: end session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: end incarnation: commit: %w", err)
	}
	return nil
}

// MergesAbandonedByStop returns, in queue order, the rows of the
// coordinator session sessionName that the stop which ended the
// incarnation instanceID abandoned. That stop can come before the restart:
// at a host shutdown, the sidecar stops before the restore at login.
//
// A row qualifies only when its ended_at is equal to the ended_at of the
// sessions row of instanceID. EndIncarnation writes both with one time
// stamp. A row that an earlier stop of the same instance abandoned has an
// earlier ended_at, so it does not qualify. When the sessions row is open,
// no row qualifies.
func (d *DB) MergesAbandonedByStop(sessionName, instanceID string) ([]PendingMerge, error) {
	const q = `
SELECT p.repo, p.pr, p.session_name, p.instance_id, p.queue_position, p.status, p.title, p.error,
       p.queued_at, p.last_checked_at, p.merged_at, p.ended_at
  FROM pending_merges p
  JOIN sessions s ON s.instance_id = p.instance_id
 WHERE p.session_name = ? AND p.instance_id = ?
   AND p.status = 'abandoned' AND p.error = ?
   AND s.ended_at IS NOT NULL AND p.ended_at = s.ended_at
 ORDER BY p.queue_position ASC`
	rows, err := d.conn.Query(q, sessionName, instanceID, abandonedByStopError)
	if err != nil {
		return nil, fmt.Errorf("db: merges abandoned by stop: %w", err)
	}
	defer rows.Close()
	var merges []PendingMerge
	for rows.Next() {
		m, err := scanPendingMergeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("db: merges abandoned by stop: scan: %w", err)
		}
		merges = append(merges, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: merges abandoned by stop: iterate: %w", err)
	}
	return merges, nil
}

// WatchingMerges returns the watching rows of the coordinator session
// sessionName, in queue order, whatever their instance_id.
func (d *DB) WatchingMerges(sessionName string) ([]PendingMerge, error) {
	const q = `
SELECT repo, pr, session_name, instance_id, queue_position, status, title, error,
       queued_at, last_checked_at, merged_at, ended_at
  FROM pending_merges
 WHERE session_name = ? AND status = 'watching'
 ORDER BY queue_position ASC`
	rows, err := d.conn.Query(q, sessionName)
	if err != nil {
		return nil, fmt.Errorf("db: watching merges: %w", err)
	}
	defer rows.Close()
	var merges []PendingMerge
	for rows.Next() {
		m, err := scanPendingMergeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("db: watching merges: scan: %w", err)
		}
		merges = append(merges, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: watching merges: iterate: %w", err)
	}
	return merges, nil
}

// ResumeMerges gives each row of merges back to the queue of sessionName,
// owned by instanceID. A row that the stop of a sidecar abandoned becomes
// watching again. A row that is still watching only changes owner. A row in
// any other status does not change: the old watcher already decided its
// outcome and notified the coordinator. queue_position does not change, so
// the queue order stays the same. It returns the PR numbers of the rows that
// are watching after the call.
func (d *DB) ResumeMerges(sessionName, instanceID string, merges []PendingMerge) ([]int, error) {
	if len(merges) == 0 {
		return nil, nil
	}
	tx, err := d.conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("db: resume merges: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	const q = `
UPDATE pending_merges
   SET status      = 'watching',
       instance_id = ?,
       error       = NULL,
       ended_at    = NULL
 WHERE repo = ? AND pr = ? AND session_name = ?
   AND status IN ('watching', 'abandoned')`
	var resumed []int
	for _, m := range merges {
		res, err := tx.Exec(q, instanceID, m.Repo, m.PR, sessionName)
		if err != nil {
			return nil, fmt.Errorf("db: resume merges: PR #%d: %w", m.PR, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			resumed = append(resumed, m.PR)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("db: resume merges: commit: %w", err)
	}
	return resumed, nil
}

// CancelMerge transitions a watching row owned by instanceID to 'cancelled'.
// Returns (true, nil) when the row was cancelled; (false, nil) when the row
// does not exist, is already terminal, or is owned by a different instanceID.
//
// Scoping is (pr, repo, instance_id) so that a coordinator can only cancel
// rows in its own repo AND its own incarnation.
func (d *DB) CancelMerge(pr int, repo, instanceID string) (bool, error) {
	now := time.Now().UnixMilli()
	const q = `
UPDATE pending_merges
   SET status   = 'cancelled',
       ended_at = ?
 WHERE repo = ? AND pr = ? AND instance_id = ? AND status = 'watching'`
	res, err := d.conn.Exec(q, now, repo, pr, instanceID)
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
//   - "abandoned"      → abandoned rows for the same session_name, from every
//     coordinator incarnation. The current instanceID is included: a
//     restore can continue the instance after a stop abandoned its rows.
//
// instance_id is unique per coordinator (and every coordinator only ever
// writes rows for its own repo), so this selector is already
// repo-implicit and does not need an explicit repo filter.
func (d *DB) MergeQueueForInstance(instanceID, sessionName, filter string) ([]PendingMerge, error) {
	var (
		q    string
		args []any
	)
	sevenDaysAgo := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()

	switch filter {
	case "failed":
		q = `SELECT repo, pr, session_name, instance_id, queue_position, status, title, error,
		            queued_at, last_checked_at, merged_at, ended_at
		       FROM pending_merges
		      WHERE instance_id = ? AND status = 'failed'
		      ORDER BY queue_position ASC`
		args = []any{instanceID}
	case "abandoned":
		q = `SELECT repo, pr, session_name, instance_id, queue_position, status, title, error,
		            queued_at, last_checked_at, merged_at, ended_at
		       FROM pending_merges
		      WHERE session_name = ? AND status = 'abandoned'
		      ORDER BY queue_position ASC`
		args = []any{sessionName}
	case "all":
		// Include all terminal states (merged, cancelled, failed, abandoned) plus
		// watching, from the last 7 days, scoped to this instanceID.
		q = `SELECT repo, pr, session_name, instance_id, queue_position, status, title, error,
		            queued_at, last_checked_at, merged_at, ended_at
		       FROM pending_merges
		      WHERE instance_id = ? AND queued_at >= ?
		      ORDER BY queue_position ASC`
		args = []any{instanceID, sevenDaysAgo}
	default:
		// Default: only watching rows for this instanceID.
		q = `SELECT repo, pr, session_name, instance_id, queue_position, status, title, error,
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
		&m.Repo, &m.PR, &m.SessionName, &m.InstanceID, &m.QueuePosition, &m.Status,
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
		&m.Repo, &m.PR, &m.SessionName, &m.InstanceID, &m.QueuePosition, &m.Status,
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
