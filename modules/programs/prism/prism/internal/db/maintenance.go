package db

import (
	"fmt"
	"time"
)

// Prune deletes agent_events older than olderThan, and delivered or failed
// bus_messages older than olderThan. It does NOT delete agent_status rows or
// undelivered/unfailed bus_messages.
//
// harness_frames is pruned separately via PruneHarnessFrames so callers can
// pick a different (typically shorter) retention window for the raw wire
// archive — the JSONL frames are voluminous on a busy session, while
// agent_events stays at the historical 90-day default.
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
		return fmt.Errorf("db: prune bus_messages (delivered): %w", err)
	}

	if _, err := d.conn.Exec(
		"DELETE FROM bus_messages WHERE failed_at IS NOT NULL AND failed_at < ?", threshold,
	); err != nil {
		return fmt.Errorf("db: prune bus_messages (failed): %w", err)
	}

	return nil
}
