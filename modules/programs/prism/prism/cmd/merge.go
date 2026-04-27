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

	"github.com/google/uuid"
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
	rootCmd.AddCommand(mergeCmd)
}

func runMerge(cmd *cobra.Command, args []string) error {
	prArg := args[0]
	pr, err := strconv.Atoi(prArg)
	if err != nil || pr <= 0 {
		return fmt.Errorf("prism merge: invalid PR number %q — must be a positive integer", prArg)
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

	// Look up instance_id for the calling session. When no instance_id is
	// registered (e.g. a coordinator session opened outside of prism switch,
	// which is the common case for @main sessions), mint one on the fly and
	// write it to the DB so the rest of the merge flow proceeds normally.
	var instanceID string
	if callerSession != "" {
		status, statusErr := d.CurrentStatus(callerSession)
		if statusErr != nil {
			return fmt.Errorf("prism merge: look up session %q: %w", callerSession, statusErr)
		}
		if status != nil && status.InstanceID != nil {
			instanceID = *status.InstanceID
		}
	}
	if instanceID == "" {
		// Guard: if we have no session identity at all, we cannot mint a
		// meaningful instance_id — fail with a clear message rather than
		// creating a garbage DB row keyed on an empty session name.
		if callerSession == "" {
			return fmt.Errorf("prism merge: cannot determine calling session — run from inside a prism tmux session or set PRISM_SESSION_NAME")
		}

		// Determine worktree and repo for the on-the-fly registration.
		// Prefer the value already stored in agent_status (if the row exists
		// with a non-empty worktree); fall back to the process's CWD.
		worktree, _ := os.Getwd()
		// Re-query: the status row may already exist with a worktree.
		if existing, _ := d.CurrentStatus(callerSession); existing != nil && existing.Worktree != "" {
			worktree = existing.Worktree
		}
		repo := deriveRepo(worktree)
		if repo == "" {
			// Not inside a bare-repo worktree — derive from the session name.
			if idx := strings.Index(callerSession, "@"); idx > 0 {
				repo = callerSession[:idx]
			}
		}

		minted := uuid.New().String()
		// Ensure the agent_status row exists before writing instance_id.
		if upsertErr := d.UpsertStatus(callerSession, repo, worktree, "idle", nil, nil); upsertErr != nil {
			return fmt.Errorf("prism merge: register session %q: %w", callerSession, upsertErr)
		}
		// Clear any stale ended_at so the session is visible as active in the
		// dashboard and in ended_at IS NULL queries. This mirrors the
		// tmux-session-start handler (event.go:ClearEnded) which does the same
		// immediately after UpsertStatus, and covers the case where the session
		// was previously ended but has been re-opened without prism switch.
		if clearErr := d.ClearEnded(callerSession); clearErr != nil {
			fmt.Fprintf(os.Stderr, "prism merge: clear ended for %q: %v\n", callerSession, clearErr)
		}
		if setErr := d.SetInstanceID(callerSession, minted); setErr != nil {
			return fmt.Errorf("prism merge: set instance_id for session %q: %w", callerSession, setErr)
		}
		// Insert a sessions row so the incarnation is fully registered
		// (idempotent: INSERT OR IGNORE).
		if insErr := d.InsertSession(db.Session{
			InstanceID:  minted,
			SessionName: callerSession,
			Repo:        repo,
			Worktree:    worktree,
		}); insErr != nil {
			// Non-fatal: log and continue. The instance_id is already written
			// to agent_status, which is sufficient for EnqueueMerge.
			fmt.Fprintf(os.Stderr, "prism merge: insert session row for %q: %v\n", callerSession, insErr)
		}
		instanceID = minted
	}

	sessionName := callerSession
	if sessionName == "" {
		sessionName = "unknown"
	}

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
	fmt.Println("The merge-queue watcher will drive this PR through CI and merge it automatically.")
	fmt.Println("You will be notified when it merges or fails.")
	fmt.Println()
	fmt.Println("Track progress with: prism merges")
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
