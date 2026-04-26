package review

// lifecycle.go — session lifecycle helpers for review rounds.
//
// This file contains functions that manage the tmux and DB lifecycle of review
// sessions: round numbering, session killing, cleanup, and per-agent session
// detection. These helpers are called from Run(), RunAsync(), and the cleanup
// command — they have no dependency on prompt or result formatting.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// NextRoundNumber returns the next round number for the given parent session.
// It queries the DB for all per-agent sessions (new shape: ~review-<N>-<agent>)
// so that the count is accurate even after previous rounds have been cleaned up.
// Returns 1 when no prior rounds exist.
//
// Old-shape round sessions (~review-<N> with pure integer suffix) are NOT
// counted — they belong to the pre-PR-C model and should not affect the counter.
// Old-shape agent sub-sessions (~review-<N>~<agent>) are also excluded.
func NextRoundNumber(d *db.DB, parentSession string) int {
	prefix := parentSession + "~review-"
	rows, err := d.AllStatusesWithPrefix(prefix)
	if err != nil {
		return 1
	}
	max := 0
	for _, row := range rows {
		suffix := strings.TrimPrefix(row.SessionName, prefix)
		// New shape: "N-<agent-name>" (e.g. "1-review-goal", "2-review-code").
		// Extract the leading integer before the first '-'.
		dashIdx := strings.Index(suffix, "-")
		if dashIdx <= 0 {
			// Pure integer (old round session, e.g. "1") or no dash at all —
			// skip; these are old-shape rows that we do not count.
			continue
		}
		nStr := suffix[:dashIdx]
		// Ensure nStr is a pure integer (not something like "1~review" from
		// old-shape agent sub-sessions that somehow snuck in).
		n, err := strconv.Atoi(nStr)
		if err != nil || n <= 0 {
			continue
		}
		// Validate: the agent portion must not contain '~' (old-shape markers).
		agentPart := suffix[dashIdx+1:]
		if strings.Contains(agentPart, "~") {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max + 1
}

// KillSessionPrefix kills all tmux sessions whose names start with the given
// prefix. Used to clean up all ~review-* sessions for a parent.
func KillSessionPrefix(prefix string) {
	out, err := tmux.Run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return
	}
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name != "" && strings.HasPrefix(name, prefix) {
			_ = tmux.KillSession(name)
		}
	}
}

// KillSessionsByNames kills the specified tmux sessions by exact name.
func KillSessionsByNames(names []string) {
	for _, name := range names {
		_ = tmux.KillSession(name)
	}
}

// KillCurrentRoundSessions kills only the sessions in the given list.
// Used by SIGINT handlers to kill only the current round's in-progress sessions
// without touching previous rounds' persisted sessions.
func KillCurrentRoundSessions(agentSessions []string) {
	KillSessionsByNames(agentSessions)
}

// KillReviewSessionsForParent kills all review sessions for the given parent.
// Uses DB group membership (GroupMembersForParent) as the primary source, with
// a name-prefix fallback for pre-migration rows where group_id is not set.
// This is the public API used by cleanup.go for cascading parent cleanup.
// It kills ALL review sessions across all rounds (for prism cleanup --yes --session <parent>).
func KillReviewSessionsForParent(parentSession string) {
	KillReviewSessionsForParentWithDB(nil, parentSession)
}

// KillReviewSessionsForParentWithDB is like KillReviewSessionsForParent but
// uses the DB for group membership when available.
func KillReviewSessionsForParentWithDB(d *db.DB, parentSession string) {
	prefix := parentSession + "~review-"

	// Try DB-backed group membership first (post-migration rows).
	if d != nil {
		members, err := d.GroupMembersForParent(parentSession)
		if err == nil && len(members) > 0 {
			names := make([]string, 0, len(members))
			for _, m := range members {
				names = append(names, m.SessionName)
			}
			KillSessionsByNames(names)
			return
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[prism] warning: KillReviewSessionsForParentWithDB: DB error for %q: %v — using name-prefix fallback\n", parentSession, err)
		}
		// len(members) == 0: fall through to name-prefix for pre-migration rows.
	}

	// Pre-migration fallback: kill by name prefix.
	KillSessionPrefix(prefix)
}

// CleanupReviewSessionsForParent kills all review sessions for the
// given parent AND cleans up their DB rows (port allocations, ended state,
// bus messages). Called by prism cleanup --yes --session <parent> to cascade
// the cleanup to all review sessions.
// Uses DB group membership (GroupMembersForParent) as the primary source, with
// a name-prefix fallback for pre-migration rows where group_id is not set.
func CleanupReviewSessionsForParent(d *db.DB, parentSession string) {
	prefix := parentSession + "~review-"

	// Try DB-backed group membership first (post-migration rows).
	members, err := d.GroupMembersForParent(parentSession)
	if err == nil && len(members) > 0 {
		// DB-backed: clean up only the actual group members.
		names := make([]string, 0, len(members))
		for _, row := range members {
			cleanupAgentSession(d, row.SessionName)
			names = append(names, row.SessionName)
		}
		// Kill the tmux sessions (best effort, idempotent).
		KillSessionsByNames(names)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prism] warning: CleanupReviewSessionsForParent: DB group error for %q: %v — using name-prefix fallback\n", parentSession, err)
	}
	// Pre-migration fallback: find rows by name prefix and kill by prefix.

	// Find all review session rows in the DB.
	rows, err := d.AllStatusesWithPrefix(prefix)
	if err == nil {
		for _, row := range rows {
			cleanupAgentSession(d, row.SessionName)
		}
	}

	// Kill the tmux sessions (best effort, idempotent).
	KillSessionPrefix(prefix)
}

// cleanupAgentSession cleans up the DB state for a completed agent session.
//
// In addition to releasing the port and marking the row ended, this transitions
// the state to "error" when the row is non-terminal. That matters for the
// review monitor's GroupCompleted check (#1051 AC-6): a half-alive agent
// stuck at "idle" would otherwise block the group's terminal-state count
// forever. State="error" is a valid agent state machine transition from any
// non-terminal state and is treated as terminal by GroupCompleted.
func cleanupAgentSession(d *db.DB, agentSession string) {
	st, lookupErr := d.CurrentStatus(agentSession)
	if lookupErr == nil && st != nil && !isTerminalAgentState(st.State) {
		_ = d.UpsertStatus(agentSession, st.Repo, st.Worktree, "error", nil, nil)
	}
	_ = d.ReleasePort(agentSession)
	_ = d.SetEnded(agentSession)
	_ = d.PurgeBusMessages(agentSession)
}

// IsPerAgentSession returns true if the given session name matches the new
// per-agent session shape: <parent>~review-<N>-<agent-name>.
// This is used to distinguish new-model sessions from old-shape round sessions.
func IsPerAgentSession(sessionName, parentSession string) bool {
	prefix := parentSession + "~review-"
	if !strings.HasPrefix(sessionName, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(sessionName, prefix)
	// Must have a dash separating the round number from the agent name.
	dashIdx := strings.Index(suffix, "-")
	if dashIdx <= 0 {
		return false
	}
	nStr := suffix[:dashIdx]
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		return false
	}
	// Agent portion must not contain '~' (old-shape marker).
	agentPart := suffix[dashIdx+1:]
	return agentPart != "" && !strings.Contains(agentPart, "~")
}
