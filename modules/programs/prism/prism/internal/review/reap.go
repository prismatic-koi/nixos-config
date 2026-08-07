package review

// reap.go — automatic release of finished review-agent sessions (issue #2649).
//
// # The leak
//
// A review round spawns five agent sessions. When the round completes, each
// agent's `agent_status` row reaches `finished`, but nothing releases the
// session: the tmux session stays alive, the harness port stays allocated, and
// `ended_at` stays NULL. The per-isolator concurrency cap counts rows with
// `ended_at IS NULL`, so every completed round permanently consumes five
// slots. A worker that runs three rounds leaks fifteen. The cap is global, so
// finished work starves live work, and the operator sees "at concurrency cap"
// while almost nothing is running.
//
// # What a reap does, and does not do
//
// A reap is the `prism close --keep-worktree` primitive applied to one review
// agent: kill the tmux session, kill the sidecar, drop the container's
// per-session temp state, release the harness port, stamp `ended_at`, and
// purge queued bus messages. It is the same per-agent teardown that
// `CleanupReviewSessionsForParent` already runs when the parent worker is
// cleaned up, so the reaper introduces no new teardown behaviour — only a new
// trigger for it.
//
// A reap never:
//
//   - deletes an `agent_status`, `sessions`, `agent_events`, or `session_groups`
//     row. `prism retro` and the retro flow read historical review data, so the
//     rows must survive. The reap writes `ended_at` and clears `harness_port`;
//     it deletes nothing.
//   - removes a worktree. A review agent has no worktree of its own — it
//     inherits the parent's (see `isSafeToRemoveWorktree` in cmd/cleanup.go and
//     issue #2638) — so there is nothing here that could remove one. No git
//     command runs on this path at all.
//   - deletes a branch. Same reason. #2638 / PR #2639 fixed the descendant
//     branch-deletion defect in `prism cleanup`; the reaper sidesteps that path
//     entirely by never calling cleanup.
//
// # Why the reaper cannot reach a live agent (#2613)
//
// The candidate query (`db.ReapableReviewAgents`) gates on
// `session_groups.delivered_at`, which is written exactly once, by the monitor,
// after the review-complete prompt has been accepted for the round (#2259).
// While a round is running, delivered_at is NULL for its group, so NO member of
// that round is a candidate — not even a member that already reached
// `finished` while its four siblings are still working. Group-level gating,
// not per-session gating, is what makes reaching a live agent impossible.
//
// Three further conditions narrow the set: the row must not already be ended,
// its state must be terminal, and the grace period must have elapsed. The
// session-name shape is re-checked in Go before any teardown runs, so a
// hand-written `session_groups` row cannot make a worker or a coordinator into
// a candidate.

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ReapGracePeriod is how long a review agent stays live after its round's
// review-complete prompt is delivered. It is the single source of truth for
// the number quoted in prose; the four prose sites are listed on
// TestReapGracePeriod_MatchesDocumentedValue, which fails when this constant
// changes.
//
// The documented post-round read — `prism checkin <parent>~review-<N>-<agent>`
// — does not depend on this window. `prism checkin` renders `agent_events`
// rows out of the prism DB, and the reap preserves every one of them, so the
// read keeps working for the life of the database. The grace period protects
// the surfaces that DO need a live session: the `runCheckinSessionLegacy`
// tmux screen-scrape fallback, `tmux attach`, and an operator reading the pane
// directly straight after a round.
//
// Fifteen minutes is long enough for the worker's next turn and for an
// operator who reacts to the review-complete notification, and short enough
// that a worker never holds more than one round's worth of finished agents.
const ReapGracePeriod = 15 * time.Minute

// reviewAgentNamePattern is the defence-in-depth shape check applied to every
// candidate before it is torn down: `<parent>~review-<N>-<role>`.
//
// The SQL join to session_groups already restricts candidates to review-group
// members, so this is a second, independent reason a non-review session cannot
// be reaped. It also excludes investigator sessions
// (`<invoker>~investigate-<slug>`), which own no session_groups row today but
// must stay out of scope if that ever changes — an investigator does not
// self-terminate and is the invoker's to clean up.
var reviewAgentNamePattern = regexp.MustCompile(`^.+~review-\d+-.+$`)

// Test seams. Production wires these to the real side effects; tests replace
// them so the suite never signals a host tmux server, a host sidecar process,
// or the container temp-file tree. Keeping the seams here (rather than
// exporting the whole teardown) means the reap ordering under test is the
// production ordering.
var (
	reapKillTmuxSession = func(name string) { _ = tmux.KillSession(name) }
	reapKillSidecar     = session.KillSidecar
	reapRemoveContainer = removeReviewAgentContainer
)

// reapContainerBudget bounds the per-agent container teardown. It matches the
// 30-second per-child budget `stopAndRemoveChildContainers` uses in
// cmd/cleanup.go, so a reap and a parent cleanup cost the same in the worst
// case.
const reapContainerBudget = 30 * time.Second

// ReapResult reports what one sweep did. Counts, not errors: every step of a
// reap is best-effort, and a sweep that partially fails must not abort the
// caller (a review spawn, or the monitor's post-delivery wait).
type ReapResult struct {
	// Reaped names the sessions that were torn down, in the order processed.
	Reaped []string
	// Skipped names candidates rejected by the Go-side shape re-check.
	Skipped []string
}

// ReapDeliveredReviewAgents releases every review-agent session that has been
// terminal since before `now - grace`, in a round whose review-complete prompt
// has already been delivered.
//
// Pass groupID to scope the sweep to one round — the monitor does this for the
// round it has just delivered. Pass "" to sweep every group, which is what the
// pre-spawn sweep in `prism review` does: the concurrency cap is global, so one
// worker's leaked agents starve every other worker, and a per-parent sweep
// would not reclaim them.
//
// grace of zero or less uses ReapGracePeriod. Errors reading the candidate set
// are returned; errors tearing an individual candidate down are logged and the
// sweep continues, so one wedged tmux session cannot block the rest.
func ReapDeliveredReviewAgents(d *db.DB, groupID string, now time.Time, grace time.Duration) (ReapResult, error) {
	var res ReapResult
	if d == nil {
		return res, fmt.Errorf("reap: nil database handle")
	}
	if grace <= 0 {
		grace = ReapGracePeriod
	}

	cutoff := now.Add(-grace).UnixMilli()
	candidates, err := d.ReapableReviewAgents(groupID, cutoff)
	if err != nil {
		return res, fmt.Errorf("reap: list candidates: %w", err)
	}
	for _, c := range candidates {
		// Defence in depth. The query cannot return a non-review session
		// today, so this branch is unreachable through the supported write
		// paths — it exists so that a future session kind registered against
		// session_groups, or a hand-edited row, is skipped loudly rather than
		// torn down silently.
		if !reviewAgentNamePattern.MatchString(c.SessionName) {
			proglog.Warnf("[prism reap] refusing to reap %q: name is not a review-agent session (group %s) — skipping\n",
				c.SessionName, c.GroupID)
			res.Skipped = append(res.Skipped, c.SessionName)
			continue
		}
		reapOne(d, c)
		res.Reaped = append(res.Reaped, c.SessionName)
	}
	if len(res.Reaped) > 0 {
		proglog.Infof("[prism reap] released %d finished review agent(s) after the %s grace period\n",
			len(res.Reaped), grace)
	}
	return res, nil
}

// reapOne applies the `prism close --keep-worktree` teardown to a single
// review agent. Every step is best-effort and idempotent.
//
// The order matters: kill the process-side state first (tmux pane, sidecar,
// container temp files) and write the DB last. If the process dies between
// steps, the next sweep still sees `ended_at IS NULL` and retries the whole
// sequence. Writing `ended_at` first would mark the row reaped while the tmux
// session was still alive, and no later sweep would revisit it.
func reapOne(d *db.DB, c db.ReapCandidate) {
	proglog.Infof("[prism reap] releasing %s (state=%s, group=%s, parent=%s)\n",
		c.SessionName, c.State, c.GroupID, c.ParentSession)

	reapKillTmuxSession(c.SessionName)
	reapKillSidecar(c.SessionName)
	reapRemoveContainer(c.SessionName, c.IsolationMode)

	// Release the harness port. A row whose port is already NULL is a
	// no-op success; a missing row returns an error, which cannot happen
	// here because the candidate came from that same table.
	if err := d.ReleasePort(c.SessionName); err != nil {
		proglog.Warnf("[prism reap] warning: release port for %q: %v\n", c.SessionName, err)
	}
	// Stamp ended_at. This is the step that returns the concurrency slot:
	// Isolator.Cap counts agent_status rows with ended_at IS NULL.
	if err := d.SetEnded(c.SessionName); err != nil {
		proglog.Warnf("[prism reap] warning: stamp ended_at for %q: %v\n", c.SessionName, err)
	}
	if err := d.PurgeBusMessages(c.SessionName); err != nil {
		proglog.Warnf("[prism reap] warning: purge bus messages for %q: %v\n", c.SessionName, err)
	}
}

// removeReviewAgentContainer drops the per-session container state for a
// review agent, dispatching on its persisted isolation mode. For `bwrap` and
// `sandbox-exec` this is temp-file cleanup; for `host` it is a no-op.
//
// It mirrors `stopAndRemoveChildContainers` in cmd/cleanup.go, including the
// fall back to bwrap for a row whose isolation_mode column is empty or names
// a mode the registry does not know.
func removeReviewAgentContainer(sessionName, isolationMode string) {
	mode := config.IsolationBwrap
	if isolationMode != "" {
		mode = config.IsolationMode(isolationMode)
	}
	name := container.NameForSession(sessionName)
	iso, err := container.For(mode, container.ConstructorOpts{Name: name})
	if err != nil {
		if iso, err = container.For(config.IsolationBwrap, container.ConstructorOpts{Name: name}); err != nil {
			proglog.Warnf("[prism reap] warning: unknown isolation mode %q for %q: %v\n",
				isolationMode, sessionName, err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), reapContainerBudget)
	defer cancel()
	iso.EnsureRemoved(ctx, nil)
}

// reapSleep and reapNow are the clock seams used by ReapGroupAfterGrace.
// Tests replace the pair with a fake clock so the post-delivery path is
// exercised without a real 15-minute wall-clock wait, while keeping the
// production relationship intact: the sweep reads the clock AFTER the wait
// has advanced it.
var (
	reapSleep = time.Sleep
	reapNow   = time.Now
)

// ReapGroupAfterGrace waits out the grace period and then reaps groupID.
//
// It is the "on round completion" trigger the monitor uses: the monitor
// process already exists per round and has nothing left to do after delivery,
// so hosting the wait there costs one idle process per in-flight round and
// needs no timer daemon, no cron entry, and no new lifecycle surface.
//
// The wait is deliberate. Reaping the instant the prompt is delivered would
// take the tmux session away while the worker is still reading the round, and
// would leave an operator who reacts to the notification with nothing to
// attach to.
//
// A monitor that is killed during the wait leaks its round. That is why the
// pre-spawn sweep in `prism review` runs unscoped — it is the backstop that
// collects any round whose monitor did not survive, including rounds belonging
// to other workers.
func ReapGroupAfterGrace(d *db.DB, groupID string, grace time.Duration) {
	if grace <= 0 {
		grace = ReapGracePeriod
	}
	proglog.Infof("[prism reap] group %s delivered — releasing its agents in %s\n", groupID, grace)
	reapSleep(grace)
	// The wait itself is what satisfies the grace period: delivered_at was
	// written just before it started, so by now `now - delivered_at >= grace`
	// holds by construction. Re-applying grace to the sweep is deliberate
	// belt-and-braces — a monitor whose wait returned early (a stubbed sleep, a
	// signal) must not reap ahead of the documented window.
	res, err := ReapDeliveredReviewAgents(d, groupID, reapNow(), grace)
	if err != nil {
		proglog.Warnf("[prism reap] warning: post-delivery reap of group %s: %v\n", groupID, err)
		return
	}
	proglog.Infof("[prism reap] group %s: released %d agent(s)\n", groupID, len(res.Reaped))
}
