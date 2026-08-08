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

	// LifecycleEventsTailSQL is the #2703 tailer's batch read. It LEFT JOINs
	// sessions and agent_status by instance_id to pick up the closed-set
	// label columns (#2699 section 6: repo, agent_role, isolation_mode,
	// end_state) that a handful of event types need. The join is per-row
	// label enrichment, never an aggregate: the counter value is still
	// exactly one per tailed row, so #2699 section 3 is not in tension with
	// it.
	//
	// spawn_inputs IS now joined (issue #2720), for profile_name only. It
	// was excluded in #2703/PR #2718 because sql_boundary_test.go's
	// unambiguous-name check banned the bare table name spawn_inputs
	// anywhere in this package's source — stricter than the spec it
	// enforced. #2699 section 5 bans two COLUMNS of spawn_inputs
	// (prompt_text, extras), not the table, and both are already caught by
	// the separate forbiddenColumns identifier scan. #2699 section 6 lists
	// profile as an explicitly safe label. #2720 narrowed the boundary test
	// to the column-level ban that the spec actually calls for; see the
	// comment at that edit site (sql_boundary_test.go) for why the other
	// three whole-table bans (harness_frames, bus_messages,
	// pending_replay_deliveries) are unaffected and remain in force — those
	// tables hold nothing but free-text bodies, so a blanket ban is correct
	// for them and wrong for spawn_inputs.
	//
	// The join is safe against the 90-day prune. sessions and agent_events
	// are pruned inside the SAME Prune() transaction (internal/db/
	// maintenance.go), sessions on ended_at and agent_events on created_at,
	// and an event's created_at always precedes its session's ended_at. So
	// whenever a sessions row is old enough to be pruned, every agent_events
	// row for that instance_id is at least as old and is pruned in the same
	// transaction: an agent_events row can never be tailed after its
	// session row is gone. agent_status rows are pruned only once their own
	// ended_at is old AND no live sessions row shares the instance_id — a
	// strictly later condition than the sessions prune — so the same
	// argument covers it. spawn_inputs is declared
	// `ON DELETE CASCADE REFERENCES sessions(instance_id)` (internal/db/
	// db.go) and is deleted, if at all, by the very same
	// `DELETE FROM sessions ...` statement in the same Prune() transaction
	// — there is no separate spawn_inputs DELETE and no separate condition
	// to check, so the sessions argument above applies to it unchanged: a
	// spawn_inputs row cannot vanish while its agent_events row is still
	// ahead of the cursor.
	//
	// None of the projected columns appear in the #2699 section 5 forbidden
	// list: repo, type are on agent_events; agent_role, end_state are on
	// sessions; isolation_mode is on agent_status; profile_name is on
	// spawn_inputs and is not prompt_text or extras.
	LifecycleEventsTailSQL = `SELECT ae.rowid, ae.type, ae.repo, s.agent_role, s.end_state, ast.isolation_mode, si.profile_name ` +
		`FROM agent_events ae ` +
		`LEFT JOIN sessions s ON s.instance_id = ae.instance_id ` +
		`LEFT JOIN agent_status ast ON ast.instance_id = ae.instance_id ` +
		`LEFT JOIN spawn_inputs si ON si.instance_id = ae.instance_id ` +
		`WHERE ae.rowid > ? ORDER BY ae.rowid ASC LIMIT ?`
)

// AllSQL is every statement the exporter issues, in one slice, for the
// boundary test to walk.
var AllSQL = []string{
	AgentEventsMaxRowIDSQL,
	AgentEventsTailSQL,
	LifecycleEventsTailSQL,
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

// lifecycleEvent is the per-row value tailed by the number 2703 lifecycle
// counters. Every field beyond Type is optional: most event types this
// tailer sees carry none of the joined columns, and the dispatcher
// (lifecycle.go) reads only the fields the event type it is handling needs.
type lifecycleEvent struct {
	Type          string
	Repo          string
	AgentRole     string
	EndState      string
	IsolationMode string
	// ProfileName is the resolved label value: spawn_inputs.profile_name, or
	// "default" when that column is NULL (no --profile flag was passed).
	// The NULL->default fold happens in Records below, at scan time, so
	// lifecycle.go's dispatch never has to special-case NULL.
	ProfileName string
}

// lifecycleEventSource is the tailcursor.Source over agent_events used by
// the #2703 lifecycle and outcome counters. It shares MaxID with
// agentEventSource: both tail the SAME table by the SAME rowid space, so a
// second, independent MAX(rowid) query would be redundant and would trip
// the boundary test that permits MAX() only in AgentEventsMaxRowIDSQL, for
// no reason.
type lifecycleEventSource struct {
	conn *sql.DB
}

var _ tailcursor.Source[lifecycleEvent] = lifecycleEventSource{}

func (s lifecycleEventSource) MaxID(ctx context.Context) (int64, error) {
	return agentEventSource{conn: s.conn}.MaxID(ctx)
}

func (s lifecycleEventSource) Records(ctx context.Context, afterID int64, limit int) ([]tailcursor.Record[lifecycleEvent], error) {
	rows, err := s.conn.QueryContext(ctx, LifecycleEventsTailSQL, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("exporter: tail lifecycle events after %d: %w", afterID, err)
	}
	defer rows.Close()

	out := make([]tailcursor.Record[lifecycleEvent], 0, limit)
	for rows.Next() {
		var (
			rec                                             tailcursor.Record[lifecycleEvent]
			repo                                            string
			agentRole, endState, isolationMode, profileName sql.NullString
		)
		if err := rows.Scan(&rec.ID, &rec.Value.Type, &repo, &agentRole, &endState, &isolationMode, &profileName); err != nil {
			return nil, fmt.Errorf("exporter: scan lifecycle event row: %w", err)
		}
		rec.Value.Repo = repo
		rec.Value.AgentRole = agentRole.String
		rec.Value.EndState = endState.String
		rec.Value.IsolationMode = isolationMode.String
		// A spawn with no --profile flag has profile_name NULL. Label it
		// "default", not the empty string (#2720 AC).
		if profileName.Valid {
			rec.Value.ProfileName = profileName.String
		} else {
			rec.Value.ProfileName = "default"
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("exporter: iterate lifecycle event rows: %w", err)
	}
	return out, nil
}
