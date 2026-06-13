package db

import (
	"fmt"
	"time"
)

// Prune deletes rows older than olderThan from the prism database. All
// deletions execute inside a single transaction; if any individual DELETE
// fails the transaction is rolled back and the database is left in its
// pre-Prune state.
//
// Tables that Prune touches:
//
//   - agent_events       — rows with created_at < threshold.
//   - bus_messages       — rows that have been delivered (delivered_at
//     IS NOT NULL AND delivered_at < threshold) or
//     permanently failed (failed_at IS NOT NULL AND
//     failed_at < threshold). Undelivered, unfailed
//     messages are never pruned.
//   - sessions           — rows with ended_at IS NOT NULL AND
//     ended_at < threshold. Live sessions (ended_at
//     IS NULL) are never pruned.
//   - spawn_outcome      — pruned indirectly via ON DELETE CASCADE from
//     sessions(instance_id). Prune does not issue
//     an explicit DELETE against this table.
//   - spawn_inputs       — pruned indirectly via ON DELETE CASCADE from
//     sessions(instance_id). Prune does not issue
//     an explicit DELETE against this table.
//   - agent_status       — rows with ended_at IS NOT NULL AND
//     ended_at < threshold AND no live counterpart
//     in sessions (i.e. no sessions row sharing the
//     same instance_id has ended_at IS NULL). The
//     live-counterpart guard avoids deleting the
//     status row for a session that was restarted
//     under the same instance_id.
//   - session_groups     — rows whose members are all gone, i.e. no
//     surviving sessions or agent_status row still
//     references the group_id. This runs last so
//     that the preceding DELETEs have already
//     removed every referencing row that they can.
//
// Tables that Prune deliberately does NOT touch:
//
//   - harness_frames     — pruned separately via PruneHarnessFrames so
//     callers can pick a different (typically
//     shorter) retention window for the raw wire
//     archive. The JSONL frames are voluminous on
//     a busy session, while agent_events stays at
//     the historical 90-day default.
//   - pending_merges     — operational ledger for the local serial merge
//     queue. Rows are short-lived (queued → merged
//     or failed within a single PR cycle) and the
//     table is not expected to accrete.
//   - schema_version     — a single-row table tracking the live schema
//     version. Pruning it would break startup.
//
// Atomicity: all DELETEs run inside one transaction with PRAGMA
// foreign_keys = ON (set at connection time in Open). The cascade from
// sessions to spawn_outcome / spawn_inputs and the SET NULL behaviour
// on agent_status.group_id / sessions.group_id all participate in the
// same transaction; a failure on any single DELETE rolls the entire
// Prune back.
func (d *DB) Prune(olderThan time.Duration) error {
	threshold := time.Now().Add(-olderThan).UnixMilli()

	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("db: prune: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(
		"DELETE FROM agent_events WHERE created_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune agent_events: %w", err)
	}

	if _, err := tx.Exec(
		"DELETE FROM bus_messages WHERE delivered_at IS NOT NULL AND delivered_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune bus_messages (delivered): %w", err)
	}

	if _, err := tx.Exec(
		"DELETE FROM bus_messages WHERE failed_at IS NOT NULL AND failed_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune bus_messages (failed): %w", err)
	}

	// sessions: ON DELETE CASCADE removes the matching spawn_outcome and
	// spawn_inputs rows; ON DELETE SET NULL clears group_id on any
	// remaining agent_status rows that still reference a now-deleted
	// session's group_id.
	if _, err := tx.Exec(
		"DELETE FROM sessions WHERE ended_at IS NOT NULL AND ended_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune sessions: %w", err)
	}

	// agent_status: only delete rows that are themselves ended AND have no
	// live counterpart in sessions (a sessions row sharing the same
	// instance_id with ended_at IS NULL). The NOT EXISTS guard protects
	// against deleting the status row of a session that was restarted
	// under the same instance_id between threshold and now.
	if _, err := tx.Exec(`
DELETE FROM agent_status
WHERE ended_at IS NOT NULL
  AND ended_at < ?
  AND NOT EXISTS (
    SELECT 1 FROM sessions
    WHERE sessions.instance_id = agent_status.instance_id
      AND sessions.ended_at IS NULL
  )`, threshold,
	); err != nil {
		return fmt.Errorf("db: prune agent_status: %w", err)
	}

	// session_groups: delete any group with no surviving members. A
	// group has members when at least one sessions row OR one
	// agent_status row still references its group_id. After the
	// preceding sessions and agent_status DELETEs above, any group
	// whose members were all pruned is now orphaned and safe to remove.
	if _, err := tx.Exec(`
DELETE FROM session_groups
WHERE group_id NOT IN (
    SELECT group_id FROM sessions      WHERE group_id IS NOT NULL
    UNION
    SELECT group_id FROM agent_status  WHERE group_id IS NOT NULL
)`,
	); err != nil {
		return fmt.Errorf("db: prune session_groups: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: prune: commit: %w", err)
	}
	return nil
}
