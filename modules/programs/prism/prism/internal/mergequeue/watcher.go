// Package mergequeue implements the local serial merge queue for prism (#783).
//
// The Watcher is a goroutine started by the coordinator's sidecar on init.
// It polls the head of the pending_merges queue for the sidecar's session_name
// and drives PRs through the merge lifecycle using the GitHub CLI:
//
//   - CLEAN    → gh pr merge --squash
//   - UNSTABLE → gh pr merge --squash, but only when every required check
//     reports SUCCESS; optional checks are ignored
//   - BEHIND   → gh pr update-branch
//   - BLOCKED  → terminate as failed when every required check has concluded
//     and at least one concluded in a failure state (#2525); keep watching
//     otherwise, because an outstanding approval or a still-running check can
//     resolve on its own
//   - Others   → keep watching (transient)
//
// On each terminal outcome (merged, failed, closed) a bus notification is
// delivered to the enqueuing coordinator session via the harness HTTP API.
// Every notification names the PR. The merge and close outcomes add the worker
// session's archive path and prompt git pull + prism cleanup. The
// required-check failure outcome deliberately omits the cleanup prompt,
// because the branch is still needed for the fix (#2525).
//
// Watcher lifetime equals coordinator session lifetime. On sidecar shutdown,
// all watching rows for this instance_id are transitioned to 'abandoned' by
// AbandonWatchingMerges, preventing a future sidecar from inheriting stale rows.
package mergequeue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/branchprotect"
	"github.com/prismatic-koi/prism/internal/checkstate"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/forge"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
)

const (
	// PollInterval is how often the watcher ticks. Reduced from 45s to 30s
	// as part of the async-poll-and-notify redesign (#2420) so coordinators
	// see merge outcomes more quickly after human action lands.
	PollInterval = 30 * time.Second
)

// protectionCache holds a cached snapshot of the branch-protection state for
// the target branch, together with an expiry timestamp.
//
// configured reports whether the branch is protected at all — the
// GitHub API returns HTTP 404 when no protection is configured, which the
// watcher treats as a distinct state ("no protection, wait for a human") per
// the #2420 redesign, NOT as a merge licence.
//
// requiredChecks is meaningful only when configured is true; it holds the
// deduplicated list of required status check names (both legacy commit-
// status contexts and modern check-run names).
type protectionCache struct {
	configured     bool
	requiredChecks []string
	fetchAt        time.Time
}

// protectionCacheTTL is how long we reuse a cached branch-protection
// response before re-fetching. Five minutes is short enough to pick up
// rule changes but long enough to avoid hammering the API on every tick.
const protectionCacheTTL = 5 * time.Minute

// Watcher runs the merge-queue polling loop for a single coordinator session.
type Watcher struct {
	db          *db.DB
	instanceID  string
	sessionName string
	httpClient  *http.Client

	// repo is the GitHub owner/name slug (e.g. "prismatic-koi/nixos-config")
	// resolved once at construction time from the coordinator session's
	// worktree. Every gh invocation routes through w.runGH which prepends
	// "--repo <repo>" so calls succeed regardless of the sidecar's CWD (#1055).
	// An empty string means resolution failed — Run() logs and exits cleanly
	// rather than entering a poll loop that can never succeed.
	repo string

	// protection caches the branch-protection snapshot (configured yes/no plus
	// the required status check names when configured) for the target branch.
	// Populated lazily on the first tick that needs it and refreshed when the
	// TTL expires.
	protection protectionCache

	// nowFunc returns the current time. Tests may override this to control TTL
	// expiry without sleeping.
	nowFunc func() time.Time

	// runGHFunc is the function used to invoke the gh CLI. It is configurable
	// so tests can inject a stub that captures argv. When nil, runGH falls back
	// to execGH (the real os/exec implementation).
	runGHFunc func(ctx context.Context, args ...string) ([]byte, error)
}

// New creates a Watcher. instanceID and sessionName identify the coordinator
// session that owns this watcher. httpClient is used for notification delivery;
// pass nil to use the default client.
//
// New resolves the GitHub owner/name slug once, using the worktree path
// recorded for sessionName in agent_status. Subsequent gh invocations are
// pinned to that slug via "--repo", so the watcher works even when the sidecar
// process CWD is not a git repo (#1055). If resolution fails (no row in
// agent_status, missing worktree, or `gh repo view` errors), repo is left
// empty and a warning is logged; Run() will then exit cleanly without polling.
func New(database *db.DB, instanceID, sessionName string, httpClient *http.Client) *Watcher {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	w := &Watcher{
		db:          database,
		instanceID:  instanceID,
		sessionName: sessionName,
		httpClient:  httpClient,
	}
	w.repo = resolveRepo(database, sessionName)
	return w
}

// resolveRepo looks up the coordinator session's worktree path in agent_status
// and runs `gh repo view --json nameWithOwner -q .nameWithOwner` from that
// directory to obtain the GitHub owner/name slug. Returns "" on any failure
// (no agent_status row, empty worktree, gh error, etc.) and logs a single
// warning naming the cause.
func resolveRepo(database *db.DB, sessionName string) string {
	status, err := database.CurrentStatus(sessionName)
	if err != nil {
		log.Printf("[mergequeue] cannot resolve repo for session %q: agent_status lookup failed: %v — watcher will not start", sessionName, err)
		return ""
	}
	if status == nil {
		log.Printf("[mergequeue] cannot resolve repo for session %q: no agent_status row — watcher will not start", sessionName)
		return ""
	}
	worktree := strings.TrimSpace(status.Worktree)
	if worktree == "" {
		log.Printf("[mergequeue] cannot resolve repo for session %q: agent_status.worktree is empty — watcher will not start", sessionName)
		return ""
	}

	// GitLab guardrail (#2669): the merge queue is GitHub-only. Detect a
	// gitlab.com origin remote and skip cleanly — no repo resolves, so Run()
	// logs once and returns without ever calling gh against a repo it cannot
	// resolve. This is a silent skip, not an error: no notification is sent.
	if forge.IsGitLabDir(worktree) {
		log.Printf("[mergequeue] skipping watcher for session %q: origin is a gitlab.com remote — the merge queue watcher does not support GitLab (#2669)", sessionName)
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner")
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[mergequeue] cannot resolve repo for session %q from worktree %q: gh repo view: %v: %s — watcher will not start",
			sessionName, worktree, err, strings.TrimSpace(string(out)))
		return ""
	}
	repo := strings.TrimSpace(string(out))
	if repo == "" {
		log.Printf("[mergequeue] cannot resolve repo for session %q from worktree %q: gh repo view returned empty output — watcher will not start",
			sessionName, worktree)
		return ""
	}
	log.Printf("[mergequeue] resolved repo for session %q: %s", sessionName, repo)
	return repo
}

// Run starts the polling loop and blocks until ctx is cancelled.
// It is safe to run in a goroutine.
//
// If the watcher has no resolved repo (resolution failed at New() time), Run
// logs a single warning and returns immediately. The poll loop never starts,
// so the goroutine exits cleanly and the coordinator session remains usable
// for non-merge-queue work. (#1055)
func (w *Watcher) Run(ctx context.Context) {
	if w.repo == "" {
		log.Printf("[mergequeue] watcher NOT started for session %q (instance=%s): owner/name resolution failed at startup — see prior log line", w.sessionName, w.instanceID)
		return
	}
	log.Printf("[mergequeue] watcher started (instance=%s, repo=%s)", w.instanceID, w.repo)
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	// Run once immediately on startup, then on each tick.
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Printf("[mergequeue] watcher stopped (instance=%s)", w.instanceID)
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick processes the head of the queue once.
//
// The state machine mirrors the #2420 redesign: the watcher only produces
// a coordinator notification for five terminal events —
//
//   - the watcher squash-merged the PR successfully (`mergeOutcomePrismDriven`),
//   - the PR was merged out-of-band by a human or another tool
//     (`mergeOutcomeExternal`),
//   - the PR was closed without merging,
//   - a genuine `gh pr merge` mutation failure that was not a branch-moved race,
//     and
//   - the PR is BLOCKED and a required check has concluded in a failure state
//     (#2525) — the one BLOCKED sub-state that cannot resolve without a new
//     push, so silent polling would hang the queue forever.
//
// Every other observed state (checks pending, review pending, changes
// requested, new commits pushed, unprotected repo, branch behind base) results
// in a silent continuation — the poller keeps polling and the coordinator's
// initial-invocation message already told them what to wait for.
//
// Critically, an unprotected repo (branch-protection API returns HTTP 404) is
// NEVER auto-merged, even when GitHub's mergeStateStatus is CLEAN. Absence of
// protection is not a licence to merge — see the #2420 rationale.
func (w *Watcher) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	head, err := w.db.MergeQueueHead(w.sessionName)
	if err != nil {
		log.Printf("[mergequeue] db error reading head: %v", err)
		return
	}
	if head == nil {
		// Queue is empty — no-op.
		return
	}

	log.Printf("[mergequeue] polling head PR #%d (queue_pos=%d)", head.PR, head.QueuePosition)
	// Pass head.Repo so the heartbeat update only touches the row belonging
	// to this coordinator's repo, never a same-numbered row from another
	// repo sharing the same prism.db (issue #2354).
	if err := w.db.UpdateMergeLastChecked(head.PR, head.Repo); err != nil {
		log.Printf("[mergequeue] UpdateMergeLastChecked PR #%d: %v", head.PR, err)
	}

	prInfo, err := w.fetchPRInfo(ctx, head.PR)
	if err != nil {
		log.Printf("[mergequeue] fetchPRInfo PR #%d: %v — leaving watching", head.PR, err)
		return
	}

	// Terminal: PR observed as MERGED on a fresh poll. The merge was
	// performed by an external actor (human via GitHub UI, another tool,
	// or a tab racing the watcher) — prism did NOT execute the merge
	// mutation. Notify with the out-of-band message.
	if prInfo.State == "MERGED" || prInfo.isMerged() {
		w.succeedAndNotify(ctx, head, mergeOutcomeExternal, prInfo)
		return
	}

	// Terminal: PR closed without merging.
	if prInfo.State == "CLOSED" && !prInfo.isMerged() {
		w.notifyClosedNotMerged(ctx, head)
		return
	}

	// Branch-protection gate (#2420 core rule): NEVER auto-merge on a repo
	// without branch protection. A 404 on the protection endpoint is a
	// description of state, not a licence.
	prot, err := w.fetchProtection(ctx)
	if err != nil {
		log.Printf("[mergequeue] PR #%d branch-protection fetch failed: %v — staying watching", head.PR, err)
		return
	}
	if !prot.configured {
		log.Printf("[mergequeue] PR #%d has no branch protection — staying watching for human review/approve/merge", head.PR)
		return
	}

	// Branch protection IS configured. Trust GitHub's mergeStateStatus
	// (which reflects the protection rules — required checks, required
	// reviews, up-to-date-with-base) as the positive signal for CLEAN, and
	// use the required-checks list to disambiguate UNSTABLE (optional
	// checks failing but required ones green).
	switch prInfo.MergeStateStatus {
	case "CLEAN":
		w.tryMerge(ctx, head)

	case "UNSTABLE":
		// UNSTABLE means the PR is mergeable but at least one check is
		// non-success. We merge iff all REQUIRED checks are SUCCESS,
		// ignoring optional/slow checks that will never gate the merge
		// under the repo's protection rules.
		if requiredChecksAllPassed(prInfo.StatusCheckRollup, prot.requiredChecks) {
			log.Printf("[mergequeue] PR #%d UNSTABLE but all required checks passed — proceeding to merge", head.PR)
			w.tryMerge(ctx, head)
		} else {
			log.Printf("[mergequeue] PR #%d UNSTABLE — required checks not yet SUCCESS — staying watching silently", head.PR)
		}

	case "BEHIND":
		log.Printf("[mergequeue] PR #%d is BEHIND — calling gh pr update-branch", head.PR)
		if err := w.updateBranch(ctx, head.PR); err != nil {
			log.Printf("[mergequeue] gh pr update-branch PR #%d: %v — leaving watching", head.PR, err)
		}
		// Leave row as watching; next tick will pick up the rebased state.

	case "BLOCKED":
		// #2525: BLOCKED covers two states that are not alike.
		//
		//  1. A required check still runs, or a human approval is
		//     outstanding. GitHub resolves this on its own. Silent polling
		//     is correct.
		//  2. Every required check has concluded and at least one concluded
		//     in a failure state. Nothing resolves this without a new push,
		//     so silent polling hangs the queue until the coordinator
		//     session ends. The operator, not the coordinator, then finds
		//     the dead row.
		//
		// failedRequiredChecks separates the two. It returns a non-empty
		// list only when the required-check set is completely accounted for
		// in the rollup AND a real failure conclusion is present, so the
		// documented BLOCKED-to-CLEAN drift window between mergeStateStatus
		// and statusCheckRollup cannot produce a spurious failure.
		//
		// This adds a terminal FAILURE transition only. It adds no merge
		// path — the #2420 rule that prism never merges without a positive
		// signal is untouched.
		if failed := failedRequiredChecks(prInfo.StatusCheckRollup, prot.requiredChecks); len(failed) > 0 {
			w.notifyRequiredChecksFailed(ctx, head, failed)
			return
		}
		log.Printf("[mergequeue] PR #%d BLOCKED but no required check has conclusively failed — staying watching silently", head.PR)

	case "DIRTY", "UNKNOWN", "HAS_HOOKS", "DRAFT":
		// #2420: none of these are coordinator-actionable terminal events
		// mid-poll. DIRTY may resolve when the worker rebases; UNKNOWN is a
		// transient GitHub side effect; DRAFT and HAS_HOOKS are the
		// worker's responsibility, not prism's. Keep polling silently — the
		// initial-invocation message told the coordinator what to expect.
		log.Printf("[mergequeue] PR #%d %s — staying watching silently", head.PR, prInfo.MergeStateStatus)

	default:
		log.Printf("[mergequeue] PR #%d unknown mergeStateStatus=%q — staying watching silently", head.PR, prInfo.MergeStateStatus)
	}
}

// notifyClosedNotMerged transitions the head row to terminal (status='failed'
// per the existing DB schema) and delivers the closed-without-merge
// notification. The notification text uses the #2420 completion-message
// discipline: it contains "Please clean up the branch and worktree" and does
// NOT imply prism performed the cleanup itself.
func (w *Watcher) notifyClosedNotMerged(ctx context.Context, head *db.PendingMerge) {
	// Scoped by head.Repo per issue #2354.
	if err := w.db.TerminateMerge(head.PR, head.Repo, "failed", "PR was closed without merging"); err != nil {
		log.Printf("[mergequeue] TerminateMerge(closed) PR #%d: %v", head.PR, err)
	}
	text := fmt.Sprintf("PR #%d closed without merge. Please clean up the branch and worktree.", head.PR)
	log.Printf("[mergequeue] %s", text)
	w.notify(ctx, head.SessionName, head.InstanceID, text)
}

// notifyRequiredChecksFailed transitions the head row to terminal
// (status='failed') and tells the coordinator which required checks failed
// (#2525).
//
// The transition is what stops the poll loop: MergeQueueHead only ever
// returns rows with status='watching', so once this row is 'failed' it is no
// longer the head and no later tick re-observes it. That is also what bounds
// the notification to exactly one per PR.
//
// The text deliberately does NOT carry the "Please clean up the branch and
// worktree" phrase that every completion message carries. Nothing merged and
// nothing closed — the branch still holds the work and the worker needs it to
// push the fix.
//
// failed must be non-empty; the caller guarantees this.
func (w *Watcher) notifyRequiredChecksFailed(ctx context.Context, head *db.PendingMerge, failed []string) {
	names := joinFailedCheckNames(failed)
	// Scoped by head.Repo per issue #2354.
	if err := w.db.TerminateMerge(head.PR, head.Repo, "failed", "CI failed: "+names); err != nil {
		log.Printf("[mergequeue] TerminateMerge(ci-failed) PR #%d: %v", head.PR, err)
	}
	text := renderRequiredChecksFailedText(head.PR, names)
	log.Printf("[mergequeue] %s", text)
	w.notify(ctx, head.SessionName, head.InstanceID, text)
}

// renderRequiredChecksFailedText composes the required-check-failure
// notification (#2525). names is the already-joined, already-bounded list
// produced by joinFailedCheckNames.
//
// The wording follows the #2420 message discipline: name the PR, name what
// failed, name who acts next. It must never contain "Please clean up the
// branch and worktree" — see notifyRequiredChecksFailed.
func renderRequiredChecksFailedText(pr int, names string) string {
	return fmt.Sprintf(
		"PR #%d CI failed: %s. Worker needs to fix and push. No merge will happen until then.",
		pr, names,
	)
}

// maxFailedCheckNamesLen bounds the rendered failed-check name list. A repo
// can require an arbitrary number of checks, and the list lands in both the
// pending_merges.error column and the coordinator notification.
const maxFailedCheckNamesLen = 400

// joinFailedCheckNames renders check names as a comma-separated list, capped
// at maxFailedCheckNamesLen bytes. Truncation drops any partial UTF-8 rune at
// the cut point rather than emitting invalid UTF-8.
func joinFailedCheckNames(names []string) string {
	joined := strings.Join(names, ", ")
	if len(joined) <= maxFailedCheckNamesLen {
		return joined
	}
	return strings.ToValidUTF8(joined[:maxFailedCheckNamesLen], "") + "..."
}

// tryMerge attempts `gh pr merge --squash` on head. On success transitions to
// merged and notifies; on branch-moved race (exit 1 + "already merged" or
// "sha1" message) leaves watching; on other errors transitions to failed.
func (w *Watcher) tryMerge(ctx context.Context, head *db.PendingMerge) {
	log.Printf("[mergequeue] PR #%d CLEAN — attempting gh pr merge --squash", head.PR)
	out, err := w.runGH(ctx, "pr", "merge", fmt.Sprintf("%d", head.PR), "--squash")
	if err == nil {
		log.Printf("[mergequeue] PR #%d merged successfully", head.PR)
		w.succeedAndNotify(ctx, head, mergeOutcomePrismDriven, nil)
		return
	}

	// First: reconcile by checking the PR's actual state. Some "errors" are
	// races where the PR did merge — trusting the observed state is more
	// reliable than pattern-matching error strings. This is still a
	// prism-driven merge (we issued the mutation; it errored but the merge
	// took effect), so the notification distinguishes the reconciliation
	// breadcrumb from the external-merge case (#2298).
	if state := w.checkPRMergedState(ctx, head.PR); state == "MERGED" {
		log.Printf("[mergequeue] PR #%d merge mutation errored but PR is MERGED — reconciling as success", head.PR)
		w.succeedAndNotify(ctx, head, mergeOutcomeReconciled, nil)
		return
	}

	// Second: existing branch-moved-race keyword check (transient races
	// where the PR didn't merge but will likely merge on retry).
	combinedOutput := strings.ToLower(string(out) + err.Error())
	if isBranchMovedRace(combinedOutput) {
		log.Printf("[mergequeue] PR #%d branch-moved race detected — leaving watching for next tick", head.PR)
		return
	}

	// Third: genuine failure.
	errMsg := fmt.Sprintf("gh pr merge failed: %s", strings.TrimSpace(string(out)))
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	log.Printf("[mergequeue] PR #%d merge failed: %v", head.PR, errMsg)
	w.failAndNotify(head, errMsg)
}

// checkPRMergedState queries the PR's current state from GitHub and returns
// the state string (e.g. "MERGED", "OPEN", "CLOSED"). Returns "" if the
// query fails or the response cannot be parsed. This is called on the error
// path of tryMerge to reconcile cases where the mutation errored but the PR
// actually merged (e.g. "Merge already in progress" races).
func (w *Watcher) checkPRMergedState(ctx context.Context, pr int) string {
	out, err := w.runGH(ctx, "pr", "view", fmt.Sprintf("%d", pr), "--json", "state")
	if err != nil {
		return "" // unknown — fall through to existing paths
	}
	var v struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return ""
	}
	return v.State
}

// mergeOutcome identifies which of the three success paths called
// succeedAndNotify. The DB transition is identical for all three (the
// pending_merges row terminates as 'merged'); the values only steer the
// notification text rendered for the coordinator. Introduced in #2298 so the
// coordinator can distinguish an externally-merged PR (the watcher saw a
// fait-accompli MERGED state on a fresh poll) from a prism-driven merge.
type mergeOutcome int

const (
	// mergeOutcomePrismDriven — gh pr merge --squash returned err == nil and
	// the merge commit was created by prism's own mutation. Canonical case;
	// existing notification text is preserved byte-for-byte.
	mergeOutcomePrismDriven mergeOutcome = iota

	// mergeOutcomeExternal — fresh poll observed state=MERGED before prism
	// attempted any mutation. The merge was performed by a human via the
	// GitHub UI, by another tool, or by a tab racing the watcher. The
	// coordinator notification names the merger and does NOT instruct
	// `prism cleanup`, because prism did not perform the merge.
	mergeOutcomeExternal

	// mergeOutcomeReconciled — gh pr merge --squash returned a non-nil error
	// but checkPRMergedState revealed the PR is MERGED. This is the in-flight
	// race recovery from #1645 — the mutation took effect despite the error.
	// Still a prism-driven merge (we issued the mutation) so the coordinator
	// is told to `git pull` + `prism cleanup`, with a reconciliation
	// breadcrumb appended.
	mergeOutcomeReconciled
)

// succeedAndNotify transitions head to merged and notifies the coordinator.
// outcome selects which of the three notification variants is rendered (see
// mergeOutcome). prInfoForExternal supplies mergedBy/mergedAt when
// outcome == mergeOutcomeExternal; ignored otherwise. Pass nil when the
// caller does not have a prInfo (prism-driven and reconciled paths).
func (w *Watcher) succeedAndNotify(ctx context.Context, head *db.PendingMerge, outcome mergeOutcome, prInfoForExternal *prInfo) {
	// Capture mergedAt at the same instant TerminateMerge persists it, so the
	// spawn_outcome.pr_merged_at write reflects the canonical wall-clock the
	// merge-queue row carries. Using time.Now() here (and inside TerminateMerge)
	// rather than gh's mergedAt field is a deliberate trade-off: gh-supplied
	// timestamps drift in the BLOCKED → CLEAN race window, and the operator
	// view of "when did we merge it" is the moment the watcher transitioned
	// the row, not the moment GitHub recorded the squash commit.
	mergedAt := time.Now().UnixMilli()
	// Pass head.Repo so the terminal write only touches the row belonging
	// to this coordinator's repo, never a same-numbered row from another
	// repo sharing the same prism.db (issue #2354).
	if err := w.db.TerminateMerge(head.PR, head.Repo, "merged", ""); err != nil {
		log.Printf("[mergequeue] TerminateMerge(merged) PR #%d: %v", head.PR, err)
	}
	// Issue #2110: persist pr_merged_at on the worker's spawn_outcome row
	// alongside the notification. The write happens BEFORE w.notify is called
	// below so the notification firing path is unchanged — a write error logs
	// and continues, never blocking or delaying the notification.
	//
	// The worker is located by joining via spawn_outcome.pr_number, which the
	// worker-side capture path (events.go on `gh pr create` completion) wrote
	// at PR open. When the worker died before that capture fired, the lookup
	// returns "" and the update is a no-op — we lose pr_merged_at for that
	// session, but the merge still notifies.
	if iid, err := w.db.InstanceIDForPRNumber(head.PR); err != nil {
		log.Printf("[mergequeue] InstanceIDForPRNumber(PR #%d): %v", head.PR, err)
	} else if iid != "" {
		if err := w.db.UpdateSpawnOutcomePRMergedAt(iid, mergedAt); err != nil {
			log.Printf("[mergequeue] UpdateSpawnOutcomePRMergedAt(iid=%s, pr=%d): %v", iid, head.PR, err)
		} else {
			log.Printf("[mergequeue] persisted pr_merged_at on worker spawn_outcome (iid=%s, pr=%d)", iid, head.PR)
		}
	} else {
		log.Printf("[mergequeue] PR #%d: no worker spawn_outcome row carries pr_number=%d — skipping pr_merged_at persistence", head.PR, head.PR)
	}

	notifyText := w.renderSuccessNotifyText(head, outcome, prInfoForExternal)
	log.Printf("[mergequeue] %s", notifyText)
	w.notify(ctx, head.SessionName, head.InstanceID, notifyText)
}

// renderSuccessNotifyText composes the coordinator notification text for one
// of the three success outcomes (#2298, #2420). All three completion messages
// end with the discipline phrase "Please clean up the branch and worktree";
// none imply prism performed the cleanup itself. The prism-driven and
// reconciled variants surface the worker session's archive_path when one is
// recorded; the external (out-of-band) variant may omit the archive because
// prism did not perform the merge, but still ends with the cleanup phrase.
func (w *Watcher) renderSuccessNotifyText(head *db.PendingMerge, outcome mergeOutcome, prInfoForExternal *prInfo) string {
	switch outcome {
	case mergeOutcomeExternal:
		return renderExternalMergeNotifyText(head.PR, prInfoForExternal)
	case mergeOutcomeReconciled:
		archivePath := w.lookupWorkerArchivePath(head)
		if archivePath != "" {
			return fmt.Sprintf(
				"PR #%d merged. (Reconciled — gh mutation errored but PR is MERGED.) Archive: %s. Please clean up the branch and worktree.",
				head.PR, archivePath,
			)
		}
		return fmt.Sprintf(
			"PR #%d merged. (Reconciled — gh mutation errored but PR is MERGED.) Please clean up the branch and worktree.",
			head.PR,
		)
	default: // mergeOutcomePrismDriven
		archivePath := w.lookupWorkerArchivePath(head)
		if archivePath != "" {
			return fmt.Sprintf(
				"PR #%d merged. Archive: %s. Please clean up the branch and worktree.",
				head.PR, archivePath,
			)
		}
		return fmt.Sprintf(
			"PR #%d merged. Please clean up the branch and worktree.",
			head.PR,
		)
	}
}

// renderExternalMergeNotifyText composes the out-of-band merge notification
// text (#2298, updated for #2420). Fires when the poller observes a PR in
// MERGED state that prism did not merge itself — the merger is named by login
// where available, and a merge timestamp is included when present. The
// completion-message discipline requires the "Please clean up the branch and
// worktree" tail; the wording does NOT imply prism performed the cleanup.
func renderExternalMergeNotifyText(pr int, prInfo *prInfo) string {
	var login, mergedAt string
	if prInfo != nil {
		if prInfo.MergedBy != nil {
			login = strings.TrimSpace(prInfo.MergedBy.Login)
		}
		if prInfo.MergedAt != nil {
			mergedAt = strings.TrimSpace(*prInfo.MergedAt)
		}
	}

	var detail string
	switch {
	case login != "" && mergedAt != "":
		detail = fmt.Sprintf(" (merged by @%s at %s)", login, mergedAt)
	case login != "":
		detail = fmt.Sprintf(" (merged by @%s)", login)
	case mergedAt != "":
		detail = fmt.Sprintf(" (merged at %s)", mergedAt)
	default:
		detail = ""
	}

	return fmt.Sprintf(
		"PR #%d merged out-of-band%s. Please clean up the branch and worktree.",
		pr, detail,
	)
}

// failAndNotify transitions head to failed and notifies the coordinator.
//
// After the #2420 redesign, the polling state machine only reaches
// failAndNotify via the tryMerge error path (a genuine `gh pr merge --squash`
// failure that survived the reconciliation and branch-moved-race checks).
// The other cases that previously routed here now go elsewhere:
//
//   - closed without merging → notifyClosedNotMerged
//   - required check concluded in failure → notifyRequiredChecksFailed (#2525)
//   - BLOCKED awaiting review or approval, and merge conflicts → no terminal
//     transition at all; the poller stays watching silently
//
// See tick() for the routing, plus the #2420 and #2525 discipline.
func (w *Watcher) failAndNotify(head *db.PendingMerge, errMsg string) {
	// Pass head.Repo so the terminal write is scoped to this coordinator's
	// repo (issue #2354).
	if err := w.db.TerminateMerge(head.PR, head.Repo, "failed", errMsg); err != nil {
		log.Printf("[mergequeue] TerminateMerge(failed) PR #%d: %v", head.PR, err)
	}
	notifyText := fmt.Sprintf("PR #%d merge failed: %s", head.PR, errMsg)
	log.Printf("[mergequeue] %s", notifyText)
	w.notify(context.Background(), head.SessionName, head.InstanceID, notifyText)
}

// notify delivers a notification to the coordinator via the appropriate
// delivery path based on the harness type. For pi (TransportSocketPipe)
// coordinators, it routes through the host-API Unix socket (#1364). For
// HTTP harness coordinators, it uses the HTTP API (prompt_async).
// Failure is non-fatal (logged).
func (w *Watcher) notify(ctx context.Context, targetSession, targetInstanceID, text string) {
	status, err := w.db.CurrentStatus(targetSession)
	if err != nil || status == nil {
		log.Printf("[mergequeue] notify: cannot find coordinator session %q: %v", targetSession, err)
		return
	}

	// PI (socket-pipe) coordinators do not have an HTTP server —
	// route through the coordinator's host-API Unix socket instead (#1364).
	if status.Harness != nil {
		if shape, ok := harness.ShapeOf(*status.Harness); ok && shape == harness.TransportSocketPipe {
			log.Printf("[mergequeue] notify: routing via host-API socket for pi coordinator=%s", targetSession)
			// Use "followUp" so the coordinator receives the merge-queue outcome
			// after its current turn completes, even when it is mid-stream at
			// delivery time. Queue outcomes are post-turn signals.
			if deliverErr := promptdelivery.DeliverToSession(targetSession, status, text, buildNotifyBody, "", "followUp"); deliverErr != nil {
				log.Printf("[mergequeue] notify: FAILED (pi path) — coordinator=%s reason=%v", targetSession, deliverErr)
			} else {
				log.Printf("[mergequeue] notify: delivered to pi coordinator=%s via host-API socket", targetSession)
			}
			return
		}
	}

	// HTTP harness path: require harness port and session ID.
	if status.HarnessPort == nil {
		log.Printf("[mergequeue] notify: coordinator %q has no harness port", targetSession)
		return
	}
	port := *status.HarnessPort

	// Validate harness_session_id.
	if status.HarnessSessionID == nil || *status.HarnessSessionID == "" {
		log.Printf("[mergequeue] notify: coordinator %q has no harness_session_id", targetSession)
		return
	}
	sid := *status.HarnessSessionID

	body := buildNotifyBody(text, status)
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		log.Printf("[mergequeue] notify: marshal body: %v", err)
		return
	}

	url := fmt.Sprintf("http://localhost:%d/session/%s/prompt_async", port, sid)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		log.Printf("[mergequeue] notify: create request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		log.Printf("[mergequeue] notify: HTTP request: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		log.Printf("[mergequeue] notify: HTTP status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
		return
	}
	log.Printf("[mergequeue] notify: delivered to %s", targetSession)
}

// lookupWorkerArchivePath finds the most recent sessions row for the PR's
// worker branch (heuristic: session_name ends with "@<prNumber>").
// Returns the archive_path if found, or "".
//
// Scoping strategy (to avoid returning an archive from a different repo that
// happens to share the same PR number):
//
//  1. Primary path — when head.InstanceID is non-empty, look up the
//     coordinator session via its instance_id to obtain the authoritative
//     repo slug, then scope the session-name suffix query to that repo.
//
//  2. Legacy fallback — when head.InstanceID is empty (rows written before
//     instance_id was added to pending_merges), scope by w.repo directly.
//     A log line is emitted so the legacy path is visible in production logs.
func (w *Watcher) lookupWorkerArchivePath(head *db.PendingMerge) string {
	branchSuffix := fmt.Sprintf("@%d", head.PR)

	var repo string
	if head.InstanceID != "" {
		// Primary path: resolve the repo from the coordinator session that owns
		// this pending_merges row. This is the most precise scope — it ties the
		// worker-archive lookup to the exact coordinator instance that enqueued
		// the PR, preventing cross-repo collisions when two repos share a PR
		// number (e.g. nixos-config@782 vs myrepo@782).
		coordinator, err := w.db.SessionByInstanceID(head.InstanceID)
		if err == nil && coordinator != nil && coordinator.Repo != "" {
			repo = coordinator.Repo
		}
	}
	if repo == "" {
		// Legacy fallback: instance_id was not set on this pending_merges row
		// (pre-dates the instance_id column), or the coordinator session row is
		// missing. Scope by w.repo, which is resolved once at watcher startup.
		if head.InstanceID == "" {
			log.Printf("[mergequeue] lookupWorkerArchivePath PR #%d: PendingMerge has empty instance_id — using legacy repo fallback (repo=%q)", head.PR, w.repo)
		}
		repo = w.repo
	}

	var sessions []db.Session
	var err error
	if repo != "" {
		sessions, err = w.db.SessionsByNamePatternAndRepo(branchSuffix, repo)
	} else {
		// Last resort: no repo info available; fall back to unscoped lookup.
		sessions, err = w.db.SessionsByNamePattern(branchSuffix)
	}
	if err != nil || len(sessions) == 0 {
		return ""
	}
	// Pick the most recent session.
	best := sessions[0]
	for _, s := range sessions[1:] {
		if s.StartedAt.After(best.StartedAt) {
			best = s
		}
	}
	if best.ArchivePath != nil {
		return *best.ArchivePath
	}
	return ""
}

// buildNotifyBody constructs the prompt_async body.
func buildNotifyBody(text string, status *db.Status) map[string]any {
	body := map[string]any{
		"parts": []map[string]string{
			{"type": "text", "text": text},
		},
	}
	modelID := status.RootModelID
	if modelID == nil {
		modelID = status.ModelID
	}
	if modelID != nil {
		slashIdx := strings.Index(*modelID, "/")
		providerID := *modelID
		modelIDStr := ""
		if slashIdx >= 0 {
			providerID = (*modelID)[:slashIdx]
			modelIDStr = (*modelID)[slashIdx+1:]
		}
		body["model"] = map[string]string{
			"providerID": providerID,
			"modelID":    modelIDStr,
		}
	}
	return body
}

// prInfoJSONFields is the comma-separated list of fields requested from
// `gh pr view --json`. Exposed as a package-level constant so tests can assert
// the field list never regresses to use the (invalid) "merged" field again —
// `gh pr view` rejects unknown JSON fields with a non-zero exit, so an invalid
// field name silently breaks the entire merge-queue watcher (see #1014 fallout).
//
// `mergedBy` was added in #2298 so the external-merge notification can name the
// human (or bot) who clicked Squash-and-merge before the watcher's mutation.
const prInfoJSONFields = "state,mergedAt,mergedBy,mergeStateStatus,statusCheckRollup,reviewDecision"

// userRef is the shape `gh pr view --json mergedBy` returns: an object with a
// `login` field. Null in JSON unmarshals to a nil *userRef.
type userRef struct {
	Login string `json:"login"`
}

// prInfo holds the fields we care about from `gh pr view --json`.
type prInfo struct {
	State    string  `json:"state"`
	MergedAt *string `json:"mergedAt"`
	// MergedBy identifies the GitHub user who clicked Squash-and-merge. nil
	// when the PR is unmerged or when gh returned mergedBy=null. Consumed by
	// the external-merge notification path (#2298).
	MergedBy          *userRef     `json:"mergedBy"`
	MergeStateStatus  string       `json:"mergeStateStatus"`
	StatusCheckRollup []checkEntry `json:"statusCheckRollup"`
	// ReviewDecision is the PR's review decision from GitHub: "REVIEW_REQUIRED",
	// "APPROVED", "CHANGES_REQUESTED", or "" (empty when no review policy exists).
	ReviewDecision string `json:"reviewDecision"`
}

// isMerged reports whether the PR has been merged. `gh pr view` emits
// mergedAt as a non-null ISO-8601 timestamp string when the PR is merged, and
// null otherwise (which unmarshals to a nil pointer).
func (p *prInfo) isMerged() bool {
	return p.MergedAt != nil && *p.MergedAt != ""
}

// checkEntry is an alias for the shared checkstate.CheckEntry shape (#2527).
// The alias keeps every existing reference and test fixture in this package
// compiling unchanged while the classification logic itself lives in
// internal/checkstate, shared with cmd/merge.go's invocation-time probe.
type checkEntry = checkstate.CheckEntry

// fetchPRInfo calls `gh pr view <pr> --json` and returns the parsed result.
func (w *Watcher) fetchPRInfo(ctx context.Context, pr int) (*prInfo, error) {
	out, err := w.runGH(ctx, "pr", "view", fmt.Sprintf("%d", pr),
		"--json", prInfoJSONFields)
	if err != nil {
		return nil, fmt.Errorf("gh pr view: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var info prInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parse gh pr view output: %w", err)
	}
	return &info, nil
}

// failedRequiredChecks and requiredChecksAllPassed are thin wrappers over the
// shared internal/checkstate package (#2527). The classification logic
// itself — pending vs. concluded vs. failed, the BLOCKED-to-CLEAN drift-window
// handling, the FAILURE/TIMED_OUT/CANCELLED/ACTION_REQUIRED allowlist — lives
// there exactly once, shared with cmd/merge.go's invocation-time probe.
func failedRequiredChecks(rollup []checkEntry, required []string) []string {
	return checkstate.FailedRequiredChecks(rollup, required)
}

func requiredChecksAllPassed(rollup []checkEntry, required []string) bool {
	return checkstate.RequiredChecksAllPassed(rollup, required)
}

// fetchProtection returns the branch-protection snapshot for the protected
// target branch ("main"), using a short-TTL in-memory cache to avoid
// hammering the API on every polling tick.
//
// A configured=false result means neither the classic branch-protection
// endpoint nor the rulesets effective-rules endpoint found any protection
// (#2436) — the repo has no protection at all. Per the #2420 rule, callers
// must NOT auto-merge in that case, and this path is treated as a successful
// probe (no error returned), NOT as a transient failure.
//
// On any other error (network, permissions, rate-limit) fetchProtection
// returns a zero protectionCache and err so the caller can take the
// conservative path of staying watching without merging.
func (w *Watcher) fetchProtection(ctx context.Context) (protectionCache, error) {
	now := w.now()
	if !w.protection.fetchAt.IsZero() && now.Before(w.protection.fetchAt.Add(protectionCacheTTL)) {
		// Cache hit — includes cached "not configured" results so a
		// bootstrap repo doesn't retry on every tick.
		return w.protection, nil
	}

	// branchprotect.Probe issues `gh api ...` calls, and `gh api` rejects
	// the `--repo` flag ("unknown flag: --repo") — unlike `gh pr ...`
	// subcommands, which require it for CWD-independence (#1055). The paths
	// below are already fully-qualified (repos/<owner>/<repo>/...), so
	// routing through w.runGHNoRepo (not w.runGH) here is both correct and
	// required (#2438).
	res, err := branchprotect.Probe(ctx, w.runGHNoRepo,
		fmt.Sprintf("repos/%s/branches/main/protection", w.repo),
		fmt.Sprintf("repos/%s/rules/branches/main", w.repo),
	)
	if err != nil {
		return protectionCache{}, err
	}

	state := protectionCache{configured: res.Configured, requiredChecks: res.RequiredChecks, fetchAt: now}
	w.protection = state
	if res.Configured {
		log.Printf("[mergequeue] fetched branch protection for %s: required=%v", w.repo, res.RequiredChecks)
	} else {
		log.Printf("[mergequeue] branch protection not configured for %s (no classic protection or effective ruleset)", w.repo)
	}
	return state, nil
}

// fetchRequiredChecks is a thin adapter over fetchProtection that preserves
// the pre-#2420 return contract (names, err) for callers that only care about
// the required-checks list. When the branch is unprotected it returns (nil,
// nil) — the caller then decides whether "no gates configured" is safe.
//
// New tick-path code should call fetchProtection directly so the
// configured=false state is not silently discarded.
func (w *Watcher) fetchRequiredChecks(ctx context.Context) ([]string, error) {
	state, err := w.fetchProtection(ctx)
	if err != nil {
		return nil, err
	}
	return state.requiredChecks, nil
}

// now returns the current time. It uses w.nowFunc when set (for tests) and
// time.Now() otherwise.
func (w *Watcher) now() time.Time {
	if w.nowFunc != nil {
		return w.nowFunc()
	}
	return time.Now()
}

// isBranchMovedRace heuristically detects the GitHub branch-moved race from
// error output: the merge call fails because the head SHA changed between our
// CLEAN observation and the merge call. We treat this as transient.
func isBranchMovedRace(output string) bool {
	keywords := []string{
		"already merged",
		"pull request is not mergeable",
		"base branch was modified",
	}
	for _, kw := range keywords {
		if strings.Contains(output, kw) {
			return true
		}
	}
	return false
}

// updateBranch calls `gh pr update-branch <pr>` to rebase the PR onto main.
func (w *Watcher) updateBranch(ctx context.Context, pr int) error {
	out, err := w.runGH(ctx, "pr", "update-branch", fmt.Sprintf("%d", pr))
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runGH runs `gh --repo <owner/name> <args...>` and returns combined output.
// The "--repo" flag is prepended unconditionally so calls succeed regardless
// of the sidecar process's CWD (#1055). Uses a 30s timeout per call to avoid
// hanging the poll loop.
//
// runGH dispatches to w.runGHFunc when set (used by tests to inject a stub
// that captures argv), or falls back to execGH for real os/exec invocation.
func (w *Watcher) runGH(ctx context.Context, args ...string) ([]byte, error) {
	full := make([]string, 0, len(args)+2)
	full = append(full, "--repo", w.repo)
	full = append(full, args...)
	if w.runGHFunc != nil {
		return w.runGHFunc(ctx, full...)
	}
	return execGH(ctx, full...)
}

// runGHNoRepo runs `gh <args...>` WITHOUT the "--repo" flag and returns
// combined output. Used exclusively for `gh api ...` calls (the
// branch-protection probe), because `gh api` rejects "--repo" outright
// ("unknown flag: --repo") — unlike `gh pr ...` subcommands, which need it
// for CWD-independence (#1055). See runGH's doc comment for that path.
//
// Like runGH, this dispatches to w.runGHFunc when set (so tests can capture
// the exact argv handed to gh), falling back to execGH otherwise.
func (w *Watcher) runGHNoRepo(ctx context.Context, args ...string) ([]byte, error) {
	if w.runGHFunc != nil {
		return w.runGHFunc(ctx, args...)
	}
	return execGH(ctx, args...)
}

// execGH is the real gh-CLI invocation. Extracted from runGH so tests can
// substitute Watcher.runGHFunc without losing the production code path.
func execGH(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	return cmd.CombinedOutput()
}
