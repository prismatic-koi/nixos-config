// Package db provides the prism SQLite database layer.
//
// The database is located at $XDG_STATE_HOME/prism/prism.db, falling back to
// $HOME/.local/state/prism/prism.db. All four tables (agent_events,
// agent_status, bus_messages, schema_version) are created on Open if they do
// not already exist.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register sqlite3 driver
)

// DB wraps a SQLite connection.
type DB struct {
	conn *sql.DB
	path string
}

// Path returns the filesystem path of the database file.
func (d *DB) Path() string { return d.path }

// QueryRow executes a query that returns at most one row. Exposed for testing.
func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.conn.QueryRow(query, args...)
}

// Event represents a row in the agent_events table.
type Event struct {
	ID          string
	SessionName string
	Repo        string
	Worktree    string
	OpencodeSID *string
	Type        string
	Payload     string // raw JSON
	CreatedAt   time.Time
}

// Status represents a row in the agent_status table.
type Status struct {
	SessionName string
	Repo        string
	Worktree    string
	State       string
	Title       *string
	OpencodeSID *string
	AgentName   *string
	ModelID     *string
	LastSeen    time.Time
	EndedAt     *time.Time
}

// BusMessage represents a row in the bus_messages table.
type BusMessage struct {
	ID          string
	FromSession string
	ToSession   string
	Repo        string
	Text        string
	Urgency     string
	SentAt      time.Time
	DeliveredAt *time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS agent_events (
  id           TEXT PRIMARY KEY,
  session_name TEXT NOT NULL,
  repo         TEXT NOT NULL,
  worktree     TEXT NOT NULL,
  opencode_sid TEXT,
  type         TEXT NOT NULL,
  payload      TEXT NOT NULL,
  created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_session ON agent_events(session_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_repo    ON agent_events(repo, type, created_at DESC);

CREATE TABLE IF NOT EXISTS agent_status (
  session_name TEXT PRIMARY KEY,
  repo         TEXT NOT NULL,
  worktree     TEXT NOT NULL,
  state        TEXT NOT NULL,
  title        TEXT,
  opencode_sid TEXT,
  agent_name   TEXT,
  model_id     TEXT,
  last_seen    INTEGER NOT NULL,
  ended_at     INTEGER
);

CREATE TABLE IF NOT EXISTS bus_messages (
  id           TEXT PRIMARY KEY,
  from_session TEXT NOT NULL,
  to_session   TEXT NOT NULL,
  repo         TEXT NOT NULL,
  text         TEXT NOT NULL,
  urgency      TEXT NOT NULL DEFAULT 'normal',
  sent_at      INTEGER NOT NULL,
  delivered_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_bus_pending ON bus_messages(to_session, delivered_at)
  WHERE delivered_at IS NULL;

CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER NOT NULL
);
`

// Open opens (or creates) the prism database at path.
// It creates parent directories as needed, enables WAL mode, runs the full
// schema, and sets schema_version=1 if the table is empty. If the DB is at
// version 1, it applies the v2 migration (adding agent_name and model_id
// columns to agent_status) and bumps schema_version to 2.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("db: create parent dirs: %w", err)
	}

	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}

	// Enable WAL mode for better concurrent read/write performance.
	if _, err := conn.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: set WAL mode: %w", err)
	}

	// Wait up to 5 seconds before returning SQLITE_BUSY when the DB is locked
	// by another process (e.g. the plugin writing concurrently).
	if _, err := conn.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: set busy timeout: %w", err)
	}

	// Create all tables.
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: apply schema: %w", err)
	}

	// Set schema_version=2 if the table is empty (fresh database already has
	// all columns including the v2 additions, so no migration is needed).
	var count int
	if err := conn.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: check schema_version: %w", err)
	}
	if count == 0 {
		if _, err := conn.Exec("INSERT INTO schema_version (version) VALUES (2)"); err != nil {
			conn.Close()
			return nil, fmt.Errorf("db: set schema_version: %w", err)
		}
	}

	// Apply pending migrations.
	var version int
	if err := conn.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: read schema_version: %w", err)
	}
	if version == 1 {
		// Migration v1 → v2: add agent_name and model_id to agent_status.
		migrations := []string{
			"ALTER TABLE agent_status ADD COLUMN agent_name TEXT",
			"ALTER TABLE agent_status ADD COLUMN model_id TEXT",
			"UPDATE schema_version SET version = 2",
		}
		for _, m := range migrations {
			if _, err := conn.Exec(m); err != nil {
				conn.Close()
				return nil, fmt.Errorf("db: migration v1→v2: %w", err)
			}
		}
	}

	return &DB{conn: conn, path: path}, nil
}

// Close closes the underlying database connection.
func (d *DB) Close() error {
	return d.conn.Close()
}

// UpsertStatus inserts or updates the agent_status row for sessionName.
// repo and worktree are only set on initial insert — they do not change on
// conflict. title, opencodeSID, agentName, and modelID are updated only when
// non-nil (COALESCE).
func (d *DB) UpsertStatus(sessionName, repo, worktree, state string, title *string, opencodeSID *string) error {
	return d.UpsertStatusWithAgent(sessionName, repo, worktree, state, title, opencodeSID, nil, nil)
}

// UpsertStatusWithAgent is like UpsertStatus but also accepts agentName and
// modelID, which are written to agent_status.agent_name and agent_status.model_id
// using COALESCE (only overwriting when non-nil).
func (d *DB) UpsertStatusWithAgent(sessionName, repo, worktree, state string, title *string, opencodeSID *string, agentName *string, modelID *string) error {
	now := time.Now().UnixMilli()
	const q = `
INSERT INTO agent_status (session_name, repo, worktree, state, title, opencode_sid, agent_name, model_id, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_name) DO UPDATE SET
  state        = excluded.state,
  title        = COALESCE(excluded.title, title),
  opencode_sid = COALESCE(excluded.opencode_sid, opencode_sid),
  agent_name   = COALESCE(excluded.agent_name, agent_name),
  model_id     = COALESCE(excluded.model_id, model_id),
  last_seen    = excluded.last_seen`
	_, err := d.conn.Exec(q, sessionName, repo, worktree, state, title, opencodeSID, agentName, modelID, now)
	if err != nil {
		return fmt.Errorf("db: upsert status: %w", err)
	}
	return nil
}

// UpsertStatusIfNotTerminal upserts the state for sessionName only when the
// current state is not already a terminal state (finished, interrupted, or
// deleted) and the session has not yet been ended (ended_at IS NULL). Returns
// (true, nil) if the update was applied, (false, nil) if the session was
// already in a terminal state, has been ended, or did not exist, or
// (false, err) on a database error.
//
// This is used by the pane-died hook to transition active sessions to
// "interrupted" without clobbering a clean "finished" that was written first,
// and without acting on sessions that have already been ended by cleanup.
func (d *DB) UpsertStatusIfNotTerminal(sessionName, state string) (bool, error) {
	now := time.Now().UnixMilli()
	const q = `
UPDATE agent_status
SET state = ?, last_seen = ?
WHERE session_name = ?
  AND ended_at IS NULL
  AND state NOT IN ('finished', 'interrupted', 'deleted')`
	res, err := d.conn.Exec(q, state, now, sessionName)
	if err != nil {
		return false, fmt.Errorf("db: upsert status if not terminal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("db: upsert status if not terminal: rows affected: %w", err)
	}
	return n > 0, nil
}

// SetEnded marks the session as ended by setting ended_at to now.
func (d *DB) SetEnded(sessionName string) error {
	now := time.Now().UnixMilli()
	_, err := d.conn.Exec(
		"UPDATE agent_status SET ended_at = ? WHERE session_name = ?",
		now, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set ended: %w", err)
	}
	return nil
}

// WriteEvent inserts an event row into agent_events.
func (d *DB) WriteEvent(e Event) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	createdAt := e.CreatedAt.UnixMilli()
	const q = `
INSERT INTO agent_events (id, session_name, repo, worktree, opencode_sid, type, payload, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := d.conn.Exec(q, e.ID, e.SessionName, e.Repo, e.Worktree, e.OpencodeSID, e.Type, e.Payload, createdAt)
	if err != nil {
		return fmt.Errorf("db: write event: %w", err)
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

	q := "SELECT id, session_name, repo, worktree, opencode_sid, type, payload, created_at FROM agent_events" +
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
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.OpencodeSID, &e.Type, &e.Payload, &createdAt); err != nil {
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
// contains a "messageId" field matching one of the provided IDs. Only events
// of the specified types are returned; pass nil for types to return all types.
// Results are ordered by created_at ASC.
//
// This is used by checkin's secondary query to fetch tool_call, tool_result,
// permission_ask, permission_denied, and thinking events that belong to a set
// of user message turns retrieved by the primary query.
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
	conditions = append(conditions, "JSON_EXTRACT(payload, '$.messageId') IN ("+strings.Join(idPlaceholders, ",")+")")

	if len(types) > 0 {
		typePlaceholders := make([]string, len(types))
		for i, t := range types {
			typePlaceholders[i] = "?"
			args = append(args, t)
		}
		conditions = append(conditions, "type IN ("+strings.Join(typePlaceholders, ",")+")")
	}

	q := "SELECT id, session_name, repo, worktree, opencode_sid, type, payload, created_at FROM agent_events" +
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
		if err := rows.Scan(&e.ID, &e.SessionName, &e.Repo, &e.Worktree, &e.OpencodeSID, &e.Type, &e.Payload, &createdAt); err != nil {
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

// CurrentStatus returns the agent_status row for sessionName, or nil if not found.
func (d *DB) CurrentStatus(sessionName string) (*Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, opencode_sid, agent_name, model_id, last_seen, ended_at
FROM agent_status
WHERE session_name = ?`
	row := d.conn.QueryRow(q, sessionName)
	s, err := scanStatus(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: current status: %w", err)
	}
	return s, nil
}

// AllActiveStatus returns all agent_status rows where ended_at IS NULL.
func (d *DB) AllActiveStatus() ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, opencode_sid, agent_name, model_id, last_seen, ended_at
FROM agent_status
WHERE ended_at IS NULL`
	return d.queryStatuses(q)
}

// AllActiveStatusForRepo returns all active agent_status rows for repo.
func (d *DB) AllActiveStatusForRepo(repo string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, opencode_sid, agent_name, model_id, last_seen, ended_at
FROM agent_status
WHERE ended_at IS NULL AND repo = ?`
	return d.queryStatuses(q, repo)
}

// WaitingCount returns the number of active sessions with state='waiting'.
func (d *DB) WaitingCount() (int, error) {
	var n int
	err := d.conn.QueryRow(
		"SELECT COUNT(*) FROM agent_status WHERE state = 'waiting' AND ended_at IS NULL",
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("db: waiting count: %w", err)
	}
	return n, nil
}

// PendingMessages returns undelivered bus_messages for toSession with the given urgency.
func (d *DB) PendingMessages(toSession string, urgency string) ([]BusMessage, error) {
	const q = `
SELECT id, from_session, to_session, repo, text, urgency, sent_at, delivered_at
FROM bus_messages
WHERE to_session = ? AND urgency = ? AND delivered_at IS NULL`
	rows, err := d.conn.Query(q, toSession, urgency)
	if err != nil {
		return nil, fmt.Errorf("db: pending messages: %w", err)
	}
	defer rows.Close()

	var msgs []BusMessage
	for rows.Next() {
		m, err := scanBusMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan bus message: %w", err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate bus messages: %w", err)
	}
	return msgs, nil
}

// MarkDelivered sets delivered_at on the given bus message.
func (d *DB) MarkDelivered(messageID string) error {
	now := time.Now().UnixMilli()
	_, err := d.conn.Exec(
		"UPDATE bus_messages SET delivered_at = ? WHERE id = ?",
		now, messageID,
	)
	if err != nil {
		return fmt.Errorf("db: mark delivered: %w", err)
	}
	return nil
}

// PurgeBusMessages deletes all undelivered bus_messages rows where
// from_session or to_session matches sessionName. Delivered messages
// (delivered_at IS NOT NULL) are left untouched. It is safe to call when no
// matching rows exist — the operation is a no-op and returns nil.
func (d *DB) PurgeBusMessages(sessionName string) error {
	const q = `
DELETE FROM bus_messages
WHERE delivered_at IS NULL
  AND (from_session = ? OR to_session = ?)`
	if _, err := d.conn.Exec(q, sessionName, sessionName); err != nil {
		return fmt.Errorf("db: purge bus messages: %w", err)
	}
	return nil
}

// WriteBusMessage inserts a new row into bus_messages with delivered_at=NULL.
func (d *DB) WriteBusMessage(msg BusMessage) error {
	var sentAt int64
	if msg.SentAt.IsZero() {
		sentAt = time.Now().UnixMilli()
	} else {
		sentAt = msg.SentAt.UnixMilli()
	}
	const q = `
INSERT INTO bus_messages (id, from_session, to_session, repo, text, urgency, sent_at, delivered_at)
VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`
	_, err := d.conn.Exec(q, msg.ID, msg.FromSession, msg.ToSession, msg.Repo, msg.Text, msg.Urgency, sentAt)
	if err != nil {
		return fmt.Errorf("db: write bus message: %w", err)
	}
	return nil
}

// Prune deletes agent_events older than olderThan, and delivered bus_messages
// older than olderThan. It does NOT delete agent_status rows or undelivered
// bus_messages.
func (d *DB) Prune(olderThan time.Duration) error {
	threshold := time.Now().Add(-olderThan).UnixMilli()

	if _, err := d.conn.Exec(
		"DELETE FROM agent_events WHERE created_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune agent_events: %w", err)
	}

	if _, err := d.conn.Exec(
		"DELETE FROM bus_messages WHERE delivered_at IS NOT NULL AND delivered_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune bus_messages: %w", err)
	}

	return nil
}

// queryStatuses is a helper that runs a SELECT on agent_status and scans rows.
func (d *DB) queryStatuses(q string, args ...any) ([]Status, error) {
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query statuses: %w", err)
	}
	defer rows.Close()

	var statuses []Status
	for rows.Next() {
		s, err := scanStatus(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan status: %w", err)
		}
		statuses = append(statuses, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate statuses: %w", err)
	}
	return statuses, nil
}

// scanner abstracts *sql.Row and *sql.Rows for scanStatus.
type scanner interface {
	Scan(dest ...any) error
}

func scanStatus(s scanner) (*Status, error) {
	var st Status
	var lastSeen int64
	var endedAt sql.NullInt64
	err := s.Scan(
		&st.SessionName, &st.Repo, &st.Worktree, &st.State,
		&st.Title, &st.OpencodeSID, &st.AgentName, &st.ModelID, &lastSeen, &endedAt,
	)
	if err != nil {
		return nil, err
	}
	st.LastSeen = time.UnixMilli(lastSeen)
	if endedAt.Valid {
		t := time.UnixMilli(endedAt.Int64)
		st.EndedAt = &t
	}
	return &st, nil
}

func scanBusMessage(s scanner) (BusMessage, error) {
	var m BusMessage
	var sentAt int64
	var deliveredAt sql.NullInt64
	err := s.Scan(
		&m.ID, &m.FromSession, &m.ToSession, &m.Repo, &m.Text, &m.Urgency,
		&sentAt, &deliveredAt,
	)
	if err != nil {
		return BusMessage{}, err
	}
	m.SentAt = time.UnixMilli(sentAt)
	if deliveredAt.Valid {
		t := time.UnixMilli(deliveredAt.Int64)
		m.DeliveredAt = &t
	}
	return m, nil
}
