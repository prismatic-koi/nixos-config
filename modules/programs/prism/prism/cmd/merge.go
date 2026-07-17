package cmd

// prism merge <pr> — enqueue a PR for the local serial merge queue (#783).
//
// Coordinator-only: looks up the calling session's instance_id from the DB,
// pre-flight-validates that the PR exists and is open, then inserts a row into
// pending_merges with status = 'watching'. The sidecar's merge-queue watcher
// drives the merge lifecycle asynchronously.
//
// Idempotent: if the PR already has a non-terminal row (watching), the command
// returns success without inserting a duplicate.
//
// Worker sessions and review agents are denied: this command is restricted to
// coordinators.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
	"github.com/prismatic-koi/prism/internal/session"
)

var mergeCmd = &cobra.Command{
	Use:   "merge <pr>",
	Short: "Enqueue a PR for the local serial merge queue",
	Long: `Enqueue a PR for the coordinator's local serial merge queue.

The sidecar's merge-queue watcher drives the merge lifecycle asynchronously:
it polls the head of the queue, rebases when needed, merges when clean, and
notifies you via prompt when each PR completes (merged or failed).

This command is coordinator-only. Worker sessions must not invoke it.

Idempotent: calling prism merge <pr> a second time for an already-queued
(non-terminal) PR returns success without inserting a duplicate row.`,
	Args: cobra.ExactArgs(1),
	RunE: runMerge,
}

func init() {
	mergeCmd.Flags().Bool("wait", false, "Block until the merge queue reports a terminal state for this PR (synchronous mode). Without --wait, returns immediately and the watcher delivers a notification later via prism prompt.")
	mergeCmd.Flags().Duration("timeout", defaultMergeWaitTimeout, "Timeout for --wait. On expiry, exits non-zero with a status payload distinguishable from a real merge failure. Ignored when --wait is not set.")
	mergeCmd.Flags().Bool("json", false, "Emit the terminal status as a JSON object on stdout (only useful with --wait). Suppresses textual output.")
	rootCmd.AddCommand(mergeCmd)
}

func runMerge(cmd *cobra.Command, args []string) error {
	prArg := args[0]
	pr, err := strconv.Atoi(prArg)
	if err != nil || pr <= 0 {
		return fmt.Errorf("prism merge: invalid PR number %q — must be a positive integer", prArg)
	}
	waitFlag, _ := cmd.Flags().GetBool("wait")
	jsonFlag, _ := cmd.Flags().GetBool("json")
	timeoutFlag, _ := cmd.Flags().GetDuration("timeout")

	// Resolve the calling session's repo up front. Every downstream probe
	// and DB write is now repo-scoped (issue #2354) so we need this value
	// available before the re-entry short-circuit runs. When we cannot
	// resolve a repo (running outside a prism tmux session, DB error,
	// row not yet present) callerRepo stays empty and the probe falls
	// through as if no row exists — the enqueue path below re-derives
	// repo and produces a clear error if resolution is still impossible.
	callerRepo := resolveCallerRepo()

	// Re-entry short-circuit (#1875). Probe the merge ledger BEFORE running
	// the `gh pr view` preflight or hitting the sidecar /merge endpoint.
	// This is the cmd/-layer side of the DB-level idempotence in
	// EnqueueMerge: the DB does the right thing on duplicate inserts, but
	// the user-facing behaviour was misleading (a second `prism merge N`
	// reported "enqueued" as if it were fresh) and gratuitous network cost
	// was paid on every re-entry (one `gh pr view` round-trip per call).
	//
	//   - Terminal row, status == "merged" — short-circuit and print the
	//     recorded status. This is a true no-op: the PR is done.
	//   - Terminal row, status in {failed, cancelled, abandoned} — fall
	//     through. These are documented retry points in the coordinator
	//     flow (on CI failure / merge conflicts / closed-without-merging,
	//     the coordinator re-runs `prism merge <pr>` after the worker
	//     fixes the underlying issue). The DB layer's EnqueueMerge then
	//     treats the terminal row as gone and inserts a fresh `watching`
	//     row via its ON CONFLICT branch.
	//   - Non-terminal row (watching/...) — skip preflight, skip the
	//     duplicate enqueue, and print an "already in queue" message that is
	//     distinguishable from the fresh-enqueue line.
	//   - No row — fall through to the normal enqueue path.
	//
	// The lookup is scoped to callerRepo so a same-numbered row belonging
	// to a different repo cannot short-circuit our own enqueue (issue
	// #2354). Uses newWaitProbe() so the lookup works on both the host
	// (direct DB) and inside a sandbox (host-API proxy); a probe failure
	// falls through rather than erroring so the user still gets sensible
	// behaviour.
	if done, reentryErr := observeExistingMergeRow(pr, callerRepo, waitFlag, jsonFlag, timeoutFlag); done {
		return reentryErr
	}

	// Inside a bwrap sandbox: proxy the enqueue to the host sidecar (#1043).
	// The host's prism.db is invisible to direct DB writes from inside the
	// sandbox, so falling through to the DB path would silently write to a
	// shadow tmpfs DB that the merge-queue watcher never sees. We must NOT
	// silently fall back to the direct DB path on socket failure — that is the
	// exact behaviour this fix replaces. A clear error and non-zero exit is
	// the correct response (AC-6).
	//
	// Coordinator-only enforcement happens on the sidecar side via
	// requireCoordinator, which returns HTTP 403 for worker sessions. The
	// #2420 initial-state probe runs inside the sandbox (gh works there)
	// before the proxy call so invalid or terminal PRs do not pin sidecar
	// resources and the coordinator gets the state-table message immediately.
	if apiURL := sandboxenv.HostAPISocket(); apiURL != "" {
		decision, probeErr := probeInitialState(pr)
		if probeErr != nil {
			return fmt.Errorf("prism merge: %w", probeErr)
		}
		quietStdout := waitFlag && jsonFlag
		if done, dErr := emitInitialStateMessage(decision, quietStdout); done {
			return dErr
		}
		title := decision.Title
		var titlePtr *string
		if title != "" {
			titlePtr = &title
		}
		// The sidecar returns the full PendingMerge struct as JSON. Decode
		// only the fields we need for the user-facing message; the rest is
		// ignored. Field names match the Go struct exactly (default JSON
		// marshalling of internal/db.PendingMerge has no struct tags).
		var row struct {
			PR            int    `json:"PR"`
			QueuePosition int64  `json:"QueuePosition"`
			Status        string `json:"Status"`
		}
		if proxyErr := proxyToHostAPI(apiURL, "/merge", map[string]any{
			"pr":    pr,
			"title": titlePtr,
		}, &row); proxyErr != nil {
			return fmt.Errorf("prism merge: %w", proxyErr)
		}
		// JSON-exclusive contract (#1500): when --wait and --json are both
		// set, the terminal status is the only thing on stdout. Suppress
		// the textual enqueue-confirmation lines on that path so a
		// `--wait --json` consumer sees a single parseable JSON object.
		if !quietStdout {
			fmt.Printf("PR #%d enqueued (queue_position=%d, status=%s)\n", row.PR, row.QueuePosition, row.Status)
			if title != "" {
				fmt.Printf("  %s\n", title)
			}
		}
		if waitFlag {
			return waitForMergeTerminal(pr, callerRepo, jsonFlag, timeoutFlag)
		}
		fmt.Println("You will be notified when the outcome is decided.")

		fmt.Println()
		fmt.Println("Track progress with: prism merges")
		return nil
	}

	// Guard: coordinator-only.
	callerSession := review.LookupParentSession()
	d, dbErr := openDB()
	if dbErr != nil {
		return fmt.Errorf("prism merge: open db: %w", dbErr)
	}
	defer d.Close()

	if callerSession != "" {
		if !session.IsCoordinatorSession(callerSession, d) {
			return fmt.Errorf(`prism merge: this command is for coordinator sessions only.

Workers must not enqueue merges directly. Ask your coordinator to run:

  prism merge %d

See: modules/programs/prism/agents/coordinator.md`, pr)
		}
	}

	// Look up instance_id for the calling session. The sidecar mints
	// instance_id at startup, so it should always be present in the DB row.
	// If it is missing, the sidecar startup did not run correctly — return a
	// clear error rather than attempting on-the-fly recovery.
	if callerSession == "" {
		return fmt.Errorf("prism merge: cannot determine calling session — run from inside a prism tmux session or set PRISM_SESSION_NAME")
	}
	status, statusErr := d.CurrentStatus(callerSession)
	if statusErr != nil {
		return fmt.Errorf("prism merge: look up session %q: %w", callerSession, statusErr)
	}
	if status == nil || status.InstanceID == nil || *status.InstanceID == "" {
		return fmt.Errorf("prism merge: session %q has no instance_id — the sidecar did not start correctly", callerSession)
	}
	instanceID := *status.InstanceID

	sessionName := callerSession

	// Prefer the stored repo from agent_status over the session-name split
	// (which is a best-effort fallback in resolveCallerRepo). agent_status
	// is written at sidecar startup from the same source, but in tests the
	// two paths may diverge (e.g. seeded rows use different repo values),
	// and status.Repo is the authoritative record for this coordinator.
	repo := status.Repo
	if repo == "" {
		repo = callerRepo
	}
	if repo == "" {
		return fmt.Errorf("prism merge: cannot determine repo for session %q — agent_status.repo is empty and session name has no '@' prefix", callerSession)
	}

	// #2420 initial-state probe: describe what will happen, then enqueue
	// for non-terminal outcomes. Terminal outcomes (already merged, closed
	// without merge, merge conflict) short-circuit here — no row is written
	// to pending_merges. Timeout is kept tight (5s per gh call) so the
	// overall command returns well within the 2-second AC when the API
	// responds promptly.
	decision, err := probeInitialState(pr)
	if err != nil {
		return fmt.Errorf("prism merge: %w", err)
	}
	quietStdout := waitFlag && jsonFlag
	if done, dErr := emitInitialStateMessage(decision, quietStdout); done {
		return dErr
	}
	title := decision.Title

	// Enqueue (idempotent). Pass title for `prism merges list` display.
	// Repo is required so pending_merges rows are keyed on (repo, pr) and
	// PR numbers can safely collide across repos (issue #2354).
	var titlePtr *string
	if title != "" {
		titlePtr = &title
	}
	row, err := d.EnqueueMerge(pr, repo, sessionName, instanceID, titlePtr)
	if err != nil {
		return fmt.Errorf("prism merge: %w", err)
	}

	// JSON-exclusive contract (#1500): suppress the textual enqueue
	// confirmation when --wait and --json are both set.
	if !quietStdout {
		fmt.Printf("PR #%d enqueued (queue_position=%d, status=%s)\n", pr, row.QueuePosition, row.Status)
		if title != "" {
			fmt.Printf("  %s\n", title)
		}
	}
	if waitFlag {
		return waitForMergeTerminal(pr, repo, jsonFlag, timeoutFlag)
	}
	fmt.Println("You will be notified when the outcome is decided.")

	fmt.Println()
	fmt.Println("Track progress with: prism merges")
	return nil
}

// resolveCallerRepo returns the short repo slug for the calling prism
// session, or "" when it cannot be determined. This is the best-effort
// resolver used by the re-entry short-circuit (which happens BEFORE the
// main enqueue path opens the DB) so it must not error on missing state.
//
// The resolution order is:
//
//  1. review.LookupParentSession() — gives us the session name (either
//     from PRISM_SESSION_NAME or the current tmux session).
//  2. Split at the first '@' — the coordinator naming convention is
//     `<repo>@<branch>`, and the pre-migration backfill in
//     migrateV37ToV38 uses the same split, so this yields the same repo
//     slug agent_status.repo carries in every prism-managed session.
//
// The stronger source — status.Repo from agent_status — is used by the
// main enqueue path below (which opens the DB anyway) and overrides this
// fallback when available. This function exists purely so the re-entry
// probe can be repo-scoped without paying an extra DB open.
func resolveCallerRepo() string {
	caller := review.LookupParentSession()
	if caller == "" {
		return ""
	}
	if idx := strings.Index(caller, "@"); idx > 0 {
		return caller[:idx]
	}
	return ""
}

// observeExistingMergeRow is the cmd/-layer re-entry short-circuit for
// `prism merge` (#1875). It probes the pending_merges ledger before any
// `gh pr view` preflight or sidecar round-trip and decides whether the
// caller can be served entirely from the existing row.
//
// Returns (true, err) when the caller should return immediately with err
// (which may be nil for a successful no-op). Returns (false, nil) when the
// caller should continue with the normal preflight + enqueue path.
//
// On any probe error or open-DB failure, returns (false, nil) — this is a
// best-effort short-circuit, never a hard failure. Falling through means
// the caller pays the gh round-trip but still gets a correct outcome via
// the idempotent DB layer.
func observeExistingMergeRow(pr int, repo string, waitFlag, jsonMode bool, timeout time.Duration) (bool, error) {
	// A missing repo means we cannot safely scope the lookup — a match on
	// pr alone would re-open the exact cross-repo collision issue #2354
	// closed. Fall through to the normal enqueue path, which resolves the
	// repo authoritatively from agent_status and returns a clear error if
	// resolution is still impossible.
	if repo == "" {
		return false, nil
	}
	probe, err := newWaitProbe()
	if err != nil {
		return false, nil
	}
	defer probe.Close()
	row, err := probe.Merge(pr, repo)
	if err != nil || row == nil {
		return false, nil
	}
	if row.Status == "merged" {
		// Already merged: this is a true no-op. Print the recorded
		// status (sharing emitMergeWaitTerminal with --wait so the
		// human / JSON output shape is identical) and return.
		return true, emitMergeWaitTerminal(row, jsonMode)
	}
	if isMergeTerminalStatus(row.Status) {
		// Other terminal states (failed / cancelled / abandoned) are
		// retry points in the documented coordinator flow: on CI
		// failure, merge conflicts, or a closed-without-merging row,
		// the coordinator prompts the worker to fix and then re-runs
		// `prism merge <pr>` to re-enqueue. Falling through here lets
		// the normal preflight + EnqueueMerge path run — EnqueueMerge
		// treats a terminal row as gone (per its ON CONFLICT branch)
		// and inserts a fresh `watching` row.
		return false, nil
	}
	// Non-terminal: the PR is already in the queue. Skip preflight and the
	// duplicate-enqueue round-trip. With --wait, drop straight into the
	// poll loop. Without --wait, print an "already in queue" message that
	// is distinguishable from the fresh-enqueue line.
	quietStdout := waitFlag && jsonMode
	if !quietStdout {
		fmt.Printf("PR #%d already in queue (queue_position=%d, status=%s)\n", row.PR, row.QueuePosition, row.Status)
		if row.Title != nil && *row.Title != "" {
			fmt.Printf("  %s\n", *row.Title)
		}
	}
	if waitFlag {
		return true, waitForMergeTerminal(pr, repo, jsonMode, timeout)
	}
	return true, nil
}

// isMergeTerminalStatus mirrors internal/db.isMergeTerminal at the cmd
// layer so the short-circuit in observeExistingMergeRow does not need to
// import an unexported predicate. The set is small and stable; if a new
// terminal status is added to the DB layer, this list must be updated to
// match.
func isMergeTerminalStatus(status string) bool {
	switch status {
	case "merged", "failed", "cancelled", "abandoned":
		return true
	}
	return false
}

// observeAlreadyTerminal returns (true, err) when the given PR has an
// existing terminal row in pending_merges and --wait should return
// immediately with that status. Returns (false, nil) when the row is missing
// or non-terminal (the caller should proceed with the normal enqueue path).
// On DB / proxy errors, returns (false, nil) so the caller falls through to
// the regular path — best-effort short-circuit, never a hard failure.
//
// Uses newWaitProbe() so the lookup is correct both on the host (direct DB)
// and inside a sandbox (host-API proxy). Reading the host's prism.db
// directly from inside a sandbox would hit a shadow tmpfs DB the merge-queue
// watcher never writes to, silently returning "no row" and skipping the
// short-circuit — see issue #1500 review-code feedback for the parallel bug
// in the wait poll loop below.
//
// repo scopes the lookup so we cannot short-circuit on a same-numbered row
// from a different repo (issue #2354). An empty repo returns (false, nil).
func observeAlreadyTerminal(pr int, repo string, jsonMode bool) (bool, error) {
	if repo == "" {
		return false, nil
	}
	probe, err := newWaitProbe()
	if err != nil {
		return false, nil
	}
	defer probe.Close()
	row, err := probe.Merge(pr, repo)
	if err != nil || row == nil {
		return false, nil
	}
	switch row.Status {
	case "merged", "failed", "cancelled", "abandoned":
		return true, emitMergeWaitTerminal(row, jsonMode)
	}
	return false, nil
}

// waitForMergeTerminal polls the merge-queue ledger for the given PR until it
// reaches a terminal state (merged/failed/cancelled/abandoned), the timeout
// elapses, or the user interrupts. The merge-queue watcher (running in the
// host sidecar) writes the terminal row — this poll loop only observes it;
// killing this process does NOT cancel the merge.
//
// Sandbox-aware via newWaitProbe(): host shells read prism.db directly,
// in-sandbox callers route reads through the sidecar's /merges/by-pr
// endpoint. Without this, --wait inside a sandbox would poll a tmpfs shadow
// DB and never observe the terminal (issue #1500 review-code feedback).
//
// repo scopes each poll to the caller's repo so --wait cannot observe a
// terminal state belonging to a different repo's PR of the same number
// (issue #2354). An empty repo returns an error rather than polling
// unscoped — that would reintroduce exactly the incident this fix closes.
func waitForMergeTerminal(pr int, repo string, jsonMode bool, timeout time.Duration) error {
	if repo == "" {
		return fmt.Errorf("prism merge --wait: cannot determine repo for the calling session — --wait requires a resolvable repo to avoid cross-repo PR-number collisions (issue #2354)")
	}
	probe, err := newWaitProbe()
	if err != nil {
		return fmt.Errorf("prism merge --wait: %w", err)
	}
	defer probe.Close()

	var lastRow *db.PendingMerge
	err = pollWait(context.Background(), timeout,
		500*time.Millisecond, 5*time.Second,
		func() (bool, error) {
			row, qErr := probe.Merge(pr, repo)
			if qErr != nil {
				// Transient — keep polling.
				proglog.Debugf("[prism merge --wait] probe error: %v (will retry)\n", qErr)
				return false, nil
			}
			if row == nil {
				return false, nil
			}
			lastRow = row
			switch row.Status {
			case "merged", "failed", "cancelled", "abandoned":
				return true, emitMergeWaitTerminal(row, jsonMode)
			}
			return false, nil
		})
	// Translate the pollWait outcome. waitExitTimeout gets a structured
	// payload (distinguishable from a real merge failure per AC); other
	// outcomes propagate verbatim.
	if err != nil {
		switch exitCodeOf(err) {
		case waitExitTimeout:
			_ = emitMergeWaitTimeout(pr, lastRow, jsonMode, timeout)
			return newExitErr(waitExitTimeout, "")
		default:
			return err
		}
	}
	return nil
}

// exitCodeOf returns the ExitCode of err if it implements an ExitCode method,
// else 0. Avoids errors.As ceremony at every call site.
func exitCodeOf(err error) int {
	type exitCoder interface{ ExitCode() int }
	if err == nil {
		return 0
	}
	if ec, ok := err.(exitCoder); ok {
		return ec.ExitCode()
	}
	return 0
}

// mergeWaitJSON is the JSON shape emitted by `prism merge --wait --json`.
// Stable schema: every key is always present (zero values for absent
// timestamps so consumers do not need to handle key-missing).
type mergeWaitJSON struct {
	PR       int     `json:"pr"`
	Status   string  `json:"status"`
	Title    *string `json:"title"`
	Error    *string `json:"error"`
	MergedAt *string `json:"merged_at"`
	EndedAt  *string `json:"ended_at"`
}

func emitMergeWaitTerminal(row *db.PendingMerge, jsonMode bool) error {
	if row == nil {
		return fmt.Errorf("prism merge --wait: terminal row is nil")
	}
	if jsonMode {
		payload := mergeWaitJSON{PR: row.PR, Status: row.Status, Title: row.Title, Error: row.Error}
		if row.MergedAt != nil {
			s := row.MergedAt.UTC().Format(time.RFC3339)
			payload.MergedAt = &s
		}
		if row.EndedAt != nil {
			s := row.EndedAt.UTC().Format(time.RFC3339)
			payload.EndedAt = &s
		}
		data, mErr := json.Marshal(payload)
		if mErr != nil {
			return fmt.Errorf("prism merge --wait: marshal JSON: %w", mErr)
		}
		if pErr := printJSON(data); pErr != nil {
			return pErr
		}
	} else {
		if row.Status == "merged" {
			// Include the stored PR title in the already-merged short-circuit
			// output so a cross-repo mismatch — the incident shape of issue
			// #2354 — is visually detectable by the caller. When the title
			// is unavailable (older row, or the row was created before we
			// captured titles), fall back to the original terse form.
			if row.Title != nil && *row.Title != "" {
				fmt.Printf("PR #%d merged: %s\n", row.PR, *row.Title)
			} else {
				fmt.Printf("PR #%d merged.\n", row.PR)
			}
		} else {
			fmt.Printf("PR #%d ended with status %q", row.PR, row.Status)
			if row.Error != nil && *row.Error != "" {
				fmt.Printf(": %s", *row.Error)
			}
			fmt.Println()
		}
	}
	if row.Status == "merged" {
		return nil
	}
	return newExitErr(waitExitTerminalFail, "")
}

func emitMergeWaitTimeout(pr int, lastRow *db.PendingMerge, jsonMode bool, timeout time.Duration) error {
	if jsonMode {
		payload := struct {
			PR            int    `json:"pr"`
			Status        string `json:"status"`
			Waited        string `json:"waited"`
			LastRowStatus string `json:"last_row_status"`
			LastCheckedAt string `json:"last_checked_at"`
		}{PR: pr, Status: "timeout", Waited: timeout.String()}
		if lastRow != nil {
			payload.LastRowStatus = lastRow.Status
			if lastRow.LastCheckedAt != nil {
				payload.LastCheckedAt = lastRow.LastCheckedAt.UTC().Format(time.RFC3339)
			}
		}
		data, mErr := json.Marshal(payload)
		if mErr != nil {
			return fmt.Errorf("prism merge --wait timeout: marshal: %w", mErr)
		}
		return printJSON(data)
	}
	fmt.Fprintf(os.Stderr, "prism merge --wait: timed out after %s; merge queue continues running.\n", formatDurationShort(timeout))
	fmt.Fprintf(os.Stderr, "  Track progress with: prism merges list\n")
	return nil
}

// preflight is a thin adapter over probeInitialState that preserves the
// pre-#2420 return contract (title, error). Callers that only need the
// title — the sandbox proxy path and the direct-DB enqueue path — use
// preflight. Callers that need to emit the #2420 state-table initial-state
// message use probeInitialState directly.
func preflight(pr int) (string, error) {
	decision, err := probeInitialState(pr)
	if err != nil {
		return "", err
	}
	return decision.Title, nil
}

// initialStateOutcome names one row in the #2420 state table for
// `prism merge` invocation. It steers the synchronous message emitted to
// the coordinator and decides whether the PR is enqueued for polling.
type initialStateOutcome int

const (
	// initialOutcomeAlreadyMerged — PR is already MERGED at invocation.
	// Coordinator is told to clean up; no poller starts.
	initialOutcomeAlreadyMerged initialStateOutcome = iota

	// initialOutcomeClosedNotMerged — PR is CLOSED without a merge.
	// Coordinator is told a human closed it; no poller starts.
	initialOutcomeClosedNotMerged

	// initialOutcomeConflict — PR has merge conflicts at invocation. The
	// worker must rebase; command exits non-zero, no poller starts.
	initialOutcomeConflict

	// initialOutcomeEnqueueReady — branch protected, all gates green.
	// Watcher will merge on the next tick.
	initialOutcomeEnqueueReady

	// initialOutcomeEnqueuePending — branch protected, required checks
	// still running. Watcher polls until they finish (or the PR closes).
	initialOutcomeEnqueuePending

	// initialOutcomeEnqueueReview — branch protected, awaiting human
	// approval. Watcher polls silently until reviewDecision transitions
	// to APPROVED (and checks are green), or the PR is closed or merged
	// out-of-band.
	initialOutcomeEnqueueReview

	// initialOutcomeEnqueueUnprotected — no branch protection at all.
	// Watcher polls silently and NEVER auto-merges; a human must merge
	// or close the PR.
	initialOutcomeEnqueueUnprotected
)

// initialStateDecision holds the outcome of the invocation-time probe.
type initialStateDecision struct {
	Outcome initialStateOutcome
	Message string // rendered per the #2420 state-table row for Outcome
	Title   string // PR title (used for enqueue-row display)
	BaseRef string // target branch name (empty when not observable)
}

// probeInitialStatePRView is the parsed shape of `gh pr view --json` used by
// probeInitialState. Fields absent from the response unmarshal to zero
// values — the probe treats missing state fields defensively (it can enqueue
// on partial data; the watcher will observe the full state on the next tick).
type probeInitialStatePRView struct {
	State             string       `json:"state"`
	Number            int          `json:"number"`
	Title             string       `json:"title"`
	MergedAt          *string      `json:"mergedAt"`
	MergeStateStatus  string       `json:"mergeStateStatus"`
	ReviewDecision    string       `json:"reviewDecision"`
	BaseRefName       string       `json:"baseRefName"`
	StatusCheckRollup []checkEntry `json:"statusCheckRollup"`
}

// checkEntry mirrors the mergequeue package's rollup entry shape locally so
// probeInitialState can parse gh pr view output without an import cycle.
type checkEntry struct {
	Name       string `json:"name"`
	Context    string `json:"context"`
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
	State      string `json:"state"`
}

// probeInitialState is the #2420 synchronous initial-state probe. It queries
// `gh pr view` for the PR's current shape, and — for non-terminal cases —
// `gh api repos/:owner/:repo/branches/:branch/protection` to determine
// whether the target branch is protected. The result names the state-table
// row and carries the rendered coordinator-facing initial message.
//
// Terminology note: this function is a `prism merge` command-side helper, so
// it uses `gh` in the calling shell's CWD (not `--repo <slug>`); the repo
// slug is discovered by gh from git config. The watcher's own gh calls in
// internal/mergequeue always use `--repo <slug>` because the sidecar runs
// from an unrelated CWD.
func probeInitialState(pr int) (initialStateDecision, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	prOut, err := exec.CommandContext(ctx, "gh", "pr", "view", fmt.Sprintf("%d", pr),
		"--json", "state,number,title,mergedAt,mergeStateStatus,reviewDecision,baseRefName,statusCheckRollup").CombinedOutput()
	if err != nil {
		return initialStateDecision{}, fmt.Errorf("PR #%d not found or gh CLI error: %s", pr, strings.TrimSpace(string(prOut)))
	}
	var view probeInitialStatePRView
	if jsonErr := json.Unmarshal(prOut, &view); jsonErr != nil {
		return initialStateDecision{}, fmt.Errorf("parse gh pr view output: %w", jsonErr)
	}

	dec := initialStateDecision{Title: view.Title, BaseRef: view.BaseRefName}

	// Terminal: already merged (either state=MERGED or non-null mergedAt).
	if strings.EqualFold(view.State, "MERGED") || (view.MergedAt != nil && *view.MergedAt != "") {
		dec.Outcome = initialOutcomeAlreadyMerged
		dec.Message = fmt.Sprintf("PR #%d already merged. Please clean up the branch and worktree.", pr)
		return dec, nil
	}

	// Terminal: closed without merge.
	if strings.EqualFold(view.State, "CLOSED") {
		dec.Outcome = initialOutcomeClosedNotMerged
		dec.Message = fmt.Sprintf("PR #%d closed without merge. No action required from you; a human closed this. Please clean up the branch and worktree.", pr)
		return dec, nil
	}

	// OPEN. Second: merge conflict?
	if strings.EqualFold(view.MergeStateStatus, "DIRTY") {
		dec.Outcome = initialOutcomeConflict
		dec.Message = fmt.Sprintf("PR #%d has conflicts. Worker needs to rebase.", pr)
		return dec, nil
	}

	// Non-terminal. Query branch protection for the PR's base branch
	// (default main when unknown). A 404 means the repo has no protection
	// at all — the #2420 rule: NEVER auto-merge, wait for a human.
	baseRef := view.BaseRefName
	if baseRef == "" {
		baseRef = "main"
	}
	protected, requiredNames, protectErr := probeBranchProtection(baseRef)
	if protectErr != nil {
		return initialStateDecision{}, fmt.Errorf("probe branch protection: %w", protectErr)
	}

	if !protected {
		dec.Outcome = initialOutcomeEnqueueUnprotected
		dec.Message = fmt.Sprintf(
			"PR #%d has no branch protection configured. Not auto-merging. Waiting for a human to review and either approve the PR or merge it themselves. No action required from you.",
			pr,
		)
		return dec, nil
	}

	// Protected. CLEAN = ready to merge immediately (watcher will merge on next tick).
	if strings.EqualFold(view.MergeStateStatus, "CLEAN") {
		dec.Outcome = initialOutcomeEnqueueReady
		dec.Message = fmt.Sprintf("PR #%d ready. Merging now.", pr)
		return dec, nil
	}

	// Enumerate pending required checks.
	pending := pendingRequiredCheckNames(view.StatusCheckRollup, requiredNames)
	if len(pending) > 0 {
		dec.Outcome = initialOutcomeEnqueuePending
		dec.Message = fmt.Sprintf(
			"PR #%d waiting on %d check(s): [%s]. Standing by; will merge when green. No action required from you.",
			pr, len(pending), strings.Join(pending, ", "),
		)
		return dec, nil
	}

	// Protected, no pending required checks, not CLEAN — human review
	// required. Include the anti-reviewer-shopping guidance verbatim.
	dec.Outcome = initialOutcomeEnqueueReview
	dec.Message = fmt.Sprintf(
		"PR #%d requires human approval. No action required from you — do not request reviewers, do not add approvers, just wait. Will merge automatically when approved and checks pass, or notify if merged out-of-band.",
		pr,
	)
	return dec, nil
}

// probeBranchProtection queries the GitHub branch-protection endpoint for the
// given base branch. Returns (protected, requiredCheckNames, err):
//
//   - protected=false, err=nil — the endpoint returned HTTP 404, which is the
//     canonical "branch not protected" response. #2420 treats this as a state,
//     not a failure.
//   - protected=true, err=nil — protection is configured; requiredCheckNames
//     enumerates the required status checks (union of legacy contexts and
//     modern check-run names).
//   - err != nil — the API call failed in a way we cannot classify (network,
//     permissions). Callers surface the error to the coordinator.
//
// The gh CLI resolves the current repo from git config when `--repo` is
// omitted — this mirrors the invocation shape used elsewhere in cmd/merge.go.
func probeBranchProtection(baseRef string) (bool, []string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if baseRef == "" {
		baseRef = "main"
	}
	out, err := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/branches/%s/protection", baseRef)).CombinedOutput()
	if err != nil {
		low := strings.ToLower(string(out))
		if strings.Contains(low, "http 404") ||
			strings.Contains(low, "branch not protected") ||
			strings.Contains(low, "\"status\":\"404\"") {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("gh api branch protection: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var resp struct {
		RequiredStatusChecks struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
			} `json:"checks"`
		} `json:"required_status_checks"`
	}
	if jsonErr := json.Unmarshal(out, &resp); jsonErr != nil {
		return false, nil, fmt.Errorf("parse branch protection response: %w", jsonErr)
	}
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
	return true, names, nil
}

// pendingRequiredCheckNames returns required check names that are not yet
// SUCCESS in the rollup (missing, IN_PROGRESS, QUEUED, empty conclusion,
// etc.). Only required checks are considered — optional/slow checks that
// do not gate the merge are silently ignored.
func pendingRequiredCheckNames(rollup []checkEntry, required []string) []string {
	if len(required) == 0 {
		return nil
	}
	passed := make(map[string]bool, len(rollup))
	for _, c := range rollup {
		name := c.Name
		if name == "" {
			name = c.Context
		}
		if name == "" {
			continue
		}
		ok := strings.EqualFold(c.Conclusion, "SUCCESS") ||
			strings.EqualFold(c.State, "SUCCESS")
		passed[name] = ok
	}
	var pending []string
	for _, req := range required {
		if ok, found := passed[req]; !found || !ok {
			pending = append(pending, req)
		}
	}
	return pending
}

// emitInitialStateMessage prints the #2420 initial-state message to stdout
// and returns whether the caller should short-circuit (no enqueue). For
// terminal outcomes it returns (true, err) so the caller returns immediately.
// For enqueue outcomes it returns (false, nil) and the caller continues into
// the existing enqueue path.
//
// The message is suppressed on the JSON-quiet stdout contract
// (--wait --json), because that mode's stdout is a single parseable JSON
// object; the initial state is inferred by the caller from the terminal
// status probe.
func emitInitialStateMessage(dec initialStateDecision, quietStdout bool) (bool, error) {
	if !quietStdout {
		fmt.Println(dec.Message)
	}
	switch dec.Outcome {
	case initialOutcomeAlreadyMerged, initialOutcomeClosedNotMerged:
		return true, nil
	case initialOutcomeConflict:
		// Non-zero exit distinguishes this from the successful terminal
		// short-circuits above. Coordinator prompts worker to rebase.
		return true, newExitErr(waitExitTerminalFail, "")
	default:
		return false, nil
	}
}
