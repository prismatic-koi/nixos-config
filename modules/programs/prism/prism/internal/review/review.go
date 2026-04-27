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
//	$XDG_STATE_HOME/prism/run/<12-hex-of-sha256(session)>/agent-startup.log
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
	"fmt"
	"regexp"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
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

// AgentResult holds the outcome for a single review agent.
type AgentResult struct {
	Agent   Agent
	Passed  bool   // true = passed, false = failed / errored
	Output  string // last assistant message text, or error description
	IsError bool   // true = infrastructure/timeout failure
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
