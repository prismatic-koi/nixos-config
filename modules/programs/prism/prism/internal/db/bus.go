package db

import (
	"database/sql"
	"fmt"
	"time"
)

// WriteBusMessage inserts a new row into bus_messages with delivered_at=NULL.
// When msg.ToInstanceID is non-nil, it is written to to_instance_id so that
// delivery can be filtered to the correct session incarnation.
func (d *DB) WriteBusMessage(msg BusMessage) error {
	var sentAt int64
	if msg.SentAt.IsZero() {
		sentAt = time.Now().UnixMilli()
	} else {
		sentAt = msg.SentAt.UnixMilli()
	}
	const q = `
INSERT INTO bus_messages (id, from_session, to_session, to_instance_id, repo, text, urgency, sent_at, delivered_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`
	_, err := d.conn.Exec(q, msg.ID, msg.FromSession, msg.ToSession, msg.ToInstanceID, msg.Repo, msg.Text, msg.Urgency, sentAt)
	if err != nil {
		return fmt.Errorf("db: write bus message: %w", err)
	}
	return nil
}

// WriteBusMessageDelivered inserts a new row into bus_messages with
// delivered_at set to now. It records an audit-trail write for a prompt that
// was delivered via HTTP, so the plugin does not deliver it again.
func (d *DB) WriteBusMessageDelivered(msg BusMessage) error {
	now := time.Now().UnixMilli()
	var sentAt int64
	if msg.SentAt.IsZero() {
		sentAt = now
	} else {
		sentAt = msg.SentAt.UnixMilli()
	}
	const q = `
INSERT INTO bus_messages (id, from_session, to_session, to_instance_id, repo, text, urgency, sent_at, delivered_at, failed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`
	_, err := d.conn.Exec(q, msg.ID, msg.FromSession, msg.ToSession, msg.ToInstanceID, msg.Repo, msg.Text, msg.Urgency, sentAt, now)
	if err != nil {
		return fmt.Errorf("db: write bus message delivered: %w", err)
	}
	return nil
}

// WriteBusMessageFailed inserts a new row into bus_messages with failed_at set
// to now and delivered_at=NULL. This records a notification that was attempted
// but could not be delivered after all retries were exhausted. It is the
// authoritative signal that a notification was silently lost.
func (d *DB) WriteBusMessageFailed(msg BusMessage) error {
	now := time.Now().UnixMilli()
	var sentAt int64
	if msg.SentAt.IsZero() {
		sentAt = now
	} else {
		sentAt = msg.SentAt.UnixMilli()
	}
	const q = `
INSERT INTO bus_messages (id, from_session, to_session, to_instance_id, repo, text, urgency, sent_at, delivered_at, failed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`
	_, err := d.conn.Exec(q, msg.ID, msg.FromSession, msg.ToSession, msg.ToInstanceID, msg.Repo, msg.Text, msg.Urgency, sentAt, now)
	if err != nil {
		return fmt.Errorf("db: write bus message failed: %w", err)
	}
	return nil
}

// PurgeBusMessages deletes undelivered and unfailed bus_messages rows where
// from_session or to_session matches sessionName. Delivered messages
// (delivered_at IS NOT NULL) and failed messages (failed_at IS NOT NULL) are
// left untouched so that delivery audit records survive session cleanup. It is
// safe to call when no matching rows exist — the operation is a no-op and
// returns nil.
func (d *DB) PurgeBusMessages(sessionName string) error {
	const q = `
DELETE FROM bus_messages
WHERE delivered_at IS NULL
  AND failed_at IS NULL
  AND (from_session = ? OR to_session = ?)`
	if _, err := d.conn.Exec(q, sessionName, sessionName); err != nil {
		return fmt.Errorf("db: purge bus messages: %w", err)
	}
	return nil
}

// PurgeStaleInstanceMessages deletes undelivered and unfailed bus_messages
// addressed to toSession whose to_instance_id does not match
// currentInstanceID. This purges messages written to a previous incarnation
// of the session that never got delivered. Messages with to_instance_id IS
// NULL (no instance tag), delivered messages
// (delivered_at IS NOT NULL), and failed-delivery audit records
// (failed_at IS NOT NULL) are all left intact.
//
// It is safe to call when no matching rows exist — the operation is a no-op
// and returns nil.
func (d *DB) PurgeStaleInstanceMessages(toSession, currentInstanceID string) error {
	const q = `
DELETE FROM bus_messages
WHERE to_session = ?
  AND delivered_at IS NULL
  AND failed_at IS NULL
  AND to_instance_id IS NOT NULL
  AND to_instance_id != ?`
	if _, err := d.conn.Exec(q, toSession, currentInstanceID); err != nil {
		return fmt.Errorf("db: purge stale instance messages: %w", err)
	}
	return nil
}

// FindRecentEquivalentBusMessage returns the most recent bus_messages row
// matching (fromSession, toSession, text) byte-equal whose sent_at is within
// the given window (>= now - window). It returns (nil, nil) when no match
// exists. failed_at must be NULL on the match. A failed send is not a valid
// prior delivery to dedup against, so the caller must retry. delivered_at can
// be NULL (queued but not yet flushed). That still counts as a prior
// in-flight delivery the caller must not duplicate.
//
// This powers the sender-side idempotency guard in `prism escalate` so a
// worker that re-runs the same `escalate` invocation within a short window
// (default 5 minutes) does not produce a second delivery to the coordinator.
func (d *DB) FindRecentEquivalentBusMessage(fromSession, toSession, text string, window time.Duration) (*BusMessage, error) {
	threshold := time.Now().Add(-window).UnixMilli()
	const q = `
SELECT id, from_session, to_session, to_instance_id, repo, text, urgency, sent_at, delivered_at, failed_at
FROM bus_messages
WHERE from_session = ?
  AND to_session = ?
  AND text = ?
  AND failed_at IS NULL
  AND sent_at >= ?
ORDER BY sent_at DESC
LIMIT 1`
	row := d.conn.QueryRow(q, fromSession, toSession, text, threshold)
	m, err := scanBusMessage(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: find recent equivalent bus message: %w", err)
	}
	return &m, nil
}

// scanBusMessage scans a BusMessage from the given scanner.
func scanBusMessage(s scanner) (BusMessage, error) {
	var m BusMessage
	var sentAt int64
	var deliveredAt sql.NullInt64
	var failedAt sql.NullInt64
	var toInstanceID sql.NullString
	err := s.Scan(
		&m.ID, &m.FromSession, &m.ToSession, &toInstanceID, &m.Repo, &m.Text, &m.Urgency,
		&sentAt, &deliveredAt, &failedAt,
	)
	if err != nil {
		return BusMessage{}, err
	}
	m.SentAt = time.UnixMilli(sentAt)
	if deliveredAt.Valid {
		t := time.UnixMilli(deliveredAt.Int64)
		m.DeliveredAt = &t
	}
	if failedAt.Valid {
		t := time.UnixMilli(failedAt.Int64)
		m.FailedAt = &t
	}
	if toInstanceID.Valid {
		id := toInstanceID.String
		m.ToInstanceID = &id
	}
	return m, nil
}
