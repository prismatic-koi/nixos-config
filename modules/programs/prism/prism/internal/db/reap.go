package db

// reap.go — the recorded cause for an agent_status row that a lifecycle path
// closed (#2613).
//
// Background. Several prism paths close an agent_status row by stamping
// ended_at, and some of them also force the state to "error" first. Once the
// row is closed, db.GroupResults drops it, and the review report could only
// see two facts: the state string and the closing time. Two very different
// events — "the readiness gate never saw the agent come up" and "a lifecycle
// path force-terminated a running agent" — both land on state "error", so the
// report had to name both and could name neither. A coordinator reading the
// DB was in the same position.
//
// The fix is to make each closing path say why it closed the row. Every path
// writes one `session_reaped` event carrying a SessionReapCause before it
// stamps ended_at. The review classifier reads that event back and reports one
// cause, never a disjunction.
//
// Why an agent_events row and not an agent_status column: agent_status holds
// the CURRENT state of a session, and a schema change there needs a migration
// plus a backfill. agent_events is already the append-only diagnostic trail
// that carries `startup_error` (#1222) and `stall_error` (#2239) — the two
// causes that were already distinguishable. A reap cause is the same kind of
// fact, so it belongs on the same trail and needs no migration.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SessionReapEventType is the agent_events.type value that carries a
// SessionReapCause payload.
const SessionReapEventType = "session_reaped"

// tmuxSessionEndEventType is the event the tmux session-closed hook writes
// (cmd/event.go). It is read here as a fallback cause: the hook stamps
// ended_at without rewriting state, so a row that the sidecar had already
// moved to "error" reads as a reap with no cause of its own.
const tmuxSessionEndEventType = "tmux_session_end"

// SessionReapCause names the lifecycle path that closed an agent_status row.
// Each value maps to exactly one path — a value must never stand for "one of
// these two things happened".
type SessionReapCause string

const (
	// ReapCauseReadinessGate — the review readiness gate timed out waiting
	// for the agent to signal that it was up, and cleaned the half-alive
	// session away (internal/review/readiness.go).
	ReapCauseReadinessGate SessionReapCause = "readiness_gate"
	// ReapCauseSpawnFailure — session.SpawnSession returned an error and the
	// spawn loop cleaned up the partially-created session
	// (internal/review/run.go).
	ReapCauseSpawnFailure SessionReapCause = "spawn_failure"
	// ReapCauseMonitorTimeout — the review monitor's outer safety deadline
	// fired while this member was still non-terminal, so the sweep
	// force-terminated it (internal/review/monitor.go).
	ReapCauseMonitorTimeout SessionReapCause = "monitor_timeout"
	// ReapCauseParentCleanup — a cleanup of the parent worker session
	// cascaded to this review agent (internal/review/lifecycle.go).
	ReapCauseParentCleanup SessionReapCause = "parent_cleanup"
	// ReapCauseCleanupCommand — an operator ran `prism cleanup` / `prism
	// close` against this session directly (cmd/cleanup.go).
	ReapCauseCleanupCommand SessionReapCause = "cleanup_command"
	// ReapCauseAutoRelease — the automatic release closed an already-terminal
	// review agent once the grace period after its round's review-complete
	// prompt had elapsed (internal/review/reap.go, issue #2649).
	//
	// This cause is unlike the other five. They all close a row that was
	// still running; this one closes a row that had already stopped. It is
	// recorded for the same reason they are: after #2649 this is the most
	// common closer of review-agent rows by a wide margin, and a coordinator
	// asking "why is this row closed" must not be told that nothing recorded
	// why.
	ReapCauseAutoRelease SessionReapCause = "auto_release"
)

// Description renders the cause as a single, specific sentence fragment for a
// report. It never names two possibilities.
func (c SessionReapCause) Description() string {
	switch c {
	case ReapCauseReadinessGate:
		return "the review readiness gate closed it after the agent failed to signal that it was up"
	case ReapCauseSpawnFailure:
		return "the spawn of this agent failed and the spawn loop closed the partially-created session"
	case ReapCauseMonitorTimeout:
		return "the review monitor's safety deadline fired while this agent was still running, and the sweep force-terminated it"
	case ReapCauseParentCleanup:
		return "a cleanup of the parent worker session cascaded to this review agent"
	case ReapCauseCleanupCommand:
		return "an operator closed this session with `prism cleanup` or `prism close`"
	case ReapCauseAutoRelease:
		return "the automatic release closed this already-finished review agent after the grace period following its round"
	case "":
		return ""
	default:
		return fmt.Sprintf("a lifecycle path recorded the cause %q", string(c))
	}
}

// SessionEndCause is the diagnostic record read back for one closed session:
// the reap cause a lifecycle path recorded, plus the sidecar-written failure
// events that explain the agent's own behaviour before the row was closed.
//
// The zero value means "nothing was recorded" — Recorded() reports false.
type SessionEndCause struct {
	// Cause is the SessionReapCause from the latest session_reaped event.
	Cause SessionReapCause
	// Detail is the free-text detail recorded with the cause, if any.
	Detail string
	// StartupError is the reason from the latest startup_error event
	// (#1222): the agent never ran.
	StartupError string
	// StallError is the reason from the latest stall_error event (#2239):
	// the agent ran, then went silent.
	StallError string
	// TmuxSessionEnded reports whether a tmux_session_end event exists for
	// the session — the tmux session-closed hook stamps ended_at without
	// rewriting state.
	TmuxSessionEnded bool
}

// Recorded reports whether any explanatory record exists for the session.
func (c SessionEndCause) Recorded() bool {
	return c.Cause != "" || c.StartupError != "" || c.StallError != "" || c.TmuxSessionEnded
}

// sessionReapPayload is the JSON shape of a session_reaped event payload.
type sessionReapPayload struct {
	Cause  string `json:"cause"`
	Detail string `json:"detail,omitempty"`
}

// RecordSessionReap writes a session_reaped event for sessionName. Call it
// immediately BEFORE the path stamps ended_at, so a reader that sees a closed
// row also sees the cause.
//
// detail is optional free text — the readiness-gate timeout message, the
// spawn error, and so on. It is truncated so a very long error cannot bloat
// the event row.
//
// Writing a reap for an unknown session is not an error: WriteEvent records
// the event and skips the last_seen bump. The caller may ignore the returned
// error — every call site is already on a cleanup path where a lost
// diagnostic must not mask the cleanup itself.
func (d *DB) RecordSessionReap(sessionName string, cause SessionReapCause, detail string) error {
	if strings.TrimSpace(sessionName) == "" {
		return fmt.Errorf("db: record session reap: session name is required")
	}
	if cause == "" {
		return fmt.Errorf("db: record session reap: cause is required for %q", sessionName)
	}

	// Best-effort repo/worktree so the event row matches its siblings. Both
	// columns are NOT NULL, so an unknown session writes empty strings.
	repo, worktree := "", ""
	var instanceID *string
	if st, err := d.CurrentStatus(sessionName); err == nil && st != nil {
		repo, worktree = st.Repo, st.Worktree
		instanceID = st.InstanceID
	}

	payload, err := json.Marshal(sessionReapPayload{
		Cause:  string(cause),
		Detail: truncateReapDetail(detail),
	})
	if err != nil {
		return fmt.Errorf("db: record session reap: marshal payload: %w", err)
	}

	return d.WriteEvent(Event{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Repo:        repo,
		Worktree:    worktree,
		InstanceID:  instanceID,
		Type:        SessionReapEventType,
		Payload:     string(payload),
		CreatedAt:   time.Now(),
	})
}

// RecordReapBestEffort is RecordSessionReap for the cleanup paths, which must
// not fail because a diagnostic write failed. It discards the error, tolerates
// a nil receiver and a blank session name, and takes the detail as an optional
// trailing argument so a caller with nothing to add can omit it.
func (d *DB) RecordReapBestEffort(sessionName string, cause SessionReapCause, detail ...string) {
	if d == nil || sessionName == "" || cause == "" {
		return
	}
	text := ""
	for _, s := range detail {
		if strings.TrimSpace(s) != "" {
			text = s
			break
		}
	}
	_ = d.RecordSessionReap(sessionName, cause, text)
}

// maxReapDetailLen bounds the free-text detail stored on a session_reaped
// event. Long enough for a readiness-gate message or a spawn error, short
// enough that a pathological error string cannot bloat the trail.
const maxReapDetailLen = 512

func truncateReapDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if len(detail) <= maxReapDetailLen {
		return detail
	}
	return detail[:maxReapDetailLen] + "…"
}

// SessionEndCauses returns the recorded close cause for each of sessionNames,
// keyed by session name. Sessions with nothing recorded are absent from the
// map, so a caller can use the zero value for "no record".
//
// One batched query covers the whole set, ordered so the most recent row per
// (session_name, type) comes first; the reduction below keeps only that row.
// This mirrors the batched event fetch in GroupResults rather than issuing one
// query per session.
func (d *DB) SessionEndCauses(sessionNames []string) (map[string]SessionEndCause, error) {
	// De-duplicate and drop blanks so the IN list is minimal and stable.
	seen := make(map[string]struct{}, len(sessionNames))
	names := make([]string, 0, len(sessionNames))
	for _, n := range sessionNames {
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	if len(names) == 0 {
		return map[string]SessionEndCause{}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	q := `
SELECT session_name, type, payload
FROM agent_events
WHERE session_name IN (` + placeholders + `)
  AND type IN ('` + SessionReapEventType + `', 'startup_error', 'stall_error', '` + tmuxSessionEndEventType + `')
ORDER BY session_name ASC, type ASC, created_at DESC, rowid DESC`

	args := make([]any, 0, len(names))
	for _, n := range names {
		args = append(args, n)
	}
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: session end causes: query: %w", err)
	}
	defer rows.Close()

	out := make(map[string]SessionEndCause, len(names))
	latest := make(map[string]struct{}, 4*len(names))
	for rows.Next() {
		var sess, evtType, payload string
		if err := rows.Scan(&sess, &evtType, &payload); err != nil {
			return nil, fmt.Errorf("db: session end causes: scan: %w", err)
		}
		key := sess + "\x00" + evtType
		if _, dup := latest[key]; dup {
			continue
		}
		latest[key] = struct{}{}

		c := out[sess]
		switch evtType {
		case SessionReapEventType:
			var p sessionReapPayload
			if json.Unmarshal([]byte(payload), &p) == nil && p.Cause != "" {
				c.Cause = SessionReapCause(p.Cause)
				c.Detail = p.Detail
			}
		case "startup_error":
			c.StartupError = reasonFromPayload(payload)
		case "stall_error":
			c.StallError = reasonFromPayload(payload)
		case tmuxSessionEndEventType:
			c.TmuxSessionEnded = true
		}
		out[sess] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: session end causes: iterate: %w", err)
	}

	// Drop entries that ended up empty (e.g. a session_reaped row with an
	// unparseable payload) so Recorded() and map presence agree.
	for name, c := range out {
		if !c.Recorded() {
			delete(out, name)
		}
	}
	return out, nil
}

// reasonFromPayload extracts the "reason" field from a startup_error /
// stall_error payload, falling back to the raw payload when the JSON does not
// carry one. Mirrors the extraction GroupResults performs for live rows so the
// two read paths report identical text.
func reasonFromPayload(payload string) string {
	if payload == "" {
		return ""
	}
	var p struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err == nil && p.Reason != "" {
		return p.Reason
	}
	return payload
}
