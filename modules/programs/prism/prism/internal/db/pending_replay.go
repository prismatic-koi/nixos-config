package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PendingReplayRow is one persisted row in pending_replay_deliveries. It is
// the on-disk twin of the sidecar's in-memory pendingReplayDelivery struct
// (internal/sidecar/delivery_dedup.go): a /prompt frame that arrived while
// the PI extension was disconnected and could not be enqueued on the
// outbound writer. See issue #2359 Gap B.
//
// The sidecar buffers deliveries here from its /prompt handler and drains
// them on the next successful pipe handshake (flushPendingReplay). Persisting
// to disk survives sidecar restart, so a coordinator's reply cannot vanish
// if the worker's sidecar cycles between delivery and the next handshake.
type PendingReplayRow struct {
	SessionName string
	DeliveryID  string
	Text        string
	DeliverAs   string
	Source      string
	QueuedAt    time.Time
}

// InsertPendingReplayDelivery persists a pending-replay entry. The insert
// uses ON CONFLICT DO NOTHING keyed by (session_name, delivery_id) so a
// caller that re-queues the same delivery_id (e.g. the flushPendingReplay
// re-buffer path when the outbound channel is not yet live) does not create
// a duplicate row. This mirrors the in-memory dedup semantics from #1685.
//
// When row.DeliveryID is empty (legacy caller shape with no minted UUID),
// a synthetic key of the form "no-id:<nanoseconds>" is used so that
// multiple no-ID entries do not collapse into one row.
//
// Returns the persist key actually written (either row.DeliveryID or the
// synthetic key) so the caller can later delete the exact row via
// DeletePendingReplayDelivery after successfully flushing it. Returns an
// empty string and a wrapped error on database failure. A successful call
// with an already-existing (session_name, delivery_id) still returns the
// key (the caller's row is treated as a dedup no-op, and the existing row
// remains authoritative).
func (d *DB) InsertPendingReplayDelivery(row PendingReplayRow) (string, error) {
	if row.SessionName == "" {
		return "", fmt.Errorf("db: insert pending replay: session_name is required")
	}
	keyID := row.DeliveryID
	if keyID == "" {
		// Synthetic key: nanosecond-precision timestamp keeps successive
		// no-ID entries distinct without needing a separate serial column.
		keyID = fmt.Sprintf("no-id:%d", time.Now().UnixNano())
	}
	queuedAt := row.QueuedAt
	if queuedAt.IsZero() {
		queuedAt = time.Now()
	}
	const q = `
INSERT INTO pending_replay_deliveries
  (session_name, delivery_id, text, deliver_as, source, queued_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(session_name, delivery_id) DO NOTHING`
	if _, err := d.conn.Exec(q,
		row.SessionName, keyID, row.Text, row.DeliverAs, row.Source, queuedAt.UnixMilli(),
	); err != nil {
		return "", fmt.Errorf("db: insert pending replay delivery: %w", err)
	}
	return keyID, nil
}

// LoadPendingReplayDeliveries returns all persisted pending-replay entries
// for sessionName in FIFO (queued_at ascending) order. Called by the sidecar
// at startup so that entries buffered by a prior sidecar incarnation are
// re-enqueued in the same order the caller submitted them. Returns an empty
// slice (not nil error) when no rows exist for sessionName.
func (d *DB) LoadPendingReplayDeliveries(sessionName string) ([]PendingReplayRow, error) {
	const q = `
SELECT session_name, delivery_id, text, deliver_as, source, queued_at
FROM pending_replay_deliveries
WHERE session_name = ?
ORDER BY queued_at ASC, delivery_id ASC`
	rows, err := d.conn.Query(q, sessionName)
	if err != nil {
		return nil, fmt.Errorf("db: load pending replay deliveries: %w", err)
	}
	defer rows.Close()
	var out []PendingReplayRow
	for rows.Next() {
		var row PendingReplayRow
		var queuedMs int64
		if err := rows.Scan(
			&row.SessionName, &row.DeliveryID, &row.Text,
			&row.DeliverAs, &row.Source, &queuedMs,
		); err != nil {
			return nil, fmt.Errorf("db: scan pending replay delivery: %w", err)
		}
		row.QueuedAt = time.UnixMilli(queuedMs)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate pending replay deliveries: %w", err)
	}
	return out, nil
}

// DeletePendingReplayDelivery removes one pending-replay entry keyed by
// (session_name, delivery_id). Called by the sidecar's flushPendingReplay
// after a delivery has been successfully enqueued on the outbound writer so
// that a subsequent sidecar restart does not re-deliver an already-forwarded
// frame. Returns nil when the row does not exist (idempotent).
//
// For no-ID entries (delivery_id == ""), the caller must remember and pass
// the synthetic key that InsertPendingReplayDelivery generated — the caller
// obtains it via LoadPendingReplayDeliveries' DeliveryID field, which
// surfaces the stored (synthetic or real) key verbatim.
func (d *DB) DeletePendingReplayDelivery(sessionName, deliveryID string) error {
	if sessionName == "" {
		return fmt.Errorf("db: delete pending replay: session_name is required")
	}
	if deliveryID == "" {
		// Empty delivery_id is not a valid stored key: InsertPendingReplayDelivery
		// substitutes a synthetic key when the caller passes empty. Deleting
		// with empty here would silently no-op; surface it as an error so
		// callers are forced to plumb the stored key correctly.
		return fmt.Errorf("db: delete pending replay: delivery_id is required (synthetic key expected for legacy callers)")
	}
	const q = `DELETE FROM pending_replay_deliveries WHERE session_name = ? AND delivery_id = ?`
	if _, err := d.conn.Exec(q, sessionName, deliveryID); err != nil {
		return fmt.Errorf("db: delete pending replay delivery: %w", err)
	}
	return nil
}

// DeletePendingReplayDeliveriesForSession removes every pending-replay entry
// for sessionName. Called from two lifecycle boundaries so a fresh session
// incarnation on the same name cannot resurrect a previous incarnation's
// stale coordinator directives (issue #2359 review-context follow-up):
//
//   - `prism cleanup` (severPiResumeLinkage) ends the session and severs the
//     pi resume linkage; the pending-replay buffer is the CLI-visibility
//     twin of that linkage and must be wiped at the same time.
//   - `event tmux-session-start` re-seeds a previously-ended row back to
//     `idle` when a spawn reuses the branch name (#2094 respawn-after-cleanup
//     path). The re-seed clears `ended_at` but the pending-replay rows would
//     otherwise still be scoped to `session_name`, so
//     `restorePendingReplayFromDB` on the fresh incarnation would drain them
//     into the new agent — a stale directive from the previous incarnation.
//     Clearing here on re-seed is the load-bearing fix for that hazard.
//
// Returns nil when no rows match (idempotent).
func (d *DB) DeletePendingReplayDeliveriesForSession(sessionName string) error {
	if sessionName == "" {
		return fmt.Errorf("db: delete pending replay for session: session_name is required")
	}
	if _, err := d.conn.Exec(
		`DELETE FROM pending_replay_deliveries WHERE session_name = ?`,
		sessionName,
	); err != nil {
		return fmt.Errorf("db: delete pending replay for session: %w", err)
	}
	return nil
}

// PrunePendingReplayDeliveriesOlderThan removes every pending-replay entry
// whose queued_at is older than the given cutoff (Unix milliseconds).
// Called from the periodic maintenance job so an abandoned session's rows
// do not accumulate indefinitely.
//
// The bound is generous by default (see internal/db/maintenance.go): the
// buffer's normal lifetime is minutes-to-hours (until the next handshake),
// so a multi-day cutoff only catches rows whose session never came back.
//
// Returns the number of rows deleted and any database error.
func (d *DB) PrunePendingReplayDeliveriesOlderThan(cutoffMillis int64) (int64, error) {
	res, err := d.conn.Exec(
		`DELETE FROM pending_replay_deliveries WHERE queued_at < ?`,
		cutoffMillis,
	)
	if err != nil {
		return 0, fmt.Errorf("db: prune pending replay deliveries: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: prune pending replay deliveries rows affected: %w", err)
	}
	return n, nil
}

// CountPendingReplayDeliveries returns the number of persisted entries for
// sessionName. Used in tests to assert that flush-then-delete leaves the
// table empty and that a re-flush does not resurrect deleted entries.
func (d *DB) CountPendingReplayDeliveries(sessionName string) (int, error) {
	var n int
	err := d.conn.QueryRow(
		`SELECT COUNT(*) FROM pending_replay_deliveries WHERE session_name = ?`,
		sessionName,
	).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("db: count pending replay deliveries: %w", err)
	}
	return n, nil
}
