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
	// Idempotent observation: if the PR already has a terminal row in the
	// DB, --wait must return immediately with the recorded status. We check
	// this BEFORE the proxy/host-API enqueue path so an already-merged PR
	// short-circuits without a round-trip. The check uses a read-only DB
	// open and is best-effort: any error falls through to the normal enqueue
	// path so the user still gets a sensible behaviour.
	if waitFlag {
		if done, watErr := observeAlreadyTerminal(pr, jsonFlag); done {
			return watErr
		}
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
	// preflight is run inside the sandbox (gh works there) before the proxy
	// call so that invalid PRs do not pin sidecar resources.
	if apiURL := sandboxenv.HostAPISocket(); apiURL != "" {
		title, preflightErr := preflight(pr)
		if preflightErr != nil {
			return fmt.Errorf("prism merge: %w", preflightErr)
		}
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
		fmt.Printf("PR #%d enqueued (queue_position=%d, status=%s)\n", row.PR, row.QueuePosition, row.Status)
		if title != "" {
			fmt.Printf("  %s\n", title)
		}
		if waitFlag {
			return waitForMergeTerminal(pr, jsonFlag, timeoutFlag)
		}
		fmt.Println("The merge-queue watcher will drive this PR through CI and merge it automatically.")
		fmt.Println("You will be notified when it merges or fails.")
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

See: modules/programs/prism/opencode/agents/coordinator.md`, pr)
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

	// Pre-flight: verify the PR exists and is open, and fetch its title.
	// Timeout is kept tight (5s) so the overall command returns well within
	// the 2-second AC when the GitHub API responds promptly.
	title, err := preflight(pr)
	if err != nil {
		return fmt.Errorf("prism merge: %w", err)
	}

	// Enqueue (idempotent). Pass title for `prism merges list` display.
	var titlePtr *string
	if title != "" {
		titlePtr = &title
	}
	row, err := d.EnqueueMerge(pr, sessionName, instanceID, titlePtr)
	if err != nil {
		return fmt.Errorf("prism merge: %w", err)
	}

	fmt.Printf("PR #%d enqueued (queue_position=%d, status=%s)\n", pr, row.QueuePosition, row.Status)
	if title != "" {
		fmt.Printf("  %s\n", title)
	}
	if waitFlag {
		return waitForMergeTerminal(pr, jsonFlag, timeoutFlag)
	}
	fmt.Println("The merge-queue watcher will drive this PR through CI and merge it automatically.")
	fmt.Println("You will be notified when it merges or fails.")
	fmt.Println()
	fmt.Println("Track progress with: prism merges")
	return nil
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
func observeAlreadyTerminal(pr int, jsonMode bool) (bool, error) {
	probe, err := newWaitProbe()
	if err != nil {
		return false, nil
	}
	defer probe.Close()
	row, err := probe.Merge(pr)
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
func waitForMergeTerminal(pr int, jsonMode bool, timeout time.Duration) error {
	probe, err := newWaitProbe()
	if err != nil {
		return fmt.Errorf("prism merge --wait: %w", err)
	}
	defer probe.Close()

	var lastRow *db.PendingMerge
	err = pollWait(context.Background(), timeout,
		500*time.Millisecond, 5*time.Second,
		func() (bool, error) {
			row, qErr := probe.Merge(pr)
			if qErr != nil {
				// Transient — keep polling.
				fmt.Fprintf(os.Stderr, "[prism merge --wait] probe error: %v (will retry)\n", qErr)
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
			fmt.Printf("PR #%d merged.\n", row.PR)
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
			PR             int    `json:"pr"`
			Status         string `json:"status"`
			Waited         string `json:"waited"`
			LastRowStatus  string `json:"last_row_status"`
			LastCheckedAt  string `json:"last_checked_at"`
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

// preflight calls `gh pr view <pr> --json state,number,title` to verify the PR
// exists and is open. Returns (title, nil) on success. Returns ("", err) if the
// PR is closed, merged, or not found.
// Timeout is 5s — callers should complete within ~2s for typical API latency.
func preflight(pr int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "pr", "view", fmt.Sprintf("%d", pr),
		"--json", "state,number,title").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("PR #%d not found or gh CLI error: %s", pr, strings.TrimSpace(string(out)))
	}
	var info struct {
		State  string `json:"state"`
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	if jsonErr := json.Unmarshal(out, &info); jsonErr != nil {
		return "", fmt.Errorf("parse gh pr view output: %w", jsonErr)
	}
	switch strings.ToUpper(info.State) {
	case "OPEN":
		// Good.
	case "CLOSED":
		return "", fmt.Errorf("PR #%d is already closed — cannot enqueue", pr)
	case "MERGED":
		return "", fmt.Errorf("PR #%d is already merged — cannot enqueue", pr)
	default:
		return "", fmt.Errorf("PR #%d has unexpected state %q — cannot enqueue", pr, info.State)
	}

	fmt.Fprintf(os.Stderr, "prism merge: enqueueing PR #%d: %s\n", pr, info.Title)
	return info.Title, nil
}
