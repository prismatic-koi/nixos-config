package exporter

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/prismatic-koi/prism/internal/tailcursor"
)

// Every SQL statement the exporter issues is declared here, as an exported
// constant, so a test can assert on the whole set at once. See
// sql_boundary_test.go.
//
// Two rules govern what may appear below (#2699 section 5 and section 3):
//
//  1. The exporter is a "/stats"-class surface. It may SELECT aggregate
//     functions and closed-set grouping columns only. It must never read a
//     raw TEXT body column — agent_events.payload, spawn_inputs.prompt_text,
//     harness_frames.payload, bus_messages.text,
//     pending_replay_deliveries.text, agent_status.title, issue_ref,
//     spawn_inputs.extras, pending_merges.error, or
//     spawn_outcome.rubric_breakdown.
//
//  2. No counter value may come from a full-table aggregate. COUNT(*) over
//     agent_events would decrease at the 90-day prune horizon and Prometheus
//     would read that as a counter reset. Counters are produced by tailing
//     only.
//
// AgentEventsMaxRowIDSQL does use MAX(), and that is not a breach of rule 2:
// its result initialises and clamps a CURSOR, never a counter value. A
// cursor is allowed to move in both directions; a counter is not.
const (
	// AgentEventsMaxRowIDSQL reads the current head of agent_events.
	//
	// rowid is SQLite's implicit monotonic insertion key. prism already
	// tails agent_events by it — see db.QuerySessionEventsSinceRowID and
	// db.MaxSessionEventRowID. agent_events.id is a TEXT uuid and is not
	// ordered, so it cannot serve as the cursor.
	AgentEventsMaxRowIDSQL = `SELECT COALESCE(MAX(rowid), 0) FROM agent_events`

	// AgentEventsTailSQL reads the next batch of events after the cursor.
	//
	// The projection is (rowid, type) and nothing else. type is one of the
	// closed set of lifecycle event kinds and is a safe label (#2699
	// section 6). payload is NOT read: it holds assistant and user message
	// content.
	AgentEventsTailSQL = `SELECT rowid, type FROM agent_events WHERE rowid > ? ORDER BY rowid ASC LIMIT ?`
)

// AllSQL is every statement the exporter issues, in one slice, for the
// boundary test to walk.
var AllSQL = []string{
	AgentEventsMaxRowIDSQL,
	AgentEventsTailSQL,
}

// agentEventSource is the tailcursor.Source over agent_events. The value
// type is the event type string — the only column beyond the cursor that
// this metric needs.
type agentEventSource struct {
	conn *sql.DB
}

var _ tailcursor.Source[string] = agentEventSource{}

func (s agentEventSource) MaxID(ctx context.Context) (int64, error) {
	var maxID int64
	if err := s.conn.QueryRowContext(ctx, AgentEventsMaxRowIDSQL).Scan(&maxID); err != nil {
		return 0, fmt.Errorf("exporter: read agent_events head: %w", err)
	}
	return maxID, nil
}

func (s agentEventSource) Records(ctx context.Context, afterID int64, limit int) ([]tailcursor.Record[string], error) {
	rows, err := s.conn.QueryContext(ctx, AgentEventsTailSQL, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("exporter: tail agent_events after %d: %w", afterID, err)
	}
	defer rows.Close()

	out := make([]tailcursor.Record[string], 0, limit)
	for rows.Next() {
		var rec tailcursor.Record[string]
		if err := rows.Scan(&rec.ID, &rec.Value); err != nil {
			return nil, fmt.Errorf("exporter: scan agent_events row: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("exporter: iterate agent_events rows: %w", err)
	}
	return out, nil
}
