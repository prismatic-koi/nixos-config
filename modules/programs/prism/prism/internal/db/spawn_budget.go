package db

import (
	"context"
	"fmt"
	"time"
)

// countRoleEventsSinceSQL counts the agent_events rows of one type whose
// session has one agent_role, written at or after a time in Unix ms.
//
// The role comes from the same sessions join that the exporter uses to
// label prism_spawns_total (exporter.LifecycleEventsTailSQL), so for
// type session.spawn_intent and role worker this count and the exporter
// counter agree on one host.
//
// CROSS JOIN makes sessions the outer loop. SQLite then probes the
// covering index idx_events_instance(instance_id, type, created_at) once
// per session of the role. sessions holds 90 days of spawns at most, and
// agent_events holds every event of the same 90 days, so this order reads
// far fewer rows than a scan of the created_at range.
const countRoleEventsSinceSQL = `SELECT COUNT(*) FROM sessions s ` +
	`CROSS JOIN agent_events ae ON ae.instance_id = s.instance_id ` +
	`WHERE s.agent_role = ? AND ae.type = ? AND ae.created_at >= ?`

// CountRoleEventsSince returns the number of agent_events rows of
// eventType, written at or after since, whose session has agentRole. The
// spawn budget (internal/spawnbudget) uses it to count the worker spawns of
// this host in the last 7 days.
func (d *DB) CountRoleEventsSince(ctx context.Context, eventType, agentRole string, since time.Time) (int, error) {
	var n int
	if err := d.conn.QueryRowContext(ctx, countRoleEventsSinceSQL, agentRole, eventType, since.UnixMilli()).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: count %s events for role %s: %w", eventType, agentRole, err)
	}
	return n, nil
}
