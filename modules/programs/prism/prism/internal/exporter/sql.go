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
// Two rules govern what may appear below:
//
//  1. The exporter is a "/stats"-class surface. It may SELECT aggregate
//     functions and closed-set grouping columns only. It must never read a
//     raw TEXT body column — spawn_inputs.prompt_text, harness_frames.payload,
//     bus_messages.text, pending_replay_deliveries.text, agent_status.title,
//     issue_ref, spawn_inputs.extras, pending_merges.error, or
//     spawn_outcome.rubric_breakdown.
//
//     agent_events.payload is the one nuanced case. Rule 1 records
//     it as "aggregate numbers out, never emit the string": the message body
//     itself must never leave this surface, but the per-turn numeric fields
//     inside it (token counts and the model's own cost) are aggregate
//     numbers and are allowed out. CostEventsTailSQL therefore reads payload
//     only through JSON_EXTRACT of a fixed allowlist of scalar fields
//     ($.model, $.inputTokens, $.outputTokens, $.cacheReadTokens,
//     $.cacheWriteTokens, $.cost) — never as a bare projected column. The
//     boundary test (sql_boundary_test.go) enforces exactly that shape: it
//     still fails a bare `payload` read and any JSON path outside the
//     allowlist. This mirrors the narrowing of the spawn_inputs table
//     ban to a column-level ban — the mechanical test was stricter than the
//     rule it enforces, and the cost counters need the numbers rule 1
//     already permits. Do NOT widen the allowlist to a free-text field
//     ($.text is the message body) without re-reading rule 1.
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
	// closed set of lifecycle event kinds and is a safe label. payload is
	// NOT read: it holds assistant and user message
	// content.
	AgentEventsTailSQL = `SELECT rowid, type FROM agent_events WHERE rowid > ? ORDER BY rowid ASC LIMIT ?`

	// LifecycleEventsTailSQL is the lifecycle tailer's batch read. It LEFT
	// JOINs sessions and agent_status by instance_id to pick up the
	// closed-set label columns (repo, agent_role, isolation_mode, end_state)
	// that a handful of event types need. The join is per-row label
	// enrichment, never an aggregate: the counter value is still exactly one
	// per tailed row, so rule 2 is not in tension with it.
	//
	// spawn_inputs is joined, for profile_name only. The SQL boundary bans
	// two COLUMNS of spawn_inputs (prompt_text, extras), not the table, and
	// both are already caught by the separate forbiddenColumns identifier
	// scan. profile is an explicitly safe label. The boundary test uses the
	// column-level ban rather than a whole-table one; see the comment at
	// that edit site (sql_boundary_test.go) for why the other three
	// whole-table bans (harness_frames, bus_messages,
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
	// None of the projected columns appear in the SQL boundary's forbidden
	// list: repo, type are on agent_events; agent_role, end_state are on
	// sessions; isolation_mode is on agent_status; profile_name is on
	// spawn_inputs and is not prompt_text or extras.
	LifecycleEventsTailSQL = `SELECT ae.rowid, ae.type, ae.repo, s.agent_role, s.end_state, ast.isolation_mode, si.profile_name ` +
		`FROM agent_events ae ` +
		`LEFT JOIN sessions s ON s.instance_id = ae.instance_id ` +
		`LEFT JOIN agent_status ast ON ast.instance_id = ae.instance_id ` +
		`LEFT JOIN spawn_inputs si ON si.instance_id = ae.instance_id ` +
		`WHERE ae.rowid > ? ORDER BY ae.rowid ASC LIMIT ?`

	// CostEventsTailSQL is the cost tailer's batch read: the per-turn cost
	// and token counters, and the account and profile dimensions they carry.
	//
	// It tails agent_events by rowid exactly as the other two tailers do, so
	// the same prune-safety argument holds unchanged: a counter value comes
	// from one tailed row, never from an aggregate over a table the 90-day
	// prune shrinks. The `AND ae.type = 'msg_assistant'`
	// filter narrows the batch to the only rows that carry token usage; it
	// does not break the cursor, which still advances by ascending rowid over
	// the whole-table id space (MaxID reads the whole table). Skipped rowids
	// are simply never revisited, which is correct — they carry no cost.
	//
	// payload is read ONLY through JSON_EXTRACT of the fixed numeric/model
	// allowlist (see rule 1 above): the message body string never leaves the
	// query, only the aggregate numbers inside it. account_name is a plain
	// column stamped at write time —
	// it is the account NAME, mapped to the server-assigned org ID at emit
	// time (cost.go), never used as an identity label directly.
	//
	// profile_name is read from agent_events, a plain column stamped at
	// write time — NOT through a spawn_inputs join. A spawn_inputs join is
	// wrong: it misses for every session with no spawn_inputs row (a
	// coordinator is never spawned), so all coordinator spend would fold to
	// "default". Reading the write-time column attributes each row to its
	// real tier. See db/profile_name.go.
	//
	// Counter-continuity: these are TAIL-CURSOR counters. A pre-migration
	// row has profile_name NULL and folds to the explicit "unknown"
	// placeholder at scan time (Records below), never an empty label.
	// Historical spend on those rows is NOT backfilled — the tier was never
	// recorded, so any backfill would be a guess (the retroactive-
	// attribution trap).
	//
	// None of the projected columns appear in the SQL boundary's forbidden
	// list: rowid is the cursor; $.model is a closed-set label; the four token
	// fields and $.cost are aggregate numbers; account_name and profile_name
	// are closed-set operator dimensions, both on agent_events. The
	// spawn_inputs join is gone entirely from this statement.
	CostEventsTailSQL = `SELECT ae.rowid, ` +
		`COALESCE(JSON_EXTRACT(ae.payload, '$.model'), ''), ` +
		`COALESCE(JSON_EXTRACT(ae.payload, '$.inputTokens'), 0), ` +
		`COALESCE(JSON_EXTRACT(ae.payload, '$.outputTokens'), 0), ` +
		`COALESCE(JSON_EXTRACT(ae.payload, '$.cacheReadTokens'), 0), ` +
		`COALESCE(JSON_EXTRACT(ae.payload, '$.cacheWriteTokens'), 0), ` +
		`COALESCE(JSON_EXTRACT(ae.payload, '$.cost'), 0.0), ` +
		`ae.account_name, ae.profile_name ` +
		`FROM agent_events ae ` +
		`WHERE ae.rowid > ? AND ae.type = 'msg_assistant' ` +
		`ORDER BY ae.rowid ASC LIMIT ?`
)

// The four state-gauge statements. Unlike the tail statements above,
// these are recomputed on every scrape (a gauge carries no monotonicity
// contract, so a plain SELECT is safe here) and each is a bare
// projection with no aggregate function — the counting happens in Go, in
// gauges.go — so none of these trips the full-table-aggregate ban that rule
// 2 above enforces for counters.
//
// BusMessagesPendingSQL is the one that collides with an existing whole-
// table ban: sql_boundary_test.go's unambiguous-name check banned the bare
// table name bus_messages anywhere in this package's source, which is
// stricter than the SQL boundary actually requires. The boundary bans the
// COLUMN bus_messages.text ("free-form inter-session messages"), not the
// table, and that column is already caught by the separate forbiddenColumns
// identifier scan. This statement reads bus_messages.repo only. The
// boundary test is narrowed to the column-level ban, exactly as the
// spawn_inputs and agent_events.payload whole-table bans are narrowed; see
// the comment at that edit
// site (sql_boundary_test.go) for why harness_frames and
// pending_replay_deliveries remain whole-table bans — every column in those
// two holds free-text bodies, so a blanket ban is correct for them and wrong
// for bus_messages.
const (
	// SessionsActiveSQL backs prism_sessions_active{repo,agent_role,state}.
	// ended_at IS NULL selects the live rows; agent_status is never pruned
	// while a row is live (internal/db/maintenance.go), so this is safe
	// against the 90-day prune by construction, independent of the tail-
	// cursor argument that protects the counters above.
	SessionsActiveSQL = `SELECT repo, root_agent_name, state FROM agent_status WHERE ended_at IS NULL`

	// MergeQueueDepthSQL backs prism_merge_queue_depth{repo}: the rows
	// currently enqueued and being watched.
	MergeQueueDepthSQL = `SELECT repo FROM pending_merges WHERE status = 'watching'`

	// MergesByStatusSQL backs prism_merges_by_status{repo,status}: every
	// pending_merges row, watching or terminal.
	MergesByStatusSQL = `SELECT repo, status FROM pending_merges`

	// BusMessagesPendingSQL backs prism_bus_messages_pending{repo}. It reads
	// bus_messages.repo only — never .text, which the SQL boundary bans (see
	// the narrowing note above).
	BusMessagesPendingSQL = `SELECT repo FROM bus_messages WHERE delivered_at IS NULL`

	// SidecarLivenessSQL backs the prism_sidecars_live and
	// prism_sidecars_stale gauges. It reads every session whose sidecar has
	// not ended, plus the one column (last_seen) needed to classify it as
	// live or stale against SidecarStaleThreshold (see gauges.go). state is
	// read too, so the classifier can exclude states that are quiet by
	// design (idle, waiting, escalated) rather than flagging them as dead.
	// Like SessionsActiveSQL, ended_at IS
	// NULL means agent_status is never pruned while the row is live, so
	// this is safe against the 90-day prune by construction. last_seen and
	// state are both closed-set/heartbeat columns, not body columns, so
	// neither is in the SQL boundary's forbidden list.
	SidecarLivenessSQL = `SELECT repo, last_seen, state FROM agent_status WHERE ended_at IS NULL`
)

// AllSQL is every statement the exporter issues, in one slice, for the
// boundary test to walk.
var AllSQL = []string{
	AgentEventsMaxRowIDSQL,
	AgentEventsTailSQL,
	LifecycleEventsTailSQL,
	CostEventsTailSQL,
	SessionsActiveSQL,
	MergeQueueDepthSQL,
	MergesByStatusSQL,
	BusMessagesPendingSQL,
	SidecarLivenessSQL,
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

// lifecycleEvent is the per-row value tailed by the lifecycle counters. Every field beyond Type is optional: most event types this
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
// the lifecycle and outcome counters. It shares MaxID with
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
	// NOTE: costEvent and costEventSource are declared below; they tail the
	// same table for the cost and token counters. See cost.go for the
	// accumulator that consumes them.

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
		// "default", not the empty string.
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

// costEvent is the per-row value tailed by the cost and token counters.
// It carries the numbers extracted from a msg_assistant payload (model, the
// four token kinds, and the model-reported cost), plus the two dimensions
// the counters attribute along: the account NAME recorded on the row and the
// spawn's profile.
type costEvent struct {
	// Model is the full "provider/modelID" string from payload $.model. An
	// empty value means the payload carried no model; apply skips such rows,
	// matching prism stats' collectModelMetrics, which does the same.
	Model string

	InputTokens  float64
	OutputTokens float64
	CacheRead    float64
	CacheWrite   float64

	// EventCost is payload $.cost — the cost the agent reported directly. It
	// is the fallback pricing.Cost uses for a model absent from the pricing
	// table (e.g. openrouter/*), so the exporter and prism stats agree.
	EventCost float64

	// AccountName is agent_events.account_name verbatim: the account NAME
	// active when the row was written, or "" when the column is SQL NULL (a
	// row that predates the column). It is mapped to the account org ID at
	// emit time (cost.go); "" and any name with no usage snapshot both resolve
	// to account_org_id="unknown".
	AccountName string

	// ProfileName is the resolved profile label: agent_events.profile_name
	// (stamped at write time), or "unknown" when that column is NULL — a row
	// that never recorded the tier. The fold happens at scan time so the
	// accumulator never special-cases NULL.
	ProfileName string
}

// costEventSource is the tailcursor.Source over agent_events used by the
// cost and token counters. Like lifecycleEventSource it shares MaxID
// with agentEventSource: all three tail the SAME table by the SAME rowid
// space, and a second MAX(rowid) query would be redundant and would trip the
// boundary test that permits MAX() only in AgentEventsMaxRowIDSQL.
type costEventSource struct {
	conn *sql.DB
}

var _ tailcursor.Source[costEvent] = costEventSource{}

func (s costEventSource) MaxID(ctx context.Context) (int64, error) {
	return agentEventSource{conn: s.conn}.MaxID(ctx)
}

func (s costEventSource) Records(ctx context.Context, afterID int64, limit int) ([]tailcursor.Record[costEvent], error) {
	rows, err := s.conn.QueryContext(ctx, CostEventsTailSQL, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("exporter: tail cost events after %d: %w", afterID, err)
	}
	defer rows.Close()

	out := make([]tailcursor.Record[costEvent], 0, limit)
	for rows.Next() {
		var (
			rec                                  tailcursor.Record[costEvent]
			model                                string
			input, output, cacheRead, cacheWrite int64
			cost                                 float64
			accountName, profileName             sql.NullString
		)
		if err := rows.Scan(&rec.ID, &model, &input, &output, &cacheRead, &cacheWrite, &cost, &accountName, &profileName); err != nil {
			return nil, fmt.Errorf("exporter: scan cost event row: %w", err)
		}
		rec.Value = costEvent{
			Model:        model,
			InputTokens:  float64(input),
			OutputTokens: float64(output),
			CacheRead:    float64(cacheRead),
			CacheWrite:   float64(cacheWrite),
			EventCost:    cost,
			// NULL account_name -> "", which the org-ID resolver folds to
			// "unknown".
			AccountName: accountName.String,
		}
		// A row with profile_name NULL never recorded the tier; fold it to the
		// explicit "unknown" placeholder, never the empty string (matches the
		// repo fold). New rows always carry a resolved tier, so "unknown" here
		// means "written before the tier was recorded", not "coordinator".
		if profileName.Valid {
			rec.Value.ProfileName = profileName.String
		} else {
			rec.Value.ProfileName = unknownProfile
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("exporter: iterate cost event rows: %w", err)
	}
	return out, nil
}
