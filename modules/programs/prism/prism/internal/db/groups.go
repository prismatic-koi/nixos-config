package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// terminalStates is the set of agent states that indicate a session has stopped
// working and will not make further progress.
//
// Note: this includes "deleted" (cleaned up mid-run) intentionally — a deleted
// session will never complete, so it counts as terminal for GroupCompleted
// purposes.
//
// Note: this deliberately EXCLUDES "interrupted". An interrupted session may
// resume — the user can redirect it via `prism prompt <agent-session>` and the
// agent will continue toward a terminal state (#1495). Treating "interrupted"
// as terminal here would close out a review group the moment the user reaches
// for Esc to redirect a review agent, contaminating the review-complete prompt
// with a false-error verdict before the redirection has a chance to take
// effect. The user's escape hatch for genuinely abandoning an interrupted
// agent is `prism cleanup --yes --session <agent>`, which transitions the
// session to "deleted" — and "deleted" IS terminal here.
//
// This differs from review.go's isTerminalState, which omits "deleted" because
// prism review uses separate cleanup detection logic.
var terminalStates = []string{"finished", "error", "deleted"}

// isTerminalState reports whether state is a terminal agent state.
func isTerminalState(state string) bool {
	for _, s := range terminalStates {
		if state == s {
			return true
		}
	}
	return false
}

// RegisterGroup inserts a new row into session_groups and returns the generated
// group_id. parent_session identifies the session that owns this group (e.g.
// the worker running `prism review`).
//
// Use RegisterGroupWithPR when you have the PR number and round; the
// worker-sidecar recovery watcher (#1709 reopen) reads those columns to
// reconstruct enough context to deliver the review-complete prompt when the
// detached monitor subprocess dies. RegisterGroup remains the back-compat
// entry point for callers that do not have the PR/round (unit-test setup).
func (d *DB) RegisterGroup(parentSession string) (string, error) {
	return d.RegisterGroupWithPR(parentSession, "", 0)
}

// RegisterGroupWithPR is RegisterGroup with PR and round metadata. Both
// arguments are stored as-is; an empty prNumber or zero round is written as
// SQL NULL. The recovery watcher tolerates NULLs by emitting a degraded
// (but still actionable) review-complete header.
func (d *DB) RegisterGroupWithPR(parentSession, prNumber string, round int) (string, error) {
	groupID := uuid.New().String()
	const q = `INSERT INTO session_groups (group_id, parent_session, pr_number, round) VALUES (?, ?, ?, ?)`
	var pr any
	if prNumber != "" {
		pr = prNumber
	}
	var rnd any
	if round > 0 {
		rnd = round
	}
	if _, err := d.conn.Exec(q, groupID, parentSession, pr, rnd); err != nil {
		return "", fmt.Errorf("db: register group: %w", err)
	}
	return groupID, nil
}

// GroupInfo captures the session_groups metadata used by the worker-sidecar
// recovery watcher (#1709 reopen). PRNumber and Round are empty/zero for
// groups registered via the legacy RegisterGroup helper.
type GroupInfo struct {
	GroupID       string
	ParentSession string
	PRNumber      string
	Round         int
	CreatedAt     time.Time
	// DeliveredAt is the time the review-complete prompt was accepted for
	// this group (#2259), or nil when the round has not been delivered.
	DeliveredAt *time.Time
}

// LatestGroupForParent returns the most recently created session_groups row
// whose parent_session matches parentSession, or (nil, nil) when no group
// exists for the parent. Used by the worker-sidecar recovery watcher
// (#1709 reopen) to identify the in-flight review group while
// reviewingInFlight is set; ActiveReviewGroupForParent's "has any
// non-terminal member" criterion is the wrong question here, because the
// stuck-but-complete case the watcher exists to handle has ZERO non-terminal
// members by definition.
func (d *DB) LatestGroupForParent(parentSession string) (*GroupInfo, error) {
	const q = `SELECT group_id, parent_session, pr_number, round, created_at, delivered_at
	            FROM session_groups
	            WHERE parent_session = ?
	            ORDER BY created_at DESC, group_id DESC
	            LIMIT 1`
	row := d.conn.QueryRow(q, parentSession)
	gi, err := scanGroupInfoRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: latest group for parent %q: %w", parentSession, err)
	}
	return gi, nil
}

// scanGroupInfoRow scans one session_groups row (group_id, parent_session,
// pr_number, round, created_at, delivered_at, in that order) into a GroupInfo.
// Shared by every reader of that shape so the NULL handling for pr_number,
// round, and delivered_at stays in one place.
func scanGroupInfoRow(row *sql.Row) (*GroupInfo, error) {
	var gi GroupInfo
	var prNumber sql.NullString
	var round sql.NullInt64
	var deliveredAt sql.NullInt64
	if err := row.Scan(&gi.GroupID, &gi.ParentSession, &prNumber, &round, &gi.CreatedAt, &deliveredAt); err != nil {
		return nil, err
	}
	if prNumber.Valid {
		gi.PRNumber = prNumber.String
	}
	if round.Valid {
		gi.Round = int(round.Int64)
	}
	if deliveredAt.Valid {
		t := time.UnixMilli(deliveredAt.Int64)
		gi.DeliveredAt = &t
	}
	return &gi, nil
}

// GetGroup returns the session_groups metadata for groupID, or (nil, nil) if
// the row does not exist. Used by the worker-sidecar recovery watcher to
// reconstruct the review-complete delivery message when the detached monitor
// subprocess dies.
func (d *DB) GetGroup(groupID string) (*GroupInfo, error) {
	const q = `SELECT group_id, parent_session, pr_number, round, created_at, delivered_at FROM session_groups WHERE group_id = ?`
	row := d.conn.QueryRow(q, groupID)
	gi, err := scanGroupInfoRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: get group %q: %w", groupID, err)
	}
	return gi, nil
}

// GroupsForParent returns every session_groups row whose parent_session
// matches parentSession, ordered by round ascending (ties broken by
// created_at ascending) — the natural review-cycle order (issue #2584).
// Returns an empty, non-nil slice when parentSession has no review groups.
//
// round is a native session_groups column; callers must group and order
// review cycles by it directly rather than parsing a round number out of a
// session name (docs/retro.md section 3).
func (d *DB) GroupsForParent(parentSession string) ([]GroupInfo, error) {
	const q = `SELECT group_id, parent_session, pr_number, round, created_at, delivered_at
	            FROM session_groups
	            WHERE parent_session = ?
	            ORDER BY round ASC, created_at ASC, group_id ASC`
	rows, err := d.conn.Query(q, parentSession)
	if err != nil {
		return nil, fmt.Errorf("db: groups for parent %q: %w", parentSession, err)
	}
	defer rows.Close()

	out := []GroupInfo{}
	for rows.Next() {
		var gi GroupInfo
		var prNumber sql.NullString
		var round sql.NullInt64
		var deliveredAt sql.NullInt64
		if err := rows.Scan(&gi.GroupID, &gi.ParentSession, &prNumber, &round, &gi.CreatedAt, &deliveredAt); err != nil {
			return nil, fmt.Errorf("db: groups for parent %q: scan: %w", parentSession, err)
		}
		if prNumber.Valid {
			gi.PRNumber = prNumber.String
		}
		if round.Valid {
			gi.Round = int(round.Int64)
		}
		if deliveredAt.Valid {
			t := time.UnixMilli(deliveredAt.Int64)
			gi.DeliveredAt = &t
		}
		out = append(out, gi)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: groups for parent %q: iterate: %w", parentSession, err)
	}
	return out, nil
}

// GroupCompleted reports whether the review group has reached a terminal
// state. A group is considered terminal when EITHER:
//
//   - its session_groups.delivered_at is non-NULL (#2259) — the
//     review-complete prompt was successfully accepted by `prism prompt`
//     for this group, either via the happy-path `review.MonitorFunc` or via
//     the recovery primitive `review.DeliverGroupResults`. This is the
//     authoritative end-of-life signal and short-circuits the predicate;
//     any subsequent mutation of agent_status (including the per-process
//     sidecar-restart anti-pattern in cmd/sidecar.go that overwrites a
//     terminal state with `idle`) cannot move the group back to in-progress;
//     OR
//   - every agent_status row with this group_id has reached a terminal
//     state. A row is considered terminal when EITHER its `state` is in
//     terminalStates (finished, error, deleted), OR its `ended_at` is
//     non-NULL (the session row has been closed by `prism cleanup`,
//     `prism reset`, or any other lifecycle path that calls SetEnded /
//     MarkAllEnded).
//
// The `ended_at` arm is what makes the user's escape hatch work. When the
// user runs `prism cleanup --yes --session <interrupted-agent>` to abandon
// an interrupted review agent, the cleanup path:
//
//  1. SIGTERMs the sidecar, which writes state="interrupted".
//  2. Calls SetEnded(session), which sets ended_at but leaves state alone.
//
// Without the `ended_at` arm here, an interrupted-then-cleaned-up agent's
// row would have state="interrupted" forever, which is no longer terminal
// per terminalStates (#1495), and GroupCompleted would never return true.
// The review monitor would then spin until its overall safety timeout.
// Treating `ended_at IS NOT NULL` as terminal closes that gap for any path
// that ends a session row without rewriting state — not just `prism cleanup`,
// but also `prism reset`'s MarkAllEnded.
//
// Returns (true, nil) when delivered_at is set, or when all members are
// terminal (including the case where there are no members yet — caller
// should guard against that if needed).
// Returns (false, nil) when at least one member is still running and the
// group has not yet been delivered.
// Returns (false, err) on a database error.
func (d *DB) GroupCompleted(groupID string) (bool, error) {
	// Short-circuit: a group whose delivered_at has been written is
	// terminal regardless of any subsequent agent_status mutation (#2259).
	var deliveredAt sql.NullInt64
	if err := d.conn.QueryRow(
		`SELECT delivered_at FROM session_groups WHERE group_id = ?`, groupID,
	).Scan(&deliveredAt); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("db: group completed: read delivered_at: %w", err)
		}
		// No session_groups row: caller is operating on an ad-hoc group_id
		// that was never registered (e.g. some legacy tests). Fall through
		// to the agent_status-based predicate, which is the back-compat
		// behaviour.
	} else if deliveredAt.Valid {
		return true, nil
	}

	// Fall back to the agent_status-based predicate for groups that have
	// not yet been delivered.
	placeholders := make([]string, len(terminalStates))
	args := make([]any, 0, 1+len(terminalStates))
	args = append(args, groupID)
	for i, s := range terminalStates {
		placeholders[i] = "?"
		args = append(args, s)
	}
	q := `SELECT COUNT(*) FROM agent_status
          WHERE group_id = ?
            AND state NOT IN (` + strings.Join(placeholders, ",") + `)
            AND ended_at IS NULL`
	var nonTerminalCount int
	if err := d.conn.QueryRow(q, args...).Scan(&nonTerminalCount); err != nil {
		return false, fmt.Errorf("db: group completed: %w", err)
	}
	return nonTerminalCount == 0, nil
}

// SetGroupDeliveredAt records the epoch-ms timestamp at which the review-
// complete prompt was successfully accepted by `prism prompt` for this
// group (#2259). This is the authoritative end-of-life signal read by
// GroupCompleted, ReviewGroupsList, and the in-progress guard in
// review.ActiveReviewGroupForParent.
//
// The UPDATE is conditional on delivered_at IS NULL so the timestamp
// reflects the FIRST successful delivery, not the most recent one. A
// double-delivery race between the monitor and the recovery watcher (see
// monitor_recovery_race_test.go) thus produces a stable, ordering-
// independent timestamp.
//
// Returns nil when the row exists and is updated, when the row exists but
// already has delivered_at set (idempotent), or when no row exists for
// groupID. Returns an error only on a database failure. This is a write-
// path call site — callers (MonitorFunc, DeliverGroupResults) treat its
// failures as warnings rather than aborting the delivery, since the prompt
// has already been accepted by `prism prompt` at the call site.
func (d *DB) SetGroupDeliveredAt(groupID string) error {
	if groupID == "" {
		return fmt.Errorf("db: set group delivered_at: empty group_id")
	}
	const q = `UPDATE session_groups
	           SET delivered_at = ?
	           WHERE group_id = ? AND delivered_at IS NULL`
	if _, err := d.conn.Exec(q, time.Now().UnixMilli(), groupID); err != nil {
		return fmt.Errorf("db: set group delivered_at: %w", err)
	}
	return nil
}

// DeliveredGroupIDsForParent returns the set of group_ids belonging to
// parentSession whose delivered_at is non-NULL (#2259). Used by
// review.ActiveReviewGroupForParent to short-circuit the in-progress guard
// for groups that have already had their review-complete prompt delivered.
func (d *DB) DeliveredGroupIDsForParent(parentSession string) (map[string]struct{}, error) {
	const q = `SELECT group_id FROM session_groups
	           WHERE parent_session = ? AND delivered_at IS NOT NULL`
	rows, err := d.conn.Query(q, parentSession)
	if err != nil {
		return nil, fmt.Errorf("db: delivered group ids for parent: %w", err)
	}
	defer rows.Close()
	result := make(map[string]struct{})
	for rows.Next() {
		var gid string
		if err := rows.Scan(&gid); err != nil {
			return nil, fmt.Errorf("db: delivered group ids for parent: scan: %w", err)
		}
		result[gid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: delivered group ids for parent: iterate: %w", err)
	}
	return result, nil
}

// GroupResults returns the terminal state and last assistant message for every
// member of the group, keyed by session_name. It is intended for use after
// GroupCompleted returns true. Members that are still active are included but
// their State may be non-terminal.
//
// Note: this function is the verdict-aggregation read; it is called once
// (per delivery) before the group's authoritative end-of-life signal
// (session_groups.delivered_at, #2259) is written. The delivered_at column
// is the locking signal for "do not re-aggregate" — callers downstream of
// the recovery watcher rely on GroupCompleted's delivered_at short-circuit
// to avoid calling this function a second time once a group has been
// delivered. Do not consult delivered_at here; do consult it at the
// monitor-loop and in-progress-guard sites that decide whether to call this
// function in the first place.
//
// Rows whose `ended_at` is non-NULL are intentionally EXCLUDED from the
// returned map. This is what makes the user's escape hatch flow correctly
// through `buildMonitorResults`'s missing-session branch (#1495): when the
// user runs `prism cleanup --yes --session <interrupted-agent>` to abandon
// an interrupted review agent, the cleanup path sets ended_at without
// rewriting state. By dropping ended rows here, the cleaned-up agent
// surfaces as "session not found in group" — IsError=true with the
// pre-existing missing-session message — and the remaining agents are
// reported normally. (For any race in which a normally-finished agent has
// ended_at set before the monitor reads results, that agent's verdict is
// also dropped; this is rare and acceptable, since the user must have
// initiated cleanup mid-review.)
//
// LastMessage is populated from the most recent msg_assistant event payload
// for each session. It is empty when no such event has been recorded.
func (d *DB) GroupResults(groupID string) (map[string]GroupMemberResult, error) {
	return d.groupResults(groupID, false)
}

// GroupResultsAll is GroupResults without the `ended_at IS NULL` filter — it
// includes every member of the group, whether or not its agent_status row has
// been closed. Live callers must use GroupResults (see its doc comment for
// why the #1495 cleanup escape hatch depends on excluding ended rows). This
// variant exists for a historical read where exclusion would be wrong: by the
// time `prism retro` runs, every review-agent row for a completed round is
// closed (ended_at IS NOT NULL) — measured on the live DB, 100% of historical
// review agent_status rows are closed. Calling GroupResults on a historical
// group_id would therefore return an empty map and mislabel every agent as
// having no verdict, which is false for most of them (issue #2584 / #2594).
func (d *DB) GroupResultsAll(groupID string) (map[string]GroupMemberResult, error) {
	return d.groupResults(groupID, true)
}

// groupResults is the shared implementation behind GroupResults and
// GroupResultsAll. includeEnded controls whether rows with a non-NULL
// ended_at are included.
func (d *DB) groupResults(groupID string, includeEnded bool) (map[string]GroupMemberResult, error) {
	// Fetch each member's session_name, state, and root_agent_name.
	statusQ := `
SELECT session_name, state, COALESCE(root_agent_name, '')
FROM agent_status
WHERE group_id = ?`
	if !includeEnded {
		// Exclude rows that have been ended (ended_at IS NOT NULL) — see comment above.
		statusQ += ` AND ended_at IS NULL`
	}
	rows, err := d.conn.Query(statusQ, groupID)
	if err != nil {
		return nil, fmt.Errorf("db: group results: query statuses: %w", err)
	}
	defer rows.Close()

	results := make(map[string]GroupMemberResult)
	for rows.Next() {
		var r GroupMemberResult
		if err := rows.Scan(&r.SessionName, &r.State, &r.RootAgent); err != nil {
			return nil, fmt.Errorf("db: group results: scan status: %w", err)
		}
		results[r.SessionName] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: group results: iterate statuses: %w", err)
	}

	if len(results) == 0 {
		return results, nil
	}

	// Batched event fetch: pull every msg_assistant, startup_error, and
	// stall_error event for the entire member set in a single query, ordered
	// so that the most recent row per (session_name, type) comes first. The
	// Go-side reduction below keeps only that first row per pair (#1868 F7 —
	// replaces the previous N+1 shape of 2 QueryRow calls per member).
	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	placeholders := strings.Repeat("?,", len(names))
	placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
	eventQ := `
SELECT session_name, type, payload
FROM agent_events
WHERE session_name IN (` + placeholders + `)
  AND type IN ('msg_assistant', 'startup_error', 'stall_error')
ORDER BY session_name ASC, type ASC, created_at DESC, rowid DESC`
	args := make([]any, 0, len(names))
	for _, n := range names {
		args = append(args, n)
	}
	eventRows, err := d.conn.Query(eventQ, args...)
	if err != nil {
		return nil, fmt.Errorf("db: group results: query events: %w", err)
	}
	defer eventRows.Close()

	// seenLatest tracks which (session_name, type) pairs we have already
	// recorded the most-recent row for. ORDER BY puts the most recent first
	// within each (session_name, type) partition, so we accept the first row
	// we see for each pair and ignore the rest.
	seenLatest := make(map[string]struct{}, 3*len(names))
	for eventRows.Next() {
		var sessName, evtType, payload string
		if err := eventRows.Scan(&sessName, &evtType, &payload); err != nil {
			return nil, fmt.Errorf("db: group results: scan event: %w", err)
		}
		key := sessName + "\x00" + evtType
		if _, seen := seenLatest[key]; seen {
			continue
		}
		seenLatest[key] = struct{}{}

		r, ok := results[sessName]
		if !ok {
			// Defensive: should be impossible because the IN list is built
			// from the results keys.
			continue
		}
		switch evtType {
		case "msg_assistant":
			r.LastMessage = payload
		case "startup_error":
			// Extract the "reason" field from the JSON payload. This is
			// written by writeStartupError in the sidecar when WaitHealthy
			// or CreateSession fails, allowing the review monitor to
			// distinguish a no-start failure from a mid-run crash (#1222).
			if payload != "" {
				var p struct {
					Reason string `json:"reason"`
				}
				if jsonErr := json.Unmarshal([]byte(payload), &p); jsonErr == nil && p.Reason != "" {
					r.StartupError = p.Reason
				} else {
					r.StartupError = payload
				}
			}
		case "stall_error":
			// Extract the "reason" field from the JSON payload. This is
			// written by the sidecar's inactivity watchdog when it fires
			// after one or more inbound frames were received (#2239),
			// allowing the review monitor to report a mid-run stall instead
			// of mislabelling it as a no-start failure.
			if payload != "" {
				var p struct {
					Reason string `json:"reason"`
				}
				if jsonErr := json.Unmarshal([]byte(payload), &p); jsonErr == nil && p.Reason != "" {
					r.StallError = p.Reason
				} else {
					r.StallError = payload
				}
			}
		}
		results[sessName] = r
	}
	if err := eventRows.Err(); err != nil {
		return nil, fmt.Errorf("db: group results: iterate events: %w", err)
	}

	return results, nil
}

// IsGroupMember returns true when sessionName has a non-NULL group_id in
// agent_status (i.e. it belongs to a session group). Returns false for
// pre-migration rows where group_id is NULL.
func (d *DB) IsGroupMember(sessionName string) (bool, error) {
	var groupID sql.NullString
	const q = `SELECT group_id FROM agent_status WHERE session_name = ?`
	if err := d.conn.QueryRow(q, sessionName).Scan(&groupID); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("db: is group member: %w", err)
	}
	return groupID.Valid && groupID.String != "", nil
}

// GroupParentForMember returns the session_groups.parent_session of the group
// that sessionName belongs to.
//
// The answer is strictly DB-backed: it comes from the
// agent_status.group_id → session_groups.parent_session join and from nothing
// else. Unlike ParentSessionFor, this helper has NO name-heuristic fallback.
// The /checkin worker-scope check (issue #2587) is the caller that needs that
// property. A review agent whose session_groups row was deleted must fail the
// scope check, and a name heuristic would admit it on the strength of its
// name alone.
//
// Returns:
//
//   - (parent, true, nil)  — the join produced a row.
//   - ("", false, nil)     — no agent_status row, a NULL group_id, or a
//     group_id whose session_groups row no longer exists.
//   - ("", false, err)     — the query failed.
func (d *DB) GroupParentForMember(sessionName string) (string, bool, error) {
	const q = `
SELECT sg.parent_session
FROM agent_status AS a
JOIN session_groups AS sg ON a.group_id = sg.group_id
WHERE a.session_name = ?`
	var parent string
	if err := d.conn.QueryRow(q, sessionName).Scan(&parent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("db: group parent for member %q: %w", sessionName, err)
	}
	return parent, true, nil
}

// HasReviewGroup returns true when sessionName is the parent_session of at
// least one row in session_groups. This is the DB-backed way to detect whether
// a session has spawned a review group (replacing the "~review" name heuristic).
func (d *DB) HasReviewGroup(parentSession string) (bool, error) {
	var count int
	const q = `SELECT COUNT(*) FROM session_groups WHERE parent_session = ?`
	if err := d.conn.QueryRow(q, parentSession).Scan(&count); err != nil {
		return false, fmt.Errorf("db: has review group: %w", err)
	}
	return count > 0, nil
}

// AllGroupParents returns a map of group_id → parent_session for all rows in
// session_groups. This is the efficient batch counterpart to ParentSessionFor:
// callers that need parent attribution for a large set of sessions can fetch the
// whole map in one query rather than issuing N individual lookups.
//
// The returned map only contains groups registered in session_groups; sessions
// whose group_id is NULL (pre-migration rows) or whose group_id has no matching
// session_groups row are absent from the map (callers should fall back to the
// name heuristic for those).
func (d *DB) AllGroupParents() (map[string]string, error) {
	const q = `SELECT group_id, parent_session FROM session_groups`
	rows, err := d.conn.Query(q)
	if err != nil {
		return nil, fmt.Errorf("db: all group parents: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var groupID, parent string
		if err := rows.Scan(&groupID, &parent); err != nil {
			return nil, fmt.Errorf("db: all group parents: scan: %w", err)
		}
		result[groupID] = parent
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: all group parents: iterate: %w", err)
	}
	return result, nil
}

// AllProfileNames returns a map from instance_id to spawn_inputs.profile_name
// for every spawn_inputs row that has a non-NULL profile_name. Instance IDs
// with no spawn_inputs row, or a NULL profile_name (spawned without
// --profile, or predating the spawn_inputs write path — #2092 / #2093), are
// simply absent from the returned map; callers treat a missing key the same
// as an explicit "no profile recorded". Used by the dashboard to batch-fetch
// profile tiers for all displayed sessions in a single query (issue #2640).
func (d *DB) AllProfileNames() (map[string]string, error) {
	const q = `SELECT instance_id, profile_name FROM spawn_inputs WHERE profile_name IS NOT NULL`
	rows, err := d.conn.Query(q)
	if err != nil {
		return nil, fmt.Errorf("db: all profile names: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var instanceID, profileName string
		if err := rows.Scan(&instanceID, &profileName); err != nil {
			return nil, fmt.Errorf("db: all profile names: scan: %w", err)
		}
		result[instanceID] = profileName
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: all profile names: iterate: %w", err)
	}
	return result, nil
}

// ParentSessionFor returns the authoritative parent session name for the given
// session. It is the single named source of truth for parent attribution,
// used by both the dashboard (via AllGroupParents + StatusToAgentSession) and
// prism sessions list (via AllGroupParents in the renderSessionTable sort key).
//
// Resolution order:
//  1. DB-backed (post-migration): looks up session_groups.parent_session via
//     agent_status.group_id. This is the most reliable source — it records the
//     actual caller at spawn time.
//  2. Name-heuristic fallback (pre-migration rows where group_id IS NULL):
//     strips the "~…" suffix from the session name and returns the prefix as
//     the parent name. e.g. "nixos-config@main~review-1-review-code" → parent
//     is "nixos-config@main".
//
// Returns "" when no parent can be determined (top-level sessions, sessions
// with no group_id and no "~" in the branch component, or DB errors).
func (d *DB) ParentSessionFor(sessionName string) string {
	// Step 1: try DB-backed group_id → session_groups.parent_session.
	const q = `
SELECT sg.parent_session
FROM agent_status AS a
JOIN session_groups AS sg ON a.group_id = sg.group_id
WHERE a.session_name = ?`
	var parent string
	err := d.conn.QueryRow(q, sessionName).Scan(&parent)
	if err == nil && parent != "" {
		return parent
	}
	// err == sql.ErrNoRows or group_id IS NULL: fall through to name heuristic.

	// Step 2: name-heuristic fallback — strip the "~…" suffix from the branch
	// component.  Session names are of the form "repo@branch~suffix" where
	// "~suffix" marks a depth-2 review session.  The parent is "repo@branch".
	if idx := strings.Index(sessionName, "@"); idx >= 0 {
		branch := sessionName[idx+1:] // e.g. "main~review-1-review-code"
		if tildeIdx := strings.Index(branch, "~"); tildeIdx >= 0 {
			return sessionName[:idx] + "@" + branch[:tildeIdx]
		}
	}
	return ""
}

// GroupMembersForParent returns all agent_status rows whose group_id belongs
// to a session_groups row with parent_session = parentSession.
func (d *DB) GroupMembersForParent(parentSession string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE group_id IN (SELECT group_id FROM session_groups WHERE parent_session = ?)`
	return d.queryStatuses(q, parentSession)
}

// GroupMembersForGroup returns all agent_status rows for the given group_id
// (including rows with non-NULL ended_at). Used by the `prism reviews list`
// surface to enumerate the agents in a review round even after they have been
// cleaned up.
func (d *DB) GroupMembersForGroup(groupID string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE group_id = ?
ORDER BY session_name ASC`
	return d.queryStatuses(q, groupID)
}

// ReviewGroupSummary captures one row of the `prism reviews list` ledger:
// a session_groups row plus a roll-up of its members. The PRNumber is
// extracted from the parent session's review session naming convention
// (`<parent>~review-<N>-<agent>`); when the group has no members yet, PRNumber
// is left empty and the caller can fall back to other sources.
type ReviewGroupSummary struct {
	GroupID       string
	ParentSession string
	CreatedAt     time.Time
	// Members lists the per-agent session names belonging to this group,
	// sorted by session name. May be empty if the group has not yet had any
	// members written, or if all members have been cleaned up via
	// `prism cleanup`.
	Members []string
	// AgentStates aligns with Members (same length, same order). Each entry
	// is the agent_status.state at the time of query: "active", "finished",
	// "interrupted", "error", or "deleted".
	AgentStates []string
	// GroupState is a roll-up of AgentStates:
	//   - "in-progress" when at least one member is non-terminal
	//   - "completed"   when all members are terminal (finished/error/deleted)
	//   - "empty"       when the group has no members
	GroupState string
}

// ReviewGroupsList returns every session_groups row, paired with its members
// and a rolled-up GroupState, ordered by created_at DESC (newest first).
// limit ≤ 0 returns all rows.
//
// Used by `prism reviews list` (issue #1500) as a dedicated review-group
// ledger. The list is unfiltered — all groups for all parents are returned;
// the caller filters by parent or repo if desired.
func (d *DB) ReviewGroupsList(limit int) ([]ReviewGroupSummary, error) {
	q := `SELECT group_id, parent_session, created_at, delivered_at FROM session_groups
	            ORDER BY created_at DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.conn.Query(q)
	if err != nil {
		return nil, fmt.Errorf("db: review groups list: %w", err)
	}
	defer rows.Close()

	// deliveredByGroup records which group_ids have a non-NULL delivered_at
	// (#2259). The roll-up below maps these to GroupState="completed"
	// regardless of member state.
	deliveredByGroup := make(map[string]bool)
	var out []ReviewGroupSummary
	for rows.Next() {
		var s ReviewGroupSummary
		var deliveredAt sql.NullInt64
		if scanErr := rows.Scan(&s.GroupID, &s.ParentSession, &s.CreatedAt, &deliveredAt); scanErr != nil {
			return nil, fmt.Errorf("db: review groups list: scan: %w", scanErr)
		}
		if deliveredAt.Valid {
			deliveredByGroup[s.GroupID] = true
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: review groups list: iterate: %w", err)
	}

	// Initialise per-group slices so groups with zero members still produce
	// non-nil empty Members / AgentStates (preserving the prior shape).
	for i := range out {
		out[i].Members = []string{}
		out[i].AgentStates = []string{}
	}

	if len(out) == 0 {
		return out, nil
	}

	// Batched member fetch: pull every agent_status row for the full group
	// set in one query (#1868 F8 — replaces the previous per-group
	// GroupMembersForGroup loop). The IN-list approach is used in lieu of
	// a JOIN so the row-construction logic in queryStatuses is reusable
	// unchanged. Ordered by group_id, session_name so the per-group slices
	// below are stable.
	idxByGroup := make(map[string]int, len(out))
	nonTerminalByGroup := make(map[string]int, len(out))
	groupIDs := make([]any, 0, len(out))
	for i := range out {
		idxByGroup[out[i].GroupID] = i
		groupIDs = append(groupIDs, out[i].GroupID)
	}
	placeholders := strings.Repeat("?,", len(groupIDs))
	placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
	memberQ := `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE group_id IN (` + placeholders + `)
ORDER BY group_id ASC, session_name ASC`
	members, mErr := d.queryStatuses(memberQ, groupIDs...)
	if mErr != nil {
		return nil, fmt.Errorf("db: review groups list: batched members: %w", mErr)
	}
	for _, m := range members {
		if m.GroupID == nil {
			continue // defensive: should not happen given the WHERE clause
		}
		idx, ok := idxByGroup[*m.GroupID]
		if !ok {
			continue
		}
		out[idx].Members = append(out[idx].Members, m.SessionName)
		out[idx].AgentStates = append(out[idx].AgentStates, m.State)
		if !isTerminalState(m.State) && m.EndedAt == nil {
			nonTerminalByGroup[*m.GroupID]++
		}
	}
	for i := range out {
		switch {
		case deliveredByGroup[out[i].GroupID]:
			// delivered_at is the authoritative end-of-life signal (#2259);
			// a group whose review-complete prompt was delivered is
			// classified as completed regardless of member state.
			out[i].GroupState = "completed"
		case len(out[i].Members) == 0:
			out[i].GroupState = "empty"
		case nonTerminalByGroup[out[i].GroupID] == 0:
			out[i].GroupState = "completed"
		default:
			out[i].GroupState = "in-progress"
		}
	}
	return out, nil
}
