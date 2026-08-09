package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// HarnessFrameDirectionIn marks frames received from the extension.
const HarnessFrameDirectionIn = "in"

// HarnessFrameDirectionOut marks frames sent from the sidecar to the extension.
const HarnessFrameDirectionOut = "out"

// HarnessFrame represents a row in the harness_frames table.
//
// Direction is one of "in" (extension → sidecar) or "out" (sidecar → extension).
// Payload is the raw JSONL bytes of the frame (excluding the trailing newline)
// so callers can write it back out verbatim with one append.
type HarnessFrame struct {
	ID          string
	SessionName string
	// InstanceID links the frame to the spawning sessions row. Nullable so
	// frames captured before instance_id is minted (or for legacy rows) are
	// still recorded; no FK is enforced for the same reason.
	InstanceID *string
	Direction  string
	// Type is the value of the JSON "type" field at the top level of the
	// frame, denormalised so --types filtering does not need JSON_EXTRACT.
	// Empty when the frame omits the field or fails JSON parsing.
	Type      string
	Payload   string
	CreatedAt time.Time
}

// WriteHarnessFrame inserts a single row into harness_frames.
//
// The function is intentionally narrow: callers (sidecar.frame_archive) handle
// derivation of the type field from the raw JSONL bytes, ID generation, and
// timestamping. This keeps the SQL layer dumb and easy to test.
func (d *DB) WriteHarnessFrame(f HarnessFrame) error {
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now()
	}
	// Second redaction control (issue #2589). harness_frames stores the raw
	// wire bytes, so it carries the same credential exposure as
	// agent_events. See redact.go.
	f.Payload = d.redactPayload(f.Payload)
	const q = `
INSERT INTO harness_frames (id, session_name, instance_id, direction, type, payload, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`
	if _, err := d.conn.Exec(q,
		f.ID, f.SessionName, f.InstanceID, f.Direction, f.Type, f.Payload, f.CreatedAt.UnixMilli(),
	); err != nil {
		return fmt.Errorf("db: write harness frame: %w", err)
	}
	return nil
}

// QueryHarnessFrames returns harness frames for sessionName ordered by
// created_at ASC (chronological).
//
// direction filters to one of "in" or "out"; pass "" for both.
// types filters to the comma-separated set of frame types; pass nil for all.
// afterID is a cursor used by --follow: when non-empty, only frames whose
// rowid is strictly greater than the rowid of the named frame are returned.
// (We use rowid rather than created_at so two frames recorded in the same
// millisecond are returned in insertion order.)
func (d *DB) QueryHarnessFrames(sessionName, direction string, types []string, afterID string) ([]HarnessFrame, error) {
	args := []any{sessionName}
	conditions := []string{"session_name = ?"}

	if direction != "" {
		conditions = append(conditions, "direction = ?")
		args = append(args, direction)
	}

	if len(types) > 0 {
		placeholders := make([]string, len(types))
		for i, t := range types {
			placeholders[i] = "?"
			args = append(args, t)
		}
		conditions = append(conditions, "type IN ("+strings.Join(placeholders, ",")+")")
	}

	if afterID != "" {
		var afterRowID int64
		err := d.conn.QueryRow(
			"SELECT rowid FROM harness_frames WHERE id = ?", afterID,
		).Scan(&afterRowID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("QueryHarnessFrames: cursor frame %q not found", afterID)
		}
		if err != nil {
			return nil, fmt.Errorf("db: resolve harness-frame cursor: %w", err)
		}
		conditions = append(conditions, "rowid > ?")
		args = append(args, afterRowID)
	}

	q := `SELECT id, session_name, instance_id, direction, type, payload, created_at
	      FROM harness_frames
	      WHERE ` + strings.Join(conditions, " AND ") +
		` ORDER BY created_at ASC, rowid ASC`

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query harness frames: %w", err)
	}
	defer rows.Close()

	var frames []HarnessFrame
	for rows.Next() {
		var f HarnessFrame
		var createdAtMs int64
		if err := rows.Scan(&f.ID, &f.SessionName, &f.InstanceID, &f.Direction, &f.Type, &f.Payload, &createdAtMs); err != nil {
			return nil, fmt.Errorf("db: scan harness frame: %w", err)
		}
		f.CreatedAt = time.UnixMilli(createdAtMs)
		frames = append(frames, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate harness frames: %w", err)
	}
	return frames, nil
}

// CountHarnessFrames returns the total number of harness frames for the
// session. Used by `prism logs --harness-events` to distinguish "no frames
// recorded — this is a non-PI session" from "PI session that just hasn't
// produced any frames yet" (the latter is a legitimate empty result).
func (d *DB) CountHarnessFrames(sessionName string) (int, error) {
	var n int
	if err := d.conn.QueryRow(
		"SELECT COUNT(*) FROM harness_frames WHERE session_name = ?", sessionName,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: count harness frames: %w", err)
	}
	return n, nil
}

// LastInboundFrameAt returns the created_at of the most recent inbound
// (extension → sidecar) harness frame for sessionName. ok is false when the
// session has no inbound frame on record.
//
// The monitor's group-wide safety-deadline sweep uses this to tell a live
// review agent from a dead-watchdog row (#2729). The sidecar's inactivity
// watchdog resets on inbound frames only (internal/sidecar/events.go), so a
// member with a recent inbound frame has a watchdog that has not yet fired
// and still owns the session; a member whose newest inbound frame is older
// than that watchdog window, yet is still non-terminal, has a dead watchdog
// — the case the monitor sweep is the backstop for. Inbound is therefore the
// correct direction to read: an outbound frame the sidecar sent does not
// reset the watchdog and does not prove the agent is alive.
func (d *DB) LastInboundFrameAt(sessionName string) (time.Time, bool, error) {
	var ms sql.NullInt64
	err := d.conn.QueryRow(
		`SELECT MAX(created_at) FROM harness_frames WHERE session_name = ? AND direction = ?`,
		sessionName, HarnessFrameDirectionIn,
	).Scan(&ms)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("db: last inbound frame for %q: %w", sessionName, err)
	}
	if !ms.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(ms.Int64), true, nil
}

// PruneHarnessFrames deletes frames older than olderThan.
//
// IMPORTANT: this only touches the harness_frames table. agent_events and
// sessions are unaffected — the goal is to retire raw wire-protocol bytes
// (which are voluminous on a busy session) while preserving the structured
// agent_events derived from them. See P5.LOGS retention AC.
func (d *DB) PruneHarnessFrames(olderThan time.Duration) error {
	threshold := time.Now().Add(-olderThan).UnixMilli()
	if _, err := d.conn.Exec(
		"DELETE FROM harness_frames WHERE created_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune harness frames: %w", err)
	}
	return nil
}
