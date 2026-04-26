// Package review implements the prism review execution engine.
//
// prism review <pr-number> spawns N review agent sessions as independent
// top-level tmux sessions named <parent>~review-<N>-<agent-name> (where N is
// the 1-indexed round number), polls the prism DB until all agents reach the
// "finished" state, reads their last msg_assistant event, and returns
// aggregated findings to stdout.
//
// Session architecture (new per-agent model, PR-C):
//   - Five independent top-level tmux sessions per review round:
//     <parent>~review-<N>-review-goal
//     <parent>~review-<N>-review-code
//     <parent>~review-<N>-review-security
//     <parent>~review-<N>-review-qa
//     <parent>~review-<N>-review-context
//   - Each session has its own port allocation, sidecar, and container.
//   - Sessions persist until prism cleanup is invoked on the parent.
//   - No round-level multi-window session is created.
//
// The ~ separator in the session name is used by the dashboard for depth-2
// child detection (review sessions appear indented under their parent branch).
//
// Readiness gate (#1051 Piece A):
//
// Each per-agent session.SpawnSession call only returns "the tmux session was
// created and the sidecar process was kicked off" — opencode itself runs
// inside the bwrap/podman/host process the tmux pane launches, several steps
// further along, and may take seconds to bind its TCP port (≈8.5s observed
// in the worst healthy case captured in #1051) or never bind at all if
// startup fails silently. The spawn loop therefore runs a per-agent
// readiness gate via gateReviewAgents (see readiness.go) after spawning, in
// parallel goroutines so one slow agent does not delay the others.
//
// Each gate calls session.WaitForReady, which polls the DB for either:
//   - any state_change event (the sidecar wrote one when the first SSE event
//     arrived from opencode, which can only happen after opencode bound its
//     port), or
//   - agent_status.harness_session_id becoming non-NULL (the sidecar saw
//     opencode's session.created event).
//
// The default per-agent readiness window is 30s
// (DefaultReviewReadinessTimeout). On timeout, the gate emits
// "<role> failed to start: not ready within 30s" via OnProgress, populates
// the spawn-failure slot for that agent, and runs the standard cleanup
// (KillSidecar + cleanupAgentSession + tmux.KillSession). The other agents
// are unaffected; the review proceeds with the survivors.
//
// Per-agent startup log (#1051 Piece B):
//
// Each spawned session has an agent-startup.log file in its run directory:
//
//	$XDG_STATE_HOME/prism/run/<session>/agent-startup.log
//
// SpawnSession writes timestamped breadcrumbs to this file describing the
// spawn-time sequence (DB seed, port allocation, tmux session creation,
// sidecar startup, readiness gate outcome). It is the forensic trail
// covering the gap between "session created in DB" and "first SSE event
// arrives at the sidecar" — exactly the window in which the silent failure
// reported by #1051 occurs. The bwrap-side stderr lands in agent-run.log in
// the same directory; together they cover the full pre-opencode startup
// timeline.
//
// Async Ack contract (#1051 Piece C):
//
// RunAsync's AsyncResult.Ack distinguishes successfully-ready agents from
// failed-to-spawn / failed-to-ready agents:
//
//	Spawned: 3, Failed: 2 (review-goal: not ready within 30s, review-qa: not ready within 30s)
//
// so the worker session reading the Ack sees a partial-success outcome
// immediately instead of discovering it 20 minutes later when the monitor
// timeout fires.
package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// linkedIssueRe matches "Closes #N", "Refs #N", "Fixes #N", "References #N"
// (case-insensitive) in PR body text. Capture group 1 is the issue number.
var linkedIssueRe = regexp.MustCompile(`(?i)(?:closes|fixes|refs|references)\s+#(\d+)`)

// DiffMaxBytes is the maximum diff size (in bytes) before truncation.
// Configurable for testing; production code uses this default.
const DiffMaxBytes = 200 * 1024 // 200 KB

// DiffMaxLines is the maximum number of diff lines before truncation.
// Configurable for testing; production code uses this default.
const DiffMaxLines = 4000

// DiffInlineMaxLines is the threshold (in lines) below which diffs are inlined
// directly in the agent prompt. Diffs at or above this threshold are written to
// a temp file and agents are given the file path. Configurable via the
// PRISM_REVIEW_DIFF_INLINE_MAX env var or --diff-inline-max flag.
const DiffInlineMaxLines = 500

// DiffInlineMaxBytes is the threshold (in bytes) below which diffs are inlined.
// Used alongside DiffInlineMaxLines; the smaller of the two limits applies.
const DiffInlineMaxBytes = 20 * 1024 // 20 KB

// PRContext holds PR metadata and diff fetched once at spawn time, before any
// review-agent sessions are created. All five agents receive the same context
// in their initial prompt — no per-agent duplication.
type PRContext struct {
	// PRNumber is the pull request number (string, e.g. "819").
	PRNumber string
	// Title is the PR title.
	Title string
	// Body is the PR body text (may be empty).
	Body string
	// HeadRefName is the head branch name.
	HeadRefName string
	// HeadRefOid is the full head commit SHA.
	HeadRefOid string
	// BaseRefName is the base branch name.
	BaseRefName string
	// BaseRefOid is the full base commit SHA.
	BaseRefOid string
	// Additions is the number of lines added.
	Additions int
	// Deletions is the number of lines deleted.
	Deletions int
	// ChangedFiles is the number of files changed.
	ChangedFiles int
	// RecentCommits is the output of `git log --oneline -20`.
	RecentCommits string
	// BranchCommits is the output of `git log origin/<base>..HEAD`.
	BranchCommits string
	// Diff is the full PR diff (may be truncated if above inline threshold).
	// When DiffFilePath is non-empty, Diff is empty and the diff is on disk.
	Diff string
	// DiffTruncated is true when the diff was truncated due to size limits.
	DiffTruncated bool
	// DiffFilePath is the path to the diff file when the diff is above the
	// inline threshold. When empty, the diff is inlined in Diff.
	DiffFilePath string
	// DiffLines is the total number of lines in the diff (for reporting).
	DiffLines int
	// DiffBytes is the total size in bytes of the diff (for reporting).
	DiffBytes int
	// LinkedIssues contains the fetched text for each issue referenced by
	// "Closes #N" or "Refs #N" in the PR body. Keys are issue numbers.
	// Values are the raw output of `gh issue view <N>`, or a failure marker
	// if the issue could not be fetched.
	LinkedIssues map[string]string
	// FetchFailed is true when gh failed and only minimal info is available.
	FetchFailed bool
	// WorktreePath is the absolute path to the worktree being reviewed.
	// Review agents should treat the worktree as read-only.
	WorktreePath string
	// Round is the review round number (1-indexed). Used in DiffFilePath.
	Round int
}

// prViewJSON is the JSON shape returned by `gh pr view --json ...`.
type prViewJSON struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	HeadRefName  string `json:"headRefName"`
	HeadRefOid   string `json:"headRefOid"`
	BaseRefName  string `json:"baseRefName"`
	BaseRefOid   string `json:"baseRefOid"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changedFiles"`
}

// FetchPRContextOpts controls how FetchPRContext gathers context.
type FetchPRContextOpts struct {
	// PRNumber is the pull request number (e.g. "819").
	PRNumber string
	// Round is the 1-indexed review round number. Used to name the diff file
	// when the diff exceeds the inline threshold (avoids collisions).
	Round int
	// MaxBytes is the hard cap on diff size before truncation. Zero means
	// use DiffMaxBytes.
	MaxBytes int
	// MaxLines is the hard cap on diff line count before truncation. Zero means
	// use DiffMaxLines.
	MaxLines int
	// InlineMaxBytes is the threshold below which the diff is inlined in the
	// prompt. Zero means use DiffInlineMaxBytes.
	InlineMaxBytes int
	// InlineMaxLines is the threshold below which the diff is inlined in the
	// prompt. Zero means use DiffInlineMaxLines.
	InlineMaxLines int
	// Worktree is the path to the git worktree for running git commands.
	// When empty, git commands run in the current directory.
	Worktree string
}

// FetchPRContext fetches PR metadata and diff from the gh CLI.
// If gh fails or is unavailable, it returns a PRContext with FetchFailed=true
// and only the PR number populated — the caller must not treat this as an error;
// the review run continues with a minimal prompt (fallback behaviour).
// maxBytes and maxLines control truncation of the diff; pass 0 to use the defaults.
func FetchPRContext(prNumber string, maxBytes, maxLines int) PRContext {
	return FetchPRContextWithOpts(FetchPRContextOpts{
		PRNumber: prNumber,
		MaxBytes: maxBytes,
		MaxLines: maxLines,
	})
}

// FetchPRContextWithOpts fetches PR metadata, git log, diff, and linked issues.
// It is the full implementation; FetchPRContext is a thin wrapper for callers
// that only need the legacy (maxBytes, maxLines) API.
func FetchPRContextWithOpts(opts FetchPRContextOpts) PRContext {
	maxBytes := opts.MaxBytes
	maxLines := opts.MaxLines
	inlineMaxBytes := opts.InlineMaxBytes
	inlineMaxLines := opts.InlineMaxLines

	if maxBytes <= 0 {
		maxBytes = DiffMaxBytes
	}
	if maxLines <= 0 {
		maxLines = DiffMaxLines
	}
	if inlineMaxBytes <= 0 {
		inlineMaxBytes = DiffInlineMaxBytes
	}
	if inlineMaxLines <= 0 {
		inlineMaxLines = DiffInlineMaxLines
	}

	prCtx := PRContext{PRNumber: opts.PRNumber, Round: opts.Round}

	// Fetch PR metadata.
	viewOut, err := runGH("pr", "view", opts.PRNumber, "--json",
		"number,title,body,headRefName,headRefOid,baseRefName,baseRefOid,additions,deletions,changedFiles")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not fetch PR metadata via gh: %v — agents will fall back to git-based discovery\n", err)
		prCtx.FetchFailed = true
		return prCtx
	}

	var meta prViewJSON
	if jsonErr := json.Unmarshal([]byte(viewOut), &meta); jsonErr != nil {
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not parse PR metadata JSON: %v — agents will fall back to git-based discovery\n", jsonErr)
		prCtx.FetchFailed = true
		return prCtx
	}

	prCtx.Title = meta.Title
	prCtx.Body = meta.Body
	prCtx.HeadRefName = meta.HeadRefName
	prCtx.HeadRefOid = meta.HeadRefOid
	prCtx.BaseRefName = meta.BaseRefName
	prCtx.BaseRefOid = meta.BaseRefOid
	prCtx.Additions = meta.Additions
	prCtx.Deletions = meta.Deletions
	prCtx.ChangedFiles = meta.ChangedFiles

	// Gather git log — non-fatal; missing log output is noted in the prompt.
	prCtx.RecentCommits = runGitInWorktree(opts.Worktree, "log", "--oneline", "-20")
	if meta.BaseRefName != "" {
		prCtx.BranchCommits = runGitInWorktree(opts.Worktree, "log", "origin/"+meta.BaseRefName+"..HEAD")
	}

	// Fetch linked issues — non-fatal; unfetchable issues get a clear marker.
	issueNumbers := parseLinkedIssues(meta.Body)
	if len(issueNumbers) > 0 {
		prCtx.LinkedIssues = make(map[string]string, len(issueNumbers))
		for _, num := range issueNumbers {
			issueText, issueErr := runGH("issue", "view", num)
			if issueErr != nil {
				prCtx.LinkedIssues[num] = fmt.Sprintf("[issue #%s could not be fetched: %v]", num, issueErr)
			} else {
				prCtx.LinkedIssues[num] = issueText
			}
		}
	}

	// Fetch diff.
	diffOut, diffErr := runGH("pr", "diff", opts.PRNumber)
	if diffErr != nil {
		// Diff failure is non-fatal: we have metadata, just no diff content.
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not fetch PR diff via gh: %v — agents will use git diff instead\n", diffErr)
		// Leave Diff and DiffFilePath empty; the prompt will note diff unavailability.
	} else {
		prCtx.DiffBytes = len(diffOut)
		prCtx.DiffLines = strings.Count(diffOut, "\n")

		// Truncate to hard limits first.
		truncated, wasTruncated := truncateDiff(diffOut, maxBytes, maxLines)
		prCtx.DiffTruncated = wasTruncated

		// Decide inline vs file based on ORIGINAL (pre-truncation) size.
		if len(diffOut) <= inlineMaxBytes && strings.Count(diffOut, "\n") <= inlineMaxLines {
			// Small enough to inline.
			prCtx.Diff = truncated
		} else {
			// Large diff — write to a temp file and point agents at the path.
			diffPath := diffFilePath(opts.PRNumber, opts.Round)
			if writeErr := os.WriteFile(diffPath, []byte(diffOut), 0o644); writeErr != nil {
				fmt.Fprintf(os.Stderr, "[prism review] warning: could not write diff to %s: %v — inlining diff instead\n", diffPath, writeErr)
				prCtx.Diff = truncated
			} else {
				prCtx.DiffFilePath = diffPath
			}
		}
	}

	return prCtx
}

// diffFilePath returns the path for the diff temp file for a given PR and round.
// Format: /tmp/prism-review-<pr>-round-<N>.diff
func diffFilePath(prNumber string, round int) string {
	if round <= 0 {
		round = 1
	}
	return fmt.Sprintf("/tmp/prism-review-%s-round-%d.diff", prNumber, round)
}

// parseLinkedIssues extracts issue numbers referenced by "Closes #N", "Refs #N",
// "Fixes #N", or "References #N" in the PR body (case-insensitive).
// Returns a deduplicated, ordered list of issue number strings (e.g. ["123", "456"]).
func parseLinkedIssues(body string) []string {
	// Match patterns like: Closes #123, Refs #456, Fixes #789, References #012
	// Allow for optional comma or whitespace after the number.
	re := linkedIssueRe
	matches := re.FindAllStringSubmatch(body, -1)
	seen := make(map[string]bool)
	var result []string
	for _, m := range matches {
		num := m[1]
		if !seen[num] {
			seen[num] = true
			result = append(result, num)
		}
	}
	return result
}

// runGitInWorktree runs a git command in the given worktree directory and returns
// its stdout. Returns an empty string on error (git log failures are non-fatal).
func runGitInWorktree(worktree string, args ...string) string {
	cmd := exec.Command("git", args...)
	if worktree != "" {
		cmd.Dir = worktree
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return stdout.String()
}

// truncateDiff truncates a diff to at most maxBytes bytes and maxLines lines.
// Returns the (possibly truncated) diff and a bool indicating truncation.
func truncateDiff(diff string, maxBytes, maxLines int) (string, bool) {
	// Check byte limit first.
	if len(diff) > maxBytes {
		// Truncate to maxBytes, then find the last newline to avoid a mid-line cut.
		truncated := diff[:maxBytes]
		if idx := strings.LastIndex(truncated, "\n"); idx > 0 {
			truncated = truncated[:idx]
		}
		return truncated + "\n... [truncated — use git diff origin/<base>...HEAD for full content]", true
	}

	// Check line limit.
	lines := strings.Split(diff, "\n")
	if len(lines) > maxLines {
		return strings.Join(lines[:maxLines], "\n") + "\n... [truncated — use git diff origin/<base>...HEAD for full content]", true
	}

	return diff, false
}

// runGH executes a gh command and returns its stdout as a string.
// Returns an error if gh is not found or exits with a non-zero status.
func runGH(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return "", err
	}
	return stdout.String(), nil
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

// Agent describes a single review agent to run.
type Agent struct {
	// Name is the agent identifier, e.g. "review-goal".
	Name string
	// OpencodeName is the opencode --agent flag value, e.g. "review-goal".
	OpencodeName string
}

// Agents returns the five-agent review set.
// Each agent corresponds to a specialised opencode agent definition under
// modules/programs/prism/opencode/agents/.
func Agents() []Agent {
	return []Agent{
		{Name: "review-goal", OpencodeName: "review-goal"},
		{Name: "review-code", OpencodeName: "review-code"},
		{Name: "review-security", OpencodeName: "review-security"},
		{Name: "review-qa", OpencodeName: "review-qa"},
		{Name: "review-context", OpencodeName: "review-context"},
	}
}

// RoleValidator is a function that reports whether a given agent role is
// valid for the active harness. Returns nil when valid; an error with a
// descriptive message when invalid. This matches the signature of
// harness.Harness.ValidateAgentRole — callers pass h.ValidateAgentRole
// directly.
type RoleValidator func(role string) error

// CheckAgentAvailability verifies that all given agents are valid for the
// active harness. The validator function should be h.ValidateAgentRole from
// the active harness adapter. Returns a descriptive error listing any
// invalid agents; returns nil when all are valid.
//
// This is intentionally skipped in container mode because the check cannot
// reliably inspect the container filesystem.
func CheckAgentAvailability(agents []Agent, validate RoleValidator) error {
	var invalid []string
	var firstErr error
	for _, ag := range agents {
		if err := validate(ag.Name); err != nil {
			invalid = append(invalid, ag.Name)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if len(invalid) > 0 {
		return fmt.Errorf(
			"review agents not available: %s\n%v",
			strings.Join(invalid, ", "), firstErr,
		)
	}
	return nil
}

// AgentsByName filters the agents slice to only those whose Name is in the
// allowedNames set. Returns an error if any name in allowedNames does not exist
// in agents.
func AgentsByName(agents []Agent, allowedNames []string) ([]Agent, error) {
	available := make(map[string]Agent, len(agents))
	for _, a := range agents {
		available[a.Name] = a
	}
	var result []Agent
	var unknown []string
	for _, name := range allowedNames {
		if a, ok := available[name]; ok {
			result = append(result, a)
		} else {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown agent name(s): %s\navailable: %s",
			strings.Join(unknown, ", "),
			strings.Join(agentNames(agents), ", "),
		)
	}
	return result, nil
}

func agentNames(agents []Agent) []string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return names
}

// AgentResult holds the outcome for a single review agent.
type AgentResult struct {
	Agent   Agent
	Passed  bool   // true = passed, false = failed / errored
	Output  string // last assistant message text, or error description
	IsError bool   // true = infrastructure/timeout failure
}

// Opts configures a review run.
type Opts struct {
	// PRNumber is the pull request number to review.
	PRNumber string
	// ParentSession is the prism session name of the calling worker/coordinator.
	ParentSession string
	// WorkerSession is the session name that will receive the async delivery
	// prompt when the review completes. For RunAsync, this MUST be set to the
	// session running `prism review`. For the synchronous Run path, it is
	// unused and may be left empty.
	WorkerSession string
	// Worktree is the absolute path to the parent session's worktree.
	Worktree string
	// Agents is the list of review agents to spawn.
	Agents []Agent
	// Harness is the runtime harness to use ("opencode").
	Harness string
	// RuntimeEnvVars holds harness-specific environment variables to inject
	// into each spawned agent session (host-mode only; container-mode sessions
	// receive env vars via the container runtime). Populated from
	// harness.Harness.RuntimeEnv() by the caller. When nil, no harness-specific
	// env vars are injected.
	RuntimeEnvVars map[string]string
	// Timeout is the per-agent maximum wait time.
	Timeout time.Duration
	// DBPath is the path to the prism database. If empty, the default is used.
	DBPath string
	// PluginHostPath is the path to the opencode plugin file.
	PluginHostPath string
	// ProfilesFile is the loaded profiles.json, used to resolve per-agent
	// container config blobs via ContainerConfigForRole. When nil, no config
	// injection is performed (equivalent to pre-PR-B behaviour).
	ProfilesFile *config.ProfilesFile
	// ContainerMode: when true, each agent runs in its own container.
	ContainerMode bool
	// OnProgress is an optional callback invoked for each progress event:
	// spawn, finish, timeout, and spawn failure. It receives a formatted
	// progress line (without trailing newline). The caller is responsible for
	// writing and flushing the line. When nil, no progress output is emitted.
	OnProgress func(line string)
	// PRCtx is the pre-fetched PR context (metadata + diff). When populated,
	// buildReviewPrompt injects it into the initial prompt for every agent so
	// agents are productive from turn 1. When nil or FetchFailed is true, a
	// minimal fallback prompt is used instead.
	PRCtx *PRContext
	// SizeBudget is the maximum inline size (bytes) for full per-agent findings
	// in the formatted output. When the total findings exceed this budget they
	// are written to /tmp/prism-review-<pr>-round-<N>.md and a pointer is
	// included inline. Zero uses the default (20 KB). Can also be overridden
	// via the PRISM_REVIEW_SIZE_BUDGET environment variable.
	SizeBudget int
	// IsolationMode is the resolved isolation mode to use when spawning review
	// agent sessions. Valid values: "podman", "bwrap", "host". When set, it is
	// forwarded to session.SpawnOpts.IsolationMode for every spawned agent.
	// When empty, spawnAgentOnlyLayout will call cfg.EffectiveIsolationMode()
	// to resolve the machine default rather than silently falling back to host.
	IsolationMode string
	// ReadinessTimeout is the per-agent deadline for the post-spawn
	// readiness gate (#1051 Piece A). Zero falls back to
	// DefaultReviewReadinessTimeout (30s). The gate runs concurrently per
	// agent so the worst-case wall time is one timeout, not five.
	ReadinessTimeout time.Duration
}

// FormatAgentDisplayName converts an agent name like "review-goal" to a
// display name like "Review-Goal" for progress output lines.
func FormatAgentDisplayName(name string) string {
	parts := strings.Split(name, "-")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

// FormatProgressDuration formats a duration for progress output lines.
// Below 60s: "28.4s". At or above 60s: "1m12s".
func FormatProgressDuration(d time.Duration) string {
	if d < 60*time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	return fmt.Sprintf("%dm%ds", m, s)
}

// Run executes the review. It returns the aggregated results and an error.
//
// Each agent is spawned as its own independent top-level tmux session named
// <parent>~review-<N>-<agent.Name>. Previous rounds' sessions are NOT killed
// — they persist until prism cleanup is invoked on the parent. This is a
// deliberate behaviour change from the old multi-window round model.
//
// On SIGINT, only the current round's in-progress sessions are killed by the
// caller via KillSessionsByNames (using the session names from onSessionsCreated).
// Previous rounds remain untouched.
func Run(ctx context.Context, opts Opts, onSessionsCreated func(sessionNames []string)) ([]AgentResult, error) {
	if opts.ParentSession == "" {
		return nil, fmt.Errorf("parent session name is required")
	}

	// Resolve worktree path.
	worktree := opts.Worktree
	if worktree == "" {
		return nil, fmt.Errorf("worktree path is required")
	}

	// Open DB.
	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = defaultDBPath()
	}
	d, err := db.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	// Determine round number from DB. We do NOT kill previous review sessions —
	// they persist until prism cleanup on the parent (deliberate).
	round := NextRoundNumber(d, opts.ParentSession)
	roundPrefix := fmt.Sprintf("%s~review-%d-", opts.ParentSession, round)

	repo := deriveRepo(opts.ParentSession)

	agents := opts.Agents
	if len(agents) == 0 {
		agents = Agents()
	}

	// Register a session group for this review round. Every spawned agent
	// session will carry this group_id, enabling GroupCompleted-based
	// termination detection and GroupResults-based result aggregation.
	// Fail fast if RegisterGroup fails — no sessions are spawned without a
	// group to belong to (AC: edge-case).
	groupID, groupErr := d.RegisterGroup(opts.ParentSession)
	if groupErr != nil {
		return nil, fmt.Errorf("register review group: %w", groupErr)
	}

	// Spawn each agent as its own independent top-level tmux session.
	// spawnErr[i] is non-nil if agent i failed to spawn. Agents that fail to
	// spawn are excluded from polling; they receive an error AgentResult.
	agentSessions := make([]string, len(agents))
	spawnErr := make([]error, len(agents))
	spawnTimes := make([]time.Time, len(agents))

	for i, ag := range agents {
		// Per-agent session: <parent>~review-<N>-<agent.Name>
		agentSession := roundPrefix + ag.Name
		agentSessions[i] = agentSession

		// Build the prompt for the review agent.
		// Inject the worktree path into PRCtx so agents know where the
		// branch is checked out (and that it is read-only).
		prCtxWithWorktree := opts.PRCtx
		if prCtxWithWorktree != nil && !prCtxWithWorktree.FetchFailed {
			// Shallow-copy so we don't mutate the shared PRCtx.
			ctxCopy := *prCtxWithWorktree
			ctxCopy.WorktreePath = worktree
			prCtxWithWorktree = &ctxCopy
		}
		prompt := buildReviewPrompt(opts.PRNumber, prCtxWithWorktree)

		// Resolve the per-agent config blob. Each agent gets its own hardened
		// opencode.json that declares only that one review agent.
		//
		// In sandboxed mode (podman or bwrap) a missing or empty blob means the
		// sandbox falls back to the host config (wrong agent identity).
		// ResolveAgentConfigContent surfaces this as an explicit error to
		// prevent silent wrong-agent spawns.
		agentConfigContent, configErr := ResolveAgentConfigContent(opts.IsolationMode, opts.ProfilesFile, ag.Name)
		if configErr != nil {
			spawnErr[i] = configErr
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), configErr))
			}
			continue
		}

		// For bwrap sessions, write the opencode.json config file to disk now
		// so it is present before the agent pane opens. The bwrap harness
		// checks for file existence (os.Stat) rather than reading ConfigContent
		// from the session state, so it must be written here at spawn time.
		// This mirrors the pattern in cmd/spawn.go:334-340 for regular bwrap spawns.
		// Podman mode does NOT need this write — the sidecar's Create() path
		// already writes the file before the container starts.
		// sandbox-exec mode does NOT yet use this path — it has no bwrap-equivalent
		// mount mechanism. Config delivery for sandbox-exec is deferred to #1016.
		if opts.IsolationMode == string(config.IsolationBwrap) && agentConfigContent != "" {
			containerName := container.NameForSession(agentSession)
			if writeErr := container.WriteOpencodeConfig(containerName, agentConfigContent); writeErr != nil {
				spawnErr[i] = fmt.Errorf("review: write opencode config for bwrap agent %s: %w", ag.Name, writeErr)
				if opts.OnProgress != nil {
					opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), spawnErr[i]))
				}
				continue
			}
		}

		// Spawn the per-agent session via the shared primitive. SpawnSession
		// handles DB seed (with root_agent_name from AgentRole), port
		// allocation, tmux session creation, and sidecar startup — keeping
		// review.go free of direct db/tmux/sidecar machinery (#859).
		//
		// WorktreeReadOnly=true ensures review containers cannot modify the
		// branch under review (satisfies the [security] acceptance criterion).
		spawnOpts := session.SpawnOpts{
			SessionName:      agentSession,
			Repo:             repo,
			Worktree:         worktree,
			AgentRole:        ag.Name,
			Prompt:           prompt,
			ConfigContent:    agentConfigContent,
			Layout:           session.LayoutAgentOnly,
			ContainerMode:    opts.ContainerMode,
			IsolationMode:    opts.IsolationMode,
			PluginHostPath:   opts.PluginHostPath,
			WorktreeReadOnly: true,
			GroupID:          groupID,
			RuntimeEnvVars:   opts.RuntimeEnvVars,
		}
		if spawnSessErr := session.SpawnSession(d, spawnOpts); spawnSessErr != nil {
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), spawnSessErr))
			}
			// Clean up this agent's resources (sidecar, DB row, tmux session).
			// SpawnSession may have partially progressed; be defensive so a
			// second spawn attempt with the same name doesn't see stale state.
			session.KillSidecar(agentSession)
			cleanupAgentSession(d, agentSession)
			_ = tmux.KillSession(agentSession)
			spawnErr[i] = fmt.Errorf("spawn session for %s: %w", ag.Name, spawnSessErr)
			continue
		}

		// Capture the spawn time so the readiness-gate phase below can
		// reset it once the agent is actually ready, and so the polling
		// phase has a sensible fallback if the gate is somehow skipped.
		// The "started" progress line is emitted by the readiness gate,
		// not here — see #1051 for why "spawned" is not the same as
		// "ready".
		spawnTimes[i] = time.Now()
	}

	// Per-agent readiness gate (#1051 Piece A). Runs in parallel goroutines
	// so one slow agent does not delay the others. Updates spawnErr[i] for
	// agents whose gate trips on timeout, and emits "<role> started" /
	// "<role> failed to start: not ready within <timeout>" via OnProgress.
	gateReviewAgents(d, agents, agentSessions, spawnErr, spawnTimes,
		opts.ReadinessTimeout, opts.OnProgress)

	// Check if all agents failed to spawn (or to become ready) — surface
	// a combined error. With the readiness gate in place, "all failed"
	// covers both pre-spawn failures (config errors, SpawnSession errors)
	// and never-came-up failures (readiness timeouts).
	allFailed := true
	for _, se := range spawnErr {
		if se == nil {
			allFailed = false
			break
		}
	}
	if allFailed {
		return nil, fmt.Errorf("all review agents failed to spawn")
	}

	// Build the subset of agents that successfully spawned AND became ready
	// for polling and SIGINT notification.
	var liveAgents []Agent
	var liveSessions []string
	var liveSpawnTimes []time.Time
	for i, se := range spawnErr {
		if se == nil {
			liveAgents = append(liveAgents, agents[i])
			liveSessions = append(liveSessions, agentSessions[i])
			liveSpawnTimes = append(liveSpawnTimes, spawnTimes[i])
		}
	}

	// Notify the caller with all successfully-ready session names for SIGINT handling.
	if onSessionsCreated != nil {
		onSessionsCreated(liveSessions)
	}

	// Poll DB until all live agents finish or timeout. Uses GroupCompleted
	// for termination detection instead of per-session name-based polling.
	liveResults, pollErr := pollAgents(ctx, d, liveAgents, liveSessions, opts.Timeout, liveSpawnTimes, opts.OnProgress, groupID)

	// Sessions persist — do NOT kill them here. The user can re-read them later.
	// Containers, tmux sessions, and sidecars remain alive until prism cleanup
	// is invoked on the parent, which cascades via KillReviewSessionsForParent.

	// Merge live results with spawn-failure results, preserving original agent order.
	results := make([]AgentResult, len(agents))
	liveIdx := 0
	for i, ag := range agents {
		if spawnErr[i] != nil {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: failed to spawn agent: %v", spawnErr[i]),
				IsError: true,
			}
		} else {
			results[i] = liveResults[liveIdx]
			liveIdx++
		}
	}

	return results, pollErr
}

// AsyncResult is returned immediately by RunAsync. It contains the group_id
// and session names for the spawned review agents, and a human-readable
// acknowledgement message.
type AsyncResult struct {
	// GroupID is the session_groups.group_id for this review round.
	GroupID string
	// SessionNames is the list of spawned agent session names.
	SessionNames []string
	// Round is the 1-indexed review round number.
	Round int
	// Ack is a human-readable acknowledgement message to display to the worker.
	Ack string
}

// RunAsync spawns the review agents (same as Run's spawn phase), registers a
// group, starts a detached monitor process, and returns immediately with an
// AsyncResult. The monitor process will poll for group completion and deliver
// aggregated results to opts.WorkerSession via prism prompt.
//
// Unlike Run, RunAsync does NOT block while agents execute. The caller should
// display Ack to the worker and proceed without waiting for review results.
//
// opts.WorkerSession must be set to the session name that will receive the
// delivery prompt when the review completes.
//
// prismBinary is passed to StartMonitorProcess; pass "" to use os.Executable().
func RunAsync(opts Opts, prismBinary string) (*AsyncResult, error) {
	if opts.ParentSession == "" {
		return nil, fmt.Errorf("parent session name is required")
	}
	if opts.WorkerSession == "" {
		return nil, fmt.Errorf("worker session name is required for async review")
	}

	worktree := opts.Worktree
	if worktree == "" {
		return nil, fmt.Errorf("worktree path is required")
	}

	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = defaultDBPath()
	}
	d, err := db.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	agents := opts.Agents
	if len(agents) == 0 {
		agents = Agents()
	}

	// In-progress guard: reject if a round is already active for this parent.
	// We check for any group with at least one non-terminal member.
	activeGroupID, activeErr := ActiveReviewGroupForParent(d, opts.ParentSession)
	if activeErr != nil {
		// Non-fatal: log and proceed (better to allow duplicate than block falsely).
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not check for active review group: %v\n", activeErr)
	} else if activeGroupID != "" {
		// Determine the round number for a useful error message.
		members, _ := d.GroupMembersForParent(opts.ParentSession)
		activeRound := ReviewRoundForGroup(members)
		if activeRound > 0 {
			return nil, fmt.Errorf("prism review: round %d is already in progress for this PR (group %s).\n"+
				"Wait for it to complete or cancel the sessions with `prism cleanup`.",
				activeRound, activeGroupID)
		}
		return nil, fmt.Errorf("prism review: a review round is already in progress for session %q (group %s).\n"+
			"Wait for it to complete or cancel the sessions with `prism cleanup`.",
			opts.ParentSession, activeGroupID)
	}

	// Determine round number from DB.
	round := NextRoundNumber(d, opts.ParentSession)
	roundPrefix := fmt.Sprintf("%s~review-%d-", opts.ParentSession, round)
	repo := deriveRepo(opts.ParentSession)

	// Register session group.
	groupID, groupErr := d.RegisterGroup(opts.ParentSession)
	if groupErr != nil {
		return nil, fmt.Errorf("register review group: %w", groupErr)
	}

	// Spawn each agent.
	agentSessions := make([]string, len(agents))
	spawnErr := make([]error, len(agents))

	for i, ag := range agents {
		agentSession := roundPrefix + ag.Name
		agentSessions[i] = agentSession

		prCtxWithWorktree := opts.PRCtx
		if prCtxWithWorktree != nil && !prCtxWithWorktree.FetchFailed {
			ctxCopy := *prCtxWithWorktree
			ctxCopy.WorktreePath = worktree
			prCtxWithWorktree = &ctxCopy
		}
		prompt := buildReviewPrompt(opts.PRNumber, prCtxWithWorktree)

		agentConfigContent, configErr := ResolveAgentConfigContent(opts.IsolationMode, opts.ProfilesFile, ag.Name)
		if configErr != nil {
			spawnErr[i] = configErr
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), configErr))
			}
			continue
		}

		// For bwrap sessions, write the opencode.json config file to disk now
		// so it is present before the agent pane opens. Mirrors the pattern in
		// cmd/spawn.go:334-340 for regular bwrap spawns.
		// sandbox-exec mode does NOT yet use this path — config delivery for
		// sandbox-exec is deferred to #1016.
		if opts.IsolationMode == string(config.IsolationBwrap) && agentConfigContent != "" {
			containerName := container.NameForSession(agentSession)
			if writeErr := container.WriteOpencodeConfig(containerName, agentConfigContent); writeErr != nil {
				spawnErr[i] = fmt.Errorf("review: write opencode config for bwrap agent %s: %w", ag.Name, writeErr)
				if opts.OnProgress != nil {
					opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), spawnErr[i]))
				}
				continue
			}
		}

		spawnOpts := session.SpawnOpts{
			SessionName:      agentSession,
			Repo:             repo,
			Worktree:         worktree,
			AgentRole:        ag.Name,
			Prompt:           prompt,
			ConfigContent:    agentConfigContent,
			Layout:           session.LayoutAgentOnly,
			ContainerMode:    opts.ContainerMode,
			IsolationMode:    opts.IsolationMode,
			PluginHostPath:   opts.PluginHostPath,
			WorktreeReadOnly: true,
			GroupID:          groupID,
			RuntimeEnvVars:   opts.RuntimeEnvVars,
		}
		if spawnSessErr := session.SpawnSession(d, spawnOpts); spawnSessErr != nil {
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), spawnSessErr))
			}
			session.KillSidecar(agentSession)
			cleanupAgentSession(d, agentSession)
			_ = tmux.KillSession(agentSession)
			spawnErr[i] = fmt.Errorf("spawn session for %s: %w", ag.Name, spawnSessErr)
			continue
		}

		// "started" is now emitted by the readiness gate below, not here.
		// See #1051 — "spawned" is not the same as "opencode is ready".
	}

	// Per-agent readiness gate (#1051 Piece A). Runs concurrently so one
	// slow agent does not delay the others. Updates spawnErr[i] for agents
	// whose gate trips, and emits "<role> started" or
	// "<role> failed to start: not ready within <timeout>" via OnProgress.
	// spawnTimes is unused by RunAsync (the monitor process owns timing
	// from this point on), but the gate signature requires it; allocate a
	// throwaway slice so the in-loop write does not blow up.
	gateSpawnTimes := make([]time.Time, len(agents))
	gateReviewAgents(d, agents, agentSessions, spawnErr, gateSpawnTimes,
		opts.ReadinessTimeout, opts.OnProgress)

	// Check if all agents failed to spawn or to become ready.
	allFailed := true
	for _, se := range spawnErr {
		if se == nil {
			allFailed = false
			break
		}
	}
	if allFailed {
		return nil, fmt.Errorf("all review agents failed to spawn")
	}

	// Collect successfully-spawned-and-ready sessions, plus the failure
	// list for the Ack (#1051 Piece C) so the worker sees which agents
	// did not come up and why.
	var liveSessions []string
	var liveAgents []Agent
	type failedAgent struct {
		name   string
		reason string
	}
	var failures []failedAgent
	for i, se := range spawnErr {
		if se == nil {
			liveSessions = append(liveSessions, agentSessions[i])
			liveAgents = append(liveAgents, agents[i])
		} else {
			failures = append(failures, failedAgent{
				name:   agents[i].Name,
				reason: failureReason(se),
			})
		}
	}

	// Start the detached monitor process.
	monitorOpts := MonitorOpts{
		GroupID:       groupID,
		WorkerSession: opts.WorkerSession,
		PRNumber:      opts.PRNumber,
		Round:         round,
		Agents:        liveAgents,
		AgentSessions: liveSessions,
		DBPath:        dbPath,
		Timeout:       opts.Timeout * 2, // 2x per-agent timeout as group monitor limit
		SizeBudget:    opts.SizeBudget,
	}
	if startErr := StartMonitorProcess(monitorOpts, prismBinary); startErr != nil {
		// Monitor failed to start — not fatal for spawning, but warn loudly.
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not start monitor process: %v\n"+
			"Review results will NOT be delivered automatically.\n"+
			"Check agent progress with: prism checkin %s~review-%d-<agent>\n",
			startErr, opts.ParentSession, round)
	}

	// Transition the worker session to "reviewing" so that:
	//   1. The coordinator does not receive a premature "has finished" notification.
	//   2. The dashboard and `prism list-sessions` display the worker as awaiting
	//      review results rather than finished or idle.
	// This write uses the still-open DB handle (d). The sidecar's upsertState
	// path is intentionally bypassed here: the sidecar is running in the worker
	// session and will respect the reviewing state when it checks currentDBState()
	// before firing the coordinator notification.
	//
	// Non-fatal: if the update fails (e.g. session row not found) the monitor
	// will still deliver results and the worker will still receive them — only
	// the interim display state is affected.
	if workerStatus, stErr := d.CurrentStatus(opts.WorkerSession); stErr == nil && workerStatus != nil {
		if err := d.UpsertStatus(opts.WorkerSession, workerStatus.Repo, workerStatus.Worktree,
			string(agent.StateReviewing), nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "[prism review] warning: could not set worker state to reviewing: %v\n", err)
		}
	} else if stErr != nil {
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not look up worker session %q: %v\n", opts.WorkerSession, stErr)
	}

	// Build acknowledgement message (#1051 Piece C: surface partial-success).
	failurePairs := make([][2]string, 0, len(failures))
	for _, f := range failures {
		failurePairs = append(failurePairs, [2]string{f.name, f.reason})
	}
	ack := buildAsyncAck(opts.PRNumber, round, groupID, liveSessions, failurePairs, opts.WorkerSession)

	return &AsyncResult{
		GroupID:      groupID,
		SessionNames: liveSessions,
		Round:        round,
		Ack:          ack,
	}, nil
}

// failureReason returns the user-facing reason string for a spawn / readiness
// failure. For *session.ReadinessTimeoutError it produces "not ready within
// <timeout>" (matching the AC-5 example text exactly); other errors are
// passed through verbatim.
func failureReason(err error) string {
	if err == nil {
		return ""
	}
	if session.IsReadinessTimeout(err) {
		// session.ReadinessTimeoutError already renders as "not ready
		// within <timeout>" via its Error() method. Surfacing the inner
		// message keeps the Ack readable without redundant prefixes from
		// the gate-side wrapping.
		var rte *session.ReadinessTimeoutError
		if errors.As(err, &rte) {
			return rte.Error()
		}
	}
	return err.Error()
}

// buildAsyncAck constructs the acknowledgement message returned to the worker
// immediately after spawning the review agents. failures is a list of
// (agentName, reason) pairs for agents that did not become ready — see
// #1051 AC-5: "Coordinators reading the Ack should be able to see
// `Spawned: 3, Failed: 2 (review-goal: not ready within 30s, …)`."
func buildAsyncAck(prNumber string, round int, groupID string, sessionNames []string, failures [][2]string, workerSession string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Review in progress — PR #%s, round %d (group: %s)\n\n", prNumber, round, groupID))

	// Spawned/Failed summary line — the headline scan target for operators
	// reading the Ack at a glance. Always emit both numbers, even when
	// Failed is 0, so the format is stable across runs.
	sb.WriteString(fmt.Sprintf("Spawned: %d", len(sessionNames)))
	if len(failures) > 0 {
		// Inline reasons in the same order as the failures slice (which
		// preserves the original agent order from the spawn loop).
		var parts []string
		for _, f := range failures {
			parts = append(parts, fmt.Sprintf("%s: %s", f[0], f[1]))
		}
		sb.WriteString(fmt.Sprintf(", Failed: %d (%s)", len(failures), strings.Join(parts, ", ")))
	} else {
		sb.WriteString(", Failed: 0")
	}
	sb.WriteString("\n\n")

	if len(sessionNames) > 0 {
		sb.WriteString(fmt.Sprintf("Spawned %d review agents:\n", len(sessionNames)))
		for _, name := range sessionNames {
			sb.WriteString(fmt.Sprintf("  • %s\n", name))
		}
	}
	if len(failures) > 0 {
		sb.WriteString(fmt.Sprintf("\n%d agent(s) failed to start. Inspect the per-session startup log for details:\n", len(failures)))
		for _, f := range failures {
			sb.WriteString(fmt.Sprintf("  • %s — %s\n", f[0], f[1]))
		}
		sb.WriteString("\nStartup logs:\n")
		sb.WriteString("  prism logs <session> --startup            # spawn-time breadcrumbs\n")
		sb.WriteString("  prism logs <session> --agent-run          # bwrap stderr (if it got that far)\n")
	}
	sb.WriteString(fmt.Sprintf("\nResults will be delivered to session %q via prism prompt when all agents complete.\n", workerSession))
	sb.WriteString("\n**Do NOT commit, merge, or announce completion** until the review-complete prompt arrives.\n")
	if len(sessionNames) > 0 {
		sb.WriteString(fmt.Sprintf("\nYou may monitor progress with:\n  prism checkin %s~review-%d-review-goal\n", workerSession, round))
	}
	return sb.String()
}

// ResolveAgentConfigContent resolves the per-agent opencode.json config blob
// for a single agent in sandboxed mode (podman, bwrap, or sandbox-exec). It is
// factored out of Run so that it can be unit-tested independently of the
// tmux/DB machinery.
//
// Returns ("", nil) in host mode (isolationMode == "host" or ""), because no
// config injection is needed — opencode is launched directly on the host.
//
// In sandboxed mode (isolationMode == "podman", "bwrap", or "sandbox-exec"):
//   - Returns an error if pf is nil (missing profiles file).
//   - Returns an error if ContainerConfigForRole returns an error.
//   - Returns an error if the resolved blob is empty (stale profiles.json).
//   - Returns the non-empty blob when resolution succeeds.
//
// Exported so that cmd/review_test.go (and integration tests) can exercise the
// config-resolution path without needing a live DB or tmux session.
func ResolveAgentConfigContent(isolationMode string, pf *config.ProfilesFile, agentName string) (string, error) {
	needsConfig := isolationMode == string(config.IsolationPodman) || isolationMode == string(config.IsolationBwrap) || isolationMode == string(config.IsolationSandboxExec)
	if !needsConfig {
		return "", nil
	}
	if pf == nil {
		return "", fmt.Errorf("review: %s mode requires a profiles file to resolve per-agent config for %q; got nil ProfilesFile", isolationMode, agentName)
	}
	blob, cfgErr := config.ContainerConfigForRole(pf, agentName)
	if cfgErr != nil {
		return "", fmt.Errorf("review: ContainerConfigForRole(%q): %w", agentName, cfgErr)
	}
	if blob == "" {
		return "", fmt.Errorf("review: no container config blob for agent %q — profiles.json appears to be stale (missing container_review_*_config fields)\nhint: rebuild the system with the prism NixOS module to regenerate profiles.json", agentName)
	}
	return blob, nil
}

// buildReviewPrompt returns the initial prompt string for a review agent.
// When prCtx is non-nil and FetchFailed is false, the prompt begins with a
// structured context block (git log, PR metadata, linked issues, diff)
// followed by the role-specific content.
// When prCtx is nil or FetchFailed is true, a minimal fallback prompt is used.
//
// The context block always appears BEFORE the role-specific content so
// that agents read full context first and role directives second.
func buildReviewPrompt(prNumber string, prCtx *PRContext) string {
	if prCtx == nil || prCtx.FetchFailed {
		// Fallback: minimal prompt with only the PR number.
		return fmt.Sprintf(
			"Review PR #%s. Use `git diff origin/<base>...HEAD` to see the diff, "+
				"`git log --oneline -20` for recent commits, and check the linked issue "+
				"for acceptance criteria. Report your findings clearly.",
			prNumber,
		)
	}

	var sb strings.Builder

	// ── Context header ────────────────────────────────────────────────────
	sb.WriteString("## Context for your review\n\n")
	sb.WriteString("This context has been gathered for you. You do not need to re-run these commands.\n\n")

	// ── Recent commits ────────────────────────────────────────────────────
	sb.WriteString("### Recent commits (`git log --oneline -20`)\n\n")
	if prCtx.RecentCommits == "" {
		sb.WriteString("(not available)\n")
	} else {
		sb.WriteString("```\n")
		sb.WriteString(strings.TrimRight(prCtx.RecentCommits, "\n"))
		sb.WriteString("\n```\n")
	}
	sb.WriteString("\n")

	// ── Branch commits ────────────────────────────────────────────────────
	if prCtx.BaseRefName != "" {
		sb.WriteString(fmt.Sprintf("### This branch vs origin/%s (`git log origin/%s..HEAD`)\n\n",
			prCtx.BaseRefName, prCtx.BaseRefName))
	} else {
		sb.WriteString("### This branch vs base\n\n")
	}
	if prCtx.BranchCommits == "" {
		sb.WriteString("(not available)\n")
	} else {
		sb.WriteString("```\n")
		sb.WriteString(strings.TrimRight(prCtx.BranchCommits, "\n"))
		sb.WriteString("\n```\n")
	}
	sb.WriteString("\n")

	// ── PR metadata ───────────────────────────────────────────────────────
	sb.WriteString("### PR metadata\n\n")
	sb.WriteString(fmt.Sprintf("You are reviewing PR #%s.\n\n", prCtx.PRNumber))
	sb.WriteString(fmt.Sprintf("- Title: %q\n", prCtx.Title))
	sb.WriteString(fmt.Sprintf("- Head branch: %s\n", prCtx.HeadRefName))
	sb.WriteString(fmt.Sprintf("- Head commit: %s\n", prCtx.HeadRefOid))
	sb.WriteString(fmt.Sprintf("- Base branch: %s\n", prCtx.BaseRefName))
	sb.WriteString(fmt.Sprintf("- Base commit: %s\n", prCtx.BaseRefOid))
	if prCtx.WorktreePath != "" {
		sb.WriteString(fmt.Sprintf("- Worktree: %s (read-only)\n", prCtx.WorktreePath))
	}
	sb.WriteString(fmt.Sprintf("- Files changed: %d (+%d -%d lines)\n", prCtx.ChangedFiles, prCtx.Additions, prCtx.Deletions))
	sb.WriteString("\n")

	// ── PR body ───────────────────────────────────────────────────────────
	sb.WriteString("### PR body\n\n")
	body := strings.TrimSpace(prCtx.Body)
	if body == "" {
		sb.WriteString("(no body)\n")
	} else {
		// Wrap in a blockquote-style indentation to prevent any triple-backtick
		// sequences in the body from colliding with the diff code fence below.
		// Each line is prefixed with "> " so fence markers become "> ```" which
		// markdown renderers treat as quoted text, not a code fence boundary.
		for _, line := range strings.Split(body, "\n") {
			sb.WriteString("> ")
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")

	// ── Linked issues ─────────────────────────────────────────────────────
	sb.WriteString("### Linked issues\n\n")
	if len(prCtx.LinkedIssues) == 0 {
		sb.WriteString("(no linked issues found)\n")
	} else {
		// Emit issues in a stable order by collecting and sorting keys.
		keys := make([]string, 0, len(prCtx.LinkedIssues))
		for k := range prCtx.LinkedIssues {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, num := range keys {
			issueText := prCtx.LinkedIssues[num]
			sb.WriteString(fmt.Sprintf("#### Issue #%s\n\n", num))
			sb.WriteString("```\n")
			sb.WriteString(strings.TrimRight(issueText, "\n"))
			sb.WriteString("\n```\n\n")
		}
	}

	// ── Diff ──────────────────────────────────────────────────────────────
	sb.WriteString("### Diff\n\n")
	switch {
	case prCtx.DiffFilePath != "":
		// Large diff written to a file — give agents the path and guidance.
		sb.WriteString(fmt.Sprintf(
			"The diff for this PR is large (%d lines, %d KB). It has been saved to:\n\n"+
				"  %s\n\n"+
				"Query it with native git on the workspace or grep/rg on the file:\n\n"+
				"  git diff --stat origin/%s..HEAD                    # overview\n"+
				"  git log origin/%s..HEAD -- <path>                  # per-file history\n"+
				"  rg '<pattern>' %s    # search the diff\n"+
				"  git show HEAD -- <path>                            # specific file state\n",
			prCtx.DiffLines,
			prCtx.DiffBytes/1024,
			prCtx.DiffFilePath,
			prCtx.BaseRefName, prCtx.BaseRefName,
			prCtx.DiffFilePath,
		))
	case prCtx.Diff == "":
		sb.WriteString("(diff not available — use `git diff origin/" + prCtx.BaseRefName + "...HEAD` to fetch it)\n")
	default:
		if prCtx.DiffTruncated {
			sb.WriteString("Note: the diff below has been truncated due to size. Use `git diff origin/" +
				prCtx.BaseRefName + "...HEAD` to fetch any missing hunks.\n\n")
		}
		sb.WriteString("```diff\n")
		sb.WriteString(prCtx.Diff)
		if !strings.HasSuffix(prCtx.Diff, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n")
	}
	sb.WriteString("\n")

	// ── Tool preference guidance ───────────────────────────────────────────
	sb.WriteString("---\n\n")
	sb.WriteString("You may still run any git command to re-query or dig deeper as your review requires. " +
		"Prefer native git (`git show`, `git diff`, `git log`) over `gh` for cross-branch inspection — " +
		"it's faster, works offline, and doesn't consume API rate limits.\n\n")
	sb.WriteString("---\n\n")

	// ── PR under review (legacy compat section) ───────────────────────────
	// Kept for AC: tests that check "## PR under review" are still met via
	// the "### PR metadata" section. However, tests specifically looking for
	// "## PR under review" need to be updated. We keep backward compat by
	// noting this is now under "## Context for your review > ### PR metadata".
	sb.WriteString("Your role-specific instructions follow below.\n\n")
	sb.WriteString("---\n\n")

	return sb.String()
}

// sortStrings sorts a slice of strings in-place (insertion sort — small slices only).
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

// pollAgents polls the DB until all agents reach a terminal state or the
// timeout expires. Uses db.GroupCompleted(groupID) as the primary termination
// signal — this replaces the prior name-prefix-based approach (#860, Issue E).
//
// Per-agent progress tracking (onProgress callbacks for "finished" and "timed
// out" events) still checks individual session states so that progress lines
// are emitted at the right moment.
//
// spawnTimes[i] is the time agent i was spawned; used to compute durations.
// onProgress may be nil.
func pollAgents(ctx context.Context, d *db.DB, agents []Agent, agentSessions []string, timeout time.Duration, spawnTimes []time.Time, onProgress func(string), groupID string) ([]AgentResult, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	deadline := time.Now().Add(timeout)
	finished := make([]bool, len(agents))
	timedOut := make([]bool, len(agents))

	const pollInterval = 2 * time.Second

	for {
		// Primary termination check: ask the DB whether all group members
		// have reached a terminal state. This is the group-id-based path
		// introduced by #860 (Issue E).
		groupDone, groupErr := d.GroupCompleted(groupID)
		if groupErr != nil {
			// DB error — log and fall through to per-agent checks below
			// so progress tracking still works.
			fmt.Fprintf(os.Stderr, "[prism review] warning: GroupCompleted(%s): %v\n", groupID, groupErr)
		}

		// Even when groupDone is true, scan agents once more to emit any
		// remaining progress lines for agents whose terminal transition
		// hasn't been reported yet.
		for i, agentSession := range agentSessions {
			if finished[i] || timedOut[i] {
				continue
			}
			status, err := d.CurrentStatus(agentSession)
			if err != nil {
				continue
			}
			if status != nil && isTerminalState(status.State) {
				finished[i] = true
				if onProgress != nil {
					elapsed := time.Since(spawnTimes[i])
					onProgress(fmt.Sprintf("%s finished in %s", FormatAgentDisplayName(agents[i].Name), FormatProgressDuration(elapsed)))
				}
			} else if time.Now().After(deadline) {
				timedOut[i] = true
				if onProgress != nil {
					elapsed := time.Since(spawnTimes[i])
					onProgress(fmt.Sprintf("%s timed out after %s", FormatAgentDisplayName(agents[i].Name), FormatProgressDuration(elapsed)))
				}
			}
		}

		// Check context cancellation.
		select {
		case <-ctx.Done():
			for i := range agents {
				if !finished[i] && !timedOut[i] {
					timedOut[i] = true
				}
			}
			return buildResults(agents, agentSessions, d, finished, timedOut, timeout, true, groupID), ctx.Err()
		default:
		}

		// If the group is completed (all members terminal), we're done.
		if groupDone && groupErr == nil {
			break
		}

		if time.Now().After(deadline) {
			// Mark all remaining as timed out and emit progress lines.
			for i := range agents {
				if !finished[i] && !timedOut[i] {
					timedOut[i] = true
					if onProgress != nil {
						elapsed := time.Since(spawnTimes[i])
						onProgress(fmt.Sprintf("%s timed out after %s", FormatAgentDisplayName(agents[i].Name), FormatProgressDuration(elapsed)))
					}
				}
			}
			break
		}

		// Sleep while remaining responsive to context cancellation.
		select {
		case <-ctx.Done():
			for i := range agents {
				if !finished[i] {
					timedOut[i] = true
				}
			}
			return buildResults(agents, agentSessions, d, finished, timedOut, timeout, true, groupID), ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	return buildResults(agents, agentSessions, d, finished, timedOut, timeout, false, groupID), nil
}

// isTerminalState returns true if the state is considered terminal (finished or error).
func isTerminalState(state string) bool {
	switch state {
	case "finished", "interrupted", "error":
		return true
	}
	return false
}

// BuildResults is the exported entry point for tests. See buildResults.
// Production code calls buildResults directly (via pollAgents).
func BuildResults(agents []Agent, agentSessions []string, d *db.DB, finished, timedOut []bool, timeout time.Duration, cancelled bool, groupID string) []AgentResult {
	return buildResults(agents, agentSessions, d, finished, timedOut, timeout, cancelled, groupID)
}

// BuildReviewPromptForTest is an exported wrapper around buildReviewPrompt for
// use in external test packages. It allows tests to verify prompt content,
// section ordering, and fallback behaviour without needing a live tmux/DB.
func BuildReviewPromptForTest(prNumber string, prCtx *PRContext) string {
	return buildReviewPrompt(prNumber, prCtx)
}

// TruncateDiffForTest is an exported wrapper around truncateDiff for unit tests.
func TruncateDiffForTest(diff string, maxBytes, maxLines int) (string, bool) {
	return truncateDiff(diff, maxBytes, maxLines)
}

// ParseLinkedIssuesForTest is an exported wrapper around parseLinkedIssues for
// use in external test packages.
func ParseLinkedIssuesForTest(body string) []string {
	return parseLinkedIssues(body)
}

// DiffFilePathForTest is an exported wrapper around diffFilePath for tests.
func DiffFilePathForTest(prNumber string, round int) string {
	return diffFilePath(prNumber, round)
}

// BuildAsyncAckForTest is an exported wrapper around buildAsyncAck for use
// in external tests. Mirrors the production signature so AC-5 assertions
// can verify the partial-success summary text without spinning up a real
// review run.
func BuildAsyncAckForTest(prNumber string, round int, groupID string, sessionNames []string, failures [][2]string, workerSession string) string {
	return buildAsyncAck(prNumber, round, groupID, sessionNames, failures, workerSession)
}

// PollAgentsForTest is an exported wrapper around pollAgents for use in tests.
// It accepts pre-seeded DB rows and returns both results and the progress lines
// emitted via onProgress.
//
// groupID is the session_groups.group_id for the poll loop's GroupCompleted
// termination check. When empty, the caller must ensure the group has been
// registered via db.RegisterGroup before calling this function — passing ""
// will cause GroupCompleted to return true immediately (zero members).
func PollAgentsForTest(ctx context.Context, d *db.DB, agents []Agent, agentSessions []string, timeout time.Duration, spawnTimes []time.Time, onProgress func(string), groupID string) ([]AgentResult, error) {
	return pollAgents(ctx, d, agents, agentSessions, timeout, spawnTimes, onProgress, groupID)
}

// buildResults constructs AgentResult entries from polling outcomes.
// finished[i] is true when agent i reached a terminal DB state; timedOut[i] is
// true when it did not finish before the deadline; cancelled is true on context
// cancellation (e.g. SIGINT).
//
// When groupID is non-empty, result data (terminal state and last assistant
// message) is fetched via db.GroupResults(groupID) in a single batch query.
// When groupID is empty (tests that pre-date the group wiring), per-session
// individual queries are used as a fallback.
//
// Layer 3 (fail-safe): when cancelled is true, no result may have Passed=true.
// Every result is either IsError=true or annotated as incomplete. This prevents
// a false PASS even if layers 1 or 2 develop regressions later.
//
// Layer 2: agents whose DB terminal state is "interrupted" or "error" always
// produce an error result, regardless of any msg_assistant events they may have.
// Only agents that reached the "finished" state cleanly proceed to AssessPassed.
func buildResults(agents []Agent, agentSessions []string, d *db.DB, finished, timedOut []bool, timeout time.Duration, cancelled bool, groupID string) []AgentResult {
	// Pre-fetch group member data when a group_id is available. This replaces
	// individual per-session CurrentStatus + QueryEvents calls with a single
	// batch query via GroupResults (#860, Issue E).
	var groupData map[string]db.GroupMemberResult
	if groupID != "" {
		var grErr error
		groupData, grErr = d.GroupResults(groupID)
		if grErr != nil {
			// Non-fatal: fall back to per-session queries below.
			fmt.Fprintf(os.Stderr, "[prism review] warning: GroupResults(%s): %v — falling back to per-session queries\n", groupID, grErr)
			groupData = nil
		}
	}

	results := make([]AgentResult, len(agents))
	for i, ag := range agents {
		agentSession := agentSessions[i]

		// Layer 3 fail-safe: when the whole review was cancelled (SIGINT),
		// never produce a Passed=true result for any agent. Agents that did not
		// finish are timed-out/cancelled; agents that did "finish" before the
		// signal arrived are annotated as incomplete rather than trusted.
		if cancelled {
			if !finished[i] {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  fmt.Sprintf("ERROR: review cancelled before agent completed (waited %s)", formatDuration(timeout)),
					IsError: true,
				}
			} else {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  "ERROR: review cancelled mid-run — result may be incomplete",
					IsError: true,
				}
			}
			continue
		}

		// Agent did not finish before the timeout deadline.
		if timedOut[i] {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: timed out after %s", formatDuration(timeout)),
				IsError: true,
			}
			continue
		}

		// Resolve state and last message: prefer GroupResults batch data when
		// available, fall back to individual DB queries.
		var agentState string
		var lastPayload string
		if mr, ok := groupData[agentSession]; ok {
			agentState = mr.State
			lastPayload = mr.LastMessage
		} else {
			// Fallback: individual per-session queries.
			status, stErr := d.CurrentStatus(agentSession)
			if stErr == nil && status != nil {
				agentState = status.State
			}
			events, evtErr := d.QueryEvents(agentSession, 1, nil, nil, []string{"msg_assistant"})
			if evtErr == nil && len(events) > 0 {
				lastPayload = events[len(events)-1].Payload
			}
		}

		// Layer 2: check the agent's actual DB terminal state. Only "finished"
		// is considered a clean completion; "interrupted" and "error" are errors
		// regardless of what msg_assistant events may exist.
		if agentState == "interrupted" || agentState == "error" {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: agent did not complete cleanly (state: %s)", agentState),
				IsError: true,
			}
			continue
		}

		// Read last msg_assistant event.
		if lastPayload == "" {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  "ERROR: no output produced",
				IsError: true,
			}
			continue
		}

		// Extract text from the last msg_assistant event.
		// Layer 1 applies here: AssessPassed requires an explicit positive marker.
		text := extractAssistantText(lastPayload)
		passed, kind := AssessPassed(text)

		if !passed && kind == VerdictNone {
			// Agent finished cleanly but emitted no recognisable verdict.
			// Surface the output as an error so a human can judge.
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  "ERROR: no verdict found in agent output — review output:\n" + text,
				IsError: true,
			}
			continue
		}

		results[i] = AgentResult{
			Agent:  ag,
			Passed: passed,
			Output: text,
		}
	}
	return results
}

// VerdictKind describes what kind of verdict marker was found by AssessPassed.
type VerdictKind int

const (
	VerdictNone VerdictKind = iota // no recognised verdict marker
	VerdictPass                    // explicit PASS marker
	VerdictFail                    // explicit FAIL marker
)

// extractAssistantText parses the text field from a msg_assistant payload.
func extractAssistantText(payload string) string {
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err == nil && p.Text != "" {
		return p.Text
	}
	return payload
}

// AssessPassed determines whether a review agent passed by requiring an
// explicit positive verdict marker. It returns (passed bool, kind VerdictKind).
//
// Layer 1 defence: default to fail, not pass. An agent's output must contain
// an explicit <verdict>PASS</verdict> marker to be classified as passed. Any
// other text — benign partial output, startup messages, empty strings — returns
// (false, VerdictNone) so the caller can surface it for human inspection.
//
// Recognised markers (case-insensitive):
//   - <verdict>PASS</verdict>  → (true,  VerdictPass)
//   - <verdict>FAIL</verdict>  → (false, VerdictFail)
//   - anything else            → (false, VerdictNone)
//
// Exported so it can be tested directly without needing a live DB.
func AssessPassed(text string) (bool, VerdictKind) {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "<verdict>pass</verdict>") {
		return true, VerdictPass
	}
	if strings.Contains(lower, "<verdict>fail</verdict>") {
		return false, VerdictFail
	}
	return false, VerdictNone
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

// isTerminalAgentState mirrors the terminalStates set in internal/db/db.go
// (finished, interrupted, error, deleted). Duplicated here so the review
// package does not have to widen the db package's exported surface for a
// single check.
func isTerminalAgentState(state string) bool {
	switch state {
	case "finished", "interrupted", "error", "deleted":
		return true
	}
	return false
}

// DefaultFindingsSizeBudget is the default maximum inline size (in bytes) for
// full per-agent findings. When the total findings exceed this budget, they are
// written to a file and a pointer is included instead.
const DefaultFindingsSizeBudget = 20 * 1024 // 20 KB

// FindingsSizeBudgetEnvVar is the environment variable that overrides the
// default findings size budget. Set to an integer byte count (e.g. "10240").
const FindingsSizeBudgetEnvVar = "PRISM_REVIEW_SIZE_BUDGET"

// sanitisePRNumber returns a version of prNumber safe for use in a filename by
// retaining only alphanumeric characters and hyphens. This prevents path
// traversal when prNumber comes from user-supplied CLI input.
func sanitisePRNumber(prNumber string) string {
	var b strings.Builder
	for _, r := range prNumber {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	safe := b.String()
	if safe == "" {
		safe = "unknown"
	}
	return safe
}

// FindingsFilePath returns the path where full findings are written when the
// size budget is exceeded. prNumber is sanitised before use to prevent path
// traversal; round identifies the review round.
func FindingsFilePath(prNumber string, round int) string {
	return fmt.Sprintf("/tmp/prism-review-%s-round-%d.md", sanitisePRNumber(prNumber), round)
}

// resolveSizeBudget returns the effective size budget. sizeBudget ≤ 0 means use
// the default (or the env-var override, if set).
func resolveSizeBudget(sizeBudget int) int {
	if sizeBudget > 0 {
		return sizeBudget
	}
	if v := os.Getenv(FindingsSizeBudgetEnvVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultFindingsSizeBudget
}

// FormatResults formats the aggregated results as a human-readable report.
// It always includes a one-line-per-agent summary header for terse scanning,
// followed by full per-agent findings (verdict, summary, blocking issues, and
// non-blocking observations).
//
// When total output exceeds sizeBudget bytes (default 20 KB when ≤ 0), the
// full findings are written to /tmp/prism-review-<pr>-round-<round>.md and the
// inline output contains only summaries + blocking issues with a file pointer.
// Pass round=0 to omit the file-overflow path (used in unit tests).
//
// Returns the formatted string and a boolean indicating whether all passed.
func FormatResults(results []AgentResult, prNumber string, round int, sizeBudget int) (string, bool) {
	budget := resolveSizeBudget(sizeBudget)

	var header strings.Builder
	var findings strings.Builder
	allPassed := true
	var failed []string

	for _, r := range results {
		// ── Summary header line ──────────────────────────────────────────
		if r.Passed {
			header.WriteString(fmt.Sprintf("✓ %-20s passed\n", r.Agent.Name))
		} else {
			allPassed = false
			failed = append(failed, r.Agent.Name)
			if r.IsError {
				header.WriteString(fmt.Sprintf("✗ %-20s error\n", r.Agent.Name))
			} else {
				header.WriteString(fmt.Sprintf("✗ %-20s failed\n", r.Agent.Name))
			}
		}

		// ── Full per-agent findings ──────────────────────────────────────
		findings.WriteString(fmt.Sprintf("\n### %s\n\n", r.Agent.Name))
		if r.Passed {
			findings.WriteString("**Verdict:** PASS\n\n")
		} else if r.IsError {
			findings.WriteString("**Verdict:** ERROR\n\n")
		} else {
			findings.WriteString("**Verdict:** FAIL\n\n")
		}
		if r.Output != "" {
			findings.WriteString(r.Output)
			if !strings.HasSuffix(r.Output, "\n") {
				findings.WriteString("\n")
			}
		}
	}

	if !allPassed {
		header.WriteString(fmt.Sprintf("\n%d agent(s) failed. Retry: prism review %s --only %s\n",
			len(failed), prNumber, strings.Join(failed, ",")))
	}

	findingsStr := findings.String()
	headerStr := header.String()

	// ── Size budget check ────────────────────────────────────────────────
	totalSize := len(headerStr) + len(findingsStr)
	if round > 0 && totalSize > budget {
		// Overflow: write full findings to file and inline a pointer.
		filePath := FindingsFilePath(prNumber, round)
		fullContent := headerStr + "\n## Per-agent findings\n" + findingsStr
		writeErr := os.WriteFile(filePath, []byte(fullContent), 0o644)

		var result strings.Builder
		result.WriteString(headerStr)
		if writeErr == nil {
			result.WriteString(fmt.Sprintf(
				"\nFull findings: `%s` (%d KB) — read with `cat` or `rg` as needed.\n",
				filePath, totalSize/1024,
			))
		} else {
			// File write failed — inline the findings anyway rather than losing them.
			result.WriteString("\n## Per-agent findings\n")
			result.WriteString(findingsStr)
		}
		return result.String(), allPassed
	}

	// Findings fit within budget — inline them.
	var result strings.Builder
	result.WriteString(headerStr)
	result.WriteString("\n## Per-agent findings\n")
	result.WriteString(findingsStr)
	return result.String(), allPassed
}

// LookupParentSession looks up the parent session from the environment or DB.
// When called from inside a container, PRISM_SESSION_NAME is set to the
// agent's session name. We use that directly (the worker's session name).
func LookupParentSession() string {
	// Try PRISM_SESSION_NAME first (set when running inside a container/agent).
	if s := os.Getenv("PRISM_SESSION_NAME"); s != "" {
		return s
	}
	// Fall back to TMUX current session.
	sess, err := tmux.CurrentSession()
	if err != nil {
		return ""
	}
	return sess
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

// KillCurrentRoundSessions kills only the sessions in the given list.
// Used by SIGINT handlers to kill only the current round's in-progress sessions
// without touching previous rounds' persisted sessions.
func KillCurrentRoundSessions(agentSessions []string) {
	KillSessionsByNames(agentSessions)
}

// defaultDBPath returns the default prism DB path.
func defaultDBPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, _ := os.UserHomeDir()
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "prism.db")
}

// deriveRepo returns the repo portion of a session name (before "@").
func deriveRepo(sessionName string) string {
	if idx := strings.Index(sessionName, "@"); idx >= 0 {
		return sessionName[:idx]
	}
	return sessionName
}

// formatDuration formats a duration as "Xm" or "Xs" for display.
func formatDuration(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
