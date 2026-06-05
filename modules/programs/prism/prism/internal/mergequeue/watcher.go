// Package mergequeue implements the local serial merge queue for prism (#783).
//
// The Watcher is a goroutine started by the coordinator's sidecar on init.
// It polls the head of the pending_merges queue for the sidecar's session_name
// and drives PRs through the merge lifecycle using the GitHub CLI:
//
//   - CLEAN   → gh pr merge --squash
//   - BEHIND  → gh pr update-branch
//   - DIRTY   → fail with "merge conflicts"
//   - BLOCKED → fail on CI failure or review required, keep watching otherwise
//   - Others  → keep watching (transient)
//
// On each terminal outcome (merged, failed, closed) a bus notification is
// delivered to the enqueuing coordinator session via the harness HTTP API.
// The notification text names the PR, includes the worker session's archive
// path, and prompts git pull + prism cleanup.
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

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
)

const (
	// PollInterval is how often the watcher ticks.
	PollInterval = 45 * time.Second
)

// requiredChecksCache holds a cached list of required status check names
// fetched from branch protection, together with an expiry timestamp.
type requiredChecksCache struct {
	names   []string
	fetchAt time.Time
}

// requiredChecksCacheTTL is how long we reuse a cached branch-protection
// response before re-fetching. Five minutes is short enough to pick up
// rule changes but long enough to avoid hammering the API on every tick.
const requiredChecksCacheTTL = 5 * time.Minute

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

	// requiredChecks caches the branch-protection required status check names
	// for the target branch. Populated lazily on the first UNSTABLE tick and
	// refreshed when the TTL expires.
	requiredChecks requiredChecksCache

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
	if err := w.db.UpdateMergeLastChecked(head.PR); err != nil {
		log.Printf("[mergequeue] UpdateMergeLastChecked PR #%d: %v", head.PR, err)
	}

	prInfo, err := w.fetchPRInfo(ctx, head.PR)
	if err != nil {
		log.Printf("[mergequeue] fetchPRInfo PR #%d: %v — leaving watching", head.PR, err)
		return
	}

	// PR closed without merging.
	if prInfo.State == "CLOSED" && !prInfo.isMerged() {
		msg := "PR was closed without merging"
		w.failAndNotify(head, msg)
		return
	}

	// PR already merged (shouldn't happen, but handle it gracefully).
	if prInfo.State == "MERGED" || prInfo.isMerged() {
		w.succeedAndNotify(ctx, head)
		return
	}

	switch prInfo.MergeStateStatus {
	case "CLEAN":
		w.tryMerge(ctx, head)

	case "BEHIND":
		log.Printf("[mergequeue] PR #%d is BEHIND — calling gh pr update-branch", head.PR)
		if err := w.updateBranch(ctx, head.PR); err != nil {
			log.Printf("[mergequeue] gh pr update-branch PR #%d: %v — leaving watching", head.PR, err)
		}
		// Leave row as watching; next tick will see UNSTABLE→BLOCKED→CLEAN cycle.

	case "DIRTY":
		w.failAndNotify(head, "merge conflicts")

	case "BLOCKED":
		// Disambiguate: CI failure, review required, changes requested, or still running.
		if hasCIFailure(prInfo.StatusCheckRollup) {
			w.failAndNotify(head, "CI failed")
		} else if prInfo.ReviewDecision == "REVIEW_REQUIRED" {
			w.failAndNotify(head, "human reviewer approval required before merge")
		} else if prInfo.ReviewDecision == "CHANGES_REQUESTED" {
			w.failAndNotify(head, "reviewer requested changes — fix and re-request review")
		} else {
			// Include the actual ReviewDecision string so any future unhandled
			// value (new GitHub enum addition, unexpected empty/"" state) surfaces
			// in the operator log rather than vanishing into a generic message.
			log.Printf("[mergequeue] PR #%d BLOCKED but no CI failure or known review requirement (reviewDecision=%q) — staying watching", head.PR, prInfo.ReviewDecision)
		}

	case "UNSTABLE":
		// UNSTABLE means the PR is mergeable but at least one check is
		// non-success. We fetch the required branch-protection checks and
		// merge if all of them are SUCCESS, ignoring optional/slow checks.
		required, err := w.fetchRequiredChecks(ctx)
		if err != nil {
			log.Printf("[mergequeue] PR #%d UNSTABLE — cannot fetch required checks: %v — leaving watching", head.PR, err)
			return
		}
		if requiredChecksAllPassed(prInfo.StatusCheckRollup, required) {
			log.Printf("[mergequeue] PR #%d UNSTABLE but all required checks passed — proceeding to merge", head.PR)
			w.tryMerge(ctx, head)
		} else {
			// Log which required checks are not yet SUCCESS.
			var pending []string
			passed := make(map[string]bool, len(prInfo.StatusCheckRollup))
			for _, c := range prInfo.StatusCheckRollup {
				name := checkName(c)
				if name == "" {
					continue
				}
				ok := strings.EqualFold(c.Conclusion, "SUCCESS") ||
					strings.EqualFold(c.State, "SUCCESS")
				passed[name] = ok
			}
			for _, req := range required {
				if ok, found := passed[req]; !found || !ok {
					pending = append(pending, req)
				}
			}
			log.Printf("[mergequeue] PR #%d UNSTABLE — required checks not yet SUCCESS: %v — staying watching", head.PR, pending)
		}

	case "UNKNOWN", "HAS_HOOKS", "DRAFT":
		log.Printf("[mergequeue] PR #%d %s — staying watching", head.PR, prInfo.MergeStateStatus)

	default:
		log.Printf("[mergequeue] PR #%d unknown mergeStateStatus=%q — staying watching", head.PR, prInfo.MergeStateStatus)
	}
}

// tryMerge attempts `gh pr merge --squash` on head. On success transitions to
// merged and notifies; on branch-moved race (exit 1 + "already merged" or
// "sha1" message) leaves watching; on other errors transitions to failed.
func (w *Watcher) tryMerge(ctx context.Context, head *db.PendingMerge) {
	log.Printf("[mergequeue] PR #%d CLEAN — attempting gh pr merge --squash", head.PR)
	out, err := w.runGH(ctx, "pr", "merge", fmt.Sprintf("%d", head.PR), "--squash")
	if err == nil {
		log.Printf("[mergequeue] PR #%d merged successfully", head.PR)
		w.succeedAndNotify(ctx, head)
		return
	}

	// First: reconcile by checking the PR's actual state. Some "errors" are
	// races where the PR did merge — trusting the observed state is more
	// reliable than pattern-matching error strings.
	if state := w.checkPRMergedState(ctx, head.PR); state == "MERGED" {
		log.Printf("[mergequeue] PR #%d merge mutation errored but PR is MERGED — reconciling as success", head.PR)
		w.succeedAndNotify(ctx, head)
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

// succeedAndNotify transitions head to merged and notifies the coordinator.
func (w *Watcher) succeedAndNotify(ctx context.Context, head *db.PendingMerge) {
	// Capture mergedAt at the same instant TerminateMerge persists it, so the
	// spawn_outcome.pr_merged_at write reflects the canonical wall-clock the
	// merge-queue row carries. Using time.Now() here (and inside TerminateMerge)
	// rather than gh's mergedAt field is a deliberate trade-off: gh-supplied
	// timestamps drift in the BLOCKED → CLEAN race window, and the operator
	// view of "when did we merge it" is the moment the watcher transitioned
	// the row, not the moment GitHub recorded the squash commit.
	mergedAt := time.Now().UnixMilli()
	if err := w.db.TerminateMerge(head.PR, "merged", ""); err != nil {
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

	// Look up the worker session's archive_path from the sessions table.
	archivePath := w.lookupWorkerArchivePath(head)
	var notifyText string
	if archivePath != "" {
		notifyText = fmt.Sprintf(
			"PR #%d merged. Archive: %s. Run `git pull` in @main and `prism cleanup` the worker session.",
			head.PR, archivePath,
		)
	} else {
		notifyText = fmt.Sprintf(
			"PR #%d merged. Run `git pull` in @main and `prism cleanup` the worker session.",
			head.PR,
		)
	}
	log.Printf("[mergequeue] %s", notifyText)
	w.notify(ctx, head.SessionName, head.InstanceID, notifyText)
}

// failAndNotify transitions head to failed and notifies the coordinator.
func (w *Watcher) failAndNotify(head *db.PendingMerge, errMsg string) {
	if err := w.db.TerminateMerge(head.PR, "failed", errMsg); err != nil {
		log.Printf("[mergequeue] TerminateMerge(failed) PR #%d: %v", head.PR, err)
	}
	var notifyText string
	switch errMsg {
	case "merge conflicts":
		notifyText = fmt.Sprintf("PR #%d has merge conflicts — worker rebase needed", head.PR)
	case "CI failed":
		notifyText = fmt.Sprintf("PR #%d CI failed — needs worker fix", head.PR)
	case "PR was closed without merging":
		notifyText = fmt.Sprintf("PR #%d was closed without merging — removed from queue", head.PR)
	case "human reviewer approval required before merge":
		notifyText = fmt.Sprintf("PR #%d is blocked — human reviewer approval required before merge", head.PR)
	case "reviewer requested changes — fix and re-request review":
		notifyText = fmt.Sprintf("PR #%d is blocked — reviewer requested changes — fix and re-request review", head.PR)
	default:
		notifyText = fmt.Sprintf("PR #%d merge failed: %s", head.PR, errMsg)
	}
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
const prInfoJSONFields = "state,mergedAt,mergeStateStatus,statusCheckRollup,reviewDecision"

// prInfo holds the fields we care about from `gh pr view --json`.
type prInfo struct {
	State             string       `json:"state"`
	MergedAt          *string      `json:"mergedAt"`
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

type checkEntry struct {
	// Name is the check-run name (e.g. "pr-gate") or the commit-status context.
	// GitHub's statusCheckRollup uses "name" for check-runs and "context" for
	// legacy commit statuses; we populate both via the Name field and fall back
	// to Context when Name is empty.
	Name       string `json:"name"`
	Context    string `json:"context"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
	// State is the legacy commit-status field (e.g. "SUCCESS", "FAILURE").
	// Modern check-runs use Conclusion instead.
	State string `json:"state"`
}

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

// hasCIFailure returns true when at least one check in rollup has
// conclusion = FAILURE.
func hasCIFailure(rollup []checkEntry) bool {
	for _, c := range rollup {
		if strings.EqualFold(c.Conclusion, "FAILURE") {
			return true
		}
	}
	return false
}

// checkName returns the canonical name for a rollup entry, preferring the
// check-run Name field and falling back to the legacy commit-status Context.
func checkName(c checkEntry) string {
	if c.Name != "" {
		return c.Name
	}
	return c.Context
}

// requiredChecksAllPassed returns true iff every name in required has a
// corresponding entry in rollup with a successful conclusion.
//
// "Success" means:
//   - modern check-run: Conclusion == "SUCCESS" (case-insensitive)
//   - legacy commit-status: State == "SUCCESS" (case-insensitive)
//
// A required name that is missing from rollup entirely is treated as
// not-yet-passed and returns false.
func requiredChecksAllPassed(rollup []checkEntry, required []string) bool {
	if len(required) == 0 {
		// No required checks configured — treat as not-gated (conservative:
		// don't block forever, but callers should detect this upstream).
		return false
	}
	// Build a name→success map from the rollup.
	passed := make(map[string]bool, len(rollup))
	for _, c := range rollup {
		name := checkName(c)
		if name == "" {
			continue
		}
		// A check is successful if either the modern conclusion or the
		// legacy state field indicates SUCCESS.
		ok := strings.EqualFold(c.Conclusion, "SUCCESS") ||
			strings.EqualFold(c.State, "SUCCESS")
		passed[name] = ok
	}
	for _, req := range required {
		ok, found := passed[req]
		if !found || !ok {
			return false
		}
	}
	return true
}

// branchProtectionResponse is the subset of the GitHub branch protection API
// response we care about.
type branchProtectionResponse struct {
	RequiredStatusChecks struct {
		Contexts []string `json:"contexts"` // legacy commit-status names
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"` // modern check-run names
	} `json:"required_status_checks"`
}

// fetchRequiredChecks returns the list of required status check names for the
// protected target branch ("main"), using a short-TTL in-memory cache to
// avoid hammering the API on every polling tick.
//
// On any error (network, permissions, rate-limit) it returns nil, err so the
// caller can take the conservative path of staying watching.
func (w *Watcher) fetchRequiredChecks(ctx context.Context) ([]string, error) {
	now := w.now()
	if len(w.requiredChecks.names) > 0 && now.Before(w.requiredChecks.fetchAt.Add(requiredChecksCacheTTL)) {
		// Cache hit.
		return w.requiredChecks.names, nil
	}

	out, err := w.runGH(ctx, "api", fmt.Sprintf("repos/%s/branches/main/protection", w.repo))
	if err != nil {
		return nil, fmt.Errorf("gh api branch protection: %w: %s", err, strings.TrimSpace(string(out)))
	}

	var resp branchProtectionResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse branch protection response: %w", err)
	}

	// Collect names from both the legacy contexts list and the modern checks
	// list, deduplicating.
	seen := make(map[string]bool)
	var names []string
	for _, ctx := range resp.RequiredStatusChecks.Contexts {
		if ctx != "" && !seen[ctx] {
			seen[ctx] = true
			names = append(names, ctx)
		}
	}
	for _, c := range resp.RequiredStatusChecks.Checks {
		if c.Context != "" && !seen[c.Context] {
			seen[c.Context] = true
			names = append(names, c.Context)
		}
	}

	w.requiredChecks = requiredChecksCache{
		names:   names,
		fetchAt: now,
	}
	log.Printf("[mergequeue] fetched required checks for %s: %v", w.repo, names)
	return names, nil
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

// execGH is the real gh-CLI invocation. Extracted from runGH so tests can
// substitute Watcher.runGHFunc without losing the production code path.
func execGH(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	return cmd.CombinedOutput()
}
