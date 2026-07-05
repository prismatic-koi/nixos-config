package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/proglog"

	sqlitelib "modernc.org/sqlite"
)

// currentStateOf looks up the current agent state for sessionName. Returns
// ("", nil) when no row exists (fresh insert path — caller should skip
// transition validation).
func (d *DB) currentStateOf(sessionName string) (agent.AgentState, error) {
	var state string
	err := d.conn.QueryRow(
		"SELECT state FROM agent_status WHERE session_name = ?", sessionName,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: read current state: %w", err)
	}
	return agent.AgentState(state), nil
}

// checkTransition reads the current state for sessionName and validates that
// transitioning to toState is permitted. When the transition is invalid it
// logs a warning to stderr (including session name, from→to pair, and caller
// context) and returns — it does not return an error, so callers are never
// blocked by an invalid transition.
//
// Same-state "transitions" (fromState == toState) are always silently skipped:
// they represent metadata-only upserts (title, harness_session_id, model_id,
// last_seen) where the state value does not actually change, so there is
// nothing to validate.
//
// Callers pass a short context string (e.g. "UpsertStatus", "pane-died") that
// is included in the log line to help locate the call site.
func (d *DB) checkTransition(sessionName string, toState agent.AgentState, callerCtx string) {
	fromState, err := d.currentStateOf(sessionName)
	if err != nil {
		// Non-fatal: if we can't read the current state we skip validation.
		proglog.Errorf("[prism] %s: could not read current state for %q: %v\n",
			callerCtx, sessionName, err)
		return
	}
	if fromState == "" {
		// No prior row — fresh insert; no transition to validate.
		return
	}
	if fromState == toState {
		// Same-state update — metadata-only refresh, nothing to validate.
		return
	}
	if err := agent.Transition(fromState, toState); err != nil {
		proglog.Errorf("[prism] %s: invalid transition for session %q: %v\n",
			callerCtx, sessionName, err)
	}
}

// UpsertStatus inserts or updates the agent_status row for sessionName.
// repo and worktree are always overwritten on conflict. title, harnessSessionID,
// agentName, and modelID are updated only when non-nil (COALESCE).
func (d *DB) UpsertStatus(sessionName, repo, worktree, state string, title *string, harnessSessionID *string) error {
	return d.UpsertStatusWithAgent(sessionName, repo, worktree, state, title, harnessSessionID, nil, nil)
}

// UpsertStatusWithAgent is like UpsertStatus but also accepts agentName and
// modelID, which are written to agent_status.agent_name and agent_status.model_id
// using COALESCE (only overwriting when non-nil). root_agent_name and root_model_id
// are NOT touched by this method — use UpsertStatusWithRootAgent for session creation.
func (d *DB) UpsertStatusWithAgent(sessionName, repo, worktree, state string, title *string, harnessSessionID *string, agentName *string, modelID *string) error {
	d.checkTransition(sessionName, agent.AgentState(state), "UpsertStatusWithAgent")
	now := time.Now().UnixMilli()
	const q = `
INSERT INTO agent_status (session_name, repo, worktree, state, title, agent_name, model_id, last_seen, harness, harness_session_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pi', ?)
ON CONFLICT(session_name) DO UPDATE SET
  state              = excluded.state,
  repo               = excluded.repo,
  worktree           = excluded.worktree,
  title              = COALESCE(excluded.title, title),
  agent_name         = COALESCE(excluded.agent_name, agent_name),
  model_id           = COALESCE(excluded.model_id, model_id),
  last_seen          = excluded.last_seen,
  harness            = COALESCE(harness, excluded.harness),
  harness_session_id = COALESCE(excluded.harness_session_id, harness_session_id)`
	_, err := d.conn.Exec(q, sessionName, repo, worktree, state, title, agentName, modelID, now, harnessSessionID)
	if err != nil {
		return fmt.Errorf("db: upsert status: %w", err)
	}
	return nil
}

// UpdateRootModelID unconditionally sets root_model_id for sessionName to the
// given model value. Unlike UpsertStatusWithRootAgent (which falls back to the
// existing value when the incoming value is nil), this method always overwrites,
// allowing the current session's model to replace a stale value from a prior
// session.
//
// It is a no-op when no row exists for sessionName (returns nil).
// Called by the sidecar when a completed assistant message from the root agent
// reveals the current model, so that coordinator notifications always reflect
// the live model configuration.
func (d *DB) UpdateRootModelID(sessionName, modelID string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET root_model_id = ? WHERE session_name = ?",
		modelID, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: update root_model_id: %w", err)
	}
	return nil
}

// UpsertStatusSeedRootAgentName is like UpsertStatus but also writes
// rootAgentName to root_agent_name when it is non-empty. On conflict (update),
// root_agent_name is written via COALESCE — the existing value is preserved if
// the incoming rootAgentName is empty.
//
// This is the spawn-time seeding path: called when we know the agent role at
// session creation time (before the sidecar's first upsertState() call fires),
// so that the DB row has a non-NULL root_agent_name from the first moment.
// The sidecar will later write the same value idempotently via
// UpsertStatusWithRootAgent (COALESCE preserves the already-set value).
//
// Invariant (isolation_mode NULL window): when isolationMode is non-empty, it
// is written atomically with the INSERT/UPDATE so the row is never observable
// with isolation_mode IS NULL after this call returns. This prevents
// ActiveSessionCountForMode from undercounting when a concurrent spawn races
// the per-mode cap check between the seed and a separate SetIsolationMode call.
// Callers that do not know the mode (e.g. tmux-session-start hook, prism
// switch) pass an empty string, which leaves the existing value unchanged.
func (d *DB) UpsertStatusSeedRootAgentName(sessionName, repo, worktree, state string, title *string, harnessSessionID *string, rootAgentName string, harnessName string, isolationMode string) error {
	d.checkTransition(sessionName, agent.AgentState(state), "UpsertStatusSeedRootAgentName")
	now := time.Now().UnixMilli()
	// When rootAgentName is empty, fall back to leaving root_agent_name as-is
	// (COALESCE with NULL excluded value preserves existing). When non-empty,
	// write it, but still use COALESCE so a later sidecar write of the same
	// value doesn't produce a spurious update.
	var rootAgentNamePtr *string
	if rootAgentName != "" {
		rootAgentNamePtr = &rootAgentName
	}
	// Resolve the harness name to write for a fresh INSERT. Default to
	// "pi" when empty so new rows always have a non-NULL harness column.
	// The UPDATE path is handled separately below.
	insertHarness := harnessName
	if insertHarness == "" {
		insertHarness = "pi"
	}
	// For the ON CONFLICT UPDATE path, pass harnessName as a pointer so that
	// the SQL can distinguish "empty (no override)" from "explicit value".
	// When harnessNamePtr is NULL, CASE preserves the existing DB value; when
	// non-NULL, it overwrites — allowing an explicit harness (e.g. "pi") to
	// replace a stale value (e.g. "pi") left in an ended DB row.
	var harnessNamePtr *string
	if harnessName != "" {
		harnessNamePtr = &harnessName
	}
	// isolationMode follows the same CASE pattern as harnessName: when the
	// caller supplies a non-empty value it is written atomically; when empty
	// the existing DB value is preserved (NULL on a fresh INSERT, existing mode
	// on UPDATE). This eliminates the isolation_mode NULL window described in
	// issue #1866: the row is born with the mode set and ActiveSessionCountForMode
	// will never undercount a session that is still being set up.
	var isolationModePtr *string
	if isolationMode != "" {
		isolationModePtr = &isolationMode
	}
	// For a fresh INSERT, use the provided isolation mode (may be NULL — callers
	// that don't know the mode leave it unset, which is acceptable for non-spawn
	// paths like the tmux-session-start hook).
	insertIsolationMode := isolationModePtr
	const q = `
INSERT INTO agent_status (session_name, repo, worktree, state, title, root_agent_name, last_seen, harness, harness_session_id, isolation_mode)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_name) DO UPDATE SET
  state              = excluded.state,
  repo               = excluded.repo,
  worktree           = excluded.worktree,
  title              = COALESCE(excluded.title, title),
  root_agent_name    = COALESCE(excluded.root_agent_name, root_agent_name),
  last_seen          = excluded.last_seen,
  harness            = CASE WHEN ? IS NOT NULL THEN ? ELSE harness END,
  harness_session_id = COALESCE(excluded.harness_session_id, harness_session_id),
  isolation_mode     = CASE WHEN ? IS NOT NULL THEN ? ELSE isolation_mode END`
	_, err := d.conn.Exec(q, sessionName, repo, worktree, state, title, rootAgentNamePtr, now, insertHarness, harnessSessionID, insertIsolationMode, harnessNamePtr, harnessNamePtr, isolationModePtr, isolationModePtr)
	if err != nil {
		return fmt.Errorf("db: upsert status seed root agent name: %w", err)
	}
	return nil
}

// UpsertStatusWithRootAgent is like UpsertStatusWithAgent but also writes
// root_agent_name and root_model_id. On conflict (update), root_agent_name and
// root_model_id prefer the incoming (excluded) value via COALESCE — the sidecar
// is authoritative and can correct a stale or wrong value on every state update.
//
// The Go sidecar is the authoritative source of root_agent_name: it calls this
// method on every state transition when Config.AgentRole is set. Because the
// sidecar value takes precedence, a row written with the wrong root_agent_name
// (e.g. from a legacy or race-condition write) is corrected on the very next
// sidecar call. The TypeScript plugin (prism-hooks.ts) does not write
// root_agent_name.
func (d *DB) UpsertStatusWithRootAgent(sessionName, repo, worktree, state string, title *string, harnessSessionID *string, agentName *string, modelID *string) error {
	d.checkTransition(sessionName, agent.AgentState(state), "UpsertStatusWithRootAgent")
	now := time.Now().UnixMilli()
	const q = `
INSERT INTO agent_status (session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, last_seen, harness, harness_session_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pi', ?)
ON CONFLICT(session_name) DO UPDATE SET
  state              = excluded.state,
  repo               = excluded.repo,
  worktree           = excluded.worktree,
  title              = COALESCE(excluded.title, title),
  agent_name         = COALESCE(excluded.agent_name, agent_name),
  model_id           = COALESCE(excluded.model_id, model_id),
  root_agent_name    = COALESCE(excluded.root_agent_name, root_agent_name),
  root_model_id      = COALESCE(excluded.root_model_id, root_model_id),
  last_seen          = excluded.last_seen,
  harness            = COALESCE(harness, excluded.harness),
  harness_session_id = COALESCE(excluded.harness_session_id, harness_session_id)`
	_, err := d.conn.Exec(q, sessionName, repo, worktree, state, title, agentName, modelID, agentName, modelID, now, harnessSessionID)
	if err != nil {
		return fmt.Errorf("db: upsert status with root agent: %w", err)
	}
	return nil
}

// UpsertStatusIfNotTerminal upserts the state for sessionName only when the
// current state is not already a terminal state (error, finished, interrupted,
// or deleted) and the session has not yet been ended (ended_at IS NULL). Returns
// (true, nil) if the update was applied, (false, nil) if the session was
// already in a terminal state, has been ended, or did not exist, or
// (false, err) on a database error.
//
// This is used by the pane-died hook to transition active sessions to
// "interrupted" without clobbering a clean "finished" or an "error" state
// written directly by the sidecar on startup failure, and without acting on
// sessions that have already been ended by cleanup.
func (d *DB) UpsertStatusIfNotTerminal(sessionName, state string) (bool, error) {
	// Snapshot the current state before the write so that the advisory
	// transition check (below) sees the from-state rather than the newly
	// written to-state.
	fromState, _ := d.currentStateOf(sessionName)
	now := time.Now().UnixMilli()
	const q = `
UPDATE agent_status
SET state = ?, last_seen = ?
WHERE session_name = ?
  AND ended_at IS NULL
  AND state NOT IN ('error', 'finished', 'interrupted', 'deleted')`
	res, err := d.conn.Exec(q, state, now, sessionName)
	if err != nil {
		return false, fmt.Errorf("db: upsert status if not terminal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("db: upsert status if not terminal: rows affected: %w", err)
	}
	// Validate only when the write was actually applied (n > 0) and we have a
	// prior state to validate from. When the SQL WHERE clause suppresses the
	// write (session already terminal), there is no transition to check.
	if n > 0 && fromState != "" {
		if terr := agent.Transition(fromState, agent.AgentState(state)); terr != nil {
			proglog.Errorf("[prism] UpsertStatusIfNotTerminal: invalid transition for session %q: %v\n",
				sessionName, terr)
		}
	}
	return n > 0, nil
}

// UpsertStatusInterruptedOverrideFinished transitions the session to
// "interrupted", allowing it to override a "finished" state in addition to the
// active states that UpsertStatusIfNotTerminal covers.  This is used by the
// pane-died hook when the pane exited with a non-zero exit code: a non-zero
// exit means the process was killed or crashed, so even a prior "finished" that
// the plugin wrote should be corrected to "interrupted".
//
// "error" is left intact — a startup failure that wrote "error" directly (via
// writeStartupError in sidecar.go) must not be overwritten to "interrupted" by
// the pane-died hook, since "error" is the correct terminal state in that case.
// "deleted" is still left intact — a deleted session should not be resurrected.
// "interrupted" is also left alone to avoid a no-op double-write.
//
// Returns (true, nil) if the update was applied, (false, nil) if the row did
// not exist, was already error/interrupted/deleted, or has ended_at set.
func (d *DB) UpsertStatusInterruptedOverrideFinished(sessionName string) (bool, error) {
	// Snapshot the current state before the write so that the advisory
	// transition check (below) sees the from-state rather than the newly
	// written to-state.
	fromState, _ := d.currentStateOf(sessionName)
	now := time.Now().UnixMilli()
	const q = `
UPDATE agent_status
SET state = 'interrupted', last_seen = ?
WHERE session_name = ?
  AND ended_at IS NULL
  AND state NOT IN ('error', 'interrupted', 'deleted')`
	res, err := d.conn.Exec(q, now, sessionName)
	if err != nil {
		return false, fmt.Errorf("db: upsert status interrupted override finished: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("db: upsert status interrupted override finished: rows affected: %w", err)
	}
	// Validate only when the write was actually applied (n > 0) and we have a
	// prior state to validate from. When the SQL WHERE clause suppresses the
	// write (session already interrupted or deleted), there is no transition
	// to check.
	if n > 0 && fromState != "" {
		if terr := agent.Transition(fromState, agent.StateInterrupted); terr != nil {
			proglog.Errorf("[prism] UpsertStatusInterruptedOverrideFinished: invalid transition for session %q: %v\n",
				sessionName, terr)
		}
	}
	return n > 0, nil
}

// SetEnded marks the session as ended by setting ended_at to now.
// It also cascades the ended_at to any child review-agent rows whose
// session_name matches the pattern "<sessionName>~review-%", setting
// ended_at only on rows where it is not already set (idempotent).
//
// Both tables are updated atomically in a single transaction:
//   - agent_status: ended_at = now for the matched rows
//   - sessions: ended_at = now, end_state = 'reset' for the corresponding
//     instance_id rows
//
// end_state is set to 'reset' rather than a lifecycle-derived value because
// SetEnded is called in contexts where the caller does not yet have a final
// state to report (e.g. prism cleanup, review teardown). The sessions row
// may subsequently be overwritten by UpdateSessionEnded with a more precise
// end_state (e.g. 'finished') — that call is idempotent with respect to
// ended_at and simply refines end_state.
//
// The session name is escaped for SQL LIKE wildcards before being used as a
// pattern prefix (using the same escaping as AllStatusesWithPrefix) so that
// session names containing `%`, `_`, or `\` are handled correctly.
func (d *DB) SetEnded(sessionName string) error {
	now := time.Now().UnixMilli()
	// Escape LIKE special characters in sessionName so that literal `%`, `_`,
	// and `\` in the name are matched exactly, not as wildcards.
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(sessionName)
	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("db: set ended: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const agentStatusQ = `
UPDATE agent_status
SET    ended_at = ?
WHERE  (session_name = ? OR session_name LIKE ? || '~review-%' ESCAPE '\')
  AND  ended_at IS NULL`
	if _, err := tx.Exec(agentStatusQ, now, sessionName, escaped); err != nil {
		return fmt.Errorf("db: set ended: agent_status update: %w", err)
	}

	// Also update the sessions table for the corresponding instance_id rows.
	// end_state 'reset' is used because SetEnded is called before the caller
	// knows the final lifecycle outcome; UpdateSessionEnded may overwrite with
	// a more specific end_state (e.g. 'finished') in the same cleanup pass.
	const sessionsQ = `
UPDATE sessions
SET    ended_at  = ?,
       end_state = 'reset'
WHERE  instance_id IN (
         SELECT instance_id
         FROM   agent_status
         WHERE  (session_name = ? OR session_name LIKE ? || '~review-%' ESCAPE '\')
           AND  instance_id IS NOT NULL
       )
  AND  ended_at IS NULL`
	if _, err := tx.Exec(sessionsQ, now, sessionName, escaped); err != nil {
		return fmt.Errorf("db: set ended: sessions update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: set ended: commit: %w", err)
	}
	return nil
}

// MarkAllEnded marks every row in agent_status where ended_at IS NULL as ended
// by setting ended_at = now (Unix milliseconds). The state column is intentionally
// left unchanged — ended_at IS NULL is the canonical "active session" filter used
// throughout the codebase; state captures the last known agent state before teardown
// and is not overwritten here.
//
// Both tables are updated atomically in a single transaction:
//   - agent_status: ended_at = now for all rows where ended_at IS NULL
//   - sessions: ended_at = now, end_state = 'reset' for the corresponding
//     instance_id rows (matched via the instance_id column in agent_status)
//
// end_state is set to 'reset' because MarkAllEnded is called by `prism reset`,
// which forcibly terminates all live sessions regardless of their lifecycle
// state. 'reset' is more precise than 'interrupted' (which implies a pane
// crash) and more honest than 'finished' (which implies a clean exit).
//
// It is used by `prism reset` to atomically close all live sessions in one
// query rather than iterating over them individually.
//
// MarkAllEnded is intentionally narrow: it only stamps ended_at / end_state.
// It does NOT clear harness_session_id — the per-session pi conversation
// resume pointer is wiped by the sibling method ClearAllResumePointers,
// which `prism reset` calls immediately after MarkAllEnded. Keeping the two
// concerns separate means MarkAllEnded can be reused by any future caller
// that wants to bulk-end rows without altering resume semantics (issue #1947).
//
// Returns the number of agent_status rows updated and any database error.
// When there are no rows with ended_at IS NULL, returns (0, nil) — not an error.
func (d *DB) MarkAllEnded() (int64, error) {
	now := time.Now().UnixMilli()
	tx, err := d.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("db: mark all ended: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec(
		"UPDATE agent_status SET ended_at = ? WHERE ended_at IS NULL",
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("db: mark all ended: agent_status update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: mark all ended: rows affected: %w", err)
	}

	// Also update the sessions table for all instance_ids whose agent_status
	// rows were just ended. end_state 'reset' is used because MarkAllEnded is
	// invoked exclusively by `prism reset`, which forcibly terminates all live
	// sessions — 'reset' accurately describes the cause of termination.
	_, err = tx.Exec(`
UPDATE sessions
SET    ended_at  = ?,
       end_state = 'reset'
WHERE  instance_id IN (
         SELECT instance_id
         FROM   agent_status
         WHERE  instance_id IS NOT NULL
           AND  ended_at    = ?
       )
  AND  ended_at IS NULL`,
		now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("db: mark all ended: sessions update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("db: mark all ended: commit: %w", err)
	}
	return n, nil
}

// ClearAllResumePointers clears the per-session pi conversation resume
// pointer (agent_status.harness_session_id) on every row in agent_status.
// It is the FS-companion-free, DB-side half of the `prism reset` resume-wipe
// (issue #1947): after this call, no row carries a UUID that would cause
// the next `prism switch` / `prism agent-run` to append `--session <uuid>`
// to the pi invocation (see internal/container/pi_invocation.go).
//
// Relationship to MarkAllEnded:
//   - MarkAllEnded stamps ended_at / end_state (it ends sessions).
//   - ClearAllResumePointers wipes harness_session_id (it forgets conversations).
//
// `prism reset` calls MarkAllEnded then ClearAllResumePointers — the order
// does not matter (the columns are independent), but the call site documents
// reset's intent: end every session, then forget the resume pointer.
//
// The column is set to NULL (rather than the empty string) so that the
// COALESCE in UpsertStatusSeedRootAgentName treats it as "no override" on
// the next upsert — exactly mirroring the pre-#1838 "fresh row" semantics.
// CurrentStatus / scanStatus already map a NULL column to a nil *string in
// the Status struct.
//
// Returns the number of rows whose harness_session_id was actually cleared
// (i.e. previously non-NULL) and any database error. Rows that were already
// NULL are not counted. Returns (0, nil) on an empty / all-NULL table.
func (d *DB) ClearAllResumePointers() (int64, error) {
	res, err := d.conn.Exec(
		"UPDATE agent_status SET harness_session_id = NULL WHERE harness_session_id IS NOT NULL",
	)
	if err != nil {
		return 0, fmt.Errorf("db: clear all resume pointers: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: clear all resume pointers: rows affected: %w", err)
	}
	return n, nil
}

// ClearHarnessSessionID clears agent_status.harness_session_id for sessionName
// AND for any review-agent child rows whose session_name matches
// "<sessionName>~review-%" (mirroring SetEnded's LIKE-escape semantics).
//
// This is the per-session counterpart to ClearAllResumePointers (issue #1947).
// It is called from the `prism cleanup` paths so that re-spawning a NEW
// session on the SAME branch name does not pick up the cleaned session's
// stale harness_session_id and unconditionally resume the dead pi conversation
// (issue #2035).
//
// Cleanup must sever the resume linkage on two surfaces:
//
//  1. The DB: this method nulls agent_status.harness_session_id so
//     cmd/agent_run.go's spawn-time read (`status.HarnessSessionID`) returns
//     a nil pointer on the next spawn — and the in-sandbox PIInvocation
//     therefore omits `--session <id>` and starts a fresh pi conversation.
//
//  2. The filesystem: the JSONL transcript under
//     <piSessionsRoot>/<encodePiCWD(worktree)>/*_<harness_session_id>.jsonl
//     — handled by container.RemovePiResumeJSONL on the cleanup side.
//
// Fix (1) is load-bearing on its own; fix (2) is defence-in-depth. See the
// issue body for the full forensics.
//
// The session name is escaped for SQL LIKE wildcards before being used as a
// pattern prefix so that names containing `%`, `_`, or `\` are handled
// correctly.
//
// Returns nil even when no rows matched — the call is idempotent and safe
// to invoke against a session whose harness_session_id was already NULL.
func (d *DB) ClearHarnessSessionID(sessionName string) error {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(sessionName)
	const q = `
UPDATE agent_status
SET    harness_session_id = NULL
WHERE  session_name = ? OR session_name LIKE ? || '~review-%' ESCAPE '\'`
	if _, err := d.conn.Exec(q, sessionName, escaped); err != nil {
		return fmt.Errorf("db: clear harness session id: %w", err)
	}
	return nil
}

// SetMuted sets the muted flag for sessionName to muted (true = 1, false = 0).
//
// Returns (false, nil) when no agent_status row exists for sessionName so
// callers (the `prism mute` CLI) can report a clear "session not found"
// error without inserting a phantom row. Returns (true, nil) when the row
// exists and the column was updated.
//
// The boolean does not indicate whether the value actually changed — calling
// SetMuted twice with the same value is a no-op-on-state but still returns
// (true, nil) so the CLI's idempotence path can be observed by the caller.
func (d *DB) SetMuted(sessionName string, muted bool) (bool, error) {
	val := 0
	if muted {
		val = 1
	}
	res, err := d.conn.Exec(
		`UPDATE agent_status SET muted = ? WHERE session_name = ?`,
		val, sessionName,
	)
	if err != nil {
		return false, fmt.Errorf("db: set muted: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("db: set muted rows affected: %w", err)
	}
	return n > 0, nil
}

// IsMuted reports whether the session is currently muted. Returns
// (false, false, nil) when no row exists or the row exists but the session
// has already been ended (ended_at IS NOT NULL) — in both cases the mute
// CLI treats the session as "not found" and refuses to toggle.
//
// Restricting the lookup to live (ended_at IS NULL) rows matches the AC for
// `prism cleanup --session <name>` followed by `prism mute <name>`: after
// cleanup, the row carries ended_at, so mute reports "session not found".
// The flag column itself is not erased — it persists alongside ended_at for
// audit — but the operator-facing surface treats the session as gone.
func (d *DB) IsMuted(sessionName string) (bool, bool, error) {
	var m int64
	err := d.conn.QueryRow(
		`SELECT muted FROM agent_status WHERE session_name = ? AND ended_at IS NULL`,
		sessionName,
	).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("db: is muted: %w", err)
	}
	return m != 0, true, nil
}

// ClearEnded clears the ended_at timestamp for sessionName, making the session
// visible again to AllActiveStatus and the dashboard (which both filter
// WHERE ended_at IS NULL). Called when a session resumes from a terminal state
// so that the resumed session re-appears in all active-session views.
func (d *DB) ClearEnded(sessionName string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET ended_at = NULL WHERE session_name = ?",
		sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: clear ended: %w", err)
	}
	return nil
}

// AllocatePort picks the lowest unused port from the range PortRangeStart–PortRangeEnd,
// writes it to agent_status.harness_port for sessionName, and returns it.
//
// A port is considered "in use" if it is assigned to a session whose ended_at IS NULL
// and harness_port IS NOT NULL. Ports assigned to ended sessions (ended_at IS NOT NULL)
// are reclaimed and available for reuse.
//
// In addition to the DB check, each candidate port is probed at the OS level via
// a brief TCP listen on 127.0.0.1:<port>. This prevents conflicts with non-prism
// processes that happen to be using a port in the range.
//
// Returns an error if all ports in the range are exhausted or if the session does
// not exist in agent_status.
//
// Concurrency mechanism — transaction + retry with unique-constraint guard (#1865 / DB-F4):
//
// The original check-then-write design was a race: two concurrent callers could
// both read the same usedPorts snapshot, both pick the same free port, both call
// portAvailable() (which may transiently succeed for both because the OS listen
// window is brief), and both write the same port to different rows — with neither
// UPDATE failing. The fix wraps each attempt in a BEGIN IMMEDIATE transaction so
// that SQLite serialises the read+write pair: once the first writer commits, the
// second writer's read sees the updated row and picks a different port. A
// partial unique index on harness_port (WHERE harness_port IS NOT NULL AND ended_at IS NULL,
// added in migration v32→v33) provides a second line of defence: even if two transactions
// race to write the same port, only one can commit; the other receives a
// SQLITE_CONSTRAINT_UNIQUE error and retries from scratch. The retry budget is
// bounded (maxRetries) to prevent an infinite loop when the range is exhausted.
func (d *DB) AllocatePort(sessionName string) (int, error) {
	const maxRetries = 10
	for attempt := range maxRetries {
		port, err := d.allocatePortOnce(sessionName)
		if err == nil {
			return port, nil
		}
		// Retry on unique-constraint violations (another concurrent caller
		// committed the same port between our read and our write) and on
		// SQLITE_BUSY / SQLITE_BUSY_SNAPSHOT (WAL lock contention when many
		// goroutines issue write transactions simultaneously).
		if (isUniqueConstraintError(err) || isBusyError(err)) && attempt < maxRetries-1 {
			continue
		}
		return 0, err
	}
	return 0, fmt.Errorf("db: allocate port: failed after %d attempts due to concurrent allocation", maxRetries)
}

// allocatePortOnce performs a single attempt of the port-allocation logic
// inside a BEGIN IMMEDIATE transaction. Returns an error (possibly wrapping a
// unique-constraint violation or SQLITE_BUSY) if the chosen port was claimed
// by a concurrent writer or if the write lock could not be acquired.
func (d *DB) allocatePortOnce(sessionName string) (int, error) {
	ctx := context.Background()
	// Obtain a dedicated connection from the pool so that we can issue
	// BEGIN IMMEDIATE directly. database/sql's BeginTx does not expose
	// SQLite-specific locking modes; using a raw connection and executing
	// "BEGIN IMMEDIATE" is the canonical way to avoid SQLITE_BUSY_SNAPSHOT
	// (which the busy_timeout does not suppress).
	conn, err := d.conn.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("db: allocate port: acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		// SQLITE_BUSY: another writer holds the write lock; caller retries.
		return 0, fmt.Errorf("db: allocate port: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(ctx, "ROLLBACK") //nolint:errcheck
		}
	}()

	// Collect ports currently assigned to active (non-ended) sessions.
	rows, err := conn.QueryContext(ctx,
		"SELECT harness_port FROM agent_status WHERE ended_at IS NULL AND harness_port IS NOT NULL",
	)
	if err != nil {
		return 0, fmt.Errorf("db: allocate port: query used ports: %w", err)
	}
	usedPorts := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, fmt.Errorf("db: allocate port: scan port: %w", err)
		}
		usedPorts[p] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("db: allocate port: iterate ports: %w", err)
	}

	// Find the lowest unused port that is also available at the OS level.
	for port := PortRangeStart; port <= PortRangeEnd; port++ {
		if usedPorts[port] {
			continue
		}
		if !portAvailable(port) {
			continue
		}
		// Write the allocated port inside the transaction. The unique partial
		// index on harness_port (WHERE harness_port IS NOT NULL AND ended_at IS NULL) means this
		// UPDATE fails with a constraint error if another transaction committed
		// the same port concurrently.
		res, err := conn.ExecContext(ctx,
			"UPDATE agent_status SET harness_port = ? WHERE session_name = ?",
			port, sessionName,
		)
		if err != nil {
			// Propagate as-is; the caller checks isUniqueConstraintError.
			return 0, fmt.Errorf("db: allocate port: update: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("db: allocate port: rows affected: %w", err)
		}
		if n == 0 {
			return 0, fmt.Errorf("db: allocate port: session %q not found in agent_status", sessionName)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return 0, fmt.Errorf("db: allocate port: commit: %w", err)
		}
		committed = true
		return port, nil
	}

	return 0, fmt.Errorf("db: allocate port: all ports in range %d–%d are exhausted", PortRangeStart, PortRangeEnd)
}

// isUniqueConstraintError reports whether err is a SQLite unique-constraint
// violation (SQLITE_CONSTRAINT_UNIQUE or SQLITE_CONSTRAINT_PRIMARYKEY).
func isUniqueConstraintError(err error) bool {
	var sqliteErr *sqlitelib.Error
	if errors.As(err, &sqliteErr) {
		// SQLite extended result codes: 2067 = SQLITE_CONSTRAINT_UNIQUE,
		// 1555 = SQLITE_CONSTRAINT_PRIMARYKEY.
		code := sqliteErr.Code()
		return code == 2067 || code == 1555
	}
	return false
}

// isBusyError reports whether err is a SQLite lock-contention error
// (SQLITE_BUSY=5, SQLITE_BUSY_RECOVERY=261, SQLITE_BUSY_SNAPSHOT=517,
// SQLITE_BUSY_TIMEOUT=773, or plain SQLITE_LOCKED=6).
// These arise under WAL mode when many goroutines issue write transactions
// simultaneously — the busy timeout does not apply to SQLITE_BUSY_SNAPSHOT.
func isBusyError(err error) bool {
	var sqliteErr *sqlitelib.Error
	if errors.As(err, &sqliteErr) {
		// All SQLITE_BUSY variants share the low 8 bits == 5;
		// SQLITE_LOCKED shares the low 8 bits == 6.
		code := sqliteErr.Code()
		primary := code & 0xFF
		return primary == 5 || primary == 6
	}
	return false
}

// ReleasePort sets harness_port = NULL for the given session.
// Returns an error if the session does not exist in agent_status.
// Calling ReleasePort on a session whose harness_port is already NULL is
// idempotent and returns nil.
func (d *DB) ReleasePort(sessionName string) error {
	res, err := d.conn.Exec(
		"UPDATE agent_status SET harness_port = NULL WHERE session_name = ?",
		sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: release port: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("db: release port: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("db: release port: session %q not found in agent_status", sessionName)
	}
	return nil
}

// SetIsolationMode records the resolved isolation mode for the given session.
// mode is one of "bwrap", "sandbox-exec", or "host". This is persisted so that
// prism restore can re-spawn the session in the same isolation mode.
// It is a no-op when no row exists for sessionName (returns nil).
func (d *DB) SetIsolationMode(sessionName, mode string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET isolation_mode = ? WHERE session_name = ?",
		mode, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set isolation_mode: %w", err)
	}
	return nil
}

// SetContainersEnabled writes agent_status.containers_enabled for sessionName
// (#2317 / #2323). enabled=true is the runtime gate the sidecar reads to
// decide whether to start the per-session filtering podman API socket proxy;
// the spawn-time CLI flag --containers flips this on. enabled=false leaves the
// row at the default (proxy not started).
//
// No-op when sessionName has no agent_status row (the UPDATE affects zero rows
// and returns nil). Mirrors SetIsolationMode / SetGroupID — write the boolean
// as an INTEGER 0/1 so the column shape matches the schema (INTEGER NOT NULL
// DEFAULT 0).
func (d *DB) SetContainersEnabled(sessionName string, enabled bool) error {
	var v int
	if enabled {
		v = 1
	}
	_, err := d.conn.Exec(
		"UPDATE agent_status SET containers_enabled = ? WHERE session_name = ?",
		v, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set containers_enabled: %w", err)
	}
	return nil
}

// portAvailable checks whether a TCP port is available on localhost by
// attempting a brief listen. Returns true if the port is free.
func portAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// RefreshWorktree unconditionally updates the repo and worktree columns for an
// existing agent_status row. It also resets state to idle and refreshes
// last_seen, making it useful for prism restore (which needs a clean idle state
// regardless of what the prior session left behind). Unlike UpsertStatus, this
// does not insert a new row when none exists — it is a no-op for unknown sessions.
//
// It is a no-op when no row exists for sessionName (returns nil).
func (d *DB) RefreshWorktree(sessionName, repo, worktree string) error {
	// RefreshWorktree is an administrative reset (correcting corrupted values
	// during prism restore), not a normal lifecycle transition. It bypasses
	// the state machine advisory check intentionally — idle is not a valid
	// to-state in ValidTransitions, so calling checkTransition here would
	// always produce spurious warnings on every restore invocation.
	now := time.Now().UnixMilli()
	_, err := d.conn.Exec(
		`UPDATE agent_status
		    SET repo = ?, worktree = ?, state = ?, last_seen = ?
		  WHERE session_name = ?`,
		repo, worktree, "idle", now, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: refresh worktree: %w", err)
	}
	return nil
}

// SetInstanceID writes a UUID instance_id to the agent_status row for
// sessionName. Called on tmux-session-start to uniquely identify this session
// incarnation.
func (d *DB) SetInstanceID(sessionName, instanceID string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET instance_id = ? WHERE session_name = ?",
		instanceID, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set instance_id: %w", err)
	}
	return nil
}

// SetGroupID writes a group_id to the agent_status row for sessionName. Called
// by SpawnSession when opts.GroupID is non-empty to associate the new session
// with a session_groups entry (Issue E hook — see #849 §3.1 and #860).
//
// No-op when sessionName has no agent_status row (the UPDATE affects zero rows
// and returns nil).
func (d *DB) SetGroupID(sessionName, groupID string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET group_id = ? WHERE session_name = ?",
		groupID, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set group_id: %w", err)
	}
	return nil
}

// ClearInstanceID sets instance_id to NULL for sessionName. Called on
// tmux-session-end to mark the session incarnation as over.
func (d *DB) ClearInstanceID(sessionName string) error {
	_, err := d.conn.Exec(
		"UPDATE agent_status SET instance_id = NULL WHERE session_name = ?",
		sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: clear instance_id: %w", err)
	}
	return nil
}

// EnsureStatusRow inserts a minimal agent_status row for sessionName if one
// does not already exist. When the row already exists, it is left completely
// untouched (INSERT OR IGNORE semantics). This is used by the sidecar to
// guarantee that UpdateHarnessSessionID finds a row to UPDATE even when the
// session_status frame arrives before the first state_change or turn_start
// that would normally create the row via upsertState.
//
// The inserted row uses state="active" as a safe default; it will be
// overwritten by the next upsertState call.
func (d *DB) EnsureStatusRow(sessionName, repo, worktree string) error {
	now := time.Now().UnixMilli()
	const q = `
INSERT OR IGNORE INTO agent_status (session_name, repo, worktree, state, last_seen)
VALUES (?, ?, ?, 'active', ?)`
	_, err := d.conn.Exec(q, sessionName, repo, worktree, now)
	if err != nil {
		return fmt.Errorf("db: ensure status row: %w", err)
	}
	return nil
}

// UpdateHarnessSessionID unconditionally sets harness_session_id for sessionName
// to the given sid value. Unlike UpsertStatus (which only updates harness_session_id
// via COALESCE when non-nil), this always overwrites — allowing the sidecar to
// keep the stored SID current when the user creates a new harness session
// mid-conversation (e.g. via /continue or TUI restart).
//
// It writes to both agent_status and sessions (matching by instance_id) so
// that cleanup.runSessionArchive can read harness_session_id directly from the
// sessions row without a fallback query. The sessions update is best-effort:
// if no row exists for the current instance_id it is silently skipped.
//
// It is a no-op when no row exists for sessionName (returns nil).
func (d *DB) UpdateHarnessSessionID(sessionName, sid string) error {
	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("db: update harness_session_id: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(
		"UPDATE agent_status SET harness_session_id = ? WHERE session_name = ?",
		sid, sessionName,
	); err != nil {
		return fmt.Errorf("db: update harness_session_id (agent_status): %w", err)
	}

	// Also update sessions.harness_session_id for the current incarnation so
	// that cleanup reads the correct value without a fallback query.
	if _, err := tx.Exec(`
		UPDATE sessions
		   SET harness_session_id = ?
		 WHERE instance_id = (
		         SELECT instance_id FROM agent_status WHERE session_name = ?
		       )`, sid, sessionName,
	); err != nil {
		return fmt.Errorf("db: update harness_session_id (sessions): %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: update harness_session_id: commit: %w", err)
	}
	return nil
}

// HarnessSessionIDForInstance returns the harness_session_id stored in
// agent_status for the given instance_id, or "" when not found or NULL.
// Used by cleanup as a fallback when sessions.harness_session_id is NULL
// (e.g. for sessions started before UpdateHarnessSessionID was fixed to
// also write to sessions).
func (d *DB) HarnessSessionIDForInstance(instanceID string) (string, error) {
	var sid sql.NullString
	err := d.conn.QueryRow(
		"SELECT harness_session_id FROM agent_status WHERE instance_id = ?",
		instanceID,
	).Scan(&sid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: harness_session_id for instance: %w", err)
	}
	if !sid.Valid {
		return "", nil
	}
	return sid.String, nil
}

// CurrentStatus returns the agent_status row for sessionName, or nil if not found.
func (d *DB) CurrentStatus(sessionName string) (*Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
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
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE ended_at IS NULL`
	return d.queryStatuses(q)
}

// ActiveSessionCountForMode returns the number of agent_status rows where
// ended_at IS NULL AND isolation_mode = mode. Used by the per-isolator
// concurrency cap checks. Returns 0 when no rows match.
//
// Replaces the mode-specific ActiveBwrapSessionCount and
// ActiveSandboxExecSessionCount helpers; callable for any IsolationMode value.
func (d *DB) ActiveSessionCountForMode(mode string) (int, error) {
	var n int
	err := d.conn.QueryRow(
		"SELECT COUNT(*) FROM agent_status WHERE ended_at IS NULL AND isolation_mode = ?",
		mode,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("db: active session count for mode %q: %w", mode, err)
	}
	return n, nil
}

// ActiveSessionsForMode returns the agent_status rows where ended_at IS NULL
// AND isolation_mode = mode, suitable for cap-error detail listings.
//
// Replaces the mode-specific ActiveBwrapSessions and ActiveSandboxExecSessions
// helpers; callable for any IsolationMode value.
func (d *DB) ActiveSessionsForMode(mode string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE ended_at IS NULL AND isolation_mode = ?`
	return d.queryStatuses(q, mode)
}

// AllActiveStatusForRepo returns all active agent_status rows for repo.
func (d *DB) AllActiveStatusForRepo(repo string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE ended_at IS NULL AND repo = ?`
	return d.queryStatuses(q, repo)
}

// AllActiveStatusForRepoAndOtherCoordinators returns all active agent_status
// rows that belong to repo, PLUS active rows from other repos that are
// coordinator sessions. "Coordinator" is defined as:
//
//	(root_agent_name = 'coordinator')
//	OR
//	(root_agent_name IS NULL AND session_name = '<other-repo>@main')
//
// The second clause handles pre-migration rows where root_agent_name has not
// yet been written, using the same @main name-heuristic as isCoordinatorSession
// in internal/sidecar/helpers.go.
//
// This is the default scope for `prism list-sessions` (no --all): own-repo
// sessions are always shown; other-repo sessions are shown only when they are
// coordinators. Other-repo workers are hidden as noise.
func (d *DB) AllActiveStatusForRepoAndOtherCoordinators(repo string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE ended_at IS NULL
  AND (
    repo = ?
    OR (
      repo != ?
      AND (
        root_agent_name = 'coordinator'
        OR (root_agent_name IS NULL AND session_name = (repo || '@main'))
      )
    )
  )`
	return d.queryStatuses(q, repo, repo)
}

// ActiveStatusForRepoWorktree returns the active (ended_at IS NULL)
// agent_status row for the given repo and worktree path, or nil when no such
// row exists. This is the natural-key dedupe check used by
// `prism spawn --reuse` and by the `prism spawn --branch main` default-reuse
// path (#2352).
//
// The second argument MUST be the full worktree filesystem path
// (e.g. "/Users/me/code/myrepo/main"), NOT the branch name. Every production
// writer of the `worktree` column stores a full path — SpawnSession seeds
// via `UpsertStatusSeedRootAgentName(session, repo, opts.Worktree, …)` with
// `opts.Worktree = worktreePath`, and `event tmux-session-start --worktree`
// is invoked with the pane's cwd. Passing a branch name here (the pre-#2352
// signature) silently mismatched every real row, so the dedupe never fired
// in production and the fall-through path failed at `tmux new-session -ds`
// with "duplicate session".
func (d *DB) ActiveStatusForRepoWorktree(repo, worktreePath string) (*Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE ended_at IS NULL AND repo = ? AND worktree = ?
LIMIT 1`
	statuses, err := d.queryStatuses(q, repo, worktreePath)
	if err != nil {
		return nil, err
	}
	if len(statuses) == 0 {
		return nil, nil
	}
	return &statuses[0], nil
}

// AllStatusesWithPrefix returns all agent_status rows (active and ended)
// whose session_name starts with the given prefix. Used by `prism checkin
// <parent>~review` to enumerate all review rounds including completed ones.
//
// The prefix is matched using LIKE with proper escaping of SQL LIKE wildcard
// characters (`%`, `_`, `\`) so that session names containing these characters
// are handled correctly.
func (d *DB) AllStatusesWithPrefix(prefix string) ([]Status, error) {
	// Escape LIKE special characters in the prefix so literal underscores and
	// percent signs in session names are matched exactly, not as wildcards.
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE session_name LIKE ? ESCAPE '\'`
	return d.queryStatuses(q, escaped+"%")
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

// ActivePISessionsForRepo returns all active agent_status rows where
// harness = 'pi' and repo = repo and state IN ('active', 'idle', 'waiting').
// Used by the /apply-profile and /register-provider host-API endpoints to
// resolve coordinator-scope targets (P3.LIVE, #1214).
//
// Design note: issue #1214 originally proposed filtering by coordinator_session_name
// to restrict fan-out to sessions spawned by the calling coordinator. That column
// does not exist in the schema; repo-based filtering is the closest available
// approximation. In the common single-coordinator-per-repo case the semantics are
// identical. When multiple coordinators run against the same repo simultaneously the
// scope is slightly broader — all PI sessions in the repo receive the frame rather
// than only those owned by the calling coordinator. This is acceptable for the P3
// milestone; a coordinator_session_name column can be added in a follow-up if
// strict per-coordinator targeting is required.
func (d *DB) ActivePISessionsForRepo(repo string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE ended_at IS NULL AND harness = 'pi' AND repo = ? AND state IN ('active', 'idle', 'waiting')`
	return d.queryStatuses(q, repo)
}

// AllActivePISessions returns all active agent_status rows where harness = 'pi'
// and state IN ('active', 'idle', 'waiting'). Used for scope=global fan-out
// by the /apply-profile and /register-provider host-API endpoints (P3.LIVE, #1214).
func (d *DB) AllActivePISessions() ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE ended_at IS NULL AND harness = 'pi' AND state IN ('active', 'idle', 'waiting')`
	return d.queryStatuses(q)
}

// CoordinatorCandidatesForRepo returns all active agent_status rows for repo
// that look like coordinator candidates: same repo, ended_at IS NULL, and
// either root_agent_name = "coordinator" OR (root_agent_name IS NULL AND
// session_name = "<repo>@main") so that pre-migration rows are still
// surfaced via the name convention. Rows are ordered by last_seen DESC so
// the most-recently-active candidate appears first.
//
// This is the discovery primitive for `prism escalate`: zero candidates means
// no coordinator is running; one candidate means auto-discovery succeeds; two
// or more means the worker must specify --to explicitly.
func (d *DB) CoordinatorCandidatesForRepo(repo string) ([]Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE repo = ? AND ended_at IS NULL
  AND (root_agent_name = 'coordinator' OR (root_agent_name IS NULL AND session_name = ? || '@main'))
ORDER BY last_seen DESC`
	return d.queryStatuses(q, repo, repo)
}

// CoordinatorForRepo returns the agent_status row for the active coordinator
// session of repo (i.e. the row where repo = repo AND root_agent_name =
// "coordinator" AND ended_at IS NULL). Returns nil when no coordinator exists.
// When multiple rows match (schema violation), the most-recently-seen row is
// returned and a duplicate is silently tolerated.
func (d *DB) CoordinatorForRepo(repo string) (*Status, error) {
	const q = `
SELECT session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, root_model_id, isolation_mode, instance_id, last_seen, ended_at, harness, harness_session_id, harness_port, group_id, muted, containers_enabled
FROM agent_status
WHERE repo = ? AND root_agent_name = 'coordinator' AND ended_at IS NULL
ORDER BY last_seen DESC
LIMIT 1`
	row := d.conn.QueryRow(q, repo)
	s, err := scanStatus(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: coordinator for repo: %w", err)
	}
	return s, nil
}

// RootAgentName returns the root_agent_name for sessionName, or "" when the
// row does not exist or root_agent_name is NULL (pre-migration row).
// The second return value (rowExists) distinguishes the two empty cases:
//   - ("", false, nil)  — no agent_status row found (new/unknown session)
//   - ("", true, nil)   — row found but root_agent_name is NULL (pre-migration)
//   - (name, true, nil) — row found with a populated root_agent_name
func (d *DB) RootAgentName(sessionName string) (name string, rowExists bool, err error) {
	var ns sql.NullString
	const q = `SELECT root_agent_name FROM agent_status WHERE session_name = ?`
	if scanErr := d.conn.QueryRow(q, sessionName).Scan(&ns); scanErr != nil {
		if scanErr == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, fmt.Errorf("db: root agent name: %w", scanErr)
	}
	if !ns.Valid {
		return "", true, nil
	}
	return ns.String, true, nil
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
	var instanceID sql.NullString
	var harness sql.NullString
	var harnessSessionID sql.NullString
	var harnessPort sql.NullInt64
	var isolationMode sql.NullString
	var groupID sql.NullString
	var muted int64
	var containersEnabled int64
	err := s.Scan(
		&st.SessionName, &st.Repo, &st.Worktree, &st.State,
		&st.Title, &st.AgentName, &st.ModelID,
		&st.RootAgentName, &st.RootModelID, &isolationMode, &instanceID, &lastSeen, &endedAt,
		&harness, &harnessSessionID, &harnessPort, &groupID, &muted, &containersEnabled,
	)
	if err != nil {
		return nil, err
	}
	if groupID.Valid {
		g := groupID.String
		st.GroupID = &g
	}
	st.LastSeen = time.UnixMilli(lastSeen)
	if endedAt.Valid {
		t := time.UnixMilli(endedAt.Int64)
		st.EndedAt = &t
	}
	// isolation_mode: NULL means not recorded (pre-v10 row).
	if isolationMode.Valid {
		st.IsolationMode = isolationMode.String
	}
	if instanceID.Valid {
		id := instanceID.String
		st.InstanceID = &id
	}
	if harness.Valid {
		h := harness.String
		st.Harness = &h
	}
	if harnessSessionID.Valid {
		hsid := harnessSessionID.String
		st.HarnessSessionID = &hsid
	}
	if harnessPort.Valid {
		hp := int(harnessPort.Int64)
		st.HarnessPort = &hp
	}
	st.Muted = muted != 0
	st.ContainersEnabled = containersEnabled != 0
	return &st, nil
}

// SetHarness unconditionally sets the harness column for sessionName.
// It is used in tests and tooling to override the harness for an existing row.
func (d *DB) SetHarness(sessionName, harness string) error {
	_, err := d.conn.Exec(
		`UPDATE agent_status SET harness = ? WHERE session_name = ?`,
		harness, sessionName,
	)
	if err != nil {
		return fmt.Errorf("db: set harness: %w", err)
	}
	return nil
}

// SetHarnessRaw is an alias for SetHarness provided for callers that need a
// separate symbol (e.g. test fallback paths).
func (d *DB) SetHarnessRaw(sessionName, harness string) error {
	return d.SetHarness(sessionName, harness)
}

// UpsertStatusFull is like UpsertStatus but also accepts an explicit harness
// value. All pointer parameters are optional (nil = COALESCE / preserve existing).
// The signature mirrors UpsertStatusWithRootAgent extended with a harness parameter.
func (d *DB) UpsertStatusFull(sessionName, repo, worktree, state string, title, harnessSessionID, agentName, modelID, rootAgentName, harness *string) error {
	d.checkTransition(sessionName, agent.AgentState(state), "UpsertStatusFull")
	now := time.Now().UnixMilli()
	harnessVal := "pi"
	if harness != nil {
		harnessVal = *harness
	}
	const q = `
INSERT INTO agent_status (session_name, repo, worktree, state, title, agent_name, model_id, root_agent_name, last_seen, harness, harness_session_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_name) DO UPDATE SET
  state              = excluded.state,
  repo               = excluded.repo,
  worktree           = excluded.worktree,
  title              = COALESCE(excluded.title, title),
  agent_name         = COALESCE(excluded.agent_name, agent_name),
  model_id           = COALESCE(excluded.model_id, model_id),
  root_agent_name    = COALESCE(excluded.root_agent_name, root_agent_name),
  last_seen          = excluded.last_seen,
  harness            = excluded.harness,
  harness_session_id = COALESCE(excluded.harness_session_id, harness_session_id)`
	_, err := d.conn.Exec(q, sessionName, repo, worktree, state, title, agentName, modelID, rootAgentName, now, harnessVal, harnessSessionID)
	if err != nil {
		return fmt.Errorf("db: upsert status full: %w", err)
	}
	return nil
}

// SessionsByAbtestPairID returns all sessions rows (active and ended) whose
// spawn_inputs.abtest_pair_id equals pairID. The result is ordered by
// started_at ASC so the caller can treat index 0 as "session A" and index 1
// as "session B" deterministically.
//
// A pair that is fully cleaned up (both sessions ended) is still returned —
// callers use the results to render historical comparison views.
//
// The query joins sessions → spawn_inputs on instance_id. Sessions that have
// no spawn_inputs row (pre-migration or non-abtest) are excluded.
func (d *DB) SessionsByAbtestPairID(pairID string) ([]Session, error) {
	// We cannot use sessionsSelectCols directly because a JOIN with
	// spawn_inputs would make 'instance_id' ambiguous in SQLite.
	// Explicitly qualify all columns with the sessions table alias.
	const q = `
SELECT s.instance_id, s.session_name, s.agent_role, s.root_agent_name,
       s.repo, s.worktree, s.harness, s.harness_session_id, s.group_id,
       s.started_at, s.ended_at, s.end_state, s.archive_path, s.prism_version,
       s.parent_session
  FROM sessions s
INNER JOIN spawn_inputs si ON si.instance_id = s.instance_id
WHERE si.abtest_pair_id = ?
ORDER BY s.started_at ASC`
	return d.querySessions(q, pairID)
}

// AbtestPairRow holds summary data for a single abtest pair, aggregated from
// spawn_inputs and spawn_outcome.
type AbtestPairRow struct {
	PairID string // shared abtest_pair_id UUID

	// Session A (first spawned, lower started_at)
	SessionNameA string
	InstanceIDA  string
	ProfileA     string // spawn_inputs.profile_name; "" when not set
	StartedAtA   int64  // ms epoch
	EndedAtA     *int64 // nil when still running

	// Session B (second spawned)
	SessionNameB string
	InstanceIDB  string
	ProfileB     string
	StartedAtB   int64
	EndedAtB     *int64

	// Metrics from spawn_outcome (nil when outcome not yet computed)
	TurnsA        *int // msg_assistant_count
	TurnsB        *int
	TokensInputA  *int64
	TokensInputB  *int64
	TokensOutputA *int64
	TokensOutputB *int64
	DurationMsA   *int64
	DurationMsB   *int64
	EndStateA     *string
	EndStateB     *string
}

// AbtestPairsAll returns one AbtestPairRow per distinct abtest_pair_id.
// Pairs are ordered by the started_at of their first session (oldest first).
// Sessions that lack spawn_inputs or spawn_outcome rows are included but with
// nil metric fields.
func (d *DB) AbtestPairsAll() ([]AbtestPairRow, error) {
	// Fetch all (instance_id, session_name, started_at, ended_at, abtest_pair_id, profile_name)
	// tuples ordered by pair_id, started_at. We then group by pair_id in Go.
	const q = `
SELECT
    si.abtest_pair_id,
    s.instance_id,
    s.session_name,
    s.started_at,
    s.ended_at,
    COALESCE(si.profile_name, ''),
    COALESCE(so.msg_assistant_count, 0),
    COALESCE(so.tokens_input_total, 0),
    COALESCE(so.tokens_output_total, 0),
    so.duration_ms,
    so.end_state
FROM spawn_inputs si
INNER JOIN sessions s ON s.instance_id = si.instance_id
LEFT  JOIN spawn_outcome so ON so.instance_id = si.instance_id
WHERE si.abtest_pair_id IS NOT NULL
ORDER BY si.abtest_pair_id ASC, s.started_at ASC`

	rows, err := d.conn.Query(q)
	if err != nil {
		return nil, fmt.Errorf("db: abtest_pairs_all: %w", err)
	}
	defer rows.Close()

	// Accumulate into a map pair_id → AbtestPairRow (filling A slot first,
	// then B). Order of insertion is preserved via pairOrder slice.
	type abtestRawRow struct {
		pairID      string
		instanceID  string
		sessionName string
		startedAt   int64
		endedAt     sql.NullInt64
		profile     string
		turns       int
		tokensIn    int64
		tokensOut   int64
		durationMs  sql.NullInt64
		endState    sql.NullString
	}

	var rawRows []abtestRawRow
	for rows.Next() {
		var r abtestRawRow
		if scanErr := rows.Scan(
			&r.pairID, &r.instanceID, &r.sessionName, &r.startedAt, &r.endedAt,
			&r.profile, &r.turns, &r.tokensIn, &r.tokensOut, &r.durationMs, &r.endState,
		); scanErr != nil {
			return nil, fmt.Errorf("db: abtest_pairs_all: scan: %w", scanErr)
		}
		rawRows = append(rawRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: abtest_pairs_all: iterate: %w", err)
	}

	// Group into pairs by pairID.
	pairMap := make(map[string]*AbtestPairRow)
	var pairOrder []string
	for _, r := range rawRows {
		pr, exists := pairMap[r.pairID]
		if !exists {
			pr = &AbtestPairRow{PairID: r.pairID}
			pairMap[r.pairID] = pr
			pairOrder = append(pairOrder, r.pairID)
		}
		// Fill A first, then B.
		if pr.InstanceIDA == "" {
			// Slot A
			pr.InstanceIDA = r.instanceID
			pr.SessionNameA = r.sessionName
			pr.ProfileA = r.profile
			pr.StartedAtA = r.startedAt
			if r.endedAt.Valid {
				v := r.endedAt.Int64
				pr.EndedAtA = &v
			}
			turns := r.turns
			pr.TurnsA = &turns
			tokIn := r.tokensIn
			pr.TokensInputA = &tokIn
			tokOut := r.tokensOut
			pr.TokensOutputA = &tokOut
			if r.durationMs.Valid {
				v := r.durationMs.Int64
				pr.DurationMsA = &v
			}
			if r.endState.Valid {
				v := r.endState.String
				pr.EndStateA = &v
			}
		} else {
			// Slot B
			pr.InstanceIDB = r.instanceID
			pr.SessionNameB = r.sessionName
			pr.ProfileB = r.profile
			pr.StartedAtB = r.startedAt
			if r.endedAt.Valid {
				v := r.endedAt.Int64
				pr.EndedAtB = &v
			}
			turns := r.turns
			pr.TurnsB = &turns
			tokIn := r.tokensIn
			pr.TokensInputB = &tokIn
			tokOut := r.tokensOut
			pr.TokensOutputB = &tokOut
			if r.durationMs.Valid {
				v := r.durationMs.Int64
				pr.DurationMsB = &v
			}
			if r.endState.Valid {
				v := r.endState.String
				pr.EndStateB = &v
			}
		}
	}

	result := make([]AbtestPairRow, 0, len(pairOrder))
	for _, pid := range pairOrder {
		result = append(result, *pairMap[pid])
	}
	return result, nil
}

// AbtestPairsForSessions returns a map of session_name → abtest_pair_id for
// sessions that have a non-NULL abtest_pair_id in spawn_inputs. Only sessions
// whose instance_id appears in spawn_inputs with a non-NULL abtest_pair_id are
// included. This is used by list-sessions to render the ↔ pair indicator.
func (d *DB) AbtestPairsForSessions() (map[string]string, error) {
	const q = `
SELECT a.session_name, si.abtest_pair_id
FROM agent_status a
INNER JOIN sessions s ON s.session_name = a.session_name AND s.ended_at IS NULL
INNER JOIN spawn_inputs si ON si.instance_id = s.instance_id
WHERE si.abtest_pair_id IS NOT NULL`
	rows, err := d.conn.Query(q)
	if err != nil {
		return nil, fmt.Errorf("db: abtest pairs: %w", err)
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var name, pairID string
		if err := rows.Scan(&name, &pairID); err != nil {
			return nil, fmt.Errorf("db: abtest pairs scan: %w", err)
		}
		result[name] = pairID
	}
	return result, rows.Err()
}
