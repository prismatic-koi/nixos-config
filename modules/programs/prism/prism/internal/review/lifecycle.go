package review

// lifecycle.go — session lifecycle helpers for review rounds.
//
// This file contains functions that manage the tmux and DB lifecycle of review
// sessions: round numbering, session killing, cleanup, and per-agent session
// detection. These helpers are called from Run(), RunAsync(), and the cleanup
// command — they have no dependency on prompt or result formatting.

import (
	"context"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// NextRoundNumber returns the next round number for the given parent session.
// It queries the DB for all per-agent sessions (new shape: ~review-<N>-<agent>)
// so that the count is accurate even after previous rounds have been cleaned up.
// Returns 1 when no prior rounds exist.
//
// Old-shape round sessions (~review-<N> with pure integer suffix) are NOT
// counted. They must not affect the counter. Old-shape agent sub-sessions
// (~review-<N>~<agent>) are also excluded.
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

// killTmuxSession is the tmux kill of KillSessionsByNames. Tests replace it,
// so that a test can act as the session-closed hook.
var killTmuxSession = tmux.KillSession

// KillSessionsByNames kills the specified tmux sessions by exact name.
func KillSessionsByNames(names []string) {
	for _, name := range names {
		_ = killTmuxSession(name)
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
			proglog.Warnf("[prism] warning: KillReviewSessionsForParentWithDB: DB error for %q: %v — using name-prefix fallback\n", parentSession, err)
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
//
// It stops the sidecar of each review session first, on every platform.
// It returns the sessions whose sidecar did not stop. For those it changes
// nothing else: the tmux session, the row, and the port stay. The caller
// must pass them to db.SetEndedExcept when it ends the parent, because the
// db.SetEnded cascade would end their rows too.
//
// It also removes the stored fallback plan and the monitor log of each round
// of the parent (RemoveMonitorOptsForParent, RemoveMonitorLogsForParent).
//
// parentInstanceID is the instance ID of the parent incarnation that the
// archive step of the caller copies. The caller reads it before the tmux
// kill of the parent, because the session-closed hook can clear
// agent_status.instance_id of the parent first. The audit logs of the
// children go to the log of that incarnation. Pass "" when the tmux session
// of the parent still runs: the hand-over then reads agent_status.
func CleanupReviewSessionsForParent(d *db.DB, parentSession, parentInstanceID string) (failed []string) {
	prefix := parentSession + "~review-"
	defer RemoveMonitorOptsForParent(d, parentSession)
	defer RemoveMonitorLogsForParent(d, parentSession)

	// Try DB-backed group membership first (post-migration rows).
	members, err := d.GroupMembersForParent(parentSession)
	if err == nil && len(members) > 0 {
		// DB-backed: clean up only the actual group members.
		names := make([]string, 0, len(members))
		for _, row := range members {
			names = append(names, row.SessionName)
		}
		var stopped []string
		stopped, failed = stopAgentSidecars(names)
		for _, name := range stopped {
			cleanupAgentSession(d, name, parentInstanceID, db.ReapCauseParentCleanup)
		}
		// Kill the tmux sessions (best effort, idempotent).
		KillSessionsByNames(stopped)
		return failed
	}
	if err != nil {
		proglog.Warnf("[prism] warning: CleanupReviewSessionsForParent: DB group error for %q: %v — using name-prefix fallback\n", parentSession, err)
	}
	// Pre-migration fallback: find rows by name prefix and kill by prefix.

	// Find all review session rows in the DB.
	rows, err := d.AllStatusesWithPrefix(prefix)
	if err == nil {
		names := make([]string, 0, len(rows))
		for _, row := range rows {
			names = append(names, row.SessionName)
		}
		var stopped []string
		stopped, failed = stopAgentSidecars(names)
		for _, name := range stopped {
			cleanupAgentSession(d, name, parentInstanceID, db.ReapCauseParentCleanup)
		}
		if len(failed) > 0 {
			// A prefix kill would also close the tmux session of a review
			// agent whose sidecar still runs.
			KillSessionsByNames(stopped)
			return failed
		}
	}

	// Kill the tmux sessions (best effort, idempotent).
	KillSessionPrefix(prefix)
	return failed
}

// RemoveMonitorOptsForParent removes the stored monitor opts file (the
// fallback plan) of each review round of parentSession. Errors are logged,
// not returned: a file that stays on disk does not stop the cleanup.
func RemoveMonitorOptsForParent(d *db.DB, parentSession string) {
	groups, err := d.GroupsForParent(parentSession)
	if err != nil {
		proglog.Warnf("[prism] warning: list review groups of %q: %v\n", parentSession, err)
		return
	}
	for _, g := range groups {
		if rmErr := RemoveMonitorOpts(g.GroupID); rmErr != nil {
			proglog.Warnf("[prism] warning: remove monitor opts of group %s: %v\n", g.GroupID, rmErr)
		}
	}
}

// StopReviewChildrenForEnd prepares db.SetEnded(parent). That call also
// ends every open `<parent>~review-*` row, so each of those rows must lose
// its sidecar first. For each open child row it stops the sidecar and then
// closes the tmux session of the child. It returns the children whose
// sidecar still runs. Pass them to db.SetEndedExcept, so that their rows and
// their ports stay reserved.
func StopReviewChildrenForEnd(d *db.DB, parent string) []string {
	rows, err := d.AllStatusesWithPrefix(parent + "~review-")
	if err != nil {
		proglog.Warnf("[prism] warning: list review children of %q: %v\n", parent, err)
		return nil
	}
	var open []string
	for _, row := range rows {
		if row.EndedAt == nil {
			open = append(open, row.SessionName)
		}
	}
	stopped, failed := stopAgentSidecars(open)
	for _, name := range stopped {
		reapKillTmuxSession(name)
	}
	return failed
}

// stopAgentSidecars stops the sidecars of names at the same time, so that
// several slow stops cost one stop timeout. It returns the names in their
// input order, split by the result.
func stopAgentSidecars(names []string) (stopped, failed []string) {
	ok := make([]bool, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			ok[i] = stopAgentSidecar(name)
		}(i, name)
	}
	wg.Wait()
	for i, name := range names {
		if ok[i] {
			stopped = append(stopped, name)
		} else {
			proglog.Warnf("[prism] warning: sidecar of %q did not stop — keeping its tmux session, row, and port\n", name)
			failed = append(failed, name)
		}
	}
	return stopped, failed
}

// cleanupFailedAgentSession is the cleanup of a review agent whose spawn
// or readiness gate failed. It stops the sidecar first, then runs
// cleanupAgentSession and closes the tmux session. If the sidecar does not
// stop, it keeps the tmux session, the row, and the port, and only marks
// the row "error", so that the round can complete. The release stops the
// sidecar of that row later (#3168).
func cleanupFailedAgentSession(d *db.DB, agentSession string, cause db.SessionReapCause, detail string) {
	if !stopAgentSidecar(agentSession) {
		proglog.Warnf("[prism] warning: sidecar of %q did not stop — keeping its tmux session, row, and port\n", agentSession)
		_, _ = markAgentSessionError(d, agentSession, cause, detail)
		return
	}
	cleanupAgentSession(d, agentSession, "", cause, detail)
	_ = tmux.KillSession(agentSession)
}

// markAgentSessionError records cause and moves a non-terminal row to state
// "error". It does not end the row. It returns the row as it read it.
func markAgentSessionError(d *db.DB, agentSession string, cause db.SessionReapCause, detail ...string) (*db.Status, error) {
	st, lookupErr := d.CurrentStatus(agentSession)
	if lookupErr == nil && st != nil && st.EndedAt == nil {
		d.RecordReapBestEffort(agentSession, cause, detail...)
	}
	if lookupErr == nil && st != nil && !isTerminalAgentState(st.State) {
		_ = d.UpsertStatus(agentSession, st.Repo, st.Worktree, "error", nil, nil)
	}
	return st, lookupErr
}

// cleanupAgentSession cleans up the DB state for a completed agent session.
//
// In addition to releasing the port and marking the row ended, this transitions
// the state to "error" when the row is non-terminal. That matters for the
// review monitor's GroupCompleted check: a half-alive agent
// stuck at "idle" would otherwise block the group's terminal-state count
// forever. State="error" is a valid agent state machine transition from any
// non-terminal state and is treated as terminal by GroupCompleted.
//
// cause names the path that is closing the row. It is recorded as a
// session_reaped event so the review report and a coordinator reading the DB
// can both name one cause instead of guessing between the paths that leave
// state="error" behind. detail is optional free text; pass "" when the cause
// alone is the whole story.
//
// The record is guarded on ended_at, exactly like the state write below it and
// like db.SetEnded's own `AND ended_at IS NULL`. Without the guard this helper
// claims a close it did not perform: `prism cleanup` of a parent worker
// cascades through CleanupReviewSessionsForParent, whose GroupMembersForParent
// returns every member row across every round — including rows a readiness
// gate closed hours earlier. Recording unconditionally would stamp
// `parent_cleanup` over that row's real `readiness_gate` cause, and
// SessionEndCauses returns the latest event, so the report would name a cause
// that is false. A wrong cause is worse than the disjunction this all
// replaces.
//
// parentInstanceID is as for CleanupReviewSessionsForParent.
func cleanupAgentSession(d *db.DB, agentSession, parentInstanceID string, cause db.SessionReapCause, detail ...string) {
	st, _ := markAgentSessionError(d, agentSession, cause, detail...)
	_ = d.ReleasePort(agentSession)
	_ = d.SetEnded(agentSession)
	_ = d.PurgeBusMessages(agentSession)
	// Remove the child's own instance-ID-keyed directory trees (work dir,
	// TMPDIR, podman-proxy audit dir, and prism-container audit dir). Every
	// parent-cleanup path routes through this function for each review-agent
	// child, so this is the single place that closes the gap where a child
	// session's directories otherwise outlive its parent's cleanup (issue
	// #2960). The removals are non-fatal and idempotent — a host-isolation
	// child has no work dir, and a child that never used containers has no
	// audit dir.
	//
	// The audit log of a child that used `prism container` goes to the
	// parent, because a review agent has no archive of its own. Its builds,
	// containers, and images are then swept.
	for _, childID := range childIncarnationIDs(d, agentSession, st) {
		sweepChildPrismContainers(agentSession, childID, handOverChildPrismContainerAudit(d, agentSession, childID, parentInstanceID))
		container.RemoveSessionWorkDir(childID)
		container.RemoveSessionTmpDir(childID)
		container.RemovePodmanProxyAuditDir(childID)
	}
}

// childIncarnationIDs returns the instance ID in st and the instance ID of
// each sessions row of agentSession, with no duplicates. The sessions rows
// are necessary: when the tmux session of the child closes outside prism,
// the session-closed hook clears agent_status.instance_id, and this can
// occur before cleanupAgentSession runs (#3279).
func childIncarnationIDs(d *db.DB, agentSession string, st *db.Status) []string {
	var ids []string
	add := func(id string) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if st != nil && st.InstanceID != nil {
		add(*st.InstanceID)
	}
	sessions, err := d.SessionsByName(agentSession)
	if err != nil {
		proglog.Warnf("[prism] warning: cleanup: list incarnations of %q failed (%v) — cleaning up the current incarnation only\n", agentSession, err)
	}
	for _, s := range sessions {
		add(s.InstanceID)
	}
	return ids
}

// handOverChildPrismContainerAudit appends the prism-container audit log of
// a review-agent child to the log of incarnation parentInstanceID of its
// parent. An empty parentInstanceID selects the current incarnation in
// agent_status. Every caller that can see a real log runs before the
// parent's archive step, so the parent archive keeps the lines.
//
// It reports whether the child's audit dir can be removed: true when the
// child has no log or the append worked. On false the dir stays on disk,
// and a warning names it.
func handOverChildPrismContainerAudit(d *db.DB, agentSession, childID, parentInstanceID string) bool {
	if !container.PrismContainerAuditDirExists(childID) {
		return true
	}
	parent, _, _ := strings.Cut(agentSession, "~")
	if parentInstanceID == "" {
		if st, err := d.CurrentStatus(parent); err == nil && st != nil && st.InstanceID != nil {
			parentInstanceID = *st.InstanceID
		}
	}
	if parentInstanceID == "" {
		proglog.Warnf("[prism] warning: cleanup: keep the prism-container audit log of %q: parent %q has no instance ID\n", agentSession, parent)
		return false
	}
	if err := prismcontainer.AppendAuditLog(childID, parentInstanceID); err != nil {
		proglog.Warnf("[prism] warning: cleanup: keep the prism-container audit log of %q: append to parent %q failed (%v)\n", agentSession, parent, err)
		return false
	}
	return true
}

// childPrismContainerRunner and childPrismContainerBuildExecutor run podman
// for sweepChildPrismContainers. A nil executor selects the executor of
// the host platform.
var (
	childPrismContainerRunner        prismcontainer.Runner = prismcontainer.ExecRunner{}
	childPrismContainerBuildExecutor prismcontainer.BuildExecutor
)

// SetChildPrismContainerRunnerForTest replaces the podman runner and the
// build executor of the review-child sweep until the returned restore
// runs. Tests in other packages use it, so that a cleanup test never runs
// the real podman or systemctl. Production code must not call it.
func SetChildPrismContainerRunnerForTest(r prismcontainer.Runner, e prismcontainer.BuildExecutor) (restore func()) {
	prevR, prevE := childPrismContainerRunner, childPrismContainerBuildExecutor
	childPrismContainerRunner, childPrismContainerBuildExecutor = r, e
	return func() { childPrismContainerRunner, childPrismContainerBuildExecutor = prevR, prevE }
}

// sweepChildPrismContainers stops the `prism container` builds and removes
// the containers and images of one review-agent child. With removeDir, it
// then removes the audit dir of the child, but only after a successful
// sweep. It issues no podman command when the child has no prism-container
// audit dir.
func sweepChildPrismContainers(agentSession, instanceID string, removeDir bool) {
	if !container.PrismContainerAuditDirExists(instanceID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	executor := childPrismContainerBuildExecutor
	if executor == nil {
		executor = prismcontainer.DefaultBuildExecutor(runtime.GOOS, childPrismContainerRunner, nil)
	}
	var err error
	if removeDir {
		err = prismcontainer.RemoveAuditDir(ctx, childPrismContainerRunner, executor, instanceID)
	} else {
		err = prismcontainer.SweepSession(ctx, childPrismContainerRunner, executor, []string{instanceID})
	}
	if err != nil {
		proglog.Warnf("[prism] warning: cleanup: prism-container sweep for %q failed (%v) — continuing cleanup\n", agentSession, err)
	}
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
