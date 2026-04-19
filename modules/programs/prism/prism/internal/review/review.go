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
package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// DiffMaxBytes is the maximum diff size (in bytes) before truncation.
// Configurable for testing; production code uses this default.
const DiffMaxBytes = 200 * 1024 // 200 KB

// DiffMaxLines is the maximum number of diff lines before truncation.
// Configurable for testing; production code uses this default.
const DiffMaxLines = 4000

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
	// Diff is the full PR diff (may be truncated).
	Diff string
	// DiffTruncated is true when the diff was truncated due to size limits.
	DiffTruncated bool
	// FetchFailed is true when gh failed and only minimal info is available.
	FetchFailed bool
	// WorktreePath is the absolute path to the worktree being reviewed.
	// Review agents should treat the worktree as read-only.
	WorktreePath string
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

// FetchPRContext fetches PR metadata and diff from the gh CLI.
// If gh fails or is unavailable, it returns a PRContext with FetchFailed=true
// and only the PR number populated — the caller must not treat this as an error;
// the review run continues with a minimal prompt (fallback behaviour).
// maxBytes and maxLines control truncation of the diff; pass 0 to use the defaults.
func FetchPRContext(prNumber string, maxBytes, maxLines int) PRContext {
	if maxBytes <= 0 {
		maxBytes = DiffMaxBytes
	}
	if maxLines <= 0 {
		maxLines = DiffMaxLines
	}

	ctx := PRContext{PRNumber: prNumber}

	// Fetch PR metadata.
	viewOut, err := runGH("pr", "view", prNumber, "--json",
		"number,title,body,headRefName,headRefOid,baseRefName,baseRefOid,additions,deletions,changedFiles")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not fetch PR metadata via gh: %v — agents will fall back to git-based discovery\n", err)
		ctx.FetchFailed = true
		return ctx
	}

	var meta prViewJSON
	if jsonErr := json.Unmarshal([]byte(viewOut), &meta); jsonErr != nil {
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not parse PR metadata JSON: %v — agents will fall back to git-based discovery\n", jsonErr)
		ctx.FetchFailed = true
		return ctx
	}

	ctx.Title = meta.Title
	ctx.Body = meta.Body
	ctx.HeadRefName = meta.HeadRefName
	ctx.HeadRefOid = meta.HeadRefOid
	ctx.BaseRefName = meta.BaseRefName
	ctx.BaseRefOid = meta.BaseRefOid
	ctx.Additions = meta.Additions
	ctx.Deletions = meta.Deletions
	ctx.ChangedFiles = meta.ChangedFiles

	// Fetch diff.
	diffOut, diffErr := runGH("pr", "diff", prNumber)
	if diffErr != nil {
		// Diff failure is non-fatal: we have metadata, just no diff content.
		fmt.Fprintf(os.Stderr, "[prism review] warning: could not fetch PR diff via gh: %v — agents will use git diff instead\n", diffErr)
		// Leave Diff empty; the prompt will note diff unavailability.
	} else {
		ctx.Diff, ctx.DiffTruncated = truncateDiff(diffOut, maxBytes, maxLines)
	}

	return ctx
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

// CheckAgentAvailability verifies that the opencode agent definition files for
// all given agents exist on the host filesystem. Returns a descriptive error
// listing any missing agents; returns nil when all are present.
//
// This performs a best-effort pre-flight check by looking for agent .md files
// in $XDG_CONFIG_HOME/opencode/agents/ (or ~/.config/opencode/agents/). It is
// intentionally skipped in container mode because the check cannot reliably
// inspect the container filesystem.
func CheckAgentAvailability(agents []Agent) error {
	dir := opencodeAgentsDir()
	var missing []string
	for _, ag := range agents {
		path := filepath.Join(dir, ag.Name+".md")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missing = append(missing, ag.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"review requires opencode agent definitions that are not present in %s: %s\n"+
				"hint: ensure the system has been rebuilt with the prism NixOS module",
			dir, strings.Join(missing, ", "),
		)
	}
	return nil
}

// opencodeAgentsDir returns the path to the opencode agents directory.
func opencodeAgentsDir() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, _ := os.UserHomeDir()
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "opencode", "agents")
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
	// Worktree is the absolute path to the parent session's worktree.
	Worktree string
	// Agents is the list of review agents to spawn.
	Agents []Agent
	// Harness is the runtime harness to use ("opencode").
	Harness string
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

		// Seed agent_status for the agent session, including root_agent_name so
		// the DB reflects the reviewer type from the first moment (closes the
		// capture gap referenced in #844).
		if err := d.UpsertStatusSeedRootAgentName(agentSession, repo, worktree, "idle", nil, nil, ag.Name); err != nil {
			spawnErr[i] = fmt.Errorf("seed status for %s: %w", ag.Name, err)
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), err))
			}
			continue
		}

		// Allocate a port for this agent session.
		port, portErr := d.AllocatePort(agentSession)
		if portErr != nil {
			_ = d.SetEnded(agentSession)
			spawnErr[i] = fmt.Errorf("allocate port for %s: %w", ag.Name, portErr)
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), portErr))
			}
			continue
		}

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
		// In container mode a missing or empty blob means the container falls
		// back to the image default (build agent). ResolveAgentConfigContent
		// surfaces this as an explicit error to prevent silent build-agent spawns.
		agentConfigContent, configErr := ResolveAgentConfigContent(opts.ContainerMode, opts.ProfilesFile, ag.Name)
		if configErr != nil {
			_ = d.SetEnded(agentSession)
			spawnErr[i] = configErr
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), configErr))
			}
			continue
		}

		// Start the sidecar for this agent.
		// WorktreeReadOnly=true ensures review containers cannot modify the
		// branch under review (satisfies the [security] acceptance criterion).
		sidecarOpts := session.StartSidecarOpts{
			Port:             port,
			ContainerMode:    opts.ContainerMode,
			AgentRole:        ag.Name,
			Worktree:         worktree,
			PluginHostPath:   opts.PluginHostPath,
			InitialPrompt:    prompt,
			ConfigContent:    agentConfigContent,
			WorktreeReadOnly: true,
		}
		if sidecarErr := session.StartSidecarWithOpts(agentSession, sidecarOpts); sidecarErr != nil {
			fmt.Fprintf(os.Stderr, "[prism review] warning: could not start sidecar for %s: %v\n", ag.Name, sidecarErr)
		}

		// Create the independent top-level tmux session for this agent.
		agentCmd := buildAgentCommand(ag, agentSession, port, prompt, opts.ContainerMode)
		if err := createAgentSession(agentSession, worktree, agentCmd, port, opts.ContainerMode); err != nil {
			// Emit spawn-failure progress line immediately.
			if opts.OnProgress != nil {
				opts.OnProgress(fmt.Sprintf("%s failed to start: %v", FormatAgentDisplayName(ag.Name), err))
			}
			// Clean up this agent's resources (sidecar, DB row, tmux session).
			session.KillSidecar(agentSession)
			cleanupAgentSession(d, agentSession)
			_ = tmux.KillSession(agentSession)
			spawnErr[i] = fmt.Errorf("create session for %s: %w", ag.Name, err)
			continue
		}

		// Emit "started" progress line immediately after successful spawn.
		if opts.OnProgress != nil {
			opts.OnProgress(fmt.Sprintf("%s started", FormatAgentDisplayName(ag.Name)))
		}
		spawnTimes[i] = time.Now()
	}

	// Check if all agents failed to spawn — surface a combined error.
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

	// Build the subset of agents that successfully spawned for polling and
	// SIGINT notification.
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

	// Notify the caller with all successfully-spawned session names for SIGINT handling.
	if onSessionsCreated != nil {
		onSessionsCreated(liveSessions)
	}

	// Poll DB until all live agents finish or timeout.
	liveResults, pollErr := pollAgents(ctx, d, liveAgents, liveSessions, opts.Timeout, liveSpawnTimes, opts.OnProgress)

	// Sessions persist — do NOT kill them here. The user can re-read them later.
	// Sidecar processes are cleaned up since the agents have finished.
	for _, agentSession := range liveSessions {
		session.KillSidecar(agentSession)
	}

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

// ResolveAgentConfigContent resolves the per-agent opencode.json config blob
// for a single agent in container mode. It is factored out of Run so that it
// can be unit-tested independently of the tmux/DB machinery.
//
// Returns ("", nil) in host mode (containerMode=false), because no config
// injection is needed — opencode is launched directly on the host.
//
// In container mode (containerMode=true):
//   - Returns an error if pf is nil (missing profiles file).
//   - Returns an error if ContainerConfigForRole returns an error.
//   - Returns an error if the resolved blob is empty (stale profiles.json).
//   - Returns the non-empty blob when resolution succeeds.
//
// Exported so that cmd/review_test.go (and integration tests) can exercise the
// config-resolution path without needing a live DB or tmux session.
func ResolveAgentConfigContent(containerMode bool, pf *config.ProfilesFile, agentName string) (string, error) {
	if !containerMode {
		return "", nil
	}
	if pf == nil {
		return "", fmt.Errorf("review: container mode requires a profiles file to resolve per-agent config for %q; got nil ProfilesFile", agentName)
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
// PR-context section (metadata + diff) followed by the role-specific content.
// When prCtx is nil or FetchFailed is true, a minimal fallback prompt is used.
//
// The PR-context section always appears BEFORE the role-specific content so
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

	// ── PR under review ───────────────────────────────────────────────────
	sb.WriteString("## PR under review\n\n")
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
	if prCtx.DiffTruncated {
		sb.WriteString("\nNote: the diff below has been truncated due to size. Use `git diff origin/" +
			prCtx.BaseRefName + "...HEAD` to fetch any missing hunks.\n")
	}
	sb.WriteString("\n")

	// ── PR body ───────────────────────────────────────────────────────────
	sb.WriteString("## PR body\n\n")
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

	// ── Full diff ─────────────────────────────────────────────────────────
	sb.WriteString("## Full diff\n\n")
	if prCtx.Diff == "" {
		sb.WriteString("(diff not available — use `git diff origin/" + prCtx.BaseRefName + "...HEAD` to fetch it)\n")
	} else {
		sb.WriteString("```diff\n")
		sb.WriteString(prCtx.Diff)
		if !strings.HasSuffix(prCtx.Diff, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n")
	}
	sb.WriteString("\n")

	sb.WriteString("Your role-specific instructions follow below.\n\n")
	sb.WriteString("---\n\n")

	return sb.String()
}

// buildAgentCommand returns the command string for a review agent session.
// agentSession is the full session name (e.g. "nixos-config@feature~review-1-review-goal"),
// used as PRISM_SESSION_NAME so the plugin correctly identifies the DB row.
func buildAgentCommand(ag Agent, agentSession string, port int, prompt string, containerMode bool) string {
	if containerMode {
		// Use podman attach to bridge the tmux pane to the container's PTY.
		// The container runs opencode in combined TUI + HTTP mode (RFC #691, Phase 1a).
		// --sig-proxy=false prevents podman from forwarding signals (e.g. SIGINT from
		// Ctrl-C) to the container process; instead the ^C byte reaches opencode's TUI
		// as literal stdin input, which it handles as an interrupt keystroke — matching
		// host-mode behaviour where Ctrl-C interrupts the current turn, not the process.
		// The container name is shell-quoted so that any unexpected characters in
		// the session name cannot be interpreted as shell metacharacters when
		// buildReadinessWaitCmd embeds this string in the readiness shell script.
		return "podman attach --sig-proxy=false " + shellQuote(container.NameForSession(agentSession))
	}
	escapedPrompt := shellQuote(prompt)
	// Prepend the experimental bash-tool timeout env var so review agents also
	// get the 15-min default. Scoped to opencode only — not pi or other harnesses.
	return fmt.Sprintf("OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS=900000 PRISM_SESSION_NAME=%s opencode --agent %s --port %d --hostname 127.0.0.1 --prompt %s",
		shellQuote(agentSession), ag.OpencodeName, port, escapedPrompt)
}

// createAgentSession creates a new independent top-level tmux session for a
// review agent. In container mode it prepends a readiness wait like the
// standard session setup. The session is created with a single "agent" window
// that runs the given command.
func createAgentSession(agentSession, worktree, agentCmd string, port int, containerMode bool) error {
	cmd := agentCmd
	if containerMode && port != 0 {
		readyPath, pathErr := session.SidecarReadyPath(agentSession)
		if pathErr == nil {
			// Remove any stale ready file from a previous lifecycle.
			_ = os.Remove(readyPath)
			cmd = buildReadinessWaitCmd(readyPath, agentCmd)
		}
	}
	// Create the session (starts with a bare shell in window 0).
	if err := tmux.NewSessionDetached(agentSession, worktree); err != nil {
		return fmt.Errorf("new-session %q: %w", agentSession, err)
	}
	// Create window 1 with the agent command. Window 0 remains as a shell.
	// Using NewWindow ensures the command runs via "sh -c" and semicolons in
	// the readiness wait script are not consumed by tmux's command parser.
	if err := tmux.NewWindow(agentSession, 1, "agent", worktree, cmd); err != nil {
		return fmt.Errorf("new-window for %q: %w", agentSession, err)
	}
	// Select the agent window (1) as the default.
	_ = tmux.SelectWindow(agentSession, 1)
	return nil
}

// buildReadinessWaitCmd mirrors session.buildReadinessWaitCmd (unexported).
// Polls for the readiness file and runs the attach command directly once ready.
func buildReadinessWaitCmd(readyPath, attachCmd string) string {
	return fmt.Sprintf(
		`i=0; while [ ! -f %s ] && [ $i -lt 240 ]; do sleep 0.5; i=$((i+1)); done; `+
			`if [ ! -f %s ]; then `+
			`echo "prism: container did not become ready within 120s" >&2; exit 1; `+
			`fi; `+
			`%s`,
		shellQuote(readyPath), shellQuote(readyPath), attachCmd,
	)
}

// pollAgents polls the DB until all agents reach "finished" or the timeout expires.
// spawnTimes[i] is the time agent i was spawned; used to compute durations for
// progress output. onProgress is called for each "finished" or "timed out" event;
// it may be nil.
func pollAgents(ctx context.Context, d *db.DB, agents []Agent, agentSessions []string, timeout time.Duration, spawnTimes []time.Time, onProgress func(string)) ([]AgentResult, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	deadline := time.Now().Add(timeout)
	finished := make([]bool, len(agents))
	timedOut := make([]bool, len(agents))

	const pollInterval = 2 * time.Second

	for {
		allDone := true
		for i, agentSession := range agentSessions {
			if finished[i] || timedOut[i] {
				continue
			}
			status, err := d.CurrentStatus(agentSession)
			if err != nil {
				// DB error — treat as still running.
				allDone = false
				continue
			}
			if status != nil && isTerminalState(status.State) {
				finished[i] = true
				// Emit "finished" progress line.
				if onProgress != nil {
					elapsed := time.Since(spawnTimes[i])
					onProgress(fmt.Sprintf("%s finished in %s", FormatAgentDisplayName(agents[i].Name), FormatProgressDuration(elapsed)))
				}
			} else if time.Now().After(deadline) {
				timedOut[i] = true
				// Emit "timed out" progress line.
				if onProgress != nil {
					elapsed := time.Since(spawnTimes[i])
					onProgress(fmt.Sprintf("%s timed out after %s", FormatAgentDisplayName(agents[i].Name), FormatProgressDuration(elapsed)))
				}
			} else {
				allDone = false
			}
		}

		// Check context cancellation.
		select {
		case <-ctx.Done():
			// Build partial results with all non-finished agents as timed out.
			return buildResults(agents, agentSessions, d, finished, timedOut, timeout, true), ctx.Err()
		default:
		}

		if allDone {
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
			return buildResults(agents, agentSessions, d, finished, timedOut, timeout, true), ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	return buildResults(agents, agentSessions, d, finished, timedOut, timeout, false), nil
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
func BuildResults(agents []Agent, agentSessions []string, d *db.DB, finished, timedOut []bool, timeout time.Duration, cancelled bool) []AgentResult {
	return buildResults(agents, agentSessions, d, finished, timedOut, timeout, cancelled)
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

// PollAgentsForTest is an exported wrapper around pollAgents for use in tests.
// It accepts pre-seeded DB rows and returns both results and the progress lines
// emitted via onProgress.
func PollAgentsForTest(ctx context.Context, d *db.DB, agents []Agent, agentSessions []string, timeout time.Duration, spawnTimes []time.Time, onProgress func(string)) ([]AgentResult, error) {
	return pollAgents(ctx, d, agents, agentSessions, timeout, spawnTimes, onProgress)
}

// buildResults constructs AgentResult entries from polling outcomes.
// finished[i] is true when agent i reached a terminal DB state; timedOut[i] is
// true when it did not finish before the deadline; cancelled is true on context
// cancellation (e.g. SIGINT).
//
// Layer 3 (fail-safe): when cancelled is true, no result may have Passed=true.
// Every result is either IsError=true or annotated as incomplete. This prevents
// a false PASS even if layers 1 or 2 develop regressions later.
//
// Layer 2: agents whose DB terminal state is "interrupted" or "error" always
// produce an error result, regardless of any msg_assistant events they may have.
// Only agents that reached the "finished" state cleanly proceed to AssessPassed.
func buildResults(agents []Agent, agentSessions []string, d *db.DB, finished, timedOut []bool, timeout time.Duration, cancelled bool) []AgentResult {
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

		// Layer 2: check the agent's actual DB terminal state. Only "finished"
		// is considered a clean completion; "interrupted" and "error" are errors
		// regardless of what msg_assistant events may exist.
		status, stErr := d.CurrentStatus(agentSession)
		if stErr == nil && status != nil && (status.State == "interrupted" || status.State == "error") {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: agent did not complete cleanly (state: %s)", status.State),
				IsError: true,
			}
			continue
		}

		// Read last msg_assistant event.
		events, err := d.QueryEvents(agentSession, 1, nil, nil, []string{"msg_assistant"})
		if err != nil || len(events) == 0 {
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
		text := extractAssistantText(events[len(events)-1].Payload)
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
func cleanupAgentSession(d *db.DB, agentSession string) {
	_ = d.ReleasePort(agentSession)
	_ = d.SetEnded(agentSession)
	_ = d.PurgeBusMessages(agentSession)
}

// FormatResults formats the aggregated results as a human-readable report.
// Returns the formatted string and a boolean indicating whether all passed.
func FormatResults(results []AgentResult, prNumber string) (string, bool) {
	var sb strings.Builder
	allPassed := true
	var failed []string

	for _, r := range results {
		if r.Passed {
			sb.WriteString(fmt.Sprintf("✓ %-20s passed\n", r.Agent.Name))
		} else {
			allPassed = false
			failed = append(failed, r.Agent.Name)
			if r.IsError {
				sb.WriteString(fmt.Sprintf("✗ %-20s %s\n", r.Agent.Name, r.Output))
			} else {
				// Content failure: include the output.
				sb.WriteString(fmt.Sprintf("✗ %-20s\n", r.Agent.Name))
				// Indent the output.
				for _, line := range strings.Split(r.Output, "\n") {
					sb.WriteString("  " + line + "\n")
				}
			}
		}
	}

	if !allPassed {
		sb.WriteString(fmt.Sprintf("\n%d agent(s) failed. Retry: prism review %s --only %s\n",
			len(failed), prNumber, strings.Join(failed, ",")))
	}

	return sb.String(), allPassed
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

// KillReviewSessionsForParent kills all ~review-* tmux sessions for the given parent.
// This is the public API used by cleanup.go for cascading parent cleanup.
// It kills ALL review sessions across all rounds (for prism cleanup --yes --session <parent>).
func KillReviewSessionsForParent(parentSession string) {
	prefix := parentSession + "~review-"
	KillSessionPrefix(prefix)
}

// CleanupReviewSessionsForParent kills all ~review-* tmux sessions for the
// given parent AND cleans up their DB rows (port allocations, ended state,
// bus messages). Called by prism cleanup --yes --session <parent> to cascade
// the cleanup to all review sessions.
func CleanupReviewSessionsForParent(d *db.DB, parentSession string) {
	prefix := parentSession + "~review-"

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

// shellQuote wraps s in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// formatDuration formats a duration as "Xm" or "Xs" for display.
func formatDuration(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
