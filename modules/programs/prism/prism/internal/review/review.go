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
	"context"
	"encoding/json"
	"fmt"
	"os"
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

// DefaultAgents returns the default set of review agents for the opencode harness.
func DefaultAgents() []Agent {
	return []Agent{
		{Name: "review", OpencodeName: "review"},
	}
}

// EnhancedAgents returns the five-agent set used in enhanced review mode.
// Each agent corresponds to a specialised opencode agent definition under
// modules/programs/prism/opencode/agents-enhanced/.
func EnhancedAgents() []Agent {
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
			"enhanced review requires opencode agent definitions that are not present in %s: %s\n"+
				"hint: ensure the enhancedReview Nix option is enabled and the system has been rebuilt",
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
		agents = DefaultAgents()
	}

	// Spawn each agent as its own independent top-level tmux session.
	agentSessions := make([]string, len(agents))
	for i, ag := range agents {
		// Per-agent session: <parent>~review-<N>-<agent.Name>
		agentSession := roundPrefix + ag.Name
		agentSessions[i] = agentSession

		// Seed agent_status for the agent session.
		if err := d.UpsertStatus(agentSession, repo, worktree, "idle", nil, nil); err != nil {
			// Clean up already-started agents before returning.
			for j := 0; j < i; j++ {
				session.KillSidecar(agentSessions[j])
				cleanupAgentSession(d, agentSessions[j])
				_ = tmux.KillSession(agentSessions[j])
			}
			return nil, fmt.Errorf("seed status for %s: %w", ag.Name, err)
		}

		// Allocate a port for this agent session.
		port, portErr := d.AllocatePort(agentSession)
		if portErr != nil {
			for j := 0; j < i; j++ {
				session.KillSidecar(agentSessions[j])
				cleanupAgentSession(d, agentSessions[j])
				_ = tmux.KillSession(agentSessions[j])
			}
			// Also clean up the DB row we just seeded for i.
			_ = d.SetEnded(agentSession)
			return nil, fmt.Errorf("allocate port for %s: %w", ag.Name, portErr)
		}

		// Build the prompt for the review agent.
		prompt := buildReviewPrompt(opts.PRNumber)

		// Resolve the per-agent config blob. Each agent gets its own hardened
		// opencode.json that declares only that one review agent.
		agentConfigContent := ""
		if opts.ContainerMode && opts.ProfilesFile != nil {
			if blob, cfgErr := config.ContainerConfigForRole(opts.ProfilesFile, ag.Name); cfgErr == nil {
				agentConfigContent = blob
			}
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
			// Clean up agents 0..i (including the current one whose session failed).
			// Kill the sidecar for agent i (already started above).
			session.KillSidecar(agentSession)
			cleanupAgentSession(d, agentSession)
			// Kill tmux session for agent i if it was created (best effort).
			_ = tmux.KillSession(agentSession)

			// Clean up agents 0..i-1 that fully started.
			for j := 0; j < i; j++ {
				session.KillSidecar(agentSessions[j])
				cleanupAgentSession(d, agentSessions[j])
				_ = tmux.KillSession(agentSessions[j])
			}
			return nil, fmt.Errorf("create session for %s: %w", ag.Name, err)
		}
	}

	// Notify the caller with all session names for SIGINT handling.
	if onSessionsCreated != nil {
		onSessionsCreated(agentSessions)
	}

	// Poll DB until all agents finish or timeout.
	results, pollErr := pollAgents(ctx, d, agents, agentSessions, opts.Timeout)

	// Sessions persist — do NOT kill them here. The user can re-read them later.
	// Sidecar processes are cleaned up since the agent has finished.
	for _, agentSession := range agentSessions {
		session.KillSidecar(agentSession)
	}

	return results, pollErr
}

// buildReviewPrompt returns the initial prompt string for a review agent.
func buildReviewPrompt(prNumber string) string {
	return fmt.Sprintf("Review PR #%s. Run `gh pr view %s` and `gh pr diff %s` to get the full diff. Check the linked issue for acceptance criteria and validate each one. Report your findings clearly.", prNumber, prNumber, prNumber)
}

// buildAgentCommand returns the command string for a review agent session.
// agentSession is the full session name (e.g. "nixos-config@feature~review-1-review-goal"),
// used as PRISM_SESSION_NAME so the plugin correctly identifies the DB row.
func buildAgentCommand(ag Agent, agentSession string, port int, prompt string, containerMode bool) string {
	if containerMode {
		// Use podman attach to bridge the tmux pane to the container's PTY.
		// The container runs opencode in combined TUI + HTTP mode (RFC #691, Phase 1a).
		// The container name is shell-quoted so that any unexpected characters in
		// the session name cannot be interpreted as shell metacharacters when
		// buildReadinessWaitCmd embeds this string in the readiness shell script.
		return "podman attach " + shellQuote(container.NameForSession(agentSession))
	}
	escapedPrompt := shellQuote(prompt)
	return fmt.Sprintf("PRISM_SESSION_NAME=%s opencode --agent %s --port %d --hostname 127.0.0.1 --prompt %s",
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
func pollAgents(ctx context.Context, d *db.DB, agents []Agent, agentSessions []string, timeout time.Duration) ([]AgentResult, error) {
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
			} else if time.Now().After(deadline) {
				timedOut[i] = true
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
			// Mark all remaining as timed out.
			for i := range agents {
				if !finished[i] {
					timedOut[i] = true
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

// buildResults constructs AgentResult entries from polling outcomes.
// finished[i] is true when agent i reached a terminal DB state; timedOut[i] is
// true when it did not finish before the deadline; cancelled is true on context
// cancellation (e.g. SIGINT). Agents in finished[i]=true always show their
// actual output, even when the process is being cancelled.
func buildResults(agents []Agent, agentSessions []string, d *db.DB, finished, timedOut []bool, timeout time.Duration, cancelled bool) []AgentResult {
	results := make([]AgentResult, len(agents))
	for i, ag := range agents {
		agentSession := agentSessions[i]
		// Only report as timed-out / cancelled when the agent genuinely did not
		// finish. An agent that completed before a signal arrived should still
		// show its actual output.
		if (timedOut[i] || cancelled) && !finished[i] {
			results[i] = AgentResult{
				Agent:   ag,
				Passed:  false,
				Output:  fmt.Sprintf("ERROR: timed out after %s", formatDuration(timeout)),
				IsError: true,
			}
			continue
		}

		// Read last msg_assistant event.
		events, err := d.QueryEvents(agentSession, 1, nil, nil, []string{"msg_assistant"})
		if err != nil || len(events) == 0 {
			// Check if there was a crash (state is interrupted/error without output).
			status, _ := d.CurrentStatus(agentSession)
			if status != nil && (status.State == "interrupted" || status.State == "error") {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  fmt.Sprintf("ERROR: agent crashed (state: %s)", status.State),
					IsError: true,
				}
			} else {
				results[i] = AgentResult{
					Agent:   ag,
					Passed:  false,
					Output:  "ERROR: no output produced",
					IsError: true,
				}
			}
			continue
		}

		// Extract text from the last msg_assistant event.
		text := extractAssistantText(events[len(events)-1].Payload)
		passed := AssessPassed(text)

		results[i] = AgentResult{
			Agent:  ag,
			Passed: passed,
			Output: text,
		}
	}
	return results
}

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

// AssessPassed heuristically determines whether a review agent passed.
// A review "passes" when the agent found no blocking issues. We look for
// common patterns that indicate a clean review.
//
// Exported so it can be tested directly without needing a live DB.
func AssessPassed(text string) bool {
	lower := strings.ToLower(text)
	// Explicit failure indicators.
	failPhrases := []string{
		"please fix",
		"needs to be fixed",
		"must be fixed",
		"blocking issue",
		"this is a bug",
		"error found",
		"security issue",
		"vulnerability",
	}
	for _, phrase := range failPhrases {
		if strings.Contains(lower, phrase) {
			return false
		}
	}
	// If the text contains ✗ it likely has failures.
	if strings.Contains(text, "✗") {
		return false
	}
	return true
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
